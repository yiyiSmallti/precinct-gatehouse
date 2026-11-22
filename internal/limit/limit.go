// Package limit enforces per-key request-count rate limits (concurrency, RPM
// and daily RPD) in memory, recovering daily counts from request_log on start.
package limit

import (
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// Error describes a rate-limit rejection.
type Error struct {
	Err        *oai.Error
	RetryAfter int // seconds, >0 for RPM rejections
}

// Snapshot is a point-in-time view of a key's live limit counters.
type Snapshot struct {
	Inflight  int `json:"inflight"`
	RPMUsed   int `json:"rpm_used"`
	TodayUsed int `json:"today_used"`
}

type keyState struct {
	inflight      int
	rpm           []time.Time
	day           string
	todayCount    int
	todayReserved int
	initialized   bool
}

// Limiter enforces concurrency, RPM and RPD limits per API key.
type Limiter struct {
	db     *sql.DB
	mu     sync.Mutex
	states map[string]*keyState
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
	var count int
	if l.db != nil {
		if err := l.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE key_id=? AND day=? AND admitted=1 AND status<500`, keyID, day).Scan(&count); err != nil {
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
		ks.initialized = true
	}
	return nil
}

// Acquire checks concurrency → RPM → RPD and admits one request if allowed.
// The caller must invoke release(countable) when the request completes;
// countable should be true when the final HTTP status is < 500.
func (l *Limiter) Acquire(keyID string, lim keys.Limits, now time.Time) (func(countable bool), *Error) {
	day := now.Local().Format("2006-01-02")
	if err := l.ensureDay(keyID, day); err != nil {
		return nil, &Error{Err: oai.Errorf(http.StatusInternalServerError, "limiter_error", "%v", err)}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ks := l.states[keyID]
	if ks.day != day {
		ks.day = day
		ks.todayCount = 0
		ks.todayReserved = 0
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

	ks.inflight++
	ks.rpm = append(ks.rpm, now)
	ks.todayReserved++

	var once sync.Once
	return func(countable bool) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if ks.inflight > 0 {
				ks.inflight--
			}
			if ks.todayReserved > 0 {
				ks.todayReserved--
			}
			if countable && ks.day == day {
				ks.todayCount++
			}
		})
	}, nil
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
	if ks.day == day {
		todayUsed = ks.todayCount + ks.todayReserved
	}
	return Snapshot{
		Inflight:  ks.inflight,
		RPMUsed:   len(ks.rpm),
		TodayUsed: todayUsed,
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
