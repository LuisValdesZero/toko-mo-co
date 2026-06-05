package tracker

import (
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"tokomoco/store"
)

// UnknownModelEntry tracks a model that was seen but has no pricing configured.
type UnknownModelEntry struct {
	Model     string    `json:"model"`
	HitCount  int       `json:"hit_count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// PricingStore provides an in-memory cache of model pricing backed by SQLite.
// All mutations go through the DB then reload the cache so the hot-path
// (LookupPricing) only needs a read lock.
//
// Prefix matching uses a sorted slice (longest prefixes first) so
// LookupPricing is O(k) where k = number of prefixes, with early exit on
// first match — much better cache locality than map iteration.
type PricingStore struct {
	db *store.DB

	mu             sync.RWMutex
	cache          map[string]ModelPricing // model_prefix → pricing (exact lookups)
	sortedPrefixes []prefixEntry           // sorted longest-first for prefix matching

	unknownMu     sync.RWMutex
	unknownModels map[string]*UnknownModelEntry
}

// prefixEntry pairs a prefix string with its pricing for sorted-prefix matching.
type prefixEntry struct {
	prefix  string
	pricing ModelPricing
}

// NewPricingStore creates a pricing store and loads the cache from the DB.
func NewPricingStore(db *store.DB) *PricingStore {
	ps := &PricingStore{
		db:            db,
		cache:         make(map[string]ModelPricing),
		unknownModels: make(map[string]*UnknownModelEntry),
	}
	ps.ReloadCache()
	return ps
}

// ReloadCache fetches all pricing rows from the DB and rebuilds the in-memory
// map and sorted prefix list.
func (ps *PricingStore) ReloadCache() {
	rows, err := ps.db.ListModelPricing()
	if err != nil {
		log.Printf("[PRICING] failed to reload cache: %v", err)
		return
	}

	newCache := make(map[string]ModelPricing, len(rows))
	sorted := make([]prefixEntry, 0, len(rows))
	for _, r := range rows {
		p := ModelPricing{
			InputPer1M:       r.InputPer1M,
			CachedInputPer1M: r.CachedInputPer1M,
			OutputPer1M:      r.OutputPer1M,
		}
		newCache[r.ModelPrefix] = p
		sorted = append(sorted, prefixEntry{prefix: r.ModelPrefix, pricing: p})
	}
	// Sort longest prefix first — first match wins as longest match.
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].prefix) > len(sorted[j].prefix)
	})

	ps.mu.Lock()
	ps.cache = newCache
	ps.sortedPrefixes = sorted
	ps.mu.Unlock()
}

// SeedDefaults inserts the hardcoded defaultModelPricing entries into the DB
// if the table is empty. Returns true if seeding occurred.
func (ps *PricingStore) SeedDefaults() (bool, error) {
	count, err := ps.db.CountModelPricing()
	if err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}

	now := time.Now()
	for prefix, p := range defaultModelPricing {
		row := store.ModelPricingRow{
			ModelPrefix:      prefix,
			InputPer1M:       p.InputPer1M,
			CachedInputPer1M: p.CachedInputPer1M,
			OutputPer1M:      p.OutputPer1M,
			Provider:         detectProvider(prefix),
			Source:           "seed",
			UpdatedAt:        now,
		}
		if _, err := ps.db.InsertModelPricing(row); err != nil {
			log.Printf("[PRICING] failed to seed %q: %v", prefix, err)
			// Continue seeding remaining entries
		}
	}

	ps.ReloadCache()
	return true, nil
}

// ResetDefaults deletes all entries and re-seeds from hardcoded defaults.
func (ps *PricingStore) ResetDefaults() error {
	if err := ps.db.DeleteAllModelPricing(); err != nil {
		return err
	}
	_, err := ps.SeedDefaults()
	return err
}

// LookupPricing finds the best pricing for a model. Hot path — uses RLock only.
//
// Priority:
//  1. Exact match in DB cache
//  2. Longest-prefix match in DB cache
//  3. Exact/prefix match in hardcoded defaults (fallback)
//  4. Unknown model: track it, return mid-tier fallback
func (ps *PricingStore) LookupPricing(model string) ModelPricing {
	ps.mu.RLock()
	cache := ps.cache
	sorted := ps.sortedPrefixes
	ps.mu.RUnlock()

	// 1. Exact match in DB cache — O(1)
	if p, ok := cache[model]; ok {
		return p
	}

	// 2. Longest-prefix match in DB cache — sorted longest-first, first match wins
	for _, entry := range sorted {
		if strings.HasPrefix(model, entry.prefix) {
			return entry.pricing
		}
	}

	// 3. Hardcoded fallback — exact then longest-prefix
	if p, ok := defaultModelPricing[model]; ok {
		return p
	}
	best := ""
	for key := range defaultModelPricing {
		if strings.HasPrefix(model, key) && len(key) > len(best) {
			best = key
		}
	}
	if best != "" {
		return defaultModelPricing[best]
	}

	// 4. Unknown model
	ps.trackUnknownModel(model)
	return ModelPricing{InputPer1M: 3.0, OutputPer1M: 15.0}
}

// maxUnknownModels caps the unknownModels map to prevent unbounded memory growth.
// When the cap is reached, the oldest entry (by LastSeen) is evicted.
const maxUnknownModels = 500

// trackUnknownModel records a model that couldn't be matched.
func (ps *PricingStore) trackUnknownModel(model string) {
	now := time.Now()
	ps.unknownMu.Lock()
	defer ps.unknownMu.Unlock()

	if entry, ok := ps.unknownModels[model]; ok {
		entry.HitCount++
		entry.LastSeen = now
	} else {
		// Evict oldest entry if at capacity
		if len(ps.unknownModels) >= maxUnknownModels {
			var oldestKey string
			var oldestTime time.Time
			for k, e := range ps.unknownModels {
				if oldestKey == "" || e.LastSeen.Before(oldestTime) {
					oldestKey = k
					oldestTime = e.LastSeen
				}
			}
			if oldestKey != "" {
				delete(ps.unknownModels, oldestKey)
			}
		}
		ps.unknownModels[model] = &UnknownModelEntry{
			Model:     model,
			HitCount:  1,
			FirstSeen: now,
			LastSeen:  now,
		}
		log.Printf("[PRICING] unknown model detected: %q (using mid-tier fallback $3/$15)", model)
	}
}

// UnknownModels returns all tracked unknown models.
func (ps *PricingStore) UnknownModels() []UnknownModelEntry {
	ps.unknownMu.RLock()
	defer ps.unknownMu.RUnlock()

	result := make([]UnknownModelEntry, 0, len(ps.unknownModels))
	for _, e := range ps.unknownModels {
		result = append(result, *e)
	}
	return result
}

// ClearUnknownModel removes a model from the unknown tracking list.
func (ps *PricingStore) ClearUnknownModel(model string) {
	ps.unknownMu.Lock()
	delete(ps.unknownModels, model)
	ps.unknownMu.Unlock()
}

// IsKnownModel returns true if the model has pricing configured (exact or prefix match)
// in either the DB cache or hardcoded defaults. Unlike LookupPricing, this does NOT
// track the model as unknown or return a fallback — it's a pure existence check.
func (ps *PricingStore) IsKnownModel(model string) bool {
	ps.mu.RLock()
	cache := ps.cache
	sorted := ps.sortedPrefixes
	ps.mu.RUnlock()

	// 1. Exact match in DB cache
	if _, ok := cache[model]; ok {
		return true
	}

	// 2. Prefix match in DB cache (sorted longest-first)
	for _, entry := range sorted {
		if strings.HasPrefix(model, entry.prefix) {
			return true
		}
	}

	// 3. Hardcoded defaults — exact match
	if _, ok := defaultModelPricing[model]; ok {
		return true
	}

	// 4. Hardcoded defaults — prefix match
	for key := range defaultModelPricing {
		if strings.HasPrefix(model, key) {
			return true
		}
	}

	return false
}

// CacheSize returns the number of entries in the in-memory cache.
func (ps *PricingStore) CacheSize() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.cache)
}

// detectProvider infers the provider from a model prefix string.
func detectProvider(prefix string) string {
	switch {
	case strings.HasPrefix(prefix, "gpt-") ||
		strings.HasPrefix(prefix, "o1") ||
		strings.HasPrefix(prefix, "o3") ||
		strings.HasPrefix(prefix, "o4"):
		return "openai"
	case strings.HasPrefix(prefix, "claude-"):
		return "anthropic"
	case strings.HasPrefix(prefix, "gemini-"):
		return "google"
	case strings.Contains(prefix, "/"):
		// vendor-namespaced ids (e.g. "openai/gpt-4o", "meta-llama/...") are
		// reached through the OpenRouter custom provider.
		return "openrouter"
	default:
		return "other"
	}
}
