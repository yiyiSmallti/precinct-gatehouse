package compat

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// ResponsesMeta is what DecodeResponses learns about a request besides the
// Chat Completions body.
type ResponsesMeta struct {
	Model string
	// DroppedTools are tool types the request declared that Chat Completions
	// cannot carry.
	DroppedTools []string
	// DroppedItems are input item types that were skipped.
	DroppedItems []string
	// CustomTools names the `custom` (freeform) tools, which are sent as
	// functions taking one `input` string; calls to them are turned back
	// into custom_tool_call items.
	CustomTools map[string]bool
}

// DecodeResponses translates an OpenAI /v1/responses JSON body into an OpenAI
// Chat Completions request object.
func DecodeResponses(body []byte) (oai.Object, ResponsesMeta, error) {
	var meta ResponsesMeta
	in, err := oai.Decode(body)
	if err != nil {
		return nil, meta, err
	}
	reqModel := oai.Str(in, "model")
	meta.Model = reqModel
	if prev := oai.Str(in, "previous_response_id"); prev != "" {
		return nil, meta, oai.Errorf(http.StatusBadRequest, "previous_response_not_supported", "previous_response_id is not supported")
	}
	out := oai.Object{"model": reqModel}

	var msgs []any
	if inst := oai.Str(in, "instructions"); inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	// Chat Completions only allows image parts in user messages, so images a
	// tool returns are held here and sent in one user message right after the
	// run of tool messages; the tool messages stay adjacent to their
	// assistant tool_calls.
	var toolImages []any
	flushToolImages := func() {
		if len(toolImages) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": toolImages})
			toolImages = nil
		}
	}
	var extraTools []any
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
				flushToolImages()
				role := oai.Str(item, "role")
				if role == "developer" {
					role = "system"
				}
				content := convertResponseInputContent(item["content"])
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			case typ == "function_call" || typ == "custom_tool_call" || typ == "local_shell_call" || typ == "shell_call":
				flushToolImages()
				callID := oai.Str(item, "call_id")
				if callID == "" {
					callID = oai.Str(item, "id")
				}
				name, args := oai.Str(item, "name"), oai.Str(item, "arguments")
				switch typ {
				case "custom_tool_call":
					args = string(oai.Encode(map[string]any{"input": oai.Str(item, "input")}))
				case "local_shell_call", "shell_call":
					name = strings.TrimSuffix(typ, "_call")
					args = string(oai.Encode(item["action"]))
				}
				tc := map[string]any{
					"id":       callID,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				}
				// calls follow the assistant text of the same turn: keep them
				// in one assistant message
				if len(msgs) > 0 {
					if prev, ok := msgs[len(msgs)-1].(map[string]any); ok && oai.Str(prev, "role") == "assistant" {
						prevTCs, _ := prev["tool_calls"].([]any)
						prev["tool_calls"] = append(prevTCs, tc)
						continue
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				})
			case typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "local_shell_call_output" || typ == "shell_call_output":
				callID := oai.Str(item, "call_id")
				if callID == "" {
					callID = oai.Str(item, "id")
				}
				text, images := splitToolOutput(item["output"])
				if len(images) > 0 {
					if text == "" {
						text = toolImagePlaceholder
					}
					toolImages = append(toolImages, map[string]any{"type": "text", "text": oai.ToolImageLabel + callID + ":"})
					toolImages = append(toolImages, images...)
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      text,
				})
			case typ == "additional_tools":
				// Codex Desktop sends tool definitions as an input item; they
				// join the request's tools.
				if ts, ok := item["tools"].([]any); ok {
					extraTools = append(extraTools, ts...)
				}
			case typ == "reasoning":
				// dropped
			case typ == "compaction":
				// a compacted history: keep it when it has readable text
				if txt := compactionText(item); txt != "" {
					flushToolImages()
					msgs = append(msgs, map[string]any{"role": "user", "content": compactionPrefix + txt})
				} else {
					meta.DroppedItems = appendUnique(meta.DroppedItems, typ)
				}
			case typ == "":
				return nil, meta, oai.Errorf(http.StatusBadRequest, "unsupported_input_item", "input item has no type")
			default:
				// server-side tool calls (web_search_call, file_search_call,
				// ...), item_reference and unknown items cannot be replayed
				// statelessly; skip them instead of failing the session
				meta.DroppedItems = appendUnique(meta.DroppedItems, typ)
			}
		}
		flushToolImages()
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}

	var dropped []string
	tools, _ := in["tools"].([]any)
	tools = append(tools[:len(tools):len(tools)], extraTools...)
	if len(tools) > 0 {
		var chatTools []any
		for _, rt := range tools {
			t, ok := rt.(map[string]any)
			if !ok {
				continue
			}
			tt := oai.Str(t, "type")
			if tt == "custom" {
				if meta.CustomTools == nil {
					meta.CustomTools = map[string]bool{}
				}
				meta.CustomTools[oai.Str(t, "name")] = true
				chatTools = append(chatTools, customToolFunction(t))
				continue
			}
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
		if tt := oai.Str(tc, "type"); tt == "function" || tt == "custom" {
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
	meta.DroppedTools = dropped
	return out, meta, nil
}

const compactionPrefix = "[Summary of the earlier conversation]\n"

// compactionText returns the readable summary of a compaction item, if any;
// encrypted_content is opaque and ignored.
func compactionText(item map[string]any) string {
	for _, k := range []string{"summary", "content", "text"} {
		switch v := item[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			var parts []string
			for _, it := range v {
				if m, ok := it.(map[string]any); ok {
					if t := oai.Str(m, "text"); t != "" {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
	}
	return ""
}

// customToolFunction turns a `custom` (freeform) tool into a function with a
// single string argument `input`, describing the expected input format.
func customToolFunction(t map[string]any) map[string]any {
	desc := oai.Str(t, "description")
	note := "Call this tool with the raw tool input as the `input` string argument."
	if f, ok := t["format"].(map[string]any); ok && oai.Str(f, "type") == "grammar" {
		note += " The input must follow this " + oai.Str(f, "syntax") + " grammar:\n" + oai.Str(f, "definition")
	}
	if desc != "" {
		desc += "\n\n"
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        oai.Str(t, "name"),
		"description": desc + note,
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"input": map[string]any{"type": "string"}},
			"required":   []any{"input"},
		},
	}}
}

// customToolInput unwraps the `input` argument of a call to a custom tool.
func customToolInput(args string) string {
	if v, err := oai.DecodeValue([]byte(args)); err == nil {
		if m, ok := v.(map[string]any); ok {
			if s, ok := m["input"].(string); ok {
				return s
			}
		}
	}
	return args
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
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
			case "input_image", "image_url", "image":
				if img := imagePart(p); img != nil {
					parts = append(parts, img)
				}
			case "input_file":
				parts = append(parts, inputFilePart(p))
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

// inputFilePart converts an `input_file` part into an image part for image
// files and into a text part (extracted text or a notice) otherwise.
func inputFilePart(p map[string]any) map[string]any {
	if fd := oai.Str(p, "file_data"); strings.HasPrefix(fd, "data:image/") {
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": fd}}
	}
	return map[string]any{"type": "text", "text": documentText(responsesInputFile(p))}
}

// imagePart converts a Responses or Chat image content part
// (`input_image`/`image_url`/`image`, with `image_url` as a string or
// {url, detail}, or a bare `url`) into a Chat `image_url` part, or nil when it
// carries no URL.
func imagePart(p map[string]any) map[string]any {
	var url, detail string
	switch u := p["image_url"].(type) {
	case string:
		url = u
	case map[string]any:
		url, detail = oai.Str(u, "url"), oai.Str(u, "detail")
	}
	if url == "" {
		url = oai.Str(p, "url")
	}
	if url == "" {
		url = inlineImageURL(p)
	}
	if url == "" {
		return nil
	}
	if detail == "" {
		detail = oai.Str(p, "detail")
	}
	iu := map[string]any{"url": url}
	if detail != "" {
		iu["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": iu}
}

// inlineImageURL builds a data URI from an image given as base64 data plus a
// media type: MCP's {data, mimeType} or Anthropic's
// {source: {type: base64, media_type, data}} / {source: {type: url, url}}.
func inlineImageURL(p map[string]any) string {
	if src, ok := p["source"].(map[string]any); ok {
		if oai.Str(src, "type") == "url" {
			return oai.Str(src, "url")
		}
		p = src
	}
	data := oai.Str(p, "data")
	mime := oai.Str(p, "mimeType")
	if mime == "" {
		mime = oai.Str(p, "mime_type")
	}
	if mime == "" {
		mime = oai.Str(p, "media_type")
	}
	if data == "" || !strings.HasPrefix(mime, "image/") {
		return ""
	}
	return "data:" + mime + ";base64," + data
}

const (
	toolImagePlaceholder   = "[image returned by tool; attached in the next user message]"
	inlineImagePlaceholder = "[image]"
	// inlineImageMin is the smallest base64 payload pulled out of tool output
	// text as an image; shorter data URIs stay text.
	inlineImageMin = 4096
)

// splitToolOutput turns a function_call_output `output` into the tool
// message's text and the image parts it returned. Images must not stay in
// the tool message: Chat Completions rejects image parts outside user
// messages and providers that render tool results as text tokenize the whole
// base64 payload.
func splitToolOutput(v any) (string, []any) {
	switch ov := v.(type) {
	case nil:
		return "", nil
	case string:
		// Codex sends a content array JSON-encoded into the string.
		t := strings.TrimSpace(ov)
		if (strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{")) &&
			strings.Contains(t, "image") {
			// Only an output that carries an image is read as content parts;
			// any other JSON result stays verbatim.
			if parsed, err := oai.DecodeValue([]byte(t)); err == nil {
				if text, images, ok := splitContentParts(parsed); ok && len(images) > 0 {
					return text, images
				}
			}
		}
		return extractInlineImages(ov)
	default:
		if text, images, ok := splitContentParts(ov); ok {
			return text, images
		}
		return extractInlineImages(string(oai.Encode(ov)))
	}
}

// splitContentParts reads v as one content part or an array of them. ok is
// false when v holds no recognised part, so that an ordinary JSON result is
// kept as text instead of being dropped.
func splitContentParts(v any) (text string, images []any, ok bool) {
	var parts []any
	switch t := v.(type) {
	case []any:
		parts = t
	case map[string]any:
		parts = []any{t}
	default:
		return "", nil, false
	}
	var texts []string
	// keepRaw keeps a part this adapter does not convert as text, so that
	// pulling the images out of an output never loses the rest of it
	keepRaw := func(raw any) {
		s, ok := raw.(string)
		if !ok {
			s = string(oai.Encode(raw))
		}
		s, imgs := extractInlineImages(s)
		if s != "" {
			texts = append(texts, s)
		}
		images = append(images, imgs...)
	}
	for _, raw := range parts {
		p, isObj := raw.(map[string]any)
		if !isObj {
			keepRaw(raw)
			continue
		}
		switch oai.Str(p, "type") {
		case "input_text", "output_text", "text":
			ok = true
			s, imgs := extractInlineImages(oai.Str(p, "text"))
			if s != "" {
				texts = append(texts, s)
			}
			images = append(images, imgs...)
		case "input_file":
			ok = true
			fp := inputFilePart(p)
			if oai.Str(fp, "type") == "image_url" {
				images = append(images, fp)
			} else {
				texts = append(texts, oai.Str(fp, "text"))
			}
		case "input_image", "image_url", "image":
			ok = true
			if img := imagePart(p); img != nil {
				images = append(images, img)
			} else {
				keepRaw(p)
			}
		default:
			keepRaw(p)
		}
	}
	return strings.Join(texts, "\n"), images, ok
}

// extractInlineImages moves large `data:image/...;base64,` URIs out of s into
// image parts, leaving a short placeholder in the text.
func extractInlineImages(s string) (string, []any) {
	spans := oai.DataImageSpans(s, inlineImageMin, false)
	if len(spans) == 0 {
		return s, nil
	}
	var b strings.Builder
	var images []any
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp[0]])
		b.WriteString(inlineImagePlaceholder)
		images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": s[sp[0]:sp[1]]}})
		last = sp[1]
	}
	b.WriteString(s[last:])
	return b.String(), images
}

type openRespTool struct {
	custom      bool
	outputIndex int
	itemID      string
	callID      string
	name        string
	args        strings.Builder
}

// ResponsesTranslator translates OpenAI Chat Completions responses and streams
// into OpenAI Responses format.
type ResponsesTranslator struct {
	Model string
	// CustomTools names the request's custom tools (see ResponsesMeta).
	CustomTools map[string]bool

	respID         string
	createdAt      int64
	seq            int
	outputIndex    int
	openKind       string
	openItemID     string
	openText       strings.Builder
	openTools      map[int]*openRespTool
	openToolOrder  []int
	completedItems []any
	finishReason   string
	usage          oai.Usage
}

// NewResponsesTranslator creates a per-request OpenAI Responses translator.
func NewResponsesTranslator(requestedModel string) *ResponsesTranslator {
	return &ResponsesTranslator{
		Model:          requestedModel,
		outputIndex:    -1,
		completedItems: []any{},
	}
}

// callItem builds a function_call item, or a custom_tool_call item when name
// is one of the request's custom tools.
func (t *ResponsesTranslator) callItem(id, callID, name, args, status string) map[string]any {
	if t.CustomTools[name] {
		input := ""
		if args != "" {
			input = customToolInput(args)
		}
		return map[string]any{"type": "custom_tool_call", "id": id, "call_id": callID, "name": name, "input": input, "status": status}
	}
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": name, "arguments": args, "status": status}
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
					output = append(output, t.callItem(fmt.Sprintf("fc_%s_%d", idSuffix, i), oai.Str(tc, "id"), oai.Str(fn, "name"), oai.Str(fn, "arguments"), "completed"))
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
	}
	t.openKind = ""
	t.openText.Reset()
}

func (t *ResponsesTranslator) closeOpenTools(w http.ResponseWriter) {
	for _, tcIdx := range t.openToolOrder {
		st := t.openTools[tcIdx]
		if st == nil {
			continue
		}
		text := st.args.String()
		if st.custom {
			input := customToolInput(text)
			t.emitEvent(w, "response.custom_tool_call_input.delta", map[string]any{
				"item_id":      st.itemID,
				"output_index": st.outputIndex,
				"delta":        input,
			})
			t.emitEvent(w, "response.custom_tool_call_input.done", map[string]any{
				"item_id":      st.itemID,
				"output_index": st.outputIndex,
				"input":        input,
			})
		} else {
			t.emitEvent(w, "response.function_call_arguments.done", map[string]any{
				"item_id":      st.itemID,
				"output_index": st.outputIndex,
				"arguments":    text,
			})
		}
		item := t.callItem(st.itemID, st.callID, st.name, text, "completed")
		t.emitEvent(w, "response.output_item.done", map[string]any{
			"output_index": st.outputIndex,
			"item":         item,
		})
		t.completedItems = append(t.completedItems, item)
	}
	t.openTools = nil
	t.openToolOrder = nil
}

func (t *ResponsesTranslator) startReasoning(w http.ResponseWriter) {
	t.closeOpenItem(w)
	t.closeOpenTools(w)
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
	t.closeOpenTools(w)
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

func (t *ResponsesTranslator) startFunctionCall(w http.ResponseWriter, tcIdx int, callID, name string) *openRespTool {
	t.closeOpenItem(w)
	t.outputIndex++
	st := &openRespTool{
		outputIndex: t.outputIndex,
		itemID:      fmt.Sprintf("fc_%s_%d", reqIDFromWriter(w), t.outputIndex),
		callID:      callID,
		name:        name,
	}
	if t.openTools == nil {
		t.openTools = map[int]*openRespTool{}
	}
	st.custom = t.CustomTools[name]
	t.openTools[tcIdx] = st
	t.openToolOrder = append(t.openToolOrder, tcIdx)
	t.emitEvent(w, "response.output_item.added", map[string]any{
		"output_index": st.outputIndex,
		"item":         t.callItem(st.itemID, callID, name, "", "in_progress"),
	})
	return st
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
			st := t.openTools[tcIdx]
			if st == nil {
				st = t.startFunctionCall(w, tcIdx, id, name)
			} else {
				if st.callID == "" && id != "" {
					st.callID = id
				}
				if st.name == "" && name != "" {
					st.name = name
				}
			}
			if args != "" {
				st.args.WriteString(args)
				if st.custom {
					// a custom tool's input is unwrapped from the arguments
					// JSON once they are complete
					continue
				}
				t.emitEvent(w, "response.function_call_arguments.delta", map[string]any{
					"item_id":      st.itemID,
					"output_index": st.outputIndex,
					"delta":        args,
				})
			}
		}
	}
}

func (t *ResponsesTranslator) EndStream(w http.ResponseWriter, ok bool) {
	t.closeOpenItem(w)
	t.closeOpenTools(w)
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
