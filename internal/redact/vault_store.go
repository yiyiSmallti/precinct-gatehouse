package redact

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SQLVaultStore keeps vault buckets in the vault_buckets / vault_entries
// tables of gatehouse.db (see internal/store). Bucket keys are stored as an
// HMAC and values as AES-256-GCM ciphertext bound to their bucket and
// placeholder; both keys are derived from the local user.key, so the
// database alone reveals neither who a bucket belongs to nor any value.
//
// SQLite in WAL mode with a busy timeout (store.Open) lets the two processes
// of an FD handoff upgrade use the same file concurrently.
type SQLVaultStore struct {
	db    *sql.DB
	idKey []byte
	aead  cipher.AEAD
}

// NewSQLVaultStore derives the store keys from secret (the local user.key).
func NewSQLVaultStore(db *sql.DB, secret []byte) (*SQLVaultStore, error) {
	if len(secret) < 32 {
		return nil, errors.New("vault store: local key too short")
	}
	block, err := aes.NewCipher(derive(secret, "gatehouse vault encryption v1"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SQLVaultStore{db: db, idKey: derive(secret, "gatehouse vault bucket id v1"), aead: aead}, nil
}

func derive(secret []byte, label string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// BucketID implements VaultStore.
func (s *SQLVaultStore) BucketID(key string) string {
	m := hmac.New(sha256.New, s.idKey)
	m.Write([]byte(key))
	return hex.EncodeToString(m.Sum(nil))
}

// Touch implements VaultStore. Each statement runs on its own (autocommit):
// a read-then-write transaction would fail with SQLITE_BUSY_SNAPSHOT when the
// other process of an FD handoff commits in between, and each statement here
// is correct on its own whatever the other process does around it.
func (s *SQLVaultStore) Touch(id string, now time.Time, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM vault_entries WHERE bucket=?
		AND EXISTS (SELECT 1 FROM vault_buckets WHERE bucket=? AND seen_at < ?)`, id, id, now.Add(-ttl).UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO vault_buckets(bucket, seen_at) VALUES(?, ?)
		ON CONFLICT(bucket) DO UPDATE SET seen_at=max(seen_at, excluded.seen_at)`, id, now.UnixMilli())
	return err
}

// Put implements VaultStore.
func (s *SQLVaultStore) Put(id, placeholder, value string) error {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(value)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), aad(id, placeholder))
	_, err := s.db.Exec(`INSERT INTO vault_entries(bucket, placeholder, value) VALUES(?, ?, ?)
		ON CONFLICT(bucket, placeholder) DO UPDATE SET value=excluded.value`, id, placeholder, sealed)
	return err
}

// Get implements VaultStore.
func (s *SQLVaultStore) Get(id, placeholder string) (string, bool, error) {
	var sealed []byte
	err := s.db.QueryRow(`SELECT value FROM vault_entries WHERE bucket=? AND placeholder=?`, id, placeholder).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	ns := s.aead.NonceSize()
	if len(sealed) < ns {
		return "", false, errors.New("vault entry is malformed")
	}
	plain, err := s.aead.Open(nil, sealed[:ns], sealed[ns:], aad(id, placeholder))
	if err != nil {
		// a different user.key or a tampered row; the cipher error says nothing about the value
		return "", false, fmt.Errorf("vault entry cannot be decrypted: %w", err)
	}
	return string(plain), true, nil
}

// Count implements VaultStore.
func (s *SQLVaultStore) Count(id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM vault_entries WHERE bucket=?`, id).Scan(&n)
	return n, err
}

// Sweep implements VaultStore. A bucket touched between the two statements
// keeps its row; its entries were idle beyond the TTL and Touch would have
// dropped them as well.
func (s *SQLVaultStore) Sweep(cutoff time.Time) error {
	c := cutoff.UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM vault_entries WHERE bucket IN (SELECT bucket FROM vault_buckets WHERE seen_at < ?)`, c); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM vault_buckets WHERE seen_at < ?`, c)
	return err
}

func aad(id, placeholder string) []byte {
	return []byte(id + "\x00" + placeholder)
}
