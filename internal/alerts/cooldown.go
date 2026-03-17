package alerts

import (
	"sync"
	"time"
)

type CooldownManager struct {
	mu       sync.Mutex
	cooldowns map[string]time.Time
	period   time.Duration
}

func NewCooldownManager(period time.Duration) *CooldownManager {
	return &CooldownManager{
		cooldowns: make(map[string]time.Time),
		period:    period,
	}
}

func (c *CooldownManager) Allow(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	last, exists := c.cooldowns[key]
	if exists && time.Since(last) < c.period {
		return false
	}

	c.cooldowns[key] = time.Now()
	return true
}
