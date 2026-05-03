package payment

import (
	"sync"
	"time"
)

// IdempotencyCache stores successes and terminal failures keyed by billing idempotency keys.
type IdempotencyCache struct {
	mu    sync.RWMutex
	items map[string]idemRecord
}

type idemRecord struct {
	preimageHex string
	err         error
	expiresAt   time.Time
}

func NewIdempotencyCache() *IdempotencyCache {
	c := &IdempotencyCache{items: make(map[string]idemRecord)}
	go c.cleanupLoop()
	return c
}

const defaultIdemTTL = 24 * time.Hour

func (c *IdempotencyCache) cleanupLoop() {
	t := time.NewTicker(7 * time.Minute)
	defer t.Stop()
	for range t.C {
		c.gc()
	}
}

func (c *IdempotencyCache) gc() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.items {
		if now.After(v.expiresAt) {
			delete(c.items, k)
		}
	}
}

// Get returns a cache hit replay. replay is nil on miss.
func (c *IdempotencyCache) Get(key string) (replay *idemRecord, hit bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[key]
	if !ok || time.Now().After(v.expiresAt) {
		return nil, false
	}
	cp := v
	return &cp, true
}

// SetSuccess records a preimage for the idempotency key.
func (c *IdempotencyCache) SetSuccess(key, preimageHex string, ttl time.Duration) {
	c.set(key, preimageHex, nil, ttl)
}

// SetFailure records a terminal failure to replay briefly (same TTL as successes).
func (c *IdempotencyCache) SetFailure(key string, failure error, ttl time.Duration) {
	c.set(key, "", failure, ttl)
}

func (c *IdempotencyCache) set(key, preimageHex string, failure error, ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultIdemTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = idemRecord{
		preimageHex: preimageHex,
		err:         failure,
		expiresAt:   time.Now().Add(ttl),
	}
}
