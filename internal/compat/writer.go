// Package compat translates Anthropic Messages and OpenAI Responses requests
// and responses to/from the OpenAI Chat Completions pipeline.
package compat

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// Translator converts pipeline Chat Completions responses and errors into a
// client-facing wire protocol.
type Translator interface {
	WriteError(w http.ResponseWriter, status int, body []byte)
	WriteJSON(w http.ResponseWriter, body []byte)
	StartStream(w http.ResponseWriter)
	OnChunk(w http.ResponseWriter, chunk oai.Object)
	EndStream(w http.ResponseWriter, ok bool)
}

// Writer wraps an http.ResponseWriter, buffering JSON/error bodies and
// streaming SSE chunks through a Translator while preserving gateway headers.
type Writer struct {
	w         http.ResponseWriter
	tr        Translator
	hdr       http.Header
	status    int
	streaming bool
	buf       bytes.Buffer
	sseBuf    bytes.Buffer
	sawDone   bool
	sawFinish bool
	finished  bool
}

// NewWriter creates a translating ResponseWriter around w.
func NewWriter(w http.ResponseWriter, tr Translator) *Writer {
	return &Writer{
		w:   w,
		tr:  tr,
		hdr: make(http.Header),
	}
}

func (cw *Writer) Header() http.Header { return cw.hdr }

func (cw *Writer) copyHeaders() {
	dst := cw.w.Header()
	for k, vals := range cw.hdr {
		if strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		dst[k] = append([]string(nil), vals...)
	}
}

func (cw *Writer) WriteHeader(status int) {
	if cw.status != 0 {
		return
	}
	cw.status = status
	cw.copyHeaders()
	ct := cw.hdr.Get("Content-Type")
	if status == http.StatusOK && strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		cw.streaming = true
		cw.tr.StartStream(cw.w)
		cw.Flush()
	}
}

func (cw *Writer) Write(p []byte) (int, error) {
	if cw.status == 0 {
		cw.WriteHeader(http.StatusOK)
	}
	if !cw.streaming {
		return cw.buf.Write(p)
	}
	cw.sseBuf.Write(p)
	for {
		b := cw.sseBuf.Bytes()
		idx := bytes.IndexByte(b, '\n')
		if idx < 0 {
			break
		}
		line := string(bytes.TrimRight(b[:idx], "\r"))
		cw.sseBuf.Next(idx + 1)
		cw.processLine(line)
	}
	return len(p), nil
}

func (cw *Writer) processLine(line string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		cw.sawDone = true
		return
	}
	if payload[0] != '{' {
		return
	}
	chunk, err := oai.Decode([]byte(payload))
	if err != nil {
		return
	}
	if ch, _ := chunk["choices"].([]any); len(ch) > 0 {
		for _, c := range ch {
			if cm, _ := c.(map[string]any); cm != nil && cm["finish_reason"] != nil {
				cw.sawFinish = true
			}
		}
	}
	cw.tr.OnChunk(cw.w, chunk)
}

// Flush forwards flushes to the underlying ResponseWriter when streaming.
func (cw *Writer) Flush() {
	if cw.streaming {
		if fl, ok := cw.w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// Finish completes buffered JSON/error responses or terminates an SSE stream.
func (cw *Writer) Finish() {
	if cw.finished {
		return
	}
	cw.finished = true
	if cw.status == 0 {
		return
	}
	cw.copyHeaders()
	if cw.status != http.StatusOK {
		cw.tr.WriteError(cw.w, cw.status, cw.buf.Bytes())
		return
	}
	if cw.streaming {
		if cw.sseBuf.Len() > 0 {
			cw.processLine(strings.TrimRight(cw.sseBuf.String(), "\r"))
			cw.sseBuf.Reset()
		}
		cw.tr.EndStream(cw.w, cw.sawDone || cw.sawFinish)
		cw.Flush()
		return
	}
	cw.tr.WriteJSON(cw.w, cw.buf.Bytes())
}

func reqIDFromWriter(w http.ResponseWriter) string {
	id := w.Header().Get("X-Gatehouse-Request-Id")
	return strings.TrimPrefix(id, "req_")
}
