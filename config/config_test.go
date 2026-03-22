package config

import (
	"os"
	"testing"
	"time"
)

// ── Default tests ─────────────────────────────────────────────────────────

func TestDefault_Values(t *testing.T) {
	cfg := Default()

	checks := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"Port", cfg.Port, "8081"},
		{"DBPath", cfg.DBPath, "proxy.db"},
		{"DBKeepDays", cfg.DBKeepDays, 30},
		{"UpstreamTimeoutSec", cfg.UpstreamTimeoutSec, 300},
		{"LoopThreshold", cfg.LoopThreshold, 3},
		{"LoopSimilarity", cfg.LoopSimilarity, 0.8},
		{"LoopWindowMinutes", cfg.LoopWindowMinutes, 5},
		{"RetryEnabled", cfg.RetryEnabled, true},
		{"RetryMaxAttempts", cfg.RetryMaxAttempts, 3},
		{"FallbackEnabled", cfg.FallbackEnabled, false},
		{"FallbackStrategy", cfg.FallbackStrategy, "same_tier"},
		{"AuthEnabled", cfg.AuthEnabled, false},
		{"CacheEnabled", cfg.CacheEnabled, true},
		{"CacheMaxEntries", cfg.CacheMaxEntries, 1000},
		{"CacheTTLMinutes", cfg.CacheTTLMinutes, 60},
		{"CacheOnlyTemp0", cfg.CacheOnlyTemp0, true},
		{"PIIEnabled", cfg.PIIEnabled, false},
		{"PIIMode", cfg.PIIMode, "redact"},
		{"InjectionMode", cfg.InjectionMode, "metadata"},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Default().%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestDefault_Validates(t *testing.T) {
	cfg := Default()
	if err := cfg.validate(); err != nil {
		t.Errorf("Default() should pass validation, got: %v", err)
	}
}

// ── Load tests ────────────────────────────────────────────────────────────

func TestLoad_NoFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(''): %v", err)
	}
	if cfg.Port != "8081" {
		t.Errorf("Port: got %q, want %q", cfg.Port, "8081")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	// Non-existent file should be silently ignored
	cfg, err := Load(t.TempDir() + "/nonexistent.json")
	if err != nil {
		t.Fatalf("Load(nonexistent): %v", err)
	}
	if cfg.Port != "8081" {
		t.Errorf("Port: got %q, want defaults", cfg.Port)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	path := t.TempDir() + "/config.json"
	os.WriteFile(path, []byte(`{"port":"9090","db_keep_days":7}`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port: got %q, want %q", cfg.Port, "9090")
	}
	if cfg.DBKeepDays != 7 {
		t.Errorf("DBKeepDays: got %d, want 7", cfg.DBKeepDays)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	path := t.TempDir() + "/bad.json"
	os.WriteFile(path, []byte(`{invalid}`), 0644)

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid JSON file")
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := t.TempDir() + "/config.json"
	os.WriteFile(path, []byte(`{"port":"9090"}`), 0644)

	t.Setenv("CONFIG_PORT", "7777")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "7777" {
		t.Errorf("env should override file: got %q, want %q", cfg.Port, "7777")
	}
}

func TestLoad_LegacyEnvVars(t *testing.T) {
	t.Setenv("PORT", "3000")
	t.Setenv("DB_PATH", "/tmp/test.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "3000" {
		t.Errorf("legacy PORT: got %q, want %q", cfg.Port, "3000")
	}
	if cfg.DBPath != "/tmp/test.db" {
		t.Errorf("legacy DB_PATH: got %q, want %q", cfg.DBPath, "/tmp/test.db")
	}
}

func TestLoad_EnvBoolParsing(t *testing.T) {
	t.Setenv("CONFIG_AUTH_ENABLED", "true")
	t.Setenv("CONFIG_FALLBACK_ENABLED", "1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AuthEnabled {
		t.Error("CONFIG_AUTH_ENABLED=true should set AuthEnabled=true")
	}
	if !cfg.FallbackEnabled {
		t.Error("CONFIG_FALLBACK_ENABLED=1 should set FallbackEnabled=true")
	}
}

func TestLoad_EnvFloatParsing(t *testing.T) {
	t.Setenv("CONFIG_LOOP_SIMILARITY", "0.95")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LoopSimilarity != 0.95 {
		t.Errorf("LoopSimilarity: got %f, want 0.95", cfg.LoopSimilarity)
	}
}

func TestLoad_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("CONFIG_DB_KEEP_DAYS", "not_a_number")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Should keep default
	if cfg.DBKeepDays != 30 {
		t.Errorf("invalid env int should be ignored: got %d, want 30", cfg.DBKeepDays)
	}
}

// ── Validation tests ──────────────────────────────────────────────────────

func TestValidate_LoopSimilarity(t *testing.T) {
	cfg := Default()
	cfg.LoopSimilarity = 1.5
	if err := cfg.validate(); err == nil {
		t.Error("expected error for loop_similarity > 1")
	}

	cfg.LoopSimilarity = -0.1
	if err := cfg.validate(); err == nil {
		t.Error("expected error for loop_similarity < 0")
	}
}

func TestValidate_DBKeepDays(t *testing.T) {
	cfg := Default()
	cfg.DBKeepDays = 0
	if err := cfg.validate(); err == nil {
		t.Error("expected error for db_keep_days < 1")
	}
}

func TestValidate_RetryAttempts(t *testing.T) {
	cfg := Default()
	cfg.RetryMaxAttempts = 0
	if err := cfg.validate(); err == nil {
		t.Error("expected error for retry_max_attempts < 1")
	}
	cfg.RetryMaxAttempts = 11
	if err := cfg.validate(); err == nil {
		t.Error("expected error for retry_max_attempts > 10")
	}
}

func TestValidate_FallbackStrategy(t *testing.T) {
	cfg := Default()
	cfg.FallbackStrategy = "invalid"
	if err := cfg.validate(); err == nil {
		t.Error("expected error for invalid fallback_strategy")
	}

	for _, valid := range []string{"any", "same_tier", "cheaper"} {
		cfg.FallbackStrategy = valid
		// Reset other fields to valid
		cfg2 := Default()
		cfg2.FallbackStrategy = valid
		if err := cfg2.validate(); err != nil {
			t.Errorf("fallback_strategy=%q should be valid, got: %v", valid, err)
		}
	}
}

func TestValidate_CacheMaxEntries(t *testing.T) {
	cfg := Default()
	cfg.CacheMaxEntries = 5 // below 10
	if err := cfg.validate(); err == nil {
		t.Error("expected error for cache_max_entries < 10")
	}
	cfg.CacheMaxEntries = 200000 // above 100000
	if err := cfg.validate(); err == nil {
		t.Error("expected error for cache_max_entries > 100000")
	}
}

func TestValidate_PIIMode(t *testing.T) {
	cfg := Default()
	cfg.PIIMode = "invalid"
	if err := cfg.validate(); err == nil {
		t.Error("expected error for invalid pii_mode")
	}

	for _, valid := range []string{"redact", "hash", "placeholder"} {
		cfg2 := Default()
		cfg2.PIIMode = valid
		if err := cfg2.validate(); err != nil {
			t.Errorf("pii_mode=%q should be valid, got: %v", valid, err)
		}
	}
}

func TestValidate_InjectionMode(t *testing.T) {
	cfg := Default()
	cfg.InjectionMode = "invalid"
	if err := cfg.validate(); err == nil {
		t.Error("expected error for invalid injection_mode")
	}
}

// ── Duration helpers ──────────────────────────────────────────────────────

func TestUpstreamTimeout(t *testing.T) {
	cfg := Default() // 300 sec
	if cfg.UpstreamTimeout() != 300*time.Second {
		t.Errorf("UpstreamTimeout: got %v, want 5m", cfg.UpstreamTimeout())
	}
}

func TestSessionMaxAgeDuration(t *testing.T) {
	cfg := Default() // 24 hours
	if cfg.SessionMaxAgeDuration() != 24*time.Hour {
		t.Errorf("SessionMaxAgeDuration: got %v, want 24h", cfg.SessionMaxAgeDuration())
	}
}

func TestLoopWindowDuration(t *testing.T) {
	cfg := Default() // 5 minutes
	if cfg.LoopWindowDuration() != 5*time.Minute {
		t.Errorf("LoopWindowDuration: got %v, want 5m", cfg.LoopWindowDuration())
	}
}

// ── AllowedOriginList ─────────────────────────────────────────────────────

func TestAllowedOriginList_Wildcard(t *testing.T) {
	cfg := Default() // "*"
	origins := cfg.AllowedOriginList()
	if origins != nil {
		t.Errorf("wildcard should return nil, got %v", origins)
	}
}

func TestAllowedOriginList_Multiple(t *testing.T) {
	cfg := Default()
	cfg.AllowedOrigins = "http://localhost:3000, http://example.com"
	origins := cfg.AllowedOriginList()
	if len(origins) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(origins))
	}
	if origins[0] != "http://localhost:3000" {
		t.Errorf("origins[0]: got %q", origins[0])
	}
	if origins[1] != "http://example.com" {
		t.Errorf("origins[1]: got %q", origins[1])
	}
}

func TestAllowedOriginList_Empty(t *testing.T) {
	cfg := Default()
	cfg.AllowedOrigins = ""
	origins := cfg.AllowedOriginList()
	if len(origins) != 0 {
		t.Errorf("empty string should return empty slice, got %v", origins)
	}
}
