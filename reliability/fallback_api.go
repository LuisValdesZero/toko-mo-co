package reliability

import (
	"encoding/json"
	"net/http"
	"strconv"

	"tokomoco/store"

	"github.com/gorilla/mux"
)

// FallbackAPI provides HTTP handlers for fallback configuration management
type FallbackAPI struct {
	store *FallbackStore
	db    *store.DB // for hit count and request log queries on the requests table
}

// NewFallbackAPI creates a new FallbackAPI
func NewFallbackAPI(s *FallbackStore, db *store.DB) *FallbackAPI {
	return &FallbackAPI{store: s, db: db}
}

// HandleList returns all fallback configurations
// Supports optional ?agent_id= query parameter to filter by agent
func (api *FallbackAPI) HandleList(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")

	configs, err := api.store.List(agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(configs)
}

// HandleGet returns a specific fallback configuration
func (api *FallbackAPI) HandleGet(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	config, err := api.store.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// HandleGetForModel returns the fallback configuration for a specific provider/model
func (api *FallbackAPI) HandleGetForModel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	provider := vars["provider"]
	model := vars["model"]

	config, err := api.store.GetForModel(provider, model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// HandleCreate creates a new fallback configuration
func (api *FallbackAPI) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var config FallbackConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := api.store.Create(&config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	config.ID = id
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(config)
}

// HandleUpdate updates an existing fallback configuration
func (api *FallbackAPI) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	var config FallbackConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	config.ID = id
	if err := api.store.Update(&config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// HandleDelete deletes a fallback configuration
func (api *FallbackAPI) HandleDelete(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	if err := api.store.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleToggle enables or disables a fallback configuration
func (api *FallbackAPI) HandleToggle(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := api.store.Toggle(id, body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleHitCounts returns fallback trigger counts per config.
// GET /api/fallback-configs/hit-counts?days=30
func (api *FallbackAPI) HandleHitCounts(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	days := 30 // default: last 30 days
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 0 {
			days = d
		}
	}

	counts, err := api.db.FallbackConfigHitCounts(days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build a map for O(1) lookup on the frontend: configID -> {count, last_triggered}
	result := make(map[string]interface{})
	for _, c := range counts {
		result[strconv.FormatInt(c.FallbackConfigID, 10)] = map[string]interface{}{
			"count":          c.Count,
			"last_triggered": c.LastTriggered.Unix(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleRequestLog returns paginated requests that triggered a specific fallback config.
// GET /api/fallback-configs/{id}/requests?limit=20&offset=0
func (api *FallbackAPI) HandleRequestLog(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, total, err := api.db.FallbackConfigRequests(id, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type requestEntry struct {
		ID               int64   `json:"id"`
		Timestamp        int64   `json:"timestamp"`
		AgentID          string  `json:"agent_id"`
		AppName          string  `json:"app_name"`
		Provider         string  `json:"provider"`
		Model            string  `json:"model"`
		OriginalProvider string  `json:"original_provider"`
		OriginalModel    string  `json:"original_model"`
		PromptPreview    string  `json:"prompt_preview"`
		InputTokens      int     `json:"input_tokens"`
		OutputTokens     int     `json:"output_tokens"`
		Cost             float64 `json:"cost"`
		LatencyMs        int64   `json:"latency_ms"`
		StatusCode       int     `json:"status_code"`
		ErrorMessage     string  `json:"error_message"`
	}

	entries := make([]requestEntry, len(rows))
	for i, row := range rows {
		entries[i] = requestEntry{
			ID:               row.ID,
			Timestamp:        row.Timestamp.Unix(),
			AgentID:          row.AgentID,
			AppName:          row.AppName,
			Provider:         row.Provider,
			Model:            row.Model,
			OriginalProvider: row.OriginalProvider,
			OriginalModel:    row.OriginalModel,
			PromptPreview:    row.PromptPreview,
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			Cost:             row.Cost,
			LatencyMs:        row.LatencyMs,
			StatusCode:       row.StatusCode,
			ErrorMessage:     row.ErrorMessage,
		}
	}

	response := map[string]interface{}{
		"requests": entries,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
