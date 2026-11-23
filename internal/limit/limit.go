// Package limit enforces per-key limits in memory: request counts
// (concurrency, RPM, daily RPD) and token quotas (daily, monthly, total).
// Daily request counts are recovered from request_log; token usage lives in
// the key_token_usage ledger, which is not pruned with request_log.
package limit

import (
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// Error describes a rate-limit rejection.
type Error struct {
	Err        *oai.Error
	RetryAfter int // seconds, >0 for RPM and daily/monthly token quota rejections
}

// Snapshot is a point-in-time view of a key's live limit counters.
type Snapshot struct {
	Inflight  int `json:"inflight"`
	RPMUsed   int `json:"rpm_used"`
	TodayUsed int `json:"today_used"`

	TokensToday int64 `json:"tokens_today"`
	TokensMonth int64 `json:"tokens_month"`
	TokensTotal int64 `json:"tokens_total"`
}

type keyState struct {
	inflight      int
	rpm           []time.Time
	day           string
	todayCount    int
	todayReserved int
	initialized   bool

	tokToday int64
	tokMonth int64
	tokTotal int64
}

// Limiter enforces concurrency, RPM and RPD limits per API key.
type Limiter struct {
	db     *sql.DB
	mu     sync.Mutex
	states map[string]*keyState

	// ledgerMu orders token-ledger writes against ledger reloads. A charge
	// (ledger write + in-memory increment) is therefore either fully before
	// a reload's query, so the snapshot includes it, or fully after the
	// reload's assignment, so it adds on top; a stale snapshot can never
	// overwrite a live charge. Lock order: ledgerMu, then mu. mu is still
	// never held across database access.
	ledgerMu sync.Mutex
}

// New creates a Limiter backed by db for daily count initialization.
func New(db *sql.DB) *Limiter {
	return &Limiter{
		db:     db,
		states: map[string]*keyState{},
	}
}

func (l *Limiter) ensureDay(keyID, day string) error {
	l.mu.Lock()
	ks := l.states[keyID]
	ready := ks != nil && ks.initialized && ks.day == day
	l.mu.Unlock()
	if ready {
		return nil
	}

	// Never hold l.mu across database access.
	l.ledgerMu.Lock()
	defer l.ledgerMu.Unlock()
	var count int
	var tokToday, tokMonth, tokTotal int64
	if l.db != nil {
		if err := l.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE key_id=? AND day=? AND admitted=1 AND status<500`, keyID, day).Scan(&count); err != nil {
			return err
		}
		if err := l.db.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN day=? THEN tokens END),0),
			COALESCE(SUM(CASE WHEN substr(day,1,7)=? THEN tokens END),0),
			COALESCE(SUM(tokens),0)
			FROM key_token_usage WHERE key_id=?`, day, day[:7], keyID).Scan(&tokToday, &tokMonth, &tokTotal); err != nil {
			return err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	ks = l.states[keyID]
	if ks == nil {
		ks = &keyState{}
		l.states[keyID] = ks
	}
	if !ks.initialized || ks.day != day {
		ks.day = day
		ks.todayCount = count
		ks.todayReserved = 0
		ks.tokToday, ks.tokMonth, ks.tokTotal = tokToday, tokMonth, tokTotal
		ks.initialized = true
	}
	return nil
}

// Acquire checks concurrency → RPM → RPD → token quotas and admits one
// request if allowed. The caller must invoke release(countable, tokens) when
// the request completes; countable should be true when the final HTTP status
// is < 500, and tokens is the prompt + completion tokens the request used.
func (l *Limiter) Acquire(keyID string, lim keys.Limits, now time.Time) (func(countable bool, tokens int64), *Error) {
	day := now.Local().Format("2006-01-02")
	if err := l.ensureDay(keyID, day); err != nil {
		return nil, &Error{Err: oai.Errorf(http.StatusInternalServerError, "limiter_error", "%v", err)}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ks := l.states[keyID]
	if ks.day != day {
		// ensureDay just loaded this day; a rollover between the two locks
		// is reloaded on the next request.
		if len(ks.day) < 7 || ks.day[:7] != day[:7] {
			ks.tokMonth = 0
		}
		ks.day = day
		ks.todayCount = 0
		ks.todayReserved = 0
		ks.tokToday = 0
	}

	if lim.Concurrency > 0 && ks.inflight >= lim.Concurrency {
		return nil, &Error{
			Err: oai.Errorf(http.StatusTooManyRequests, "concurrency_limit_exceeded",
				"concurrency limit (%d) reached for key", lim.Concurrency),
		}
	}

	pruneRPM(ks, now)
	if lim.RPM > 0 && len(ks.rpm) >= lim.RPM {
		wait := ks.rpm[0].Add(time.Minute).Sub(now)
		sec := int((wait + time.Second - 1) / time.Second)
		if sec < 1 {
			sec = 1
		}
		return nil, &Error{
			Err: oai.Errorf(http.StatusTooManyRequests, "rate_limit_exceeded",
				"rate limit (%d rpm) exceeded for key; retry after %ds", lim.RPM, sec),
			RetryAfter: sec,
		}
	}

	if lim.RPD > 0 && (ks.todayCount+ks.todayReserved) >= lim.RPD {
		return nil, &Error{
			Err: oai.Errorf(http.StatusTooManyRequests, "daily_limit_exceeded",
				"daily request limit (%d) reached for key", lim.RPD),
		}
	}

	if e := tokenQuotaError(ks, lim, now); e != nil {
		return nil, e
	}

	ks.inflight++
	ks.rpm = append(ks.rpm, now)
	ks.todayReserved++

	var once sync.Once
	return func(countable bool, tokens int64) {
		once.Do(func() {
			l.mu.Lock()
			if ks.inflight > 0 {
				ks.inflight--
			}
			if ks.day == day {
				if ks.todayReserved > 0 {
					ks.todayReserved--
				}
				if countable {
					ks.todayCount++
				}
			}
			l.mu.Unlock()
			if tokens <= 0 {
				return
			}
			l.ledgerMu.Lock()
			defer l.ledgerMu.Unlock()
			if l.db != nil {
				// Charge the day the request was admitted on, matching the
				// in-memory counters below.
				_, _ = l.db.Exec(`INSERT INTO key_token_usage(key_id, day, tokens) VALUES(?,?,?)
					ON CONFLICT(key_id, day) DO UPDATE SET tokens = tokens + excluded.tokens`, keyID, day, tokens)
			}
			l.mu.Lock()
			if ks.day == day {
				ks.tokToday += tokens
			}
			if len(ks.day) >= 7 && ks.day[:7] == day[:7] {
				ks.tokMonth += tokens
			}
			ks.tokTotal += tokens
			l.mu.Unlock()
		})
	}, nil
}

// tokenQuotaError reports the first exhausted token quota, or nil.
func tokenQuotaError(ks *keyState, lim keys.Limits, now time.Time) *Error {
	local := now.Local()
	switch {
	case lim.TokensDaily > 0 && ks.tokToday >= lim.TokensDaily:
		next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, local.Location())
		return quotaErr("daily", ks.tokToday, lim.TokensDaily, next.Sub(local))
	case lim.TokensMonthly > 0 && ks.tokMonth >= lim.TokensMonthly:
		next := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, local.Location())
		return quotaErr("monthly", ks.tokMonth, lim.TokensMonthly, next.Sub(local))
	case lim.TokensTotal > 0 && ks.tokTotal >= lim.TokensTotal:
		return quotaErr("total", ks.tokTotal, lim.TokensTotal, 0)
	}
	return nil
}

func quotaErr(period string, used, limit int64, wait time.Duration) *Error {
	e := &Error{Err: oai.Errorf(http.StatusTooManyRequests, "token_quota_exceeded",
		"%s token quota exhausted for key: used %d of %d tokens", period, used, limit)}
	if wait > 0 {
		e.RetryAfter = int((wait + time.Second - 1) / time.Second)
		e.Err.Message += fmt.Sprintf("; resets in %ds", e.RetryAfter)
	}
	return e
}

// ResetTokens forgets all recorded token usage for keyID, so every token
// quota of the key starts again from zero.
func (l *Limiter) ResetTokens(keyID string) error {
	l.ledgerMu.Lock()
	defer l.ledgerMu.Unlock()
	if l.db != nil {
		if _, err := l.db.Exec(`DELETE FROM key_token_usage WHERE key_id=?`, keyID); err != nil {
			return err
		}
	}
	l.mu.Lock()
	if ks := l.states[keyID]; ks != nil {
		ks.tokToday, ks.tokMonth, ks.tokTotal = 0, 0, 0
	}
	l.mu.Unlock()
	return nil
}

// Snapshot returns live usage counters for keyID.
func (l *Limiter) Snapshot(keyID string, now time.Time) Snapshot {
	day := now.Local().Format("2006-01-02")
	_ = l.ensureDay(keyID, day)

	l.mu.Lock()
	defer l.mu.Unlock()
	ks := l.states[keyID]
	if ks == nil {
		return Snapshot{}
	}
	pruneRPM(ks, now)
	todayUsed := 0
	var tokToday, tokMonth int64
	if ks.day == day {
		todayUsed = ks.todayCount + ks.todayReserved
		tokToday = ks.tokToday
	}
	if ks.day[:7] == day[:7] {
		tokMonth = ks.tokMonth
	}
	return Snapshot{
		Inflight:    ks.inflight,
		RPMUsed:     len(ks.rpm),
		TodayUsed:   todayUsed,
		TokensToday: tokToday,
		TokensMonth: tokMonth,
		TokensTotal: ks.tokTotal,
	}
}

func pruneRPM(ks *keyState, now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(ks.rpm) && !ks.rpm[i].After(cutoff) {
		i++
	}
	if i > 0 {
		copy(ks.rpm, ks.rpm[i:])
		ks.rpm = ks.rpm[:len(ks.rpm)-i]
	}
}
