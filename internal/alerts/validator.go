package alerts

import (
	"fmt"
	"math"

	"derivs-backend/internal/models"
)

const (
	MinDistancePct = 0.001 // 0.1% minimum (Distance stored as decimal: 0.001 = 0.1%)
)

// symbolThresholds — adaptive min cluster size per symbol based on typical OI.
var symbolThresholds = map[string]float64{
	"BTC":    500_000,  // $500K — large cap
	"ETH":    300_000,  // $300K — large cap
	"SOL":    200_000,  // $200K — mid cap
	"BNB":    200_000,  // $200K — mid cap
	"XRP":    150_000,  // $150K — mid cap
	"DOGE":   100_000,  // $100K — high volume meme
	"AVAX":   100_000,  // $100K — mid cap
	"LINK":   100_000,  // $100K
	"ARB":    80_000,   // $80K — smaller cap
	"OP":     80_000,   // $80K — smaller cap
	"INJ":    80_000,   // $80K
	"SUI":    80_000,   // $80K
	"TIA":    60_000,   // $60K — small cap
	"WLD":    60_000,   // $60K
	"PENDLE": 60_000,   // $60K
	"TON":    80_000,   // $80K
}

// GetMinClusterSize returns symbol-aware minimum cluster size. Default $200K.
func GetMinClusterSize(symbol string) float64 {
	if threshold, ok := symbolThresholds[symbol]; ok {
		return threshold
	}
	return MinClusterSize
}

func ValidateAlert(alert models.Alert) error {
	// Regime/OI alerts have ClusterSize = 0 — no size/distance filter, cooldown only
	if alert.ClusterSize == 0 {
		return nil
	}

	minSize := GetMinClusterSize(alert.Symbol)
	if alert.ClusterSize < minSize {
		return fmt.Errorf("cluster $%.0f below $%.0f minimum for %s",
			alert.ClusterSize, minSize, alert.Symbol)
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
