package handlers

import (
	"sync"
	"time"

	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/cache"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/liquidations"
	"derivs-backend/internal/models"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
	"derivs-backend/internal/worker"
)

// aiCacheEntry holds a cached AI analysis result with its expiry time.
type aiCacheEntry struct {
	result    models.AIAnalysis
	expiresAt time.Time
}

type Handler struct {
	aggregator         *aggregator.Aggregator
	analyzer           *analysis.Analyzer
	cache              *cache.Cache
	detector           *alerts.Detector
	calc               *feargreed.Calculator
	db                 *supabase.Client
	notifier           *notify.TelegramNotifier
	billing            *billing.StripeClient
	lemonSqueezy       *billing.LemonSqueezyClient
	adminSecret        string
	stripePriceIDBasic string
	stripePriceIDPro   string
	worker             *worker.Worker
	liqFeed            *liquidations.Feed
	startTime          time.Time

	// aiCache stores AI analysis results per symbol for 5 minutes to avoid
	// burning Claude tokens on every user refresh.
	aiCacheMu sync.Mutex
	aiCache   map[string]aiCacheEntry
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
	lemonSqueezyClient *billing.LemonSqueezyClient,
	adminSecret string,
	stripePriceIDBasic, stripePriceIDPro string,
	wrk *worker.Worker,
	liqFeed *liquidations.Feed,
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
		lemonSqueezy:       lemonSqueezyClient,
		adminSecret:        adminSecret,
		stripePriceIDBasic: stripePriceIDBasic,
		stripePriceIDPro:   stripePriceIDPro,
		worker:             wrk,
		liqFeed:            liqFeed,
		startTime:          time.Now(),
		aiCache:            make(map[string]aiCacheEntry),
	}
}
