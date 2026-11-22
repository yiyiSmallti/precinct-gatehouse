package tiered

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestIsContextLengthErrorMatchesMaxCompletionTokens(t *testing.T) {
	msg := `{"error":{"message":"max_completion_tokens is too large: 384000.This model supports at most 204800 completion tokens."}}`
	if !isContextLengthError(http.StatusBadRequest, []byte(msg)) {
		t.Fatalf("expected isContextLengthError=true for max_completion_tokens error")
	}
	if isContextLengthError(http.StatusBadRequest, []byte(`{"error":{"message":"invalid api key"}}`)) {
		t.Fatalf("unexpected true for non-context error")
	}
}

func TestClampMaxTokensForBackend(t *testing.T) {
	orig := []byte(`{"model":"DeepSeek-V4.1-Flash-1017","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":384000}`)
	out, clamped := clampMaxTokensForBackend(orig, 5000, 384000, 204800, false)
	if clamped <= 150000 || clamped >= 204800 {
		t.Fatalf("expected clamped in (150000, 204800), got %d", clamped)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	if int(parsed["max_completion_tokens"].(float64)) != clamped {
		t.Fatalf("json field mismatch: %v vs %d", parsed["max_completion_tokens"], clamped)
	}

	orig2 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`)
	out2, clamped2 := clampMaxTokensForBackend(orig2, 5000, 4096, 204800, false)
	if clamped2 != 0 || string(out2) != string(orig2) {
		t.Fatalf("expected no clamp, got clamped=%d", clamped2)
	}

	out3, clamped3 := clampMaxTokensForBackend(orig, 291000, 384000, 524288, false)
	if clamped3 <= 150000 || clamped3 >= 233288 {
		t.Fatalf("expected 512K clamped headroom ~192K, got %d (out=%s)", clamped3, string(out3))
	}
}
