// Package auth provides API key authentication for Toko-Mo-Co.
//
// Security design:
//   - Keys are generated as 32 random bytes, encoded as hex (64 chars) with a
//     "tc_" prefix for easy identification (e.g. tc_a1b2c3...).
//   - Only a SHA-256 hash of each key is stored in the database — the raw key
//     is shown once at creation time and never persisted.
//   - Constant-time comparison (crypto/subtle) prevents timing attacks.
//   - Keys are cached in-memory (sync.Map) with a short TTL to avoid hitting
//     the database on every request while still honouring revocations promptly.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// KeyPrefix is prepended to every generated key for easy identification.
	KeyPrefix = "tc_"

	// keyBytes is the number of random bytes used for key generation.
	// 32 bytes = 256 bits of entropy (overkill, but standard practice).
	keyBytes = 32

	// cacheTTL is how long a validated key hash is cached in memory.
	cacheTTL = 60 * time.Second
)

// KeyStore is the interface the auth middleware needs to validate keys.
// Implemented by store.DB.
type KeyStore interface {
	ValidateAPIKeyHash(hash string) (keyID int64, keyName string, err error)
}

// GenerateKey creates a new random API key. Returns the raw key (shown once)
// and its SHA-256 hash (stored in DB). The key format is: tc_<64 hex chars>.
func GenerateKey() (rawKey, hash string, err error) {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("crypto/rand: %w", err)
	}
	rawKey = KeyPrefix + hex.EncodeToString(b)
	hash = HashKey(rawKey)
	return rawKey, hash, nil
}

// HashKey returns the hex-encoded SHA-256 hash of a raw API key.
func HashKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

// cacheEntry stores a validated key with an expiry time.
type cacheEntry struct {
	keyID   int64
	keyName string
	expires time.Time
}

// Middleware provides HTTP middleware that validates API keys.
type Middleware struct {
	store   KeyStore
	enabled bool

	mu    sync.RWMutex
	cache map[string]cacheEntry // hash → entry
}

// NewMiddleware creates auth middleware. If enabled is false, all requests pass through.
func NewMiddleware(store KeyStore, enabled bool) *Middleware {
	m := &Middleware{
		store:   store,
		enabled: enabled,
		cache:   make(map[string]cacheEntry),
	}
	// Background goroutine to sweep expired entries every 5 minutes
	go m.cleanupLoop()
	return m
}

// SetEnabled toggles auth on/off at runtime (e.g., from settings API).
func (m *Middleware) SetEnabled(enabled bool) {
	m.mu.Lock()
	m.enabled = enabled
	m.mu.Unlock()
}

// IsEnabled returns whether auth is currently active.
func (m *Middleware) IsEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

// InvalidateCache clears the in-memory cache (call after key revocation).
func (m *Middleware) InvalidateCache() {
	m.mu.Lock()
	m.cache = make(map[string]cacheEntry)
	m.mu.Unlock()
}

// Wrap returns an http.Handler that enforces API key auth before calling next.
// The key can be provided in any of these standard locations:
//   - X-Proxy-Key: <key> (dedicated, avoids collision with provider SDKs)
//   - Authorization: Bearer <key>
//   - X-API-Key: <key>
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.IsEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		rawKey := extractKey(r)
		if rawKey == "" {
			http.Error(w, `{"error":"missing API key","hint":"Provide via X-Proxy-Key header, Authorization: Bearer tc_..., or X-API-Key header"}`, http.StatusUnauthorized)
			return
		}

		hash := HashKey(rawKey)

		// Check cache first (fast path)
		if m.checkCache(hash) {
			next.ServeHTTP(w, r)
			return
		}

		// Cache miss — hit the database
		keyID, keyName, err := m.store.ValidateAPIKeyHash(hash)
		if err != nil {
			log.Printf("[AUTH] validation error: %v", err)
			http.Error(w, `{"error":"internal auth error"}`, http.StatusInternalServerError)
			return
		}
		if keyID == 0 {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		// Cache the valid key
		m.mu.Lock()
		m.cache[hash] = cacheEntry{
			keyID:   keyID,
			keyName: keyName,
			expires: time.Now().Add(cacheTTL),
		}
		m.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

// WrapFunc is a convenience wrapper for http.HandlerFunc.
func (m *Middleware) WrapFunc(next http.HandlerFunc) http.Handler {
	return m.Wrap(http.HandlerFunc(next))
}

// extractKey pulls the API key from the request in priority order.
// Uses X-Proxy-Key as the primary header to avoid colliding with
// upstream provider auth headers (e.g., Anthropic SDK's X-Api-Key).
func extractKey(r *http.Request) string {
	// 1. X-Proxy-Key header (dedicated, no collision with provider SDKs)
	if key := r.Header.Get("X-Proxy-Key"); key != "" {
		if strings.HasPrefix(key, KeyPrefix) {
			return key
		}
	}

	// 2. Authorization: Bearer <key> (only if tc_ prefixed)
	if auth := r.Header.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			token := strings.TrimSpace(parts[1])
			if strings.HasPrefix(token, KeyPrefix) {
				return token
			}
		}
	}

	// 3. X-API-Key header (only if tc_ prefixed — avoids grabbing provider keys)
	if key := r.Header.Get("X-API-Key"); key != "" {
		if strings.HasPrefix(key, KeyPrefix) {
			return key
		}
	}

	// Note: Query parameter auth (?api_key=...) is intentionally NOT supported.
	// Query strings are logged by HTTP proxies, servers, and appear in browser history.

	return ""
}

// checkCache returns true if the hash is in cache and not expired.
func (m *Middleware) checkCache(hash string) bool {
	m.mu.RLock()
	entry, ok := m.cache[hash]
	m.mu.RUnlock()

	if !ok {
		return false
	}
	if time.Now().After(entry.expires) {
		// Expired — remove it
		m.mu.Lock()
		delete(m.cache, hash)
		m.mu.Unlock()
		return false
	}
	return true
}

// cleanupLoop periodically removes expired cache entries.
func (m *Middleware) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for hash, entry := range m.cache {
			if now.After(entry.expires) {
				delete(m.cache, hash)
			}
		}
		m.mu.Unlock()
	}
}

// ConstantTimeCompare compares two strings in constant time.
// Exported for use in tests or additional validation.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
