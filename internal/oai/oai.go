// Package oai holds the small slice of the OpenAI Chat Completions wire format
// that Gatehouse needs to read or rewrite. Requests and responses are kept as
// generic JSON objects so that fields Gatehouse does not know pass through.
package oai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Object is a decoded JSON object that preserves unknown fields.
type Object = map[string]any

// Decode parses a JSON object, keeping numbers as json.Number.
func Decode(b []byte) (Object, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var o Object
	if err := d.Decode(&o); err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return o, nil
}

// DecodeValue parses any JSON value, keeping numbers as json.Number.
func DecodeValue(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data")
	}
	return v, nil
}

// Encode serialises without HTML escaping.
func Encode(v any) []byte {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Clone deep-copies a JSON value.
func Clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = Clone(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = Clone(x)
		}
		return s
	default:
		return v
	}
}

// Str returns o[k] as a string.
func Str(o Object, k string) string {
	s, _ := o[k].(string)
	return s
}

// Bool returns o[k] as a bool.
func Bool(o Object, k string) bool {
	b, _ := o[k].(bool)
	return b
}

// Int returns o[k] as an int (0 when absent).
func Int(o Object, k string) int {
	switch n := o[k].(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// Messages returns the request messages as objects.
func Messages(req Object) []Object {
	raw, _ := req["messages"].([]any)
	out := make([]Object, 0, len(raw))
	for _, m := range raw {
		if o, ok := m.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// HasImage reports whether any message carries an image content part.
func HasImage(req Object) bool {
	for _, m := range Messages(req) {
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			if po, ok := p.(map[string]any); ok {
				t := Str(po, "type")
				if t == "image_url" || t == "input_image" || t == "image" {
					return true
				}
			}
		}
	}
	return false
}

// Text concatenates the textual content of a message.
func Text(m Object) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if po, ok := p.(map[string]any); ok && Str(po, "type") == "text" {
				b.WriteString(Str(po, "text"))
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	return ""
}

// EstimateTokens is a cheap prompt-size estimate used by routing rules:
// roughly one token per CJK character and one per four other characters.
func EstimateTokens(req Object) int {
	n := 0
	for _, m := range Messages(req) {
		n += 4
		s := Text(m)
		if tc, ok := m["tool_calls"].([]any); ok {
			s += string(Encode(tc))
		}
		other := 0
		for _, r := range s {
			if r >= 0x2E80 && utf8.RuneLen(r) >= 3 {
				n++
			} else {
				other++
			}
		}
		n += (other + 3) / 4
	}
	return n
}

// LastUserText returns the text of the last user message.
func LastUserText(req Object) string {
	ms := Messages(req)
	for i := len(ms) - 1; i >= 0; i-- {
		if Str(ms[i], "role") == "user" {
			return Text(ms[i])
		}
	}
	return ""
}

// Usage is the token usage reported by a provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
}

// UsageOf reads resp["usage"].
func UsageOf(resp Object) (Usage, bool) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return Usage{}, false
	}
	us := Usage{PromptTokens: Int(u, "prompt_tokens"), CompletionTokens: Int(u, "completion_tokens"), TotalTokens: Int(u, "total_tokens")}
	if ptd, ok := u["prompt_tokens_details"].(map[string]any); ok {
		us.CachedTokens = Int(ptd, "cached_tokens")
	}
	if us.TotalTokens == 0 {
		us.TotalTokens = us.PromptTokens + us.CompletionTokens
	}
	return us, true
}

// Error is an OpenAI-style error response.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// Errorf builds an Error.
func Errorf(status int, code, format string, a ...any) *Error {
	typ := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		typ = "authentication_error"
	case status == http.StatusForbidden:
		typ = "permission_error"
	case status == http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case status >= 500:
		typ = "api_error"
	}
	return &Error{Status: status, Message: fmt.Sprintf(format, a...), Type: typ, Code: code}
}

// WriteError writes err as an OpenAI error body.
func WriteError(w http.ResponseWriter, err *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.Status)
	_, _ = w.Write(Encode(map[string]any{"error": err}))
}
