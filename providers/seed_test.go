package providers

import (
	"os"
	"testing"

	"tokomoco/store"

	_ "modernc.org/sqlite"
)

func testProviderStore(t *testing.T) *ProviderStore {
	t.Helper()
	f, err := os.CreateTemp("", "prov-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	db, err := store.Open("", f.Name())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewProviderStore(db.DB())
}

func TestSeedOpenRouter(t *testing.T) {
	ps := testProviderStore(t)

	created, err := ps.SeedOpenRouter()
	if err != nil {
		t.Fatalf("SeedOpenRouter: %v", err)
	}
	if !created {
		t.Fatalf("expected openrouter provider created on first run")
	}

	// Idempotent — a second run creates nothing.
	if again, _ := ps.SeedOpenRouter(); again {
		t.Fatalf("re-seed should not create the provider again")
	}

	all, err := ps.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Exactly one provider, named "openrouter" (not renamed to or-* families).
	if len(all) != 1 || all[0].Name != "openrouter" {
		t.Fatalf("want a single provider named openrouter, got %+v", all)
	}
	p := all[0]
	if p.BaseURL != "https://openrouter.ai/api" || p.AuthEnvVar != "OPENROUTER_API_KEY" || p.APIFormat != "openai" {
		t.Fatalf("openrouter wrong base/auth/format: %+v", p)
	}

	// Model families are listed as models, ordered alphabetically by family.
	wantModels := []string{
		"anthropic/claude-3.5-haiku",
		"deepseek/deepseek-chat",
		"google/gemini-2.5-flash-lite",
		"meta-llama/llama-3.3-70b-instruct",
		"openai/gpt-4o-mini",
		"qwen/qwen-2.5-7b-instruct",
	}
	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatalf("Get(%d): %v", p.ID, err)
	}
	if len(got.Models) != len(wantModels) {
		t.Fatalf("models = %v, want %v", got.Models, wantModels)
	}
	for i, m := range wantModels {
		if got.Models[i] != m {
			t.Fatalf("models[%d] = %q, want %q (alphabetical by family)", i, got.Models[i], m)
		}
	}
}
