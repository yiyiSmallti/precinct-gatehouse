package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
)

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	if err := SetPassword(db, "admin", "old-password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	srv := New(Deps{DB: db})

	loginWith := func(pwd string) string {
		t.Helper()
		body := []byte(`{"username":"admin","password":"` + pwd + `"}`)
		req, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login failed status=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == cookieName {
				return c.Value
			}
		}
		t.Fatal("missing session cookie from login")
		return ""
	}

	checkMe := func(tok string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	tokA := loginWith("old-password-123")
	tokB := loginWith("old-password-123")
	if checkMe(tokA) != http.StatusOK || checkMe(tokB) != http.StatusOK {
		t.Fatalf("expected both initial sessions authorized")
	}

	// Session B rotates the password: other sessions (tokA) and old tokB must be invalidated,
	// while a rotated cookie is issued for Session B.
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/password", bytes.NewReader([]byte(`{"old":"old-password-123","new":"new-password-456"}`)))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tokB})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gatehouse-Admin", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("changePassword status=%d body=%s", rec.Code, rec.Body.String())
	}
	var tokB2 string
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			tokB2 = c.Value
		}
	}
	if tokB2 == "" || tokB2 == tokB {
		t.Fatalf("expected fresh replacement session cookie, got %q (old %q)", tokB2, tokB)
	}

	if st := checkMe(tokA); st != http.StatusUnauthorized {
		t.Fatalf("expected session A revoked after password change, got %d", st)
	}
	if st := checkMe(tokB); st != http.StatusUnauthorized {
		t.Fatalf("expected old session B token revoked after password change, got %d", st)
	}
	if st := checkMe(tokB2); st != http.StatusOK {
		t.Fatalf("expected replacement session B2 authorized, got %d", st)
	}

	// Out-of-band password reset (e.g. CLI set-password) must also invalidate active sessions immediately.
	if err := SetPassword(db, "admin", "cli-reset-pass-789"); err != nil {
		t.Fatalf("SetPassword CLI reset: %v", err)
	}
	if st := checkMe(tokB2); st != http.StatusUnauthorized {
		t.Fatalf("expected session B2 revoked after out-of-band password change, got %d", st)
	}
}
