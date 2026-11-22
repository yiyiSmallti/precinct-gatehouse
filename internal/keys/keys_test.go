package keys

import (
	"errors"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
)

func TestLookupDoesNotCacheStaleRowAcrossLocalInvalidation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	s := NewStore(db)
	defer s.Close()

	// 1. Overlapping SetEnabled(false) must prevent caching the old enabled row.
	plain1, k1, err := s.Create(CreateOpts{Name: "disable-race"})
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	s.testAfterLookupScan = func() {
		s.testAfterLookupScan = nil
		if err := s.SetEnabled(k1.ID, false); err != nil {
			t.Errorf("SetEnabled(false): %v", err)
		}
	}
	// First Lookup scanned the enabled row before SetEnabled; subsequent Lookup must see ErrDisabled.
	if _, err := s.Lookup(plain1); err != nil {
		t.Fatalf("overlapping Lookup 1: %v", err)
	}
	if _, err := s.Lookup(plain1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled after SetEnabled(false), got %v", err)
	}

	// 2. Overlapping Delete must prevent caching the old active row.
	plain2, k2, err := s.Create(CreateOpts{Name: "delete-race"})
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	s.testAfterLookupScan = func() {
		s.testAfterLookupScan = nil
		if err := s.Delete(k2.ID); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	if _, err := s.Lookup(plain2); err != nil {
		t.Fatalf("overlapping Lookup 2: %v", err)
	}
	if _, err := s.Lookup(plain2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}

	// 3. Overlapping Reset must prevent caching the old secret while the new secret succeeds.
	plain3, k3, err := s.Create(CreateOpts{Name: "reset-race"})
	if err != nil {
		t.Fatalf("Create 3: %v", err)
	}
	var newPlain3 string
	s.testAfterLookupScan = func() {
		s.testAfterLookupScan = nil
		var rerr error
		newPlain3, _, rerr = s.Reset(k3.ID)
		if rerr != nil {
			t.Errorf("Reset: %v", rerr)
		}
	}
	if _, err := s.Lookup(plain3); err != nil {
		t.Fatalf("overlapping Lookup 3: %v", err)
	}
	if _, err := s.Lookup(plain3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for old secret after Reset, got %v", err)
	}
	if _, err := s.Lookup(newPlain3); err != nil {
		t.Fatalf("expected new secret after Reset to succeed, got %v", err)
	}
}
