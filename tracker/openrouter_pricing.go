package tracker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"tokomoco/store"
)

// openRouterModelsURL is the public catalogue endpoint that returns per-model
// pricing (USD per token, as strings).
const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

type orPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type orModel struct {
	ID      string    `json:"id"`
	Pricing orPricing `json:"pricing"`
}

type orModelsResponse struct {
	Data []orModel `json:"data"`
}

// RefreshFromOpenRouter fetches the OpenRouter model catalogue (using
// OPENROUTER_API_KEY — the same key the proxy forwards with) and upserts each
// model's price into the pricing store under the `openrouter/<id>` prefix, which
// is how the proxy addresses OpenRouter models. Prices are converted from
// USD-per-token to USD-per-1M. Returns the number of rows inserted/updated.
func RefreshFromOpenRouter(db *store.DB, ps *PricingStore) (int, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return 0, fmt.Errorf("OPENROUTER_API_KEY is not set")
	}

	req, err := http.NewRequest("GET", openRouterModelsURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch openrouter models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("openrouter models returned %d", resp.StatusCode)
	}

	var parsed orModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse openrouter models: %w", err)
	}

	// Map existing prefixes -> id so we update in place instead of conflicting.
	existing, err := db.ListModelPricing()
	if err != nil {
		return 0, fmt.Errorf("list existing pricing: %w", err)
	}
	idByPrefix := make(map[string]int64, len(existing))
	for _, e := range existing {
		idByPrefix[e.ModelPrefix] = e.ID
	}

	updated := 0
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		inPer1M := perMillionUSD(m.Pricing.Prompt)
		outPer1M := perMillionUSD(m.Pricing.Completion)
		if inPer1M == 0 && outPer1M == 0 {
			continue // free or unpriced model — nothing to record
		}
		prefix := "openrouter/" + m.ID
		row := store.ModelPricingRow{
			ModelPrefix: prefix,
			InputPer1M:  inPer1M,
			OutputPer1M: outPer1M,
			Provider:    "openrouter",
			Source:      "openrouter",
			UpdatedAt:   time.Now(),
		}
		if id, ok := idByPrefix[prefix]; ok {
			row.ID = id
			if err := db.UpdateModelPricing(row); err == nil {
				updated++
			}
		} else {
			if _, err := db.InsertModelPricing(row); err == nil {
				updated++
			}
		}
	}

	ps.ReloadCache()
	return updated, nil
}

// perMillionUSD parses an OpenRouter per-token price string (USD) and converts
// it to USD per 1M tokens. Returns 0 on empty/invalid input.
func perMillionUSD(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * 1_000_000
}
