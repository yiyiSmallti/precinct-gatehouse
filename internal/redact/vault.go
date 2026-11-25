package redact

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Vault holds placeholder→value mappings in memory, one bucket per
// user+session. A bucket is dropped after TTL without activity. The vault is
// never written to logs, audit records or metering.
//
// With a VaultStore attached (redaction.vault_persist) every new mapping is
// also written through to the store, and a lookup that misses in memory falls
// back to it, so placeholders survive a restart and resolve across the two
// processes of an FD handoff upgrade that share one database.
type Vault struct {
	ttl     time.Duration
	mu      sync.Mutex
	buckets map[string]*Bucket
	lastGC  time.Time

	store    VaultStore // nil: memory only
	log      *slog.Logger
	sweeping atomic.Bool
}

// VaultStore persists vault buckets. Implementations must never store the
// bucket key or a value in plaintext.
type VaultStore interface {
	// BucketID maps a bucket key (user+session) to its opaque stored id.
	BucketID(key string) string
	// Touch marks a bucket active at now. A bucket whose stored activity is
	// older than ttl is emptied first: it expired while no process held it.
	Touch(id string, now time.Time, ttl time.Duration) error
	Put(id, placeholder, value string) error
	Get(id, placeholder string) (string, bool, error)
	Count(id string) (int, error)
	// Sweep deletes buckets (and their entries) idle since before cutoff.
	Sweep(cutoff time.Time) error
}

// Bucket is one user+session mapping.
type Bucket struct {
	mu   sync.RWMutex
	m    map[string]string
	seen time.Time

	id     string // stored bucket id when the vault is persistent
	stored time.Time
	vault  *Vault
}

// NewVault creates a vault.
func NewVault(ttl time.Duration) *Vault {
	return &Vault{ttl: ttl, buckets: map[string]*Bucket{}}
}

// SetStore attaches a persistent store. Call before the vault is used.
func (v *Vault) SetStore(s VaultStore, log *slog.Logger) {
	v.mu.Lock()
	v.store, v.log = s, log
	v.mu.Unlock()
}

// Persistent reports whether mappings are written through to a store.
func (v *Vault) Persistent() bool { return v.store != nil }

// SetTTL updates the default bucket TTL and immediately sweeps buckets idle beyond ttl.
func (v *Vault) SetTTL(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := time.Now()
	v.mu.Lock()
	v.ttl = ttl
	v.sweepLocked(now)
	v.mu.Unlock()
}

// sweepLocked drops idle buckets from memory and, in the background, from
// the store. Expiry on disk is decided by the stored activity time, which the
// other process of an FD handoff also updates.
func (v *Vault) sweepLocked(now time.Time) {
	for k, b := range v.buckets {
		b.mu.RLock()
		stale := now.Sub(b.seen) > v.ttl
		b.mu.RUnlock()
		if stale {
			delete(v.buckets, k)
		}
	}
	v.lastGC = now
	if v.store != nil && v.sweeping.CompareAndSwap(false, true) {
		cutoff := now.Add(-v.ttl)
		go func() {
			defer v.sweeping.Store(false)
			if err := v.store.Sweep(cutoff); err != nil {
				v.warn("vault sweep failed", err)
			}
		}()
	}
}

// Bucket returns (creating) the bucket for key and marks it active using the vault's default TTL.
func (v *Vault) Bucket(key string) *Bucket {
	return v.BucketWithTTL(key, v.ttl)
}

// BucketWithTTL returns (creating) the bucket for key, enforcing ttl for expiration, and marks it active.
func (v *Vault) BucketWithTTL(key string, ttl time.Duration) *Bucket {
	now := time.Now()
	v.mu.Lock()
	if ttl <= 0 {
		ttl = v.ttl
	}
	if now.Sub(v.lastGC) > time.Minute {
		v.sweepLocked(now)
	}
	b := v.buckets[key]
	if b != nil {
		b.mu.RLock()
		stale := now.Sub(b.seen) > ttl
		b.mu.RUnlock()
		if stale {
			delete(v.buckets, key)
			b = nil
		}
	}
	if b == nil {
		b = &Bucket{m: map[string]string{}, vault: v}
		if v.store != nil {
			b.id = v.store.BucketID(key)
		}
		v.buckets[key] = b
	}
	// b.mu is taken before v.mu is released so a request that finds this
	// bucket waits for the stored activity update below; otherwise its
	// write could land before Touch empties an expired stored bucket.
	b.mu.Lock()
	v.mu.Unlock()
	defer b.mu.Unlock()
	b.seen = now
	if v.store != nil && now.Sub(b.stored) >= touchEvery(ttl) {
		b.stored = now
		if err := v.store.Touch(b.id, now, ttl); err != nil {
			v.warn("vault touch failed", err)
		}
	}
	return b
}

// touchEvery throttles stored activity updates; it stays well below ttl so a
// bucket in use is never swept as idle by another process.
func touchEvery(ttl time.Duration) time.Duration {
	return min(time.Minute, ttl/4)
}

// warn logs a store failure. Store errors never carry values (they are bound
// as SQL parameters) and never fail a request: the mapping is still in memory.
func (v *Vault) warn(msg string, err error) {
	if v.log != nil {
		v.log.Warn(msg, "err", err)
	}
}

// Put stores a mapping, writing it through to the store when it is new.
func (b *Bucket) Put(placeholder, value string) {
	b.mu.Lock()
	old, ok := b.m[placeholder]
	b.m[placeholder] = value
	b.mu.Unlock()
	if s := b.store(); s != nil && (!ok || old != value) {
		if err := s.Put(b.id, placeholder, value); err != nil {
			b.vault.warn("vault write failed", err)
		}
	}
}

// Get looks up a canonical placeholder, falling back to the store on a
// memory miss (a mapping written before a restart or by the other process).
func (b *Bucket) Get(placeholder string) (string, bool) {
	b.mu.RLock()
	v, ok := b.m[placeholder]
	b.mu.RUnlock()
	s := b.store()
	if ok || s == nil {
		return v, ok
	}
	v, ok, err := s.Get(b.id, placeholder)
	if err != nil {
		b.vault.warn("vault read failed", err)
		return "", false
	}
	if ok {
		b.mu.Lock()
		b.m[placeholder] = v
		b.mu.Unlock()
	}
	return v, ok
}

// Len returns the number of mappings (the stored count when memory holds none).
func (b *Bucket) Len() int {
	b.mu.RLock()
	n := len(b.m)
	b.mu.RUnlock()
	s := b.store()
	if n > 0 || s == nil {
		return n
	}
	n, err := s.Count(b.id)
	if err != nil {
		b.vault.warn("vault read failed", err)
		return 0
	}
	return n
}

func (b *Bucket) store() VaultStore {
	if b.vault == nil {
		return nil
	}
	return b.vault.store
}
