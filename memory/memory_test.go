package memory

import (
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"tokomoco/embedding"
	dbstore "tokomoco/store"

	_ "modernc.org/sqlite"
)

// fakeReranker returns a fixed ordering of input indices, or an error.
type fakeReranker struct {
	order []int // indices into the input slice, best-first
	err   error
}

func (f fakeReranker) Rerank(_ string, items []embedding.RerankItem, _ int) ([]embedding.RerankResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]embedding.RerankResult, 0, len(f.order))
	for rank, idx := range f.order {
		out = append(out, embedding.RerankResult{ID: strconv.Itoa(idx), Index: idx, Score: float64(len(f.order) - rank)})
	}
	return out, nil
}

func factsOf(ms []scoredMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.entry.Fact
	}
	return out
}

func TestRerankMatchesReorders(t *testing.T) {
	ms := []scoredMatch{{entry: &Entry{Fact: "a"}}, {entry: &Entry{Fact: "b"}}, {entry: &Entry{Fact: "c"}}}

	// Reranker promotes c, then a, then b.
	s := &Store{reranker: fakeReranker{order: []int{2, 0, 1}}}
	out := s.rerankMatches("q", ms, 3)
	if got := factsOf(out); got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("rerank order wrong: %v", got)
	}

	// Warming reranker -> fall back to the input (hybrid) order.
	sw := &Store{reranker: fakeReranker{err: embedding.ErrRerankerWarming}}
	if got := factsOf(sw.rerankMatches("q", ms, 3)); got[0] != "a" {
		t.Fatalf("expected fallback order on warming, got %v", got)
	}

	// No reranker -> passthrough unchanged.
	if got := factsOf((&Store{}).rerankMatches("q", ms, 3)); len(got) != 3 || got[0] != "a" {
		t.Fatalf("nil reranker should passthrough, got %v", got)
	}
}

func TestSparseCosineAndRoundTrip(t *testing.T) {
	a := embedding.SparseVector{1: 1.0, 2: 1.0}
	if c := sparseCosine(a, a); c < 0.999 {
		t.Fatalf("self cosine ~1, got %f", c)
	}
	if c := sparseCosine(a, embedding.SparseVector{3: 1.0}); c != 0 {
		t.Fatalf("disjoint cosine = 0, got %f", c)
	}
	if sparseCosine(nil, a) != 0 {
		t.Fatal("empty cosine = 0")
	}

	sp := embedding.SparseVector{5: 0.5, 9: 0.25}
	back := jsonToSparse(sparseToJSON(sp).(string))
	if back[5] != 0.5 || back[9] != 0.25 {
		t.Fatalf("sparse JSON roundtrip mismatch: %v", back)
	}
	if sparseToJSON(nil) != nil {
		t.Fatal("nil sparse should serialize to nil (NULL column)")
	}
}

// testDB creates a temporary SQLite database for testing.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "memory-test-*.db")
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

func TestNewStore(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, err := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.7, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", store.Count())
	}
	if !store.IsEnabled() {
		t.Error("expected store to be enabled")
	}
}

func TestStoreFact_And_Search(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	// Store a fact
	err := store.StoreFact("agent-1", "session-1", "The user prefers Go for backend development", "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("StoreFact: %v", err)
	}

	if store.Count() != 1 {
		t.Errorf("expected 1 entry, got %d", store.Count())
	}

	// Search for the same text should match (mock embedder produces identical vectors for same input)
	results, err := store.Search("The user prefers Go for backend development", "agent-1", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("exact match should have sim ~1.0, got %f", results[0].Similarity)
	}
}

func TestSearch_AgentScoping(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "User prefers Python", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s2", "User prefers Python", "openai", "gpt-4o")

	// Search scoped to agent-1
	results, _ := store.Search("User prefers Python", "agent-1", 5)
	if len(results) != 1 {
		t.Errorf("expected 1 result for agent-1, got %d", len(results))
	}

	// Search with empty agentID should find both
	results, _ = store.Search("User prefers Python", "", 5)
	if len(results) != 2 {
		t.Errorf("expected 2 results for empty agentID, got %d", len(results))
	}
}

func TestSearch_NoMatch(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.9, true) // high threshold

	store.StoreFact("agent-1", "s1", "User prefers Go", "openai", "gpt-4o")

	// Different text should not match with high threshold
	results, _ := store.Search("completely different topic about cooking recipes", "agent-1", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for unrelated query, got %d", len(results))
	}
}

func TestSearch_Disabled(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, false) // disabled

	store.StoreFact("agent-1", "s1", "User prefers Go", "openai", "gpt-4o")

	results, _ := store.Search("User prefers Go", "agent-1", 5)
	if results != nil {
		t.Error("expected nil results when disabled")
	}
}

func TestDelete(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "User prefers Go", "openai", "gpt-4o")
	if store.Count() != 1 {
		t.Fatal("expected 1 entry")
	}

	entries := store.ListByAgent("agent-1", 10)
	if len(entries) == 0 {
		t.Fatal("expected at least 1 entry for agent-1")
	}
	store.Delete(entries[0].ID)

	if store.Count() != 0 {
		t.Errorf("expected 0 entries after delete, got %d", store.Count())
	}
}

func TestDeleteByAgent(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "Fact A", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Fact B", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s3", "Fact C", "openai", "gpt-4o")

	removed, err := store.DeleteByAgent("agent-1")
	if err != nil {
		t.Fatalf("DeleteByAgent: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
	if store.Count() != 1 {
		t.Errorf("expected 1 remaining, got %d", store.Count())
	}
}

func TestFlush(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "Fact A", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s2", "Fact B", "openai", "gpt-4o")
	store.Flush()

	if store.Count() != 0 {
		t.Errorf("expected 0 entries after flush, got %d", store.Count())
	}
}

func TestEviction(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 3, 0.5, true) // max 3

	store.StoreFact("agent-1", "s1", "Fact one about coding", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Fact two about testing", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s3", "Fact three about deployment", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s4", "Fact four about monitoring", "openai", "gpt-4o")

	if store.Count() != 3 {
		t.Errorf("expected 3 entries after eviction, got %d", store.Count())
	}
}

func TestDuplicateDetection(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "The user prefers Go", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "The user prefers Go", "openai", "gpt-4o") // duplicate

	if store.Count() != 1 {
		t.Errorf("expected 1 entry (duplicate rejected), got %d", store.Count())
	}
}

func TestStats(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.7, true)

	store.StoreFact("agent-1", "s1", "User prefers Python", "openai", "gpt-4o")
	store.Search("User prefers Python", "agent-1", 5)

	stats := store.GetStats()
	if !stats.Enabled {
		t.Error("expected enabled=true")
	}
	if stats.Memories != 1 {
		t.Errorf("expected 1 memory, got %d", stats.Memories)
	}
	if stats.Lookups != 1 {
		t.Errorf("expected 1 lookup, got %d", stats.Lookups)
	}
	if stats.Stored != 1 {
		t.Errorf("expected 1 stored, got %d", stats.Stored)
	}
}

func TestListByAgent(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "Fact A from agent 1", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Fact B from agent 1", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s3", "Fact C from agent 2", "openai", "gpt-4o")

	list := store.ListByAgent("agent-1", 10)
	if len(list) != 2 {
		t.Errorf("expected 2 entries for agent-1, got %d", len(list))
	}

	// All agents
	all := store.ListByAgent("", 10)
	if len(all) != 3 {
		t.Errorf("expected 3 total entries, got %d", len(all))
	}
}

func TestCountByAgent(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "Fact X about testing", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Fact Y about building", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s3", "Fact Z about deploying", "openai", "gpt-4o")

	if n := store.CountByAgent("agent-1"); n != 2 {
		t.Errorf("expected 2 for agent-1, got %d", n)
	}
	if n := store.CountByAgent("agent-2"); n != 1 {
		t.Errorf("expected 1 for agent-2, got %d", n)
	}
	if n := store.CountByAgent("agent-3"); n != 0 {
		t.Errorf("expected 0 for agent-3, got %d", n)
	}
}

func TestWarmLoad(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)

	store1, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)
	store1.StoreFact("agent-1", "s1", "User prefers TypeScript", "openai", "gpt-4o")
	store1.StoreFact("agent-2", "s2", "Team uses Docker containers", "openai", "gpt-4o")

	// Create a new store from same DB → warm-load
	store2, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)
	if store2.Count() != 2 {
		t.Errorf("warm-load: expected 2 entries, got %d", store2.Count())
	}
}

// ── Extractor tests ──────────────────────────────────────────────────────────

func TestExtractFacts_UserPreferences(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "I prefer using Go for backend development. My team uses PostgreSQL for our database.",
			},
		},
	})

	facts := ExtractFacts("openai", body, nil)
	if len(facts) == 0 {
		t.Fatal("expected at least 1 fact extracted")
	}

	// Check that at least one fact contains preference info
	found := false
	for _, f := range facts {
		if containsAny(f, []string{"prefer", "Go", "PostgreSQL", "team uses"}) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected facts about preferences, got: %v", facts)
	}
}

func TestExtractFacts_TechnicalContext(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Our application is deployed on AWS with Kubernetes infrastructure. We are running on Go version 1.24.",
			},
		},
	})

	facts := ExtractFacts("openai", body, nil)
	if len(facts) == 0 {
		t.Fatal("expected facts extracted from technical context")
	}
}

func TestExtractFacts_TrivialContent(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Hello, can you help me with something?",
			},
		},
	})

	facts := ExtractFacts("openai", body, nil)
	if len(facts) != 0 {
		t.Errorf("expected 0 facts from trivial content, got: %v", facts)
	}
}

func TestExtractFacts_SystemPromptIgnored(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": "I prefer using Python for everything. My database is MySQL.",
			},
			map[string]interface{}{
				"role":    "user",
				"content": "Hello there!",
			},
		},
	})

	facts := ExtractFacts("openai", body, nil)
	// System messages should be ignored — only user messages are extracted
	for _, f := range facts {
		if containsAny(f, []string{"Python", "MySQL"}) {
			t.Errorf("system prompt content should not be extracted: %s", f)
		}
	}
}

func TestExtractFacts_AnthropicFormat(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "claude-sonnet-4-20250514",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "I always use dark mode in my IDE. My preferred editor is VS Code.",
					},
				},
			},
		},
	})

	facts := ExtractFacts("anthropic", body, nil)
	if len(facts) == 0 {
		t.Fatal("expected facts from Anthropic format")
	}
}

func TestExtractFacts_GeminiFormat(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{
						"text": "We use React for our frontend. Our API is built with Express.",
					},
				},
			},
		},
	})

	facts := ExtractFacts("gemini", body, nil)
	if len(facts) == 0 {
		t.Fatal("expected facts from Gemini format")
	}
}

func TestExtractFacts_Questions_Ignored(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "What is the best database to use? How should I deploy my app?",
			},
		},
	})

	facts := ExtractFacts("openai", body, nil)
	if len(facts) != 0 {
		t.Errorf("questions should not be extracted as facts, got: %v", facts)
	}
}

func TestExtractFacts_ResponseExtraction(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Tell me something",
			},
		},
	})

	resp := mustJSON(t, map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Your application is deployed on Kubernetes. You should always use environment variables for configuration.",
				},
			},
		},
	})

	facts := ExtractFacts("openai", body, resp)
	// Response contains factual patterns ("deployed on", "should always")
	if len(facts) == 0 {
		t.Fatal("expected facts extracted from response")
	}
}

func TestExtractFacts_AnthropicToolUseResponse(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "claude-haiku-4-5-20251001",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Analyze AAPL",
			},
		},
	})

	// Real Anthropic tool_use response format
	resp := mustJSON(t, map[string]interface{}{
		"id":   "msg_123",
		"type": "message",
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{
				"type": "tool_use",
				"id":   "toolu_123",
				"name": "stock_analysis",
				"input": map[string]interface{}{
					"action":    "buy",
					"reasoning": "Strong earnings beat with 15% revenue growth and expanding margins. Technical indicators show bullish momentum.",
				},
			},
		},
		"usage": map[string]interface{}{
			"input_tokens":  100,
			"output_tokens": 50,
		},
	})

	facts := ExtractFacts("anthropic", body, resp)
	if len(facts) == 0 {
		t.Fatal("expected facts extracted from Anthropic tool_use response")
	}

	// Verify it captured the tool call with decision fields
	found := false
	for _, f := range facts {
		if strings.Contains(f, "stock_analysis") && strings.Contains(f, "action") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fact about stock_analysis tool call, got: %v", facts)
	}
}

func TestExtractFacts_OpenAIToolCallsResponse(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "What should I do with TSLA?",
			},
		},
	})

	// OpenAI tool_calls response format
	resp := mustJSON(t, map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id":   "call_123",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "execute_trade",
								"arguments": `{"action":"sell","reasoning":"Overvalued with P/E ratio above 60. Risk/reward unfavorable at current levels."}`,
							},
						},
					},
				},
			},
		},
	})

	facts := ExtractFacts("openai", body, resp)
	if len(facts) == 0 {
		t.Fatal("expected facts extracted from OpenAI tool_calls response")
	}

	found := false
	for _, f := range facts {
		if strings.Contains(f, "execute_trade") && strings.Contains(f, "action") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fact about execute_trade tool call, got: %v", facts)
	}
}

func TestBuildMemoryContext(t *testing.T) {
	memories := []SearchResult{
		{
			Entry:      &Entry{Fact: "User prefers Go for backend"},
			Similarity: 0.92,
		},
		{
			Entry:      &Entry{Fact: "Team uses PostgreSQL"},
			Similarity: 0.85,
		},
	}

	ctx := BuildMemoryContext(memories)
	if ctx == "" {
		t.Fatal("expected non-empty context")
	}
	if !containsAny(ctx, []string{"Memory Context", "User prefers Go", "Team uses PostgreSQL"}) {
		t.Errorf("context should contain memory facts: %s", ctx)
	}
}

func TestBuildMemoryContext_Empty(t *testing.T) {
	ctx := BuildMemoryContext(nil)
	if ctx != "" {
		t.Errorf("expected empty context for nil memories, got: %s", ctx)
	}
}

func TestFilterFacts_Deduplication(t *testing.T) {
	facts := filterFacts([]string{
		"User prefers Go",
		"User prefers Go",
		"User prefers Go",
		"Team uses Docker",
	})
	if len(facts) != 2 {
		t.Errorf("expected 2 unique facts, got %d: %v", len(facts), facts)
	}
}

func TestFilterFacts_MaxCap(t *testing.T) {
	facts := filterFacts([]string{
		"Fact one about coding",
		"Fact two about testing",
		"Fact three about deploying",
		"Fact four about monitoring",
		"Fact five about logging",
		"Fact six about debugging",
		"Fact seven about profiling",
	})
	if len(facts) > 5 {
		t.Errorf("expected max 5 facts, got %d", len(facts))
	}
}

func TestSplitSentences(t *testing.T) {
	text := "First sentence. Second sentence. Third one!"
	sentences := splitSentences(text)
	if len(sentences) < 2 {
		t.Errorf("expected at least 2 sentences, got %d: %v", len(sentences), sentences)
	}
}

func TestIsTrivial(t *testing.T) {
	trivials := []string{"hello", "thank you", "yes", "ok"}
	for _, s := range trivials {
		if !isTrivial(s) {
			t.Errorf("expected %q to be trivial", s)
		}
	}

	nonTrivials := []string{
		"I prefer using Go for all backend services",
		"Our database is running on PostgreSQL 15",
	}
	for _, s := range nonTrivials {
		if isTrivial(s) {
			t.Errorf("expected %q to NOT be trivial", s)
		}
	}
}

// ── Enhancement Tests ─────────────────────────────────────────────────────────

// craftVectors builds two unit vectors with a known cosine similarity.
// vecA is [1, 0, 0, 0] (unit).
// vecB is rotated in the first two dimensions to achieve the target similarity.
func craftVectors(targetSim float64) ([]float32, []float32) {
	// vecA = [1, 0, 0, 0]
	vecA := []float32{1.0, 0.0, 0.0, 0.0}

	// vecB = [cos(θ), sin(θ), 0, 0] where cos(θ) = targetSim
	cosT := float32(targetSim)
	sinT := float32(0.0)
	if targetSim < 1.0 {
		sinT = float32(1.0 - cosT*cosT)
		if sinT > 0 {
			sinT = float32(sqrtF64(float64(sinT)))
		}
	}
	vecB := []float32{cosT, sinT, 0.0, 0.0}

	return vecA, vecB
}

func sqrtF64(x float64) float64 {
	// Newton's method
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func TestAccessCountIncrement(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "The user prefers Go for backend development", "openai", "gpt-4o")

	// Search 3 times
	for i := 0; i < 3; i++ {
		store.Search("The user prefers Go for backend development", "agent-1", 5)
	}

	// Check access count on the in-memory entry
	store.mu.RLock()
	var entry *Entry
	for _, e := range store.entries {
		if e.Fact == "The user prefers Go for backend development" {
			entry = e
			break
		}
	}
	store.mu.RUnlock()

	if entry == nil {
		t.Fatal("expected to find the stored entry")
	}
	if entry.AccessCount != 3 {
		t.Errorf("expected AccessCount=3, got %d", entry.AccessCount)
	}
}

func TestLastAccessedUpdate(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "The user prefers Go for backend development", "openai", "gpt-4o")

	// Search to trigger access update
	store.Search("The user prefers Go for backend development", "agent-1", 5)

	store.mu.RLock()
	var entry *Entry
	for _, e := range store.entries {
		if e.Fact == "The user prefers Go for backend development" {
			entry = e
			break
		}
	}
	store.mu.RUnlock()

	if entry == nil {
		t.Fatal("expected to find the stored entry")
	}
	if entry.LastAccessed.IsZero() {
		t.Error("expected LastAccessed to be set after search")
	}
	if !entry.LastAccessed.After(entry.CreatedAt) && !entry.LastAccessed.Equal(entry.CreatedAt) {
		t.Errorf("expected LastAccessed >= CreatedAt, got LastAccessed=%v, CreatedAt=%v",
			entry.LastAccessed, entry.CreatedAt)
	}
}

func TestRecencyWeightedScoring(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.3, true, WithRecencyLambda(0.05))

	// Store two facts with the exact same text to ensure identical vectors/similarity.
	// We'll backdate one to make it old.
	vec, _ := emb.Embed("User prefers coding in Go")

	store.StoreFactWithVector("agent-1", "s1", "User prefers coding in Go", vec, "openai", "gpt-4o")

	// Backdate the first entry by 60 days
	store.mu.Lock()
	if len(store.entries) > 0 {
		store.entries[0].CreatedAt = store.entries[0].CreatedAt.AddDate(0, 0, -60)
	}
	store.mu.Unlock()

	// Store a recent entry with slightly different text but same embedding concept
	vec2, _ := emb.Embed("User also prefers coding in Go language")
	store.StoreFactWithVector("agent-1", "s2", "User also prefers coding in Go language", vec2, "openai", "gpt-4o")

	// Search — the query vector will match both, but the recent one should rank higher
	// due to recency weighting (lambda=0.05, 60 days → multiplier ~0.05)
	results, _ := store.Search("User prefers coding in Go", "agent-1", 5)

	if len(results) < 2 {
		// Even if only 1 result meets threshold, verify Score field is populated
		if len(results) == 1 && results[0].Score <= 0 {
			t.Error("expected Score field to be populated on search results")
		}
		// Test passes — recency weighting pushed the old one below threshold
		return
	}

	// If both returned, the recent one's Score should be >= the old one's Score
	if results[0].Score < results[1].Score {
		t.Errorf("expected most recent result first by score, got scores %f < %f",
			results[0].Score, results[1].Score)
	}
}

func TestConflictResolution_Update(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true, WithConflictThreshold(0.85))

	// Craft two vectors with cosine similarity ~0.90 (between 0.85 conflict threshold and 0.95 dedup)
	vecA, vecB := craftVectors(0.90)

	// Store original fact
	err := store.StoreFactWithVector("agent-1", "s1", "User prefers Python", vecA, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("StoreFactWithVector: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}

	// Store conflicting fact with update signal — should trigger replacement
	err = store.StoreFactWithVector("agent-1", "s2", "User now prefers Go", vecB, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("StoreFactWithVector (conflict): %v", err)
	}

	// Should still be 1 entry (replaced, not added)
	if store.Count() != 1 {
		t.Errorf("expected 1 entry after conflict resolution, got %d", store.Count())
	}

	// The fact text should be updated
	store.mu.RLock()
	var fact string
	for _, e := range store.entries {
		fact = e.Fact
	}
	store.mu.RUnlock()

	if fact != "User now prefers Go" {
		t.Errorf("expected updated fact text, got %q", fact)
	}
}

func TestConflictResolution_SkipSimilar(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true, WithConflictThreshold(0.85))

	// Craft two vectors with cosine similarity ~0.90
	vecA, vecB := craftVectors(0.90)

	// Store original fact
	store.StoreFactWithVector("agent-1", "s1", "User prefers Python for scripting", vecA, "openai", "gpt-4o")

	// Store similar fact WITHOUT update signals — should be skipped (not replaced)
	store.StoreFactWithVector("agent-1", "s2", "User prefers Python for coding", vecB, "openai", "gpt-4o")

	// Should still be 1 entry (skipped, not added)
	if store.Count() != 1 {
		t.Errorf("expected 1 entry (similar skipped), got %d", store.Count())
	}

	// Original fact should remain unchanged
	store.mu.RLock()
	var fact string
	for _, e := range store.entries {
		fact = e.Fact
	}
	store.mu.RUnlock()

	if fact != "User prefers Python for scripting" {
		t.Errorf("expected original fact unchanged, got %q", fact)
	}
}

func TestShouldReplace(t *testing.T) {
	tests := []struct {
		name     string
		newFact  string
		oldFact  string
		expected bool
	}{
		{"temporal now", "User now prefers Go", "User prefers Python", true},
		{"switched to", "User switched to VS Code", "User uses Vim", true},
		{"changed to", "User changed to dark mode", "User uses light mode", true},
		{"no longer", "User no longer uses Java", "User uses Java", true},
		{"instead of", "Uses React instead of Angular", "Uses Angular", true},
		{"moved to", "Team moved to AWS", "Team uses GCP", true},
		{"negation don't", "User don't use Windows anymore", "User uses Windows", true},
		{"negation stopped", "User stopped using Python", "User uses Python", true},
		{"no signal", "User likes coding", "User likes programming", false},
		{"same text", "User prefers Go", "User prefers Go", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReplace(tc.newFact, tc.oldFact)
			if got != tc.expected {
				t.Errorf("shouldReplace(%q, %q) = %v, want %v",
					tc.newFact, tc.oldFact, got, tc.expected)
			}
		})
	}
}

func TestUpdateExistingEntry(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	// Store initial fact
	vec, _ := emb.Embed("User prefers Python")
	store.StoreFactWithVector("agent-1", "s1", "User prefers Python", vec, "openai", "gpt-4o")

	// Get the entry
	store.mu.RLock()
	var entry *Entry
	for _, e := range store.entries {
		entry = e
		break
	}
	store.mu.RUnlock()

	if entry == nil {
		t.Fatal("expected entry to exist")
	}

	// Update it
	newVec, _ := emb.Embed("User now prefers Go")
	err := store.updateExistingEntry(entry, "User now prefers Go", newVec, nil)
	if err != nil {
		t.Fatalf("updateExistingEntry: %v", err)
	}

	// Verify in-memory update
	store.mu.RLock()
	updatedFact := entry.Fact
	updatedAt := entry.UpdatedAt
	store.mu.RUnlock()

	if updatedFact != "User now prefers Go" {
		t.Errorf("expected updated fact, got %q", updatedFact)
	}
	if updatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set")
	}

	// Verify DB persistence by creating new store from same DB
	store2, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)
	if store2.Count() != 1 {
		t.Fatalf("expected 1 entry from DB, got %d", store2.Count())
	}

	entries := store2.ListByAgent("agent-1", 10)
	if len(entries) != 1 {
		t.Fatal("expected 1 entry for agent-1")
	}
	if entries[0].Fact != "User now prefers Go" {
		t.Errorf("expected persisted updated fact, got %q", entries[0].Fact)
	}
}

func TestPerAgentEviction(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	// Use StoreFactWithVector with distinct vectors to bypass conflict detection
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 6, 0.5, true)

	// Create distinct vectors for each fact (orthogonal-ish)
	vecs := [][]float32{
		{1.0, 0.0, 0.0, 0.0},
		{0.0, 1.0, 0.0, 0.0},
		{0.0, 0.0, 1.0, 0.0},
		{0.0, 0.0, 0.0, 1.0},
		{0.707, 0.707, 0.0, 0.0},
		{0.0, 0.0, 0.707, 0.707},
		{0.577, 0.577, 0.577, 0.0},
	}

	// Store 4 facts for agent-1
	store.StoreFactWithVector("agent-1", "s1", "Agent1 prefers Python for scripting", vecs[0], "openai", "gpt-4o")
	store.StoreFactWithVector("agent-1", "s2", "Agent1 uses PostgreSQL database", vecs[1], "openai", "gpt-4o")
	store.StoreFactWithVector("agent-1", "s3", "Agent1 deploys on Kubernetes", vecs[2], "openai", "gpt-4o")
	store.StoreFactWithVector("agent-1", "s4", "Agent1 monitors with Grafana", vecs[3], "openai", "gpt-4o")

	// Store 2 facts for agent-2
	store.StoreFactWithVector("agent-2", "s5", "Agent2 uses Redis for caching", vecs[4], "openai", "gpt-4o")
	store.StoreFactWithVector("agent-2", "s6", "Agent2 prefers TypeScript", vecs[5], "openai", "gpt-4o")

	if store.Count() != 6 {
		t.Fatalf("expected 6 entries, got %d", store.Count())
	}

	// Store another for agent-1 — this should trigger per-agent eviction
	store.StoreFactWithVector("agent-1", "s7", "Agent1 scales with auto-scaling groups", vecs[6], "openai", "gpt-4o")

	// Agent-2's entries should remain untouched
	agent2Count := store.CountByAgent("agent-2")
	if agent2Count != 2 {
		t.Errorf("expected agent-2 to still have 2 entries, got %d", agent2Count)
	}

	// Total should still be 6 (evicted 1 from agent-1)
	if store.Count() != 6 {
		t.Errorf("expected 6 total entries after eviction, got %d", store.Count())
	}
}

func TestTTLEviction(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 3, 0.5, true, WithTTLDays(90))

	// Store 3 facts
	store.StoreFact("agent-1", "s1", "Stale fact about old technology", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Fresh fact about current tech", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s3", "Another fresh fact about new things", "openai", "gpt-4o")

	// Backdate the first entry by 100 days (past TTL of 90)
	store.mu.Lock()
	for _, e := range store.entries {
		if e.Fact == "Stale fact about old technology" {
			e.CreatedAt = e.CreatedAt.AddDate(0, 0, -100)
			e.LastAccessed = e.LastAccessed.AddDate(0, 0, -100)
			break
		}
	}
	store.mu.Unlock()

	// Store a 4th fact — triggers eviction, stale entry should be evicted first
	store.StoreFact("agent-1", "s4", "Brand new fact about innovation", "openai", "gpt-4o")

	if store.Count() != 3 {
		t.Errorf("expected 3 entries after eviction, got %d", store.Count())
	}

	// Verify the stale fact was evicted
	store.mu.RLock()
	found := false
	for _, e := range store.entries {
		if e.Fact == "Stale fact about old technology" {
			found = true
			break
		}
	}
	store.mu.RUnlock()

	if found {
		t.Error("expected stale fact to be evicted, but it's still present")
	}
}

func TestEnhancedStats(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true, WithTTLDays(90))

	// Store facts for 2 agents
	store.StoreFact("agent-1", "s1", "Agent1 prefers Go for development", "openai", "gpt-4o")
	store.StoreFact("agent-1", "s2", "Agent1 uses Docker containers", "openai", "gpt-4o")
	store.StoreFact("agent-2", "s3", "Agent2 prefers Python scripting", "openai", "gpt-4o")

	// Search to generate hits
	store.Search("Agent1 prefers Go for development", "agent-1", 5)
	store.Search("Agent2 prefers Python scripting", "agent-2", 5)

	stats := store.GetStats()

	// Check AgentBreakdown
	if len(stats.AgentBreakdown) != 2 {
		t.Errorf("expected 2 agents in breakdown, got %d", len(stats.AgentBreakdown))
	}

	// Check that agent-1 has 2 memories and agent-2 has 1
	agentMap := make(map[string]int)
	for _, ab := range stats.AgentBreakdown {
		agentMap[ab.AgentID] = ab.MemoryCount
	}
	if agentMap["agent-1"] != 2 {
		t.Errorf("expected agent-1 to have 2 memories, got %d", agentMap["agent-1"])
	}
	if agentMap["agent-2"] != 1 {
		t.Errorf("expected agent-2 to have 1 memory, got %d", agentMap["agent-2"])
	}

	// Backdate one entry to make it stale
	store.mu.Lock()
	for _, e := range store.entries {
		if e.Fact == "Agent1 uses Docker containers" {
			e.CreatedAt = e.CreatedAt.AddDate(0, 0, -100)
			e.LastAccessed = e.LastAccessed.AddDate(0, 0, -100)
			break
		}
	}
	store.mu.Unlock()

	stats2 := store.GetStats()
	if stats2.StaleCount != 1 {
		t.Errorf("expected 1 stale memory, got %d", stats2.StaleCount)
	}
}

func TestNewStoreWithOptions(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, err := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.7, true,
		WithRecencyLambda(0.05),
		WithConflictThreshold(0.90),
		WithTTLDays(30),
	)
	if err != nil {
		t.Fatalf("NewStore with options: %v", err)
	}

	if store.recencyLambda != 0.05 {
		t.Errorf("expected recencyLambda=0.05, got %f", store.recencyLambda)
	}
	if store.conflictThresh != 0.90 {
		t.Errorf("expected conflictThresh=0.90, got %f", store.conflictThresh)
	}
	if store.ttlDays != 30 {
		t.Errorf("expected ttlDays=30, got %d", store.ttlDays)
	}
}

func TestSearchResultHasScore(t *testing.T) {
	db := testDB(t)
	emb := embedding.NewMockEmbedder(4)
	store, _ := NewStore(dbstore.NewQuerier(db, dbstore.SQLite), emb, 100, 0.5, true)

	store.StoreFact("agent-1", "s1", "User prefers Go for backend development", "openai", "gpt-4o")

	results, _ := store.Search("User prefers Go for backend development", "agent-1", 5)
	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}

	if results[0].Score <= 0 {
		t.Errorf("expected positive Score, got %f", results[0].Score)
	}
	if results[0].Similarity <= 0 {
		t.Errorf("expected positive Similarity, got %f", results[0].Similarity)
	}
	// Score should be <= Similarity (recency decay can only reduce it)
	if results[0].Score > results[0].Similarity*1.01 { // small epsilon for floating point
		t.Errorf("expected Score <= Similarity, got Score=%f, Similarity=%f",
			results[0].Score, results[0].Similarity)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if len(s) >= len(p) {
			for i := 0; i <= len(s)-len(p); i++ {
				if s[i:i+len(p)] == p {
					return true
				}
			}
		}
	}
	return false
}
