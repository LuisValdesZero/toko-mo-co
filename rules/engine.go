package rules

import (
	"log"
	"sort"
	"sync"
	"time"

	"tokomoco/detector"
	"tokomoco/tracker"
)

// ── Context & Result ──────────────────────────────────────────────────────────

// RuleContext carries everything available at evaluation time.
// Built once per request in proxy/handler.go after loop detection.
type RuleContext struct {
	// Identity
	AgentID  string
	AppName  string
	Provider string // "openai" | "anthropic" | "gemini"
	Model    string

	// Session state (in-memory; zero DB round-trips)
	Session     *tracker.Session
	InputTokens int
	Cost        float64 // estimated cost of THIS request (pre-upstream)

	// Loop detection result from detector.LoopDetector
	LoopResult detector.LoopDetectionResult

	// Prompt content
	PromptPreview string
	RawMessages   []map[string]interface{} // nil-safe; used by inject_prompt action
}

// RuleResult is returned by Engine.Evaluate().
// The handler switches on Action to decide what to do.
type RuleResult struct {
	Action               ActionType
	BlockStatus          int    // HTTP status code when Action is block or rate_limit
	BlockMessage         string // response body sent to the caller
	OverrideModel        string // non-empty when Action is override_model
	InjectedSystemPrompt string // non-empty when Action is inject_prompt
	RedirectURL          string // non-empty when Action is redirect
	MatchedRule          *Rule  // which rule fired; nil if no match
	RateLimited          bool   // true when a rate-limit action triggered
}

// ── Engine ────────────────────────────────────────────────────────────────────

// Engine evaluates rules against a RuleContext.
// Safe for concurrent use across goroutines.
type Engine struct {
	mu      sync.RWMutex
	rules   []*Rule      // sorted priority DESC, id ASC; replaced atomically on reload
	limiter *RateLimiter
	store   *RuleStore
	stopCh  chan struct{}
}

// NewEngine creates an Engine, loads rules from the DB, and starts the
// background reload loop (polls every reloadInterval).
func NewEngine(rs *RuleStore) (*Engine, error) {
	e := &Engine{
		limiter: NewRateLimiter(),
		store:   rs,
		stopCh:  make(chan struct{}),
	}
	if err := e.reload(); err != nil {
		return nil, err
	}
	go e.reloadLoop(30 * time.Second)
	return e, nil
}

// Close stops the background reload goroutine.
func (e *Engine) Close() {
	close(e.stopCh)
}

// Reload triggers an immediate synchronous rule reload.
// Called by the REST API after any Create/Update/Delete/Toggle operation.
func (e *Engine) Reload() {
	if err := e.reload(); err != nil {
		log.Printf("[RULES] reload error: %v", err)
	}
}

// RuleCount returns the number of enabled rules currently loaded.
func (e *Engine) RuleCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.rules)
}

// Evaluate is the hot path. Called synchronously from handleRequest.
//
// Contract:
//   - Returns in < 1ms for any realistic rule set (< 1000 rules, < 10 conditions each).
//   - The caller must act on result.Action before sending the upstream request.
//   - If matched == false, the request is unconditionally allowed.
func (e *Engine) Evaluate(ctx *RuleContext) (RuleResult, bool) {
	e.mu.RLock()
	rules := e.rules // snapshot the slice pointer; elements are immutable after compile
	e.mu.RUnlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		// Scope filter: skip if this rule is agent-scoped and doesn't match
		if rule.ScopeAgentID != "" &&
			rule.ScopeAgentID != ctx.AgentID &&
			rule.ScopeAgentID != ctx.AppName {
			continue
		}
		// All conditions must match (AND semantics)
		if matchAll(rule.compiled, ctx) {
			result := rule.compiledAct.Apply(ctx, rule)
			// If a rate-limit action allowed the request, don't report a "match"
			// so the handler treats it as a normal allow.
			if result.Action == ActionAllow {
				return result, false
			}
			return result, true
		}
	}
	return RuleResult{Action: ActionAllow}, false
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// matchAll returns true iff every compiled condition matches ctx.
// Short-circuits on the first false.
func matchAll(conditions []Condition, ctx *RuleContext) bool {
	for _, c := range conditions {
		if !c.Evaluate(ctx) {
			return false
		}
	}
	return true
}

// reload fetches all rules from the store, compiles them, and atomically
// replaces the in-memory slice.
func (e *Engine) reload() error {
	rawRules, err := e.store.List()
	if err != nil {
		return err
	}

	compiled := make([]*Rule, 0, len(rawRules))
	for _, r := range rawRules {
		if err := e.compile(r); err != nil {
			log.Printf("[RULES] skipping rule %d (%q): compile error: %v", r.ID, r.Name, err)
			continue
		}
		compiled = append(compiled, r)
	}

	// Sort: priority DESC, then id ASC for stable ordering
	sort.Slice(compiled, func(i, j int) bool {
		if compiled[i].Priority != compiled[j].Priority {
			return compiled[i].Priority > compiled[j].Priority
		}
		return compiled[i].ID < compiled[j].ID
	})

	e.mu.Lock()
	e.rules = compiled
	e.mu.Unlock()

	log.Printf("[RULES] loaded %d rules (%d total, %d skipped due to errors)",
		len(compiled), len(rawRules), len(rawRules)-len(compiled))
	return nil
}

// compile populates rule.compiled and rule.compiledAct from the spec fields.
// The rule struct must not be used for evaluation until compile returns nil.
func (e *Engine) compile(rule *Rule) error {
	conditions := make([]Condition, 0, len(rule.Conditions))
	for _, spec := range rule.Conditions {
		c, err := compileCondition(spec, e.limiter, rule.ID)
		if err != nil {
			return err
		}
		conditions = append(conditions, c)
	}

	act, err := compileAction(rule.Action, e.limiter)
	if err != nil {
		return err
	}

	rule.compiled = conditions
	rule.compiledAct = act
	return nil
}

// reloadLoop polls the store on every tick until Close() is called.
func (e *Engine) reloadLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := e.reload(); err != nil {
				log.Printf("[RULES] background reload error: %v", err)
			}
		case <-e.stopCh:
			return
		}
	}
}
