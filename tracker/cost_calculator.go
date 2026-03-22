package tracker

import (
	"strings"

	"tokomoco/store"
)

// ModelPricing represents pricing for a model (all figures: USD per 1M tokens).
// CachedInputPer1M is the discounted rate for prompt-cache hits (0 = not supported).
// Last updated: 2026-02-14  —  update this comment whenever the table is refreshed.
//
// Gemini tiering note: Google uses a 200k-token context threshold (not 128k).
// Models with a "-long" suffix represent the >200k tier.
type ModelPricing struct {
	InputPer1M       float64 // Standard input tokens
	CachedInputPer1M float64 // Cache-read tokens (0 if unsupported)
	OutputPer1M      float64 // Output / completion tokens
}

// pricingStore is the DB-backed pricing store. When set, lookupPricing delegates
// to it. When nil (e.g., in tests), the hardcoded defaults are used directly.
var pricingStore *PricingStore

// InitPricingStore creates and sets the package-level pricing store.
// Call once at startup from main.go. Returns the store for API handler use.
func InitPricingStore(db *store.DB) *PricingStore {
	pricingStore = NewPricingStore(db)
	return pricingStore
}

// GetPricingStore returns the active pricing store (nil if not initialized).
func GetPricingStore() *PricingStore {
	return pricingStore
}

// defaultModelPricing contains the hardcoded seed data and last-resort fallback.
// These are inserted into the DB on first run and serve as a fallback if the
// DB cache has no match. Previously named "modelPricing".
//
// Pricing sources:
//   OpenAI   — https://platform.openai.com/docs/pricing
//   Anthropic — https://www.anthropic.com/pricing
//   Google   — https://ai.google.dev/pricing
var defaultModelPricing = map[string]ModelPricing{

	// ── OpenAI GPT-5 family ───────────────────────────────────────────────────
	"gpt-5.2-pro": { // GPT-5.2 Pro — smartest / most precise
		InputPer1M:       21.00,
		CachedInputPer1M: 0,
		OutputPer1M:      168.00,
	},
	"gpt-5.2": { // GPT-5.2 — best for coding & agentic tasks
		InputPer1M:       1.75,
		CachedInputPer1M: 0.175,
		OutputPer1M:      14.00,
	},
	"gpt-5-mini": { // GPT-5 mini — faster, cheaper for well-defined tasks
		InputPer1M:       0.25,
		CachedInputPer1M: 0.025,
		OutputPer1M:      2.00,
	},

	// ── OpenAI GPT-4o family ─────────────────────────────────────────────────
	"gpt-4o-mini": {
		InputPer1M:       0.15,
		CachedInputPer1M: 0.075,
		OutputPer1M:      0.60,
	},
	"gpt-4o": {
		InputPer1M:       5.00,
		CachedInputPer1M: 2.50,
		OutputPer1M:      15.00,
	},

	// ── OpenAI o-series (reasoning) ──────────────────────────────────────────
	"o4-mini": {
		InputPer1M:       1.10,
		CachedInputPer1M: 0.275,
		OutputPer1M:      4.40,
	},
	"o3-mini": {
		InputPer1M:       1.10,
		CachedInputPer1M: 0.55,
		OutputPer1M:      4.40,
	},
	"o3": {
		InputPer1M:       10.00,
		CachedInputPer1M: 2.50,
		OutputPer1M:      40.00,
	},
	"o1-mini": {
		InputPer1M:       1.10,
		CachedInputPer1M: 0.55,
		OutputPer1M:      4.40,
	},
	"o1": {
		InputPer1M:       15.00,
		CachedInputPer1M: 7.50,
		OutputPer1M:      60.00,
	},

	// ── OpenAI legacy ─────────────────────────────────────────────────────────
	"gpt-4-turbo": {
		InputPer1M:       10.00,
		CachedInputPer1M: 0,
		OutputPer1M:      30.00,
	},
	"gpt-4": {
		InputPer1M:       30.00,
		CachedInputPer1M: 0,
		OutputPer1M:      60.00,
	},
	"gpt-3.5-turbo": {
		InputPer1M:       0.50,
		CachedInputPer1M: 0,
		OutputPer1M:      1.50,
	},

	// ── Anthropic Claude Opus 4.x ────────────────────────────────────────────
	// claude-opus-4.6 / claude-opus-4.5 — $5 input, $25 output (same price tier)
	"claude-opus-4-6": {
		InputPer1M:       5.00,
		CachedInputPer1M: 0.50,
		OutputPer1M:      25.00,
	},
	"claude-opus-4-5": {
		InputPer1M:       5.00,
		CachedInputPer1M: 0.50,
		OutputPer1M:      25.00,
	},
	// claude-opus-4.1 / claude-opus-4 — $15 input, $75 output
	"claude-opus-4-1": {
		InputPer1M:       15.00,
		CachedInputPer1M: 1.50,
		OutputPer1M:      75.00,
	},
	"claude-opus-4": {
		InputPer1M:       15.00,
		CachedInputPer1M: 1.50,
		OutputPer1M:      75.00,
	},

	// ── Anthropic Claude Sonnet 4.x ──────────────────────────────────────────
	"claude-sonnet-4-5": {
		InputPer1M:       3.00,
		CachedInputPer1M: 0.30,
		OutputPer1M:      15.00,
	},
	"claude-sonnet-4": {
		InputPer1M:       3.00,
		CachedInputPer1M: 0.30,
		OutputPer1M:      15.00,
	},

	// ── Anthropic Claude Haiku 4.x ───────────────────────────────────────────
	// claude-haiku-4.5 — $1 input, $5 output
	// Note: there is no "claude-haiku-4" model; only haiku-4.5 and 3-haiku exist.
	"claude-haiku-4-5": {
		InputPer1M:       1.00,
		CachedInputPer1M: 0.10,
		OutputPer1M:      5.00,
	},

	// ── Anthropic Claude 3.x ─────────────────────────────────────────────────
	// claude-sonnet-3.7 (deprecated) — same price as claude-sonnet-4
	"claude-sonnet-3-7": {
		InputPer1M:       3.00,
		CachedInputPer1M: 0.30,
		OutputPer1M:      15.00,
	},
	"claude-3-5-sonnet": {
		InputPer1M:       3.00,
		CachedInputPer1M: 0.30,
		OutputPer1M:      15.00,
	},
	"claude-3-5-haiku": {
		InputPer1M:       0.80,
		CachedInputPer1M: 0.08,
		OutputPer1M:      4.00,
	},
	// claude-opus-3 (deprecated)
	"claude-3-opus": {
		InputPer1M:       15.00,
		CachedInputPer1M: 1.50,
		OutputPer1M:      75.00,
	},
	"claude-3-sonnet": {
		InputPer1M:       3.00,
		CachedInputPer1M: 0,
		OutputPer1M:      15.00,
	},
	"claude-3-haiku": {
		InputPer1M:       0.25,
		CachedInputPer1M: 0.03,
		OutputPer1M:      1.25,
	},

	// ── Google Gemini 3 ──────────────────────────────────────────────────────
	// Tiering: Google bills differently above 200k tokens (-long suffix).
	// Gemini 3 Pro (gemini-3-pro-preview) — paid tier standard pricing
	"gemini-3-pro-preview-long": { // > 200k tokens
		InputPer1M:       4.00,
		CachedInputPer1M: 0.40,
		OutputPer1M:      18.00,
	},
	"gemini-3-pro-preview": { // ≤ 200k tokens
		InputPer1M:       2.00,
		CachedInputPer1M: 0.20,
		OutputPer1M:      12.00,
	},
	// Gemini 3 Flash Preview (gemini-3-flash-preview) — text/image/video input
	"gemini-3-flash-preview": {
		InputPer1M:       0.50,
		CachedInputPer1M: 0.05,
		OutputPer1M:      3.00,
	},

	// ── Google Gemini 2.5 ────────────────────────────────────────────────────
	// Tiering threshold: 200k tokens (not 128k).
	"gemini-2.5-pro-long": { // > 200k tokens
		InputPer1M:       4.00,
		CachedInputPer1M: 0.40,
		OutputPer1M:      15.00,
	},
	"gemini-2.5-pro": { // ≤ 200k tokens
		InputPer1M:       1.25,
		CachedInputPer1M: 0,
		OutputPer1M:      10.00,
	},
	"gemini-2.5-flash-long": { // > 200k tokens
		InputPer1M:       0.30,
		CachedInputPer1M: 0,
		OutputPer1M:      3.50,
	},
	"gemini-2.5-flash": { // ≤ 200k tokens
		InputPer1M:       0.075,
		CachedInputPer1M: 0,
		OutputPer1M:      0.30,
	},

	// ── Google Gemini 2.0 ────────────────────────────────────────────────────
	"gemini-2.0-flash-exp": { // experimental / free tier
		InputPer1M:       0.00,
		CachedInputPer1M: 0,
		OutputPer1M:      0.00,
	},
	"gemini-2.0-flash": {
		InputPer1M:       0.10,
		CachedInputPer1M: 0,
		OutputPer1M:      0.40,
	},

	// ── Google Gemini 1.5 ────────────────────────────────────────────────────
	// Tiering threshold: 128k tokens for 1.5 family.
	"gemini-1.5-pro-long": { // > 128k tokens
		InputPer1M:       3.50,
		CachedInputPer1M: 0,
		OutputPer1M:      10.50,
	},
	"gemini-1.5-pro": { // ≤ 128k tokens
		InputPer1M:       1.25,
		CachedInputPer1M: 0,
		OutputPer1M:      5.00,
	},
	"gemini-1.5-flash-long": { // > 128k tokens
		InputPer1M:       0.15,
		CachedInputPer1M: 0,
		OutputPer1M:      0.60,
	},
	"gemini-1.5-flash": { // ≤ 128k tokens
		InputPer1M:       0.075,
		CachedInputPer1M: 0,
		OutputPer1M:      0.30,
	},
}

// CalculateCost calculates the cost for a request.
//
// cachedInputTokens — tokens served from the provider's prompt cache (0 if none).
// These are billed at CachedInputPer1M instead of InputPer1M.
// Regular inputTokens should NOT include the cached portion (pass only the
// non-cached count so they sum correctly).
func CalculateCost(model string, inputTokens, cachedInputTokens, outputTokens int) float64 {
	p := lookupPricing(model)
	inputCost  := (float64(inputTokens)       / 1_000_000) * p.InputPer1M
	cachedCost := (float64(cachedInputTokens) / 1_000_000) * p.CachedInputPer1M
	outputCost := (float64(outputTokens)      / 1_000_000) * p.OutputPer1M
	return inputCost + cachedCost + outputCost
}

// lookupPricing finds the best-matching pricing entry for a model name.
// If the DB-backed PricingStore is initialized, delegates to it (which checks
// DB cache → hardcoded defaults → unknown model fallback). Otherwise falls back
// to the hardcoded defaults directly (backward compat for tests).
func lookupPricing(model string) ModelPricing {
	if pricingStore != nil {
		return pricingStore.LookupPricing(model)
	}
	// Fallback: no DB store (test context or pre-init)
	if p, ok := defaultModelPricing[model]; ok {
		return p
	}
	best := ""
	for key := range defaultModelPricing {
		if strings.HasPrefix(model, key) && len(key) > len(best) {
			best = key
		}
	}
	if best != "" {
		return defaultModelPricing[best]
	}
	return ModelPricing{InputPer1M: 3.0, OutputPer1M: 15.0}
}

// GetModelPricing returns the pricing entry for a model (for display in the dashboard).
func GetModelPricing(model string) ModelPricing {
	return lookupPricing(model)
}

// IsKnownModel returns true if the model is recognized by the pricing system
// (exact or prefix match in DB cache or hardcoded defaults). Returns true for
// any model if the pricing store is not initialized (fail-open for tests).
func IsKnownModel(model string) bool {
	if pricingStore != nil {
		return pricingStore.IsKnownModel(model)
	}
	// No pricing store — check hardcoded defaults directly
	if _, ok := defaultModelPricing[model]; ok {
		return true
	}
	for key := range defaultModelPricing {
		if strings.HasPrefix(model, key) {
			return true
		}
	}
	return false
}
