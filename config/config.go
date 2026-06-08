// Package config loads and validates proxy configuration from a YAML/JSON file
// with environment variable overrides.  Every field has a sensible default so
// the proxy works out of the box with zero configuration.
package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the top-level configuration structure.
// Field precedence (highest to lowest):
//  1. Environment variable
//  2. config.json file
//  3. Default value
type Config struct {
	// Server
	Port           string `json:"port"`            // HTTP listen port (default: 8080)
	AllowedOrigins string `json:"allowed_origins"` // CORS origins for WS ("*" or "http://localhost:3000,…")

	// Database
	DBPath      string `json:"db_path"`      // SQLite file path (default: proxy.db)
	DatabaseURL string `json:"database_url"` // Postgres DSN (postgres://...). When set, Postgres is used instead of SQLite.
	DBKeepDays  int    `json:"db_keep_days"` // Days of request history to retain (default: 30)

	// Upstream timeouts
	UpstreamTimeoutSec int `json:"upstream_timeout_sec"` // Upstream HTTP timeout (default: 300)

	// Loop detection
	LoopThreshold     int     `json:"loop_threshold"`      // Similar requests before flagging (default: 3)
	LoopSimilarity    float64 `json:"loop_similarity"`     // Similarity threshold 0–1 (default: 0.8)
	LoopWindowMinutes int     `json:"loop_window_minutes"` // Rolling window for loop detection (default: 5)

	// Dashboard
	HistoryLimit   int `json:"history_limit"`         // Requests replayed to new WS clients (default: 200)
	SessionMaxAge  int `json:"session_max_age_hours"` // Hours before idle sessions are evicted (default: 24)
	SessionMaxSize int `json:"session_max_size"`      // Max in-memory sessions (LRU cap, default: 10000)

	// Injection
	InjectionMode    string  `json:"injection_mode"`        // "metadata" | "content" | "hybrid" (default: metadata)
	ContentThreshold float64 `json:"content_threshold_usd"` // Cost to escalate to content injection (default: 10.0)

	// Rules Engine
	RulesEnabled bool `json:"rules_enabled"` // Enable the rules engine (default: true)

	// Retry & Fallback
	RetryEnabled      bool   `json:"retry_enabled"`          // Enable automatic retries (default: true)
	RetryMaxAttempts  int    `json:"retry_max_attempts"`     // Max retry attempts (default: 3)
	RetryInitialDelay int    `json:"retry_initial_delay_ms"` // Initial retry delay in ms (default: 1000)
	RetryMaxDelay     int    `json:"retry_max_delay_ms"`     // Max retry delay in ms (default: 30000)
	FallbackEnabled   bool   `json:"fallback_enabled"`       // Enable fallback to alternate providers (default: false)
	FallbackStrategy  string `json:"fallback_strategy"`      // "any" | "same_tier" | "cheaper" (default: same_tier)

	// Authentication
	AuthEnabled bool `json:"auth_enabled"` // Require API keys for proxy endpoints (default: false)

	// Response Cache
	CacheEnabled    bool `json:"cache_enabled"`     // Enable response cache (default: true)
	CacheMaxEntries int  `json:"cache_max_entries"` // Max cached responses (default: 1000)
	CacheTTLMinutes int  `json:"cache_ttl_minutes"` // Default TTL in minutes (default: 60)
	CacheOnlyTemp0  bool `json:"cache_only_temp0"`  // Only cache temperature=0 requests (default: true)

	// Security — PII Redaction
	PIIEnabled    bool   `json:"pii_enabled"`    // Enable PII/secret redaction (default: false)
	PIIMode       string `json:"pii_mode"`       // "redact" | "hash" | "placeholder" (default: "redact")
	PIICategories string `json:"pii_categories"` // Comma-separated enabled category keys (default: all)

	// Semantic Cache
	SemanticCacheEnabled    bool    `json:"semantic_cache_enabled"`     // Enable embedding-based cache (default: false)
	SemanticCacheThreshold  float64 `json:"semantic_cache_threshold"`   // Cosine similarity threshold 0–1 (default: 0.95)
	SemanticCacheMaxVectors int     `json:"semantic_cache_max_vectors"` // Max stored vectors (default: 10000)
	EmbeddingProvider       string  `json:"embedding_provider"`         // "openai" | "aratiri-bge-m3" (default: openai)
	EmbeddingModel          string  `json:"embedding_model"`            // Embedding model name (default: text-embedding-3-small)
	EmbeddingAPIKey         string  `json:"embedding_api_key"`          // Embedding API key (OpenAI: OPENAI_API_KEY; aratiri-bge-m3: PLATFORM_API_KEY, sent as X-API-Key)
	EmbeddingBaseURL        string  `json:"embedding_base_url"`         // Base URL for the aratiri-bge-m3 provider (.../api/v1; /embed appended)
	SemanticCacheSparseWeight float64 `json:"semantic_cache_sparse_weight"` // Hybrid blend: weight of the sparse score, 0–1 (bge-m3 only; default 0.3)

	// Pricing
	PricingOpenRouterAutoRefresh bool `json:"pricing_openrouter_auto_refresh"` // Refresh model prices from OpenRouter on boot + daily (default: true)

	// Memory Layer (mem0-style agent memory)
	MemoryEnabled        bool    `json:"memory_enabled"`            // Enable memory extraction and retrieval (default: false)
	MemoryMaxEntries     int     `json:"memory_max_entries"`        // Max stored memory facts (default: 10000)
	MemoryThreshold      float64 `json:"memory_threshold"`          // Similarity threshold for retrieval 0–1 (default: 0.7)
	MemoryMaxResults     int     `json:"memory_max_results"`        // Max memories injected per request (default: 5)
	MemoryRecencyLambda  float64 `json:"memory_recency_lambda"`     // Decay rate for recency-weighted scoring (default: 0.01)
	MemoryConflictThresh float64 `json:"memory_conflict_threshold"` // Similarity threshold for conflict detection (default: 0.85)
	MemoryTTLDays        int     `json:"memory_ttl_days"`           // Days before unused memories are eviction candidates (default: 90)

	// NeMo Guard — jailbreak detection (env-gated; enabled when NeMoGuardURL is set)
	NeMoGuardURL          string  `json:"nemo_guard_url"`           // NeMo Guard NIM base URL; empty = disabled
	NeMoGuardClassifyPath string  `json:"nemo_guard_classify_path"` // classify endpoint path (default: /v1/classify)
	NeMoGuardAPIKey       string  `json:"nemo_guard_api_key"`       // optional Bearer token
	NeMoGuardMode         string  `json:"nemo_guard_mode"`          // "block" (default) | "flag"
	NeMoGuardThreshold    float64 `json:"nemo_guard_threshold"`     // score >= threshold = jailbreak (when NIM returns a score)
	NeMoGuardTimeoutSec   int     `json:"nemo_guard_timeout_sec"`   // per-request timeout (default: 10)
}

// Default returns a Config with all defaults pre-filled.
func Default() Config {
	return Config{
		Port:                    "8081",
		AllowedOrigins:          "*",
		DBPath:                  "proxy.db",
		DBKeepDays:              30,
		UpstreamTimeoutSec:      300,
		LoopThreshold:           3,
		LoopSimilarity:          0.8,
		LoopWindowMinutes:       5,
		HistoryLimit:            200,
		SessionMaxAge:           24,
		SessionMaxSize:          10_000,
		InjectionMode:           "metadata",
		ContentThreshold:        10.0,
		RulesEnabled:            true,
		RetryEnabled:            true,
		RetryMaxAttempts:        3,
		RetryInitialDelay:       1000,  // 1 second
		RetryMaxDelay:           30000, // 30 seconds
		FallbackEnabled:         false, // Conservative default
		FallbackStrategy:        "same_tier",
		AuthEnabled:             false, // Off by default — enable after creating first key
		CacheEnabled:            true,
		CacheMaxEntries:         1000,
		CacheTTLMinutes:         60,
		CacheOnlyTemp0:          true,
		PIIEnabled:              false, // Off by default — enable in Security settings
		PIIMode:                 "redact",
		PIICategories:           "",    // Empty = all categories when enabled (populated on first save)
		SemanticCacheEnabled:    false, // Off by default — requires embedding API key
		SemanticCacheThreshold:  0.95,
		SemanticCacheMaxVectors: 10000,
		EmbeddingProvider:       "openai",
		EmbeddingModel:          "text-embedding-3-small",
		EmbeddingAPIKey:         "",    // Falls back to OPENAI_API_KEY env var
		EmbeddingBaseURL:        "http://platform-api.service.consul:8000/api/v1", // aratiri-bge-m3 provider
		SemanticCacheSparseWeight: 0.3,
		PricingOpenRouterAutoRefresh: true,
		MemoryEnabled:           false, // Off by default — requires embedding API key
		MemoryMaxEntries:        10000,
		MemoryThreshold:         0.7, // Lower than semantic cache — memories are loosely related
		MemoryMaxResults:        5,
		MemoryRecencyLambda:     0.01, // 30-day-old memory retains ~74% weight
		MemoryConflictThresh:    0.85, // Similarity threshold for conflict detection
		MemoryTTLDays:           90,   // Memories not accessed in 90 days are eviction candidates

		NeMoGuardClassifyPath: "/v1/classify",
		NeMoGuardMode:         "block",
		NeMoGuardThreshold:    0.5,
		NeMoGuardTimeoutSec:   10,
	}
}

// Load loads configuration from an optional file path, then applies
// environment variable overrides.  If path is "" the file is skipped.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		if err := loadFile(path, &cfg); err != nil {
			return cfg, fmt.Errorf("config file %q: %w", path, err)
		}
		log.Printf("[CONFIG] loaded from %s", path)
	} else {
		log.Printf("[CONFIG] no config file — using defaults + env")
	}

	applyEnv(&cfg)
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("config validation: %w", err)
	}

	cfg.log()
	return cfg, nil
}

// loadFile reads a JSON config file into cfg (unknown keys are silently ignored).
func loadFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // file is optional
		}
		return err
	}
	return json.Unmarshal(data, cfg)
}

// applyEnv overrides cfg fields with environment variables when set.
// Naming convention: CONFIG_<UPPER_FIELD_NAME>, e.g. CONFIG_PORT.
func applyEnv(cfg *Config) {
	setStr := func(env string, dest *string) {
		if v := os.Getenv(env); v != "" {
			*dest = v
		}
	}
	setInt := func(env string, dest *int) {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dest = n
			}
		}
	}
	setFloat := func(env string, dest *float64) {
		if v := os.Getenv(env); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dest = f
			}
		}
	}
	setBool := func(env string, dest *bool) {
		if v := os.Getenv(env); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dest = b
			}
		}
	}

	// Legacy: honour bare PORT and DB_PATH env vars used in earlier versions.
	setStr("PORT", &cfg.Port)
	setStr("DB_PATH", &cfg.DBPath)

	// Namespaced env vars for all fields
	setStr("CONFIG_PORT", &cfg.Port)
	setStr("CONFIG_ALLOWED_ORIGINS", &cfg.AllowedOrigins)
	setStr("CONFIG_DB_PATH", &cfg.DBPath)
	// When CONFIG_DATABASE_URL (a postgres:// DSN) is set, the store uses Postgres
	// instead of the SQLite file above (selection happens in store.Open).
	setStr("CONFIG_DATABASE_URL", &cfg.DatabaseURL)
	setInt("CONFIG_DB_KEEP_DAYS", &cfg.DBKeepDays)
	setInt("CONFIG_UPSTREAM_TIMEOUT_SEC", &cfg.UpstreamTimeoutSec)
	setInt("CONFIG_LOOP_THRESHOLD", &cfg.LoopThreshold)
	setFloat("CONFIG_LOOP_SIMILARITY", &cfg.LoopSimilarity)
	setInt("CONFIG_LOOP_WINDOW_MINUTES", &cfg.LoopWindowMinutes)
	setInt("CONFIG_HISTORY_LIMIT", &cfg.HistoryLimit)
	setInt("CONFIG_SESSION_MAX_AGE", &cfg.SessionMaxAge)
	setInt("CONFIG_SESSION_MAX_SIZE", &cfg.SessionMaxSize)
	setStr("CONFIG_INJECTION_MODE", &cfg.InjectionMode)
	setFloat("CONFIG_CONTENT_THRESHOLD", &cfg.ContentThreshold)
	setBool("CONFIG_RULES_ENABLED", &cfg.RulesEnabled)

	// Retry & Fallback
	setBool("CONFIG_RETRY_ENABLED", &cfg.RetryEnabled)
	setInt("CONFIG_RETRY_MAX_ATTEMPTS", &cfg.RetryMaxAttempts)
	setInt("CONFIG_RETRY_INITIAL_DELAY", &cfg.RetryInitialDelay)
	setInt("CONFIG_RETRY_MAX_DELAY", &cfg.RetryMaxDelay)
	setBool("CONFIG_FALLBACK_ENABLED", &cfg.FallbackEnabled)
	setStr("CONFIG_FALLBACK_STRATEGY", &cfg.FallbackStrategy)

	// Authentication
	setBool("CONFIG_AUTH_ENABLED", &cfg.AuthEnabled)

	// Response Cache
	setBool("CONFIG_CACHE_ENABLED", &cfg.CacheEnabled)
	setInt("CONFIG_CACHE_MAX_ENTRIES", &cfg.CacheMaxEntries)
	setInt("CONFIG_CACHE_TTL_MINUTES", &cfg.CacheTTLMinutes)
	setBool("CONFIG_CACHE_ONLY_TEMP0", &cfg.CacheOnlyTemp0)

	// Security — PII Redaction
	setBool("CONFIG_PII_ENABLED", &cfg.PIIEnabled)
	setStr("CONFIG_PII_MODE", &cfg.PIIMode)
	setStr("CONFIG_PII_CATEGORIES", &cfg.PIICategories)

	// Semantic Cache
	setBool("CONFIG_SEMANTIC_CACHE_ENABLED", &cfg.SemanticCacheEnabled)
	setFloat("CONFIG_SEMANTIC_CACHE_THRESHOLD", &cfg.SemanticCacheThreshold)
	setInt("CONFIG_SEMANTIC_CACHE_MAX_VECTORS", &cfg.SemanticCacheMaxVectors)
	setStr("CONFIG_EMBEDDING_PROVIDER", &cfg.EmbeddingProvider)
	setStr("CONFIG_EMBEDDING_MODEL", &cfg.EmbeddingModel)
	setStr("CONFIG_EMBEDDING_API_KEY", &cfg.EmbeddingAPIKey)
	setStr("CONFIG_EMBEDDING_BASE_URL", &cfg.EmbeddingBaseURL)
	setFloat("CONFIG_SEMANTIC_CACHE_SPARSE_WEIGHT", &cfg.SemanticCacheSparseWeight)

	// Pricing
	setBool("CONFIG_PRICING_OPENROUTER_AUTO", &cfg.PricingOpenRouterAutoRefresh)

	// Memory Layer
	setBool("CONFIG_MEMORY_ENABLED", &cfg.MemoryEnabled)
	setInt("CONFIG_MEMORY_MAX_ENTRIES", &cfg.MemoryMaxEntries)
	setFloat("CONFIG_MEMORY_THRESHOLD", &cfg.MemoryThreshold)
	setInt("CONFIG_MEMORY_MAX_RESULTS", &cfg.MemoryMaxResults)
	setFloat("CONFIG_MEMORY_RECENCY_LAMBDA", &cfg.MemoryRecencyLambda)
	setFloat("CONFIG_MEMORY_CONFLICT_THRESHOLD", &cfg.MemoryConflictThresh)
	setInt("CONFIG_MEMORY_TTL_DAYS", &cfg.MemoryTTLDays)

	// NeMo Guard (jailbreak detection) — presence of the URL enables the feature
	setStr("CONFIG_NEMOGUARD_URL", &cfg.NeMoGuardURL)
	setStr("CONFIG_NEMOGUARD_CLASSIFY_PATH", &cfg.NeMoGuardClassifyPath)
	setStr("CONFIG_NEMOGUARD_API_KEY", &cfg.NeMoGuardAPIKey)
	setStr("CONFIG_NEMOGUARD_MODE", &cfg.NeMoGuardMode)
	setFloat("CONFIG_NEMOGUARD_THRESHOLD", &cfg.NeMoGuardThreshold)
	setInt("CONFIG_NEMOGUARD_TIMEOUT_SEC", &cfg.NeMoGuardTimeoutSec)
}

// validate returns an error if any config value is out of range.
func (cfg *Config) validate() error {
	var errs []string
	if cfg.LoopSimilarity < 0 || cfg.LoopSimilarity > 1 {
		errs = append(errs, "loop_similarity must be 0.0–1.0")
	}
	if cfg.LoopThreshold < 1 {
		errs = append(errs, "loop_threshold must be >= 1")
	}
	if cfg.DBKeepDays < 1 {
		errs = append(errs, "db_keep_days must be >= 1")
	}
	if cfg.HistoryLimit < 1 {
		errs = append(errs, "history_limit must be >= 1")
	}
	if cfg.UpstreamTimeoutSec < 1 {
		errs = append(errs, "upstream_timeout_sec must be >= 1")
	}
	if cfg.NeMoGuardURL != "" {
		if cfg.NeMoGuardMode != "block" && cfg.NeMoGuardMode != "flag" {
			errs = append(errs, `nemo_guard_mode must be "block" or "flag"`)
		}
		if cfg.NeMoGuardThreshold < 0 || cfg.NeMoGuardThreshold > 1 {
			errs = append(errs, "nemo_guard_threshold must be 0.0–1.0")
		}
		if cfg.NeMoGuardTimeoutSec < 1 || cfg.NeMoGuardTimeoutSec > 60 {
			errs = append(errs, "nemo_guard_timeout_sec must be 1–60")
		}
	}
	// Validate runtime-tunable settings via the shared function
	errs = append(errs, validateSettings(cfg.RetryMaxAttempts, cfg.RetryInitialDelay,
		cfg.RetryMaxDelay, cfg.FallbackStrategy, cfg.LoopThreshold,
		cfg.LoopSimilarity, cfg.LoopWindowMinutes, cfg.InjectionMode,
		cfg.ContentThreshold, cfg.CacheMaxEntries, cfg.CacheTTLMinutes,
		cfg.PIIMode, cfg.SemanticCacheThreshold, cfg.SemanticCacheMaxVectors,
		cfg.MemoryThreshold, cfg.MemoryMaxEntries, cfg.MemoryMaxResults,
		cfg.MemoryRecencyLambda, cfg.MemoryConflictThresh, cfg.MemoryTTLDays)...)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// maskAPIKey returns a masked version of an API key for safe display.
// Shows "sk-...last4" or "(not set)" if empty.
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****" + key[len(key)-2:]
	}
	return key[:3] + "..." + key[len(key)-4:]
}

// validateSettings validates the runtime-tunable settings shared between
// Config.validate() and SettingsAPI.HandlePut(). Single source of truth.
func validateSettings(
	retryMaxAttempts, retryInitialDelay, retryMaxDelay int,
	fallbackStrategy string,
	loopThreshold int, loopSimilarity float64, loopWindowMinutes int,
	injectionMode string, contentThreshold float64,
	cacheMaxEntries, cacheTTLMinutes int,
	piiMode string,
	semanticCacheThreshold float64, semanticCacheMaxVectors int,
	memoryThreshold float64, memoryMaxEntries, memoryMaxResults int,
	memoryRecencyLambda, memoryConflictThresh float64, memoryTTLDays int,
) []string {
	var errs []string
	if retryMaxAttempts < 1 || retryMaxAttempts > 10 {
		errs = append(errs, "retry_max_attempts must be 1–10")
	}
	if retryInitialDelay < 100 {
		errs = append(errs, "retry_initial_delay_ms must be >= 100")
	}
	if retryMaxDelay < retryInitialDelay {
		errs = append(errs, "retry_max_delay_ms must be >= retry_initial_delay_ms")
	}
	if fallbackStrategy != "any" && fallbackStrategy != "same_tier" && fallbackStrategy != "cheaper" {
		errs = append(errs, `fallback_strategy must be "any", "same_tier", or "cheaper"`)
	}
	if loopThreshold < 1 || loopThreshold > 20 {
		errs = append(errs, "loop_threshold must be 1–20")
	}
	if loopSimilarity < 0 || loopSimilarity > 1 {
		errs = append(errs, "loop_similarity must be 0.0–1.0")
	}
	if loopWindowMinutes < 1 || loopWindowMinutes > 60 {
		errs = append(errs, "loop_window_minutes must be 1–60")
	}
	if injectionMode != "metadata" && injectionMode != "content" && injectionMode != "hybrid" {
		errs = append(errs, `injection_mode must be "metadata", "content", or "hybrid"`)
	}
	if contentThreshold < 0 {
		errs = append(errs, "content_threshold_usd must be >= 0")
	}
	if cacheMaxEntries < 10 || cacheMaxEntries > 100000 {
		errs = append(errs, "cache_max_entries must be 10–100000")
	}
	if cacheTTLMinutes < 1 || cacheTTLMinutes > 10080 {
		errs = append(errs, "cache_ttl_minutes must be 1–10080")
	}
	if piiMode != "" && piiMode != "redact" && piiMode != "hash" && piiMode != "placeholder" {
		errs = append(errs, `pii_mode must be "redact", "hash", or "placeholder"`)
	}
	if semanticCacheThreshold < 0.5 || semanticCacheThreshold > 1.0 {
		errs = append(errs, "semantic_cache_threshold must be 0.5–1.0")
	}
	if semanticCacheMaxVectors < 100 || semanticCacheMaxVectors > 500000 {
		errs = append(errs, "semantic_cache_max_vectors must be 100–500000")
	}
	if memoryThreshold < 0.3 || memoryThreshold > 1.0 {
		errs = append(errs, "memory_threshold must be 0.3–1.0")
	}
	if memoryMaxEntries < 100 || memoryMaxEntries > 500000 {
		errs = append(errs, "memory_max_entries must be 100–500000")
	}
	if memoryMaxResults < 1 || memoryMaxResults > 20 {
		errs = append(errs, "memory_max_results must be 1–20")
	}
	if memoryRecencyLambda < 0 || memoryRecencyLambda > 0.1 {
		errs = append(errs, "memory_recency_lambda must be 0–0.1")
	}
	if memoryConflictThresh < 0.5 || memoryConflictThresh > 0.99 {
		errs = append(errs, "memory_conflict_threshold must be 0.5–0.99")
	}
	if memoryTTLDays < 7 || memoryTTLDays > 365 {
		errs = append(errs, "memory_ttl_days must be 7–365")
	}
	return errs
}

// UpstreamTimeout converts the timeout setting to a time.Duration.
func (cfg *Config) UpstreamTimeout() time.Duration {
	return time.Duration(cfg.UpstreamTimeoutSec) * time.Second
}

// NeMoGuardTimeout converts the NeMo Guard timeout setting to a time.Duration.
func (cfg *Config) NeMoGuardTimeout() time.Duration {
	return time.Duration(cfg.NeMoGuardTimeoutSec) * time.Second
}

// SessionMaxAgeDuration converts the max-age setting to a time.Duration.
func (cfg *Config) SessionMaxAgeDuration() time.Duration {
	return time.Duration(cfg.SessionMaxAge) * time.Hour
}

// LoopWindowDuration converts the loop window setting to a time.Duration.
func (cfg *Config) LoopWindowDuration() time.Duration {
	return time.Duration(cfg.LoopWindowMinutes) * time.Minute
}

// AllowedOriginList returns the allowed origins as a slice.
func (cfg *Config) AllowedOriginList() []string {
	if cfg.AllowedOrigins == "*" {
		return nil // nil means allow all
	}
	var origins []string
	for _, o := range strings.Split(cfg.AllowedOrigins, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

// log prints a startup summary of the active configuration.
func (cfg *Config) log() {
	log.Printf("[CONFIG] port=%s db=%s keep=%dd loop={threshold=%d sim=%.0f%% window=%dm} session={maxAge=%dh maxSize=%d} injection=%s retry={enabled=%v max=%d delay=%d-%dms} fallback={enabled=%v strategy=%s} auth=%v pii={enabled=%v mode=%s}",
		cfg.Port, cfg.DBPath, cfg.DBKeepDays,
		cfg.LoopThreshold, cfg.LoopSimilarity*100, cfg.LoopWindowMinutes,
		cfg.SessionMaxAge, cfg.SessionMaxSize,
		cfg.InjectionMode,
		cfg.RetryEnabled, cfg.RetryMaxAttempts, cfg.RetryInitialDelay, cfg.RetryMaxDelay,
		cfg.FallbackEnabled, cfg.FallbackStrategy,
		cfg.AuthEnabled,
		cfg.PIIEnabled, cfg.PIIMode,
	)
}

// SettingsResponse is the JSON subset of Config exposed to the settings UI.
// Only fields that the user should be able to tweak at runtime are included.
type SettingsResponse struct {
	RetryEnabled            bool    `json:"retry_enabled"`
	RetryMaxAttempts        int     `json:"retry_max_attempts"`
	RetryInitialDelay       int     `json:"retry_initial_delay_ms"`
	RetryMaxDelay           int     `json:"retry_max_delay_ms"`
	FallbackEnabled         bool    `json:"fallback_enabled"`
	FallbackStrategy        string  `json:"fallback_strategy"`
	LoopThreshold           int     `json:"loop_threshold"`
	LoopSimilarity          float64 `json:"loop_similarity"` // 0–1
	LoopWindowMinutes       int     `json:"loop_window_minutes"`
	InjectionMode           string  `json:"injection_mode"`        // "metadata" | "content" | "hybrid"
	ContentThreshold        float64 `json:"content_threshold_usd"` // Cost to escalate to content injection
	CacheEnabled            bool    `json:"cache_enabled"`
	CacheMaxEntries         int     `json:"cache_max_entries"`
	CacheTTLMinutes         int     `json:"cache_ttl_minutes"`
	CacheOnlyTemp0          bool    `json:"cache_only_temp0"`
	PIIEnabled              bool    `json:"pii_enabled"`
	PIIMode                 string  `json:"pii_mode"`
	PIICategories           string  `json:"pii_categories"`
	SemanticCacheEnabled    bool    `json:"semantic_cache_enabled"`
	SemanticCacheThreshold  float64 `json:"semantic_cache_threshold"`
	SemanticCacheMaxVectors int     `json:"semantic_cache_max_vectors"`
	EmbeddingProvider       string  `json:"embedding_provider"`
	EmbeddingModel          string  `json:"embedding_model"`
	EmbeddingAPIKey         string  `json:"embedding_api_key"`
	EmbeddingBaseURL        string  `json:"embedding_base_url"`
	SemanticCacheSparseWeight float64 `json:"semantic_cache_sparse_weight"`
	MemoryEnabled           bool    `json:"memory_enabled"`
	MemoryMaxEntries        int     `json:"memory_max_entries"`
	MemoryThreshold         float64 `json:"memory_threshold"`
	MemoryMaxResults        int     `json:"memory_max_results"`
	MemoryRecencyLambda     float64 `json:"memory_recency_lambda"`
	MemoryConflictThresh    float64 `json:"memory_conflict_threshold"`
	MemoryTTLDays           int     `json:"memory_ttl_days"`
}

// SettingsPersister is the interface for persisting settings to a database.
// Implemented by store.DB.
type SettingsPersister interface {
	SaveSettings(jsonData []byte) error
	LoadSettings() ([]byte, error)
}

// SettingsAPI handles GET/PUT /api/settings for the dashboard.
// It wraps a pointer to the live Config so changes take effect immediately.
type SettingsAPI struct {
	mu  sync.RWMutex
	cfg *Config
	// onChange is called after a successful settings update.
	// The handler (or main) can wire this to rebuild retry configs, etc.
	onChange func(*Config)
	// db persists settings across restarts. May be nil (no persistence).
	db SettingsPersister
}

// NewSettingsAPI creates a SettingsAPI wrapping the given config pointer.
// onChange may be nil; if set it is called after every successful PUT.
// db may be nil; if set, settings are persisted to the database.
func NewSettingsAPI(cfg *Config, onChange func(*Config), db SettingsPersister) *SettingsAPI {
	return &SettingsAPI{cfg: cfg, onChange: onChange, db: db}
}

// SetOnChange sets or replaces the onChange callback.
// Use this when components (like memory store) are created after NewSettingsAPI.
func (s *SettingsAPI) SetOnChange(fn func(*Config)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// HandleGet returns the current settings.
func (s *SettingsAPI) HandleGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	resp := SettingsResponse{
		RetryEnabled:            s.cfg.RetryEnabled,
		RetryMaxAttempts:        s.cfg.RetryMaxAttempts,
		RetryInitialDelay:       s.cfg.RetryInitialDelay,
		RetryMaxDelay:           s.cfg.RetryMaxDelay,
		FallbackEnabled:         s.cfg.FallbackEnabled,
		FallbackStrategy:        s.cfg.FallbackStrategy,
		LoopThreshold:           s.cfg.LoopThreshold,
		LoopSimilarity:          s.cfg.LoopSimilarity,
		LoopWindowMinutes:       s.cfg.LoopWindowMinutes,
		InjectionMode:           s.cfg.InjectionMode,
		ContentThreshold:        s.cfg.ContentThreshold,
		CacheEnabled:            s.cfg.CacheEnabled,
		CacheMaxEntries:         s.cfg.CacheMaxEntries,
		CacheTTLMinutes:         s.cfg.CacheTTLMinutes,
		CacheOnlyTemp0:          s.cfg.CacheOnlyTemp0,
		PIIEnabled:              s.cfg.PIIEnabled,
		PIIMode:                 s.cfg.PIIMode,
		PIICategories:           s.cfg.PIICategories,
		SemanticCacheEnabled:    s.cfg.SemanticCacheEnabled,
		SemanticCacheThreshold:  s.cfg.SemanticCacheThreshold,
		SemanticCacheMaxVectors: s.cfg.SemanticCacheMaxVectors,
		EmbeddingProvider:       s.cfg.EmbeddingProvider,
		EmbeddingModel:          s.cfg.EmbeddingModel,
		EmbeddingAPIKey:         maskAPIKey(s.cfg.EmbeddingAPIKey),
		EmbeddingBaseURL:        s.cfg.EmbeddingBaseURL,
		SemanticCacheSparseWeight: s.cfg.SemanticCacheSparseWeight,
		MemoryEnabled:           s.cfg.MemoryEnabled,
		MemoryMaxEntries:        s.cfg.MemoryMaxEntries,
		MemoryThreshold:         s.cfg.MemoryThreshold,
		MemoryMaxResults:        s.cfg.MemoryMaxResults,
		MemoryRecencyLambda:     s.cfg.MemoryRecencyLambda,
		MemoryConflictThresh:    s.cfg.MemoryConflictThresh,
		MemoryTTLDays:           s.cfg.MemoryTTLDays,
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandlePut applies new settings from the request body.
func (s *SettingsAPI) HandlePut(w http.ResponseWriter, r *http.Request) {
	var incoming SettingsResponse
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate using the shared validation function (single source of truth)
	errs := validateSettings(
		incoming.RetryMaxAttempts, incoming.RetryInitialDelay, incoming.RetryMaxDelay,
		incoming.FallbackStrategy,
		incoming.LoopThreshold, incoming.LoopSimilarity, incoming.LoopWindowMinutes,
		incoming.InjectionMode, incoming.ContentThreshold,
		incoming.CacheMaxEntries, incoming.CacheTTLMinutes,
		incoming.PIIMode,
		incoming.SemanticCacheThreshold, incoming.SemanticCacheMaxVectors,
		incoming.MemoryThreshold, incoming.MemoryMaxEntries, incoming.MemoryMaxResults,
		incoming.MemoryRecencyLambda, incoming.MemoryConflictThresh, incoming.MemoryTTLDays,
	)
	if len(errs) > 0 {
		http.Error(w, strings.Join(errs, "; "), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.cfg.RetryEnabled = incoming.RetryEnabled
	s.cfg.RetryMaxAttempts = incoming.RetryMaxAttempts
	s.cfg.RetryInitialDelay = incoming.RetryInitialDelay
	s.cfg.RetryMaxDelay = incoming.RetryMaxDelay
	s.cfg.FallbackEnabled = incoming.FallbackEnabled
	s.cfg.FallbackStrategy = incoming.FallbackStrategy
	s.cfg.LoopThreshold = incoming.LoopThreshold
	s.cfg.LoopSimilarity = incoming.LoopSimilarity
	s.cfg.LoopWindowMinutes = incoming.LoopWindowMinutes
	s.cfg.InjectionMode = incoming.InjectionMode
	s.cfg.ContentThreshold = incoming.ContentThreshold
	s.cfg.CacheEnabled = incoming.CacheEnabled
	s.cfg.CacheMaxEntries = incoming.CacheMaxEntries
	s.cfg.CacheTTLMinutes = incoming.CacheTTLMinutes
	s.cfg.CacheOnlyTemp0 = incoming.CacheOnlyTemp0
	s.cfg.PIIEnabled = incoming.PIIEnabled
	s.cfg.PIIMode = incoming.PIIMode
	s.cfg.PIICategories = incoming.PIICategories
	s.cfg.SemanticCacheEnabled = incoming.SemanticCacheEnabled
	s.cfg.SemanticCacheThreshold = incoming.SemanticCacheThreshold
	s.cfg.SemanticCacheMaxVectors = incoming.SemanticCacheMaxVectors
	if incoming.EmbeddingProvider != "" {
		s.cfg.EmbeddingProvider = incoming.EmbeddingProvider
	}
	if incoming.EmbeddingModel != "" {
		s.cfg.EmbeddingModel = incoming.EmbeddingModel
	}
	// Only update API key if user provided a real new key (not empty, not the masked version)
	if incoming.EmbeddingAPIKey != "" && !strings.Contains(incoming.EmbeddingAPIKey, "...") {
		s.cfg.EmbeddingAPIKey = incoming.EmbeddingAPIKey
	}
	if incoming.EmbeddingBaseURL != "" {
		s.cfg.EmbeddingBaseURL = incoming.EmbeddingBaseURL
	}
	s.cfg.SemanticCacheSparseWeight = incoming.SemanticCacheSparseWeight
	s.cfg.MemoryEnabled = incoming.MemoryEnabled
	s.cfg.MemoryMaxEntries = incoming.MemoryMaxEntries
	s.cfg.MemoryThreshold = incoming.MemoryThreshold
	s.cfg.MemoryMaxResults = incoming.MemoryMaxResults
	s.cfg.MemoryRecencyLambda = incoming.MemoryRecencyLambda
	s.cfg.MemoryConflictThresh = incoming.MemoryConflictThresh
	s.cfg.MemoryTTLDays = incoming.MemoryTTLDays
	s.mu.Unlock()

	log.Printf("[SETTINGS] updated: retry={enabled=%v max=%d delay=%d-%dms} fallback={enabled=%v strategy=%s} loop={threshold=%d sim=%.0f%% window=%dm} injection={mode=%s threshold=$%.2f} cache={enabled=%v max=%d ttl=%dm temp0=%v} pii={enabled=%v mode=%s}",
		incoming.RetryEnabled, incoming.RetryMaxAttempts, incoming.RetryInitialDelay, incoming.RetryMaxDelay,
		incoming.FallbackEnabled, incoming.FallbackStrategy,
		incoming.LoopThreshold, incoming.LoopSimilarity*100, incoming.LoopWindowMinutes,
		incoming.InjectionMode, incoming.ContentThreshold,
		incoming.CacheEnabled, incoming.CacheMaxEntries, incoming.CacheTTLMinutes, incoming.CacheOnlyTemp0,
		incoming.PIIEnabled, incoming.PIIMode,
	)

	// Persist to database so settings survive restarts.
	// Snapshot the live config values (which have the real API key) for persistence.
	if s.db != nil {
		s.mu.RLock()
		persistable := incoming
		persistable.EmbeddingProvider = s.cfg.EmbeddingProvider
		persistable.EmbeddingModel = s.cfg.EmbeddingModel
		persistable.EmbeddingAPIKey = s.cfg.EmbeddingAPIKey
		s.mu.RUnlock()
		if jsonData, err := json.Marshal(persistable); err == nil {
			if err := s.db.SaveSettings(jsonData); err != nil {
				log.Printf("[SETTINGS] ⚠ failed to persist settings: %v", err)
			} else {
				log.Printf("[SETTINGS] persisted to database")
			}
		}
	}

	if s.onChange != nil {
		s.onChange(s.cfg)
	}

	// Return response with masked API key
	s.mu.RLock()
	incoming.EmbeddingAPIKey = maskAPIKey(s.cfg.EmbeddingAPIKey)
	incoming.EmbeddingProvider = s.cfg.EmbeddingProvider
	incoming.EmbeddingModel = s.cfg.EmbeddingModel
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(incoming)
}

// LoadFromDB loads persisted settings from the database and applies them
// to the live config. Call this at startup after creating the SettingsAPI.
// Returns true if settings were loaded, false if no persisted settings exist.
func (s *SettingsAPI) LoadFromDB() bool {
	if s.db == nil {
		return false
	}
	data, err := s.db.LoadSettings()
	if err != nil || data == nil {
		return false
	}

	var saved SettingsResponse
	if err := json.Unmarshal(data, &saved); err != nil {
		log.Printf("[SETTINGS] ⚠ failed to parse persisted settings: %v", err)
		return false
	}

	s.mu.Lock()
	s.cfg.RetryEnabled = saved.RetryEnabled
	s.cfg.RetryMaxAttempts = saved.RetryMaxAttempts
	s.cfg.RetryInitialDelay = saved.RetryInitialDelay
	s.cfg.RetryMaxDelay = saved.RetryMaxDelay
	s.cfg.FallbackEnabled = saved.FallbackEnabled
	s.cfg.FallbackStrategy = saved.FallbackStrategy
	s.cfg.LoopThreshold = saved.LoopThreshold
	s.cfg.LoopSimilarity = saved.LoopSimilarity
	s.cfg.LoopWindowMinutes = saved.LoopWindowMinutes
	s.cfg.InjectionMode = saved.InjectionMode
	s.cfg.ContentThreshold = saved.ContentThreshold
	s.cfg.CacheEnabled = saved.CacheEnabled
	s.cfg.CacheMaxEntries = saved.CacheMaxEntries
	s.cfg.CacheTTLMinutes = saved.CacheTTLMinutes
	s.cfg.CacheOnlyTemp0 = saved.CacheOnlyTemp0
	s.cfg.PIIEnabled = saved.PIIEnabled
	s.cfg.PIIMode = saved.PIIMode
	s.cfg.PIICategories = saved.PIICategories
	s.cfg.SemanticCacheEnabled = saved.SemanticCacheEnabled
	s.cfg.SemanticCacheThreshold = saved.SemanticCacheThreshold
	s.cfg.SemanticCacheMaxVectors = saved.SemanticCacheMaxVectors
	if saved.EmbeddingProvider != "" {
		s.cfg.EmbeddingProvider = saved.EmbeddingProvider
	}
	if saved.EmbeddingModel != "" {
		s.cfg.EmbeddingModel = saved.EmbeddingModel
	}
	if saved.EmbeddingAPIKey != "" {
		s.cfg.EmbeddingAPIKey = saved.EmbeddingAPIKey
	}
	if saved.EmbeddingBaseURL != "" {
		s.cfg.EmbeddingBaseURL = saved.EmbeddingBaseURL
	}
	s.cfg.SemanticCacheSparseWeight = saved.SemanticCacheSparseWeight
	s.cfg.MemoryEnabled = saved.MemoryEnabled
	s.cfg.MemoryMaxEntries = saved.MemoryMaxEntries
	s.cfg.MemoryThreshold = saved.MemoryThreshold
	s.cfg.MemoryMaxResults = saved.MemoryMaxResults
	if saved.MemoryRecencyLambda > 0 {
		s.cfg.MemoryRecencyLambda = saved.MemoryRecencyLambda
	}
	if saved.MemoryConflictThresh > 0 {
		s.cfg.MemoryConflictThresh = saved.MemoryConflictThresh
	}
	if saved.MemoryTTLDays > 0 {
		s.cfg.MemoryTTLDays = saved.MemoryTTLDays
	}
	s.mu.Unlock()

	log.Printf("[SETTINGS] restored from database")
	return true
}
