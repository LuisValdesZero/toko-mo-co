package rules

import (
	"database/sql"
	"os"
	"testing"

	"tokomoco/detector"
	"tokomoco/store"
	"tokomoco/tracker"

	_ "modernc.org/sqlite"
)

// setupTestDB creates an in-memory SQLite database with the rules table.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE rules (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT    NOT NULL,
			enabled         INTEGER NOT NULL DEFAULT 1,
			priority        INTEGER NOT NULL DEFAULT 0,
			scope_agent_id  TEXT    NOT NULL DEFAULT '',
			conditions_json TEXT    NOT NULL,
			action_json     TEXT    NOT NULL,
			description     TEXT    NOT NULL DEFAULT '',
			evidence        TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("failed to create rules table: %v", err)
	}
	return db
}

// TestEngineBasicEvaluation verifies that rules are evaluated in priority order.
func TestEngineBasicEvaluation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	// Create a simple rule: block if agent_id matches "test-agent"
	rule := &Rule{
		Name:         "Block test agent",
		Enabled:      true,
		Priority:     10,
		ScopeAgentID: "",
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test-agent", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  403,
			BlockMessage: "Test agent blocked",
		},
	}

	id, err := store.Create(rule)
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}
	t.Logf("Created rule %d", id)

	engine.Reload()

	// Test context that matches the rule
	ctx := &RuleContext{
		AgentID:  "test-agent",
		Provider: "openai",
		Model:    "gpt-4",
	}

	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Errorf("expected rule to match, but it didn't")
	}
	if result.Action != ActionBlock {
		t.Errorf("expected ActionBlock, got %v", result.Action)
	}
	if result.BlockStatus != 403 {
		t.Errorf("expected BlockStatus 403, got %d", result.BlockStatus)
	}

	// Test context that doesn't match
	ctx2 := &RuleContext{
		AgentID:  "other-agent",
		Provider: "openai",
		Model:    "gpt-4",
	}

	result2, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Errorf("expected no match, but rule matched")
	}
	if result2.Action != ActionAllow {
		t.Errorf("expected ActionAllow when no match, got %v", result2.Action)
	}
}

// TestRulePriority verifies that higher priority rules are evaluated first.
func TestRulePriority(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	// Create two rules with different priorities
	lowPriorityRule := &Rule{
		Name:     "Low priority allow",
		Enabled:  true,
		Priority: 1,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test", Mode: MatchExact},
		},
		Action: ActionSpec{Type: ActionAllow},
	}

	highPriorityRule := &Rule{
		Name:     "High priority block",
		Enabled:  true,
		Priority: 100,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  403,
			BlockMessage: "Blocked by high priority rule",
		},
	}

	store.Create(lowPriorityRule)
	store.Create(highPriorityRule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "test"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Errorf("expected rule to match")
	}
	// The high priority block should win
	if result.Action != ActionBlock {
		t.Errorf("expected ActionBlock from high priority rule, got %v", result.Action)
	}
	if result.MatchedRule.Name != "High priority block" {
		t.Errorf("expected high priority rule to match, got %s", result.MatchedRule.Name)
	}
}

// TestModelOverride verifies that model override works.
func TestModelOverride(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Override expensive model",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondModel, Value: "gpt-4", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:          ActionOverrideModel,
			OverrideModel: "gpt-3.5-turbo",
		},
	}

	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{Model: "gpt-4"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Errorf("expected rule to match")
	}
	if result.Action != ActionOverrideModel {
		t.Errorf("expected ActionOverrideModel, got %v", result.Action)
	}
	if result.OverrideModel != "gpt-3.5-turbo" {
		t.Errorf("expected override model gpt-3.5-turbo, got %s", result.OverrideModel)
	}
}

// TestInputTokensCondition verifies numeric conditions work.
func TestInputTokensCondition(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block large requests",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondInputTokens, Threshold: 1000, Op: "gt"},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  429,
			BlockMessage: "Request too large",
		},
	}

	store.Create(rule)
	engine.Reload()

	// Test with tokens > 1000
	ctx := &RuleContext{InputTokens: 1500}
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Errorf("expected rule to match for 1500 tokens")
	}
	if result.Action != ActionBlock {
		t.Errorf("expected ActionBlock, got %v", result.Action)
	}

	// Test with tokens <= 1000
	ctx2 := &RuleContext{InputTokens: 500}
	result2, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Errorf("expected no match for 500 tokens")
	}
	if result2.Action != ActionAllow {
		t.Errorf("expected ActionAllow, got %v", result2.Action)
	}
}

// TestLoopDetectedCondition verifies loop detection integration.
func TestLoopDetectedCondition(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block when loop detected",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondLoopDetected},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  429,
			BlockMessage: "Loop detected",
		},
	}

	store.Create(rule)
	engine.Reload()

	// Test with loop detected
	ctx := &RuleContext{
		LoopResult: detector.LoopDetectionResult{
			LoopDetected: true,
			Severity:     "high",
		},
	}
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Errorf("expected rule to match when loop detected")
	}
	if result.Action != ActionBlock {
		t.Errorf("expected ActionBlock, got %v", result.Action)
	}

	// Test without loop
	ctx2 := &RuleContext{
		LoopResult: detector.LoopDetectionResult{
			LoopDetected: false,
		},
	}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Errorf("expected no match when no loop")
	}
}

// TestCostSessionCondition verifies session cost tracking.
func TestCostSessionCondition(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block expensive sessions",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondCostSession, Threshold: 10.0, Op: "gte"},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  402,
			BlockMessage: "Session budget exceeded",
		},
	}

	store.Create(rule)
	engine.Reload()

	// Create a SessionTracker and get a session
	st := tracker.NewSessionTracker(24*3600, 1000)
	session := st.GetOrCreateSession("test-session-1")
	session.AddCost(15.0, 1000, 2000)

	ctx := &RuleContext{
		Session: session,
	}

	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Errorf("expected rule to match for high-cost session")
	}
	if result.Action != ActionBlock {
		t.Errorf("expected ActionBlock, got %v", result.Action)
	}

	// Test with low-cost session
	session2 := st.GetOrCreateSession("test-session-2")
	session2.AddCost(5.0, 500, 1000)

	ctx2 := &RuleContext{
		Session: session2,
	}

	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Errorf("expected no match for low-cost session")
	}
}

// TestDisabledRule verifies that disabled rules are not evaluated.
func TestDisabledRule(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Disabled block rule",
		Enabled:  false, // Disabled
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  403,
			BlockMessage: "Should not block",
		},
	}

	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "test"}
	result, matched := engine.Evaluate(ctx)

	if matched {
		t.Errorf("expected disabled rule not to match")
	}
	if result.Action != ActionAllow {
		t.Errorf("expected ActionAllow for disabled rule, got %v", result.Action)
	}
}

// TestScopedRule verifies that scope_agent_id filtering works.
func TestScopedRule(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:         "Scoped to agent-123",
		Enabled:      true,
		Priority:     10,
		ScopeAgentID: "agent-123",
		Conditions:   []ConditionSpec{}, // Always match if scope matches
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  403,
			BlockMessage: "Scoped block",
		},
	}

	store.Create(rule)
	engine.Reload()

	// Test with matching agent
	ctx := &RuleContext{AgentID: "agent-123"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Errorf("expected scoped rule to match for agent-123")
	}

	// Test with non-matching agent
	ctx2 := &RuleContext{AgentID: "agent-456"}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Errorf("expected scoped rule not to match for agent-456")
	}
}

// BenchmarkEngineEvaluate benchmarks the hot path evaluation.
func BenchmarkEngineEvaluate(b *testing.B) {
	db, _ := sql.Open("sqlite", ":memory:")
	defer db.Close()

	db.Exec(`CREATE TABLE rules (
		id INTEGER PRIMARY KEY, name TEXT, enabled INTEGER, priority INTEGER,
		scope_agent_id TEXT, conditions_json TEXT, action_json TEXT,
		description TEXT, created_at INTEGER, updated_at INTEGER
	)`)

	store := NewRuleStore(store.NewQuerier(db, store.SQLite))

	// Create 10 rules
	for i := 0; i < 10; i++ {
		rule := &Rule{
			Name:     "Rule " + string(rune('A'+i)),
			Enabled:  true,
			Priority: i,
			Conditions: []ConditionSpec{
				{Type: CondAgentID, Value: "never-match", Mode: MatchExact},
			},
			Action: ActionSpec{Type: ActionAllow},
		}
		store.Create(rule)
	}

	engine, _ := NewEngine(store)
	defer engine.Close()

	ctx := &RuleContext{
		AgentID:     "test-agent",
		Provider:    "openai",
		Model:       "gpt-4",
		InputTokens: 100,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Evaluate(ctx)
	}
}

func TestMain(m *testing.M) {
	// Ensure clean environment for tests
	os.Exit(m.Run())
}
