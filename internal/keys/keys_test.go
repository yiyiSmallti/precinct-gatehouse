package keys

import (
	"errors"
	"strings"
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

func TestEncryptedKeySecretPersistenceAndGracefulFallback(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Legacy key created before SetSecretKey has no encrypted secret.
	sNoEnc := NewStore(db)
	defer sNoEnc.Close()
	_, legacyKey, err := sNoEnc.Create(CreateOpts{Name: "legacy-key"})
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	if legacyKey.HasSecret {
		t.Fatal("expected legacyKey.HasSecret=false without secret key")
	}

	// Store configured with userKey encrypts new/imported/reset secrets.
	s := NewStore(db)
	defer s.Close()
	s.SetSecretKey([]byte("0123456789abcdef0123456789abcdef"))

	plain, created, err := s.Create(CreateOpts{Name: "encrypted-key"})
	if err != nil {
		t.Fatalf("Create encrypted: %v", err)
	}
	if !created.HasSecret {
		t.Fatal("expected created.HasSecret=true")
	}

	var rawCol string
	if err := db.QueryRow(`SELECT key_enc FROM api_keys WHERE id=?`, created.ID).Scan(&rawCol); err != nil {
		t.Fatalf("query key_enc: %v", err)
	}
	if rawCol == "" || strings.Contains(rawCol, plain) {
		t.Fatalf("expected encrypted ciphertext in key_enc, got %q", rawCol)
	}

	got, ok, err := s.GetSecret(created.ID)
	if err != nil || !ok || got != plain {
		t.Fatalf("GetSecret(created) = (%q, %v, %v), want (%q, true, nil)", got, ok, err, plain)
	}

	// Legacy key still returns ("", false, nil) and HasSecret=false.
	gotLeg, okLeg, err := s.GetSecret(legacyKey.ID)
	if err != nil || okLeg || gotLeg != "" {
		t.Fatalf("GetSecret(legacy) = (%q, %v, %v), want (\"\", false, nil)", gotLeg, okLeg, err)
	}
	fetchedLeg, err := s.Get(legacyKey.ID)
	if err != nil || fetchedLeg.HasSecret {
		t.Fatalf("Get(legacy) HasSecret=%v err=%v, want false", fetchedLeg.HasSecret, err)
	}

	// Imported key also stores encrypted secret.
	impPlain := "sk-ImportedSecretValue1234567890abcd"
	impKey, err := s.Import(impPlain, CreateOpts{Name: "imported-key"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !impKey.HasSecret {
		t.Fatal("expected impKey.HasSecret=true")
	}
	if gotImp, okImp, err := s.GetSecret(impKey.ID); err != nil || !okImp || gotImp != impPlain {
		t.Fatalf("GetSecret(imported) = (%q, %v, %v)", gotImp, okImp, err)
	}

	// Reset updates encrypted secret to the new plaintext.
	newPlain, resetKey, err := s.Reset(created.ID)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !resetKey.HasSecret || newPlain == plain {
		t.Fatalf("unexpected reset key state: hasSecret=%v samePlain=%v", resetKey.HasSecret, newPlain == plain)
	}
	if gotNew, okNew, err := s.GetSecret(created.ID); err != nil || !okNew || gotNew != newPlain {
		t.Fatalf("GetSecret(after reset) = (%q, %v, %v), want %q", gotNew, okNew, err, newPlain)
	}

	// Rotating user.key degrades gracefully to ("", false, nil).
	sOther := NewStore(db)
	defer sOther.Close()
	sOther.SetSecretKey([]byte("different-secret-key-0000000000000"))
	if gotOther, okOther, err := sOther.GetSecret(created.ID); err != nil || okOther || gotOther != "" {
		t.Fatalf("GetSecret(wrong key) = (%q, %v, %v), want (\"\", false, nil)", gotOther, okOther, err)
	}

	// Unknown ID returns ErrNotFound.
	if _, _, err := s.GetSecret("key_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing id, got %v", err)
	}
}
