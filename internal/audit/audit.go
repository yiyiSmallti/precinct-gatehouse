// Package audit writes one record per request. Records hold the decision path
// and redaction category counts only: never prompt text, response text or the
// placeholder-to-value mapping.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one audited request.
type Record struct {
	Time             time.Time  `json:"time"`
	RequestID        string     `json:"request_id"`
	TokenID          string     `json:"token_id,omitempty"`
	TokenName        string     `json:"token_name,omitempty"`
	Tenant           string     `json:"tenant,omitempty"` // org mode: from the policy context
	User             string     `json:"user,omitempty"`
	Department       string     `json:"department,omitempty"`
	OnBehalfOf       string     `json:"on_behalf_of,omitempty"`
	InstanceID       string     `json:"instance_id,omitempty"`
	WorkspaceID      string     `json:"workspace_id,omitempty"`
	PolicyVersion    int64      `json:"policy_version,omitempty"`
	Source           string     `json:"source,omitempty"`
	RequestedModel   string     `json:"requested_model,omitempty"`
	DataClass        string     `json:"data_class,omitempty"`
	Group            string     `json:"group,omitempty"`
	Rule             string     `json:"rule,omitempty"`
	RuleReason       string     `json:"rule_reason,omitempty"`
	Effort           string     `json:"effort,omitempty"`
	Attempts         []Attempt  `json:"attempts,omitempty"`
	Model            string     `json:"model,omitempty"`
	Target           string     `json:"target,omitempty"`
	Redaction        *Redaction `json:"redaction,omitempty"`
	Status           int        `json:"status"`
	Error            string     `json:"error,omitempty"`
	PromptTokens     int        `json:"prompt_tokens,omitempty"`
	CompletionTokens int        `json:"completion_tokens,omitempty"`
	LatencyMS        int64      `json:"latency_ms"`
	BodyReadMS       int64      `json:"body_read_ms,omitempty"`
	RequestBytes     int64      `json:"request_bytes,omitempty"`
}

// Attempt is one upstream try during failover.
type Attempt struct {
	Model  string `json:"model"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Redaction summarises the redaction stage.
type Redaction struct {
	Mode     string         `json:"mode"`
	Counts   map[string]int `json:"counts,omitempty"` // category -> findings acted on (masked/warned/blocked)
	Masked   int            `json:"masked,omitempty"`
	Warned   int            `json:"warned,omitempty"`
	Blocked  bool           `json:"blocked,omitempty"`
	Restored int            `json:"restored,omitempty"`
	Samples  []Sample       `json:"samples,omitempty"`
	Alerts   []string       `json:"alerts,omitempty"`
}

// Sample describes one finding without its value: category and length only.
type Sample struct {
	Kind   string `json:"kind"`
	Length int    `json:"length"`
	Action string `json:"action"`
}

// Sink receives audit records.
type Sink interface {
	Write(Record) error
}

// FileSink appends JSON lines to <dir>/YYYY-MM-DD.jsonl.
type FileSink struct {
	dir string
	mu  sync.Mutex
}

// NewFileSink creates the directory.
func NewFileSink(dir string) (*FileSink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileSink{dir: dir}, nil
}

// Write appends one record.
func (s *FileSink) Write(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, r.Time.Local().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
