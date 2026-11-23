package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// setupState holds the one-time code that lets the first visitor create the
// admin account from the browser. It exists only while no admin password is
// stored; the code is printed in the server log, so only someone who can read
// that log (the operator) can claim the console.
type setupState struct {
	mu   sync.Mutex
	code string
}

const setupAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I

func newSetupCode() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = setupAlphabet[int(c)%len(setupAlphabet)]
	}
	return string(out)
}

// SetupCode returns the one-time setup code when the admin account has not
// been created yet, or "" when a password is already set.
func (s *Server) SetupCode() string {
	if HasPassword(s.deps.DB) {
		return ""
	}
	s.setup.mu.Lock()
	defer s.setup.mu.Unlock()
	if s.setup.code == "" {
		s.setup.code = newSetupCode()
	}
	return s.setup.code
}

// setupStatus serves GET /admin/api/setup (no session required).
func (s *Server) setupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": !HasPassword(s.deps.DB)})
}

// completeSetup serves POST /admin/api/setup: {code, username, password}.
// It creates the admin account once and signs the caller in.
func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Gatehouse-Admin") != "1" {
		writeJSONError(w, http.StatusForbidden, "missing X-Gatehouse-Admin header")
		return
	}
	ip := clientIP(r)
	now := time.Now()
	if s.auth.isLocked(ip, now) {
		writeJSONError(w, http.StatusTooManyRequests, "too many failed attempts")
		return
	}
	var req struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	user := strings.TrimSpace(req.Username)
	if user == "" {
		user = "admin"
	}
	if len([]rune(req.Password)) < 8 {
		writeJSONError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	s.setup.mu.Lock()
	defer s.setup.mu.Unlock()
	if HasPassword(s.deps.DB) {
		writeJSONError(w, http.StatusConflict, "admin account already exists")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if s.setup.code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.setup.code)) != 1 {
		s.auth.recordFail(ip, now)
		writeJSONError(w, http.StatusUnauthorized, "invalid setup code")
		return
	}
	if err := SetPassword(s.deps.DB, user, req.Password); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setup.code = ""
	storedPass, err := getSetting(s.deps.DB, "admin_pass")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ttl := s.sessionTTL()
	tok, err := s.auth.createSession(user, storedPass, ttl, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSessionCookie(w, r, tok, ttl)
	writeJSON(w, http.StatusOK, map[string]any{"username": user})
}
