package alerts

import (
	"fmt"
	"log"
	"time"

	"derivs-backend/internal/models"
)

type Engine struct {
	cooldown *CooldownManager
}

func NewEngine() *Engine {
	return &Engine{
		cooldown: NewCooldownManager(30 * time.Minute),
	}
}

// Process takes raw alerts from Analyze() and returns only valid, non-duplicate alerts.
func (e *Engine) Process(alerts []models.Alert) []models.Alert {
	var valid []models.Alert

	for _, alert := range alerts {
		// Step 1 — Validate
		if err := ValidateAlert(alert); err != nil {
			log.Printf("[alerts] BLOCKED %s: %v", alert.ID, err)
			continue
		}

		// Step 2 — Cooldown (shared per-symbol for regime/OI alerts)
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

		// Step 3 — Downgrade HIGH to MEDIUM if cluster < $500k
		log.Printf("[engine] before downgrade: %s severity=%s cluster=%.0f",
			alert.ID, alert.Severity, alert.ClusterSize)
		if alert.Severity == "high" && alert.ClusterSize > 0 && alert.ClusterSize < highSeverityMinSize {
			log.Printf("[engine] downgrading %s from high to medium: cluster $%.0f < $500k",
				alert.ID, alert.ClusterSize)
			alert.Severity = "medium"
		}

		log.Printf("[alerts] ALLOWED %s %s: cluster $%.0f dist %.2f%%",
			alert.Severity, alert.ID, alert.ClusterSize, alert.Distance*100)
		valid = append(valid, alert)
	}

	return valid
}
