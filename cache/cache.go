// Package cache provides a global exact-match response cache for Toko-Mo-Co.
//
// Design:
//   - SHA-256 hash of (provider + model + messages + temperature + system + tools) = cache key
//   - In-memory LRU map with bounded capacity + TTL-based expiration
//   - SQLite persistence for warm-loading across restarts
//   - Atomic hit/miss counters for lock-free stats reads
//   - Non-streaming responses only (streaming is real-time by nature)
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"tokomoco/store"
)

// CacheEntry is a cached LLM response held in memory.
type CacheEntry struct {
	Hash            string
	Provider        string
	Model           string
	StatusCode      int
	ResponseBody    []byte
	ResponseHeaders map[string][]string
	InputTokens     int
	OutputTokens    int
	CostPerHit      float64
	ExpiresAt       time.Time
}

// CacheStats holds aggregate cache metrics.
type CacheStats struct {
	Enabled    bool    `json:"enabled"`
	Entries    int     `json:"entries"`
	MaxEntries int     `json:"max_entries"`
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	HitRate    float64 `json:"hit_rate"`
	TokensSaved int64  `json:"tokens_saved"`
	CostSaved  float64 `json:"cost_saved"`
	TTLMinutes int     `json:"ttl_minutes"`
}

// ResponseCache provides global exact-match caching of LLM responses.
type ResponseCache struct {
	db         *store.DB
	mu         sync.RWMutex
	entries    map[string]*CacheEntry
	lruOrder   *list.List
	lruMap     map[string]*list.Element
	maxEntries int
	defaultTTL time.Duration
	enabled    bool

	// Atomic metrics — lock-free reads from hot path
	hits       atomic.Int64
	misses     atomic.Int64
	costSaved  atomic.Int64 // stored as cost * 1_000_000 for int precision
}

// NewResponseCache creates a response cache, loads hot entries from DB, and starts
// a background cleanup goroutine.
func NewResponseCache(db *store.DB, maxEntries int, defaultTTL time.Duration, enabled bool) *ResponseCache {
	rc := &ResponseCache{
		db:         db,
		entries:    make(map[string]*CacheEntry),
		lruOrder:   list.New(),
		lruMap:     make(map[string]*list.Element),
		maxEntries: maxEntries,
		defaultTTL: defaultTTL,
		enabled:    enabled,
	}

	// Warm-load from DB
	if db != nil {
		rc.warmLoad()
	}

	// Background expired-entry cleanup every 10 minutes
	go rc.cleanupLoop()

	return rc
}

// warmLoad loads the most recently accessed entries from the database into memory.
func (rc *ResponseCache) warmLoad() {
	rows, err := rc.db.LoadHotCacheEntries(rc.maxEntries)
	if err != nil {
		log.Printf("[CACHE] warm-load failed: %v", err)
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	for _, row := range rows {
		var headers map[string][]string
		if row.ResponseHeaders != "" && row.ResponseHeaders != "{}" {
			if err := json.Unmarshal([]byte(row.ResponseHeaders), &headers); err != nil {
				log.Printf("[CACHE] warm-load: invalid headers JSON for hash=%s: %v", row.RequestHash, err)
			}
		}
		entry := &CacheEntry{
			Hash:            row.RequestHash,
			Provider:        row.Provider,
			Model:           row.Model,
			StatusCode:      row.StatusCode,
			ResponseBody:    row.ResponseBody,
			ResponseHeaders: headers,
			InputTokens:     row.InputTokens,
			OutputTokens:    row.OutputTokens,
			CostPerHit:      row.CostPerHit,
			ExpiresAt:       row.ExpiresAt,
		}
		rc.entries[row.RequestHash] = entry
		elem := rc.lruOrder.PushFront(row.RequestHash)
		rc.lruMap[row.RequestHash] = elem
	}

	if len(rows) > 0 {
		log.Printf("[CACHE] warm-loaded %d entries from DB", len(rows))
	}
}

// cleanupLoop periodically removes expired entries from memory and DB.
func (rc *ResponseCache) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rc.evictExpired()
		if rc.db != nil {
			if n, err := rc.db.PruneExpiredCache(); err == nil && n > 0 {
				log.Printf("[CACHE] pruned %d expired DB entries", n)
			}
		}
	}
}

// evictExpired removes all expired entries from the in-memory cache.
func (rc *ResponseCache) evictExpired() {
	now := time.Now()
	rc.mu.Lock()
	defer rc.mu.Unlock()

	for hash, entry := range rc.entries {
		if now.After(entry.ExpiresAt) {
			delete(rc.entries, hash)
			if elem, ok := rc.lruMap[hash]; ok {
				rc.lruOrder.Remove(elem)
				delete(rc.lruMap, hash)
			}
		}
	}
}

// BuildRequestHash creates a deterministic hash of the cache-relevant parts of a request.
// Non-deterministic fields (stream, max_tokens, top_p, top_k) are excluded.
func BuildRequestHash(provider, model string, bodyBytes []byte) string {
	var reqData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqData); err != nil {
		// If we can't parse, hash the raw body
		h := sha256.Sum256(append([]byte(provider+"|"+model+"|"), bodyBytes...))
		return hex.EncodeToString(h[:])
	}

	// Build canonical representation with only fields that affect response content
	canonical := map[string]interface{}{
		"provider": provider,
		"model":    model,
	}

	// Messages (the core prompt — this is the primary differentiator)
	if messages, ok := reqData["messages"]; ok {
		canonical["messages"] = messages
	}
	// Anthropic system prompt (top-level, separate from messages)
	if system, ok := reqData["system"]; ok {
		canonical["system"] = system
	}
	// Temperature (affects output randomness)
	if temp, ok := reqData["temperature"]; ok {
		canonical["temperature"] = temp
	}
	// Tool definitions (affect response structure)
	if tools, ok := reqData["tools"]; ok {
		canonical["tools"] = tools
	}
	// Gemini contents field
	if contents, ok := reqData["contents"]; ok {
		canonical["contents"] = contents
	}

	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		h := sha256.Sum256(bodyBytes)
		return hex.EncodeToString(h[:])
	}

	h := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(h[:])
}

// IsEnabled returns whether the cache is active.
func (rc *ResponseCache) IsEnabled() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.enabled
}

// SetEnabled toggles the cache on/off at runtime.
func (rc *ResponseCache) SetEnabled(enabled bool) {
	rc.mu.Lock()
	rc.enabled = enabled
	rc.mu.Unlock()
}

// DefaultTTL returns the configured TTL for new cache entries.
func (rc *ResponseCache) DefaultTTL() time.Duration {
	return rc.defaultTTL
}

// Lookup checks for a cached response by hash. Returns the entry and true on hit.
// On hit, promotes the entry in LRU and increments the hit counter.
func (rc *ResponseCache) Lookup(hash string) (*CacheEntry, bool) {
	rc.mu.RLock()
	entry, ok := rc.entries[hash]
	rc.mu.RUnlock()

	if !ok {
		rc.misses.Add(1)
		return nil, false
	}

	// Check expiration
	if time.Now().After(entry.ExpiresAt) {
		rc.mu.Lock()
		delete(rc.entries, hash)
		if elem, ok := rc.lruMap[hash]; ok {
			rc.lruOrder.Remove(elem)
			delete(rc.lruMap, hash)
		}
		rc.mu.Unlock()
		rc.misses.Add(1)
		return nil, false
	}

	// Promote in LRU (need write lock for LRU update)
	rc.mu.Lock()
	if elem, ok := rc.lruMap[hash]; ok {
		rc.lruOrder.MoveToFront(elem)
	}
	rc.mu.Unlock()

	// Increment hit counters
	rc.hits.Add(1)
	rc.costSaved.Add(int64(entry.CostPerHit * 1_000_000))

	// Async DB hit increment
	if rc.db != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[CACHE] panic in IncrementCacheHit: %v", r)
				}
			}()
			rc.db.IncrementCacheHit(hash) //nolint:errcheck
		}()
	}

	return entry, true
}

// Store adds or updates a cache entry. Evicts the LRU entry if at capacity.
// Persists to DB asynchronously.
func (rc *ResponseCache) Store(hash string, entry *CacheEntry) {
	rc.mu.Lock()

	// If already exists, update in place and promote
	if existing, ok := rc.entries[hash]; ok {
		existing.ResponseBody = entry.ResponseBody
		existing.ResponseHeaders = entry.ResponseHeaders
		existing.InputTokens = entry.InputTokens
		existing.OutputTokens = entry.OutputTokens
		existing.CostPerHit = entry.CostPerHit
		existing.ExpiresAt = entry.ExpiresAt
		if elem, ok := rc.lruMap[hash]; ok {
			rc.lruOrder.MoveToFront(elem)
		}
		rc.mu.Unlock()
	} else {
		// Evict LRU if at capacity
		if len(rc.entries) >= rc.maxEntries {
			rc.evictLRU()
		}
		rc.entries[hash] = entry
		elem := rc.lruOrder.PushFront(hash)
		rc.lruMap[hash] = elem
		rc.mu.Unlock()
	}

	// Async persist to DB
	if rc.db != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[CACHE] panic in persistToDB: %v", r)
				}
			}()
			rc.persistToDB(entry)
		}()
	}
}

// evictLRU removes the least recently used entry. Must be called under write lock.
func (rc *ResponseCache) evictLRU() {
	back := rc.lruOrder.Back()
	if back == nil {
		return
	}
	hash := back.Value.(string)
	rc.lruOrder.Remove(back)
	delete(rc.lruMap, hash)
	delete(rc.entries, hash)
}

// persistToDB writes a cache entry to SQLite.
func (rc *ResponseCache) persistToDB(entry *CacheEntry) {
	headersJSON, _ := json.Marshal(entry.ResponseHeaders)
	now := time.Now()
	row := store.CacheRow{
		RequestHash:     entry.Hash,
		Provider:        entry.Provider,
		Model:           entry.Model,
		StatusCode:      entry.StatusCode,
		ResponseBody:    entry.ResponseBody,
		ResponseHeaders: string(headersJSON),
		InputTokens:     entry.InputTokens,
		OutputTokens:    entry.OutputTokens,
		CostPerHit:      entry.CostPerHit,
		HitCount:        0,
		CreatedAt:       now,
		LastAccessed:    now,
		ExpiresAt:       entry.ExpiresAt,
	}
	if err := rc.db.InsertOrUpdateCache(row); err != nil {
		log.Printf("[CACHE] DB persist failed: %v", err)
	}
}

// Flush clears all entries from both memory and DB.
func (rc *ResponseCache) Flush() (int64, error) {
	rc.mu.Lock()
	rc.entries = make(map[string]*CacheEntry)
	rc.lruOrder.Init()
	rc.lruMap = make(map[string]*list.Element)
	rc.mu.Unlock()

	rc.hits.Store(0)
	rc.misses.Store(0)
	rc.costSaved.Store(0)

	if rc.db != nil {
		return rc.db.FlushCache()
	}
	return 0, nil
}

// Stats returns current cache metrics.
func (rc *ResponseCache) Stats() CacheStats {
	rc.mu.RLock()
	entryCount := len(rc.entries)
	enabled := rc.enabled
	rc.mu.RUnlock()

	hits := rc.hits.Load()
	misses := rc.misses.Load()
	total := hits + misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}

	costSaved := float64(rc.costSaved.Load()) / 1_000_000.0

	return CacheStats{
		Enabled:    enabled,
		Entries:    entryCount,
		MaxEntries: rc.maxEntries,
		Hits:       hits,
		Misses:     misses,
		HitRate:    hitRate,
		TokensSaved: 0, // computed from DB if needed
		CostSaved:  costSaved,
		TTLMinutes: int(rc.defaultTTL.Minutes()),
	}
}
