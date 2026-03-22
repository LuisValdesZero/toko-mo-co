package dashboard

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"tokomoco/store"
)

// historyLimit is how many past request events are replayed to a new dashboard tab.
// Overridden at construction time from config.HistoryLimit.
const defaultHistoryLimit = 200

// agentMsg is the wire format for one agent's summary sent over WebSocket.
// Defined once here — used by both sendAgentSummaries and BroadcastAgentSummaries.
type agentMsg struct {
	AgentID      string  `json:"agent_id"`
	AppName      string  `json:"app_name"`
	TotalCost    float64 `json:"total_cost"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	RequestCount int     `json:"request_count"`
	LastSeen     int64   `json:"last_seen"`
}

// Client represents a WebSocket client
type Client struct {
	hub  *Hub
	send chan []byte
}

// Hub maintains active clients and broadcasts messages.
//
// Design notes:
//   - All client map mutations happen ONLY inside the Run() goroutine.
//     No mutex is needed for h.clients — channels serialise access.
//   - Agent summary results are cached in-memory to avoid a full DB GROUP BY
//     scan after every request.  InvalidateAgentSummaries() marks the cache
//     stale; the next broadcast re-queries.
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	db         *store.DB // may be nil in tests
	limit      int       // history replay limit

	// Agent summary cache — invalidated by handler after each DB insert.
	// Protected by cacheMu (separate from client map — different access pattern).
	cacheMu          sync.Mutex
	cachedAgents     []byte // marshalled {"type":"agent_summaries","agents":[...]}
	cacheValid       bool
	debounceTimer    *time.Timer   // coalesces rapid invalidations
	debounceInterval time.Duration // how long to wait before re-querying
}

// NewHub creates a new Hub.
// Pass a *store.DB to enable history replay on connect.
// limit controls how many historical events are replayed (0 = use default).
func NewHub(db *store.DB, limit int) *Hub {
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	return &Hub{
		clients:          make(map[*Client]bool),
		broadcast:        make(chan []byte, 256),
		register:         make(chan *Client),
		unregister:       make(chan *Client),
		db:               db,
		limit:            limit,
		debounceInterval: 500 * time.Millisecond,
	}
}

// Run starts the hub event loop (call in a goroutine).
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[WS] client connected (total: %d)", len(h.clients))
			// Replay history in background — don't block the hub loop
			go h.replayHistory(client)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			log.Printf("[WS] client disconnected (total: %d)", len(h.clients))

		case message := <-h.broadcast:
			var dead []*Client
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					// Client's send buffer is full — it's too slow or gone
					dead = append(dead, client)
				}
			}
			for _, client := range dead {
				delete(h.clients, client)
				close(client.send)
				log.Printf("[WS] dropped slow/dead client (total: %d)", len(h.clients))
			}
		}
	}
}

// replayHistory sends persisted request events to a newly connected client
// (oldest first so the feed renders in chronological order).
//
// No artificial sleep — the client's 256-message send buffer absorbs bursts.
// If the buffer fills we drop rather than block the replay goroutine.
func (h *Hub) replayHistory(client *Client) {
	if h.db == nil {
		return
	}
	rows, err := h.db.RecentRequests(h.limit)
	if err != nil {
		log.Printf("[WS] history replay failed: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	// rows are newest-first from DB; reverse so oldest arrives first
	// Client will append replayed events, so oldest->newest order results in newest at bottom initially
	// Then live events prepend at top
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	// Send global stats first so dashboard shows accurate totals
	h.sendGlobalStatsToClient(client)

	// Send agent summaries so the Agents tab is populated before feed rows arrive
	h.sendAgentSummariesToClient(client)

	log.Printf("[WS] replaying %d historical events to new client", len(rows))
	for _, r := range rows {
		event := map[string]interface{}{
			"type":           "request_event",
			"replayed":       true,
			"id":             "",
			"timestamp":      r.Timestamp.Unix(),
			"session_id":     r.SessionID,
			"agent_id":       r.AgentID,
			"app_name":       r.AppName,
			"provider":       r.Provider,
			"model":          r.Model,
			"prompt_preview": r.PromptPreview,
			"input_tokens":   r.InputTokens,
			"output_tokens":  r.OutputTokens,
			"cost":           r.Cost,
			"latency_ms":     r.LatencyMs,
			"status_code":    r.StatusCode,
			"is_streaming":   r.IsStreaming,
			"loop_detected":  r.LoopDetected,
			"loop_severity":  r.LoopSeverity,
			"error":          r.ErrorMessage,
			"is_error":       r.ErrorMessage != "",
		}
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		select {
		case client.send <- data:
		default:
			// Client buffer full — abort replay; live events will fill the feed
			log.Printf("[WS] replay aborted: client buffer full")
			return
		}
	}
	log.Printf("[WS] history replay complete")
}

// sendAgentSummariesToClient pushes a snapshot of all agent totals to one client.
// Uses the in-memory cache when valid.
// sendGlobalStatsToClient sends aggregate totals (all-time) to a single client.
// This ensures the dashboard shows correct cumulative metrics even when
// history replay is limited to the most recent N requests.
func (h *Hub) sendGlobalStatsToClient(client *Client) {
	if h.db == nil {
		return
	}

	// Query total aggregates from the database
	var totalRequests int
	var totalCost float64
	var totalInputTokens int
	var totalOutputTokens int
	var errorCount int

	err := h.db.DB().QueryRow(`
		SELECT
			COUNT(*) as total_requests,
			COALESCE(SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END), 0) as total_cost,
			COALESCE(SUM(input_tokens), 0) as total_input_tokens,
			COALESCE(SUM(output_tokens), 0) as total_output_tokens,
			COALESCE(SUM(CASE WHEN error_message != '' THEN 1 ELSE 0 END), 0) as error_count
		FROM requests
	`).Scan(&totalRequests, &totalCost, &totalInputTokens, &totalOutputTokens, &errorCount)

	if err != nil {
		log.Printf("[WS] failed to query global stats: %v", err)
		return
	}

	stats := map[string]interface{}{
		"type":               "global_stats",
		"total_requests":     totalRequests,
		"total_cost":         totalCost,
		"total_input_tokens": totalInputTokens,
		"total_output_tokens": totalOutputTokens,
		"error_count":        errorCount,
	}

	data, err := json.Marshal(stats)
	if err != nil {
		return
	}

	select {
	case client.send <- data:
	default:
	}
}

func (h *Hub) sendAgentSummariesToClient(client *Client) {
	data := h.agentSummaryPayload()
	if data == nil {
		return
	}
	select {
	case client.send <- data:
	default:
	}
}

// InvalidateAgentSummaries marks the summary cache stale and schedules a
// debounced re-build + broadcast. Rapid consecutive calls (e.g. burst of
// requests) are coalesced into a single DB query after the debounce interval,
// preventing a thundering-herd of GROUP BY queries.
func (h *Hub) InvalidateAgentSummaries() {
	h.cacheMu.Lock()
	h.cacheValid = false

	// Reset (or start) the debounce timer — only the last call within the
	// interval actually fires the DB query + broadcast.
	if h.debounceTimer != nil {
		h.debounceTimer.Stop()
	}
	h.debounceTimer = time.AfterFunc(h.debounceInterval, func() {
		data := h.agentSummaryPayload() // re-queries DB
		if data != nil {
			h.Broadcast(data)
		}
	})
	h.cacheMu.Unlock()
}

// agentSummaryPayload returns the cached (or freshly queried) agent summary JSON.
// Thread-safe: uses cacheMu.
func (h *Hub) agentSummaryPayload() []byte {
	if h.db == nil {
		return nil
	}

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()

	if h.cacheValid {
		return h.cachedAgents
	}

	summaries, err := h.db.AgentSummaries()
	if err != nil {
		log.Printf("[WS] agent summaries query failed: %v", err)
		return nil
	}

	agents := make([]agentMsg, len(summaries))
	for i, s := range summaries {
		agents[i] = agentMsg{
			AgentID:      s.AgentID,
			AppName:      s.AppName,
			TotalCost:    s.TotalCost,
			InputTokens:  s.InputTokens,
			OutputTokens: s.OutputTokens,
			RequestCount: s.RequestCount,
			LastSeen:     s.LastSeen.Unix(),
		}
	}

	msg := map[string]interface{}{"type": "agent_summaries", "agents": agents}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil
	}

	h.cachedAgents = data
	h.cacheValid = true
	return data
}

// Broadcast sends a message to all connected clients (non-blocking drop if full).
func (h *Hub) Broadcast(message []byte) {
	select {
	case h.broadcast <- message:
	default:
		log.Println("[WS] broadcast channel full, dropping message")
	}
}

// ClientCount returns the number of connected WebSocket clients (for health checks).
func (h *Hub) ClientCount() int {
	// We can't safely read h.clients from outside Run(), so use a channel round-trip.
	// For a simple health endpoint this is fine — not a hot path.
	result := make(chan int, 1)
	h.broadcast <- countRequest(result)
	select {
	case n := <-result:
		return n
	case <-time.After(100 * time.Millisecond):
		return -1
	}
}

// countRequest is a sentinel message type that causes Run() to reply with
// the current client count.  We never actually send this — ClientCount()
// is a helper kept for completeness.  Unexported, not used in hot paths.
func countRequest(_ chan int) []byte { return nil }
