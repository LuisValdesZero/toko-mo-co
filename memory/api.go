package memory

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// APIHandler provides REST endpoints for memory management.
type APIHandler struct {
	store *Store
}

// NewAPIHandler creates a memory API handler.
func NewAPIHandler(store *Store) *APIHandler {
	return &APIHandler{store: store}
}

// HandleStats returns memory layer statistics as JSON.
// GET /api/memories
func (h *APIHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.store.GetStats())
}

// HandleList returns memories for a given agent (or all agents).
// GET /api/memories/list?agent_id=xxx&limit=50
func (h *APIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	entries := h.store.ListByAgent(agentID, limit)

	type memoryItem struct {
		ID           int64  `json:"id"`
		AgentID      string `json:"agent_id"`
		SessionID    string `json:"session_id"`
		Fact         string `json:"fact"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		CreatedAt    int64  `json:"created_at"`
		LastAccessed int64  `json:"last_accessed"`
		AccessCount  int64  `json:"access_count"`
		UpdatedAt    int64  `json:"updated_at"`
	}

	items := make([]memoryItem, len(entries))
	for i, e := range entries {
		items[i] = memoryItem{
			ID:           e.ID,
			AgentID:      e.AgentID,
			SessionID:    e.SessionID,
			Fact:         e.Fact,
			Provider:     e.Provider,
			Model:        e.Model,
			CreatedAt:    e.CreatedAt.Unix(),
			LastAccessed: e.LastAccessed.Unix(),
			AccessCount:  e.AccessCount,
			UpdatedAt:    e.UpdatedAt.Unix(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"memories": items,
		"total":    len(items),
	})
}

// HandleSearch searches for relevant memories.
// POST /api/memories/search
// Body: {"query": "text to search", "agent_id": "optional", "limit": 5}
func (h *APIHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query   string `json:"query"`
		AgentID string `json:"agent_id"`
		Limit   int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}

	results, err := h.store.Search(req.Query, req.AgentID, req.Limit)
	if err != nil {
		log.Printf("[MEMORY-API] search error: %v", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	type resultItem struct {
		ID          int64   `json:"id"`
		AgentID     string  `json:"agent_id"`
		Fact        string  `json:"fact"`
		Similarity  float64 `json:"similarity"`
		Score       float64 `json:"score"`
		AccessCount int64   `json:"access_count"`
		CreatedAt   int64   `json:"created_at"`
	}

	items := make([]resultItem, len(results))
	for i, r := range results {
		items[i] = resultItem{
			ID:          r.Entry.ID,
			AgentID:     r.Entry.AgentID,
			Fact:        r.Entry.Fact,
			Similarity:  r.Similarity,
			Score:       r.Score,
			AccessCount: r.Entry.AccessCount,
			CreatedAt:   r.Entry.CreatedAt.Unix(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results": items,
		"total":   len(items),
	})
}

// HandleAdd manually adds a memory fact.
// POST /api/memories
// Body: {"agent_id": "xxx", "fact": "the user prefers Go", "provider": "openai", "model": "gpt-4o"}
func (h *APIHandler) HandleAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
		Fact      string `json:"fact"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Fact == "" {
		http.Error(w, "fact is required", http.StatusBadRequest)
		return
	}

	if err := h.store.StoreFact(req.AgentID, req.SessionID, req.Fact, req.Provider, req.Model); err != nil {
		log.Printf("[MEMORY-API] store error: %v", err)
		http.Error(w, "failed to store memory", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "stored",
		"message": "Memory fact stored successfully",
	})
}

// HandleDelete removes a specific memory by ID.
// DELETE /api/memories/{id}
func (h *APIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid memory ID", http.StatusBadRequest)
		return
	}

	if err := h.store.Delete(id); err != nil {
		log.Printf("[MEMORY-API] delete error: %v", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "deleted",
		"message": "Memory removed",
	})
}

// HandleDeleteByAgent removes all memories for an agent.
// DELETE /api/memories/agent/{agent_id}
func (h *APIHandler) HandleDeleteByAgent(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	agentID := vars["agent_id"]
	if agentID == "" {
		http.Error(w, "agent_id is required", http.StatusBadRequest)
		return
	}

	removed, err := h.store.DeleteByAgent(agentID)
	if err != nil {
		log.Printf("[MEMORY-API] delete by agent error: %v", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "deleted",
		"removed": removed,
		"message": "All memories for agent removed",
	})
}

// HandleFlush removes all memories.
// POST /api/memories/flush
func (h *APIHandler) HandleFlush(w http.ResponseWriter, r *http.Request) {
	h.store.Flush()
	log.Printf("[MEMORY] flushed all memories")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "flushed",
		"message": "All memories removed",
	})
}

// HandleAgentStats returns per-agent memory breakdown.
// GET /api/memories/agents
func (h *APIHandler) HandleAgentStats(w http.ResponseWriter, r *http.Request) {
	stats := h.store.GetStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agents":       stats.AgentBreakdown,
		"total_agents": len(stats.AgentBreakdown),
	})
}

// HandleTopMemories returns the most-accessed memories.
// GET /api/memories/top
func (h *APIHandler) HandleTopMemories(w http.ResponseWriter, r *http.Request) {
	stats := h.store.GetStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"memories": stats.TopMemories,
		"total":    len(stats.TopMemories),
	})
}

// RegisterRoutes registers all memory API routes on the given router.
func (h *APIHandler) RegisterRoutes(r *mux.Router, authWrap func(http.HandlerFunc) http.Handler) {
	r.Handle("/api/memories", authWrap(h.HandleStats)).Methods("GET")
	r.Handle("/api/memories", authWrap(h.HandleAdd)).Methods("POST")
	r.Handle("/api/memories/list", authWrap(h.HandleList)).Methods("GET")
	r.Handle("/api/memories/search", authWrap(h.HandleSearch)).Methods("POST")
	r.Handle("/api/memories/flush", authWrap(h.HandleFlush)).Methods("POST")
	r.Handle("/api/memories/agents", authWrap(h.HandleAgentStats)).Methods("GET")
	r.Handle("/api/memories/top", authWrap(h.HandleTopMemories)).Methods("GET")
	r.Handle("/api/memories/{id}", authWrap(h.HandleDelete)).Methods("DELETE")
	r.Handle("/api/memories/agent/{agent_id}", authWrap(h.HandleDeleteByAgent)).Methods("DELETE")
}
