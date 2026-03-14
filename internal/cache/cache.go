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

// PricePoint holds a single price observation for momentum calculation.
type PricePoint struct {
	Price     float64
	Timestamp time.Time
}

type Cache struct {
	mu            sync.RWMutex
	store         map[string]entry
	ttl           time.Duration
	lastFetchTime time.Time
	priceHistory  map[string][]PricePoint
	priceHistMu   sync.RWMutex
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
	c.lastFetchTime = time.Now()
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

// Size returns the number of cached symbols (non-expired entries only).
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range c.store {
		if now.Before(e.expiresAt) {
			n++
		}
	}
	return n
}

// LastFetchTime returns the most recent cache update.
func (c *Cache) LastFetchTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFetchTime
}

// RecordPrice stores the latest price for a symbol (keeps last 2 points for momentum).
func (c *Cache) RecordPrice(symbol string, price float64) {
	c.priceHistMu.Lock()
	defer c.priceHistMu.Unlock()
	if c.priceHistory == nil {
		c.priceHistory = make(map[string][]PricePoint)
	}
	history := c.priceHistory[symbol]
	history = append(history, PricePoint{Price: price, Timestamp: time.Now()})
	if len(history) > 2 {
		history = history[len(history)-2:]
	}
	c.priceHistory[symbol] = history
}

// GetPriceMomentum returns the percentage change between the last two price points.
// Returns 0 if fewer than 2 points are available.
func (c *Cache) GetPriceMomentum(symbol string) float64 {
	c.priceHistMu.RLock()
	defer c.priceHistMu.RUnlock()
	history := c.priceHistory[symbol]
	if len(history) < 2 {
		return 0
	}
	prev := history[0].Price
	curr := history[1].Price
	if prev == 0 {
		return 0
	}
	return (curr - prev) / prev * 100
}
