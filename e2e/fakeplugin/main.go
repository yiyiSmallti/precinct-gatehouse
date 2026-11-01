// Command fakeplugin serves the non-detector extension points for the
// end-to-end tests. The first argument picks the kind:
//
//	fakeplugin policy <tokens.json>  AuthPolicy: token -> context from the file;
//	                                 "deny" -> 401, "forbid" -> 403, "crash" exits the process
//	fakeplugin audit <out.jsonl>     AuditSink: appends every record to the file
//	fakeplugin provider              Provider: echoes the last user message, JSON or SSE
//	fakeplugin hooks                 Hooks: appends " [hooked]" to the last user message;
//	                                 "#hookmodel" in it makes the hook (illegally) change the model
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fakeplugin policy|audit|provider|hooks [file]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "policy":
		plugin.Serve(plugin.Set{AuthPolicy: &policy{file: os.Args[2]}})
	case "audit":
		plugin.Serve(plugin.Set{AuditSink: &sink{file: os.Args[2]}})
	case "provider":
		plugin.Serve(plugin.Set{Provider: provider{}})
	case "hooks":
		plugin.Serve(plugin.Set{Hooks: hooks{}})
	}
}

// ---------- policy ----------

type entry struct {
	Tenant          string   `json:"tenant"`
	User            string   `json:"user"`
	Department      string   `json:"department"`
	DataClass       string   `json:"data_class"`
	AllowedGroups   []string `json:"allowed_groups"`
	Models          []string `json:"models"`
	TokensRemaining *int64   `json:"tokens_remaining"`
	Action          string   `json:"action"` // deny | forbid | crash
}

type policy struct{ file string }

func (p *policy) Resolve(_ context.Context, tok string, meta *plugin.RequestMeta) (*plugin.GatewayContext, error) {
	b, err := os.ReadFile(p.file)
	if err != nil {
		return nil, err
	}
	var table map[string]entry
	if err := json.Unmarshal(b, &table); err != nil {
		return nil, err
	}
	e, ok := table[tok]
	if !ok {
		return nil, plugin.Unauthenticated("unknown token")
	}
	switch e.Action {
	case "deny":
		return nil, plugin.Unauthenticated("token revoked")
	case "forbid":
		return nil, plugin.Forbidden("not allowed")
	case "crash":
		os.Exit(3)
	}
	gc := &plugin.GatewayContext{
		Tenant: e.Tenant, User: e.User, Department: e.Department, DataClass: e.DataClass,
		AllowedGroups: e.AllowedGroups, Models: e.Models, PolicyVersion: 7,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(), ByoApi: &plugin.ByoApi{},
	}
	if e.TokensRemaining != nil {
		gc.Budget = &plugin.Budget{HasTokensRemaining: true, TokensRemaining: *e.TokensRemaining, Period: "month"}
	}
	return gc, nil
}

// ---------- audit ----------

type sink struct {
	file string
	mu   sync.Mutex
}

func (s *sink) Write(_ context.Context, records [][]byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	for _, r := range records {
		f.Write(append(r, '\n'))
	}
	return len(records), nil
}

// ---------- provider ----------

type provider struct{}

func lastUser(body map[string]any) string {
	ms, _ := body["messages"].([]any)
	for i := len(ms) - 1; i >= 0; i-- {
		m, _ := ms[i].(map[string]any)
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

func (provider) Chat(_ context.Context, r *plugin.ChatRequest, send func(*plugin.ChatResponse) error) error {
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return send(&plugin.ChatResponse{Status: 400, Data: []byte(`{"error":{"message":"bad json"}}`)})
	}
	content := fmt.Sprintf("[plugin/%s] %s", r.Model, lastUser(body))
	usage := map[string]any{"prompt_tokens": len(r.Body) / 10, "completion_tokens": len(content) / 4, "total_tokens": len(r.Body)/10 + len(content)/4}
	if s, _ := body["stream"].(bool); s {
		if err := send(&plugin.ChatResponse{Status: 200, ContentType: "text/event-stream"}); err != nil {
			return err
		}
		for _, ch := range []map[string]any{
			{"id": "p1", "object": "chat.completion.chunk", "model": r.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}}}},
			{"id": "p1", "object": "chat.completion.chunk", "model": r.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
			{"id": "p1", "object": "chat.completion.chunk", "model": r.Model, "choices": []any{}, "usage": usage},
		} {
			b, _ := json.Marshal(ch)
			if err := send(&plugin.ChatResponse{Data: []byte("data: " + string(b) + "\n\n")}); err != nil {
				return err
			}
		}
		return send(&plugin.ChatResponse{Data: []byte("data: [DONE]\n\n")})
	}
	b, _ := json.Marshal(map[string]any{
		"id": "p1", "object": "chat.completion", "created": time.Now().Unix(), "model": r.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   usage,
	})
	return send(&plugin.ChatResponse{Status: 200, ContentType: "application/json", Data: b})
}

// ---------- hooks ----------

type hooks struct{}

func (hooks) OnRequest(_ context.Context, r *plugin.HookRequest) (*plugin.HookRequestResult, error) {
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return nil, err
	}
	ms, _ := body["messages"].([]any)
	for i := len(ms) - 1; i >= 0; i-- {
		m, _ := ms[i].(map[string]any)
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			if strings.Contains(s, "#hookmodel") {
				body["model"] = "local/b"
			}
			m["content"] = s + " [hooked:" + r.GetCtx().GetDepartment() + "]"
			break
		}
	}
	b, _ := json.Marshal(body)
	return &plugin.HookRequestResult{Body: b}, nil
}

func (hooks) OnResponse(context.Context, *plugin.HookResponse) error { return nil }
