// Package token issues and verifies Gatehouse gateway tokens: short-lived,
// scoped bearer tokens signed with a local HMAC-SHA256 key. The token itself
// carries its scope; the database only records issued ids for revocation and
// reporting.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Prefix marks a Gatehouse token, version 1.
const Prefix = "ght1."

// Data classes, lowest to highest.
var DataClasses = []string{"public", "internal", "confidential"}

// ClassRank returns the order of a data class, or -1 if unknown.
func ClassRank(c string) int {
	for i, x := range DataClasses {
		if x == c {
			return i
		}
	}
	return -1
}

// Scope limits what a token may do. Empty lists mean "no restriction".
type Scope struct {
	Groups    []string `json:"groups,omitempty"`     // routing groups the token may request
	Models    []string `json:"models,omitempty"`     // <provider>/<model> the token may request directly
	MaxTokens int64    `json:"max_tokens,omitempty"` // total token budget over the token's life (0 = unlimited)
	DataClass string   `json:"data_class,omitempty"` // highest data class the token may send (default internal)
}

// Claims is the signed token payload.
type Claims struct {
	ID        string `json:"jti"`
	Name      string `json:"name"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"` // unix seconds; 0 = never (only the local personal token)
	Scope     Scope  `json:"scope"`
}

// Errors returned by Verify.
var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad token signature")
	ErrExpired   = errors.New("token expired")
	ErrRevoked   = errors.New("token revoked")
	ErrUnknown   = errors.New("token not issued by this gateway")
)

// Issuer signs and verifies tokens.
type Issuer struct {
	key []byte
	db  *sql.DB
	now func() time.Time
}

// NewIssuer loads (or creates) <dataDir>/token.key.
func NewIssuer(dataDir string, db *sql.DB) (*Issuer, error) {
	key, err := loadOrCreateKey(filepath.Join(dataDir, "token.key"))
	if err != nil {
		return nil, err
	}
	return &Issuer{key: key, db: db, now: time.Now}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) < 32 {
			return nil, fmt.Errorf("%s: invalid key file", path)
		}
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// Issue signs a new token. ttl == 0 issues a non-expiring token.
func (is *Issuer) Issue(name string, ttl time.Duration, scope Scope) (string, Claims, error) {
	if scope.DataClass == "" {
		scope.DataClass = "internal"
	}
	if ClassRank(scope.DataClass) < 0 {
		return "", Claims{}, fmt.Errorf("unknown data class %q (public, internal, confidential)", scope.DataClass)
	}
	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return "", Claims{}, err
	}
	now := is.now()
	c := Claims{ID: "tok_" + hex.EncodeToString(idb), Name: name, IssuedAt: now.Unix(), Scope: scope}
	if ttl > 0 {
		c.ExpiresAt = now.Add(ttl).Unix()
	}
	sc, _ := json.Marshal(scope)
	if _, err := is.db.Exec(`INSERT INTO tokens(id,name,issued_at,expires_at,scope) VALUES(?,?,?,?,?)`,
		c.ID, c.Name, c.IssuedAt, c.ExpiresAt, string(sc)); err != nil {
		return "", Claims{}, err
	}
	payload, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(payload)
	return Prefix + p + "." + is.sign(p), c, nil
}

func (is *Issuer) sign(p string) string {
	m := hmac.New(sha256.New, is.key)
	m.Write([]byte(Prefix + p))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks signature, expiry and revocation.
func (is *Issuer) Verify(tok string) (Claims, error) {
	var c Claims
	rest, ok := strings.CutPrefix(tok, Prefix)
	if !ok {
		return c, ErrMalformed
	}
	p, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return c, ErrMalformed
	}
	if !hmac.Equal([]byte(sig), []byte(is.sign(p))) {
		return c, ErrSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, ErrMalformed
	}
	if c.ExpiresAt != 0 && is.now().Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	var revoked sql.NullInt64
	err = is.db.QueryRow(`SELECT revoked_at FROM tokens WHERE id=?`, c.ID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrUnknown
	}
	if err != nil {
		return c, err
	}
	if revoked.Valid {
		return c, ErrRevoked
	}
	return c, nil
}

// Revoke marks a token id (or name) revoked. Returns the number of tokens revoked.
func (is *Issuer) Revoke(idOrName string) (int64, error) {
	r, err := is.db.Exec(`UPDATE tokens SET revoked_at=? WHERE (id=? OR name=?) AND revoked_at IS NULL`, is.now().Unix(), idOrName, idOrName)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Info is one row of `gatehouse token list`.
type Info struct {
	ID, Name  string
	IssuedAt  time.Time
	ExpiresAt time.Time // zero = never
	Scope     string
	Revoked   bool
}

// List returns all issued tokens.
func (is *Issuer) List() ([]Info, error) {
	rows, err := is.db.Query(`SELECT id,name,issued_at,expires_at,scope,revoked_at IS NOT NULL FROM tokens ORDER BY issued_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Info
	for rows.Next() {
		var i Info
		var iat, exp int64
		if err := rows.Scan(&i.ID, &i.Name, &iat, &exp, &i.Scope, &i.Revoked); err != nil {
			return nil, err
		}
		i.IssuedAt = time.Unix(iat, 0)
		if exp != 0 {
			i.ExpiresAt = time.Unix(exp, 0)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// LocalToken returns the personal-mode local token stored in
// <dataDir>/local.token, issuing it on first use. It never expires and has no
// scope limits; revoke it and delete the file to rotate.
func (is *Issuer) LocalToken(dataDir string) (string, bool, error) {
	path := filepath.Join(dataDir, "local.token")
	if b, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(b))
		if _, err := is.Verify(t); err == nil {
			return t, false, nil
		}
	}
	t, _, err := is.Issue("local", 0, Scope{DataClass: "confidential"})
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return t, true, nil
}

// AllowsModel reports whether the scope lets a request name `model`
// (a routing group or <provider>/<model>).
func (s Scope) AllowsModel(model string, isGroup bool) bool {
	if len(s.Groups) == 0 && len(s.Models) == 0 {
		return true
	}
	list := s.Models
	if isGroup {
		list = s.Groups
	}
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}
