package tiered

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	zhThinkAnchorMarker = "强制思考规范提醒"
	zhThinkAnchorText   = "【强制思考规范提醒】接下来你在 <think>...</think> 块中的内部推理思考必须全程使用简体中文；最终回复语言与用户提问一致。"
)

func marshalNoEscapeHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func extractRawContentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			}
		}
		return sb.String()
	}
	return ""
}

func systemRequiresZhThinking(rawMsgs []json.RawMessage) bool {
	for _, rm := range rawMsgs {
		var hdr struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rm, &hdr); err != nil {
			continue
		}
		if hdr.Role != "system" && hdr.Role != "developer" {
			continue
		}
		txt := extractRawContentText(hdr.Content)
		if strings.Contains(txt, "【强制规范】") && strings.Contains(txt, "简体中文") {
			return true
		}
		if strings.Contains(txt, "简体中文") &&
			(strings.Contains(txt, "<think>") || strings.Contains(txt, "内部推理") || strings.Contains(txt, "思考")) {
			return true
		}
	}
	return false
}

func isThinkingDisabled(obj map[string]json.RawMessage) bool {
	if raw, ok := obj["reasoning_effort"]; ok {
		var eff string
		if json.Unmarshal(raw, &eff) == nil {
			switch strings.ToLower(strings.TrimSpace(eff)) {
			case "none", "off", "disabled", "false":
				return true
			}
		}
	}
	if raw, ok := obj["enable_thinking"]; ok {
		var en bool
		if json.Unmarshal(raw, &en) == nil && !en {
			return true
		}
	}
	if raw, ok := obj["thinking"]; ok {
		var en bool
		if json.Unmarshal(raw, &en) == nil && !en {
			return true
		}
		var th struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &th) == nil && strings.EqualFold(strings.TrimSpace(th.Type), "disabled") {
			return true
		}
	}
	if raw, ok := obj["chat_template_kwargs"]; ok {
		var kw struct {
			EnableThinking *bool `json:"enable_thinking"`
		}
		if json.Unmarshal(raw, &kw) == nil && kw.EnableThinking != nil && !*kw.EnableThinking {
			return true
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := obj[key]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil && n == 1 {
				return true
			}
		}
	}
	return false
}

// injectZhThinkAnchor appends a tiny trailing system reminder AFTER the final
// user/tool message so existing KV prefix bytes stay intact.
func injectZhThinkAnchor(body []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	if isThinkingDisabled(obj) {
		return body, false
	}
	rawMsgsField, ok := obj["messages"]
	if !ok {
		return body, false
	}
	var rawMsgs []json.RawMessage
	if err := json.Unmarshal(rawMsgsField, &rawMsgs); err != nil || len(rawMsgs) < 2 {
		return body, false
	}
	if !systemRequiresZhThinking(rawMsgs) {
		return body, false
	}

	lastIdx := len(rawMsgs) - 1
	var lastHdr struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rawMsgs[lastIdx], &lastHdr); err != nil {
		return body, false
	}
	if lastHdr.Role != "user" && lastHdr.Role != "tool" {
		return body, false
	}
	if strings.Contains(extractRawContentText(lastHdr.Content), zhThinkAnchorMarker) {
		return body, false
	}

	anchorMsg, err := marshalNoEscapeHTML(map[string]string{
		"role":    "system",
		"content": zhThinkAnchorText,
	})
	if err != nil {
		return body, false
	}
	rawMsgs = append(rawMsgs, anchorMsg)
	encMsgs, err := marshalNoEscapeHTML(rawMsgs)
	if err != nil {
		return body, false
	}
	obj["messages"] = encMsgs
	out, err := marshalNoEscapeHTML(obj)
	if err != nil {
		return body, false
	}
	return out, true
}
