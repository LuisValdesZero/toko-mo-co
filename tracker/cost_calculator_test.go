package tracker

import (
	"math"
	"testing"
)

// tolerance for floating-point comparisons (1 nano-dollar)
const epsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// TestCalculateCost_KnownModels verifies exact math against published prices
// for a representative set of models.
func TestCalculateCost_KnownModels(t *testing.T) {
	tests := []struct {
		name             string
		model            string
		inputTokens      int
		cachedInput      int
		outputTokens     int
		wantCost         float64
		description      string
	}{
		// ── gpt-4o: $5.00/$2.50/$15.00 per 1M ───────────────────────────────
		{
			name:         "gpt-4o basic",
			model:        "gpt-4o",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// 1M * $5.00 + 1M * $15.00 = $20.00
			wantCost:    20.00,
			description: "gpt-4o: 1M input + 1M output",
		},
		{
			name:         "gpt-4o small request",
			model:        "gpt-4o",
			inputTokens:  500,
			cachedInput:  0,
			outputTokens: 200,
			// (500/1e6)*5.00 + (200/1e6)*15.00 = 0.0025 + 0.003 = 0.0055
			wantCost:    0.0055,
			description: "gpt-4o: 500 input + 200 output",
		},

		// ── gpt-4o-mini: $0.15/$0.075/$0.60 per 1M ──────────────────────────
		{
			name:         "gpt-4o-mini 1M tokens",
			model:        "gpt-4o-mini",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// 1M * $0.15 + 1M * $0.60 = $0.75
			wantCost:    0.75,
			description: "gpt-4o-mini: 1M input + 1M output",
		},

		// ── claude-3-5-sonnet: $3.00/$0.30/$15.00 per 1M ────────────────────
		{
			name:         "claude-3-5-sonnet 1M tokens",
			model:        "claude-3-5-sonnet",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// 1M * $3.00 + 1M * $15.00 = $18.00
			wantCost:    18.00,
			description: "claude-3-5-sonnet: 1M input + 1M output",
		},

		// ── claude-3-opus: $15.00/$1.50/$75.00 per 1M ───────────────────────
		{
			name:         "claude-3-opus 100k tokens",
			model:        "claude-3-opus",
			inputTokens:  100_000,
			cachedInput:  0,
			outputTokens: 50_000,
			// (100000/1e6)*15.00 + (50000/1e6)*75.00
			// = 1.50 + 3.75 = 5.25
			wantCost:    5.25,
			description: "claude-3-opus: 100k input + 50k output",
		},

		// ── gpt-3.5-turbo: $0.50/$0.00/$1.50 per 1M ─────────────────────────
		{
			name:         "gpt-3.5-turbo 1M tokens",
			model:        "gpt-3.5-turbo",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// 1M * $0.50 + 1M * $1.50 = $2.00
			wantCost:    2.00,
			description: "gpt-3.5-turbo: 1M input + 1M output",
		},

		// ── o1: $15.00/$7.50/$60.00 per 1M ──────────────────────────────────
		{
			name:         "o1 reasoning model",
			model:        "o1",
			inputTokens:  200_000,
			cachedInput:  0,
			outputTokens: 50_000,
			// (200000/1e6)*15.00 + (50000/1e6)*60.00
			// = 3.00 + 3.00 = 6.00
			wantCost:    6.00,
			description: "o1: 200k input + 50k output",
		},

		// ── gemini-2.0-flash: $0.10/$0.00/$0.40 per 1M ──────────────────────
		{
			name:         "gemini-2.0-flash 1M tokens",
			model:        "gemini-2.0-flash",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// 1M * $0.10 + 1M * $0.40 = $0.50
			wantCost:    0.50,
			description: "gemini-2.0-flash: 1M input + 1M output",
		},

		// ── zero-cost model ───────────────────────────────────────────────────
		{
			name:         "gemini-2.0-flash-exp free tier",
			model:        "gemini-2.0-flash-exp",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 1_000_000,
			// $0.00 + $0.00 = $0.00
			wantCost:    0.00,
			description: "gemini-2.0-flash-exp: free tier, always $0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, tt.cachedInput, tt.outputTokens)
			if !almostEqual(got, tt.wantCost) {
				t.Errorf("%s\n  model=%q input=%d cached=%d output=%d\n  got  $%.10f\n  want $%.10f",
					tt.description, tt.model, tt.inputTokens, tt.cachedInput, tt.outputTokens,
					got, tt.wantCost)
			}
		})
	}
}

// TestCalculateCost_PrefixMatching verifies that versioned model IDs like
// "gpt-4o-2024-11-20" fall back to the "gpt-4o" pricing entry via longest-
// prefix matching.
func TestCalculateCost_PrefixMatching(t *testing.T) {
	tests := []struct {
		name         string
		versionedID  string
		canonicalID  string
		inputTokens  int
		outputTokens int
	}{
		{
			name:         "gpt-4o versioned date suffix",
			versionedID:  "gpt-4o-2024-11-20",
			canonicalID:  "gpt-4o",
			inputTokens:  300_000,
			outputTokens: 100_000,
		},
		{
			name:         "gpt-4o-mini versioned date suffix",
			versionedID:  "gpt-4o-mini-2024-07-18",
			canonicalID:  "gpt-4o-mini",
			inputTokens:  500_000,
			outputTokens: 200_000,
		},
		{
			name:         "claude-sonnet-4 versioned suffix",
			versionedID:  "claude-sonnet-4-5-20250929",
			canonicalID:  "claude-sonnet-4",
			inputTokens:  100_000,
			outputTokens: 50_000,
		},
		{
			name:         "claude-3-5-sonnet versioned date suffix",
			versionedID:  "claude-3-5-sonnet-20241022",
			canonicalID:  "claude-3-5-sonnet",
			inputTokens:  250_000,
			outputTokens: 75_000,
		},
		{
			name:         "o1 versioned date suffix",
			versionedID:  "o1-2024-12-17",
			canonicalID:  "o1",
			inputTokens:  100_000,
			outputTokens: 25_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			costVersioned := CalculateCost(tt.versionedID, tt.inputTokens, 0, tt.outputTokens)
			costCanonical := CalculateCost(tt.canonicalID, tt.inputTokens, 0, tt.outputTokens)
			if !almostEqual(costVersioned, costCanonical) {
				t.Errorf("prefix match mismatch for %q vs %q\n  versioned $%.10f\n  canonical $%.10f",
					tt.versionedID, tt.canonicalID, costVersioned, costCanonical)
			}
		})
	}
}

// TestCalculateCost_CachedInputTokens verifies that cached tokens are billed
// at CachedInputPer1M (the discounted rate) rather than InputPer1M.
func TestCalculateCost_CachedInputTokens(t *testing.T) {
	tests := []struct {
		name             string
		model            string
		inputTokens      int
		cachedInput      int
		outputTokens     int
		wantCost         float64
		description      string
	}{
		// gpt-4o: input=$5.00, cached=$2.50, output=$15.00 per 1M
		{
			name:         "gpt-4o all cached input",
			model:        "gpt-4o",
			inputTokens:  0,
			cachedInput:  1_000_000,
			outputTokens: 0,
			// 0 + (1M/1e6)*2.50 + 0 = $2.50
			wantCost:    2.50,
			description: "gpt-4o: 1M cached input only",
		},
		{
			name:         "gpt-4o mixed regular and cached",
			model:        "gpt-4o",
			inputTokens:  400_000,
			cachedInput:  600_000,
			outputTokens: 200_000,
			// (400000/1e6)*5.00 + (600000/1e6)*2.50 + (200000/1e6)*15.00
			// = 2.00 + 1.50 + 3.00 = 6.50
			wantCost:    6.50,
			description: "gpt-4o: 400k regular + 600k cached + 200k output",
		},
		{
			name:         "gpt-4o-mini cached input discount",
			model:        "gpt-4o-mini",
			inputTokens:  0,
			cachedInput:  1_000_000,
			outputTokens: 0,
			// (1M/1e6)*0.075 = $0.075
			wantCost:    0.075,
			description: "gpt-4o-mini: 1M cached input only",
		},
		{
			name:         "gpt-4o-mini cached vs standard price difference",
			model:        "gpt-4o-mini",
			inputTokens:  1_000_000,
			cachedInput:  0,
			outputTokens: 0,
			// standard: (1M/1e6)*0.15 = $0.15
			wantCost:    0.15,
			description: "gpt-4o-mini: 1M standard input (for comparison)",
		},
		// claude-3-5-sonnet: input=$3.00, cached=$0.30, output=$15.00 per 1M
		{
			name:         "claude-3-5-sonnet cached input",
			model:        "claude-3-5-sonnet",
			inputTokens:  0,
			cachedInput:  500_000,
			outputTokens: 100_000,
			// (500000/1e6)*0.30 + (100000/1e6)*15.00
			// = 0.15 + 1.50 = 1.65
			wantCost:    1.65,
			description: "claude-3-5-sonnet: 500k cached + 100k output",
		},
		// claude-3-sonnet: CachedInputPer1M=0 (not supported) → cached tokens cost $0
		{
			name:         "claude-3-sonnet no cache support",
			model:        "claude-3-sonnet",
			inputTokens:  0,
			cachedInput:  1_000_000,
			outputTokens: 0,
			// CachedInputPer1M=0 → $0.00
			wantCost:    0.00,
			description: "claude-3-sonnet: cached input costs $0 (cache not supported)",
		},
		// o3-mini: input=$1.10, cached=$0.55, output=$4.40 per 1M
		{
			name:         "o3-mini cached is 50% of standard",
			model:        "o3-mini",
			inputTokens:  1_000_000,
			cachedInput:  1_000_000,
			outputTokens: 0,
			// (1M/1e6)*1.10 + (1M/1e6)*0.55
			// = 1.10 + 0.55 = 1.65
			wantCost:    1.65,
			description: "o3-mini: 1M regular + 1M cached input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, tt.cachedInput, tt.outputTokens)
			if !almostEqual(got, tt.wantCost) {
				t.Errorf("%s\n  model=%q input=%d cached=%d output=%d\n  got  $%.10f\n  want $%.10f",
					tt.description, tt.model, tt.inputTokens, tt.cachedInput, tt.outputTokens,
					got, tt.wantCost)
			}
		})
	}
}

// TestCalculateCost_UnknownModelFallback verifies that completely unrecognised
// model names fall back to $3.00 input / $15.00 output per 1M tokens.
func TestCalculateCost_UnknownModelFallback(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		inputTokens  int
		outputTokens int
		wantCost     float64
	}{
		{
			name:         "totally unknown model 1M+1M",
			model:        "unknown-model-xyz",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			// (1M/1e6)*3.00 + (1M/1e6)*15.00 = $18.00
			wantCost: 18.00,
		},
		{
			name:         "totally unknown model small request",
			model:        "my-custom-llm-v99",
			inputTokens:  10_000,
			outputTokens: 5_000,
			// (10000/1e6)*3.00 + (5000/1e6)*15.00
			// = 0.03 + 0.075 = 0.105
			wantCost: 0.105,
		},
		{
			name:         "empty string model",
			model:        "",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			// falls back to $3/$15 → $18.00
			wantCost: 18.00,
		},
		{
			name:         "partial prefix that does not match any key",
			model:        "gpt-",
			inputTokens:  1_000_000,
			outputTokens: 0,
			// "gpt-" is not a prefix of any key — falls back to $3/$15
			// (1M/1e6)*3.00 = $3.00
			wantCost: 3.00,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, 0, tt.outputTokens)
			if !almostEqual(got, tt.wantCost) {
				t.Errorf("unknown model fallback: model=%q input=%d output=%d\n  got  $%.10f\n  want $%.10f",
					tt.model, tt.inputTokens, tt.outputTokens, got, tt.wantCost)
			}
		})
	}
}

// TestCalculateCost_ZeroTokens verifies that zero-token requests always cost $0.
func TestCalculateCost_ZeroTokens(t *testing.T) {
	models := []string{"gpt-4o", "claude-3-5-sonnet", "o1", "unknown-model"}
	for _, model := range models {
		t.Run("zero tokens for "+model, func(t *testing.T) {
			got := CalculateCost(model, 0, 0, 0)
			if got != 0.0 {
				t.Errorf("model=%q: expected $0 for zero tokens, got $%.10f", model, got)
			}
		})
	}
}

// TestCalculateCost_CostMath verifies the math formula directly:
//   cost = (input/1e6)*InputPer1M + (cached/1e6)*CachedPer1M + (output/1e6)*OutputPer1M
func TestCalculateCost_CostMath(t *testing.T) {
	// Use gpt-4o ($5.00/$2.50/$15.00) as the reference model for arithmetic checks.
	const model = "gpt-4o"

	type tokSet struct{ input, cached, output int }
	sets := []tokSet{
		{1, 0, 0},
		{0, 1, 0},
		{0, 0, 1},
		{1_234_567, 0, 0},
		{0, 0, 987_654},
		{100_000, 50_000, 25_000},
	}

	p := GetModelPricing(model)

	for _, s := range sets {
		expected := (float64(s.input)/1e6)*p.InputPer1M +
			(float64(s.cached)/1e6)*p.CachedInputPer1M +
			(float64(s.output)/1e6)*p.OutputPer1M

		got := CalculateCost(model, s.input, s.cached, s.output)
		if !almostEqual(got, expected) {
			t.Errorf("math mismatch input=%d cached=%d output=%d: got $%.10f want $%.10f",
				s.input, s.cached, s.output, got, expected)
		}
	}
}
