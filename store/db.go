// Package store provides SQLite-backed persistence for Toko-Mo-Co.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" (PostgreSQL)
	_ "modernc.org/sqlite"             // database/sql driver "sqlite"
)

// DB wraps the SQLite connection.
// The writeMu serialises all write operations (INSERT/UPDATE/DELETE)
// so concurrent goroutines never contend for SQLite's single-writer lock.
// Reads are unaffected — WAL mode allows concurrent readers alongside one writer.
type DB struct {
	db      *conn // dialect-aware handle (SQLite or Postgres); rebinds placeholders
	writeMu sync.Mutex
}

// RequestRow is one row in the live feed / requests table.
type RequestRow struct {
	ID               int64
	Timestamp        time.Time
	SessionID        string
	AgentID          string // X-Agent-ID header or inferred from User-Agent
	AppName          string // X-App-Name header
	Provider         string
	Model            string
	PromptPreview    string
	InputTokens      int
	OutputTokens     int
	Cost             float64
	LatencyMs        int64
	StatusCode       int
	IsStreaming      bool
	LoopDetected     bool
	LoopSeverity     string
	ErrorMessage     string // non-empty for upstream errors (4xx/5xx/network)
	OriginalProvider string // pre-fallback provider (empty if no fallback)
	OriginalModel    string // pre-fallback model (empty if no fallback)
	FallbackConfigID int64  // 0 = no fallback, >0 = matched fallback_configs.id
	MatchedRuleID    int64  // 0 = no rule matched, >0 = matched rules.id
	CacheHit         bool   // true if response was served from cache
	PIIRedactedCount int    // Number of PII/secret items redacted in this request
	PIICategories    string // JSON of per-category counts e.g. {"email":2,"phone":1}

	JailbreakDetected bool    // true if NeMo Guard flagged the prompt as a jailbreak
	JailbreakScore    float64 // NeMo Guard score (0..1) when available; else 0/1
	JailbreakCategory string  // detector source/category (e.g. "nemoguard"); "" if none
}

// SessionRow is the aggregate totals for one session.
type SessionRow struct {
	SessionID    string
	AgentID      string
	AppName      string
	TotalCost    float64
	InputTokens  int
	OutputTokens int
	RequestCount int
	StartTime    time.Time
	LastSeen     time.Time
}

// AgentSummary aggregates cost/usage across all sessions for one agent.
type AgentSummary struct {
	AgentID      string
	AppName      string
	TotalCost    float64
	InputTokens  int
	OutputTokens int
	RequestCount int
	LastSeen     time.Time
}

// Open opens the configured database. When databaseURL is a non-empty Postgres
// DSN (postgres://…) it uses PostgreSQL; otherwise it opens (or creates) the
// SQLite database at sqlitePath. The returned *DB is dialect-aware and rebinds
// placeholders for whichever backend is active.
func Open(databaseURL, sqlitePath string) (*DB, error) {
	if strings.TrimSpace(databaseURL) != "" {
		return openPostgres(databaseURL)
	}
	return openSQLite(sqlitePath)
}

// openSQLite opens the SQLite backend. WAL mode allows concurrent readers
// alongside a single writer.
func openSQLite(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Allow multiple connections so readers (dashboard queries, agent summaries,
	// cache lookups) don't block behind writes. busy_timeout (pragma) handles the
	// rare case of write contention.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0) // keep connections alive for the process lifetime

	d := &DB{db: newConn(db, SQLite)}
	if err := d.pragma(); err != nil {
		return nil, err
	}
	if err := d.migrate(); err != nil {
		return nil, err
	}
	return d, nil
}

// openPostgres opens the PostgreSQL backend via pgx and ensures the pgvector
// extension exists (the semantic-cache + memory tables use a native vector column).
func openPostgres(dsn string) (*DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// Postgres handles concurrent writers natively — use a real pool.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(0)
	// Bounded connect probe so a misconfigured/unreachable DB fails fast with a
	// clear error instead of hanging the whole startup.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return nil, fmt.Errorf("enable pgvector extension (is it installed on the server?): %w", err)
	}

	d := &DB{db: newConn(db, Postgres)}
	if err := d.migrate(); err != nil { // no SQLite pragmas on Postgres
		return nil, err
	}
	return d, nil
}

// pragma applies SQLite performance and durability settings.
// These must run before any table access.
func (d *DB) pragma() error {
	pragmas := []string{
		// WAL mode: readers don't block writers, writers don't block readers.
		// Critical for a proxy that reads (dashboard) while writes happen (requests).
		`PRAGMA journal_mode=WAL`,
		// 5-second timeout before returning SQLITE_BUSY — avoids spurious errors
		// when two goroutines race to write (shouldn't happen with MaxOpenConns=1,
		// but defense-in-depth for external tooling opening the DB).
		`PRAGMA busy_timeout=5000`,
		// NORMAL is safe with WAL: data is durable after each WAL frame sync.
		// FULL would fsync after every write — unnecessary overhead here.
		`PRAGMA synchronous=NORMAL`,
		// 8 MB page cache — reduces I/O on large aggregate queries (AgentSummaries).
		`PRAGMA cache_size=-8000`,
		// Store temp tables in memory — used by ORDER BY / GROUP BY internally.
		`PRAGMA temp_store=MEMORY`,
		// 64 MB memory-mapped I/O — sequential reads for history replay are fast.
		`PRAGMA mmap_size=67108864`,
	}
	for _, p := range pragmas {
		if _, err := d.db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

func (d *DB) migrate() error {
	dl := d.db.Dialect()
	// One schema literal kept intact for readability; adapted to the active
	// dialect (AUTOINCREMENT/BLOB) and executed statement-by-statement below,
	// so it works on both SQLite and Postgres (pgx won't run multi-statement Exec).
	schema := `
	CREATE TABLE IF NOT EXISTS requests (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp      INTEGER NOT NULL,
		session_id     TEXT    NOT NULL,
		agent_id       TEXT    NOT NULL DEFAULT '',
		app_name       TEXT    NOT NULL DEFAULT '',
		provider       TEXT    NOT NULL,
		model          TEXT    NOT NULL,
		prompt_preview TEXT    NOT NULL,
		input_tokens   INTEGER NOT NULL DEFAULT 0,
		output_tokens  INTEGER NOT NULL DEFAULT 0,
		cost           REAL    NOT NULL DEFAULT 0,
		latency_ms     INTEGER NOT NULL DEFAULT 0,
		status_code    INTEGER NOT NULL DEFAULT 200,
		is_streaming   INTEGER NOT NULL DEFAULT 0,
		loop_detected  INTEGER NOT NULL DEFAULT 0,
		loop_severity  TEXT    NOT NULL DEFAULT '',
		error_message  TEXT    NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS sessions (
		session_id    TEXT PRIMARY KEY,
		agent_id      TEXT    NOT NULL DEFAULT '',
		app_name      TEXT    NOT NULL DEFAULT '',
		total_cost    REAL    NOT NULL DEFAULT 0,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		start_time    INTEGER NOT NULL,
		last_seen     INTEGER NOT NULL
	);

	-- Indexes for common query patterns:
	--   AgentSummaries: GROUP BY agent_id
	--   RecentRequests: ORDER BY id DESC LIMIT n
	--   Time-range filters: WHERE timestamp > ?
	--   Session drill-down: WHERE session_id = ?
	CREATE INDEX IF NOT EXISTS idx_requests_agent     ON requests(agent_id);
	CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_requests_session   ON requests(session_id);
	CREATE INDEX IF NOT EXISTS idx_requests_provider  ON requests(provider);
	CREATE INDEX IF NOT EXISTS idx_sessions_last_seen ON sessions(last_seen DESC);

	-- Rules table for the rules engine
	CREATE TABLE IF NOT EXISTS rules (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		name            TEXT    NOT NULL,
		enabled         INTEGER NOT NULL DEFAULT 1,
		priority        INTEGER NOT NULL DEFAULT 0,
		scope_agent_id  TEXT    NOT NULL DEFAULT '',
		conditions_json TEXT    NOT NULL,
		action_json     TEXT    NOT NULL,
		description     TEXT    NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_rules_enabled  ON rules(enabled);
	CREATE INDEX IF NOT EXISTS idx_rules_priority ON rules(priority DESC);

	-- Fallback configurations table
	CREATE TABLE IF NOT EXISTS fallback_configs (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_id        TEXT    NOT NULL DEFAULT '',
		source_provider TEXT    NOT NULL,
		source_model    TEXT    NOT NULL,
		fallback_chain  TEXT    NOT NULL,
		enabled         INTEGER NOT NULL DEFAULT 1,
		priority        INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE(agent_id, source_provider, source_model)
	);

	CREATE INDEX IF NOT EXISTS idx_fallback_enabled
		ON fallback_configs(enabled, source_provider, source_model);
	CREATE INDEX IF NOT EXISTS idx_fallback_agent
		ON fallback_configs(agent_id, enabled, source_provider, source_model);

	-- API keys for proxy authentication
	CREATE TABLE IF NOT EXISTS api_keys (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT    NOT NULL,
		key_hash   TEXT    NOT NULL UNIQUE,
		prefix     TEXT    NOT NULL DEFAULT '',
		scopes     TEXT    NOT NULL DEFAULT 'proxy,dashboard',
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		last_used  INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_api_keys_hash    ON api_keys(key_hash) WHERE enabled = 1;
	CREATE INDEX IF NOT EXISTS idx_api_keys_enabled ON api_keys(enabled);

	-- Model pricing table (DB-backed, replaces hardcoded Go map)
	CREATE TABLE IF NOT EXISTS model_pricing (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		model_prefix        TEXT    NOT NULL UNIQUE,
		input_per_1m        REAL    NOT NULL DEFAULT 0,
		cached_input_per_1m REAL    NOT NULL DEFAULT 0,
		output_per_1m       REAL    NOT NULL DEFAULT 0,
		provider            TEXT    NOT NULL DEFAULT '',
		source              TEXT    NOT NULL DEFAULT 'seed',
		updated_at          INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_model_pricing_prefix   ON model_pricing(model_prefix);
	CREATE INDEX IF NOT EXISTS idx_model_pricing_provider ON model_pricing(provider);

	-- Response cache for exact-match deduplication
	CREATE TABLE IF NOT EXISTS response_cache (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		request_hash     TEXT    NOT NULL UNIQUE,
		provider         TEXT    NOT NULL,
		model            TEXT    NOT NULL,
		status_code      INTEGER NOT NULL DEFAULT 200,
		response_body    BLOB    NOT NULL,
		response_headers TEXT    NOT NULL DEFAULT '{}',
		input_tokens     INTEGER NOT NULL DEFAULT 0,
		output_tokens    INTEGER NOT NULL DEFAULT 0,
		cost_per_hit     REAL    NOT NULL DEFAULT 0,
		hit_count        INTEGER NOT NULL DEFAULT 0,
		created_at       INTEGER NOT NULL,
		last_accessed    INTEGER NOT NULL,
		expires_at       INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_response_cache_hash    ON response_cache(request_hash);
	CREATE INDEX IF NOT EXISTS idx_response_cache_expires ON response_cache(expires_at);

	-- Custom providers (user-defined LLM endpoints: Ollama, vLLM, DeepSeek, etc.)
	CREATE TABLE IF NOT EXISTS custom_providers (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT    NOT NULL UNIQUE,
		display_name TEXT    NOT NULL DEFAULT '',
		base_url     TEXT    NOT NULL,
		api_format   TEXT    NOT NULL DEFAULT 'openai',
		api_path     TEXT    NOT NULL DEFAULT '/v1/chat/completions',
		auth_header  TEXT    NOT NULL DEFAULT '',
		auth_env_var TEXT    NOT NULL DEFAULT '',
		models_json  TEXT    NOT NULL DEFAULT '[]',
		default_model TEXT   NOT NULL DEFAULT '',
		enabled      INTEGER NOT NULL DEFAULT 1,
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_custom_providers_name ON custom_providers(name);

	CREATE TABLE IF NOT EXISTS settings (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		data       TEXT    NOT NULL,
		updated_at INTEGER NOT NULL
	);

	-- Outcome feedback: tracks success/failure of LLM decisions
	CREATE TABLE IF NOT EXISTS feedback (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id  TEXT    NOT NULL DEFAULT '',
		request_id  INTEGER NOT NULL DEFAULT 0,
		agent_id    TEXT    NOT NULL DEFAULT '',
		app_name    TEXT    NOT NULL DEFAULT '',
		outcome     TEXT    NOT NULL DEFAULT '',
		score       REAL    NOT NULL DEFAULT 0,
		details     TEXT    NOT NULL DEFAULT '',
		metadata    TEXT    NOT NULL DEFAULT '{}',
		created_at  INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_feedback_agent      ON feedback(agent_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_feedback_session    ON feedback(session_id);
	CREATE INDEX IF NOT EXISTS idx_feedback_outcome    ON feedback(outcome);

	-- Prompt version tracking: records every unique system prompt per agent
	CREATE TABLE IF NOT EXISTS prompt_versions (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_id      TEXT    NOT NULL,
		app_name      TEXT    NOT NULL DEFAULT '',
		content_hash  TEXT    NOT NULL,
		content       TEXT    NOT NULL,
		previous_hash TEXT    NOT NULL DEFAULT '',
		provider      TEXT    NOT NULL DEFAULT '',
		model         TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_prompt_versions_agent   ON prompt_versions(agent_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_prompt_versions_hash    ON prompt_versions(agent_id, content_hash);

	`
	if dl == Postgres {
		schema = strings.ReplaceAll(schema, "INTEGER PRIMARY KEY AUTOINCREMENT",
			"BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY")
		schema = strings.ReplaceAll(schema, "BLOB", "BYTEA")
	}
	for _, stmt := range splitSQLStatements(schema) {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// Safely add columns to databases that predate agent identity (this also adds
	// columns intentionally absent from the CREATE TABLE above, e.g. rules.evidence
	// and requests.cache_hit). On SQLite duplicate-column errors are intentionally
	// ignored; Postgres uses ADD COLUMN IF NOT EXISTS (a no-op on a fresh schema).
	addCol := dl.AddColumn()
	for _, col := range []struct{ table, name, def string }{
		{"requests", "agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "app_name", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "error_message", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "app_name", "TEXT NOT NULL DEFAULT ''"},
		{"fallback_configs", "agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "original_provider", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "original_model", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "fallback_config_id", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "matched_rule_id", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "cache_hit", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "pii_redacted_count", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "pii_categories", "TEXT NOT NULL DEFAULT ''"},
		{"requests", "jailbreak_detected", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "jailbreak_score", "REAL NOT NULL DEFAULT 0"},
		{"requests", "jailbreak_category", "TEXT NOT NULL DEFAULT ''"},
		{"rules", "evidence", "TEXT NOT NULL DEFAULT ''"},
		{"custom_providers", "default_model", "TEXT NOT NULL DEFAULT ''"},
	} {
		d.db.Exec(`ALTER TABLE ` + col.table + ` ` + addCol + ` ` + col.name + ` ` + col.def) //nolint:errcheck
	}

	// Partial indexes for hit-count queries — must be created AFTER the ALTER TABLE
	// migrations above so the columns exist when the index is built.
	d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_fallback_config
		ON requests(fallback_config_id) WHERE fallback_config_id > 0`) //nolint:errcheck
	d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_matched_rule
		ON requests(matched_rule_id) WHERE matched_rule_id > 0`) //nolint:errcheck

	return nil
}

// InsertRequest saves a request event and upserts the session totals atomically.
func (d *DB) InsertRequest(r RequestRow) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(d.db.Dialect().Rebind(`
		INSERT INTO requests
			(timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
			 input_tokens, output_tokens, cost, latency_ms, status_code,
			 is_streaming, loop_detected, loop_severity, error_message,
			 original_provider, original_model, fallback_config_id, matched_rule_id, cache_hit,
			 pii_redacted_count, pii_categories,
			 jailbreak_detected, jailbreak_score, jailbreak_category)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		r.Timestamp.Unix(), r.SessionID, r.AgentID, r.AppName,
		r.Provider, r.Model, r.PromptPreview,
		r.InputTokens, r.OutputTokens, r.Cost, r.LatencyMs, r.StatusCode,
		boolInt(r.IsStreaming), boolInt(r.LoopDetected), r.LoopSeverity, r.ErrorMessage,
		r.OriginalProvider, r.OriginalModel, r.FallbackConfigID, r.MatchedRuleID,
		boolInt(r.CacheHit),
		r.PIIRedactedCount, r.PIICategories,
		boolInt(r.JailbreakDetected), r.JailbreakScore, r.JailbreakCategory,
	)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	// Sanitise agent_id: replace commas (our list delimiter) with underscores,
	// and treat empty/whitespace-only as "unknown" so we never append blanks.
	safeAgent := strings.ReplaceAll(strings.TrimSpace(r.AgentID), ",", "_")
	if safeAgent == "" {
		safeAgent = "unknown"
	}

	// The agent_id column stores a comma-separated list of unique agent IDs
	// that have used this session (e.g. "MarketAnalyst,SentimentAnalyst,TraderAgent").
	// On conflict: append the new agent only if not already present, capped at 20
	// entries to prevent unbounded growth on high-churn sessions.
	_, err = tx.Exec(d.db.Dialect().Rebind(`
		INSERT INTO sessions
			(session_id, agent_id, app_name, total_cost, input_tokens, output_tokens, request_count, start_time, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			agent_id      = CASE
				WHEN excluded.agent_id = '' OR excluded.agent_id = 'unknown' THEN sessions.agent_id
				WHEN sessions.agent_id = excluded.agent_id THEN sessions.agent_id
				WHEN (',' || sessions.agent_id || ',') LIKE ('%,' || excluded.agent_id || ',%') THEN sessions.agent_id
				WHEN LENGTH(sessions.agent_id) - LENGTH(REPLACE(sessions.agent_id, ',', '')) >= 19 THEN sessions.agent_id
				ELSE sessions.agent_id || ',' || excluded.agent_id
			END,
			app_name      = COALESCE(NULLIF(excluded.app_name, ''), sessions.app_name),
			total_cost    = sessions.total_cost    + excluded.total_cost,
			input_tokens  = sessions.input_tokens  + excluded.input_tokens,
			output_tokens = sessions.output_tokens + excluded.output_tokens,
			request_count = sessions.request_count + 1,
			last_seen     = excluded.last_seen`),
		r.SessionID, safeAgent, r.AppName, r.Cost, r.InputTokens, r.OutputTokens, now, now,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecentRequests returns the most recent N request rows (newest first).
func (d *DB) RecentRequests(limit int) ([]RequestRow, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, '') AS error_message,
		       COALESCE(original_provider, '') AS original_provider,
		       COALESCE(original_model, '') AS original_model,
		       COALESCE(fallback_config_id, 0) AS fallback_config_id,
		       COALESCE(matched_rule_id, 0) AS matched_rule_id
		FROM requests
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RequestRow
	for rows.Next() {
		var r RequestRow
		var ts int64
		var isStream, loopDet int
		if err := rows.Scan(
			&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost,
			&r.LatencyMs, &r.StatusCode, &isStream, &loopDet,
			&r.LoopSeverity, &r.ErrorMessage,
			&r.OriginalProvider, &r.OriginalModel, &r.FallbackConfigID,
			&r.MatchedRuleID,
		); err != nil {
			return nil, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.IsStreaming = isStream != 0
		r.LoopDetected = loopDet != 0
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetSession returns totals for a session, or zero values if not found.
func (d *DB) GetSession(sessionID string) (SessionRow, error) {
	var s SessionRow
	var start, last int64
	err := d.db.QueryRow(`
		SELECT session_id, agent_id, app_name, total_cost, input_tokens, output_tokens,
		       request_count, start_time, last_seen
		FROM sessions WHERE session_id = ?`, sessionID).
		Scan(&s.SessionID, &s.AgentID, &s.AppName,
			&s.TotalCost, &s.InputTokens, &s.OutputTokens,
			&s.RequestCount, &start, &last)
	if err == sql.ErrNoRows {
		return SessionRow{SessionID: sessionID, StartTime: time.Now()}, nil
	}
	if err != nil {
		return s, err
	}
	s.StartTime = time.Unix(start, 0)
	s.LastSeen = time.Unix(last, 0)
	return s, nil
}

// GetRequestByID returns a single request row by its primary key.
func (d *DB) GetRequestByID(id int64) (RequestRow, error) {
	var r RequestRow
	var ts int64
	err := d.db.QueryRow(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, ''), COALESCE(original_provider, ''),
		       COALESCE(original_model, ''), COALESCE(fallback_config_id, 0),
		       COALESCE(matched_rule_id, 0)
		FROM requests WHERE id = ?`, id).
		Scan(&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost, &r.LatencyMs, &r.StatusCode,
			&r.IsStreaming, &r.LoopDetected, &r.LoopSeverity,
			&r.ErrorMessage, &r.OriginalProvider, &r.OriginalModel,
			&r.FallbackConfigID, &r.MatchedRuleID)
	r.Timestamp = time.Unix(ts, 0)
	return r, err
}

// AllSessions returns sessions ordered by last activity, capped at 10000 rows
// to prevent unbounded memory usage when restoring session state at startup.
func (d *DB) AllSessions() ([]SessionRow, error) {
	rows, err := d.db.Query(`
		SELECT session_id, agent_id, app_name, total_cost, input_tokens, output_tokens,
		       request_count, start_time, last_seen
		FROM sessions ORDER BY last_seen DESC
		LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SessionRow
	for rows.Next() {
		var s SessionRow
		var start, last int64
		if err := rows.Scan(
			&s.SessionID, &s.AgentID, &s.AppName,
			&s.TotalCost, &s.InputTokens, &s.OutputTokens,
			&s.RequestCount, &start, &last,
		); err != nil {
			return nil, err
		}
		s.StartTime = time.Unix(start, 0)
		s.LastSeen = time.Unix(last, 0)
		result = append(result, s)
	}
	return result, rows.Err()
}

// SessionList returns sessions filtered by optional app_name, with pagination.
// Results are ordered by last activity (newest first).
func (d *DB) SessionList(appName string, limit, offset int) ([]SessionRow, int, error) {
	// Count total matching sessions for pagination metadata
	var total int
	var countArgs []interface{}
	countQuery := `SELECT COUNT(*) FROM sessions`
	if appName != "" {
		countQuery += ` WHERE app_name = ?`
		countArgs = append(countArgs, appName)
	}
	if err := d.db.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Fetch page
	var args []interface{}
	query := `SELECT session_id, agent_id, app_name, total_cost, input_tokens, output_tokens,
	                 request_count, start_time, last_seen
	          FROM sessions`
	if appName != "" {
		query += ` WHERE app_name = ?`
		args = append(args, appName)
	}
	query += ` ORDER BY last_seen DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []SessionRow
	for rows.Next() {
		var s SessionRow
		var start, last int64
		if err := rows.Scan(
			&s.SessionID, &s.AgentID, &s.AppName,
			&s.TotalCost, &s.InputTokens, &s.OutputTokens,
			&s.RequestCount, &start, &last,
		); err != nil {
			return nil, 0, err
		}
		s.StartTime = time.Unix(start, 0)
		s.LastSeen = time.Unix(last, 0)
		result = append(result, s)
	}
	return result, total, rows.Err()
}

// SessionRequests returns all requests belonging to a session, ordered chronologically.
func (d *DB) SessionRequests(sessionID string) ([]RequestRow, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, '') AS error_message,
		       COALESCE(original_provider, '') AS original_provider,
		       COALESCE(original_model, '') AS original_model,
		       COALESCE(fallback_config_id, 0) AS fallback_config_id,
		       COALESCE(matched_rule_id, 0) AS matched_rule_id,
		       COALESCE(cache_hit, 0) AS cache_hit,
		       COALESCE(pii_redacted_count, 0) AS pii_redacted_count
		FROM requests
		WHERE session_id = ?
		ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RequestRow
	for rows.Next() {
		var r RequestRow
		var ts int64
		var isStream, loopDet, cacheHit int
		if err := rows.Scan(
			&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost,
			&r.LatencyMs, &r.StatusCode, &isStream, &loopDet,
			&r.LoopSeverity, &r.ErrorMessage,
			&r.OriginalProvider, &r.OriginalModel, &r.FallbackConfigID,
			&r.MatchedRuleID, &cacheHit, &r.PIIRedactedCount,
		); err != nil {
			return nil, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.IsStreaming = isStream != 0
		r.LoopDetected = loopDet != 0
		r.CacheHit = cacheHit != 0
		result = append(result, r)
	}
	return result, rows.Err()
}

// AgentRequests returns the most recent requests for an agent, newest first.
func (d *DB) AgentRequests(agentID string, limit int) ([]RequestRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := d.db.Query(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, '') AS error_message,
		       COALESCE(original_provider, '') AS original_provider,
		       COALESCE(original_model, '') AS original_model,
		       COALESCE(fallback_config_id, 0) AS fallback_config_id,
		       COALESCE(matched_rule_id, 0) AS matched_rule_id,
		       COALESCE(cache_hit, 0) AS cache_hit,
		       COALESCE(pii_redacted_count, 0) AS pii_redacted_count
		FROM requests
		WHERE agent_id = ?
		ORDER BY id DESC
		LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RequestRow
	for rows.Next() {
		var r RequestRow
		var ts int64
		var isStream, loopDet, cacheHit int
		if err := rows.Scan(
			&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost,
			&r.LatencyMs, &r.StatusCode, &isStream, &loopDet,
			&r.LoopSeverity, &r.ErrorMessage,
			&r.OriginalProvider, &r.OriginalModel, &r.FallbackConfigID,
			&r.MatchedRuleID, &cacheHit, &r.PIIRedactedCount,
		); err != nil {
			return nil, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.IsStreaming = isStream != 0
		r.LoopDetected = loopDet != 0
		r.CacheHit = cacheHit != 0
		result = append(result, r)
	}
	return result, rows.Err()
}

// AgentSummaries returns per-agent rollups ordered by total cost descending.
func (d *DB) AgentSummaries() ([]AgentSummary, error) {
	rows, err := d.db.Query(`
		SELECT
			agent_id,
			MAX(app_name)      AS app_name,
			SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END) AS total_cost,
			SUM(input_tokens)  AS input_tokens,
			SUM(output_tokens) AS output_tokens,
			COUNT(*)           AS request_count,
			MAX(timestamp)     AS last_seen
		FROM requests
		GROUP BY agent_id
		ORDER BY total_cost DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []AgentSummary
	for rows.Next() {
		var a AgentSummary
		var last int64
		if err := rows.Scan(
			&a.AgentID, &a.AppName,
			&a.TotalCost, &a.InputTokens, &a.OutputTokens,
			&a.RequestCount, &last,
		); err != nil {
			return nil, err
		}
		a.LastSeen = time.Unix(last, 0)
		result = append(result, a)
	}
	return result, rows.Err()
}

// Prune deletes request rows older than the given number of days and runs
// an incremental VACUUM to reclaim disk space.  Call this on a daily ticker.
func (d *DB) Prune(keepDays int) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -keepDays).Unix()
	res, err := d.db.Exec(`DELETE FROM requests WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Incremental vacuum reclaims pages freed by the DELETE without a full rewrite.
		d.db.Exec(`PRAGMA incremental_vacuum`) //nolint:errcheck
		log.Printf("[DB] pruned %d request rows older than %d days", n, keepDays)
	}
	return n, nil
}

// FallbackHitCount holds the hit count and last-triggered time for one fallback config.
type FallbackHitCount struct {
	FallbackConfigID int64
	Count            int
	LastTriggered    time.Time
}

// FallbackConfigHitCounts returns the number of times each fallback config was
// triggered, optionally filtered to the last N days (0 = all time).
func (d *DB) FallbackConfigHitCounts(sinceDays int) ([]FallbackHitCount, error) {
	var query string
	var args []interface{}

	if sinceDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -sinceDays).Unix()
		query = `
			SELECT fallback_config_id, COUNT(*) AS hit_count, MAX(timestamp) AS last_triggered
			FROM requests
			WHERE fallback_config_id > 0 AND timestamp >= ?
			GROUP BY fallback_config_id
			ORDER BY hit_count DESC`
		args = append(args, cutoff)
	} else {
		query = `
			SELECT fallback_config_id, COUNT(*) AS hit_count, MAX(timestamp) AS last_triggered
			FROM requests
			WHERE fallback_config_id > 0
			GROUP BY fallback_config_id
			ORDER BY hit_count DESC`
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []FallbackHitCount
	for rows.Next() {
		var h FallbackHitCount
		var lastTS int64
		if err := rows.Scan(&h.FallbackConfigID, &h.Count, &lastTS); err != nil {
			return nil, err
		}
		h.LastTriggered = time.Unix(lastTS, 0)
		result = append(result, h)
	}
	return result, rows.Err()
}

// FallbackConfigRequests returns paginated requests that triggered a specific
// fallback config, ordered newest first. Returns (rows, totalCount, error).
// Uses COUNT(*) OVER() to get the total in a single query instead of N+1.
func (d *DB) FallbackConfigRequests(configID int64, limit, offset int) ([]RequestRow, int, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, '') AS error_message,
		       COALESCE(original_provider, '') AS original_provider,
		       COALESCE(original_model, '') AS original_model,
		       fallback_config_id,
		       COUNT(*) OVER() AS total_count
		FROM requests
		WHERE fallback_config_id = ?
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, configID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []RequestRow
	var total int
	for rows.Next() {
		var r RequestRow
		var ts int64
		var isStream, loopDet int
		if err := rows.Scan(
			&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost,
			&r.LatencyMs, &r.StatusCode, &isStream, &loopDet,
			&r.LoopSeverity, &r.ErrorMessage,
			&r.OriginalProvider, &r.OriginalModel, &r.FallbackConfigID,
			&total,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.IsStreaming = isStream != 0
		r.LoopDetected = loopDet != 0
		result = append(result, r)
	}
	return result, total, rows.Err()
}

// RuleHitCount holds the hit count and last-triggered time for one rule.
type RuleHitCount struct {
	RuleID        int64
	Count         int
	LastTriggered time.Time
}

// RuleHitCounts returns the number of times each rule was triggered,
// optionally filtered to the last N days (0 = all time).
func (d *DB) RuleHitCounts(sinceDays int) ([]RuleHitCount, error) {
	var query string
	var args []interface{}

	if sinceDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -sinceDays).Unix()
		query = `
			SELECT matched_rule_id, COUNT(*) AS hit_count, MAX(timestamp) AS last_triggered
			FROM requests
			WHERE matched_rule_id > 0 AND timestamp >= ?
			GROUP BY matched_rule_id
			ORDER BY hit_count DESC`
		args = append(args, cutoff)
	} else {
		query = `
			SELECT matched_rule_id, COUNT(*) AS hit_count, MAX(timestamp) AS last_triggered
			FROM requests
			WHERE matched_rule_id > 0
			GROUP BY matched_rule_id
			ORDER BY hit_count DESC`
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RuleHitCount
	for rows.Next() {
		var h RuleHitCount
		var lastTS int64
		if err := rows.Scan(&h.RuleID, &h.Count, &lastTS); err != nil {
			return nil, err
		}
		h.LastTriggered = time.Unix(lastTS, 0)
		result = append(result, h)
	}
	return result, rows.Err()
}

// RuleRequests returns paginated requests that matched a specific rule,
// ordered newest first. Returns (rows, totalCount, error).
// Uses COUNT(*) OVER() to get the total in a single query instead of N+1.
func (d *DB) RuleRequests(ruleID int64, limit, offset int) ([]RequestRow, int, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, session_id, agent_id, app_name, provider, model, prompt_preview,
		       input_tokens, output_tokens, cost, latency_ms, status_code,
		       is_streaming, loop_detected, loop_severity,
		       COALESCE(error_message, '') AS error_message,
		       COALESCE(original_provider, '') AS original_provider,
		       COALESCE(original_model, '') AS original_model,
		       COALESCE(fallback_config_id, 0) AS fallback_config_id,
		       matched_rule_id,
		       COUNT(*) OVER() AS total_count
		FROM requests
		WHERE matched_rule_id = ?
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, ruleID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []RequestRow
	var total int
	for rows.Next() {
		var r RequestRow
		var ts int64
		var isStream, loopDet int
		if err := rows.Scan(
			&r.ID, &ts, &r.SessionID, &r.AgentID, &r.AppName,
			&r.Provider, &r.Model, &r.PromptPreview,
			&r.InputTokens, &r.OutputTokens, &r.Cost,
			&r.LatencyMs, &r.StatusCode, &isStream, &loopDet,
			&r.LoopSeverity, &r.ErrorMessage,
			&r.OriginalProvider, &r.OriginalModel, &r.FallbackConfigID,
			&r.MatchedRuleID,
			&total,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.IsStreaming = isStream != 0
		r.LoopDetected = loopDet != 0
		result = append(result, r)
	}
	return result, total, rows.Err()
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// DB returns the dialect-aware query handle for use by other packages (e.g., rules).
// It rebinds '?' placeholders for the active backend, so callers keep using '?'.
func (d *DB) DB() Querier { return d.db }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LogStats logs a summary of what's in the DB (called at startup).
func (d *DB) LogStats() {
	var reqCount int
	var totalCost float64
	d.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END),0) FROM requests`).Scan(&reqCount, &totalCost)
	log.Printf("[DB] loaded %d persisted requests, total cost $%.4f", reqCount, totalCost)
}

// ═══════════════════════════════════════════════════════════════════════════════
// API Key Management
// ═══════════════════════════════════════════════════════════════════════════════

// APIKeyRow represents an API key record.
type APIKeyRow struct {
	ID        int64
	Name      string
	KeyHash   string
	Prefix    string
	Scopes    string
	Enabled   bool
	CreatedAt time.Time
	LastUsed  time.Time
	ExpiresAt time.Time
}

// ValidateAPIKeyHash checks if a SHA-256 key hash corresponds to an active,
// non-expired API key. Returns (keyID, keyName, nil) on success or (0, "", nil)
// if the key is not found / disabled / expired.
func (d *DB) ValidateAPIKeyHash(hash string) (int64, string, error) {
	var id int64
	var name string
	var expiresAt int64

	err := d.db.QueryRow(`
		SELECT id, name, expires_at
		FROM api_keys
		WHERE key_hash = ? AND enabled = 1`,
		hash,
	).Scan(&id, &name, &expiresAt)

	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return 0, "", nil
		}
		return 0, "", err
	}

	// Check expiry (0 = never expires)
	if expiresAt > 0 && time.Now().Unix() > expiresAt {
		return 0, "", nil
	}

	// Update last_used in the background (non-blocking)
	go func() {
		d.db.Exec(`UPDATE api_keys SET last_used = ? WHERE id = ?`, time.Now().Unix(), id) //nolint:errcheck
	}()

	return id, name, nil
}

// ListAPIKeys returns all API keys (without hashes).
func (d *DB) ListAPIKeys() ([]APIKeyRow, error) {
	rows, err := d.db.Query(`
		SELECT id, name, prefix, scopes, enabled, created_at, last_used, expires_at
		FROM api_keys
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []APIKeyRow
	for rows.Next() {
		var k APIKeyRow
		var createdAt, lastUsed, expiresAt int64
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scopes, &enabled, &createdAt, &lastUsed, &expiresAt); err != nil {
			return nil, err
		}
		k.Enabled = enabled != 0
		k.CreatedAt = time.Unix(createdAt, 0)
		if lastUsed > 0 {
			k.LastUsed = time.Unix(lastUsed, 0)
		}
		if expiresAt > 0 {
			k.ExpiresAt = time.Unix(expiresAt, 0)
		}
		result = append(result, k)
	}
	return result, rows.Err()
}

// InsertAPIKey creates a new API key record. Returns the new row ID.
func (d *DB) InsertAPIKey(name, hash, prefix, scopes string, expiresAt *time.Time) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var expiry int64
	if expiresAt != nil {
		expiry = expiresAt.Unix()
	}

	return d.db.InsertReturningID(`
		INSERT INTO api_keys (name, key_hash, prefix, scopes, enabled, created_at, last_used, expires_at)
		VALUES (?, ?, ?, ?, 1, ?, 0, ?)`,
		name, hash, prefix, scopes, time.Now().Unix(), expiry,
	)
}

// DeleteAPIKey permanently removes an API key.
func (d *DB) DeleteAPIKey(id int64) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// ToggleAPIKey enables or disables an API key.
func (d *DB) ToggleAPIKey(id int64, enabled bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	e := 0
	if enabled {
		e = 1
	}
	_, err := d.db.Exec(`UPDATE api_keys SET enabled = ? WHERE id = ?`, e, id)
	return err
}

// TouchAPIKeyUsage updates the last_used timestamp for a key (by hash).
func (d *DB) TouchAPIKeyUsage(hash string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`UPDATE api_keys SET last_used = ? WHERE key_hash = ?`, time.Now().Unix(), hash)
	return err
}

// CountAPIKeys returns the total number of enabled API keys.
func (d *DB) CountAPIKeys() (int, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE enabled = 1`).Scan(&count)
	return count, err
}

// ═══════════════════════════════════════════════════════════════════════════════
// Model Pricing Management
// ═══════════════════════════════════════════════════════════════════════════════

// ModelPricingRow represents a model pricing entry in the database.
type ModelPricingRow struct {
	ID               int64
	ModelPrefix      string
	InputPer1M       float64
	CachedInputPer1M float64
	OutputPer1M      float64
	Provider         string
	Source           string // "seed" or "custom"
	UpdatedAt        time.Time
}

// ListModelPricing returns all pricing entries ordered by provider and model prefix.
func (d *DB) ListModelPricing() ([]ModelPricingRow, error) {
	rows, err := d.db.Query(`
		SELECT id, model_prefix, input_per_1m, cached_input_per_1m, output_per_1m,
		       provider, source, updated_at
		FROM model_pricing
		ORDER BY provider, model_prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelPricingRow
	for rows.Next() {
		var r ModelPricingRow
		var updatedAt int64
		if err := rows.Scan(&r.ID, &r.ModelPrefix, &r.InputPer1M, &r.CachedInputPer1M,
			&r.OutputPer1M, &r.Provider, &r.Source, &updatedAt); err != nil {
			return nil, err
		}
		r.UpdatedAt = time.Unix(updatedAt, 0)
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetModelPricingByID returns a single pricing entry by ID.
func (d *DB) GetModelPricingByID(id int64) (ModelPricingRow, error) {
	var r ModelPricingRow
	var updatedAt int64
	err := d.db.QueryRow(`
		SELECT id, model_prefix, input_per_1m, cached_input_per_1m, output_per_1m,
		       provider, source, updated_at
		FROM model_pricing WHERE id = ?`, id).
		Scan(&r.ID, &r.ModelPrefix, &r.InputPer1M, &r.CachedInputPer1M,
			&r.OutputPer1M, &r.Provider, &r.Source, &updatedAt)
	if err != nil {
		return r, err
	}
	r.UpdatedAt = time.Unix(updatedAt, 0)
	return r, nil
}

// InsertModelPricing creates a new model pricing entry. Returns the new row ID.
func (d *DB) InsertModelPricing(r ModelPricingRow) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	return d.db.InsertReturningID(`
		INSERT INTO model_pricing (model_prefix, input_per_1m, cached_input_per_1m,
			output_per_1m, provider, source, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ModelPrefix, r.InputPer1M, r.CachedInputPer1M,
		r.OutputPer1M, r.Provider, r.Source, time.Now().Unix(),
	)
}

// UpdateModelPricing updates an existing model pricing entry.
func (d *DB) UpdateModelPricing(r ModelPricingRow) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`
		UPDATE model_pricing
		SET model_prefix = ?, input_per_1m = ?, cached_input_per_1m = ?,
		    output_per_1m = ?, provider = ?, source = ?, updated_at = ?
		WHERE id = ?`,
		r.ModelPrefix, r.InputPer1M, r.CachedInputPer1M,
		r.OutputPer1M, r.Provider, r.Source, time.Now().Unix(), r.ID,
	)
	return err
}

// DeleteModelPricing removes a model pricing entry by ID.
func (d *DB) DeleteModelPricing(id int64) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`DELETE FROM model_pricing WHERE id = ?`, id)
	return err
}

// CountModelPricing returns the total number of model pricing entries.
func (d *DB) CountModelPricing() (int, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM model_pricing`).Scan(&count)
	return count, err
}

// DeleteAllModelPricing removes all model pricing entries (used before re-seeding).
func (d *DB) DeleteAllModelPricing() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`DELETE FROM model_pricing`)
	return err
}

// OldestModelPricingUpdate returns the oldest updated_at timestamp across all entries.
// Returns zero time if the table is empty.
func (d *DB) OldestModelPricingUpdate() (time.Time, error) {
	var ts sql.NullInt64
	err := d.db.QueryRow(`SELECT MIN(updated_at) FROM model_pricing`).Scan(&ts)
	if err != nil || !ts.Valid {
		return time.Time{}, err
	}
	return time.Unix(ts.Int64, 0), nil
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE CACHE
// ═══════════════════════════════════════════════════════════════════════════════

// CacheRow represents a cached LLM response.
type CacheRow struct {
	ID              int64
	RequestHash     string
	Provider        string
	Model           string
	StatusCode      int
	ResponseBody    []byte
	ResponseHeaders string // JSON-encoded map[string][]string
	InputTokens     int
	OutputTokens    int
	CostPerHit      float64
	HitCount        int
	CreatedAt       time.Time
	LastAccessed    time.Time
	ExpiresAt       time.Time
}

// GetCacheByHash returns a cached response by its request hash, or nil if not found/expired.
func (d *DB) GetCacheByHash(hash string) (*CacheRow, error) {
	row := d.db.QueryRow(`
		SELECT id, request_hash, provider, model, status_code,
		       response_body, response_headers,
		       input_tokens, output_tokens, cost_per_hit, hit_count,
		       created_at, last_accessed, expires_at
		FROM response_cache
		WHERE request_hash = ? AND expires_at > ?`,
		hash, time.Now().Unix(),
	)
	var r CacheRow
	var createdAt, lastAccessed, expiresAt int64
	err := row.Scan(
		&r.ID, &r.RequestHash, &r.Provider, &r.Model, &r.StatusCode,
		&r.ResponseBody, &r.ResponseHeaders,
		&r.InputTokens, &r.OutputTokens, &r.CostPerHit, &r.HitCount,
		&createdAt, &lastAccessed, &expiresAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = time.Unix(createdAt, 0)
	r.LastAccessed = time.Unix(lastAccessed, 0)
	r.ExpiresAt = time.Unix(expiresAt, 0)
	return &r, nil
}

// InsertOrUpdateCache upserts a cache entry.
func (d *DB) InsertOrUpdateCache(r CacheRow) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`
		INSERT INTO response_cache
			(request_hash, provider, model, status_code,
			 response_body, response_headers,
			 input_tokens, output_tokens, cost_per_hit, hit_count,
			 created_at, last_accessed, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_hash) DO UPDATE SET
			response_body    = excluded.response_body,
			response_headers = excluded.response_headers,
			input_tokens     = excluded.input_tokens,
			output_tokens    = excluded.output_tokens,
			cost_per_hit     = excluded.cost_per_hit,
			last_accessed    = excluded.last_accessed,
			expires_at       = excluded.expires_at`,
		r.RequestHash, r.Provider, r.Model, r.StatusCode,
		r.ResponseBody, r.ResponseHeaders,
		r.InputTokens, r.OutputTokens, r.CostPerHit, r.HitCount,
		r.CreatedAt.Unix(), r.LastAccessed.Unix(), r.ExpiresAt.Unix(),
	)
	return err
}

// IncrementCacheHit bumps the hit count and last-accessed timestamp for a cache entry.
func (d *DB) IncrementCacheHit(hash string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(`
		UPDATE response_cache
		SET hit_count = hit_count + 1, last_accessed = ?
		WHERE request_hash = ?`,
		time.Now().Unix(), hash,
	)
	return err
}

// LoadHotCacheEntries returns the N most recently accessed non-expired cache entries.
func (d *DB) LoadHotCacheEntries(limit int) ([]CacheRow, error) {
	rows, err := d.db.Query(`
		SELECT id, request_hash, provider, model, status_code,
		       response_body, response_headers,
		       input_tokens, output_tokens, cost_per_hit, hit_count,
		       created_at, last_accessed, expires_at
		FROM response_cache
		WHERE expires_at > ?
		ORDER BY last_accessed DESC
		LIMIT ?`,
		time.Now().Unix(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CacheRow
	for rows.Next() {
		var r CacheRow
		var createdAt, lastAccessed, expiresAt int64
		if err := rows.Scan(
			&r.ID, &r.RequestHash, &r.Provider, &r.Model, &r.StatusCode,
			&r.ResponseBody, &r.ResponseHeaders,
			&r.InputTokens, &r.OutputTokens, &r.CostPerHit, &r.HitCount,
			&createdAt, &lastAccessed, &expiresAt,
		); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		r.LastAccessed = time.Unix(lastAccessed, 0)
		r.ExpiresAt = time.Unix(expiresAt, 0)
		result = append(result, r)
	}
	return result, rows.Err()
}

// FlushCache deletes all response cache entries. Returns the number of deleted rows.
func (d *DB) FlushCache() (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	result, err := d.db.Exec(`DELETE FROM response_cache`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneExpiredCache removes expired cache entries. Returns the number of deleted rows.
func (d *DB) PruneExpiredCache() (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	result, err := d.db.Exec(`DELETE FROM response_cache WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CacheStats returns aggregate cache statistics from the database.
func (d *DB) CacheStats() (entries int, totalHits int, totalSavedCost float64, err error) {
	err = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0),
		       COALESCE(SUM(hit_count), 0),
		       COALESCE(SUM(hit_count * cost_per_hit), 0)
		FROM response_cache
		WHERE expires_at > ?`,
		time.Now().Unix(),
	).Scan(&entries, &totalHits, &totalSavedCost)
	return
}

// ── Cost Savings Analytics ──────────────────────────────────────────────────

// SavingsBreakdown aggregates cost savings across all proxy features.
type SavingsBreakdown struct {
	CacheHits        int     `json:"cache_hits"`
	CacheSaved       float64 `json:"cache_cost_saved"`
	RulesBlocked     int     `json:"rules_blocked"`
	RulesSaved       float64 `json:"rules_cost_saved"`
	FallbackCount    int     `json:"fallback_count"`
	TotalSaved       float64 `json:"total_cost_saved"`
	PIIRequestCount  int     `json:"pii_request_count"`  // Requests that had PII redacted
	PIIItemsRedacted int     `json:"pii_items_redacted"` // Total individual PII items redacted
}

// GetSavingsBreakdown returns cost savings data from cache, rules, and fallback.
// Cache savings come from the requests table (cache_hit=1) which covers both
// exact-match and semantic cache hits. Rules savings come from requests blocked
// by rules (status 403/429). Fallback count is requests recovered via fallback configs.
func (d *DB) GetSavingsBreakdown() SavingsBreakdown {
	var s SavingsBreakdown

	// Cache savings: count all requests served from cache (exact + semantic).
	// The cost field on cache-hit rows reflects what the API call would have cost.
	_ = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(cost), 0)
		FROM requests
		WHERE cache_hit = 1`,
	).Scan(&s.CacheHits, &s.CacheSaved)

	// Rules blocked savings: requests that were blocked by rules (cost = estimated cost at block time)
	_ = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(cost), 0)
		FROM requests
		WHERE matched_rule_id > 0 AND status_code IN (403, 429)`,
	).Scan(&s.RulesBlocked, &s.RulesSaved)

	// Fallback recovered: requests that succeeded via fallback (count only, cost was paid to fallback provider)
	_ = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0)
		FROM requests
		WHERE fallback_config_id > 0 AND status_code = 200`,
	).Scan(&s.FallbackCount)

	// PII redaction stats
	_ = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(pii_redacted_count), 0)
		FROM requests
		WHERE pii_redacted_count > 0`,
	).Scan(&s.PIIRequestCount, &s.PIIItemsRedacted)

	s.TotalSaved = s.CacheSaved + s.RulesSaved
	return s
}

// ── Security Analytics ──────────────────────────────────────────────────────

// PIIStats holds aggregate PII redaction analytics for the /api/security/pii endpoint.
type PIIStats struct {
	TotalRequestsScanned   int            `json:"total_requests_scanned"`
	RequestsWithRedactions int            `json:"requests_with_redactions"`
	TotalItemsRedacted     int            `json:"total_items_redacted"`
	Categories             map[string]int `json:"categories"`
}

// GetPIIStats returns aggregate PII redaction statistics across all requests.
// Category counts are aggregated by parsing the per-request JSON strings.
func (d *DB) GetPIIStats() PIIStats {
	var stats PIIStats
	stats.Categories = make(map[string]int)

	// Total requests scanned (all requests since PII was enabled at least once)
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&stats.TotalRequestsScanned)

	// Requests with redactions and total items
	_ = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(pii_redacted_count), 0)
		FROM requests
		WHERE pii_redacted_count > 0`,
	).Scan(&stats.RequestsWithRedactions, &stats.TotalItemsRedacted)

	// Aggregate per-category counts from JSON strings
	rows, err := d.db.Query(`
		SELECT pii_categories FROM requests
		WHERE pii_redacted_count > 0 AND pii_categories != ''`)
	if err != nil {
		return stats
	}
	defer rows.Close()

	for rows.Next() {
		var catJSON string
		if err := rows.Scan(&catJSON); err != nil {
			continue
		}
		var cats map[string]int
		if err := json.Unmarshal([]byte(catJSON), &cats); err != nil {
			continue
		}
		for key, count := range cats {
			stats.Categories[key] += count
		}
	}

	return stats
}

// ── NeMo Guard (jailbreak) analytics ────────────────────────────────────────

// NemoGuardStats holds aggregate jailbreak-detection counts across all requests.
type NemoGuardStats struct {
	TotalRequestsScanned int `json:"total_requests_scanned"`
	JailbreaksDetected   int `json:"jailbreaks_detected"`
	Blocked              int `json:"blocked"` // detected requests rejected with 403
}

// GetNemoGuardStats returns aggregate jailbreak statistics.
func (d *DB) GetNemoGuardStats() NemoGuardStats {
	var s NemoGuardStats
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&s.TotalRequestsScanned)
	_ = d.db.QueryRow(`SELECT COALESCE(COUNT(*),0) FROM requests WHERE jailbreak_detected = 1`).Scan(&s.JailbreaksDetected)
	_ = d.db.QueryRow(`SELECT COALESCE(COUNT(*),0) FROM requests WHERE jailbreak_detected = 1 AND status_code = 403`).Scan(&s.Blocked)
	return s
}

// NemoGuardRecent is a single request flagged as a jailbreak.
type NemoGuardRecent struct {
	ID            int64   `json:"id"`
	Timestamp     int64   `json:"timestamp"`
	AgentID       string  `json:"agent_id"`
	Model         string  `json:"model"`
	PromptPreview string  `json:"prompt_preview"`
	Score         float64 `json:"score"`
	StatusCode    int     `json:"status_code"`
	Category      string  `json:"category"`
}

// NemoGuardDetailStats holds enriched jailbreak analytics for the details endpoint.
// Reuses the PII timeline/agent shapes (hour+count, agent+items+requests).
type NemoGuardDetailStats struct {
	Timeline []PIITimelineBucket `json:"timeline"`
	ByAgent  []PIIAgentBreakdown `json:"by_agent"`
	Recent   []NemoGuardRecent   `json:"recent"`
}

// GetNemoGuardDetails returns hourly timeline, per-agent breakdown, and a recent
// log of jailbreak detections.
func (d *DB) GetNemoGuardDetails() NemoGuardDetailStats {
	var stats NemoGuardDetailStats

	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
	tRows, err := d.db.Query(`
		SELECT (timestamp / 3600) * 3600 AS hour_bucket, COUNT(*) AS count
		FROM requests
		WHERE jailbreak_detected = 1 AND timestamp >= ?
		GROUP BY hour_bucket
		ORDER BY hour_bucket ASC`, sevenDaysAgo)
	if err == nil {
		defer tRows.Close()
		for tRows.Next() {
			var b PIITimelineBucket
			if err := tRows.Scan(&b.Hour, &b.Count); err == nil {
				stats.Timeline = append(stats.Timeline, b)
			}
		}
	}

	aRows, err := d.db.Query(`
		SELECT agent_id, COUNT(*) AS items, COUNT(*) AS reqs
		FROM requests
		WHERE jailbreak_detected = 1
		GROUP BY agent_id
		ORDER BY items DESC
		LIMIT 20`)
	if err == nil {
		defer aRows.Close()
		for aRows.Next() {
			var a PIIAgentBreakdown
			if err := aRows.Scan(&a.AgentID, &a.ItemsRedacted, &a.RequestCount); err == nil {
				stats.ByAgent = append(stats.ByAgent, a)
			}
		}
	}

	rRows, err := d.db.Query(`
		SELECT id, timestamp, agent_id, model, prompt_preview, jailbreak_score, status_code, jailbreak_category
		FROM requests
		WHERE jailbreak_detected = 1
		ORDER BY id DESC
		LIMIT 50`)
	if err == nil {
		defer rRows.Close()
		for rRows.Next() {
			var rc NemoGuardRecent
			if err := rRows.Scan(&rc.ID, &rc.Timestamp, &rc.AgentID, &rc.Model, &rc.PromptPreview, &rc.Score, &rc.StatusCode, &rc.Category); err == nil {
				stats.Recent = append(stats.Recent, rc)
			}
		}
	}

	return stats
}

// PIITimelineBucket is one hourly bucket for the PII detection timeline chart.
type PIITimelineBucket struct {
	Hour  int64 `json:"hour"`  // Unix timestamp (start of hour)
	Count int   `json:"count"` // total items redacted in that hour
}

// PIIAgentBreakdown shows PII detection counts per agent.
type PIIAgentBreakdown struct {
	AgentID       string `json:"agent_id"`
	ItemsRedacted int    `json:"items_redacted"`
	RequestCount  int    `json:"request_count"`
}

// PIIRecentDetection is a single request that had PII redactions.
type PIIRecentDetection struct {
	ID               int64          `json:"id"`
	Timestamp        int64          `json:"timestamp"`
	AgentID          string         `json:"agent_id"`
	Model            string         `json:"model"`
	PromptPreview    string         `json:"prompt_preview"`
	PIIRedactedCount int            `json:"pii_redacted_count"`
	PIICategories    map[string]int `json:"pii_categories"`
}

// PIIDetailStats holds enriched PII analytics for the details endpoint.
type PIIDetailStats struct {
	Timeline []PIITimelineBucket  `json:"timeline"`
	ByAgent  []PIIAgentBreakdown  `json:"by_agent"`
	Recent   []PIIRecentDetection `json:"recent"`
}

// GetPIIDetails returns enriched PII analytics: hourly timeline, per-agent
// breakdown, and recent detection log.
func (d *DB) GetPIIDetails() PIIDetailStats {
	var stats PIIDetailStats

	// ── Timeline: hourly buckets over last 7 days ────────────────────
	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
	tRows, err := d.db.Query(`
		SELECT (timestamp / 3600) * 3600 AS hour_bucket,
		       SUM(pii_redacted_count) AS count
		FROM requests
		WHERE pii_redacted_count > 0 AND timestamp >= ?
		GROUP BY hour_bucket
		ORDER BY hour_bucket ASC`, sevenDaysAgo)
	if err == nil {
		defer tRows.Close()
		for tRows.Next() {
			var b PIITimelineBucket
			if err := tRows.Scan(&b.Hour, &b.Count); err == nil {
				stats.Timeline = append(stats.Timeline, b)
			}
		}
	}

	// ── By agent ─────────────────────────────────────────────────────
	aRows, err := d.db.Query(`
		SELECT agent_id,
		       SUM(pii_redacted_count) AS items,
		       COUNT(*) AS reqs
		FROM requests
		WHERE pii_redacted_count > 0
		GROUP BY agent_id
		ORDER BY items DESC
		LIMIT 20`)
	if err == nil {
		defer aRows.Close()
		for aRows.Next() {
			var a PIIAgentBreakdown
			if err := aRows.Scan(&a.AgentID, &a.ItemsRedacted, &a.RequestCount); err == nil {
				stats.ByAgent = append(stats.ByAgent, a)
			}
		}
	}

	// ── Recent detections ────────────────────────────────────────────
	rRows, err := d.db.Query(`
		SELECT id, timestamp, agent_id, model, prompt_preview,
		       pii_redacted_count, pii_categories
		FROM requests
		WHERE pii_redacted_count > 0
		ORDER BY id DESC
		LIMIT 50`)
	if err == nil {
		defer rRows.Close()
		for rRows.Next() {
			var r PIIRecentDetection
			var catJSON string
			if err := rRows.Scan(&r.ID, &r.Timestamp, &r.AgentID, &r.Model,
				&r.PromptPreview, &r.PIIRedactedCount, &catJSON); err == nil {
				r.PIICategories = make(map[string]int)
				if catJSON != "" {
					json.Unmarshal([]byte(catJSON), &r.PIICategories)
				}
				stats.Recent = append(stats.Recent, r)
			}
		}
	}

	return stats
}

// ═══════════════════════════════════════════════════════════════════════════════
// Settings Persistence
// ═══════════════════════════════════════════════════════════════════════════════

// SaveSettings persists the settings JSON to the database.
// Uses INSERT OR REPLACE with id=1 so there's always exactly one row.
func (d *DB) SaveSettings(jsonData []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.db.Exec(
		`INSERT INTO settings (id, data, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		string(jsonData), time.Now().Unix(),
	)
	return err
}

// LoadSettings returns the persisted settings JSON, or nil if none saved.
func (d *DB) LoadSettings() ([]byte, error) {
	var data string
	err := d.db.QueryRow(`SELECT data FROM settings WHERE id = 1`).Scan(&data)
	if err != nil {
		return nil, nil // no settings saved — not an error
	}
	return []byte(data), nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Prompt Version Tracking
// ═══════════════════════════════════════════════════════════════════════════════

// PromptVersionRow represents a versioned snapshot of an agent's system prompt.
type PromptVersionRow struct {
	ID           int64
	AgentID      string
	AppName      string
	ContentHash  string
	Content      string
	PreviousHash string
	Provider     string
	Model        string
	CreatedAt    time.Time
}

// HashSystemPrompt returns a SHA-256 hex hash of the given system prompt content.
func HashSystemPrompt(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// RecordPromptVersion checks if the system prompt for an agent has changed
// since the last recorded version. If the hash differs (or no version exists
// yet), a new version row is inserted. Returns (versionID, isNew, error).
func (d *DB) RecordPromptVersion(agentID, appName, content, provider, model string) (int64, bool, error) {
	contentHash := HashSystemPrompt(content)

	// Read-only check first (outside lock) — most calls early-exit here
	var lastHash string
	err := d.db.QueryRow(`
		SELECT content_hash FROM prompt_versions
		WHERE agent_id = ?
		ORDER BY created_at DESC
		LIMIT 1`, agentID).Scan(&lastHash)

	if err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}

	// Hash unchanged — no new version needed
	if lastHash == contentHash {
		return 0, false, nil
	}

	// New or changed prompt — insert version (acquire write lock)
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	id, err := d.db.InsertReturningID(`
		INSERT INTO prompt_versions (agent_id, app_name, content_hash, content, previous_hash, provider, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, appName, contentHash, content, lastHash, provider, model, time.Now().Unix(),
	)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// PromptVersionHistory returns prompt versions for an agent, newest first.
func (d *DB) PromptVersionHistory(agentID string, limit, offset int) ([]PromptVersionRow, int, error) {
	var total int
	if err := d.db.QueryRow(`
		SELECT COUNT(*) FROM prompt_versions WHERE agent_id = ?`, agentID).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := d.db.Query(`
		SELECT id, agent_id, app_name, content_hash, content, previous_hash, provider, model, created_at
		FROM prompt_versions
		WHERE agent_id = ?
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?`, agentID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []PromptVersionRow
	for rows.Next() {
		var v PromptVersionRow
		var createdAt int64
		if err := rows.Scan(&v.ID, &v.AgentID, &v.AppName, &v.ContentHash,
			&v.Content, &v.PreviousHash, &v.Provider, &v.Model, &createdAt); err != nil {
			return nil, 0, err
		}
		v.CreatedAt = time.Unix(createdAt, 0)
		result = append(result, v)
	}
	return result, total, rows.Err()
}

// PromptVersionByID returns a single prompt version by its ID.
func (d *DB) PromptVersionByID(id int64) (PromptVersionRow, error) {
	var v PromptVersionRow
	var createdAt int64
	err := d.db.QueryRow(`
		SELECT id, agent_id, app_name, content_hash, content, previous_hash, provider, model, created_at
		FROM prompt_versions WHERE id = ?`, id).
		Scan(&v.ID, &v.AgentID, &v.AppName, &v.ContentHash,
			&v.Content, &v.PreviousHash, &v.Provider, &v.Model, &createdAt)
	if err != nil {
		return v, err
	}
	v.CreatedAt = time.Unix(createdAt, 0)
	return v, nil
}

// PromptVersionsByAgent returns all prompt versions for an agent (used for diff).
func (d *DB) PromptVersionsByAgent(agentID string) ([]PromptVersionRow, error) {
	rows, err := d.db.Query(`
		SELECT id, agent_id, app_name, content_hash, content, previous_hash, provider, model, created_at
		FROM prompt_versions
		WHERE agent_id = ?
		ORDER BY created_at ASC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PromptVersionRow
	for rows.Next() {
		var v PromptVersionRow
		var createdAt int64
		if err := rows.Scan(&v.ID, &v.AgentID, &v.AppName, &v.ContentHash,
			&v.Content, &v.PreviousHash, &v.Provider, &v.Model, &createdAt); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(createdAt, 0)
		result = append(result, v)
	}
	return result, rows.Err()
}

// ═══════════════════════════════════════════════════════════════════════════════
// Agent Timeline + Trends
// ═══════════════════════════════════════════════════════════════════════════════

// TimelineEvent is a unified event in the agent's learning timeline.
type TimelineEvent struct {
	Timestamp int64                  `json:"timestamp"`
	EventType string                 `json:"event_type"` // request, prompt_change, feedback, rule_triggered, memory_created
	Detail    map[string]interface{} `json:"detail"`
}

// AgentTimeline returns a merged chronological timeline of events for an agent.
// Events: requests, prompt changes, feedback, rules triggered.
func (d *DB) AgentTimeline(agentID string, from, to int64, limit int) ([]TimelineEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var events []TimelineEvent

	// Requests
	reqQuery := `SELECT timestamp, provider, model, cost, input_tokens, output_tokens, status_code,
	                    COALESCE(error_message,'') AS error_message,
	                    COALESCE(matched_rule_id,0) AS matched_rule_id,
	                    COALESCE(cache_hit,0) AS cache_hit
	             FROM requests WHERE agent_id = ?`
	reqArgs := []interface{}{agentID}
	if from > 0 {
		reqQuery += " AND timestamp >= ?"
		reqArgs = append(reqArgs, from)
	}
	if to > 0 {
		reqQuery += " AND timestamp <= ?"
		reqArgs = append(reqArgs, to)
	}
	reqQuery += " ORDER BY timestamp DESC LIMIT ?"
	reqArgs = append(reqArgs, limit)

	rows, err := d.db.Query(reqQuery, reqArgs...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ts int64
			var provider, model, errMsg string
			var cost float64
			var inTok, outTok, status int
			var ruleID, cacheHit int
			if err := rows.Scan(&ts, &provider, &model, &cost, &inTok, &outTok, &status, &errMsg, &ruleID, &cacheHit); err != nil {
				continue
			}
			events = append(events, TimelineEvent{
				Timestamp: ts,
				EventType: "request",
				Detail: map[string]interface{}{
					"provider": provider, "model": model, "cost": cost,
					"input_tokens": inTok, "output_tokens": outTok, "status": status,
					"error": errMsg, "rule_id": ruleID, "cache_hit": cacheHit != 0,
				},
			})
		}
	}

	// Prompt changes
	pvQuery := `SELECT created_at, content_hash, previous_hash, provider, model
	            FROM prompt_versions WHERE agent_id = ?`
	pvArgs := []interface{}{agentID}
	if from > 0 {
		pvQuery += " AND created_at >= ?"
		pvArgs = append(pvArgs, from)
	}
	if to > 0 {
		pvQuery += " AND created_at <= ?"
		pvArgs = append(pvArgs, to)
	}
	pvQuery += " ORDER BY created_at DESC LIMIT ?"
	pvArgs = append(pvArgs, limit)

	pvRows, err := d.db.Query(pvQuery, pvArgs...)
	if err == nil {
		defer pvRows.Close()
		for pvRows.Next() {
			var ts int64
			var hash, prevHash, provider, model string
			if err := pvRows.Scan(&ts, &hash, &prevHash, &provider, &model); err != nil {
				continue
			}
			events = append(events, TimelineEvent{
				Timestamp: ts,
				EventType: "prompt_change",
				Detail: map[string]interface{}{
					"content_hash": hash, "previous_hash": prevHash,
					"provider": provider, "model": model,
					"is_initial": prevHash == "",
				},
			})
		}
	}

	// Feedback
	fbQuery := `SELECT created_at, outcome, score, details
	            FROM feedback WHERE agent_id = ?`
	fbArgs := []interface{}{agentID}
	if from > 0 {
		fbQuery += " AND created_at >= ?"
		fbArgs = append(fbArgs, from)
	}
	if to > 0 {
		fbQuery += " AND created_at <= ?"
		fbArgs = append(fbArgs, to)
	}
	fbQuery += " ORDER BY created_at DESC LIMIT ?"
	fbArgs = append(fbArgs, limit)

	fbRows, err := d.db.Query(fbQuery, fbArgs...)
	if err == nil {
		defer fbRows.Close()
		for fbRows.Next() {
			var ts int64
			var outcome, details string
			var score float64
			if err := fbRows.Scan(&ts, &outcome, &score, &details); err != nil {
				continue
			}
			events = append(events, TimelineEvent{
				Timestamp: ts,
				EventType: "feedback",
				Detail: map[string]interface{}{
					"outcome": outcome, "score": score, "details": details,
				},
			})
		}
	}

	// Sort by timestamp descending (merge the three result sets)
	sortTimelineEvents(events)

	// Cap total events
	if len(events) > limit {
		events = events[:limit]
	}

	return events, nil
}

// sortTimelineEvents sorts events by timestamp descending (newest first).
func sortTimelineEvents(events []TimelineEvent) {
	for i := 1; i < len(events); i++ {
		key := events[i]
		j := i - 1
		for j >= 0 && events[j].Timestamp < key.Timestamp {
			events[j+1] = events[j]
			j--
		}
		events[j+1] = key
	}
}

// DailyTrend holds aggregated metrics for one day.
type DailyTrend struct {
	Date         string  `json:"date"` // YYYY-MM-DD
	Requests     int     `json:"requests"`
	Cost         float64 `json:"cost"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	AvgLatencyMs int64   `json:"avg_latency_ms"`
	ErrorCount   int     `json:"error_count"`
	CacheHits    int     `json:"cache_hits"`
}

// AgentTrends returns daily aggregated metrics for an agent over the given period.
func (d *DB) AgentTrends(agentID string, days int) ([]DailyTrend, error) {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()

	rows, err := d.db.Query(fmt.Sprintf(`
		SELECT
			%s AS day,
			COUNT(*) AS requests,
			COALESCE(SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END), 0) AS cost,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(AVG(latency_ms), 0) AS avg_latency,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS errors,
			COALESCE(SUM(CASE WHEN cache_hit = 1 THEN 1 ELSE 0 END), 0) AS cache_hits
		FROM requests
		WHERE agent_id = ? AND timestamp >= ?
		GROUP BY day
		ORDER BY day ASC`, d.db.Dialect().EpochToDate("timestamp")), agentID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyTrend
	for rows.Next() {
		var t DailyTrend
		var avgLatency float64
		if err := rows.Scan(&t.Date, &t.Requests, &t.Cost, &t.InputTokens,
			&t.OutputTokens, &avgLatency, &t.ErrorCount, &t.CacheHits); err != nil {
			return nil, err
		}
		t.AvgLatencyMs = int64(avgLatency)
		result = append(result, t)
	}
	return result, rows.Err()
}

// CostBucket holds the cumulative cost at a point in time.
type CostBucket struct {
	Label          string  `json:"label"`           // human-readable timestamp
	TimestampUnix  int64   `json:"timestamp_unix"`  // bucket start (unix)
	Cost           float64 `json:"cost"`            // cost in this bucket
	CumulativeCost float64 `json:"cumulative_cost"` // running total up to this bucket
}

// CostOverTime returns cumulative cost data points for the dashboard chart.
// bucketMinutes controls granularity (e.g. 60 = hourly, 15 = quarter-hour).
// hours controls how far back to look (default 24).
func (d *DB) CostOverTime(bucketMinutes, hours int) ([]CostBucket, error) {
	if bucketMinutes <= 0 {
		bucketMinutes = 60
	}
	if hours <= 0 {
		hours = 24
	}
	bucketSec := int64(bucketMinutes * 60)
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()

	// Bucket requests by time intervals: floor(timestamp / bucketSec) * bucketSec
	rows, err := d.db.Query(`
		SELECT
			(timestamp / ? ) * ? AS bucket_start,
			COALESCE(SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END), 0) AS bucket_cost
		FROM requests
		WHERE timestamp >= ?
		GROUP BY bucket_start
		ORDER BY bucket_start ASC`, bucketSec, bucketSec, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Also need cumulative cost before the window for correct running total
	var priorCost float64
	_ = d.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN cache_hit = 0 THEN cost ELSE 0 END), 0) FROM requests WHERE timestamp < ?`, cutoff).Scan(&priorCost)

	var result []CostBucket
	cumulative := priorCost
	for rows.Next() {
		var bucketStart int64
		var cost float64
		if err := rows.Scan(&bucketStart, &cost); err != nil {
			return nil, err
		}
		cumulative += cost
		t := time.Unix(bucketStart, 0).Local()
		label := t.Format("15:04")
		if hours > 24 {
			label = t.Format("Jan 2 15:04")
		}
		result = append(result, CostBucket{
			Label:          label,
			TimestampUnix:  bucketStart,
			Cost:           cost,
			CumulativeCost: cumulative,
		})
	}
	return result, rows.Err()
}

// ═══════════════════════════════════════════════════════════════════════════════
// Outcome Feedback
// ═══════════════════════════════════════════════════════════════════════════════

// FeedbackRow represents a submitted outcome for a request or session.
type FeedbackRow struct {
	ID        int64
	SessionID string
	RequestID int64
	AgentID   string
	AppName   string
	Outcome   string  // "success", "failure", "partial"
	Score     float64 // optional numeric score (0-1)
	Details   string  // human-readable outcome description
	Metadata  string  // JSON-encoded arbitrary domain data
	CreatedAt time.Time
}

// InsertFeedback stores a new feedback record.
func (d *DB) InsertFeedback(f FeedbackRow) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	return d.db.InsertReturningID(`
		INSERT INTO feedback (session_id, request_id, agent_id, app_name, outcome, score, details, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.SessionID, f.RequestID, f.AgentID, f.AppName,
		f.Outcome, f.Score, f.Details, f.Metadata, time.Now().Unix(),
	)
}

// QueryFeedback returns feedback filtered by optional agent_id, outcome, and time range.
func (d *DB) QueryFeedback(agentID, outcome string, from, to int64, limit, offset int) ([]FeedbackRow, int, error) {
	where := "1=1"
	var args []interface{}

	if agentID != "" {
		where += " AND agent_id = ?"
		args = append(args, agentID)
	}
	if outcome != "" {
		where += " AND outcome = ?"
		args = append(args, outcome)
	}
	if from > 0 {
		where += " AND created_at >= ?"
		args = append(args, from)
	}
	if to > 0 {
		where += " AND created_at <= ?"
		args = append(args, to)
	}

	// Count total
	var total int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM feedback WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Fetch page
	query := fmt.Sprintf(`
		SELECT id, session_id, request_id, agent_id, app_name, outcome, score, details, metadata, created_at
		FROM feedback WHERE %s
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?`, where)
	args = append(args, limit, offset)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []FeedbackRow
	for rows.Next() {
		var f FeedbackRow
		var createdAt int64
		if err := rows.Scan(&f.ID, &f.SessionID, &f.RequestID, &f.AgentID,
			&f.AppName, &f.Outcome, &f.Score, &f.Details, &f.Metadata, &createdAt); err != nil {
			return nil, 0, err
		}
		f.CreatedAt = time.Unix(createdAt, 0)
		result = append(result, f)
	}
	return result, total, rows.Err()
}

// AgentAccuracy holds accuracy statistics for one agent.
type AgentAccuracy struct {
	AgentID      string
	TotalCount   int
	SuccessCount int
	FailureCount int
	PartialCount int
	AvgScore     float64
	SuccessRate  float64
}

// GetAgentAccuracy returns accuracy stats based on feedback for an agent.
// If agentID is empty, returns stats aggregated across all agents.
func (d *DB) GetAgentAccuracy(agentID string, sinceDays int) (AgentAccuracy, error) {
	var a AgentAccuracy
	a.AgentID = agentID

	where := "1=1"
	var args []interface{}

	if agentID != "" {
		where += " AND agent_id = ?"
		args = append(args, agentID)
	}
	if sinceDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -sinceDays).Unix()
		where += " AND created_at >= ?"
		args = append(args, cutoff)
	}

	err := d.db.QueryRow(fmt.Sprintf(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN outcome='success' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome='failure' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome='partial' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(CASE WHEN score > 0 THEN score ELSE NULL END), 0)
		FROM feedback WHERE %s`, where), args...).
		Scan(&a.TotalCount, &a.SuccessCount, &a.FailureCount, &a.PartialCount, &a.AvgScore)
	if err != nil {
		return a, err
	}

	if a.TotalCount > 0 {
		a.SuccessRate = float64(a.SuccessCount) / float64(a.TotalCount)
	}
	return a, nil
}
