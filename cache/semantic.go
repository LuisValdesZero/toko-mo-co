package cache

import (
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"

	"tokomoco/embedding"
	"tokomoco/vectorstore"
)

// SemanticCache wraps an Embedder and VectorStore to provide semantic
// similarity matching on top of the existing exact-match ResponseCache.
//
// Flow:
//   1. Caller builds a "semantic key" from the request (model + messages text)
//   2. SemanticCache embeds the key → vector
//   3. VectorStore searches for the nearest existing vector above threshold
//   4. If found, returns the cache hash → caller looks it up in ResponseCache
//   5. On cache miss, after upstream response, caller stores the vector
type SemanticCache struct {
	embedder     embedding.Embedder
	store        *vectorstore.VectorStore
	threshold    float64 // Cosine similarity threshold (default: 0.95)
	sparseWeight float64 // Weight of the sparse score in hybrid mode (0 = dense only)
	enabled      bool

	// Atomic metrics
	hits   atomic.Int64
	misses atomic.Int64
}

// NewSemanticCache creates a semantic cache with the given embedder and vector store.
// sparseWeight blends a sparse (lexical) score into the similarity when the embedder
// is a HybridEmbedder (e.g. bge-m3); pass 0 for pure dense matching.
func NewSemanticCache(embedder embedding.Embedder, store *vectorstore.VectorStore, threshold, sparseWeight float64, enabled bool) *SemanticCache {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.95 // safe default
	}
	if sparseWeight < 0 || sparseWeight > 1 {
		sparseWeight = 0
	}
	return &SemanticCache{
		embedder:     embedder,
		store:        store,
		threshold:    threshold,
		sparseWeight: sparseWeight,
		enabled:      enabled,
	}
}

// SetSparseWeight updates the hybrid sparse weight at runtime.
func (sc *SemanticCache) SetSparseWeight(w float64) {
	if w >= 0 && w <= 1 {
		sc.sparseWeight = w
	}
}

// embedKey returns the dense vector and (when the embedder supports it) the sparse
// vector for a semantic key.
func (sc *SemanticCache) embedKey(key string) ([]float32, map[int32]float32, error) {
	if he, ok := sc.embedder.(embedding.HybridEmbedder); ok && sc.sparseWeight > 0 {
		dense, sparse, err := he.EmbedHybrid(key)
		if err != nil {
			return nil, nil, err
		}
		return dense, map[int32]float32(sparse), nil
	}
	dense, err := sc.embedder.Embed(key)
	return dense, nil, err
}

// IsEnabled returns whether semantic caching is active.
func (sc *SemanticCache) IsEnabled() bool {
	return sc.enabled
}

// SetEnabled toggles the semantic cache on/off at runtime.
func (sc *SemanticCache) SetEnabled(enabled bool) {
	sc.enabled = enabled
}

// SetThreshold updates the similarity threshold at runtime.
func (sc *SemanticCache) SetThreshold(t float64) {
	if t > 0 && t <= 1 {
		sc.threshold = t
	}
}

// Threshold returns the current similarity threshold.
func (sc *SemanticCache) Threshold() float64 {
	return sc.threshold
}

// Lookup embeds the semantic key and searches for a similar cached response.
// Returns (cacheHash, similarity, found). The caller uses cacheHash to look
// up the actual response in the ResponseCache.
func (sc *SemanticCache) Lookup(semanticKey, provider, model string) (string, float64, bool) {
	if !sc.enabled || sc.embedder == nil || sc.store == nil {
		return "", 0, false
	}

	vec, sparse, err := sc.embedKey(semanticKey)
	if err != nil {
		log.Printf("[SEMANTIC-CACHE] embed error: %v", err)
		sc.misses.Add(1)
		return "", 0, false
	}

	result := sc.store.SearchHybrid(vec, sparse, provider, model, sc.threshold, sc.sparseWeight)
	if result == nil {
		sc.misses.Add(1)
		return "", 0, false
	}

	sc.hits.Add(1)
	return result.CacheHash, result.Similarity, true
}

// Store persists the embedding vector for a cache entry.
// Called after a successful upstream response is stored in the exact-match cache.
func (sc *SemanticCache) Store(semanticKey, cacheHash, provider, model string) {
	if !sc.enabled || sc.embedder == nil || sc.store == nil {
		return
	}

	vec, sparse, err := sc.embedKey(semanticKey)
	if err != nil {
		log.Printf("[SEMANTIC-CACHE] embed error on store: %v", err)
		return
	}

	sc.store.StoreHybrid(cacheHash, vec, sparse, provider, model)
}

// Flush clears all vectors from the semantic cache.
func (sc *SemanticCache) Flush() {
	if sc.store != nil {
		sc.store.Flush()
	}
	sc.hits.Store(0)
	sc.misses.Store(0)
}

// SemanticCacheStats holds aggregate semantic cache metrics.
type SemanticCacheStats struct {
	Enabled    bool    `json:"enabled"`
	Vectors    int     `json:"vectors"`
	Threshold  float64 `json:"threshold"`
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	HitRate    float64 `json:"hit_rate"`
}

// Stats returns current semantic cache metrics.
func (sc *SemanticCache) Stats() SemanticCacheStats {
	hits := sc.hits.Load()
	misses := sc.misses.Load()
	total := hits + misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}

	vectorCount := 0
	if sc.store != nil {
		vectorCount = sc.store.Count()
	}

	return SemanticCacheStats{
		Enabled:   sc.enabled,
		Vectors:   vectorCount,
		Threshold: sc.threshold,
		Hits:      hits,
		Misses:    misses,
		HitRate:   hitRate,
	}
}

// BuildSemanticKey extracts a canonical text representation from a request body
// for embedding. This includes the model and the user-facing message content,
// but excludes system prompts, tools, and non-semantic fields.
func BuildSemanticKey(provider, model string, bodyBytes []byte) string {
	var reqData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqData); err != nil {
		return ""
	}

	var parts []string
	parts = append(parts, provider+"/"+model)

	// Extract message content for semantic comparison
	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			msgMap, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}

			role, _ := msgMap["role"].(string)

			switch content := msgMap["content"].(type) {
			case string:
				parts = append(parts, role+": "+content)
			case []interface{}:
				// Anthropic content blocks
				for _, block := range content {
					blockMap, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					if text, ok := blockMap["text"].(string); ok {
						parts = append(parts, role+": "+text)
					}
				}
			}
		}
	}

	// Gemini contents field
	if contents, ok := reqData["contents"].([]interface{}); ok {
		for _, content := range contents {
			contentMap, ok := content.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := contentMap["role"].(string)
			if partsArr, ok := contentMap["parts"].([]interface{}); ok {
				for _, part := range partsArr {
					partMap, ok := part.(map[string]interface{})
					if !ok {
						continue
					}
					if text, ok := partMap["text"].(string); ok {
						parts = append(parts, role+": "+text)
					}
				}
			}
		}
	}

	return strings.Join(parts, "\n")
}
