package handlers

import (
	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/cache"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
)

type Handler struct {
	aggregator  *aggregator.Aggregator
	analyzer    *analysis.Analyzer
	cache       *cache.Cache
	detector    *alerts.Detector
	calc        *feargreed.Calculator
	db          *supabase.Client
	notifier    *notify.TelegramNotifier
	billing     *billing.StripeClient
	adminSecret string
}

func New(
	agg *aggregator.Aggregator,
	az *analysis.Analyzer,
	c *cache.Cache,
	det *alerts.Detector,
	calc *feargreed.Calculator,
	db *supabase.Client,
	notifier *notify.TelegramNotifier,
	billingClient *billing.StripeClient,
	adminSecret string,
) *Handler {
	return &Handler{
		aggregator:  agg,
		analyzer:    az,
		cache:       c,
		detector:    det,
		calc:        calc,
		db:          db,
		notifier:    notifier,
		billing:     billingClient,
		adminSecret: adminSecret,
	}
}
