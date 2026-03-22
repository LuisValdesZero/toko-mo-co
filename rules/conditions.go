package rules

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Condition is the compiled, evaluatable form of a ConditionSpec.
// Implementations must be goroutine-safe and allocation-free in Evaluate.
type Condition interface {
	Evaluate(ctx *RuleContext) bool
	CondType() ConditionType
}

// compileCondition builds a Condition from a ConditionSpec.
// Regex patterns are compiled once here, not per-request.
// ruleID is forwarded to requestCountCondition so it shares the same rate-limit
// bucket as the corresponding rate-limit action (keyed by rule ID).
func compileCondition(spec ConditionSpec, limiter *RateLimiter, ruleID int64) (Condition, error) {
	switch spec.Type {
	case CondAgentID:
		return newStringCondition(spec, func(ctx *RuleContext) string { return ctx.AgentID })
	case CondAppName:
		return newStringCondition(spec, func(ctx *RuleContext) string { return ctx.AppName })
	case CondModel:
		return newStringCondition(spec, func(ctx *RuleContext) string { return ctx.Model })
	case CondProvider:
		return newStringCondition(spec, func(ctx *RuleContext) string { return ctx.Provider })
	case CondPromptContent:
		return newPromptCondition(spec)
	case CondInputTokens:
		return &numericCondition{
			op:        spec.Op,
			threshold: spec.Threshold,
			extract:   func(ctx *RuleContext) float64 { return float64(ctx.InputTokens) },
			condType:  CondInputTokens,
		}, nil
	case CondCostSession:
		return &numericCondition{
			op:        spec.Op,
			threshold: spec.Threshold,
			extract: func(ctx *RuleContext) float64 {
				if ctx.Session == nil {
					return 0
				}
				cost, _, _, _ := ctx.Session.GetStats()
				return cost
			},
			condType: CondCostSession,
		}, nil
	case CondCostDaily:
		if limiter == nil {
			return nil, fmt.Errorf("cost_daily condition requires a rate limiter")
		}
		return &costQuotaCondition{
			limiter:   limiter,
			threshold: spec.Threshold,
			periodSec: 86400, // 24 hours
			condType:  CondCostDaily,
		}, nil
	case CondCostMonthly:
		if limiter == nil {
			return nil, fmt.Errorf("cost_monthly condition requires a rate limiter")
		}
		return &costQuotaCondition{
			limiter:   limiter,
			threshold: spec.Threshold,
			periodSec: 2592000, // 30 days
			condType:  CondCostMonthly,
		}, nil
	case CondRequestCount:
		if limiter == nil {
			return nil, fmt.Errorf("request_count condition requires a rate limiter")
		}
		windowSec := spec.WindowSec
		if windowSec <= 0 {
			windowSec = 60
		}
		return &requestCountCondition{
			limiter:   limiter,
			maxReq:    int(spec.Threshold),
			windowSec: windowSec,
			ruleID:    ruleID,
			condType:  CondRequestCount,
		}, nil
	case CondLoopDetected:
		return &loopDetectedCondition{}, nil
	default:
		return nil, fmt.Errorf("unknown condition type: %q", spec.Type)
	}
}

// ── String condition (agent_id, app_name, model, provider) ───────────────────

type stringCondition struct {
	mode     MatchMode
	value    string         // for exact / glob
	re       *regexp.Regexp // for regex (nil otherwise)
	extract  func(*RuleContext) string
	condType ConditionType
}

func newStringCondition(spec ConditionSpec, extract func(*RuleContext) string) (*stringCondition, error) {
	sc := &stringCondition{
		mode:     spec.Mode,
		value:    spec.Value,
		extract:  extract,
		condType: spec.Type,
	}
	if spec.Mode == "" {
		sc.mode = MatchExact
	}
	if sc.mode == MatchRegex {
		re, err := regexp.Compile(spec.Value)
		if err != nil {
			return nil, fmt.Errorf("condition %s: invalid regex %q: %w", spec.Type, spec.Value, err)
		}
		sc.re = re
	}
	return sc, nil
}

func (c *stringCondition) Evaluate(ctx *RuleContext) bool {
	val := c.extract(ctx)
	switch c.mode {
	case MatchExact:
		return val == c.value
	case MatchGlob:
		matched, _ := path.Match(c.value, val)
		return matched
	case MatchRegex:
		return c.re.MatchString(val)
	default:
		return val == c.value
	}
}
func (c *stringCondition) CondType() ConditionType { return c.condType }

// ── Prompt content condition ──────────────────────────────────────────────────

type promptContentCondition struct {
	mode  MatchMode
	value string
	re    *regexp.Regexp
}

func newPromptCondition(spec ConditionSpec) (*promptContentCondition, error) {
	pc := &promptContentCondition{mode: spec.Mode, value: spec.Value}
	if pc.mode == "" {
		pc.mode = MatchExact
	}
	if pc.mode == MatchRegex {
		re, err := regexp.Compile(spec.Value)
		if err != nil {
			return nil, fmt.Errorf("prompt_content: invalid regex %q: %w", spec.Value, err)
		}
		pc.re = re
	}
	return pc, nil
}

func (c *promptContentCondition) Evaluate(ctx *RuleContext) bool {
	text := ctx.PromptPreview
	switch c.mode {
	case MatchExact:
		return text == c.value
	case MatchGlob:
		matched, _ := path.Match(c.value, text)
		return matched
	case MatchRegex:
		return c.re.MatchString(text)
	default:
		// Default: substring contains check (most useful for keyword filtering)
		return strings.Contains(strings.ToLower(text), strings.ToLower(c.value))
	}
}
func (c *promptContentCondition) CondType() ConditionType { return CondPromptContent }

// ── Numeric condition (input_tokens, cost_session) ────────────────────────────

type numericCondition struct {
	op        string
	threshold float64
	extract   func(*RuleContext) float64
	condType  ConditionType
}

func (c *numericCondition) Evaluate(ctx *RuleContext) bool {
	return evalOp(c.extract(ctx), c.op, c.threshold)
}
func (c *numericCondition) CondType() ConditionType { return c.condType }

// evalOp evaluates a numeric comparison. Defaults to "gte" if op is empty.
func evalOp(val float64, op string, threshold float64) bool {
	switch op {
	case "gt":
		return val > threshold
	case "gte", "":
		return val >= threshold
	case "lt":
		return val < threshold
	case "lte":
		return val <= threshold
	case "eq":
		return val == threshold
	default:
		return val >= threshold
	}
}

// ── Cost quota condition (cost_daily / cost_monthly) ──────────────────────────
// NOTE: This is a CHECK condition — it does NOT consume from the bucket.
// The rate-limit action's Apply() is responsible for consuming.
// Here we just check whether the current session cost has breached the threshold.

type costQuotaCondition struct {
	limiter   *RateLimiter
	threshold float64
	periodSec int
	condType  ConditionType
}

func (c *costQuotaCondition) Evaluate(ctx *RuleContext) bool {
	if ctx.Session == nil {
		return false
	}
	cost, _, _, _ := ctx.Session.GetStats()
	return cost >= c.threshold
}
func (c *costQuotaCondition) CondType() ConditionType { return c.condType }

// ── Request count condition ───────────────────────────────────────────────────
// Checks whether this request would exceed the request-rate limit.
// Peek-only (does not consume): PeekRequest reads the bucket state without
// taking a token. The rate-limit action's Apply() is the sole consumer.
// This ensures the condition and action share the same bucket (keyed by rule.ID)
// and don't interfere with each other.

type requestCountCondition struct {
	limiter   *RateLimiter
	maxReq    int
	windowSec int
	ruleID    int64 // set at compile time so condition and action share a bucket
	condType  ConditionType
}

func (c *requestCountCondition) Evaluate(ctx *RuleContext) bool {
	// Peek at the bucket keyed by the real rule ID. Returns true (condition
	// matches = "limit exceeded") when the next consume would fail.
	scope := "agent"
	return !c.limiter.PeekRequest(scope, ctx.AgentID, c.ruleID, c.maxReq, c.windowSec)
}
func (c *requestCountCondition) CondType() ConditionType { return c.condType }

// ── Loop detected condition ───────────────────────────────────────────────────

type loopDetectedCondition struct{}

func (c *loopDetectedCondition) Evaluate(ctx *RuleContext) bool {
	return ctx.LoopResult.LoopDetected
}
func (c *loopDetectedCondition) CondType() ConditionType { return CondLoopDetected }
