package limit

import (
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
)

func TestCrossMidnightReleasePreservesNewDayReservation(t *testing.T) {
	lim := New(nil)
	lims := keys.Limits{RPD: 1, Concurrency: 2}

	day1 := time.Date(2026, 11, 22, 23, 59, 50, 0, time.Local)
	releaseA, err := lim.Acquire("key_1", lims, day1)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}

	day2 := time.Date(2026, 11, 23, 0, 0, 5, 0, time.Local)
	releaseB, err := lim.Acquire("key_1", lims, day2)
	if err != nil {
		t.Fatalf("acquire B after midnight: %v", err)
	}

	// Old day's request finishes on day 2: must not decrement day 2's todayReserved.
	releaseA(true, 0)

	snap := lim.Snapshot("key_1", day2)
	if snap.TodayUsed != 1 || snap.Inflight != 1 {
		t.Fatalf("unexpected snapshot after cross-midnight releaseA: %+v", snap)
	}

	if _, lerr := lim.Acquire("key_1", lims, day2); lerr == nil || lerr.Err.Code != "daily_limit_exceeded" {
		t.Fatalf("expected daily_limit_exceeded while B is still pending, got %+v", lerr)
	}

	releaseB(true, 0)
	snapAfterB := lim.Snapshot("key_1", day2)
	if snapAfterB.TodayUsed != 1 || snapAfterB.Inflight != 0 {
		t.Fatalf("unexpected snapshot after releaseB: %+v", snapAfterB)
	}
}
