package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"tokomoco/store"

	"github.com/gorilla/mux"
)

// APIHandler serves the API key management REST endpoints.
type APIHandler struct {
	db         *store.DB
	middleware *Middleware
}

// NewAPIHandler creates a new API key management handler.
func NewAPIHandler(db *store.DB, mw *Middleware) *APIHandler {
	return &APIHandler{db: db, middleware: mw}
}

// RegisterRoutes registers all API key management routes on the given router.
// The authWrap function wraps each handler with API key auth middleware.
// auth-status is always public (needed for dashboard bootstrap before any key exists).
func (h *APIHandler) RegisterRoutes(r *mux.Router, authWrap func(http.HandlerFunc) http.Handler) {
	r.HandleFunc("/api/keys/auth-status", h.HandleAuthStatus).Methods("GET") // always public — must be before {id}
	r.Handle("/api/keys", authWrap(h.HandleList)).Methods("GET")
	r.Handle("/api/keys", authWrap(h.HandleCreate)).Methods("POST")
	r.Handle("/api/keys/{id}", authWrap(h.HandleDelete)).Methods("DELETE")
	r.Handle("/api/keys/{id}/toggle", authWrap(h.HandleToggle)).Methods("POST")
}

// HandleList returns all API keys (without hashes).
// GET /api/keys
func (h *APIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.db.ListAPIKeys()
	if err != nil {
		log.Printf("[AUTH-API] list keys: %v", err)
		http.Error(w, "failed to list keys", http.StatusInternalServerError)
		return
	}

	// Map to JSON-safe response (never expose key_hash)
	type keyResponse struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Prefix    string `json:"prefix"`
		Scopes    string `json:"scopes"`
		Enabled   bool   `json:"enabled"`
		CreatedAt int64  `json:"created_at"`
		LastUsed  int64  `json:"last_used"`
		ExpiresAt int64  `json:"expires_at"`
	}

	result := make([]keyResponse, len(keys))
	for i, k := range keys {
		result[i] = keyResponse{
			ID:        k.ID,
			Name:      k.Name,
			Prefix:    k.Prefix,
			Scopes:    k.Scopes,
			Enabled:   k.Enabled,
			CreatedAt: k.CreatedAt.Unix(),
			LastUsed:  k.LastUsed.Unix(),
			ExpiresAt: k.ExpiresAt.Unix(),
		}
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleCreate generates a new API key.
// POST /api/keys  { "name": "My Key", "scopes": "proxy,dashboard", "expires_in_days": 90 }
func (h *APIHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Scopes        string `json:"scopes"`
		ExpiresInDays int    `json:"expires_in_days"` // 0 = no expiry
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if req.Scopes == "" {
		req.Scopes = "proxy,dashboard" // default: full access
	}

	// Generate key
	rawKey, hash, err := GenerateKey()
	if err != nil {
		log.Printf("[AUTH-API] key generation failed: %v", err)
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}

	// Key prefix: first 8 chars after "tc_" for identification in the UI
	prefix := rawKey[:11] + "..." // "tc_" + 8 hex chars + "..."

	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &t
	}

	id, err := h.db.InsertAPIKey(req.Name, hash, prefix, req.Scopes, expiresAt)
	if err != nil {
		log.Printf("[AUTH-API] insert key: %v", err)
		http.Error(w, "failed to create key", http.StatusInternalServerError)
		return
	}

	// Invalidate middleware cache so new key is immediately usable
	h.middleware.InvalidateCache()

	log.Printf("[AUTH] new API key created: name=%q id=%d scopes=%s", req.Name, id, req.Scopes)

	// Return the raw key — this is the ONLY time it's visible
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":      id,
		"name":    req.Name,
		"key":     rawKey,
		"prefix":  prefix,
		"scopes":  req.Scopes,
		"message": "Save this key — it cannot be retrieved again.",
	})
}

// HandleDelete revokes an API key.
// DELETE /api/keys/{id}
func (h *APIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid key ID", http.StatusBadRequest)
		return
	}

	if err := h.db.DeleteAPIKey(id); err != nil {
		log.Printf("[AUTH-API] delete key %d: %v", id, err)
		http.Error(w, "failed to delete key", http.StatusInternalServerError)
		return
	}

	// Invalidate cache so revoked key is rejected immediately
	h.middleware.InvalidateCache()

	log.Printf("[AUTH] API key deleted: id=%d", id)
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// HandleToggle enables or disables an API key.
// POST /api/keys/{id}/toggle  { "enabled": true }
func (h *APIHandler) HandleToggle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid key ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if err := h.db.ToggleAPIKey(id, req.Enabled); err != nil {
		log.Printf("[AUTH-API] toggle key %d: %v", id, err)
		http.Error(w, "failed to toggle key", http.StatusInternalServerError)
		return
	}

	// Invalidate cache so change takes effect immediately
	h.middleware.InvalidateCache()

	log.Printf("[AUTH] API key %d toggled: enabled=%v", id, req.Enabled)
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "toggled", "enabled": req.Enabled})
}

// HandleAuthStatus returns the current auth configuration.
// GET /api/keys/auth-status
func (h *APIHandler) HandleAuthStatus(w http.ResponseWriter, r *http.Request) {
	count, err := h.db.CountAPIKeys()
	if err != nil {
		count = 0
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"auth_enabled": h.middleware.IsEnabled(),
		"key_count":    count,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseID(r *http.Request) (int64, error) {
	vars := mux.Vars(r)
	return strconv.ParseInt(vars["id"], 10, 64)
}

func respondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
