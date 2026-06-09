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

func TestSeedProviderFamilies(t *testing.T) {
	ps := testProviderStore(t)

	n, err := ps.SeedProviderFamilies()
	if err != nil {
		t.Fatalf("SeedProviderFamilies: %v", err)
	}
	if n != 6 {
		t.Fatalf("expected 6 providers seeded, got %d", n)
	}

	// Idempotent — a second run creates none.
	if n2, _ := ps.SeedProviderFamilies(); n2 != 0 {
		t.Fatalf("re-seed should create 0, got %d", n2)
	}

	all, err := ps.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	byName := map[string]*CustomProvider{}
	for _, p := range all {
		byName[p.Name] = p
	}
	want := map[string]string{
		"or-openai":    "openai/gpt-4o-mini",
		"or-anthropic": "anthropic/claude-3.5-haiku",
		"or-google":    "google/gemini-2.5-flash-lite",
		"llama":        "meta-llama/llama-3.3-70b-instruct",
		"qwen":         "qwen/qwen-2.5-7b-instruct",
		"deepseek":     "deepseek/deepseek-chat",
	}
	for name, model := range want {
		p := byName[name]
		if p == nil {
			t.Fatalf("missing seeded provider %q", name)
		}
		if p.DefaultModel != model {
			t.Fatalf("%s default_model = %q, want %q", name, p.DefaultModel, model)
		}
		if p.BaseURL != "https://openrouter.ai/api" || p.AuthEnvVar != "OPENROUTER_API_KEY" || p.APIFormat != "openai" {
			t.Fatalf("%s wrong base/auth/format: %+v", name, p)
		}
		// default_model must survive a Get round-trip too.
		got, err := ps.Get(p.ID)
		if err != nil || got.DefaultModel != model {
			t.Fatalf("Get(%d) default_model round-trip: %v / %q", p.ID, err, got.DefaultModel)
		}
	}
}
