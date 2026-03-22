package cache

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"tokomoco/embedding"
	"tokomoco/vectorstore"

	_ "modernc.org/sqlite"
)

func testSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "semantic-cache-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testSemanticCache(t *testing.T) *SemanticCache {
	t.Helper()
	emb := embedding.NewMockEmbedder(64)
	db := testSQLiteDB(t)
	vs, err := vectorstore.New(db, 64, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return NewSemanticCache(emb, vs, 0.95, true)
}

func TestSemanticCache_StoreAndLookup(t *testing.T) {
	sc := testSemanticCache(t)

	// Store a vector
	sc.Store("hello world query", "cache-hash-1", "openai", "gpt-4o")

	// Lookup with exact same key → should match (mock embedder is deterministic)
	hash, sim, found := sc.Lookup("hello world query", "openai", "gpt-4o")
	if !found {
		t.Fatal("expected to find cached entry")
	}
	if hash != "cache-hash-1" {
		t.Errorf("expected cache-hash-1, got %s", hash)
	}
	if sim < 0.99 {
		t.Errorf("exact same input should have sim ≈ 1.0, got %f", sim)
	}
}

func TestSemanticCache_MissOnDifferentInput(t *testing.T) {
	sc := testSemanticCache(t)

	sc.Store("what is the capital of france", "hash-1", "openai", "gpt-4o")

	// Different input → mock embedder produces different vector → should miss at 0.95 threshold
	_, _, found := sc.Lookup("how to cook pasta", "openai", "gpt-4o")
	if found {
		t.Error("unrelated queries should not match")
	}
}

func TestSemanticCache_Disabled(t *testing.T) {
	sc := testSemanticCache(t)
	sc.SetEnabled(false)

	sc.Store("test query", "hash-1", "openai", "gpt-4o")
	_, _, found := sc.Lookup("test query", "openai", "gpt-4o")
	if found {
		t.Error("disabled cache should not return results")
	}
}

func TestSemanticCache_ProviderScoping(t *testing.T) {
	sc := testSemanticCache(t)

	sc.Store("test query", "hash-1", "openai", "gpt-4o")

	// Different provider → miss
	_, _, found := sc.Lookup("test query", "anthropic", "claude-sonnet-4")
	if found {
		t.Error("different provider should not match")
	}
}

func TestSemanticCache_Stats(t *testing.T) {
	sc := testSemanticCache(t)

	sc.Store("query-a", "hash-a", "openai", "gpt-4o")
	sc.Lookup("query-a", "openai", "gpt-4o")          // hit
	sc.Lookup("query-miss", "openai", "gpt-4o")        // miss

	stats := sc.Stats()
	if !stats.Enabled {
		t.Error("should be enabled")
	}
	if stats.Vectors != 1 {
		t.Errorf("expected 1 vector, got %d", stats.Vectors)
	}
	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
	if stats.HitRate < 0.49 || stats.HitRate > 0.51 {
		t.Errorf("expected ~50%% hit rate, got %f", stats.HitRate)
	}
}

func TestSemanticCache_Flush(t *testing.T) {
	sc := testSemanticCache(t)

	sc.Store("q1", "h1", "openai", "gpt-4o")
	sc.Store("q2", "h2", "openai", "gpt-4o")
	sc.Flush()

	stats := sc.Stats()
	if stats.Vectors != 0 {
		t.Errorf("expected 0 vectors after flush, got %d", stats.Vectors)
	}
}

func TestSemanticCache_Threshold(t *testing.T) {
	sc := testSemanticCache(t)

	sc.SetThreshold(0.8)
	if sc.Threshold() != 0.8 {
		t.Errorf("expected threshold 0.8, got %f", sc.Threshold())
	}

	// Invalid threshold should not change
	sc.SetThreshold(0)
	if sc.Threshold() != 0.8 {
		t.Errorf("invalid threshold should not change value, got %f", sc.Threshold())
	}
	sc.SetThreshold(1.5)
	if sc.Threshold() != 0.8 {
		t.Errorf("invalid threshold should not change value, got %f", sc.Threshold())
	}
}

// ── BuildSemanticKey tests ───────────────────────────────────────────────

func TestBuildSemanticKey_OpenAI(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	key := BuildSemanticKey("openai", "gpt-4o", body)

	if !strings.Contains(key, "openai/gpt-4o") {
		t.Errorf("key should contain provider/model, got %q", key)
	}
	if !strings.Contains(key, "What is the capital of France?") {
		t.Errorf("key should contain message content, got %q", key)
	}
}

func TestBuildSemanticKey_Anthropic(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"Tell me about Go"}]}]}`)
	key := BuildSemanticKey("anthropic", "claude-sonnet-4", body)

	if !strings.Contains(key, "anthropic/claude-sonnet-4") {
		t.Errorf("key should contain provider/model, got %q", key)
	}
	if !strings.Contains(key, "Tell me about Go") {
		t.Errorf("key should contain Anthropic text block content, got %q", key)
	}
}

func TestBuildSemanticKey_InvalidJSON(t *testing.T) {
	key := BuildSemanticKey("openai", "gpt-4o", []byte("not json"))
	if key != "" {
		t.Errorf("invalid JSON should return empty key, got %q", key)
	}
}

func TestBuildSemanticKey_MultipleMessages(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"Hello"}]}`)
	key := BuildSemanticKey("openai", "gpt-4o", body)

	if !strings.Contains(key, "system: You are helpful") {
		t.Error("key should contain system message")
	}
	if !strings.Contains(key, "user: Hello") {
		t.Error("key should contain user message")
	}
}
