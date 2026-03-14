package handlers

import (
	"context"
	"log"
	"sync"
	"time"

	"derivs-backend/internal/models"
	"derivs-backend/internal/signals"
	"golang.org/x/net/websocket"
)

type subscribeMsg struct {
	Symbol string `json:"symbol"`
}

type Hub struct {
	handler *Handler
	clients map[*websocket.Conn]string // conn -> subscribed symbol
	mu      sync.RWMutex
}

func NewHub(h *Handler) *Hub {
	return &Hub{
		handler: h,
		clients: make(map[*websocket.Conn]string),
	}
}

// getOrFetch checks the cache and falls back to a live fetch + analysis if
// the entry is missing or expired. Uses symbol for fetch/cache; snapshot and
// analysis are built from that fetch — no cross-symbol reuse.
func (hub *Hub) getOrFetch(ctx context.Context, symbol string) (models.SnapshotWithAnalysis, error) {
	if cached, ok := hub.handler.cache.Get(symbol); ok {
		if cached.Snapshot.Symbol != symbol {
			log.Printf("ws: cache symbol mismatch: requested %s, got %s", symbol, cached.Snapshot.Symbol)
		}
		return cached, nil
	}

	snap, err := hub.handler.aggregator.FetchSnapshot(ctx, symbol)
	if err != nil {
		return models.SnapshotWithAnalysis{}, err
	}
	if snap.Symbol != symbol {
		log.Printf("ws: snapshot symbol mismatch: requested %s, got %s", symbol, snap.Symbol)
	}
	hub.handler.cache.RecordPrice(symbol, snap.LiquidationMap.CurrentPrice)

	engine := signals.New()
	momentum := hub.handler.cache.GetPriceMomentum(symbol)
	sigs := engine.Analyze(snap, momentum)

	ai, _ := hub.handler.analyzer.Analyze(ctx, snap, sigs, "free", "", "") // WebSocket has no user context; use free tier

	result := models.SnapshotWithAnalysis{
		Snapshot:  snap,
		Analysis:  ai,
		Alerts:    hub.handler.detector.Analyze(snap, sigs),
		FearGreed: hub.handler.calc.Calculate(snap),
		Signals:   sigs,
	}
	hub.handler.cache.Set(symbol, result)
	return result, nil
}

// ServeWS handles the WebSocket upgrade at GET /ws.
// Expected first client message: {"symbol":"BTC"}
// After that the hub pushes a fresh SnapshotWithAnalysis every 30 seconds.
func (hub *Hub) ServeWS(ws *websocket.Conn) {
	var msg subscribeMsg
	if err := websocket.JSON.Receive(ws, &msg); err != nil {
		log.Printf("ws: read subscribe message: %v", err)
		return
	}
	if msg.Symbol == "" {
		log.Printf("ws: client sent empty symbol, closing")
		return
	}

	hub.mu.Lock()
	hub.clients[ws] = msg.Symbol
	hub.mu.Unlock()

	defer func() {
		hub.mu.Lock()
		delete(hub.clients, ws)
		hub.mu.Unlock()
		log.Printf("ws: client disconnected (symbol: %s)", msg.Symbol)
	}()

	log.Printf("ws: client subscribed to %s", msg.Symbol)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Push immediately on connect so the client doesn't wait 30 seconds.
	if data, err := hub.getOrFetch(ctx, msg.Symbol); err == nil {
		if err := websocket.JSON.Send(ws, data); err != nil {
			return
		}
	} else {
		log.Printf("ws: initial fetch for %s: %v", msg.Symbol, err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		data, err := hub.getOrFetch(ctx, msg.Symbol)
		if err != nil {
			log.Printf("ws: getOrFetch(%s): %v", msg.Symbol, err)
			continue
		}
		if err := websocket.JSON.Send(ws, data); err != nil {
			// Send failure means the client disconnected.
			return
		}
	}
}
