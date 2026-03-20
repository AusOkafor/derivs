package aggregator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/config"
	"derivs-backend/internal/models"
)

type exchangeHealth struct {
	lastSuccess time.Time
	lastError   string
}

type Aggregator struct {
	cfg        *config.Config
	httpClient *http.Client
	health     map[string]exchangeHealth
	mu         sync.RWMutex
}

func New(cfg *config.Config) *Aggregator {
	return &Aggregator{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		health: make(map[string]exchangeHealth),
	}
}

// ExchangeStatus returns "ok" if last fetch < 5 minutes ago, "degraded" if 5-15 min, "down" if >15 min.
func (a *Aggregator) ExchangeStatus(exchange string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, ok := a.health[exchange]
	if !ok {
		return "unknown"
	}
	age := time.Since(h.lastSuccess)
	if age < 5*time.Minute {
		return "ok"
	}
	if age < 15*time.Minute {
		return "degraded"
	}
	return "down"
}

func (a *Aggregator) recordSuccess(exchange string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.health[exchange] = exchangeHealth{lastSuccess: time.Now()}
}

func (a *Aggregator) recordError(exchange string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := a.health[exchange]
	h.lastError = ""
	if err != nil {
		h.lastError = err.Error()
	}
	a.health[exchange] = h
}

// ─── Bybit raw response shapes ────────────────────────────────────────────────

type bybitResp[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  T      `json:"result"`
}

type bybitTickerItem struct {
	Symbol          string `json:"symbol"`
	LastPrice       string `json:"lastPrice"`
	Price24hPcnt    string `json:"price24hPcnt"`
	FundingRate     string `json:"fundingRate"`
	NextFundingTime string `json:"nextFundingTime"`
	IndexPrice      string `json:"indexPrice"`
	MarkPrice       string `json:"markPrice"`
}

type bybitTickerResult struct {
	List []bybitTickerItem `json:"list"`
}

type bybitOIItem struct {
	OpenInterest string `json:"openInterest"`
	Timestamp    string `json:"timestamp"`
}

type bybitOIResult struct {
	List []bybitOIItem `json:"list"`
}

type bybitOrderbookResult struct {
	Bids [][]string `json:"b"` // [price, size], best bid first
	Asks [][]string `json:"a"` // [price, size], best ask first
}

type bybitFundingHistItem struct {
	Symbol               string `json:"symbol"`
	FundingRate          string `json:"fundingRate"`
	FundingRateTimestamp string `json:"fundingRateTimestamp"`
}

type bybitFundingHistResult struct {
	List []bybitFundingHistItem `json:"list"`
}

type bybitLSItem struct {
	Symbol    string `json:"symbol"`
	BuyRatio  string `json:"buyRatio"`
	SellRatio string `json:"sellRatio"`
	Timestamp string `json:"timestamp"`
}

type bybitLSResult struct {
	List []bybitLSItem `json:"list"`
}

// ─── Binance raw response shapes ─────────────────────────────────────────────

type bnLongShortItem struct {
	Symbol         string `json:"symbol"`
	LongShortRatio string `json:"longShortRatio"` // Binance returns string numbers
	LongAccount    string `json:"longAccount"`
	ShortAccount   string `json:"shortAccount"`
	Timestamp      int64  `json:"timestamp"` // unix ms
}

// ─── OKX raw response shapes ───────────────────────────────────────────────────

type okxLSResult struct {
	Data [][]string `json:"data"`
}

// ─── HTTP helper ──────────────────────────────────────────────────────────────

func (a *Aggregator) publicGet(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ─── Symbol helpers ───────────────────────────────────────────────────────────

// perpSymbol converts "BTC" → "BTCUSDT" for linear perpetual endpoints.
func perpSymbol(symbol string) string {
	return strings.ToUpper(symbol) + "USDT"
}

func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

// ─── Private fetch methods ────────────────────────────────────────────────────

// fetchBybitFundingRate fetches the current funding rate from Bybit's ticker endpoint.
// Also returns indexPrice and markPrice for perp/spot basis calculation.
func (a *Aggregator) fetchBybitFundingRate(ctx context.Context, symbol string) (rate float64, nextFunding int64, indexPrice float64, markPrice float64, err error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitTickerResult]
	if err = a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("bybit", err)
		return 0, 0, 0, 0, fmt.Errorf("fetchBybitFundingRate: %w", err)
	}
	a.recordSuccess("bybit")
	if len(raw.Result.List) == 0 {
		return 0, 0, 0, 0, fmt.Errorf("fetchBybitFundingRate: no data for %s", symbol)
	}

	item := raw.Result.List[0]
	rate, _ = strconv.ParseFloat(item.FundingRate, 64)
	nextFunding, _ = strconv.ParseInt(item.NextFundingTime, 10, 64)
	indexPrice, _ = strconv.ParseFloat(item.IndexPrice, 64)
	markPrice, _ = strconv.ParseFloat(item.MarkPrice, 64)
	return
}

// fetchBinanceFundingRate fetches the current funding rate from Binance premium index.
func (a *Aggregator) fetchBinanceFundingRate(ctx context.Context, symbol string) (float64, error) {
	u := fmt.Sprintf(
		"https://fapi.binance.com/fapi/v1/premiumIndex?symbol=%s",
		perpSymbol(symbol),
	)

	var raw struct {
		Symbol         string `json:"symbol"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
	}
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("binance", err)
		return 0, fmt.Errorf("fetchBinanceFundingRate: %w", err)
	}
	a.recordSuccess("binance")
	rate, _ := strconv.ParseFloat(raw.LastFundingRate, 64)
	return rate, nil
}

// fetchOKXFundingRate fetches the current funding rate from OKX.
func (a *Aggregator) fetchOKXFundingRate(ctx context.Context, symbol string) (float64, error) {
	instID := strings.ToUpper(symbol) + "-USDT-SWAP"
	u := fmt.Sprintf(
		"https://www.okx.com/api/v5/public/funding-rate?instId=%s",
		instID,
	)

	var raw struct {
		Code string `json:"code"`
		Data []struct {
			InstID string `json:"instId"`
			FundingRate string `json:"fundingRate"`
		} `json:"data"`
	}
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("okx", err)
		return 0, fmt.Errorf("fetchOKXFundingRate: %w", err)
	}
	a.recordSuccess("okx")
	if len(raw.Data) == 0 {
		return 0, fmt.Errorf("fetchOKXFundingRate: no data for %s", symbol)
	}
	rate, _ := strconv.ParseFloat(raw.Data[0].FundingRate, 64)
	return rate, nil
}

// fetchOpenInterest fetches OI history from Bybit and a current price for
// contract→USD conversion. Two sequential calls; concurrency is handled at the
// FetchSnapshot level.
func (a *Aggregator) fetchOpenInterest(ctx context.Context, symbol string) (models.OpenInterest, error) {
	sym := perpSymbol(symbol)

	// 1. OI history (newest-first from Bybit).
	oiURL := fmt.Sprintf(
		"https://api.bybit.com/v5/market/open-interest?category=linear&symbol=%s&intervalTime=1h&limit=25",
		sym,
	)
	var oiRaw bybitResp[bybitOIResult]
	if err := a.publicGet(ctx, oiURL, &oiRaw); err != nil {
		a.recordError("bybit", err)
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest: %w", err)
	}
	a.recordSuccess("bybit")
	if len(oiRaw.Result.List) == 0 {
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest: no OI data for %s", symbol)
	}

	// 2. Current price for contracts→USD conversion.
	tickerURL := fmt.Sprintf(
		"https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s",
		sym,
	)
	var tickerRaw bybitResp[bybitTickerResult]
	if err := a.publicGet(ctx, tickerURL, &tickerRaw); err != nil {
		a.recordError("bybit", err)
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest price: %w", err)
	}
	a.recordSuccess("bybit")
	if len(tickerRaw.Result.List) == 0 {
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest: no ticker data for %s", symbol)
	}
	price, _ := strconv.ParseFloat(tickerRaw.Result.List[0].LastPrice, 64)
	if price == 0 {
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest: invalid price for %s", symbol)
	}

	// Bybit returns newest-first — reverse so oldest is at index 0.
	list := oiRaw.Result.List
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	// oiUSD converts a list entry at idx from contracts to USD notional.
	oiUSD := func(idx int) float64 {
		if idx < 0 || idx >= len(list) {
			return 0
		}
		contracts, _ := strconv.ParseFloat(list[idx].OpenInterest, 64)
		return contracts * price
	}

	n := len(list) - 1 // index of the most recent candle
	current := oiUSD(n)
	lookback := func(periods int) float64 {
		return pctChange(oiUSD(n-periods), current)
	}

	return models.OpenInterest{
		Symbol:      symbol,
		OIUsd:       current,
		OIChange1h:  lookback(1),
		OIChange4h:  lookback(4),
		OIChange24h: lookback(24),
		Timestamp:   time.Now().UTC(),
	}, nil
}

// fetchLiquidationMap builds a synthetic liquidation map from the Bybit
// orderbook. Top-10 bids become "long" levels, top-10 asks become "short"
// levels, and CurrentPrice is the bid/ask midpoint.
func (a *Aggregator) fetchLiquidationMap(ctx context.Context, symbol string) (models.LiquidationMap, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/orderbook?category=linear&symbol=%s&limit=50",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitOrderbookResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("bybit", err)
		return models.LiquidationMap{}, fmt.Errorf("fetchLiquidationMap: %w", err)
	}
	a.recordSuccess("bybit")

	ob := raw.Result
	if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return models.LiquidationMap{}, fmt.Errorf("fetchLiquidationMap: empty orderbook for %s", symbol)
	}

	parseLevel := func(entry []string, side string) (models.LiquidationLevel, bool) {
		if len(entry) < 2 {
			return models.LiquidationLevel{}, false
		}
		price, err1 := strconv.ParseFloat(entry[0], 64)
		size, err2 := strconv.ParseFloat(entry[1], 64)
		if err1 != nil || err2 != nil {
			return models.LiquidationLevel{}, false
		}
		return models.LiquidationLevel{
			Symbol:  symbol,
			Price:   price,
			Side:    side,
			SizeUsd: price * size, // contracts × price = USD notional
		}, true
	}

	const maxLevels = 10
	levels := make([]models.LiquidationLevel, 0, maxLevels*2)

	for i := 0; i < len(ob.Bids) && i < maxLevels; i++ {
		if l, ok := parseLevel(ob.Bids[i], "long"); ok {
			levels = append(levels, l)
		}
	}
	for i := 0; i < len(ob.Asks) && i < maxLevels; i++ {
		if l, ok := parseLevel(ob.Asks[i], "short"); ok {
			levels = append(levels, l)
		}
	}

	bestBid, _ := strconv.ParseFloat(ob.Bids[0][0], 64)
	bestAsk, _ := strconv.ParseFloat(ob.Asks[0][0], 64)

	return models.LiquidationMap{
		Symbol:       symbol,
		Levels:       levels,
		CurrentPrice: (bestBid + bestAsk) / 2,
		Timestamp:    time.Now().UTC(),
	}, nil
}

// fetchBinanceData fetches the global long/short account ratio from Binance.
func (a *Aggregator) fetchBinanceData(ctx context.Context, symbol string) ([]models.LongShortRatio, error) {
	u := fmt.Sprintf(
		"https://fapi.binance.com/futures/data/globalLongShortAccountRatio?symbol=%s&period=1h&limit=1",
		perpSymbol(symbol),
	)

	var raw []bnLongShortItem
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("binance", err)
		return nil, fmt.Errorf("fetchBinanceData: %w", err)
	}
	a.recordSuccess("binance")

	ratios := make([]models.LongShortRatio, 0, len(raw))
	for _, item := range raw {
		longPct, _ := strconv.ParseFloat(item.LongAccount, 64)
		shortPct, _ := strconv.ParseFloat(item.ShortAccount, 64)
		ratio, _ := strconv.ParseFloat(item.LongShortRatio, 64)

		ratios = append(ratios, models.LongShortRatio{
			Symbol:    symbol,
			Exchange:  "Binance",
			LongPct:   longPct * 100,
			ShortPct:  shortPct * 100,
			Ratio:     ratio,
			Timestamp: time.UnixMilli(item.Timestamp).UTC(),
		})
	}

	return ratios, nil
}

// FetchOIHistory returns the last 48 hourly OI candles for a symbol.
// Reuses the existing OI endpoint:
// GET https://api.bybit.com/v5/market/open-interest?category=linear&symbol={symbol}USDT&intervalTime=1h&limit=48
// Converts each entry from contracts to USD using current price from ticker.
// Returns oldest-first.
func (a *Aggregator) FetchOIHistory(ctx context.Context, symbol string, limit int) ([]models.OICandle, error) {
	sym := perpSymbol(symbol)

	oiURL := fmt.Sprintf(
		"https://api.bybit.com/v5/market/open-interest?category=linear&symbol=%s&intervalTime=1h&limit=%d",
		sym, limit,
	)
	var oiRaw bybitResp[bybitOIResult]
	if err := a.publicGet(ctx, oiURL, &oiRaw); err != nil {
		a.recordError("bybit", err)
		return nil, fmt.Errorf("FetchOIHistory: %w", err)
	}
	a.recordSuccess("bybit")
	if len(oiRaw.Result.List) == 0 {
		return nil, fmt.Errorf("FetchOIHistory: no OI data for %s", symbol)
	}

	tickerURL := fmt.Sprintf(
		"https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s",
		sym,
	)
	var tickerRaw bybitResp[bybitTickerResult]
	if err := a.publicGet(ctx, tickerURL, &tickerRaw); err != nil {
		a.recordError("bybit", err)
		return nil, fmt.Errorf("FetchOIHistory price: %w", err)
	}
	a.recordSuccess("bybit")
	if len(tickerRaw.Result.List) == 0 {
		return nil, fmt.Errorf("FetchOIHistory: no ticker data for %s", symbol)
	}
	price, _ := strconv.ParseFloat(tickerRaw.Result.List[0].LastPrice, 64)
	if price == 0 {
		return nil, fmt.Errorf("FetchOIHistory: invalid price for %s", symbol)
	}

	list := oiRaw.Result.List
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	candles := make([]models.OICandle, 0, len(list))
	for _, item := range list {
		contracts, _ := strconv.ParseFloat(item.OpenInterest, 64)
		ts, _ := strconv.ParseInt(item.Timestamp, 10, 64)
		candles = append(candles, models.OICandle{
			Timestamp: ts,
			OIUsd:     contracts * price,
		})
	}
	return candles, nil
}

// FetchFundingHistory returns the last `limit` hourly funding rate points for
// a symbol, oldest-first. Bybit returns newest-first so we reverse the list.
func (a *Aggregator) FetchFundingHistory(ctx context.Context, symbol string, limit int) ([]models.FundingRatePoint, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&limit=%d",
		perpSymbol(symbol), limit,
	)

	var raw bybitResp[bybitFundingHistResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("bybit", err)
		return nil, fmt.Errorf("FetchFundingHistory: %w", err)
	}
	a.recordSuccess("bybit")

	list := raw.Result.List
	// Reverse: Bybit is newest-first, we want oldest at index 0.
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	points := make([]models.FundingRatePoint, 0, len(list))
	for _, item := range list {
		rate, _ := strconv.ParseFloat(item.FundingRate, 64)
		ts, _ := strconv.ParseInt(item.FundingRateTimestamp, 10, 64)
		points = append(points, models.FundingRatePoint{
			Timestamp: ts,
			Rate:      rate,
		})
	}
	return points, nil
}

// fetchOKXLongShort fetches long/short ratio from OKX.
// GET https://www.okx.com/api/v5/rubik/stat/contracts/long-short-account-ratio-contract?instId={symbol}-USDT-SWAP&period=1H
// No API key required.
// Parse longRatio and shortRatio as float64 * 100 for percentages.
func (a *Aggregator) fetchOKXLongShort(ctx context.Context, symbol string) (models.LongShortRatio, error) {
	instID := strings.ToUpper(symbol) + "-USDT-SWAP"
	u := fmt.Sprintf(
		"https://www.okx.com/api/v5/rubik/stat/contracts/long-short-account-ratio-contract?instId=%s&period=1H",
		instID,
	)

	var raw struct {
		Code string       `json:"code"`
		Data [][]string   `json:"data"`
	}
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("okx", err)
		return models.LongShortRatio{}, fmt.Errorf("fetchOKXLongShort: %w", err)
	}
	a.recordSuccess("okx")
	if len(raw.Data) == 0 {
		return models.LongShortRatio{}, fmt.Errorf("fetchOKXLongShort: no data for %s", symbol)
	}

	item := raw.Data[0]
	if len(item) < 2 {
		return models.LongShortRatio{}, fmt.Errorf("fetchOKXLongShort: unexpected format for %s", symbol)
	}

	ts, _ := strconv.ParseInt(item[0], 10, 64)
	ratio, _ := strconv.ParseFloat(item[1], 64)

	// Derive longPct and shortPct from ratio
	// ratio = longs/shorts, and longPct + shortPct = 100
	// longPct = ratio / (1 + ratio) * 100
	longPct := ratio / (1+ratio) * 100
	shortPct := 100 - longPct

	return models.LongShortRatio{
		Symbol:    symbol,
		Exchange:  "OKX",
		LongPct:   longPct,
		ShortPct:  shortPct,
		Ratio:     ratio,
		Timestamp: time.UnixMilli(ts).UTC(),
	}, nil
}

// fetchBybitLongShort fetches the global long/short account ratio from Bybit.
func (a *Aggregator) fetchBybitLongShort(ctx context.Context, symbol string) (models.LongShortRatio, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/account-ratio?category=linear&symbol=%s&period=1h&limit=1",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitLSResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("bybit", err)
		return models.LongShortRatio{}, fmt.Errorf("fetchBybitLongShort: %w", err)
	}
	a.recordSuccess("bybit")
	if len(raw.Result.List) == 0 {
		return models.LongShortRatio{}, fmt.Errorf("fetchBybitLongShort: no data for %s", symbol)
	}

	item := raw.Result.List[0]
	longPct, _ := strconv.ParseFloat(item.BuyRatio, 64)
	shortPct, _ := strconv.ParseFloat(item.SellRatio, 64)
	tsMs, _ := strconv.ParseInt(item.Timestamp, 10, 64)

	longPct *= 100
	shortPct *= 100

	return models.LongShortRatio{
		Symbol:    symbol,
		Exchange:  "Bybit",
		LongPct:   longPct,
		ShortPct:  shortPct,
		Ratio:     longPct / shortPct,
		Timestamp: time.UnixMilli(tsMs).UTC(),
	}, nil
}

// ─── Public ───────────────────────────────────────────────────────────────────

// FetchTicker returns the current price and 24h change percent for a symbol.
func (a *Aggregator) FetchTicker(ctx context.Context, symbol string) (price float64, change24h float64, err error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitTickerResult]
	if err = a.publicGet(ctx, u, &raw); err != nil {
		a.recordError("bybit", err)
		return 0, 0, fmt.Errorf("FetchTicker: %w", err)
	}
	a.recordSuccess("bybit")
	if len(raw.Result.List) == 0 {
		return 0, 0, fmt.Errorf("FetchTicker: no data for %s", symbol)
	}

	item := raw.Result.List[0]
	price, _ = strconv.ParseFloat(item.LastPrice, 64)
	pct, _ := strconv.ParseFloat(item.Price24hPcnt, 64)
	change24h = pct * 100
	return
}

// FetchSnapshot fetches all four data sources concurrently and assembles a
// MarketSnapshot. Individual fetch failures are logged but do not abort the
// snapshot — callers receive whatever partial data was collected.
func (a *Aggregator) FetchSnapshot(ctx context.Context, symbol string) (models.MarketSnapshot, error) {
	snap := models.MarketSnapshot{
		Symbol:    symbol,
		Timestamp: time.Now().UTC(),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		var rates []models.ExchangeFundingRate
		var sum float64
		var count int
		var nextFunding int64

		// Bybit
		if r, nf, idxPrice, mkPrice, err := a.fetchBybitFundingRate(ctx, symbol); err != nil {
			log.Printf("aggregator: fetchBybitFundingRate: %v", err)
		} else {
			rates = append(rates, models.ExchangeFundingRate{Exchange: "Bybit", Rate: r, RatePct: r * 100})
			sum += r
			count++
			nextFunding = nf
			if idxPrice > 0 {
				perpPrice := 0.0
				if r2, _, err2 := a.FetchTicker(ctx, symbol); err2 == nil {
					perpPrice = r2
				}
				basisPct := 0.0
				if idxPrice > 0 && perpPrice > 0 {
					basisPct = (perpPrice - idxPrice) / idxPrice * 100
				}
				mu.Lock()
				snap.PerpBasis = &models.PerpBasis{
					PerpPrice:  perpPrice,
					IndexPrice: idxPrice,
					MarkPrice:  mkPrice,
					BasisPct:   basisPct,
				}
				mu.Unlock()
			}
		}
		// Binance
		if r, err := a.fetchBinanceFundingRate(ctx, symbol); err != nil {
			log.Printf("aggregator: fetchBinanceFundingRate: %v", err)
		} else {
			rates = append(rates, models.ExchangeFundingRate{Exchange: "Binance", Rate: r, RatePct: r * 100})
			sum += r
			count++
		}
		// OKX
		if r, err := a.fetchOKXFundingRate(ctx, symbol); err != nil {
			log.Printf("aggregator: fetchOKXFundingRate: %v", err)
		} else {
			rates = append(rates, models.ExchangeFundingRate{Exchange: "OKX", Rate: r, RatePct: r * 100})
			sum += r
			count++
		}

		avgRate := 0.0
		if count > 0 {
			avgRate = sum / float64(count)
		}
		mu.Lock()
		snap.FundingRate = models.FundingRate{
			Symbol:          symbol,
			Rate:            avgRate,
			NextFundingTime: nextFunding,
			Timestamp:       time.Now().UTC(),
		}
		snap.ExchangeFunding = rates
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		oi, err := a.fetchOpenInterest(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
			return
		}
		mu.Lock()
		snap.OpenInterest = oi
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		lm, err := a.fetchLiquidationMap(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
			return
		}
		mu.Lock()
		snap.LiquidationMap = lm
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		var ratios []models.LongShortRatio

		binance, err := a.fetchBinanceData(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
		} else {
			ratios = append(ratios, binance...)
		}

		bybit, err := a.fetchBybitLongShort(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
		} else {
			ratios = append(ratios, bybit)
		}

		okx, err := a.fetchOKXLongShort(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
		} else {
			ratios = append(ratios, okx)
		}

		if len(ratios) > 0 {
			mu.Lock()
			snap.LongShortRatios = ratios
			mu.Unlock()
		}
	}()

	wg.Wait()
	return snap, nil
}
