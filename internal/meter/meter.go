// Package meter records per-token usage in SQLite and summarises it.
package meter

import (
	"database/sql"
	"sync"
	"time"
)

// Record is one metered upstream call. It never contains prompt or response text.
type Record struct {
	RequestID        string
	Time             time.Time
	TokenID          string
	TokenName        string
	RequestedModel   string
	Model            string // <provider>/<model> actually used
	Target           string // local | external
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	Cost             float64
	Status           int
	Latency          time.Duration
}

// Exporter receives every record after it is stored (e.g. OTLP).
type Exporter interface {
	Export(Record)
}

// Meter stores records.
type Meter struct {
	db        *sql.DB
	mu        sync.Mutex
	exporters []Exporter
}

// New wraps an opened database.
func New(db *sql.DB) *Meter { return &Meter{db: db} }

// AddExporter registers an exporter.
func (m *Meter) AddExporter(e Exporter) { m.exporters = append(m.exporters, e) }

// Add stores a record and forwards it to exporters.
func (m *Meter) Add(r Record) error {
	m.mu.Lock()
	_, err := m.db.Exec(`INSERT INTO usage(request_id,ts,day,token_id,token_name,requested_model,model,target,prompt_tokens,completion_tokens,cached_tokens,cost,status,latency_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RequestID, r.Time.Unix(), r.Time.Local().Format("2006-01-02"), r.TokenID, r.TokenName, r.RequestedModel, r.Model, r.Target,
		r.PromptTokens, r.CompletionTokens, r.CachedTokens, r.Cost, r.Status, r.Latency.Milliseconds())
	m.mu.Unlock()
	if err != nil {
		return err
	}
	for _, e := range m.exporters {
		e.Export(r)
	}
	return nil
}

// TokensUsed returns the total tokens attributed to a token id.
func (m *Meter) TokensUsed(tokenID string) (int64, error) {
	var n sql.NullInt64
	err := m.db.QueryRow(`SELECT SUM(prompt_tokens+completion_tokens) FROM usage WHERE token_id=?`, tokenID).Scan(&n)
	return n.Int64, err
}

// Row is one line of a usage report.
type Row struct {
	Day              string  `json:"day"`
	TokenID          string  `json:"token_id"`
	TokenName        string  `json:"token_name"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
}

// Daily summarises usage by day, token and model, from `since` (inclusive, YYYY-MM-DD; empty = all).
func (m *Meter) Daily(since string) ([]Row, error) {
	rows, err := m.db.Query(`SELECT day, token_id, token_name, model, COUNT(*), SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),
		SUM(prompt_tokens), SUM(completion_tokens), SUM(cost)
		FROM usage WHERE day >= ? GROUP BY day, token_id, token_name, model ORDER BY day, token_name, model`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Day, &r.TokenID, &r.TokenName, &r.Model, &r.Requests, &r.Errors, &r.PromptTokens, &r.CompletionTokens, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestLogEntry is one row written to request_log per client request.
type RequestLogEntry struct {
	RequestID        string
	Time             time.Time
	KeyID            string
	KeyName          string
	Protocol         string
	ClientIP         string
	RequestedModel   string
	Model            string
	Status           int
	ErrorCode        string
	Stream           bool
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	LatencyMS        int64
	TTFTMS           int64
	Admitted         bool
	RedactionMode    string
	RedactionCount   int
	RouteBackend     string
	RouteReason      string
}

// LogRequest records one client request in request_log.
func (m *Meter) LogRequest(e RequestLogEntry) error {
	stream := 0
	if e.Stream {
		stream = 1
	}
	admitted := 0
	if e.Admitted {
		admitted = 1
	}
	m.mu.Lock()
	_, err := m.db.Exec(`INSERT INTO request_log(
		request_id,ts,day,key_id,key_name,protocol,client_ip,requested_model,model,
		status,error_code,stream,prompt_tokens,completion_tokens,cached_tokens,latency_ms,ttft_ms,admitted,
		redaction_mode,redaction_count,route_backend,route_reason)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.RequestID, e.Time.Unix(), e.Time.Local().Format("2006-01-02"), e.KeyID, e.KeyName,
		e.Protocol, e.ClientIP, e.RequestedModel, e.Model,
		e.Status, e.ErrorCode, stream, e.PromptTokens, e.CompletionTokens, e.CachedTokens,
		e.LatencyMS, e.TTFTMS, admitted, e.RedactionMode, e.RedactionCount, e.RouteBackend, e.RouteReason)
	m.mu.Unlock()
	return err
}

// PruneRequestLog deletes request_log rows older than days. Zero disables pruning.
func (m *Meter) PruneRequestLog(days int, now time.Time) error {
	if days <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	m.mu.Lock()
	_, err := m.db.Exec(`DELETE FROM request_log WHERE ts < ?`, cutoff)
	m.mu.Unlock()
	return err
}
