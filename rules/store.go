package rules

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RuleStore handles SQLite persistence for rules.
// It shares the proxy's existing *sql.DB connection (WAL mode, already open).
type RuleStore struct {
	db *sql.DB
}

// NewRuleStore creates a RuleStore using the provided DB connection.
func NewRuleStore(db *sql.DB) *RuleStore {
	return &RuleStore{db: db}
}

// List returns all rules ordered by priority DESC, id ASC (same order as evaluation).
func (rs *RuleStore) List() ([]*Rule, error) {
	rows, err := rs.db.Query(`
		SELECT id, name, enabled, priority, scope_agent_id,
		       conditions_json, action_json, description, evidence, created_at, updated_at
		FROM rules
		ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("rules list: %w", err)
	}
	defer rows.Close()

	var rules []*Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// GetByID returns a single rule by ID.
func (rs *RuleStore) GetByID(id int64) (*Rule, error) {
	row := rs.db.QueryRow(`
		SELECT id, name, enabled, priority, scope_agent_id,
		       conditions_json, action_json, description, evidence, created_at, updated_at
		FROM rules WHERE id = ?`, id)
	return scanRule(row)
}

// Create inserts a new rule and returns its auto-assigned ID.
func (rs *RuleStore) Create(r *Rule) (int64, error) {
	condJSON, err := json.Marshal(r.Conditions)
	if err != nil {
		return 0, fmt.Errorf("rules create: marshal conditions: %w", err)
	}
	actJSON, err := json.Marshal(r.Action)
	if err != nil {
		return 0, fmt.Errorf("rules create: marshal action: %w", err)
	}

	now := time.Now().Unix()
	res, err := rs.db.Exec(`
		INSERT INTO rules
		    (name, enabled, priority, scope_agent_id, conditions_json, action_json, description, evidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, boolToInt(r.Enabled), r.Priority, r.ScopeAgentID,
		string(condJSON), string(actJSON), r.Description, r.Evidence, now, now)
	if err != nil {
		return 0, fmt.Errorf("rules create: %w", err)
	}
	return res.LastInsertId()
}

// Update replaces all mutable fields of an existing rule.
func (rs *RuleStore) Update(r *Rule) error {
	condJSON, err := json.Marshal(r.Conditions)
	if err != nil {
		return fmt.Errorf("rules update: marshal conditions: %w", err)
	}
	actJSON, err := json.Marshal(r.Action)
	if err != nil {
		return fmt.Errorf("rules update: marshal action: %w", err)
	}

	now := time.Now().Unix()
	_, err = rs.db.Exec(`
		UPDATE rules
		SET name=?, enabled=?, priority=?, scope_agent_id=?,
		    conditions_json=?, action_json=?, description=?, evidence=?, updated_at=?
		WHERE id=?`,
		r.Name, boolToInt(r.Enabled), r.Priority, r.ScopeAgentID,
		string(condJSON), string(actJSON), r.Description, r.Evidence, now, r.ID)
	return err
}

// Delete permanently removes a rule by ID.
func (rs *RuleStore) Delete(id int64) error {
	_, err := rs.db.Exec(`DELETE FROM rules WHERE id = ?`, id)
	return err
}

// Toggle flips the enabled flag for a rule without touching other fields.
func (rs *RuleStore) Toggle(id int64, enabled bool) error {
	_, err := rs.db.Exec(`UPDATE rules SET enabled=?, updated_at=? WHERE id=?`,
		boolToInt(enabled), time.Now().Unix(), id)
	return err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// scanner abstracts *sql.Row and *sql.Rows so scanRule works for both.
type scanner interface {
	Scan(dest ...interface{}) error
}

func scanRule(s scanner) (*Rule, error) {
	var (
		r           Rule
		enabledInt  int
		condJSON    string
		actJSON     string
		createdUnix int64
		updatedUnix int64
	)
	if err := s.Scan(
		&r.ID, &r.Name, &enabledInt, &r.Priority, &r.ScopeAgentID,
		&condJSON, &actJSON, &r.Description, &r.Evidence, &createdUnix, &updatedUnix,
	); err != nil {
		return nil, fmt.Errorf("rules scan: %w", err)
	}

	r.Enabled = enabledInt != 0
	r.CreatedAt = time.Unix(createdUnix, 0)
	r.UpdatedAt = time.Unix(updatedUnix, 0)

	if err := json.Unmarshal([]byte(condJSON), &r.Conditions); err != nil {
		return nil, fmt.Errorf("rules scan conditions_json (id=%d): %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(actJSON), &r.Action); err != nil {
		return nil, fmt.Errorf("rules scan action_json (id=%d): %w", r.ID, err)
	}
	return &r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
