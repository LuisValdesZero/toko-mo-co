package tracker

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"tokomoco/store"

	"github.com/gorilla/mux"
)

// PricingAPIHandler serves the model pricing REST endpoints.
type PricingAPIHandler struct {
	store *PricingStore
	db    *store.DB
}

// NewPricingAPIHandler creates a new pricing API handler.
func NewPricingAPIHandler(ps *PricingStore, db *store.DB) *PricingAPIHandler {
	return &PricingAPIHandler{store: ps, db: db}
}

// RegisterRoutes registers all pricing management routes on the given router.
// The authWrap function wraps each handler with API key auth middleware.
// Static paths MUST be registered before {id} to avoid mux conflicts.
func (h *PricingAPIHandler) RegisterRoutes(r *mux.Router, authWrap func(http.HandlerFunc) http.Handler) {
	r.Handle("/api/pricing", authWrap(h.HandleList)).Methods("GET")
	r.Handle("/api/pricing", authWrap(h.HandleCreate)).Methods("POST")
	r.Handle("/api/pricing/unknown-models", authWrap(h.HandleUnknownModels)).Methods("GET")
	r.Handle("/api/pricing/reset-defaults", authWrap(h.HandleResetDefaults)).Methods("POST")
	r.Handle("/api/pricing/refresh-openrouter", authWrap(h.HandleRefreshOpenRouter)).Methods("POST")
	r.Handle("/api/pricing/stale-check", authWrap(h.HandleStaleCheck)).Methods("GET")
	r.Handle("/api/pricing/{id:[0-9]+}", authWrap(h.HandleGet)).Methods("GET")
	r.Handle("/api/pricing/{id:[0-9]+}", authWrap(h.HandleUpdate)).Methods("PUT")
	r.Handle("/api/pricing/{id:[0-9]+}", authWrap(h.HandleDelete)).Methods("DELETE")
}

// ── JSON response types ─────────────────────────────────────────────────────

type pricingResponse struct {
	ID               int64   `json:"id"`
	ModelPrefix      string  `json:"model_prefix"`
	InputPer1M       float64 `json:"input_per_1m"`
	CachedInputPer1M float64 `json:"cached_input_per_1m"`
	OutputPer1M      float64 `json:"output_per_1m"`
	Provider         string  `json:"provider"`
	Source           string  `json:"source"`
	UpdatedAt        int64   `json:"updated_at"`
}

func toPricingResponse(r store.ModelPricingRow) pricingResponse {
	return pricingResponse{
		ID:               r.ID,
		ModelPrefix:      r.ModelPrefix,
		InputPer1M:       r.InputPer1M,
		CachedInputPer1M: r.CachedInputPer1M,
		OutputPer1M:      r.OutputPer1M,
		Provider:         r.Provider,
		Source:           r.Source,
		UpdatedAt:        r.UpdatedAt.Unix(),
	}
}

// ── Handlers ────────────────────────────────────────────────────────────────

// HandleList returns all model pricing entries.
// GET /api/pricing
func (h *PricingAPIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.ListModelPricing()
	if err != nil {
		log.Printf("[PRICING-API] list: %v", err)
		http.Error(w, "failed to list pricing", http.StatusInternalServerError)
		return
	}

	result := make([]pricingResponse, len(rows))
	for i, row := range rows {
		result[i] = toPricingResponse(row)
	}
	pricingRespondJSON(w, http.StatusOK, result)
}

// HandleGet returns a single pricing entry by ID.
// GET /api/pricing/{id}
func (h *PricingAPIHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id, err := pricingParseID(r)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	row, err := h.db.GetModelPricingByID(id)
	if err != nil {
		http.Error(w, "pricing entry not found", http.StatusNotFound)
		return
	}
	pricingRespondJSON(w, http.StatusOK, toPricingResponse(row))
}

// HandleCreate adds a new pricing entry.
// POST /api/pricing  { "model_prefix": "...", "input_per_1m": ..., ... }
func (h *PricingAPIHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelPrefix      string  `json:"model_prefix"`
		InputPer1M       float64 `json:"input_per_1m"`
		CachedInputPer1M float64 `json:"cached_input_per_1m"`
		OutputPer1M      float64 `json:"output_per_1m"`
		Provider         string  `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.ModelPrefix == "" {
		http.Error(w, `{"error":"model_prefix is required"}`, http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		req.Provider = detectProvider(req.ModelPrefix)
	}

	row := store.ModelPricingRow{
		ModelPrefix:      req.ModelPrefix,
		InputPer1M:       req.InputPer1M,
		CachedInputPer1M: req.CachedInputPer1M,
		OutputPer1M:      req.OutputPer1M,
		Provider:         req.Provider,
		Source:           "custom",
		UpdatedAt:        time.Now(),
	}

	id, err := h.db.InsertModelPricing(row)
	if err != nil {
		log.Printf("[PRICING-API] create: %v", err)
		http.Error(w, `{"error":"failed to create pricing (model prefix may already exist)"}`, http.StatusConflict)
		return
	}

	// Reload cache and clear from unknown models
	h.store.ReloadCache()
	h.store.ClearUnknownModel(req.ModelPrefix)

	log.Printf("[PRICING] created: %q (id=%d, provider=%s)", req.ModelPrefix, id, req.Provider)
	row.ID = id
	pricingRespondJSON(w, http.StatusCreated, toPricingResponse(row))
}

// HandleUpdate modifies an existing pricing entry.
// PUT /api/pricing/{id}
func (h *PricingAPIHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pricingParseID(r)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	var req struct {
		ModelPrefix      string  `json:"model_prefix"`
		InputPer1M       float64 `json:"input_per_1m"`
		CachedInputPer1M float64 `json:"cached_input_per_1m"`
		OutputPer1M      float64 `json:"output_per_1m"`
		Provider         string  `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.ModelPrefix == "" {
		http.Error(w, `{"error":"model_prefix is required"}`, http.StatusBadRequest)
		return
	}

	row := store.ModelPricingRow{
		ID:               id,
		ModelPrefix:      req.ModelPrefix,
		InputPer1M:       req.InputPer1M,
		CachedInputPer1M: req.CachedInputPer1M,
		OutputPer1M:      req.OutputPer1M,
		Provider:         req.Provider,
		Source:           "custom",
		UpdatedAt:        time.Now(),
	}

	if err := h.db.UpdateModelPricing(row); err != nil {
		log.Printf("[PRICING-API] update %d: %v", id, err)
		http.Error(w, "failed to update pricing", http.StatusInternalServerError)
		return
	}

	h.store.ReloadCache()
	log.Printf("[PRICING] updated: %q (id=%d)", req.ModelPrefix, id)
	pricingRespondJSON(w, http.StatusOK, toPricingResponse(row))
}

// HandleDelete removes a pricing entry.
// DELETE /api/pricing/{id}
func (h *PricingAPIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pricingParseID(r)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	if err := h.db.DeleteModelPricing(id); err != nil {
		log.Printf("[PRICING-API] delete %d: %v", id, err)
		http.Error(w, "failed to delete pricing", http.StatusInternalServerError)
		return
	}

	h.store.ReloadCache()
	log.Printf("[PRICING] deleted: id=%d", id)
	pricingRespondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// HandleResetDefaults deletes all entries and re-seeds from hardcoded defaults.
// POST /api/pricing/reset-defaults
func (h *PricingAPIHandler) HandleResetDefaults(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ResetDefaults(); err != nil {
		log.Printf("[PRICING-API] reset defaults: %v", err)
		http.Error(w, "failed to reset defaults", http.StatusInternalServerError)
		return
	}

	count := h.store.CacheSize()
	log.Printf("[PRICING] reset to defaults (%d entries)", count)
	pricingRespondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "reset",
		"count":  count,
	})
}

// HandleRefreshOpenRouter pulls current model pricing from OpenRouter (using the
// server's OPENROUTER_API_KEY) and upserts it under the openrouter/<id> prefix.
// POST /api/pricing/refresh-openrouter
func (h *PricingAPIHandler) HandleRefreshOpenRouter(w http.ResponseWriter, r *http.Request) {
	n, err := RefreshFromOpenRouter(h.db, h.store)
	if err != nil {
		log.Printf("[PRICING-API] refresh-openrouter: %v", err)
		pricingRespondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("[PRICING] refreshed %d models from OpenRouter", n)
	pricingRespondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "refreshed",
		"updated": n,
	})
}

// HandleUnknownModels returns models that were seen but have no pricing.
// GET /api/pricing/unknown-models
func (h *PricingAPIHandler) HandleUnknownModels(w http.ResponseWriter, r *http.Request) {
	unknowns := h.store.UnknownModels()
	pricingRespondJSON(w, http.StatusOK, unknowns)
}

// HandleStaleCheck returns whether pricing data is stale (>90 days since oldest update).
// GET /api/pricing/stale-check
func (h *PricingAPIHandler) HandleStaleCheck(w http.ResponseWriter, r *http.Request) {
	oldest, err := h.db.OldestModelPricingUpdate()
	if err != nil || oldest.IsZero() {
		pricingRespondJSON(w, http.StatusOK, map[string]interface{}{
			"stale":    false,
			"days_old": 0,
			"message":  "no pricing data",
		})
		return
	}

	daysOld := int(time.Since(oldest).Hours() / 24)
	stale := daysOld > 90

	pricingRespondJSON(w, http.StatusOK, map[string]interface{}{
		"stale":    stale,
		"days_old": daysOld,
		"oldest":   oldest.Unix(),
	})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func pricingParseID(r *http.Request) (int64, error) {
	vars := mux.Vars(r)
	return strconv.ParseInt(vars["id"], 10, 64)
}

func pricingRespondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
