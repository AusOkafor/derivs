package alerts

import (
	"fmt"
	"log"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

var (
	blockLogMu   sync.Mutex
	lastBlockLog = make(map[string]time.Time)
)

func shouldLogBlock(key string) bool {
	blockLogMu.Lock()
	defer blockLogMu.Unlock()
	if last, ok := lastBlockLog[key]; ok {
		if time.Since(last) < 5*time.Minute {
			return false
		}
	}
	lastBlockLog[key] = time.Now()
	return true
}

type Engine struct {
	cooldown *CooldownManager
}

func NewEngine() *Engine {
	return &Engine{
		cooldown: NewCooldownManager(30 * time.Minute),
	}
}

// Cooldown returns the cooldown manager for external use (e.g. heat feed throttling).
func (e *Engine) Cooldown() *CooldownManager {
	return e.cooldown
}

// Process takes raw alerts from Analyze() and returns only valid, non-duplicate alerts.
func (e *Engine) Process(alerts []models.Alert) []models.Alert {
	var valid []models.Alert

	for _, alert := range alerts {
		// Step 1 — Validate
		if err := ValidateAlert(alert); err != nil {
			if shouldLogBlock(alert.ID) {
				log.Printf("[alerts] BLOCKED %s: %v", alert.ID, err)
			}
			continue
		}

		// Step 2 — Multi-factor confirmation (skip for regime alerts; use probability as proxy)
		if alert.ClusterSize > 0 {
			if alert.Probability < 65 {
				if shouldLogBlock(alert.ID) {
					log.Printf("[alerts] WEAK %s: probability %d%% below 65%% minimum", alert.ID, alert.Probability)
				}
				continue
			}
		}

		// Step 3 — Cooldown (shared per-symbol for regime/OI alerts)
		var cooldownKey string
		if alert.ClusterSize == 0 {
			cooldownKey = fmt.Sprintf("%s:regime", alert.Symbol)
			if !e.cooldown.Allow(cooldownKey) {
				log.Printf("[alerts] COOLDOWN regime %s", alert.Symbol)
				continue
			}
		} else {
			fp := GenerateFingerprint(alert)
			if !e.cooldown.Allow(fp) {
				log.Printf("[alerts] COOLDOWN %s: fingerprint %s", alert.ID, fp)
				continue
			}
		}

		// Step 4 — Downgrade HIGH to MEDIUM if cluster < $500k
		if alert.Severity == "high" && alert.ClusterSize > 0 && alert.ClusterSize < highSeverityMinSize {
			alert.Severity = "medium"
		}

		if alert.ClusterSize == 0 {
			log.Printf("[alerts] ALLOWED %s %s (regime/OI alert)", alert.Severity, alert.ID)
		} else {
			log.Printf("[alerts] ALLOWED %s %s: cluster $%.0f dist %.2f%%",
				alert.Severity, alert.ID, alert.ClusterSize, alert.Distance*100)
		}
		valid = append(valid, alert)
	}

	return valid
}
