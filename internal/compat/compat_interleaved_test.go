package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

type sseEvt struct {
	Event string
	Data  map[string]any
}

func parseCompatSSE(raw []byte) []sseEvt {
	var out []sseEvt
	sc := bufio.NewScanner(bytes.NewReader(raw))
	curEv := ""
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			curEv = ""
			continue
		}
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			curEv = ev
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var obj map[string]any
			_ = json.Unmarshal([]byte(data), &obj)
			out = append(out, sseEvt{Event: curEv, Data: obj})
		}
	}
	return out
}

func TestResponsesTranslatorInterleavedToolCalls(t *testing.T) {
	tr := NewResponsesTranslator("gpt-5")
	rec := httptest.NewRecorder()
	tr.StartStream(rec)

	chunk1 := oai.Object{
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": 0, "id": "call_0", "function": map[string]any{"name": "tool_a", "arguments": "{\"a\":"}},
						map[string]any{"index": 1, "id": "call_1", "function": map[string]any{"name": "tool_b", "arguments": "{\"b\":"}},
					},
				},
			},
		},
	}
	chunk2 := oai.Object{
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}},
						map[string]any{"index": 1, "function": map[string]any{"arguments": "2}"}},
					},
				},
			},
		},
	}
	tr.OnChunk(rec, chunk1)
	tr.OnChunk(rec, chunk2)
	tr.EndStream(rec, true)

	evts := parseCompatSSE(rec.Body.Bytes())
	var completed map[string]any
	addedCount := 0
	for _, ev := range evts {
		if ev.Event == "response.output_item.added" {
			addedCount++
		}
		if ev.Event == "response.completed" {
			completed, _ = ev.Data["response"].(map[string]any)
		}
	}
	if addedCount != 2 {
		t.Fatalf("expected 2 output_item.added events, got %d", addedCount)
	}
	output, _ := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 completed items, got %d: %#v", len(output), output)
	}
	want := []struct {
		callID string
		name   string
		args   string
	}{
		{"call_0", "tool_a", `{"a":1}`},
		{"call_1", "tool_b", `{"b":2}`},
	}
	for i, w := range want {
		item, _ := output[i].(map[string]any)
		if item["call_id"] != w.callID || item["name"] != w.name || item["arguments"] != w.args {
			t.Fatalf("item %d mismatch: got call_id=%v name=%v args=%v, want %+v",
				i, item["call_id"], item["name"], item["arguments"], w)
		}
	}
}

func TestAnthropicTranslatorInterleavedToolCalls(t *testing.T) {
	tr := NewAnthropicTranslator("claude-sonnet-4-5")
	rec := httptest.NewRecorder()
	tr.StartStream(rec)

	chunk1 := oai.Object{
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": 0, "id": "call_0", "function": map[string]any{"name": "tool_a", "arguments": "{\"a\":"}},
						map[string]any{"index": 1, "id": "call_1", "function": map[string]any{"name": "tool_b", "arguments": "{\"b\":"}},
					},
				},
			},
		},
	}
	chunk2 := oai.Object{
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}},
						map[string]any{"index": 1, "function": map[string]any{"arguments": "2}"}},
					},
				},
			},
		},
	}
	tr.OnChunk(rec, chunk1)
	tr.OnChunk(rec, chunk2)
	tr.EndStream(rec, true)

	evts := parseCompatSSE(rec.Body.Bytes())
	starts := map[int]map[string]any{}
	deltas := map[int]*strings.Builder{}
	stops := map[int]bool{}
	for _, ev := range evts {
		switch ev.Event {
		case "content_block_start":
			idx := int(ev.Data["index"].(float64))
			blk, _ := ev.Data["content_block"].(map[string]any)
			starts[idx] = blk
		case "content_block_delta":
			idx := int(ev.Data["index"].(float64))
			d, _ := ev.Data["delta"].(map[string]any)
			if deltas[idx] == nil {
				deltas[idx] = &strings.Builder{}
			}
			deltas[idx].WriteString(d["partial_json"].(string))
		case "content_block_stop":
			idx := int(ev.Data["index"].(float64))
			stops[idx] = true
		}
	}
	if len(starts) != 2 || len(stops) != 2 {
		t.Fatalf("expected 2 started and 2 stopped blocks, starts=%d stops=%d", len(starts), len(stops))
	}
	if starts[0]["id"] != "call_0" || starts[0]["name"] != "tool_a" || deltas[0].String() != `{"a":1}` {
		t.Fatalf("block 0 mismatch: start=%v delta=%q", starts[0], deltas[0].String())
	}
	if starts[1]["id"] != "call_1" || starts[1]["name"] != "tool_b" || deltas[1].String() != `{"b":2}` {
		t.Fatalf("block 1 mismatch: start=%v delta=%q", starts[1], deltas[1].String())
	}
}
