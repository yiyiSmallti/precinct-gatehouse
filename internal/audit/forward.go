package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// BatchWriter is an AuditSink plugin (plugin.AuditSink has this method).
type BatchWriter interface {
	Write(ctx context.Context, records [][]byte) (int, error)
}

// Forwarder writes each record to a primary sink (the local file) and queues
// it for AuditSink plugins. Plugins get batches from a background goroutine,
// so a slow or dead sink never delays a request; failed batches are retried
// with backoff and the queue is bounded (the oldest records are dropped and
// counted; the local file keeps them).
type Forwarder struct {
	Primary Sink
	sinks   []*fwdSink
	log     *slog.Logger
}

type fwdSink struct {
	name    string
	w       BatchWriter
	mu      sync.Mutex
	queue   [][]byte
	dropped int
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}
}

// QueueLimit bounds the per-sink queue.
const QueueLimit = 10000

// BatchSize is the most records per Write (Cloister accepts up to 500).
const BatchSize = 200

// NewForwarder starts one goroutine per plugin sink.
func NewForwarder(primary Sink, log *slog.Logger) *Forwarder {
	return &Forwarder{Primary: primary, log: log}
}

// Add registers a plugin sink.
func (f *Forwarder) Add(name string, w BatchWriter) {
	s := &fwdSink{name: name, w: w, wake: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{})}
	f.sinks = append(f.sinks, s)
	go s.run(f.log)
}

// Write implements Sink.
func (f *Forwarder) Write(r Record) error {
	err := f.Primary.Write(r)
	if len(f.sinks) == 0 {
		return err
	}
	b, jerr := json.Marshal(r)
	if jerr != nil {
		return jerr
	}
	for _, s := range f.sinks {
		s.mu.Lock()
		if len(s.queue) >= QueueLimit {
			s.queue = s.queue[1:]
			s.dropped++
		}
		s.queue = append(s.queue, b)
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return err
}

// Close flushes what it can within timeout and stops the goroutines.
func (f *Forwarder) Close(timeout time.Duration) {
	for _, s := range f.sinks {
		close(s.done)
	}
	deadline := time.After(timeout)
	for _, s := range f.sinks {
		select {
		case <-s.stopped:
		case <-deadline:
			return
		}
	}
}

func (s *fwdSink) run(log *slog.Logger) {
	defer close(s.stopped)
	backoff := time.Duration(0)
	for {
		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-s.done:
				s.flushOnce(log) // one last try
				return
			}
		} else {
			select {
			case <-s.wake:
			case <-time.After(time.Second):
			case <-s.done:
				s.flushOnce(log)
				return
			}
		}
		if s.flushOnce(log) {
			backoff = 0
		} else if backoff == 0 {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// flushOnce sends queued batches until the queue is empty or a write fails.
func (s *fwdSink) flushOnce(log *slog.Logger) bool {
	for {
		s.mu.Lock()
		n := min(len(s.queue), BatchSize)
		batch := append([][]byte(nil), s.queue[:n]...)
		dropped := s.dropped
		s.dropped = 0
		s.mu.Unlock()
		if dropped > 0 {
			log.Warn("audit sink queue full; oldest records dropped (still in the local audit file)", "sink", s.name, "dropped", dropped)
		}
		if n == 0 {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.w.Write(ctx, batch)
		cancel()
		if err != nil {
			log.Warn("audit sink write failed; will retry", "sink", s.name, "records", n, "err", err)
			return false
		}
		s.mu.Lock()
		// drop exactly what was sent (the queue may have shifted if it overflowed)
		if len(s.queue) >= n {
			s.queue = s.queue[n:]
		} else {
			s.queue = s.queue[:0]
		}
		s.mu.Unlock()
	}
}
