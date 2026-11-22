package compat

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// DecodeAnthropic translates an Anthropic /v1/messages JSON body into an OpenAI
// Chat Completions request object, also returning the original requested model.
func DecodeAnthropic(body []byte) (oai.Object, string, error) {
	in, err := oai.Decode(body)
	if err != nil {
		return nil, "", err
	}
	reqModel := oai.Str(in, "model")
	out := oai.Object{
		"model": reqModel,
	}

	var msgs []any
	if sys := extractAnthropicSystem(in["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	rawMsgs, _ := in["messages"].([]any)
	for _, rm := range rawMsgs {
		m, ok := rm.(map[string]any)
		if !ok {
			continue
		}
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, reqModel, err
		}
		msgs = append(msgs, converted...)
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}

	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		var chatTools []any
		for _, rt := range tools {
			t, ok := rt.(map[string]any)
			if !ok {
				continue
			}
			fn := map[string]any{"name": oai.Str(t, "name")}
			if desc := oai.Str(t, "description"); desc != "" {
				fn["description"] = desc
			}
			if schema, ok := t["input_schema"]; ok && schema != nil {
				fn["parameters"] = schema
			}
			chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
		}
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}

	if tc, ok := in["tool_choice"].(map[string]any); ok {
		switch oai.Str(tc, "type") {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": oai.Str(tc, "name")},
			}
		}
	}

	for _, pair := range [][2]string{
		{"max_tokens", "max_tokens"},
		{"temperature", "temperature"},
		{"top_p", "top_p"},
		{"stop_sequences", "stop"},
	} {
		if v, ok := in[pair[0]]; ok && v != nil {
			out[pair[1]] = v
		}
	}
	if stream := oai.Bool(in, "stream"); stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if th, ok := in["thinking"].(map[string]any); ok && oai.Str(th, "type") == "enabled" {
		out["reasoning_effort"] = "high"
	}
	if md, ok := in["metadata"].(map[string]any); ok {
		if uid := oai.Str(md, "user_id"); uid != "" {
			out["user"] = uid
		}
	}
	return out, reqModel, nil
}

func extractAnthropicSystem(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, item := range t {
			if m, ok := item.(map[string]any); ok && oai.Str(m, "type") == "text" {
				if txt := oai.Str(m, "text"); txt != "" {
					parts = append(parts, txt)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func convertAnthropicMessage(m map[string]any) ([]any, error) {
	role := oai.Str(m, "role")
	switch c := m["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": c}}, nil
	case []any:
		if role == "assistant" {
			return convertAnthropicAssistantBlocks(c), nil
		}
		return convertAnthropicUserBlocks(role, c), nil
	default:
		return []any{map[string]any{"role": role, "content": ""}}, nil
	}
}

func convertAnthropicUserBlocks(role string, blocks []any) []any {
	var toolMsgs []any
	var parts []any
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch oai.Str(blk, "type") {
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": oai.Str(blk, "tool_use_id"),
				"content":      extractToolResultContent(blk["content"]),
			})
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": oai.Str(blk, "text")})
		case "image":
			if img, ok := convertAnthropicImageBlock(blk); ok {
				parts = append(parts, img)
			}
		}
	}
	var out []any
	out = append(out, toolMsgs...)
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]any); ok && oai.Str(p, "type") == "text" {
			out = append(out, map[string]any{"role": role, "content": oai.Str(p, "text")})
			return out
		}
	}
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": role, "content": parts})
	}
	return out
}

func convertAnthropicAssistantBlocks(blocks []any) []any {
	var thinkingParts []string
	var textParts []string
	var toolCalls []any
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch oai.Str(blk, "type") {
		case "thinking":
			if th := oai.Str(blk, "thinking"); th != "" {
				thinkingParts = append(thinkingParts, th)
			}
		case "redacted_thinking":
			// dropped
		case "text":
			if txt := oai.Str(blk, "text"); txt != "" {
				textParts = append(textParts, txt)
			}
		case "tool_use":
			args := "{}"
			if inp, ok := blk["input"]; ok && inp != nil {
				args = string(oai.Encode(inp))
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   oai.Str(blk, "id"),
				"type": "function",
				"function": map[string]any{
					"name":      oai.Str(blk, "name"),
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		msg["content"] = strings.Join(textParts, "")
	} else if len(toolCalls) == 0 {
		msg["content"] = ""
	} else {
		msg["content"] = nil
	}
	if len(thinkingParts) > 0 {
		msg["reasoning_content"] = strings.Join(thinkingParts, "")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return []any{msg}
}

func convertAnthropicImageBlock(blk map[string]any) (map[string]any, bool) {
	src, _ := blk["source"].(map[string]any)
	switch oai.Str(src, "type") {
	case "base64":
		mt := oai.Str(src, "media_type")
		if mt == "" {
			mt = "image/png"
		}
		url := fmt.Sprintf("data:%s;base64,%s", mt, oai.Str(src, "data"))
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
	case "url":
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": oai.Str(src, "url")}}, true
	}
	return nil, false
}

func extractToolResultContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var textParts []string
		var parts []any
		hasImage := false
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch oai.Str(m, "type") {
			case "text":
				txt := oai.Str(m, "text")
				textParts = append(textParts, txt)
				parts = append(parts, map[string]any{"type": "text", "text": txt})
			case "image":
				if img, ok := convertAnthropicImageBlock(m); ok {
					hasImage = true
					parts = append(parts, img)
				}
			}
		}
		if hasImage {
			return parts
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n")
		}
		return string(oai.Encode(c))
	case nil:
		return ""
	default:
		return string(oai.Encode(c))
	}
}

// WriteAnthropicError writes an Anthropic-formatted error response.
func WriteAnthropicError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(oai.Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": msg,
		},
	}))
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

func mapAnthropicStopReason(finish string) string {
	switch finish {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return finish
	}
}

// AnthropicTranslator translates OpenAI Chat Completions responses and streams
// into Anthropic Messages format.
type AnthropicTranslator struct {
	Model string

	blockIdx      int
	openKind      string
	openTools     map[int]int
	openToolOrder []int
	stopReason    string
	usage         oai.Usage
}

// NewAnthropicTranslator creates a per-request Anthropic response translator.
func NewAnthropicTranslator(requestedModel string) *AnthropicTranslator {
	return &AnthropicTranslator{
		Model:      requestedModel,
		blockIdx:   -1,
		stopReason: "end_turn",
	}
}

func (t *AnthropicTranslator) WriteError(w http.ResponseWriter, status int, body []byte) {
	msg := string(body)
	if o, err := oai.Decode(body); err == nil {
		if e, ok := o["error"].(map[string]any); ok {
			if m := oai.Str(e, "message"); m != "" {
				msg = m
			}
		}
	}
	WriteAnthropicError(w, status, msg)
}

func (t *AnthropicTranslator) WriteJSON(w http.ResponseWriter, body []byte) {
	o, err := oai.Decode(body)
	if err != nil {
		WriteAnthropicError(w, http.StatusBadGateway, "upstream returned invalid JSON")
		return
	}
	content := []any{}
	stopReason := "end_turn"
	if choices, _ := o["choices"].([]any); len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			stopReason = mapAnthropicStopReason(oai.Str(c0, "finish_reason"))
			if msg, ok := c0["message"].(map[string]any); ok {
				if rc := oai.Str(msg, "reasoning_content"); rc != "" {
					content = append(content, map[string]any{
						"type":      "thinking",
						"thinking":  rc,
						"signature": "",
					})
				}
				if txt := oai.Str(msg, "content"); txt != "" {
					content = append(content, map[string]any{
						"type": "text",
						"text": txt,
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, rawTC := range tcs {
						tc, _ := rawTC.(map[string]any)
						fn, _ := tc["function"].(map[string]any)
						inp := map[string]any{}
						if args := oai.Str(fn, "arguments"); args != "" {
							if decoded, derr := oai.Decode([]byte(args)); derr == nil {
								inp = decoded
							}
						}
						content = append(content, map[string]any{
							"type":  "tool_use",
							"id":    oai.Str(tc, "id"),
							"name":  oai.Str(fn, "name"),
							"input": inp,
						})
					}
				}
			}
		}
	}
	u, _ := oai.UsageOf(o)
	resp := map[string]any{
		"id":            "msg_" + reqIDFromWriter(w),
		"type":          "message",
		"role":          "assistant",
		"model":         t.Model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":            u.PromptTokens,
			"output_tokens":           u.CompletionTokens,
			"cache_read_input_tokens": u.CachedTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(oai.Encode(resp))
}

func (t *AnthropicTranslator) emitEvent(w http.ResponseWriter, name string, data map[string]any) {
	data["type"] = name
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, oai.Encode(data))
}

func (t *AnthropicTranslator) StartStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	t.emitEvent(w, "message_start", map[string]any{
		"message": map[string]any{
			"id":            "msg_" + reqIDFromWriter(w),
			"type":          "message",
			"role":          "assistant",
			"model":         t.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":            0,
				"output_tokens":           0,
				"cache_read_input_tokens": 0,
			},
		},
	})
}

func (t *AnthropicTranslator) closeBlock(w http.ResponseWriter) {
	if t.openKind == "" {
		return
	}
	t.emitEvent(w, "content_block_stop", map[string]any{
		"index": t.blockIdx,
	})
	t.openKind = ""
}

func (t *AnthropicTranslator) closeOpenTools(w http.ResponseWriter) {
	for _, tcIdx := range t.openToolOrder {
		bIdx, ok := t.openTools[tcIdx]
		if !ok {
			continue
		}
		t.emitEvent(w, "content_block_stop", map[string]any{
			"index": bIdx,
		})
	}
	t.openTools = nil
	t.openToolOrder = nil
}

func (t *AnthropicTranslator) startBlock(w http.ResponseWriter, kind string, block map[string]any) {
	t.closeBlock(w)
	t.closeOpenTools(w)
	t.blockIdx++
	t.openKind = kind
	t.emitEvent(w, "content_block_start", map[string]any{
		"index":         t.blockIdx,
		"content_block": block,
	})
}

func (t *AnthropicTranslator) OnChunk(w http.ResponseWriter, chunk oai.Object) {
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
		t.stopReason = mapAnthropicStopReason(fr)
	}
	delta, _ := c0["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if rc := oai.Str(delta, "reasoning_content"); rc != "" {
		if t.openKind != "thinking" {
			t.startBlock(w, "thinking", map[string]any{"type": "thinking", "thinking": ""})
		}
		t.emitEvent(w, "content_block_delta", map[string]any{
			"index": t.blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
		})
	}
	if txt := oai.Str(delta, "content"); txt != "" {
		if t.openKind != "text" {
			t.startBlock(w, "text", map[string]any{"type": "text", "text": ""})
		}
		t.emitEvent(w, "content_block_delta", map[string]any{
			"index": t.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": txt},
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
			t.closeBlock(w)
			bIdx, exists := t.openTools[tcIdx]
			if !exists {
				t.blockIdx++
				bIdx = t.blockIdx
				if t.openTools == nil {
					t.openTools = map[int]int{}
				}
				t.openTools[tcIdx] = bIdx
				t.openToolOrder = append(t.openToolOrder, tcIdx)
				t.emitEvent(w, "content_block_start", map[string]any{
					"index": bIdx,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": map[string]any{},
					},
				})
			}
			if args != "" {
				t.emitEvent(w, "content_block_delta", map[string]any{
					"index": bIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
				})
			}
		}
	}
}

func (t *AnthropicTranslator) EndStream(w http.ResponseWriter, ok bool) {
	t.closeBlock(w)
	t.closeOpenTools(w)
	if !ok {
		t.emitEvent(w, "error", map[string]any{
			"error": map[string]any{
				"type":    "api_error",
				"message": "upstream stream interrupted",
			},
		})
		return
	}
	t.emitEvent(w, "message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   t.stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":            t.usage.PromptTokens,
			"output_tokens":           t.usage.CompletionTokens,
			"cache_read_input_tokens": t.usage.CachedTokens,
		},
	})
	t.emitEvent(w, "message_stop", map[string]any{})
}
