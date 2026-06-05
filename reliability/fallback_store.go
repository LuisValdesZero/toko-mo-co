package reliability

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"tokomoco/store"
)

// FallbackOption represents a single fallback target in a chain
type FallbackOption struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
}

// FallbackConfig represents a user-configured fallback mapping
type FallbackConfig struct {
	ID             int64            `json:"id"`
	AgentID        string           `json:"agent_id"`
	SourceProvider string           `json:"source_provider"`
	SourceModel    string           `json:"source_model"`
	FallbackChain  []FallbackOption `json:"fallback_chain"`
	Enabled        bool             `json:"enabled"`
	Priority       int              `json:"priority"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// FallbackStore provides database operations for fallback configurations
type FallbackStore struct {
	db store.Querier
}

// NewFallbackStore creates a new FallbackStore
func NewFallbackStore(db store.Querier) *FallbackStore {
	return &FallbackStore{db: db}
}

// List returns all fallback configurations, optionally filtered by agent_id
func (s *FallbackStore) List(agentID string) ([]*FallbackConfig, error) {
	query := `
		SELECT id, agent_id, source_provider, source_model, fallback_chain, enabled, priority, created_at, updated_at
		FROM fallback_configs`

	var rows *sql.Rows
	var err error

	if agentID != "" {
		// Filter by specific agent or global defaults
		query += ` WHERE agent_id = ? OR agent_id = '' ORDER BY agent_id DESC, priority DESC, source_provider, source_model`
		rows, err = s.db.Query(query, agentID)
	} else {
		query += ` ORDER BY agent_id, priority DESC, source_provider, source_model`
		rows, err = s.db.Query(query)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*FallbackConfig
	for rows.Next() {
		cfg, err := s.scanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, cfg)
	}
	return configs, rows.Err()
}

// Get retrieves a specific fallback configuration by ID
func (s *FallbackStore) Get(id int64) (*FallbackConfig, error) {
	row := s.db.QueryRow(`
		SELECT id, agent_id, source_provider, source_model, fallback_chain, enabled, priority, created_at, updated_at
		FROM fallback_configs
		WHERE id = ?
	`, id)

	return s.scanConfig(row)
}

// GetForModel retrieves the fallback configuration for a specific provider/model (global only)
func (s *FallbackStore) GetForModel(provider, model string) (*FallbackConfig, error) {
	row := s.db.QueryRow(`
		SELECT id, agent_id, source_provider, source_model, fallback_chain, enabled, priority, created_at, updated_at
		FROM fallback_configs
		WHERE agent_id = '' AND source_provider = ? AND source_model = ? AND enabled = 1
		LIMIT 1
	`, provider, model)

	return s.scanConfig(row)
}

// GetForAgent retrieves the fallback configuration for a specific agent/provider/model
// Implements hierarchical lookup: agent-specific > global default > not found
func (s *FallbackStore) GetForAgent(agentID, provider, model string) (*FallbackConfig, error) {
	// 1. Try agent-specific config first
	if agentID != "" {
		row := s.db.QueryRow(`
			SELECT id, agent_id, source_provider, source_model, fallback_chain, enabled, priority, created_at, updated_at
			FROM fallback_configs
			WHERE agent_id = ? AND source_provider = ? AND source_model = ? AND enabled = 1
			LIMIT 1
		`, agentID, provider, model)

		cfg, err := s.scanConfig(row)
		if err == nil {
			return cfg, nil
		}
		// If not found, fall through to global defaults
	}

	// 2. Fall back to global defaults (agent_id = '')
	return s.GetForModel(provider, model)
}

// Create inserts a new fallback configuration
func (s *FallbackStore) Create(cfg *FallbackConfig) (int64, error) {
	chainJSON, err := json.Marshal(cfg.FallbackChain)
	if err != nil {
		return 0, fmt.Errorf("marshal fallback_chain: %w", err)
	}

	now := time.Now().Unix()
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	return s.db.InsertReturningID(`
		INSERT INTO fallback_configs (agent_id, source_provider, source_model, fallback_chain, enabled, priority, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, cfg.AgentID, cfg.SourceProvider, cfg.SourceModel, string(chainJSON), enabledInt, cfg.Priority, now, now)
}

// Update modifies an existing fallback configuration
func (s *FallbackStore) Update(cfg *FallbackConfig) error {
	chainJSON, err := json.Marshal(cfg.FallbackChain)
	if err != nil {
		return fmt.Errorf("marshal fallback_chain: %w", err)
	}

	now := time.Now().Unix()
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	_, err = s.db.Exec(`
		UPDATE fallback_configs
		SET agent_id = ?, source_provider = ?, source_model = ?, fallback_chain = ?, enabled = ?, priority = ?, updated_at = ?
		WHERE id = ?
	`, cfg.AgentID, cfg.SourceProvider, cfg.SourceModel, string(chainJSON), enabledInt, cfg.Priority, now, cfg.ID)

	return err
}

// Delete removes a fallback configuration
func (s *FallbackStore) Delete(id int64) error {
	_, err := s.db.Exec(`DELETE FROM fallback_configs WHERE id = ?`, id)
	return err
}

// Toggle enables or disables a fallback configuration
func (s *FallbackStore) Toggle(id int64, enabled bool) error {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE fallback_configs
		SET enabled = ?, updated_at = ?
		WHERE id = ?
	`, enabledInt, now, id)
	return err
}

// scanConfig is a helper to scan a row into a FallbackConfig
func (s *FallbackStore) scanConfig(scanner interface {
	Scan(dest ...interface{}) error
}) (*FallbackConfig, error) {
	var cfg FallbackConfig
	var chainJSON string
	var createdUnix, updatedUnix int64
	var enabledInt int

	err := scanner.Scan(
		&cfg.ID,
		&cfg.AgentID,
		&cfg.SourceProvider,
		&cfg.SourceModel,
		&chainJSON,
		&enabledInt,
		&cfg.Priority,
		&createdUnix,
		&updatedUnix,
	)
	if err != nil {
		return nil, err
	}

	cfg.Enabled = enabledInt == 1
	cfg.CreatedAt = time.Unix(createdUnix, 0)
	cfg.UpdatedAt = time.Unix(updatedUnix, 0)

	if err := json.Unmarshal([]byte(chainJSON), &cfg.FallbackChain); err != nil {
		return nil, fmt.Errorf("unmarshal fallback_chain: %w", err)
	}

	return &cfg, nil
}

// SeedDefaults populates the database with sensible default fallback configurations
func (s *FallbackStore) SeedDefaults() error {
	// Check if any configs exist
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM fallback_configs`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil // Already seeded
	}

	// All defaults are global (agent_id = '').
	// Model IDs use the proxy's canonical short names (resolved by handler.go aliases).
	defaults := []FallbackConfig{
		{
			SourceProvider: "openai",
			SourceModel:    "gpt-4o",
			FallbackChain: []FallbackOption{
				{Provider: "anthropic", Model: "claude-sonnet-4", Priority: 1},
				{Provider: "google", Model: "gemini-2.5-pro", Priority: 2},
			},
			Enabled:  true,
			Priority: 0,
		},
		{
			SourceProvider: "openai",
			SourceModel:    "gpt-4o-mini",
			FallbackChain: []FallbackOption{
				{Provider: "anthropic", Model: "claude-haiku-4-5", Priority: 1},
				{Provider: "google", Model: "gemini-2.5-flash", Priority: 2},
			},
			Enabled:  true,
			Priority: 0,
		},
		{
			SourceProvider: "anthropic",
			SourceModel:    "claude-sonnet-4",
			FallbackChain: []FallbackOption{
				{Provider: "openai", Model: "gpt-4o", Priority: 1},
				{Provider: "google", Model: "gemini-2.5-pro", Priority: 2},
			},
			Enabled:  true,
			Priority: 0,
		},
		{
			SourceProvider: "anthropic",
			SourceModel:    "claude-haiku-4-5",
			FallbackChain: []FallbackOption{
				{Provider: "openai", Model: "gpt-4o-mini", Priority: 1},
				{Provider: "google", Model: "gemini-2.5-flash", Priority: 2},
			},
			Enabled:  true,
			Priority: 0,
		},
		{
			SourceProvider: "google",
			SourceModel:    "gemini-2.5-pro",
			FallbackChain: []FallbackOption{
				{Provider: "openai", Model: "gpt-4o", Priority: 1},
				{Provider: "anthropic", Model: "claude-sonnet-4", Priority: 2},
			},
			Enabled:  true,
			Priority: 0,
		},
	}

	for _, cfg := range defaults {
		if _, err := s.Create(&cfg); err != nil {
			return fmt.Errorf("seed default fallback %s/%s: %w", cfg.SourceProvider, cfg.SourceModel, err)
		}
	}

	return nil
}
