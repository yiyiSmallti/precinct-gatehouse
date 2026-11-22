package compat

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// DecodeResponses translates an OpenAI /v1/responses JSON body into an OpenAI
// Chat Completions request object, also returning the requested model and any
// dropped non-function tool types.
func DecodeResponses(body []byte) (oai.Object, string, []string, error) {
	in, err := oai.Decode(body)
	if err != nil {
		return nil, "", nil, err
	}
	reqModel := oai.Str(in, "model")
	if prev := oai.Str(in, "previous_response_id"); prev != "" {
		return nil, reqModel, nil, oai.Errorf(http.StatusBadRequest, "previous_response_not_supported", "previous_response_id is not supported")
	}
	out := oai.Object{"model": reqModel}

	var msgs []any
	if inst := oai.Str(in, "instructions"); inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch inp := in["input"].(type) {
	case string:
		if inp != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": inp})
		}
	case []any:
		for _, raw := range inp {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ := oai.Str(item, "type")
			switch {
			case typ == "message" || (typ == "" && oai.Str(item, "role") != ""):
				role := oai.Str(item, "role")
				if role == "developer" {
					role = "system"
				}
				content := convertResponseInputContent(item["content"])
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			case typ == "function_call":
				callID := oai.Str(item, "call_id")
				if callID == "" {
					callID = oai.Str(item, "id")
				}
				tc := map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      oai.Str(item, "name"),
						"arguments": oai.Str(item, "arguments"),
					},
				}
				if len(msgs) > 0 {
					if prev, ok := msgs[len(msgs)-1].(map[string]any); ok && oai.Str(prev, "role") == "assistant" {
						if prevTCs, ok := prev["tool_calls"].([]any); ok {
							prev["tool_calls"] = append(prevTCs, tc)
							continue
						}
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				})
			case typ == "function_call_output":
				outStr := ""
				switch ov := item["output"].(type) {
				case string:
					outStr = ov
				case nil:
					outStr = ""
				default:
					outStr = string(oai.Encode(ov))
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": oai.Str(item, "call_id"),
					"content":      outStr,
				})
			case typ == "reasoning":
				// dropped
			default:
				return nil, reqModel, nil, oai.Errorf(http.StatusBadRequest, "unsupported_input_item", "unsupported input item type %q", typ)
			}
		}
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}

	var dropped []string
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		var chatTools []any
		for _, rt := range tools {
			t, ok := rt.(map[string]any)
			if !ok {
				continue
			}
			tt := oai.Str(t, "type")
			if tt != "function" {
				if tt != "" {
					dropped = append(dropped, tt)
				}
				continue
			}
			fn := map[string]any{"name": oai.Str(t, "name")}
			if desc := oai.Str(t, "description"); desc != "" {
				fn["description"] = desc
			}
			if params, ok := t["parameters"]; ok && params != nil {
				fn["parameters"] = params
			}
			if strict, ok := t["strict"]; ok && strict != nil {
				fn["strict"] = strict
			}
			chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
		}
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}

	switch tc := in["tool_choice"].(type) {
	case string:
		out["tool_choice"] = tc
	case map[string]any:
		if oai.Str(tc, "type") == "function" {
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": oai.Str(tc, "name")},
			}
		}
	}

	for _, pair := range [][2]string{
		{"max_output_tokens", "max_tokens"},
		{"temperature", "temperature"},
		{"top_p", "top_p"},
		{"parallel_tool_calls", "parallel_tool_calls"},
	} {
		if v, ok := in[pair[0]]; ok && v != nil {
			out[pair[1]] = v
		}
	}
	if rObj, ok := in["reasoning"].(map[string]any); ok {
		if effort := oai.Str(rObj, "effort"); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	if stream := oai.Bool(in, "stream"); stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if txtObj, ok := in["text"].(map[string]any); ok {
		if fmtObj, ok := txtObj["format"].(map[string]any); ok {
			switch oai.Str(fmtObj, "type") {
			case "json_schema":
				js := map[string]any{
					"name":   fmtObj["name"],
					"schema": fmtObj["schema"],
				}
				if strict, ok := fmtObj["strict"]; ok && strict != nil {
					js["strict"] = strict
				}
				out["response_format"] = map[string]any{"type": "json_schema", "json_schema": js}
			case "json_object":
				out["response_format"] = map[string]any{"type": "json_object"}
			}
		}
	}
	if pk := oai.Str(in, "prompt_cache_key"); pk != "" {
		out["user"] = pk
	} else if u := oai.Str(in, "user"); u != "" {
		out["user"] = u
	}
	return out, reqModel, dropped, nil
}

func convertResponseInputContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []any
		for _, raw := range c {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch oai.Str(p, "type") {
			case "input_text", "output_text", "text":
				parts = append(parts, map[string]any{"type": "text", "text": oai.Str(p, "text")})
			case "input_image":
				var imgURL string
				switch u := p["image_url"].(type) {
				case string:
					imgURL = u
				case map[string]any:
					imgURL = oai.Str(u, "url")
				}
				if imgURL != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": imgURL},
					})
				}
			}
		}
		if len(parts) == 1 {
			if p0, ok := parts[0].(map[string]any); ok && oai.Str(p0, "type") == "text" {
				return oai.Str(p0, "text")
			}
		}
		return parts
	default:
		return ""
	}
}

// ResponsesTranslator translates OpenAI Chat Completions responses and streams
// into OpenAI Responses format.
type ResponsesTranslator struct {
	Model string

	respID         string
	createdAt      int64
	seq            int
	outputIndex    int
	openKind       string
	openItemID     string
	openCallID     string
	openName       string
	openToolIdx    int
	openText       strings.Builder
	completedItems []any
	finishReason   string
	usage          oai.Usage
}

// NewResponsesTranslator creates a per-request OpenAI Responses translator.
func NewResponsesTranslator(requestedModel string) *ResponsesTranslator {
	return &ResponsesTranslator{
		Model:          requestedModel,
		outputIndex:    -1,
		openToolIdx:    -1,
		completedItems: []any{},
	}
}

func (t *ResponsesTranslator) WriteError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func responsesUsage(u oai.Usage) map[string]any {
	return map[string]any{
		"input_tokens": u.PromptTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.CachedTokens,
		},
		"output_tokens": u.CompletionTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
		"total_tokens": u.TotalTokens,
	}
}

func (t *ResponsesTranslator) responseObject(status, finish string, output []any, usage *oai.Usage) map[string]any {
	if output == nil {
		output = []any{}
	}
	obj := map[string]any{
		"id":         t.respID,
		"object":     "response",
		"created_at": t.createdAt,
		"status":     status,
		"model":      t.Model,
		"output":     output,
	}
	if status == "incomplete" || (status == "completed" && finish == "length") {
		obj["status"] = "incomplete"
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage != nil {
		obj["usage"] = responsesUsage(*usage)
	}
	return obj
}

func (t *ResponsesTranslator) WriteJSON(w http.ResponseWriter, body []byte) {
	o, err := oai.Decode(body)
	if err != nil {
		oai.WriteError(w, oai.Errorf(http.StatusBadGateway, "upstream_bad_json", "provider returned invalid JSON"))
		return
	}
	idSuffix := reqIDFromWriter(w)
	t.respID = "resp_" + idSuffix
	t.createdAt = int64(oai.Int(o, "created"))
	if t.createdAt == 0 {
		t.createdAt = time.Now().Unix()
	}

	output := []any{}
	finish := ""
	if choices, _ := o["choices"].([]any); len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			finish = oai.Str(c0, "finish_reason")
			if msg, ok := c0["message"].(map[string]any); ok {
				if rc := oai.Str(msg, "reasoning_content"); rc != "" {
					output = append(output, map[string]any{
						"type": "reasoning",
						"id":   "rs_" + idSuffix,
						"summary": []any{
							map[string]any{"type": "summary_text", "text": rc},
						},
					})
				}
				txt := oai.Str(msg, "content")
				tcs, _ := msg["tool_calls"].([]any)
				if txt != "" || (len(tcs) == 0 && len(output) == 0) {
					output = append(output, map[string]any{
						"type":   "message",
						"id":     "msg_" + idSuffix,
						"status": "completed",
						"role":   "assistant",
						"content": []any{
							map[string]any{
								"type":        "output_text",
								"text":        txt,
								"annotations": []any{},
							},
						},
					})
				}
				for i, rawTC := range tcs {
					tc, _ := rawTC.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					output = append(output, map[string]any{
						"type":      "function_call",
						"id":        fmt.Sprintf("fc_%s_%d", idSuffix, i),
						"call_id":   oai.Str(tc, "id"),
						"name":      oai.Str(fn, "name"),
						"arguments": oai.Str(fn, "arguments"),
						"status":    "completed",
					})
				}
			}
		}
	}
	u, _ := oai.UsageOf(o)
	resp := t.responseObject("completed", finish, output, &u)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(oai.Encode(resp))
}

func (t *ResponsesTranslator) emitEvent(w http.ResponseWriter, typ string, data map[string]any) {
	data["type"] = typ
	data["sequence_number"] = t.seq
	t.seq++
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, oai.Encode(data))
}

func (t *ResponsesTranslator) StartStream(w http.ResponseWriter) {
	idSuffix := reqIDFromWriter(w)
	t.respID = "resp_" + idSuffix
	t.createdAt = time.Now().Unix()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	inProg := t.responseObject("in_progress", "", []any{}, nil)
	t.emitEvent(w, "response.created", map[string]any{"response": inProg})
	t.emitEvent(w, "response.in_progress", map[string]any{"response": inProg})
}

func (t *ResponsesTranslator) closeOpenItem(w http.ResponseWriter) {
	if t.openKind == "" {
		return
	}
	text := t.openText.String()
	switch t.openKind {
	case "reasoning":
		t.emitEvent(w, "response.reasoning_summary_text.done", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"summary_index": 0,
			"text":          text,
		})
		part := map[string]any{"type": "summary_text", "text": text}
		t.emitEvent(w, "response.reasoning_summary_part.done", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"summary_index": 0,
			"part":          part,
		})
		item := map[string]any{
			"type":    "reasoning",
			"id":      t.openItemID,
			"summary": []any{part},
		}
		t.emitEvent(w, "response.output_item.done", map[string]any{
			"output_index": t.outputIndex,
			"item":         item,
		})
		t.completedItems = append(t.completedItems, item)
	case "message":
		t.emitEvent(w, "response.output_text.done", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"content_index": 0,
			"text":          text,
		})
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		t.emitEvent(w, "response.content_part.done", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"content_index": 0,
			"part":          part,
		})
		item := map[string]any{
			"type":    "message",
			"id":      t.openItemID,
			"status":  "completed",
			"role":    "assistant",
			"content": []any{part},
		}
		t.emitEvent(w, "response.output_item.done", map[string]any{
			"output_index": t.outputIndex,
			"item":         item,
		})
		t.completedItems = append(t.completedItems, item)
	case "function_call":
		t.emitEvent(w, "response.function_call_arguments.done", map[string]any{
			"item_id":      t.openItemID,
			"output_index": t.outputIndex,
			"arguments":    text,
		})
		item := map[string]any{
			"type":      "function_call",
			"id":        t.openItemID,
			"call_id":   t.openCallID,
			"name":      t.openName,
			"arguments": text,
			"status":    "completed",
		}
		t.emitEvent(w, "response.output_item.done", map[string]any{
			"output_index": t.outputIndex,
			"item":         item,
		})
		t.completedItems = append(t.completedItems, item)
	}
	t.openKind = ""
	t.openToolIdx = -1
	t.openText.Reset()
}

func (t *ResponsesTranslator) startReasoning(w http.ResponseWriter) {
	t.closeOpenItem(w)
	t.outputIndex++
	t.openKind = "reasoning"
	t.openItemID = fmt.Sprintf("rs_%s_%d", reqIDFromWriter(w), t.outputIndex)
	t.emitEvent(w, "response.output_item.added", map[string]any{
		"output_index": t.outputIndex,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      t.openItemID,
			"summary": []any{},
		},
	})
	t.emitEvent(w, "response.reasoning_summary_part.added", map[string]any{
		"item_id":       t.openItemID,
		"output_index":  t.outputIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

func (t *ResponsesTranslator) startMessage(w http.ResponseWriter) {
	t.closeOpenItem(w)
	t.outputIndex++
	t.openKind = "message"
	t.openItemID = fmt.Sprintf("msg_%s_%d", reqIDFromWriter(w), t.outputIndex)
	t.emitEvent(w, "response.output_item.added", map[string]any{
		"output_index": t.outputIndex,
		"item": map[string]any{
			"type":    "message",
			"id":      t.openItemID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	})
	t.emitEvent(w, "response.content_part.added", map[string]any{
		"item_id":       t.openItemID,
		"output_index":  t.outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (t *ResponsesTranslator) startFunctionCall(w http.ResponseWriter, tcIdx int, callID, name string) {
	t.closeOpenItem(w)
	t.outputIndex++
	t.openKind = "function_call"
	t.openToolIdx = tcIdx
	t.openCallID = callID
	t.openName = name
	t.openItemID = fmt.Sprintf("fc_%s_%d", reqIDFromWriter(w), t.outputIndex)
	t.emitEvent(w, "response.output_item.added", map[string]any{
		"output_index": t.outputIndex,
		"item": map[string]any{
			"type":      "function_call",
			"id":        t.openItemID,
			"call_id":   callID,
			"name":      name,
			"arguments": "",
			"status":    "in_progress",
		},
	})
}

func (t *ResponsesTranslator) OnChunk(w http.ResponseWriter, chunk oai.Object) {
	if u, ok := oai.UsageOf(chunk); ok {
		t.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return
	}
	if fr := oai.Str(c0, "finish_reason"); fr != "" {
		t.finishReason = fr
	}
	delta, _ := c0["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if rc := oai.Str(delta, "reasoning_content"); rc != "" {
		if t.openKind != "reasoning" {
			t.startReasoning(w)
		}
		t.openText.WriteString(rc)
		t.emitEvent(w, "response.reasoning_summary_text.delta", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"summary_index": 0,
			"delta":         rc,
		})
	}
	if txt := oai.Str(delta, "content"); txt != "" {
		if t.openKind != "message" {
			t.startMessage(w)
		}
		t.openText.WriteString(txt)
		t.emitEvent(w, "response.output_text.delta", map[string]any{
			"item_id":       t.openItemID,
			"output_index":  t.outputIndex,
			"content_index": 0,
			"delta":         txt,
		})
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for i, rawTC := range tcs {
			tc, _ := rawTC.(map[string]any)
			if tc == nil {
				continue
			}
			tcIdx := i
			if _, hasIdx := tc["index"]; hasIdx {
				tcIdx = oai.Int(tc, "index")
			}
			id := oai.Str(tc, "id")
			fn, _ := tc["function"].(map[string]any)
			name := oai.Str(fn, "name")
			args := oai.Str(fn, "arguments")
			if t.openKind != "function_call" || tcIdx != t.openToolIdx {
				t.startFunctionCall(w, tcIdx, id, name)
			}
			if args != "" {
				t.openText.WriteString(args)
				t.emitEvent(w, "response.function_call_arguments.delta", map[string]any{
					"item_id":      t.openItemID,
					"output_index": t.outputIndex,
					"delta":        args,
				})
			}
		}
	}
}

func (t *ResponsesTranslator) EndStream(w http.ResponseWriter, ok bool) {
	t.closeOpenItem(w)
	if !ok {
		failed := t.responseObject("failed", "", t.completedItems, nil)
		failed["error"] = map[string]any{
			"code":    "upstream_stream_interrupted",
			"message": "upstream stream interrupted",
		}
		t.emitEvent(w, "response.failed", map[string]any{"response": failed})
		return
	}
	completed := t.responseObject("completed", t.finishReason, t.completedItems, &t.usage)
	t.emitEvent(w, "response.completed", map[string]any{"response": completed})
}
