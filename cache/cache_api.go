package cache

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
)

// CombinedCacheStats includes both exact-match and semantic cache stats.
type CombinedCacheStats struct {
	Exact    CacheStats          `json:"exact"`
	Semantic *SemanticCacheStats `json:"semantic,omitempty"`
}

// APIHandler provides REST endpoints for cache management.
type APIHandler struct {
	cache         *ResponseCache
	semanticCache *SemanticCache
	scMu          sync.RWMutex
}

// NewAPIHandler creates a cache API handler.
func NewAPIHandler(cache *ResponseCache, semanticCache *SemanticCache) *APIHandler {
	return &APIHandler{cache: cache, semanticCache: semanticCache}
}

// SetSemanticCache hot-swaps the semantic cache (thread-safe).
func (h *APIHandler) SetSemanticCache(sc *SemanticCache) {
	h.scMu.Lock()
	h.semanticCache = sc
	h.scMu.Unlock()
}

// getSemanticCache returns the current semantic cache (thread-safe).
func (h *APIHandler) getSemanticCache() *SemanticCache {
	h.scMu.RLock()
	sc := h.semanticCache
	h.scMu.RUnlock()
	return sc
}

// HandleStats returns cache statistics as JSON.
// GET /api/cache
func (h *APIHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	combined := CombinedCacheStats{
		Exact: h.cache.Stats(),
	}
	sc := h.getSemanticCache()
	if sc != nil {
		semStats := sc.Stats()
		combined.Semantic = &semStats
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(combined)
}

// HandleFlush clears all cached entries (both exact-match and semantic).
// POST /api/cache/flush
func (h *APIHandler) HandleFlush(w http.ResponseWriter, r *http.Request) {
	n, err := h.cache.Flush()
	if err != nil {
		log.Printf("[CACHE] flush error: %v", err)
		http.Error(w, "cache flush failed", http.StatusInternalServerError)
		return
	}
	// Also flush semantic cache
	sc := h.getSemanticCache()
	if sc != nil {
		sc.Flush()
	}
	log.Printf("[CACHE] flushed %d entries (exact + semantic)", n)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"flushed":%d}`, n)
}
