package limit

import (
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
)

// Failure modes covered (a ledger write the database rejects):
//  1. the charge still counts in memory, so the quota keeps blocking;
//  2. a day-rollover reload while writes keep failing does not forget it;
//  3. once the database recovers, the next charge flushes the backlog into
//     the ledger exactly once (no loss, no double count);
//  4. clearing usage also drops the backlog.
func TestLedgerWriteFailureKeepsCharge(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	failWrites := func(on bool) {
		t.Helper()
		q := `DROP TRIGGER IF EXISTS fail_ledger`
		if on {
			q = `CREATE TRIGGER fail_ledger BEFORE INSERT ON key_token_usage BEGIN SELECT RAISE(ABORT, 'injected'); END`
		}
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ledgerTotal := func() int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(`SELECT COALESCE(SUM(tokens),0) FROM key_token_usage WHERE key_id='key_x'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	lim := New(db)
	lims := keys.Limits{TokensTotal: 100}
	day1 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.Local)
	day2 := day1.AddDate(0, 0, 1)

	failWrites(true)
	rel, e := lim.Acquire("key_x", lims, day1)
	if e != nil {
		t.Fatalf("first acquire: %+v", e)
	}
	rel(true, 150)
	if got := ledgerTotal(); got != 0 {
		t.Fatalf("injected failure should keep the ledger empty, got %d", got)
	}
	// 1. Memory still enforces the quota.
	if _, e := lim.Acquire("key_x", lims, day1); e == nil || e.Err.Code != "token_quota_exceeded" {
		t.Fatalf("mode 1: want token_quota_exceeded, got %+v", e)
	}
	// 2. Rollover reload with the ledger still failing keeps the charge.
	if _, e := lim.Acquire("key_x", lims, day2); e == nil || e.Err.Code != "token_quota_exceeded" {
		t.Fatalf("mode 2: want token_quota_exceeded after rollover, got %+v", e)
	}
	if s := lim.Snapshot("key_x", day2); s.TokensTotal != 150 {
		t.Fatalf("mode 2: total after rollover = %d, want 150", s.TokensTotal)
	}

	// 3. Recovery: the next charge flushes the backlog once.
	failWrites(false)
	lims.TokensTotal = 0
	rel, e = lim.Acquire("key_x", lims, day2)
	if e != nil {
		t.Fatalf("acquire after lifting quota: %+v", e)
	}
	rel(true, 10)
	if got := ledgerTotal(); got != 160 {
		t.Fatalf("mode 3: ledger after recovery = %d, want 160", got)
	}
	fresh := New(db) // a restart reloads from the ledger alone
	if s := fresh.Snapshot("key_x", day2); s.TokensTotal != 160 || s.TokensToday != 10 {
		t.Fatalf("mode 3: reload after recovery = %+v, want total 160, today 10", s)
	}

	// 4. Clearing usage drops a pending backlog too.
	failWrites(true)
	rel, _ = lim.Acquire("key_x", lims, day2)
	rel(true, 40)
	if err := lim.ResetTokens("key_x"); err != nil {
		t.Fatal(err)
	}
	failWrites(false)
	if s := lim.Snapshot("key_x", day2.AddDate(0, 0, 1)); s.TokensTotal != 0 {
		t.Fatalf("mode 4: total after reset + rollover = %d, want 0", s.TokensTotal)
	}
	if got := ledgerTotal(); got != 0 {
		t.Fatalf("mode 4: ledger after reset = %d, want 0", got)
	}
}
