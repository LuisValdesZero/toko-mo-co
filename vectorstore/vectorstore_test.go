package vectorstore

import (
	"database/sql"
	"math"
	"os"
	"testing"
	"time"

	"tokomoco/store"

	_ "modernc.org/sqlite"
)

// testDB creates a temporary SQLite database for testing.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "vectorstore-test-*.db")
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

func TestNew(t *testing.T) {
	db := testDB(t)
	vs, err := New(store.NewQuerier(db, store.SQLite), 4, 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if vs.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", vs.Count())
	}
}

func TestStore_And_Search(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vec := []float32{0.5, 0.5, 0.5, 0.5}
	vs.Store("hash-1", vec, "openai", "gpt-4o")

	if vs.Count() != 1 {
		t.Errorf("expected 1 entry, got %d", vs.Count())
	}

	result := vs.Search(vec, "openai", "gpt-4o", 0.9)
	if result == nil {
		t.Fatal("expected to find result, got nil")
	}
	if result.CacheHash != "hash-1" {
		t.Errorf("expected hash-1, got %s", result.CacheHash)
	}
	if result.Similarity < 0.99 {
		t.Errorf("exact match should have similarity ~1.0, got %f", result.Similarity)
	}
}

func TestSearchHybrid_SparseRerank(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	// Two entries with identical dense vectors but different sparse vectors.
	dense := []float32{0.5, 0.5, 0.5, 0.5}
	vs.StoreHybrid("h-a", dense, map[int32]float32{1: 1.0, 2: 0.5}, "p", "m")
	vs.StoreHybrid("h-b", dense, map[int32]float32{9: 1.0}, "p", "m")

	// Dense matches both equally; the sparse query matches only h-a, so with a
	// non-zero sparse weight h-a must win.
	q := map[int32]float32{1: 1.0, 2: 0.5}
	res := vs.SearchHybrid(dense, q, "p", "m", 0.6, 0.5)
	if res == nil {
		t.Fatal("expected a hybrid match")
	}
	if res.CacheHash != "h-a" {
		t.Errorf("hybrid should pick the sparse-matching entry h-a, got %s", res.CacheHash)
	}

	// sparseWeight 0 degrades to a pure dense search (identical vectors still match).
	if vs.SearchHybrid(dense, q, "p", "m", 0.9, 0) == nil {
		t.Error("dense-only search should still match identical vectors")
	}
}

func TestSearch_BelowThreshold(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vs.Store("hash-1", []float32{1, 0, 0, 0}, "openai", "gpt-4o")

	result := vs.Search([]float32{0, 1, 0, 0}, "openai", "gpt-4o", 0.5)
	if result != nil {
		t.Errorf("orthogonal vectors should not match, got sim=%f", result.Similarity)
	}
}

func TestSearch_ProviderScoping(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vec := []float32{0.5, 0.5, 0.5, 0.5}
	vs.Store("hash-openai", vec, "openai", "gpt-4o")

	// Different provider → no match
	result := vs.Search(vec, "anthropic", "claude-sonnet-4", 0.9)
	if result != nil {
		t.Error("should not match across providers")
	}

	// Same provider, different model → no match
	result = vs.Search(vec, "openai", "gpt-4o-mini", 0.9)
	if result != nil {
		t.Error("should not match across models")
	}

	// Same provider + model → match
	result = vs.Search(vec, "openai", "gpt-4o", 0.9)
	if result == nil {
		t.Error("should match same provider + model")
	}
}

func TestSearch_BestMatch(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vs.Store("hash-far", []float32{1, 0, 0, 0}, "openai", "gpt-4o")
	vs.Store("hash-close", []float32{0.5, 0.5, 0.5, 0.5}, "openai", "gpt-4o")

	query := []float32{0.4, 0.5, 0.5, 0.5}
	result := vs.Search(query, "openai", "gpt-4o", 0.5)
	if result == nil {
		t.Fatal("expected match")
	}
	if result.CacheHash != "hash-close" {
		t.Errorf("expected hash-close (closer), got %s", result.CacheHash)
	}
}

func TestStore_Eviction(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 3)

	vs.Store("hash-1", []float32{1, 0, 0, 0}, "openai", "gpt-4o")
	vs.Store("hash-2", []float32{0, 1, 0, 0}, "openai", "gpt-4o")
	vs.Store("hash-3", []float32{0, 0, 1, 0}, "openai", "gpt-4o")
	vs.Store("hash-4", []float32{0, 0, 0, 1}, "openai", "gpt-4o")

	if vs.Count() != 3 {
		t.Errorf("expected 3 entries after eviction, got %d", vs.Count())
	}
}

func TestDelete(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vs.Store("hash-1", []float32{0.5, 0.5, 0.5, 0.5}, "openai", "gpt-4o")
	vs.Delete("hash-1")

	if vs.Count() != 0 {
		t.Errorf("expected 0 entries after delete, got %d", vs.Count())
	}
}

func TestFlush(t *testing.T) {
	db := testDB(t)
	vs, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	vs.Store("h1", []float32{1, 0, 0, 0}, "openai", "gpt-4o")
	vs.Store("h2", []float32{0, 1, 0, 0}, "openai", "gpt-4o")
	vs.Flush()

	if vs.Count() != 0 {
		t.Errorf("expected 0 entries after flush, got %d", vs.Count())
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"similar", []float32{1, 1, 0}, []float32{1, 1, 1}, 0.8165},
		{"empty", []float32{}, []float32{}, 0.0},
		{"diff_len", []float32{1, 0}, []float32{1, 0, 0}, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("cosineSimilarity = %f, want %f", got, tt.want)
			}
		})
	}
}

func TestFloat32Serialization(t *testing.T) {
	original := []float32{0.1, -0.5, 1.0, 3.14, 0, -1.0}
	bytes := float32ToBytes(original)
	restored := bytesToFloat32(bytes)

	if len(restored) != len(original) {
		t.Fatalf("length mismatch: got %d, want %d", len(restored), len(original))
	}

	for i := range original {
		if original[i] != restored[i] {
			t.Errorf("index %d: got %f, want %f", i, restored[i], original[i])
		}
	}
}

func TestWarmLoad(t *testing.T) {
	db := testDB(t)

	vs1, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)

	// Persist synchronously to avoid race with test cleanup
	vs1.persistToDB(&Entry{
		CacheHash: "hash-1",
		Vector:    []float32{0.5, 0.5, 0.5, 0.5},
		Provider:  "openai",
		Model:     "gpt-4o",
		CreatedAt: time.Now(),
	})
	vs1.persistToDB(&Entry{
		CacheHash: "hash-2",
		Vector:    []float32{0.1, 0.2, 0.3, 0.4},
		Provider:  "anthropic",
		Model:     "claude-sonnet-4",
		CreatedAt: time.Now(),
	})

	// Create a new VectorStore from same DB → warm-load
	vs2, _ := New(store.NewQuerier(db, store.SQLite), 4, 100)
	if vs2.Count() != 2 {
		t.Errorf("warm-load: expected 2 entries, got %d", vs2.Count())
	}

	// Verify search works on warm-loaded data
	result := vs2.Search([]float32{0.5, 0.5, 0.5, 0.5}, "openai", "gpt-4o", 0.9)
	if result == nil {
		t.Error("warm-loaded vector should be searchable")
	}
}
