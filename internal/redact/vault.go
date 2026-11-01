package redact

import (
	"sync"
	"time"
)

// Vault holds placeholder→value mappings in memory, one bucket per
// user+session. A bucket is dropped after TTL without activity. The vault is
// never written to logs, audit records or metering.
type Vault struct {
	ttl     time.Duration
	mu      sync.Mutex
	buckets map[string]*Bucket
	lastGC  time.Time
}

// Bucket is one user+session mapping.
type Bucket struct {
	mu   sync.RWMutex
	m    map[string]string
	seen time.Time
}

// NewVault creates a vault.
func NewVault(ttl time.Duration) *Vault {
	return &Vault{ttl: ttl, buckets: map[string]*Bucket{}}
}

// Bucket returns (creating) the bucket for key and marks it active.
func (v *Vault) Bucket(key string) *Bucket {
	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if now.Sub(v.lastGC) > time.Minute {
		for k, b := range v.buckets {
			b.mu.RLock()
			stale := now.Sub(b.seen) > v.ttl
			b.mu.RUnlock()
			if stale {
				delete(v.buckets, k)
			}
		}
		v.lastGC = now
	}
	b := v.buckets[key]
	if b == nil {
		b = &Bucket{m: map[string]string{}}
		v.buckets[key] = b
	}
	b.mu.Lock()
	b.seen = now
	b.mu.Unlock()
	return b
}

// Put stores a mapping.
func (b *Bucket) Put(placeholder, value string) {
	b.mu.Lock()
	b.m[placeholder] = value
	b.mu.Unlock()
}

// Get looks up a canonical placeholder.
func (b *Bucket) Get(placeholder string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.m[placeholder]
	return v, ok
}

// Len returns the number of mappings.
func (b *Bucket) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.m)
}
