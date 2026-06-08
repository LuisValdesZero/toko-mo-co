package embedding

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAratiriEmbedder_EmbedHybrid(t *testing.T) {
	var gotPath, gotKey string
	var gotBody aratiriEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(aratiriEmbedResponse{
			Model: "BAAI/bge-m3",
			Embeddings: []aratiriEmbedItem{{
				Dense:  []float32{0.1, 0.2, 0.3},
				Sparse: map[string]float32{"42": 0.5, "1001": 0.9},
			}},
		})
	}))
	defer srv.Close()

	e, err := NewAratiriEmbedder(WithAratiriBaseURL(srv.URL), WithAratiriAPIKey("test-key"))
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if e.Dimensions() != 1024 {
		t.Errorf("dims = %d, want 1024", e.Dimensions())
	}

	dense, sparse, err := e.EmbedHybrid("hola mundo")
	if err != nil {
		t.Fatalf("EmbedHybrid: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/embed") {
		t.Errorf("path = %q, want .../embed", gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("X-API-Key = %q, want test-key", gotKey)
	}
	if len(gotBody.Texts) != 1 || gotBody.Texts[0] != "hola mundo" {
		t.Errorf("request texts = %v", gotBody.Texts)
	}
	if len(dense) != 3 {
		t.Errorf("dense len = %d, want 3", len(dense))
	}
	if sparse[42] != 0.5 || sparse[1001] != 0.9 {
		t.Errorf("sparse = %v, want {42:0.5, 1001:0.9}", sparse)
	}

	// Embed (dense-only) requests only the dense output.
	if _, err := e.Embed("x"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(gotBody.Outputs) != 1 || gotBody.Outputs[0] != "dense" {
		t.Errorf("Embed outputs = %v, want [dense]", gotBody.Outputs)
	}
}

func TestAratiriEmbedder_RequiresKey(t *testing.T) {
	t.Setenv("PLATFORM_API_KEY", "")
	if _, err := NewAratiriEmbedder(WithAratiriBaseURL("http://x")); err == nil {
		t.Error("expected error when no API key is configured")
	}
}

// AratiriEmbedder must satisfy the HybridEmbedder interface.
var _ HybridEmbedder = (*AratiriEmbedder)(nil)
