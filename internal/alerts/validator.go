package alerts

import (
	"fmt"
	"math"

	"derivs-backend/internal/models"
)

const (
	MinDistancePct = 0.001 // 0.1% minimum (Distance stored as decimal: 0.001 = 0.1%)
)

func ValidateAlert(alert models.Alert) error {
	// Regime/OI alerts have ClusterSize = 0 — no size/distance filter, cooldown only
	if alert.ClusterSize == 0 {
		return nil
	}

	// Cluster size check
	if alert.ClusterSize < MinClusterSize {
		return fmt.Errorf("cluster $%.0f below $200k minimum", alert.ClusterSize)
	}

	// Distance check — block when distance < 0.1% (including 0.00%)
	if alert.Distance < MinDistancePct {
		return fmt.Errorf("distance %.3f%% below 0.1%% minimum", alert.Distance*100)
	}

	return nil
}

// GenerateFingerprint creates a unique key for this alert condition.
// For cluster-based alerts: symbol:roundedPrice:severity.
// Round to nearest 10 for BTC-range, 1 for mid-range, 0.0001 for low-value (DOGE, XRP).
// For non-cluster alerts: alert ID (e.g. funding, regime).
func GenerateFingerprint(alert models.Alert) string {
	if alert.ClusterPrice > 0 {
		var rounded int
		if alert.ClusterPrice >= 1000 {
			rounded = int(math.Round(alert.ClusterPrice/10) * 10)
		} else if alert.ClusterPrice >= 1 {
			rounded = int(math.Round(alert.ClusterPrice))
		} else {
			rounded = int(math.Round(alert.ClusterPrice * 10000))
		}
		return fmt.Sprintf("%s:%d:%s", alert.Symbol, rounded, alert.Severity)
	}
	return alert.ID
}
