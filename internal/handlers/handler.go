package handlers

import (
	"time"

	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/cache"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
	"derivs-backend/internal/worker"
)

type Handler struct {
	aggregator         *aggregator.Aggregator
	analyzer           *analysis.Analyzer
	cache              *cache.Cache
	detector           *alerts.Detector
	calc               *feargreed.Calculator
	db                 *supabase.Client
	notifier           *notify.TelegramNotifier
	billing            *billing.StripeClient
	adminSecret        string
	stripePriceIDBasic string
	stripePriceIDPro   string
	worker             *worker.Worker
	startTime          time.Time
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
	stripePriceIDBasic, stripePriceIDPro string,
	wrk *worker.Worker,
) *Handler {
	return &Handler{
		aggregator:         agg,
		analyzer:           az,
		cache:              c,
		detector:           det,
		calc:               calc,
		db:                 db,
		notifier:           notifier,
		billing:            billingClient,
		adminSecret:        adminSecret,
		stripePriceIDBasic: stripePriceIDBasic,
		stripePriceIDPro:   stripePriceIDPro,
		worker:             wrk,
		startTime:          time.Now(),
	}
}
