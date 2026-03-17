package alerts

import (
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

		// Step 2 — Fingerprint deduplication
		fp := GenerateFingerprint(alert)
		if !e.cooldown.Allow(fp) {
			log.Printf("[alerts] COOLDOWN %s: fingerprint %s", alert.ID, fp)
			continue
		}

		// Step 3 — Downgrade HIGH to MEDIUM if cluster < $500k
		if alert.Severity == "high" && alert.ClusterSize > 0 && alert.ClusterSize < highSeverityMinSize {
			alert.Severity = "medium"
		}

		log.Printf("[alerts] ALLOWED %s %s: cluster $%.0f dist %.2f%%",
			alert.Severity, alert.ID, alert.ClusterSize, alert.Distance*100)
		valid = append(valid, alert)
	}

	return valid
}
