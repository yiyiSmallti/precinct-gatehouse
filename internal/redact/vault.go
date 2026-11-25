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
	// lag is the largest touchEvery interval used since start: how far a
	// stored activity time may trail a bucket's real last use. Disk expiry
	// adds it to the ttl. It never shrinks, so after a hot reload to a shorter
	// vault_ttl, timestamps throttled at the old, longer interval are still
	// allowed for.
	lag time.Duration

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
	// reset reports that the stored bucket was missing or expired, i.e. it
	// may hold none of the mappings a live in-memory bucket has.
	Touch(id string, now time.Time, ttl time.Duration) (reset bool, err error)
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
	// unsaved holds placeholders whose last store write failed; the next Put
	// of them writes again instead of trusting the in-memory copy.
	unsaved map[string]bool
	// wmu serializes Put, so each memory update and its store write land in
	// the same order: placeholders are shared by normalized variants of a
	// value (email casing), and an older write must not reach the store after
	// a newer one. Readers only take mu.
	wmu sync.Mutex
}

// NewVault creates a vault.
func NewVault(ttl time.Duration) *Vault {
	return &Vault{ttl: ttl, lag: touchEvery(ttl), buckets: map[string]*Bucket{}}
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
	v.lag = max(v.lag, touchEvery(ttl))
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
		cutoff := now.Add(-(v.ttl + v.lag))
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
	v.lag = max(v.lag, touchEvery(ttl))
	diskTTL := ttl + v.lag
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
		reset, err := v.store.Touch(b.id, now, diskTTL)
		if err != nil {
			// b.stored stays old, so the next request retries at once
			v.warn("vault touch failed", err)
		} else {
			b.stored = now
		}
		if reset {
			b.repersistLocked()
		}
	}
	return b
}

// repersistLocked writes every in-memory mapping back to the store after
// Touch found the stored bucket missing or expired while this bucket was
// still live in memory: stored activity updates may have failed for longer
// than the disk TTL, or another process expired it with a shorter TTL.
// Without this, those mappings would stay memory-only, since later Puts of
// the same values skip the store. b.mu must be held for writing; Put updates
// memory under it before writing, so no older value can land after these.
func (b *Bucket) repersistLocked() {
	s := b.store()
	for ph, val := range b.m {
		if err := s.Put(b.id, ph, val); err != nil {
			if b.unsaved == nil {
				b.unsaved = map[string]bool{}
			}
			b.unsaved[ph] = true
			b.vault.warn("vault write failed", err)
			continue
		}
		delete(b.unsaved, ph)
	}
}

// touchEvery throttles stored activity updates; it stays well below ttl so a
// bucket in use is never swept as idle by another process.
//
// Stored mappings expire after ttl + lag (see Vault.lag), not plain ttl: the
// stored activity time trails the real last use by up to the throttle, so
// expiring at ttl could delete the mappings of a bucket still live in memory,
// which a restart or the other handoff process could then no longer restore.
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

// Put stores a mapping, writing it through to the store when it is new,
// changed, or an earlier write of it failed. A failed write never fails the
// request (the mapping is in memory); it is retried on the next Put.
func (b *Bucket) Put(placeholder, value string) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	b.mu.Lock()
	old, ok := b.m[placeholder]
	b.m[placeholder] = value
	retry := b.unsaved[placeholder]
	b.mu.Unlock()
	s := b.store()
	if s == nil || (ok && old == value && !retry) {
		return
	}
	err := s.Put(b.id, placeholder, value)
	b.mu.Lock()
	if err != nil {
		if b.unsaved == nil {
			b.unsaved = map[string]bool{}
		}
		b.unsaved[placeholder] = true
	} else if b.m[placeholder] == value {
		delete(b.unsaved, placeholder)
	}
	b.mu.Unlock()
	if err != nil {
		b.vault.warn("vault write failed", err)
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
		// a Put may have stored a newer value while the store was read
		b.mu.Lock()
		if cur, have := b.m[placeholder]; have {
			v = cur
		} else {
			b.m[placeholder] = v
		}
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
