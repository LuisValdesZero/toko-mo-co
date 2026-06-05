// Package providers manages user-defined custom LLM provider endpoints.
// Custom providers allow routing to self-hosted models (Ollama, vLLM) or
// third-party APIs (DeepSeek, Mistral, Groq) via model prefix routing.
package providers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"tokomoco/store"
)

// reservedNames are provider names that cannot be used for custom providers.
// These correspond to the built-in providers whose routes are hardcoded.
var reservedNames = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true, "google": true,
}

// validNameRE ensures provider names are lowercase alphanumeric + hyphens,
// starting and ending with alphanumeric, 2-50 chars total.
var validNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,48}[a-z0-9]$`)

// validFormats are the supported API formats for custom providers.
var validFormats = map[string]bool{
	"openai":    true,
	"anthropic": true,
}

// CustomProvider represents a user-defined LLM provider endpoint.
type CustomProvider struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`         // routing key: "ollama", "deepseek"
	DisplayName string   `json:"display_name"` // "Ollama (Local)"
	BaseURL     string   `json:"base_url"`     // "http://localhost:11434"
	APIFormat   string   `json:"api_format"`   // "openai" | "anthropic"
	APIPath     string   `json:"api_path"`     // "/v1/chat/completions"
	AuthHeader  string   `json:"auth_header"`  // raw auth header value (optional)
	AuthEnvVar  string   `json:"auth_env_var"` // env var name for API key (optional)
	Models      []string `json:"models"`       // known model names
	Enabled     bool     `json:"enabled"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

// UpstreamURL constructs the full URL for a request to this provider.
func (cp *CustomProvider) UpstreamURL() string {
	base := strings.TrimRight(cp.BaseURL, "/")
	path := cp.APIPath
	if path == "" {
		path = "/v1/chat/completions"
	}
	return base + path
}

// ResolveAuthHeader returns the authorization header value to send upstream.
// Priority: env var > stored header. Returns "" if no auth is configured.
func (cp *CustomProvider) ResolveAuthHeader() string {
	if cp.AuthEnvVar != "" {
		if val := os.Getenv(cp.AuthEnvVar); val != "" {
			// If the env var value already starts with "Bearer ", use as-is
			if strings.HasPrefix(val, "Bearer ") {
				return val
			}
			return "Bearer " + val
		}
	}
	if cp.AuthHeader != "" {
		return cp.AuthHeader
	}
	return ""
}

// ProviderStore provides DB-backed CRUD with an in-memory cache for fast lookups.
// The cache is keyed by provider name (lowercase) and only contains enabled providers.
// Thread-safe via RWMutex — reads (hot path) use RLock, writes use Lock.
type ProviderStore struct {
	db    store.Querier
	mu    sync.RWMutex
	cache map[string]*CustomProvider // name -> provider (enabled only)
}

// NewProviderStore creates a new store and loads all enabled providers into the cache.
// Invalid rows are logged and skipped — never crashes on bad data.
func NewProviderStore(db store.Querier) *ProviderStore {
	ps := &ProviderStore{
		db:    db,
		cache: make(map[string]*CustomProvider),
	}
	ps.ReloadCache()
	return ps
}

// Lookup returns the custom provider for a given name, or nil if not found/disabled.
// This is the hot-path function called on every proxy request — uses RLock only.
func (ps *ProviderStore) Lookup(name string) *CustomProvider {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.cache[name]
}

// AllNames returns a sorted slice of all enabled provider names in the cache.
func (ps *ProviderStore) AllNames() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	names := make([]string, 0, len(ps.cache))
	for name := range ps.cache {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns all custom providers (enabled and disabled) from the database.
func (ps *ProviderStore) All() ([]*CustomProvider, error) {
	rows, err := ps.db.Query(`
		SELECT id, name, display_name, base_url, api_format, api_path,
		       auth_header, auth_env_var, models_json, enabled, created_at, updated_at
		FROM custom_providers
		ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query custom_providers: %w", err)
	}
	defer rows.Close()

	var providers []*CustomProvider
	for rows.Next() {
		cp, err := scanProvider(rows)
		if err != nil {
			log.Printf("[PROVIDERS] warning: skipping row with scan error: %v", err)
			continue
		}
		providers = append(providers, cp)
	}
	return providers, rows.Err()
}

// Get returns a single custom provider by ID.
func (ps *ProviderStore) Get(id int64) (*CustomProvider, error) {
	row := ps.db.QueryRow(`
		SELECT id, name, display_name, base_url, api_format, api_path,
		       auth_header, auth_env_var, models_json, enabled, created_at, updated_at
		FROM custom_providers WHERE id = ?
	`, id)

	cp := &CustomProvider{}
	var modelsJSON string
	var enabled int
	err := row.Scan(&cp.ID, &cp.Name, &cp.DisplayName, &cp.BaseURL, &cp.APIFormat,
		&cp.APIPath, &cp.AuthHeader, &cp.AuthEnvVar, &modelsJSON, &enabled,
		&cp.CreatedAt, &cp.UpdatedAt)
	if err != nil {
		return nil, err
	}
	cp.Enabled = enabled == 1
	json.Unmarshal([]byte(modelsJSON), &cp.Models)
	if cp.Models == nil {
		cp.Models = []string{}
	}
	return cp, nil
}

// Create inserts a new custom provider. Validates before insert.
// Returns the new ID and reloads the cache.
func (ps *ProviderStore) Create(cp *CustomProvider) (int64, error) {
	if err := Validate(cp); err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	modelsJSON, _ := json.Marshal(cp.Models)
	if cp.Models == nil {
		modelsJSON = []byte("[]")
	}

	enabled := 0
	if cp.Enabled {
		enabled = 1
	}

	id, err := ps.db.InsertReturningID(`
		INSERT INTO custom_providers (name, display_name, base_url, api_format, api_path,
		                              auth_header, auth_env_var, models_json, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, cp.Name, cp.DisplayName, cp.BaseURL, cp.APIFormat, cp.APIPath,
		cp.AuthHeader, cp.AuthEnvVar, string(modelsJSON), enabled, now, now)
	if err != nil {
		return 0, fmt.Errorf("insert custom_providers: %w", err)
	}

	ps.ReloadCache()
	log.Printf("[PROVIDERS] created: %s → %s (format=%s)", cp.Name, cp.BaseURL, cp.APIFormat)
	return id, nil
}

// Update modifies an existing custom provider. Validates before update.
func (ps *ProviderStore) Update(cp *CustomProvider) error {
	if err := Validate(cp); err != nil {
		return err
	}

	now := time.Now().Unix()
	modelsJSON, _ := json.Marshal(cp.Models)
	if cp.Models == nil {
		modelsJSON = []byte("[]")
	}

	enabled := 0
	if cp.Enabled {
		enabled = 1
	}

	_, err := ps.db.Exec(`
		UPDATE custom_providers
		SET display_name = ?, base_url = ?, api_format = ?, api_path = ?,
		    auth_header = ?, auth_env_var = ?, models_json = ?, enabled = ?, updated_at = ?
		WHERE id = ?
	`, cp.DisplayName, cp.BaseURL, cp.APIFormat, cp.APIPath,
		cp.AuthHeader, cp.AuthEnvVar, string(modelsJSON), enabled, now, cp.ID)
	if err != nil {
		return fmt.Errorf("update custom_providers: %w", err)
	}

	ps.ReloadCache()
	log.Printf("[PROVIDERS] updated: %s → %s (format=%s)", cp.Name, cp.BaseURL, cp.APIFormat)
	return nil
}

// Delete removes a custom provider by ID.
func (ps *ProviderStore) Delete(id int64) error {
	// Get name for logging before delete
	cp, _ := ps.Get(id)
	name := "unknown"
	if cp != nil {
		name = cp.Name
	}

	_, err := ps.db.Exec(`DELETE FROM custom_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete custom_providers: %w", err)
	}

	ps.ReloadCache()
	log.Printf("[PROVIDERS] deleted: %s (id=%d)", name, id)
	return nil
}

// Toggle enables or disables a custom provider.
func (ps *ProviderStore) Toggle(id int64, enabled bool) error {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}

	now := time.Now().Unix()
	_, err := ps.db.Exec(`
		UPDATE custom_providers SET enabled = ?, updated_at = ? WHERE id = ?
	`, enabledInt, now, id)
	if err != nil {
		return fmt.Errorf("toggle custom_providers: %w", err)
	}

	ps.ReloadCache()

	cp, _ := ps.Get(id)
	name := "unknown"
	if cp != nil {
		name = cp.Name
	}
	log.Printf("[PROVIDERS] toggled: %s enabled=%v", name, enabled)
	return nil
}

// ReloadCache fetches all enabled providers from DB into the in-memory cache.
// Invalid rows are logged and skipped — this function never panics.
func (ps *ProviderStore) ReloadCache() {
	rows, err := ps.db.Query(`
		SELECT id, name, display_name, base_url, api_format, api_path,
		       auth_header, auth_env_var, models_json, enabled, created_at, updated_at
		FROM custom_providers WHERE enabled = 1
	`)
	if err != nil {
		log.Printf("[PROVIDERS] warning: failed to reload cache: %v", err)
		return
	}
	defer rows.Close()

	newCache := make(map[string]*CustomProvider)
	for rows.Next() {
		cp, err := scanProvider(rows)
		if err != nil {
			log.Printf("[PROVIDERS] warning: skipping row with scan error: %v", err)
			continue
		}

		// Validate each loaded row — skip bad configs
		if err := Validate(cp); err != nil {
			log.Printf("[PROVIDERS] warning: skipping invalid provider %q: %v", cp.Name, err)
			continue
		}

		newCache[cp.Name] = cp
	}

	// Atomic swap under write lock
	ps.mu.Lock()
	ps.cache = newCache
	ps.mu.Unlock()
}

// Validate checks a custom provider config for errors. Returns nil if valid.
func Validate(cp *CustomProvider) error {
	var errors []string

	// Name validation
	if cp.Name == "" {
		errors = append(errors, "name is required")
	} else if !validNameRE.MatchString(cp.Name) {
		errors = append(errors, "name must be 2-50 chars, lowercase alphanumeric and hyphens, starting/ending with alphanumeric")
	} else if reservedNames[cp.Name] {
		errors = append(errors, fmt.Sprintf("name %q is reserved (built-in provider)", cp.Name))
	}

	// Base URL validation
	if cp.BaseURL == "" {
		errors = append(errors, "base_url is required")
	} else {
		u, err := url.Parse(cp.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			errors = append(errors, "base_url must be a valid URL with scheme (e.g., http://localhost:11434)")
		}
	}

	// API format validation
	if !validFormats[cp.APIFormat] {
		errors = append(errors, fmt.Sprintf("api_format must be one of: openai, anthropic (got %q)", cp.APIFormat))
	}

	// API path validation
	if cp.APIPath != "" && !strings.HasPrefix(cp.APIPath, "/") {
		errors = append(errors, "api_path must start with /")
	}

	if len(errors) > 0 {
		return fmt.Errorf("validation failed: %s", strings.Join(errors, "; "))
	}
	return nil
}

// scanProvider scans a single row from a custom_providers query.
type scannable interface {
	Scan(dest ...interface{}) error
}

func scanProvider(s scannable) (*CustomProvider, error) {
	cp := &CustomProvider{}
	var modelsJSON string
	var enabled int
	err := s.Scan(&cp.ID, &cp.Name, &cp.DisplayName, &cp.BaseURL, &cp.APIFormat,
		&cp.APIPath, &cp.AuthHeader, &cp.AuthEnvVar, &modelsJSON, &enabled,
		&cp.CreatedAt, &cp.UpdatedAt)
	if err != nil {
		return nil, err
	}
	cp.Enabled = enabled == 1
	json.Unmarshal([]byte(modelsJSON), &cp.Models)
	if cp.Models == nil {
		cp.Models = []string{}
	}
	return cp, nil
}
