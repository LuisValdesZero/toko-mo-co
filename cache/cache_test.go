package cache

import (
	"testing"
	"time"
)

// ── BuildRequestHash tests ────────────────────────────────────────────────

func TestBuildRequestHash_Deterministic(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}],"temperature":0}`)
	h1 := BuildRequestHash("openai", "gpt-4o", body)
	h2 := BuildRequestHash("openai", "gpt-4o", body)
	if h1 != h2 {
		t.Errorf("same input produced different hashes: %q vs %q", h1, h2)
	}
}

func TestBuildRequestHash_DifferentMessages(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"Goodbye"}]}`)
	h1 := BuildRequestHash("openai", "gpt-4o", body1)
	h2 := BuildRequestHash("openai", "gpt-4o", body2)
	if h1 == h2 {
		t.Error("different messages should produce different hashes")
	}
}

func TestBuildRequestHash_DifferentProviders(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	h1 := BuildRequestHash("openai", "gpt-4o", body)
	h2 := BuildRequestHash("anthropic", "gpt-4o", body)
	if h1 == h2 {
		t.Error("different providers should produce different hashes")
	}
}

func TestBuildRequestHash_IgnoresNonDeterministic(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"temperature":0}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"temperature":0,"stream":true,"max_tokens":100,"top_p":0.9}`)

	h1 := BuildRequestHash("openai", "gpt-4o", body1)
	h2 := BuildRequestHash("openai", "gpt-4o", body2)
	if h1 != h2 {
		t.Error("non-deterministic fields (stream, max_tokens, top_p) should not affect hash")
	}
}

func TestBuildRequestHash_TemperatureMatters(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"temperature":0}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"temperature":0.7}`)
	h1 := BuildRequestHash("openai", "gpt-4o", body1)
	h2 := BuildRequestHash("openai", "gpt-4o", body2)
	if h1 == h2 {
		t.Error("different temperatures should produce different hashes")
	}
}

func TestBuildRequestHash_SystemField(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"system":"Be helpful"}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"system":"Be rude"}`)
	h1 := BuildRequestHash("anthropic", "claude-sonnet-4", body1)
	h2 := BuildRequestHash("anthropic", "claude-sonnet-4", body2)
	if h1 == h2 {
		t.Error("different system prompts should produce different hashes")
	}
}

func TestBuildRequestHash_InvalidJSON(t *testing.T) {
	h := BuildRequestHash("openai", "gpt-4o", []byte("not json"))
	if h == "" {
		t.Error("invalid JSON should still produce a hash (raw body fallback)")
	}
	if len(h) != 64 {
		t.Errorf("hash length: got %d, want 64", len(h))
	}
}

func TestBuildRequestHash_EmptyBody(t *testing.T) {
	h := BuildRequestHash("openai", "gpt-4o", []byte(""))
	if h == "" {
		t.Error("empty body should still produce a hash")
	}
}

func TestBuildRequestHash_ToolsField(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"Hello"}],"tools":[{"name":"search"}]}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	h1 := BuildRequestHash("openai", "gpt-4o", body1)
	h2 := BuildRequestHash("openai", "gpt-4o", body2)
	if h1 == h2 {
		t.Error("tools field should affect the hash")
	}
}

// ── In-memory cache tests (no DB) ────────────────────────────────────────

func TestCache_StoreAndLookup(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	entry := &CacheEntry{
		Hash:         "abc123",
		Provider:     "openai",
		Model:        "gpt-4o",
		StatusCode:   200,
		ResponseBody: []byte(`{"choices":[{"message":{"content":"Hi"}}]}`),
		CostPerHit:   0.005,
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	rc.Store("abc123", entry)

	got, ok := rc.Lookup("abc123")
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if string(got.ResponseBody) != string(entry.ResponseBody) {
		t.Error("cached response body mismatch")
	}
}

func TestCache_Miss(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	_, ok := rc.Lookup("nonexistent")
	if ok {
		t.Error("expected cache miss for nonexistent key")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	entry := &CacheEntry{
		Hash:      "expired-key",
		ExpiresAt: time.Now().Add(-1 * time.Second),
	}
	rc.Store("expired-key", entry)

	_, ok := rc.Lookup("expired-key")
	if ok {
		t.Error("expected miss for expired entry")
	}
}

func TestCache_LRUEviction(t *testing.T) {
	rc := NewResponseCache(nil, 3, time.Hour, true)

	for i := 0; i < 3; i++ {
		key := string(rune('a'+i)) + "key"
		rc.Store(key, &CacheEntry{
			Hash:      key,
			ExpiresAt: time.Now().Add(time.Hour),
		})
	}

	// Add one more — should evict the LRU (first inserted)
	rc.Store("dkey", &CacheEntry{
		Hash:      "dkey",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	if _, ok := rc.Lookup("akey"); ok {
		t.Error("expected 'akey' to be evicted (LRU)")
	}
	if _, ok := rc.Lookup("dkey"); !ok {
		t.Error("expected 'dkey' to exist")
	}
}

func TestCache_LRUPromotion(t *testing.T) {
	rc := NewResponseCache(nil, 3, time.Hour, true)

	for _, key := range []string{"first", "second", "third"} {
		rc.Store(key, &CacheEntry{
			Hash:      key,
			ExpiresAt: time.Now().Add(time.Hour),
		})
	}

	// Access "first" to promote it in LRU
	rc.Lookup("first")

	// Add new entry — should evict "second" (now the LRU), not "first"
	rc.Store("fourth", &CacheEntry{
		Hash:      "fourth",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	if _, ok := rc.Lookup("first"); !ok {
		t.Error("expected 'first' to survive after LRU promotion")
	}
	if _, ok := rc.Lookup("second"); ok {
		t.Error("expected 'second' to be evicted")
	}
}

func TestCache_Stats(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	rc.Store("key1", &CacheEntry{
		Hash:       "key1",
		CostPerHit: 0.01,
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	rc.Lookup("key1")       // hit
	rc.Lookup("nonexistent") // miss

	stats := rc.Stats()
	if stats.Hits != 1 {
		t.Errorf("Hits: got %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses: got %d, want 1", stats.Misses)
	}
	if stats.Entries != 1 {
		t.Errorf("Entries: got %d, want 1", stats.Entries)
	}
	if stats.HitRate != 0.5 {
		t.Errorf("HitRate: got %f, want 0.5", stats.HitRate)
	}
	if stats.Enabled != true {
		t.Error("expected Enabled=true")
	}
}

func TestCache_StatsZero(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)
	stats := rc.Stats()
	if stats.Hits != 0 || stats.Misses != 0 || stats.HitRate != 0 {
		t.Errorf("empty cache should have zero stats, got hits=%d misses=%d rate=%f",
			stats.Hits, stats.Misses, stats.HitRate)
	}
}

func TestCache_Flush(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	rc.Store("key1", &CacheEntry{Hash: "key1", ExpiresAt: time.Now().Add(time.Hour)})
	rc.Store("key2", &CacheEntry{Hash: "key2", ExpiresAt: time.Now().Add(time.Hour)})
	rc.Lookup("key1")

	rc.Flush()

	stats := rc.Stats()
	if stats.Entries != 0 {
		t.Errorf("entries after flush: got %d, want 0", stats.Entries)
	}
	if stats.Hits != 0 {
		t.Errorf("hits after flush: got %d, want 0", stats.Hits)
	}
}

func TestCache_EnableDisable(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)
	if !rc.IsEnabled() {
		t.Error("expected enabled=true")
	}
	rc.SetEnabled(false)
	if rc.IsEnabled() {
		t.Error("expected enabled=false after SetEnabled(false)")
	}
	rc.SetEnabled(true)
	if !rc.IsEnabled() {
		t.Error("expected enabled=true after SetEnabled(true)")
	}
}

func TestCache_StoreUpdate(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	rc.Store("key1", &CacheEntry{
		Hash:         "key1",
		ResponseBody: []byte("original"),
		ExpiresAt:    time.Now().Add(time.Hour),
	})

	rc.Store("key1", &CacheEntry{
		Hash:         "key1",
		ResponseBody: []byte("updated"),
		ExpiresAt:    time.Now().Add(time.Hour),
	})

	got, ok := rc.Lookup("key1")
	if !ok {
		t.Fatal("expected hit after update")
	}
	if string(got.ResponseBody) != "updated" {
		t.Errorf("response body: got %q, want %q", got.ResponseBody, "updated")
	}

	stats := rc.Stats()
	if stats.Entries != 1 {
		t.Errorf("entries after update: got %d, want 1", stats.Entries)
	}
}

func TestCache_DefaultTTL(t *testing.T) {
	rc := NewResponseCache(nil, 100, 45*time.Minute, true)
	if rc.DefaultTTL() != 45*time.Minute {
		t.Errorf("DefaultTTL: got %v, want 45m", rc.DefaultTTL())
	}
}

func TestCache_CostSavedTracking(t *testing.T) {
	rc := NewResponseCache(nil, 100, time.Hour, true)

	rc.Store("key1", &CacheEntry{
		Hash:       "key1",
		CostPerHit: 0.005,
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// 3 cache hits
	rc.Lookup("key1")
	rc.Lookup("key1")
	rc.Lookup("key1")

	stats := rc.Stats()
	if stats.Hits != 3 {
		t.Errorf("Hits: got %d, want 3", stats.Hits)
	}
	// 3 hits * $0.005 = $0.015
	expectedCost := 0.015
	if stats.CostSaved < expectedCost-0.001 || stats.CostSaved > expectedCost+0.001 {
		t.Errorf("CostSaved: got %f, want ~%f", stats.CostSaved, expectedCost)
	}
}
