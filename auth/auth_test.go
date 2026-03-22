package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Key generation ────────────────────────────────────────────────────────

func TestGenerateKey_Format(t *testing.T) {
	rawKey, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	if !strings.HasPrefix(rawKey, "tc_") {
		t.Errorf("key should start with 'tc_', got %q", rawKey[:10])
	}

	// tc_ + 64 hex chars = 67 total
	if len(rawKey) != 67 {
		t.Errorf("key length: got %d, want 67 (tc_ + 64 hex)", len(rawKey))
	}

	if hash == "" {
		t.Error("hash should not be empty")
	}
	if len(hash) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("hash length: got %d, want 64", len(hash))
	}
}

func TestGenerateKey_Unique(t *testing.T) {
	key1, _, _ := GenerateKey()
	key2, _, _ := GenerateKey()
	if key1 == key2 {
		t.Error("two generated keys should be different")
	}
}

func TestGenerateKey_HashMatches(t *testing.T) {
	rawKey, hash, _ := GenerateKey()
	recomputed := HashKey(rawKey)
	if hash != recomputed {
		t.Errorf("hash mismatch: GenerateKey returned %q, HashKey returned %q", hash, recomputed)
	}
}

// ── HashKey ───────────────────────────────────────────────────────────────

func TestHashKey_Deterministic(t *testing.T) {
	key := "tc_abcdef1234567890"
	h1 := HashKey(key)
	h2 := HashKey(key)
	if h1 != h2 {
		t.Error("same key should produce same hash")
	}
}

func TestHashKey_DifferentKeys(t *testing.T) {
	h1 := HashKey("tc_key_one")
	h2 := HashKey("tc_key_two")
	if h1 == h2 {
		t.Error("different keys should produce different hashes")
	}
}

// ── ConstantTimeCompare ───────────────────────────────────────────────────

func TestConstantTimeCompare(t *testing.T) {
	if !ConstantTimeCompare("hello", "hello") {
		t.Error("identical strings should compare equal")
	}
	if ConstantTimeCompare("hello", "world") {
		t.Error("different strings should not compare equal")
	}
	if ConstantTimeCompare("hello", "hell") {
		t.Error("different lengths should not compare equal")
	}
	if ConstantTimeCompare("", "") != true {
		t.Error("empty strings should compare equal")
	}
}

// ── Mock KeyStore ─────────────────────────────────────────────────────────

type mockKeyStore struct {
	keys map[string]int64 // hash → keyID
}

func (m *mockKeyStore) ValidateAPIKeyHash(hash string) (int64, string, error) {
	if id, ok := m.keys[hash]; ok {
		return id, "test-key", nil
	}
	return 0, "", nil
}

// ── Middleware tests ──────────────────────────────────────────────────────

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
}

func TestMiddleware_Disabled(t *testing.T) {
	m := NewMiddleware(&mockKeyStore{}, false)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("disabled middleware should pass through, got %d", rec.Code)
	}
}

func TestMiddleware_MissingKey_401(t *testing.T) {
	m := NewMiddleware(&mockKeyStore{}, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing key should return 401, got %d", rec.Code)
	}
}

func TestMiddleware_InvalidKey_401(t *testing.T) {
	store := &mockKeyStore{keys: map[string]int64{}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Proxy-Key", "tc_invalid_key_that_does_not_exist_1234567890abcdef")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid key should return 401, got %d", rec.Code)
	}
}

func TestMiddleware_ValidKey_XProxyKey(t *testing.T) {
	rawKey, hash, _ := GenerateKey()
	store := &mockKeyStore{keys: map[string]int64{hash: 1}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Proxy-Key", rawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("valid key via X-Proxy-Key should pass, got %d", rec.Code)
	}
}

func TestMiddleware_ValidKey_Bearer(t *testing.T) {
	rawKey, hash, _ := GenerateKey()
	store := &mockKeyStore{keys: map[string]int64{hash: 2}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("valid key via Bearer should pass, got %d", rec.Code)
	}
}

func TestMiddleware_ValidKey_XAPIKey(t *testing.T) {
	rawKey, hash, _ := GenerateKey()
	store := &mockKeyStore{keys: map[string]int64{hash: 3}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", rawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("valid key via X-API-Key should pass, got %d", rec.Code)
	}
}

func TestMiddleware_QueryParam_NotSupported(t *testing.T) {
	// Query parameter auth is intentionally not supported (security risk: logged in server/proxy logs)
	rawKey, hash, _ := GenerateKey()
	store := &mockKeyStore{keys: map[string]int64{hash: 4}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test?api_key="+rawKey, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("query param auth should be rejected, got %d", rec.Code)
	}
}

func TestMiddleware_NonLPKey_Ignored(t *testing.T) {
	// A provider key (not tc_ prefixed) should not be extracted
	store := &mockKeyStore{keys: map[string]int64{}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-openai-key-here")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should be 401 because the non-tc_ key is ignored
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-tc_ key should be ignored, got %d", rec.Code)
	}
}

func TestMiddleware_SetEnabled(t *testing.T) {
	m := NewMiddleware(&mockKeyStore{}, false)
	if m.IsEnabled() {
		t.Error("expected disabled initially")
	}
	m.SetEnabled(true)
	if !m.IsEnabled() {
		t.Error("expected enabled after SetEnabled(true)")
	}
}

func TestMiddleware_InvalidateCache(t *testing.T) {
	rawKey, hash, _ := GenerateKey()
	store := &mockKeyStore{keys: map[string]int64{hash: 1}}
	m := NewMiddleware(store, true)
	handler := m.Wrap(okHandler())

	// First request populates cache
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Proxy-Key", rawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec.Code)
	}

	// Invalidate cache
	m.InvalidateCache()

	// Remove key from store
	delete(store.keys, hash)

	// Request should now fail (cache was cleared, DB lookup returns 0)
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.Header.Set("X-Proxy-Key", rawKey)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("after cache invalidation + key removal, should get 401, got %d", rec2.Code)
	}
}

// ── extractKey priority tests ─────────────────────────────────────────────

func TestExtractKey_Priority(t *testing.T) {
	// X-Proxy-Key should take priority over Authorization
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Proxy-Key", "tc_proxy_key_wins")
	req.Header.Set("Authorization", "Bearer tc_bearer_key")

	got := extractKey(req)
	if got != "tc_proxy_key_wins" {
		t.Errorf("X-Proxy-Key should have priority, got %q", got)
	}
}

func TestExtractKey_EmptyRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	got := extractKey(req)
	if got != "" {
		t.Errorf("expected empty key, got %q", got)
	}
}
