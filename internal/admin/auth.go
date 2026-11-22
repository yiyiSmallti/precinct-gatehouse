// Package admin serves the Gatehouse management API and embedded Web UI.
package admin

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iter = 600000
	saltBytes  = 16
	hashBytes  = 32
	cookieName = "gh_admin"
	maxFails   = 5
)

// HashPassword derives a PBKDF2-SHA256 password hash in standard format.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password cannot be empty")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, hashBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword checks password against an encoded pbkdf2-sha256 string.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iter, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(dk, expected) == 1
}

// SetPassword hashes password and stores admin_user and admin_pass in settings.
func SetPassword(db *sql.DB, user, password string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return errors.New("username cannot be empty")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := setSetting(db, "admin_user", user); err != nil {
		return err
	}
	return setSetting(db, "admin_pass", hash)
}

// HasPassword reports whether an admin password has been configured in settings.
func HasPassword(db *sql.DB) bool {
	pass, err := getSetting(db, "admin_pass")
	return err == nil && pass != ""
}

func setSetting(db *sql.DB, k, v string) error {
	_, err := db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func getSetting(db *sql.DB, k string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	return v, err
}

type session struct {
	user    string
	expires time.Time
}

type authStore struct {
	ttl      time.Duration
	mu       sync.Mutex
	sessions map[string]session
	fails    map[string][]time.Time
}

func newAuthStore(ttl time.Duration) *authStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &authStore{
		ttl:      ttl,
		sessions: map[string]session{},
		fails:    map[string][]time.Time{},
	}
}

func (a *authStore) isLocked(ip string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	ts := pruneTimes(a.fails[ip], cutoff)
	if len(ts) == 0 {
		delete(a.fails, ip)
		return false
	}
	a.fails[ip] = ts
	return len(ts) >= maxFails
}

func (a *authStore) recordFail(ip string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	ts := append(pruneTimes(a.fails[ip], cutoff), now)
	a.fails[ip] = ts
}

func (a *authStore) clearFails(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

func (a *authStore) createSession(user string, now time.Time) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
	a.sessions[tok] = session{user: user, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return tok, nil
}

func (a *authStore) verifySession(tok string, now time.Time) (string, bool) {
	if tok == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tok]
	if !ok {
		return "", false
	}
	if now.After(s.expires) {
		delete(a.sessions, tok)
		return "", false
	}
	return s.user, true
}

func (a *authStore) deleteSession(tok string) {
	if tok == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

func pruneTimes(in []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(in) && !in[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return in
	}
	return append([]time.Time(nil), in[i:]...)
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
			return rip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
