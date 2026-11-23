// Package keys manages persistent API keys (sk-...) stored in SQLite with
// per-key model/group scopes, request-count rate limits and token quotas.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	secretLen      = 48
	cacheTTL       = 10 * time.Second
	touchInterval  = 30
)

// Errors returned by Store methods.
var (
	ErrNotFound = errors.New("invalid api key")
	ErrDisabled = errors.New("api key is disabled")
	ErrExpired  = errors.New("api key has expired")
)

// Limits bounds how many requests and tokens a key may use. Zero means unlimited.
//
// Token quotas count prompt + completion tokens of finished requests. They are
// checked at admission, so the request that crosses a quota still completes;
// the next one is refused.
type Limits struct {
	RPM           int   `json:"rpm"`
	RPD           int   `json:"rpd"`
	Concurrency   int   `json:"concurrency"`
	TokensDaily   int64 `json:"tokens_daily"`
	TokensMonthly int64 `json:"tokens_monthly"`
	TokensTotal   int64 `json:"tokens_total"`
}

func (l Limits) validate() error {
	if l.RPM < 0 || l.RPD < 0 || l.Concurrency < 0 {
		return fmt.Errorf("limits must be >= 0")
	}
	if l.TokensDaily < 0 || l.TokensMonthly < 0 || l.TokensTotal < 0 {
		return fmt.Errorf("token quotas must be >= 0")
	}
	return nil
}

// Key is one API key record (plaintext secret is never stored).
type Key struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt int64    `json:"expires_at"`
	Enabled   bool     `json:"enabled"`
	Models    []string `json:"models"`
	Groups    []string `json:"groups"`
	Limits
	Note       string `json:"note"`
	LastUsedAt int64  `json:"last_used_at"`
}

// CreateOpts configures a new API key.
type CreateOpts struct {
	Name        string
	ExpiresAt   int64
	Models      []string
	Groups      []string
	RPM         int
	RPD         int
	Concurrency int
	Note        string

	TokensDaily   int64
	TokensMonthly int64
	TokensTotal   int64
}

// Patch updates mutable fields on an existing API key.
type Patch struct {
	Name        *string
	ExpiresAt   *int64
	Enabled     *bool
	Models      *[]string
	Groups      *[]string
	RPM         *int
	RPD         *int
	Concurrency *int
	Note        *string

	TokensDaily   *int64
	TokensMonthly *int64
	TokensTotal   *int64
}

type cacheEntry struct {
	key     Key
	expires time.Time
}

// Store persists API keys in SQLite and caches active lookups briefly in memory.
type Store struct {
	db                  *sql.DB
	now                 func() time.Time
	cacheMu             sync.RWMutex
	cache               map[string]cacheEntry
	dataVer             int64
	localGen            uint64
	testAfterLookupScan func()

	touchMu   sync.Mutex
	lastTouch map[string]int64
	touchCh   chan touchEvent
	done      chan struct{}
	closeOnce sync.Once
}

type touchEvent struct {
	id string
	ts int64
}

// NewStore wraps an opened database and starts the async last-used updater.
func NewStore(db *sql.DB) *Store {
	s := &Store{
		db:        db,
		now:       time.Now,
		cache:     map[string]cacheEntry{},
		lastTouch: map[string]int64{},
		touchCh:   make(chan touchEvent, 256),
		done:      make(chan struct{}),
	}
	go s.touchLoop()
	return s
}

// Close flushes pending last_used_at updates and stops the background worker.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.touchCh)
		<-s.done
	})
}

func (s *Store) touchLoop() {
	defer close(s.done)
	for ev := range s.touchCh {
		_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=? AND deleted_at IS NULL`, ev.ts, ev.id)
	}
}

func (s *Store) clearCache() {
	s.cacheMu.Lock()
	clear(s.cache)
	s.localGen++
	s.cacheMu.Unlock()
}

// Create generates and stores a new API key, returning the plaintext once.
func (s *Store) Create(opts CreateOpts) (string, Key, error) {
	if err := validateCreateOpts(opts); err != nil {
		return "", Key{}, err
	}
	plain, err := newSecret()
	if err != nil {
		return "", Key{}, err
	}
	k, err := s.insertKey(plain, opts)
	if err != nil {
		return "", Key{}, err
	}
	return plain, k, nil
}

// Import stores an operator-supplied plaintext sk- key without generating a new secret.
func (s *Store) Import(plain string, opts CreateOpts) (Key, error) {
	if !strings.HasPrefix(plain, "sk-") || len(plain) < 20 {
		return Key{}, fmt.Errorf("imported key must start with sk- and be at least 20 characters")
	}
	if err := validateCreateOpts(opts); err != nil {
		return Key{}, err
	}
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM api_keys WHERE key_hash=?`, hashSecret(plain)).Scan(&exists)
	if err == nil {
		return Key{}, fmt.Errorf("key already exists")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Key{}, err
	}
	return s.insertKey(plain, opts)
}

func validateCreateOpts(opts CreateOpts) error {
	if strings.TrimSpace(opts.Name) == "" {
		return fmt.Errorf("name is required")
	}
	return Limits{RPM: opts.RPM, RPD: opts.RPD, Concurrency: opts.Concurrency,
		TokensDaily: opts.TokensDaily, TokensMonthly: opts.TokensMonthly, TokensTotal: opts.TokensTotal}.validate()
}

func (s *Store) insertKey(plain string, opts CreateOpts) (Key, error) {
	id, err := newKeyID()
	if err != nil {
		return Key{}, err
	}
	now := s.now().Unix()
	models := nonNilStrings(opts.Models)
	groups := nonNilStrings(opts.Groups)
	mj, _ := json.Marshal(models)
	gj, _ := json.Marshal(groups)
	k := Key{
		ID:        id,
		Name:      strings.TrimSpace(opts.Name),
		KeyPrefix: plain[:9],
		CreatedAt: now,
		ExpiresAt: opts.ExpiresAt,
		Enabled:   true,
		Models:    models,
		Groups:    groups,
		Limits: Limits{
			RPM:           opts.RPM,
			RPD:           opts.RPD,
			Concurrency:   opts.Concurrency,
			TokensDaily:   opts.TokensDaily,
			TokensMonthly: opts.TokensMonthly,
			TokensTotal:   opts.TokensTotal,
		},
		Note: opts.Note,
	}
	_, err = s.db.Exec(`INSERT INTO api_keys(id,name,key_hash,key_prefix,created_at,expires_at,enabled,models,groups,rpm,rpd,concurrency,note,last_used_at,tokens_daily,tokens_monthly,tokens_total)
		VALUES(?,?,?,?,?,?,1,?,?,?,?,?,?,0,?,?,?)`,
		k.ID, k.Name, hashSecret(plain), k.KeyPrefix, k.CreatedAt, k.ExpiresAt, string(mj), string(gj),
		k.RPM, k.RPD, k.Concurrency, k.Note, k.TokensDaily, k.TokensMonthly, k.TokensTotal)
	if err != nil {
		return Key{}, err
	}
	s.clearCache()
	return k, nil
}

// Lookup resolves a plaintext key, validating existence, enabled state and expiry.
func (s *Store) Lookup(plain string) (Key, error) {
	if !strings.HasPrefix(plain, "sk-") {
		return Key{}, ErrNotFound
	}
	h := hashSecret(plain)
	now := s.now()

	var dv int64
	dvOK := false
	if err := s.db.QueryRow(`PRAGMA data_version`).Scan(&dv); err == nil {
		dvOK = true
		s.cacheMu.Lock()
		if dv != s.dataVer {
			clear(s.cache)
			s.dataVer = dv
			s.localGen++
		}
		s.cacheMu.Unlock()
	}

	s.cacheMu.RLock()
	ce, ok := s.cache[h]
	gen := s.localGen
	s.cacheMu.RUnlock()

	var k Key
	if ok && now.Before(ce.expires) {
		k = ce.key
	} else {
		row := s.db.QueryRow(`SELECT id,name,key_prefix,created_at,expires_at,enabled,models,groups,rpm,rpd,concurrency,note,last_used_at,tokens_daily,tokens_monthly,tokens_total
			FROM api_keys WHERE key_hash=? AND deleted_at IS NULL`, h)
		var err error
		k, err = scanKey(row)
		if errors.Is(err, sql.ErrNoRows) {
			return Key{}, ErrNotFound
		}
		if err != nil {
			return Key{}, err
		}
		if s.testAfterLookupScan != nil {
			s.testAfterLookupScan()
		}
		s.cacheMu.Lock()
		if dvOK && dv == s.dataVer && gen == s.localGen {
			s.cache[h] = cacheEntry{key: k, expires: now.Add(cacheTTL)}
		}
		s.cacheMu.Unlock()
	}

	if !k.Enabled {
		return k, ErrDisabled
	}
	if k.ExpiresAt > 0 && now.Unix() >= k.ExpiresAt {
		return k, ErrExpired
	}
	return k, nil
}

// List returns all non-deleted API keys ordered newest first.
func (s *Store) List() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id,name,key_prefix,created_at,expires_at,enabled,models,groups,rpm,rpd,concurrency,note,last_used_at,tokens_daily,tokens_monthly,tokens_total
		FROM api_keys WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Get returns one non-deleted API key by id.
func (s *Store) Get(id string) (Key, error) {
	row := s.db.QueryRow(`SELECT id,name,key_prefix,created_at,expires_at,enabled,models,groups,rpm,rpd,concurrency,note,last_used_at,tokens_daily,tokens_monthly,tokens_total
		FROM api_keys WHERE id=? AND deleted_at IS NULL`, id)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

// Update modifies fields on an existing key and invalidates the lookup cache.
func (s *Store) Update(id string, p Patch) (Key, error) {
	cur, err := s.Get(id)
	if err != nil {
		return Key{}, err
	}
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" {
			return Key{}, fmt.Errorf("name cannot be empty")
		}
		cur.Name = n
	}
	if p.ExpiresAt != nil {
		cur.ExpiresAt = *p.ExpiresAt
	}
	if p.Enabled != nil {
		cur.Enabled = *p.Enabled
	}
	if p.Models != nil {
		cur.Models = nonNilStrings(*p.Models)
	}
	if p.Groups != nil {
		cur.Groups = nonNilStrings(*p.Groups)
	}
	if p.RPM != nil {
		if *p.RPM < 0 {
			return Key{}, fmt.Errorf("rpm must be >= 0")
		}
		cur.RPM = *p.RPM
	}
	if p.RPD != nil {
		if *p.RPD < 0 {
			return Key{}, fmt.Errorf("rpd must be >= 0")
		}
		cur.RPD = *p.RPD
	}
	if p.Concurrency != nil {
		if *p.Concurrency < 0 {
			return Key{}, fmt.Errorf("concurrency must be >= 0")
		}
		cur.Concurrency = *p.Concurrency
	}
	if p.TokensDaily != nil {
		cur.TokensDaily = *p.TokensDaily
	}
	if p.TokensMonthly != nil {
		cur.TokensMonthly = *p.TokensMonthly
	}
	if p.TokensTotal != nil {
		cur.TokensTotal = *p.TokensTotal
	}
	if err := cur.Limits.validate(); err != nil {
		return Key{}, err
	}
	if p.Note != nil {
		cur.Note = *p.Note
	}
	mj, _ := json.Marshal(nonNilStrings(cur.Models))
	gj, _ := json.Marshal(nonNilStrings(cur.Groups))
	en := 0
	if cur.Enabled {
		en = 1
	}
	res, err := s.db.Exec(`UPDATE api_keys SET name=?,expires_at=?,enabled=?,models=?,groups=?,rpm=?,rpd=?,concurrency=?,note=?,tokens_daily=?,tokens_monthly=?,tokens_total=? WHERE id=? AND deleted_at IS NULL`,
		cur.Name, cur.ExpiresAt, en, string(mj), string(gj), cur.RPM, cur.RPD, cur.Concurrency, cur.Note,
		cur.TokensDaily, cur.TokensMonthly, cur.TokensTotal, id)
	if err != nil {
		return Key{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Key{}, ErrNotFound
	}
	s.clearCache()
	return s.Get(id)
}

// SetEnabled enables or disables an API key.
func (s *Store) SetEnabled(id string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.Exec(`UPDATE api_keys SET enabled=? WHERE id=? AND deleted_at IS NULL`, en, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.clearCache()
	return nil
}

// Reset generates a fresh secret for an existing key, invalidating the old one.
func (s *Store) Reset(id string) (string, Key, error) {
	plain, err := newSecret()
	if err != nil {
		return "", Key{}, err
	}
	prefix := plain[:9]
	res, err := s.db.Exec(`UPDATE api_keys SET key_hash=?, key_prefix=? WHERE id=? AND deleted_at IS NULL`,
		hashSecret(plain), prefix, id)
	if err != nil {
		return "", Key{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", Key{}, ErrNotFound
	}
	s.clearCache()
	k, err := s.Get(id)
	return plain, k, err
}

// Delete soft-deletes an API key.
func (s *Store) Delete(id string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET deleted_at=? WHERE id=? AND deleted_at IS NULL`, s.now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.clearCache()
	return nil
}

// Touch schedules an async last_used_at update throttled to once per 30s per key.
func (s *Store) Touch(id string) {
	now := s.now().Unix()
	s.touchMu.Lock()
	if now-s.lastTouch[id] < touchInterval {
		s.touchMu.Unlock()
		return
	}
	s.lastTouch[id] = now
	s.touchMu.Unlock()

	select {
	case s.touchCh <- touchEvent{id: id, ts: now}:
	default:
	}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanKey(sc scanner) (Key, error) {
	var k Key
	var en int
	var mj, gj string
	if err := sc.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.ExpiresAt, &en, &mj, &gj,
		&k.RPM, &k.RPD, &k.Concurrency, &k.Note, &k.LastUsedAt,
		&k.TokensDaily, &k.TokensMonthly, &k.TokensTotal); err != nil {
		return Key{}, err
	}
	k.Enabled = en != 0
	_ = json.Unmarshal([]byte(mj), &k.Models)
	_ = json.Unmarshal([]byte(gj), &k.Groups)
	k.Models = nonNilStrings(k.Models)
	k.Groups = nonNilStrings(k.Groups)
	return k, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func newKeyID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "key_" + hex.EncodeToString(b), nil
}

func newSecret() (string, error) {
	const limit = 248 // 62 * 4, avoids modulo bias
	out := make([]byte, secretLen)
	buf := make([]byte, 64)
	i := 0
	for i < secretLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out[i] = base62Alphabet[b%62]
			i++
			if i == secretLen {
				break
			}
		}
	}
	return "sk-" + string(out), nil
}

func hashSecret(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
