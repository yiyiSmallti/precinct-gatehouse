package redact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

func overlapReq(text string) oai.Object {
	return oai.Object{"messages": []any{map[string]any{"role": "user", "content": text}}}
}

// A dictionary term that starts before an overlapping secret must not hide the
// secret's block policy.
func TestOverlapKeepsBlockingFinding(t *testing.T) {
	e, err := New(config.Redaction{Terms: []string{"password: hunter2"}, Categories: map[string]string{"term": "mask", "secret": "block"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Apply(context.Background(), overlapReq("password: hunter2hunter2"), ModeMask, []byte("k"), "v")
	var re *Error
	if !errors.As(err, &re) || re.Status != 400 {
		t.Fatalf("want block, got %v", err)
	}
}

// Overlapping masked findings are replaced by one placeholder covering both.
func TestOverlapMasksUnion(t *testing.T) {
	e, err := New(config.Redaction{Terms: []string{"password: hunter2"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := overlapReq("password: hunter2hunter2 end")
	if _, err := e.Apply(context.Background(), req, ModeMask, []byte("k"), "v"); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "hunter2") || !strings.HasSuffix(got, " end") {
		t.Fatalf("not fully masked: %q", got)
	}
}

// A finding that covers the synthetic "key: " prefix of a tool argument still
// masks the value.
func TestToolArgumentKeyPrefixFinding(t *testing.T) {
	e, err := New(config.Redaction{Terms: []string{"password: hunter2"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := oai.Object{"messages": []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "login", "arguments": `{"password":"hunter2hunter2"}`}},
	}}}}
	if _, err := e.Apply(context.Background(), req, ModeMask, []byte("k"), "v"); err != nil {
		t.Fatal(err)
	}
	args := req["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(args, "hunter2") || !strings.HasPrefix(args, `{"password":"{{`) {
		t.Fatalf("not masked: %s", args)
	}
}

// A malformed tool_calls entry is rejected, not a panic.
func TestMalformedToolCall(t *testing.T) {
	e, err := New(config.Redaction{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := oai.Object{"messages": []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{nil}}}}
	_, err = e.Apply(context.Background(), req, ModeMask, []byte("k"), "v")
	var re *Error
	if !errors.As(err, &re) || re.Status != 400 {
		t.Fatalf("want 400, got %v", err)
	}
}
