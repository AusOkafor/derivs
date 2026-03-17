package alerts

import (
	"fmt"

	"derivs-backend/internal/models"
)

const (
	MinDistancePct = 0.001 // 0.1% minimum (Distance stored as decimal: 0.001 = 0.1%)
)

func ValidateAlert(alert models.Alert) error {
	// Cluster size check — only when alert has cluster data
	if alert.ClusterSize > 0 && alert.ClusterSize < MinClusterSize {
		return fmt.Errorf("cluster $%.0f below $200k minimum", alert.ClusterSize)
	}

	// Distance check — Distance stored as decimal (0.01 = 1%)
	if alert.Distance > 0 && alert.Distance < MinDistancePct {
		return fmt.Errorf("distance %.3f%% below 0.1%% minimum", alert.Distance*100)
	}

	return nil
}

// GenerateFingerprint creates a unique key for this alert condition.
// For cluster-based alerts: symbol:roundedPrice:severity.
// For non-cluster alerts: alert ID (e.g. funding, regime).
func GenerateFingerprint(alert models.Alert) string {
	if alert.ClusterPrice == 0 {
		return alert.ID
	}
	roundedPrice := int(alert.ClusterPrice)
	return fmt.Sprintf("%s:%d:%s", alert.Symbol, roundedPrice, alert.Severity)
}
