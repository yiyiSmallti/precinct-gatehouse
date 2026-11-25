package redact

import (
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"sync/atomic"
)

// DefaultDetectCacheEntries bounds the detector result cache.
const DefaultDetectCacheEntries = 65536

// DetectCache remembers what each detector plugin found in a text, so the
// history of a multi-turn conversation, sent again with every turn, is not
// scanned again: a model-based detector takes seconds per long message.
//
// Entries are keyed by a salted hash of (plugin name, text) and hold only
// kinds, offsets and scores, never text. Raw plugin results are stored:
// min_confidence, never_mask and actions apply after a lookup, so a reload of
// those settings needs no invalidation. Failed calls are not cached. The cache
// is per process and shared by the configuration snapshots built on reload.
type DetectCache struct {
	salt [16]byte
	max  int

	mu sync.Mutex
	ll *list.List // front = most recently used
	m  map[[32]byte]*list.Element

	hits, misses atomic.Uint64
}

type cacheEntry struct {
	key [32]byte
	fs  []Finding
}

// NewDetectCache creates a cache holding up to max texts' results.
func NewDetectCache(max int) *DetectCache {
	c := &DetectCache{max: max, ll: list.New(), m: map[[32]byte]*list.Element{}}
	_, _ = rand.Read(c.salt[:])
	return c
}

func (c *DetectCache) key(source, text string) [32]byte {
	h := sha256.New()
	h.Write(c.salt[:])
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(text))
	var k [32]byte
	h.Sum(k[:0])
	return k
}

func (c *DetectCache) get(k [32]byte) ([]Finding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).fs, true
}

func (c *DetectCache) put(k [32]byte, fs []Finding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		el.Value.(*cacheEntry).fs = fs
		c.ll.MoveToFront(el)
		return
	}
	c.m[k] = c.ll.PushFront(&cacheEntry{key: k, fs: fs})
	for c.ll.Len() > c.max {
		el := c.ll.Back()
		c.ll.Remove(el)
		delete(c.m, el.Value.(*cacheEntry).key)
	}
}

// Stats reports cache hits, misses and the number of cached texts.
func (c *DetectCache) Stats() (hits, misses uint64, entries int) {
	c.mu.Lock()
	entries = c.ll.Len()
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), entries
}
