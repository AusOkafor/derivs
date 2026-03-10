package cache

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

type entry struct {
	data      models.SnapshotWithAnalysis
	expiresAt time.Time
}

type Cache struct {
	mu    sync.RWMutex
	store map[string]entry
	ttl   time.Duration
}

func New(ttlSeconds int) *Cache {
	c := &Cache{
		store: make(map[string]entry),
		ttl:   time.Duration(ttlSeconds) * time.Second,
	}
	go c.cleanupLoop()
	return c
}

func cacheKey(symbol string) string {
	return fmt.Sprintf("snapshot:%s", strings.ToUpper(symbol))
}

func (c *Cache) Set(symbol string, data models.SnapshotWithAnalysis) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(symbol)
	c.store[key] = entry{
		data:      data,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func (c *Cache) Get(symbol string) (models.SnapshotWithAnalysis, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := cacheKey(symbol)
	e, ok := c.store[key]
	if !ok || time.Now().After(e.expiresAt) {
		return models.SnapshotWithAnalysis{}, false
	}
	return e.data, true
}

// cleanupLoop purges expired entries every 60 seconds.
// The goroutine exits naturally when the process terminates.
func (c *Cache) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.purgeExpired()
	}
}

func (c *Cache) purgeExpired() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.store {
		if now.After(e.expiresAt) {
			delete(c.store, k)
		}
	}
}
