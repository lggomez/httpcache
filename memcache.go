package httpcache

import "sync"

// MemoryCache is an implementation of Cache that stores responses in an in-memory map.
type MemoryCache struct {
	mu    sync.RWMutex
	items map[string][]byte
}

// Get returns the []byte representation of the response and true if present, false if not
func (mc *MemoryCache) Get(key string) (resp []byte, ok bool) {
	mc.mu.RLock()
	resp, ok = mc.items[key]
	mc.mu.RUnlock()
	return resp, ok
}

// Set saves response resp to the cache with key
func (mc *MemoryCache) Set(key string, resp []byte, ttl int) {
	mc.mu.Lock()
	mc.items[key] = resp
	mc.mu.Unlock()
}

// Delete removes key from the cache
func (mc *MemoryCache) Delete(key string) {
	mc.mu.Lock()
	delete(mc.items, key)
	mc.mu.Unlock()
}

// NewMemoryCache returns a new Cache that will store items in an in-memory map
func NewMemoryCache() *MemoryCache {
	c := &MemoryCache{items: map[string][]byte{}}
	return c
}
