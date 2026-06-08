package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// SparseVector is a lexical/sparse embedding: token-id -> weight. Produced by
// bge-m3 alongside its dense vector; used for hybrid (dense+sparse) cache scoring.
type SparseVector map[int32]float32

// HybridEmbedder is an Embedder that additionally yields a sparse lexical vector.
// The semantic cache uses EmbedHybrid when the active embedder implements this,
// and falls back to the dense-only Embed path otherwise.
type HybridEmbedder interface {
	Embedder
	EmbedHybrid(text string) (dense []float32, sparse SparseVector, err error)
}

// ── Aratiri bge-m3 Embedder ─────────────────────────────────────────────────
//
// Calls the Aratiri platform-api embedding endpoint (BAAI/bge-m3), which returns
// a 1024-d dense vector and a sparse {token_id: weight} vector. Auth is the
// platform API key in the X-API-Key header (paste PLATFORM_API_KEY in Settings).
//
// Endpoint contract (platform-api routers/public_v1.py):
//   POST {baseURL}/embed
//   body: {"texts": ["..."], "outputs": ["dense","sparse"]}
//   resp: {"embeddings": [{"dense": [...1024...], "sparse": {"123": 0.4}}], "model": "BAAI/bge-m3"}

// AratiriEmbedder embeds text via the cluster bge-m3 service.
type AratiriEmbedder struct {
	baseURL    string // e.g. http://platform-api.service.consul:8000/api/v1
	apiKey     string // sent as X-API-Key
	dims       int    // 1024 for bge-m3
	httpClient *http.Client
}

// AratiriOption configures an AratiriEmbedder.
type AratiriOption func(*AratiriEmbedder)

// WithAratiriBaseURL sets the platform-api base URL (the path that exposes /embed).
func WithAratiriBaseURL(url string) AratiriOption {
	return func(e *AratiriEmbedder) {
		if url != "" {
			e.baseURL = url
		}
	}
}

// WithAratiriAPIKey sets the X-API-Key value (overrides PLATFORM_API_KEY env var).
func WithAratiriAPIKey(key string) AratiriOption {
	return func(e *AratiriEmbedder) { e.apiKey = key }
}

// NewAratiriEmbedder creates a bge-m3 embedding client. The API key falls back to
// the PLATFORM_API_KEY env var when not provided.
func NewAratiriEmbedder(opts ...AratiriOption) (*AratiriEmbedder, error) {
	e := &AratiriEmbedder{
		baseURL: "http://platform-api.service.consul:8000/api/v1",
		dims:    1024,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.apiKey == "" {
		e.apiKey = os.Getenv("PLATFORM_API_KEY")
	}
	if e.apiKey == "" {
		return nil, fmt.Errorf("bge-m3 embedder needs an API key: set PLATFORM_API_KEY or the Embedding API Key in Settings")
	}
	return e, nil
}

type aratiriEmbedRequest struct {
	Texts   []string `json:"texts"`
	Outputs []string `json:"outputs"`
}

type aratiriEmbedItem struct {
	Dense  []float32          `json:"dense"`
	Sparse map[string]float32 `json:"sparse"` // JSON object keys are strings
}

type aratiriEmbedResponse struct {
	Embeddings []aratiriEmbedItem `json:"embeddings"`
	Model      string             `json:"model"`
}

// Embed returns just the dense vector (Embedder interface).
func (e *AratiriEmbedder) Embed(text string) ([]float32, error) {
	dense, _, err := e.embed(text, []string{"dense"})
	return dense, err
}

// EmbedHybrid returns both the dense and sparse vectors (HybridEmbedder interface).
func (e *AratiriEmbedder) EmbedHybrid(text string) ([]float32, SparseVector, error) {
	return e.embed(text, []string{"dense", "sparse"})
}

// Dimensions returns the dense vector dimensionality (1024 for bge-m3).
func (e *AratiriEmbedder) Dimensions() int { return e.dims }

func (e *AratiriEmbedder) embed(text string, outputs []string) ([]float32, SparseVector, error) {
	reqBody := aratiriEmbedRequest{Texts: []string{text}, Outputs: outputs}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	url := e.baseURL + "/embed"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("embed API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	// 425 Too Early == bge-m3 GPU pod cold-starting. Treat as a transient miss.
	if resp.StatusCode == http.StatusTooEarly {
		return nil, nil, fmt.Errorf("bge-m3 warming (cold start); cache miss this request")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("embed API returned %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result aratiriEmbedResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(result.Embeddings) == 0 || len(result.Embeddings[0].Dense) == 0 {
		return nil, nil, fmt.Errorf("empty embedding in response")
	}

	item := result.Embeddings[0]
	var sparse SparseVector
	if len(item.Sparse) > 0 {
		sparse = make(SparseVector, len(item.Sparse))
		for k, v := range item.Sparse {
			id, perr := strconv.ParseInt(k, 10, 32)
			if perr != nil {
				continue // skip non-integer token ids defensively
			}
			sparse[int32(id)] = v
		}
	}
	return item.Dense, sparse, nil
}
