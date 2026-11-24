package admin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

type sessionDB struct {
	db *sql.DB
}

func newSessionDB(db *sql.DB) *sessionDB {
	if db == nil {
		return nil
	}
	return &sessionDB{db: db}
}

func hashSessionToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func touchThrottle(idleTTL time.Duration) time.Duration {
	if idleTTL <= 0 {
		return 0
	}
	t := idleTTL / 4
	if t > 15*time.Second {
		return 15 * time.Second
	}
	return t
}

func (s *sessionDB) save(tok string, rec session, now time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE expires_at <= ?`, now.UnixNano())
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO admin_sessions(token_hash, user, pass_hash, issued_at, last_active_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hashSessionToken(tok),
		rec.user,
		rec.passHash,
		rec.issued.UnixNano(),
		rec.lastActive.UnixNano(),
		rec.expires.UnixNano(),
	)
	return err
}

func (s *sessionDB) load(tok string) (session, bool) {
	if s == nil || s.db == nil {
		return session{}, false
	}
	var user, passHash string
	var issuedNS, lastActiveNS, expiresNS int64
	err := s.db.QueryRow(
		`SELECT user, pass_hash, issued_at, last_active_at, expires_at FROM admin_sessions WHERE token_hash = ?`,
		hashSessionToken(tok),
	).Scan(&user, &passHash, &issuedNS, &lastActiveNS, &expiresNS)
	if err != nil {
		return session{}, false
	}
	return session{
		user:       user,
		passHash:   passHash,
		issued:     time.Unix(0, issuedNS),
		lastActive: time.Unix(0, lastActiveNS),
		expires:    time.Unix(0, expiresNS),
	}, true
}

func (s *sessionDB) touch(tok string, at time.Time) {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.Exec(
		`UPDATE admin_sessions SET last_active_at = ? WHERE token_hash = ?`,
		at.UnixNano(),
		hashSessionToken(tok),
	)
}

func (s *sessionDB) delete(tok string) {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE token_hash = ?`, hashSessionToken(tok))
}

func (s *sessionDB) clear() {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM admin_sessions`)
}
