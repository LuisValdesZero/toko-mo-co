package reliability

import (
	"fmt"
	"log"

	"tokomoco/tracker"
)

// FallbackStrategy defines how fallback models are selected
type FallbackStrategy string

const (
	FallbackAny      FallbackStrategy = "any"       // Any available model
	FallbackSameTier FallbackStrategy = "same_tier"  // Similar cost tier
	FallbackCheaper  FallbackStrategy = "cheaper"    // Only cheaper models
)

// ModelTier represents the cost tier of a model
type ModelTier int

const (
	TierFree      ModelTier = iota
	TierCheap               // < $1/M input tokens
	TierMid                 // $1–5/M input tokens
	TierPremium             // $5–20/M input tokens
	TierExpensive           // > $20/M input tokens
)

// FallbackCandidate represents a potential fallback model
type FallbackCandidate struct {
	Provider string
	Model    string
	Tier     ModelTier
	Priority int // Lower is better
}

// GetModelTier determines the cost tier from actual pricing data in cost_calculator.go.
// Falls back to the mid tier for unknown models.
func GetModelTier(model string) ModelTier {
	pricing := tracker.GetModelPricing(model)
	inputCost := pricing.InputPer1M

	switch {
	case inputCost <= 0:
		return TierFree
	case inputCost < 1.0:
		return TierCheap
	case inputCost < 5.0:
		return TierMid
	case inputCost < 20.0:
		return TierPremium
	default:
		return TierExpensive
	}
}

// shouldIncludeCandidate determines if a candidate matches the strategy
func shouldIncludeCandidate(candidate FallbackCandidate, originalTier ModelTier, strategy FallbackStrategy) bool {
	switch strategy {
	case FallbackAny:
		return true
	case FallbackSameTier:
		return candidate.Tier == originalTier
	case FallbackCheaper:
		return candidate.Tier <= originalTier
	default:
		return true
	}
}

// getTierFallbacks returns generic fallbacks based on tier.
// These are used only when no user-configured fallback exists in the DB.
func getTierFallbacks(tier ModelTier, strategy FallbackStrategy) []FallbackCandidate {
	var candidates []FallbackCandidate

	switch tier {
	case TierCheap:
		candidates = []FallbackCandidate{
			{Provider: "openai", Model: "gpt-4o-mini", Tier: TierCheap, Priority: 10},
			{Provider: "anthropic", Model: "claude-haiku-4-5", Tier: TierCheap, Priority: 11},
			{Provider: "google", Model: "gemini-2.5-flash", Tier: TierCheap, Priority: 12},
		}
	case TierMid:
		candidates = []FallbackCandidate{
			{Provider: "openai", Model: "gpt-4o", Tier: TierMid, Priority: 10},
			{Provider: "anthropic", Model: "claude-sonnet-4", Tier: TierMid, Priority: 11},
			{Provider: "google", Model: "gemini-2.5-flash", Tier: TierMid, Priority: 12},
		}
	case TierPremium:
		candidates = []FallbackCandidate{
			{Provider: "openai", Model: "gpt-4o", Tier: TierPremium, Priority: 10},
			{Provider: "anthropic", Model: "claude-sonnet-4", Tier: TierPremium, Priority: 11},
			{Provider: "google", Model: "gemini-2.5-pro", Tier: TierPremium, Priority: 12},
		}
	case TierExpensive:
		candidates = []FallbackCandidate{
			{Provider: "anthropic", Model: "claude-opus-4-6", Tier: TierExpensive, Priority: 10},
			{Provider: "openai", Model: "o3", Tier: TierExpensive, Priority: 11},
		}
	default: // TierFree or unknown — fall back to cheap models
		candidates = []FallbackCandidate{
			{Provider: "openai", Model: "gpt-4o-mini", Tier: TierCheap, Priority: 10},
			{Provider: "anthropic", Model: "claude-haiku-4-5", Tier: TierCheap, Priority: 11},
			{Provider: "google", Model: "gemini-2.5-flash", Tier: TierCheap, Priority: 12},
		}
	}

	// Filter based on strategy
	filtered := make([]FallbackCandidate, 0, len(candidates))
	for _, c := range candidates {
		if shouldIncludeCandidate(c, tier, strategy) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// SelectFallback chooses the best fallback model with hierarchical config lookup:
//  1. Agent-specific config in the DB (if agentID provided)
//  2. Global default config in the DB (agent_id = '')
//  3. Tier-based automatic selection (hardcoded, no DB)
//
// Returns the fallback provider, model, and the matched FallbackConfig.ID
// (0 if tier-based/automatic selection was used).
func SelectFallback(agentID, originalProvider, originalModel string, strategy FallbackStrategy, store *FallbackStore) (string, string, int64, error) {
	// 1. Try database-configured fallback first (if store provided)
	if store != nil {
		if config, err := store.GetForAgent(agentID, originalProvider, originalModel); err == nil && config.Enabled {
			scope := "global"
			if config.AgentID != "" {
				scope = fmt.Sprintf("agent:%s", config.AgentID)
			}
			log.Printf("[FALLBACK] Using %s configured fallback chain for %s/%s", scope, originalProvider, originalModel)
			for _, opt := range config.FallbackChain {
				// Skip same provider to avoid same-provider retry
				if opt.Provider != originalProvider {
					log.Printf("[FALLBACK] Selected: %s/%s (%s, priority=%d) as fallback for %s/%s",
						opt.Provider, opt.Model, scope, opt.Priority, originalProvider, originalModel)
					return opt.Provider, opt.Model, config.ID, nil
				}
			}
		}
	}

	// 2. Fall back to tier-based selection
	log.Printf("[FALLBACK] No configured fallback found, using tier-based selection")
	originalTier := GetModelTier(originalModel)
	candidates := getTierFallbacks(originalTier, strategy)

	// Filter out the original provider to avoid same-provider retry
	filtered := make([]FallbackCandidate, 0)
	for _, c := range candidates {
		if c.Provider != originalProvider {
			filtered = append(filtered, c)
		}
	}

	if len(filtered) == 0 {
		return "", "", 0, fmt.Errorf("no fallback available for %s/%s with strategy %s",
			originalProvider, originalModel, strategy)
	}

	best := filtered[0]
	log.Printf("[FALLBACK] Selected: %s/%s (tier=%v, priority=%d) as fallback for %s/%s",
		best.Provider, best.Model, best.Tier, best.Priority, originalProvider, originalModel)

	return best.Provider, best.Model, 0, nil
}

// FormatFallbackInfo creates a human-readable fallback summary
func FormatFallbackInfo(originalProvider, originalModel, fallbackProvider, fallbackModel string) string {
	return fmt.Sprintf("fallback: %s/%s → %s/%s",
		originalProvider, originalModel, fallbackProvider, fallbackModel)
}
