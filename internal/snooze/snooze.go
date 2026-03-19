// Package snooze provides an in-memory per-subscriber alert snooze manager.
// Snoozes survive in-process and expire automatically — no DB required.
package snooze

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Manager stores snooze expiry times keyed by "subscriberID:SYMBOL".
type Manager struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

// Global is the package-level singleton used across the worker and handlers.
var Global = &Manager{entries: make(map[string]time.Time)}

func (m *Manager) key(subscriberID, symbol string) string {
	return subscriberID + ":" + strings.ToUpper(symbol)
}

// Snooze sets a snooze for symbol (or "ALL") for the given duration.
func (m *Manager) Snooze(subscriberID, symbol string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[m.key(subscriberID, symbol)] = time.Now().Add(d)
}

// IsSnoozed returns true if alerts for symbol (or all symbols) are snoozed for this subscriber.
func (m *Manager) IsSnoozed(subscriberID, symbol string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, k := range []string{m.key(subscriberID, symbol), m.key(subscriberID, "ALL")} {
		if exp, ok := m.entries[k]; ok && now.Before(exp) {
			return true
		}
	}
	return false
}

// Unsnooze cancels the snooze for symbol for this subscriber.
func (m *Manager) Unsnooze(subscriberID, symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, m.key(subscriberID, symbol))
}

// List returns all active snooze entries for a subscriber as symbol → expiry.
func (m *Manager) List(subscriberID string) map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := subscriberID + ":"
	result := make(map[string]time.Time)
	now := time.Now()
	for k, exp := range m.entries {
		if strings.HasPrefix(k, prefix) && now.Before(exp) {
			result[strings.TrimPrefix(k, prefix)] = exp
		}
	}
	return result
}

// FormatRemaining returns a human-readable time remaining for a snooze.
func FormatRemaining(exp time.Time) string {
	remaining := time.Until(exp).Round(time.Minute)
	if remaining <= 0 {
		return "expiring"
	}
	if remaining < time.Hour {
		return fmt.Sprintf("%dm", int(remaining.Minutes()))
	}
	return fmt.Sprintf("%.0fh%dm", remaining.Hours(), int(remaining.Minutes())%60)
}

// ParseDuration converts bot shorthand (30m, 1h, 4h, 24h) to time.Duration.
func ParseDuration(s string) (time.Duration, bool) {
	switch strings.ToLower(s) {
	case "30m":
		return 30 * time.Minute, true
	case "1h":
		return 1 * time.Hour, true
	case "4h":
		return 4 * time.Hour, true
	case "24h", "1d":
		return 24 * time.Hour, true
	}
	return 0, false
}
