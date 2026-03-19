package alerts

import (
	"log"
	"sync"
	"time"
)

var startupTime = time.Now()

const startupGracePeriod = 2 * time.Minute

type CooldownManager struct {
	mu        sync.Mutex
	cooldowns map[string]time.Time
	period    time.Duration
}

func NewCooldownManager(period time.Duration) *CooldownManager {
	cm := &CooldownManager{
		cooldowns: make(map[string]time.Time),
		period:    period,
	}
	go cm.startCleanup()
	return cm
}

func (c *CooldownManager) startCleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.cleanup()
	}
}

func (c *CooldownManager) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.cooldowns {
		if now.Sub(v) > c.period {
			delete(c.cooldowns, k)
		}
	}
	log.Printf("[alerts] cooldown cleanup: %d entries remaining", len(c.cooldowns))
}

func (c *CooldownManager) Allow(key string) bool {
	if time.Since(startupTime) < startupGracePeriod {
		return false // block all alerts during startup grace period
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	last, exists := c.cooldowns[key]
	if exists && time.Since(last) < c.period {
		return false
	}

	c.cooldowns[key] = time.Now()
	return true
}
