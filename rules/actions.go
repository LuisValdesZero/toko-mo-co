package rules

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Action is the compiled, executable form of an ActionSpec.
// Apply() is called only when all conditions match.
// Implementations must be goroutine-safe.
type Action interface {
	Apply(ctx *RuleContext, rule *Rule) RuleResult
	ActType() ActionType
}

// compileAction builds an Action from an ActionSpec.
func compileAction(spec ActionSpec, limiter *RateLimiter) (Action, error) {
	switch spec.Type {
	case ActionAllow:
		return &allowAction{}, nil

	case ActionBlock:
		status := spec.BlockStatus
		if status == 0 {
			status = http.StatusForbidden // 403
		}
		msg := spec.BlockMessage
		if msg == "" {
			msg = "Request blocked by proxy policy."
		}
		return &blockAction{status: status, message: msg}, nil

	case ActionRateLimit:
		if limiter == nil {
			return nil, fmt.Errorf("rate_limit action requires a rate limiter")
		}
		status := spec.BlockStatus
		if status == 0 {
			status = http.StatusTooManyRequests // 429
		}
		msg := spec.BlockMessage
		if msg == "" {
			msg = "Rate limit exceeded."
		}
		return &rateLimitAction{
			limiter:    limiter,
			status:     status,
			message:    msg,
			maxReq:     spec.RateLimitRequests,
			windowSec:  spec.RateLimitWindowSec,
			maxCostUSD: spec.RateLimitCostUSD,
			maxTokDay:  spec.RateLimitTokensDay,
			scope:      spec.RateLimitScope,
		}, nil

	case ActionOverrideModel:
		if spec.OverrideModel == "" {
			return nil, fmt.Errorf("override_model action requires override_model to be set")
		}
		return &overrideModelAction{model: spec.OverrideModel}, nil

	case ActionInjectPrompt:
		if spec.InjectedSystemPrompt == "" {
			return nil, fmt.Errorf("inject_prompt action requires injected_system_prompt to be set")
		}
		return &injectPromptAction{prompt: spec.InjectedSystemPrompt}, nil

	case ActionRedirect:
		// Provider failover chain takes precedence over a single redirect URL.
		if len(spec.RedirectProviders) > 0 {
			return &redirectAction{providers: spec.RedirectProviders}, nil
		}
		if spec.RedirectURL == "" {
			return nil, fmt.Errorf("redirect action requires redirect_url or redirect_providers to be set")
		}
		if err := validateUpstreamURL(spec.RedirectURL); err != nil {
			return nil, fmt.Errorf("redirect_url: %w", err)
		}
		return &redirectAction{url: spec.RedirectURL}, nil

	default:
		return nil, fmt.Errorf("unknown action type: %q", spec.Type)
	}
}

// ── allowAction ───────────────────────────────────────────────────────────────

type allowAction struct{}

func (a *allowAction) Apply(_ *RuleContext, rule *Rule) RuleResult {
	return RuleResult{Action: ActionAllow, MatchedRule: rule}
}
func (a *allowAction) ActType() ActionType { return ActionAllow }

// ── blockAction ───────────────────────────────────────────────────────────────

type blockAction struct {
	status  int
	message string
}

func (a *blockAction) Apply(_ *RuleContext, rule *Rule) RuleResult {
	return RuleResult{
		Action:      ActionBlock,
		BlockStatus: a.status,
		BlockMessage: a.message,
		MatchedRule: rule,
	}
}
func (a *blockAction) ActType() ActionType { return ActionBlock }

// ── rateLimitAction ───────────────────────────────────────────────────────────

type rateLimitAction struct {
	limiter    *RateLimiter
	status     int
	message    string
	maxReq     int
	windowSec  int
	maxCostUSD float64
	maxTokDay  int
	scope      string
}

func (a *rateLimitAction) Apply(ctx *RuleContext, rule *Rule) RuleResult {
	agentID := ctx.AgentID
	scope := a.scope
	if scope == "" {
		scope = "agent"
	}

	// Check request-rate limit
	if a.maxReq > 0 && a.windowSec > 0 {
		if !a.limiter.CheckAndConsumeRequest(scope, agentID, rule.ID, a.maxReq, a.windowSec) {
			return RuleResult{
				Action:      ActionRateLimit,
				BlockStatus: a.status,
				BlockMessage: a.message,
				MatchedRule: rule,
				RateLimited: true,
			}
		}
	}

	// Check cost quota (daily, using 86400 seconds)
	if a.maxCostUSD > 0 {
		if !a.limiter.CheckAndConsumeCost(scope, agentID, rule.ID, ctx.Cost, a.maxCostUSD, 86400) {
			return RuleResult{
				Action:      ActionRateLimit,
				BlockStatus: a.status,
				BlockMessage: "Daily cost budget exceeded.",
				MatchedRule: rule,
				RateLimited: true,
			}
		}
	}

	// Check token quota (daily)
	if a.maxTokDay > 0 {
		if !a.limiter.CheckAndConsumeTokens(scope, agentID, rule.ID, ctx.InputTokens, a.maxTokDay, 86400) {
			return RuleResult{
				Action:      ActionRateLimit,
				BlockStatus: a.status,
				BlockMessage: "Daily token budget exceeded.",
				MatchedRule: rule,
				RateLimited: true,
			}
		}
	}

	// All checks passed — allow
	return RuleResult{Action: ActionAllow, MatchedRule: rule}
}
func (a *rateLimitAction) ActType() ActionType { return ActionRateLimit }

// ── overrideModelAction ───────────────────────────────────────────────────────

type overrideModelAction struct {
	model string
}

func (a *overrideModelAction) Apply(_ *RuleContext, rule *Rule) RuleResult {
	return RuleResult{
		Action:        ActionOverrideModel,
		OverrideModel: a.model,
		MatchedRule:   rule,
	}
}
func (a *overrideModelAction) ActType() ActionType { return ActionOverrideModel }

// ── injectPromptAction ────────────────────────────────────────────────────────

type injectPromptAction struct {
	prompt string
}

func (a *injectPromptAction) Apply(_ *RuleContext, rule *Rule) RuleResult {
	return RuleResult{
		Action:               ActionInjectPrompt,
		InjectedSystemPrompt: a.prompt,
		MatchedRule:          rule,
	}
}
func (a *injectPromptAction) ActType() ActionType { return ActionInjectPrompt }

// ── redirectAction ────────────────────────────────────────────────────────────

type redirectAction struct {
	url       string
	providers []string // ordered failover chain of custom-provider names
}

func (a *redirectAction) Apply(_ *RuleContext, rule *Rule) RuleResult {
	return RuleResult{
		Action:            ActionRedirect,
		RedirectURL:       a.url,
		RedirectProviders: a.providers,
		MatchedRule:       rule,
	}
}
func (a *redirectAction) ActType() ActionType { return ActionRedirect }

// ── SSRF protection ──────────────────────────────────────────────────────────

// validateUpstreamURL checks that a URL is safe to use as an upstream target.
// Rejects non-HTTP(S) schemes, localhost, private IPs, and cloud metadata endpoints.
func validateUpstreamURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Only allow http/https schemes
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (only http/https)", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty hostname")
	}

	// Block localhost variants
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "127.0.0.1" || lower == "::1" || lower == "0.0.0.0" {
		return fmt.Errorf("localhost URLs are not allowed")
	}

	// Block cloud metadata endpoints (AWS, GCP, Azure)
	if lower == "169.254.169.254" || lower == "metadata.google.internal" {
		return fmt.Errorf("cloud metadata endpoints are not allowed")
	}

	// Block private/reserved IP ranges
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("private/reserved IP addresses are not allowed")
		}
	}

	return nil
}
