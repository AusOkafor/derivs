package alerts

import "derivs-backend/internal/models"

// IsSafeToSend is the last line of defense before any Telegram send.
// Even if engine or heat feed has a bug, this blocks bad alerts.
func IsSafeToSend(alert models.Alert) bool {
	// Regime/OI alerts have no cluster — always safe
	if alert.ClusterSize == 0 {
		return true
	}
	minSize := GetMinClusterSize(alert.Symbol)
	if alert.ClusterSize < minSize {
		return false
	}
	if alert.Distance < MinDistancePct {
		return false
	}
	return true
}
