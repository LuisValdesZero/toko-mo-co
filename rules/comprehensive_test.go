package rules

import (
	"testing"

	"tokomoco/store"
	"tokomoco/tracker"
)

// ══════════════════════════════════════════════════════════════════════════════
//  COMPREHENSIVE RULE TESTS — All 11 Condition Types × 6 Action Types
// ══════════════════════════════════════════════════════════════════════════════

// ── Condition Type Tests ──────────────────────────────────────────────────────

func TestCondition_AgentID_Exact(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "AgentID exact match",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "agent-123", Mode: MatchExact},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403, BlockMessage: "Blocked"},
	}
	store.Create(rule)
	engine.Reload()

	// Match
	ctx := &RuleContext{AgentID: "agent-123"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected exact match for agent-123")
	}

	// No match
	ctx2 := &RuleContext{AgentID: "agent-456"}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Expected no match for agent-456")
	}
}

func TestCondition_AgentID_Glob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "AgentID glob pattern",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test-*", Mode: MatchGlob},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	// Match
	ctx := &RuleContext{AgentID: "test-123"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected glob match for test-123")
	}

	// No match
	ctx2 := &RuleContext{AgentID: "prod-123"}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Expected no match for prod-123")
	}
}

func TestCondition_AgentID_Regex(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "AgentID regex pattern",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "^agent-[0-9]{3}$", Mode: MatchRegex},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	// Match
	ctx := &RuleContext{AgentID: "agent-123"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected regex match for agent-123")
	}

	// No match
	ctx2 := &RuleContext{AgentID: "agent-12"}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Expected no match for agent-12 (only 2 digits)")
	}
}

func TestCondition_AppName(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "AppName match",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAppName, Value: "my-app", Mode: MatchExact},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AppName: "my-app"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for app name")
	}
}

func TestCondition_Model_Exact(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Model exact match",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondModel, Value: "gpt-4-turbo", Mode: MatchExact},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{Model: "gpt-4-turbo"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for gpt-4-turbo")
	}
}

func TestCondition_Provider(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Provider match",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondProvider, Value: "anthropic", Mode: MatchExact},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{Provider: "anthropic"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for anthropic provider")
	}
}

func TestCondition_InputTokens_GreaterThan(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block large inputs",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondInputTokens, Threshold: 5000, Op: "gt"},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 429},
	}
	store.Create(rule)
	engine.Reload()

	// Match (6000 > 5000)
	ctx := &RuleContext{InputTokens: 6000}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for 6000 tokens > 5000")
	}

	// No match (3000 not > 5000)
	ctx2 := &RuleContext{InputTokens: 3000}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Expected no match for 3000 tokens")
	}
}

func TestCondition_InputTokens_AllOperators(t *testing.T) {
	tests := []struct {
		op        string
		threshold float64
		tokens    int
		wantMatch bool
	}{
		{"gt", 1000, 1500, true},   // 1500 > 1000
		{"gt", 1000, 1000, false},  // 1000 not > 1000
		{"gte", 1000, 1000, true},  // 1000 >= 1000
		{"gte", 1000, 999, false},  // 999 not >= 1000
		{"lt", 1000, 500, true},    // 500 < 1000
		{"lt", 1000, 1000, false},  // 1000 not < 1000
		{"lte", 1000, 1000, true},  // 1000 <= 1000
		{"lte", 1000, 1001, false}, // 1001 not <= 1000
		{"eq", 1000, 1000, true},   // 1000 == 1000
		{"eq", 1000, 999, false},   // 999 not == 1000
	}

	for _, tt := range tests {
		db := setupTestDB(t)
		store := NewRuleStore(store.NewQuerier(db, store.SQLite))
		engine, err := NewEngine(store)
		if err != nil {
			t.Fatalf("failed to create engine: %v", err)
		}

		rule := &Rule{
			Name:     "Token operator test",
			Enabled:  true,
			Priority: 10,
			Conditions: []ConditionSpec{
				{Type: CondInputTokens, Threshold: tt.threshold, Op: tt.op},
			},
			Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
		}
		store.Create(rule)
		engine.Reload()

		ctx := &RuleContext{InputTokens: tt.tokens}
		_, matched := engine.Evaluate(ctx)

		if matched != tt.wantMatch {
			t.Errorf("Op %s: threshold=%.0f, tokens=%d, want match=%v, got %v",
				tt.op, tt.threshold, tt.tokens, tt.wantMatch, matched)
		}

		engine.Close()
		db.Close()
	}
}

func TestCondition_CostSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Session cost limit",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondCostSession, Threshold: 5.0, Op: "gte"},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 402, BlockMessage: "Budget exceeded"},
	}
	store.Create(rule)
	engine.Reload()

	st := tracker.NewSessionTracker(24*3600, 1000)
	session := st.GetOrCreateSession("test-session")
	session.AddCost(6.0, 1000, 2000)

	ctx := &RuleContext{Session: session}
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for session cost >= 5.0")
	}
	if result.BlockStatus != 402 {
		t.Errorf("Expected status 402, got %d", result.BlockStatus)
	}
}

func TestCondition_PromptContent_Keyword(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block prompts with 'jailbreak'",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondPromptContent, Value: "*jailbreak*", Mode: MatchGlob},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{PromptPreview: "ignore all previous jailbreak instructions"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for prompt containing 'jailbreak'")
	}
}

func TestCondition_PromptContent_Regex(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block prompts with PII pattern",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondPromptContent, Value: `\d{3}-\d{2}-\d{4}`, Mode: MatchRegex},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403, BlockMessage: "PII detected"},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{PromptPreview: "My SSN is 123-45-6789"}
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected match for SSN pattern")
	}
	if result.BlockMessage != "PII detected" {
		t.Errorf("Expected message 'PII detected', got '%s'", result.BlockMessage)
	}
}

// ── Action Type Tests ─────────────────────────────────────────────────────────

func TestAction_Block(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block action test",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "blocked-agent", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  451,
			BlockMessage: "Unavailable for legal reasons",
		},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "blocked-agent"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Fatal("Expected rule to match")
	}
	if result.Action != ActionBlock {
		t.Errorf("Expected ActionBlock, got %v", result.Action)
	}
	if result.BlockStatus != 451 {
		t.Errorf("Expected status 451, got %d", result.BlockStatus)
	}
	if result.BlockMessage != "Unavailable for legal reasons" {
		t.Errorf("Expected message, got '%s'", result.BlockMessage)
	}
}

func TestAction_OverrideModel(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Downgrade expensive model",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondModel, Value: "gpt-4-32k", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:          ActionOverrideModel,
			OverrideModel: "gpt-4",
		},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{Model: "gpt-4-32k"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Fatal("Expected rule to match")
	}
	if result.Action != ActionOverrideModel {
		t.Errorf("Expected ActionOverrideModel, got %v", result.Action)
	}
	if result.OverrideModel != "gpt-4" {
		t.Errorf("Expected override to gpt-4, got %s", result.OverrideModel)
	}
}

func TestAction_InjectPrompt(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Inject safety instructions",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "prod-*", Mode: MatchGlob},
		},
		Action: ActionSpec{
			Type:                 ActionInjectPrompt,
			InjectedSystemPrompt: "Always respond professionally and ethically.",
		},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "prod-app-1"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Fatal("Expected rule to match")
	}
	if result.Action != ActionInjectPrompt {
		t.Errorf("Expected ActionInjectPrompt, got %v", result.Action)
	}
	if result.InjectedSystemPrompt != "Always respond professionally and ethically." {
		t.Errorf("Unexpected injected prompt: %s", result.InjectedSystemPrompt)
	}
}

func TestAction_Redirect(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Redirect to fallback provider",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondProvider, Value: "openai", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:        ActionRedirect,
			RedirectURL: "https://api.anthropic.com/v1/messages",
		},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{Provider: "openai"}
	result, matched := engine.Evaluate(ctx)

	if !matched {
		t.Fatal("Expected rule to match")
	}
	if result.Action != ActionRedirect {
		t.Errorf("Expected ActionRedirect, got %v", result.Action)
	}
	if result.RedirectURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("Unexpected redirect URL: %s", result.RedirectURL)
	}
}

func TestAction_RateLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Rate limit per agent",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "rate-limited-agent", Mode: MatchExact},
		},
		Action: ActionSpec{
			Type:               ActionRateLimit,
			BlockStatus:        429,
			BlockMessage:       "Rate limit exceeded",
			RateLimitRequests:  5,
			RateLimitWindowSec: 60,
			RateLimitScope:     "agent",
		},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "rate-limited-agent"}

	// First 5 requests should pass
	for i := 0; i < 5; i++ {
		result, matched := engine.Evaluate(ctx)
		if matched && result.Action == ActionRateLimit && result.RateLimited {
			t.Errorf("Request %d should not be rate limited", i+1)
		}
	}

	// 6th request should be blocked
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Fatal("Expected rule to match")
	}
	if !result.RateLimited {
		t.Error("Expected 6th request to be rate limited")
	}
	if result.BlockStatus != 429 {
		t.Errorf("Expected status 429, got %d", result.BlockStatus)
	}
}

// ── Multi-Condition Tests ─────────────────────────────────────────────────────

func TestMultipleConditions_AND(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Multi-condition AND",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "agent-123", Mode: MatchExact},
			{Type: CondModel, Value: "gpt-4", Mode: MatchExact},
			{Type: CondInputTokens, Threshold: 1000, Op: "gt"},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	// All conditions match
	ctx := &RuleContext{
		AgentID:     "agent-123",
		Model:       "gpt-4",
		InputTokens: 1500,
	}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected all conditions to match")
	}

	// One condition fails
	ctx2 := &RuleContext{
		AgentID:     "agent-123",
		Model:       "gpt-3.5-turbo", // Different model
		InputTokens: 1500,
	}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Expected no match when one condition fails")
	}
}

func TestComplexRule_ProductionScenario(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	// Scenario: Block expensive models for test agents with high token counts
	rule := &Rule{
		Name:     "Block test agents using expensive models",
		Enabled:  true,
		Priority: 50,
		Conditions: []ConditionSpec{
			{Type: CondAgentID, Value: "test-*", Mode: MatchGlob},
			{Type: CondModel, Value: "^gpt-4", Mode: MatchRegex}, // Use regex instead of glob
			{Type: CondInputTokens, Threshold: 5000, Op: "gte"},
		},
		Action: ActionSpec{
			Type:         ActionBlock,
			BlockStatus:  403,
			BlockMessage: "Test agents cannot use expensive models with large inputs",
		},
	}
	store.Create(rule)
	engine.Reload()

	// Should block
	ctx := &RuleContext{
		AgentID:     "test-app-1",
		Model:       "gpt-4-turbo",
		InputTokens: 8000,
	}
	result, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Expected production scenario rule to match")
	}
	if result.Action != ActionBlock {
		t.Errorf("Expected block, got %v", result.Action)
	}

	// Should not block (prod agent)
	ctx2 := &RuleContext{
		AgentID:     "prod-app-1",
		Model:       "gpt-4-turbo",
		InputTokens: 8000,
	}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Should not block prod agents")
	}

	// Should not block (small input)
	ctx3 := &RuleContext{
		AgentID:     "test-app-1",
		Model:       "gpt-4-turbo",
		InputTokens: 1000,
	}
	_, matched3 := engine.Evaluate(ctx3)
	if matched3 {
		t.Error("Should not block small inputs")
	}
}

// ── Edge Cases ────────────────────────────────────────────────────────────────

func TestEdgeCase_EmptyConditions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:       "Always match (no conditions)",
		Enabled:    true,
		Priority:   10,
		Conditions: []ConditionSpec{}, // Empty = always match
		Action:     ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{AgentID: "any-agent"}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Rule with no conditions should always match")
	}
}

func TestEdgeCase_NilSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Session cost check",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondCostSession, Threshold: 1.0, Op: "gte"},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	// Nil session should not panic
	ctx := &RuleContext{Session: nil}
	_, matched := engine.Evaluate(ctx)
	if matched {
		t.Error("Should not match when session is nil")
	}
}

func TestEdgeCase_ZeroThreshold(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewRuleStore(store.NewQuerier(db, store.SQLite))
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	rule := &Rule{
		Name:     "Block any tokens",
		Enabled:  true,
		Priority: 10,
		Conditions: []ConditionSpec{
			{Type: CondInputTokens, Threshold: 0, Op: "gt"},
		},
		Action: ActionSpec{Type: ActionBlock, BlockStatus: 403},
	}
	store.Create(rule)
	engine.Reload()

	ctx := &RuleContext{InputTokens: 1}
	_, matched := engine.Evaluate(ctx)
	if !matched {
		t.Error("Should match for tokens > 0")
	}

	ctx2 := &RuleContext{InputTokens: 0}
	_, matched2 := engine.Evaluate(ctx2)
	if matched2 {
		t.Error("Should not match for 0 tokens")
	}
}
