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

type Aggregator struct {
	cfg        *config.Config
	httpClient *http.Client
}

func New(cfg *config.Config) *Aggregator {
	return &Aggregator{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
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

// fetchFundingRate fetches the current funding rate from Bybit's ticker endpoint.
func (a *Aggregator) fetchFundingRate(ctx context.Context, symbol string) (models.FundingRate, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitTickerResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		return models.FundingRate{}, fmt.Errorf("fetchFundingRate: %w", err)
	}
	if len(raw.Result.List) == 0 {
		return models.FundingRate{}, fmt.Errorf("fetchFundingRate: no data for %s", symbol)
	}

	item := raw.Result.List[0]
	rate, _ := strconv.ParseFloat(item.FundingRate, 64)
	nextFunding, _ := strconv.ParseInt(item.NextFundingTime, 10, 64)

	return models.FundingRate{
		Symbol:          symbol,
		Rate:            rate,
		NextFundingTime: nextFunding,
		Timestamp:       time.Now().UTC(),
	}, nil
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
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest: %w", err)
	}
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
		return models.OpenInterest{}, fmt.Errorf("fetchOpenInterest price: %w", err)
	}
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
		return models.LiquidationMap{}, fmt.Errorf("fetchLiquidationMap: %w", err)
	}

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
		return nil, fmt.Errorf("fetchBinanceData: %w", err)
	}

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

// FetchFundingHistory returns the last `limit` hourly funding rate points for
// a symbol, oldest-first. Bybit returns newest-first so we reverse the list.
func (a *Aggregator) FetchFundingHistory(ctx context.Context, symbol string, limit int) ([]models.FundingRatePoint, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&limit=%d",
		perpSymbol(symbol), limit,
	)

	var raw bybitResp[bybitFundingHistResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		return nil, fmt.Errorf("FetchFundingHistory: %w", err)
	}

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

// fetchBybitLongShort fetches the global long/short account ratio from Bybit.
func (a *Aggregator) fetchBybitLongShort(ctx context.Context, symbol string) (models.LongShortRatio, error) {
	u := fmt.Sprintf(
		"https://api.bybit.com/v5/market/account-ratio?category=linear&symbol=%s&period=1h&limit=1",
		perpSymbol(symbol),
	)

	var raw bybitResp[bybitLSResult]
	if err := a.publicGet(ctx, u, &raw); err != nil {
		return models.LongShortRatio{}, fmt.Errorf("fetchBybitLongShort: %w", err)
	}
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
		return 0, 0, fmt.Errorf("FetchTicker: %w", err)
	}
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
		fr, err := a.fetchFundingRate(ctx, symbol)
		if err != nil {
			log.Printf("aggregator: %v", err)
			return
		}
		mu.Lock()
		snap.FundingRate = fr
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

		if len(ratios) > 0 {
			mu.Lock()
			snap.LongShortRatios = ratios
			mu.Unlock()
		}
	}()

	wg.Wait()
	return snap, nil
}
