package liquidations

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type LiquidationEvent struct {
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"` // "BUY" = short liquidated, "SELL" = long liquidated
	Price     float64   `json:"price"`
	Quantity  float64   `json:"quantity"`
	SizeUSD   float64   `json:"size_usd"`
	Timestamp time.Time `json:"timestamp"`
}

type RecentLiquidations struct {
	Events    []LiquidationEvent `json:"events"`
	TotalLong float64            `json:"total_long_usd"`  // longs liquidated
	TotalShort float64           `json:"total_short_usd"` // shorts liquidated
	Window    string             `json:"window"`          // "5m"
}

type Feed struct {
	mu      sync.RWMutex
	events  map[string][]LiquidationEvent // symbol -> recent events
	symbols []string
	conn    *websocket.Conn
}

func NewFeed(symbols []string) *Feed {
	return &Feed{
		events:  make(map[string][]LiquidationEvent),
		symbols: symbols,
	}
}

// Start connects to Binance liquidation WebSocket
func (f *Feed) Start(ctx context.Context) {
	log.Printf("[liquidations] starting feed for %d symbols: %v", len(f.symbols), f.symbols)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := f.connect(ctx); err != nil {
				log.Printf("[liquidations] websocket error: %v — reconnecting in 5s", err)
				time.Sleep(5 * time.Second)
			}
		}
	}
}

func (f *Feed) connect(ctx context.Context) error {
	// Build combined stream URL for all symbols
	// Format: btcusdt@forceOrder/ethusdt@forceOrder/...
	streams := ""
	for i, sym := range f.symbols {
		if i > 0 {
			streams += "/"
		}
		streams += fmt.Sprintf("%susdt@forceOrder", strings.ToLower(sym))
	}

	url := fmt.Sprintf("wss://fstream.binance.com/stream?streams=%s", streams)

	log.Printf("[liquidations] attempting WebSocket connection to Binance...")
	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		log.Printf("[liquidations] connection failed: %v (response: %v)", err, resp)
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	f.conn = conn

	log.Printf("[liquidations] connected successfully — %d symbols", len(f.symbols))

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return fmt.Errorf("read: %w", err)
			}
			f.handleMessage(msg)
		}
	}
}

type binanceForceOrderMsg struct {
	Stream string `json:"stream"`
	Data   struct {
		EventType string `json:"e"`
		Order     struct {
			Symbol    string  `json:"s"`
			Side      string  `json:"S"` // BUY or SELL
			Price     float64 `json:"ap,string"` // average price
			Quantity  float64 `json:"q,string"`  // quantity
			TradeTime int64   `json:"T"`
		} `json:"o"`
	} `json:"data"`
}

func (f *Feed) handleMessage(msg []byte) {
	var m binanceForceOrderMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}

	order := m.Data.Order
	if order.Symbol == "" {
		return
	}

	// Convert symbol: BTCUSDT -> BTC
	sym := symbolFromBinance(order.Symbol)
	price := order.Price
	qty := order.Quantity
	sizeUSD := price * qty

	// Skip tiny liquidations < $1k
	if sizeUSD < 1000 {
		return
	}

	// BUY side = short was liquidated (forced to buy back)
	// SELL side = long was liquidated (forced to sell)
	side := "short" // short liquidated
	if order.Side == "SELL" {
		side = "long" // long liquidated
	}

	event := LiquidationEvent{
		Symbol:    sym,
		Side:      side,
		Price:     price,
		Quantity:  qty,
		SizeUSD:   sizeUSD,
		Timestamp: time.Unix(order.TradeTime/1000, 0),
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	events := f.events[sym]
	events = append(events, event)

	// Keep only last 5 minutes of events
	cutoff := time.Now().Add(-5 * time.Minute)
	filtered := events[:0]
	for _, e := range events {
		if e.Timestamp.After(cutoff) {
			filtered = append(filtered, e)
		}
	}
	f.events[sym] = filtered
}

// GetRecent returns liquidations for a symbol in the last 5 minutes
func (f *Feed) GetRecent(symbol string) RecentLiquidations {
	f.mu.RLock()
	defer f.mu.RUnlock()

	events := f.events[symbol]
	var totalLong, totalShort float64

	for _, e := range events {
		if e.Side == "long" {
			totalLong += e.SizeUSD
		} else {
			totalShort += e.SizeUSD
		}
	}

	// Return last 10 events max
	recent := events
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	return RecentLiquidations{
		Events:     recent,
		TotalLong:  math.Round(totalLong),
		TotalShort: math.Round(totalShort),
		Window:     "5m",
	}
}

// GetBurst returns true if >$10M liquidated in last 30 seconds (cascade signal)
func (f *Feed) GetBurst(symbol string) (bool, float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	cutoff := time.Now().Add(-30 * time.Second)
	var total float64
	for _, e := range f.events[symbol] {
		if e.Timestamp.After(cutoff) {
			total += e.SizeUSD
		}
	}
	return total >= 10_000_000, total
}

func symbolFromBinance(s string) string {
	// Remove USDT suffix
	if len(s) > 4 && s[len(s)-4:] == "USDT" {
		return s[:len(s)-4]
	}
	return s
}
