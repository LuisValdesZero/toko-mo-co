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

// RerankItem is one candidate document for reranking.
type RerankItem struct {
	ID   string
	Text string
}

// RerankResult is one scored candidate returned by a reranker, ordered best-first.
type RerankResult struct {
	ID    string
	Score float64
	Index int // original index in the input slice
}

// ErrRerankerWarming signals the reranker GPU pod is cold-starting (HTTP 425).
// Callers treat reranking as best-effort and fall back to their prior ordering.
var ErrRerankerWarming = fmt.Errorf("reranker warming (cold start)")

// Reranker reorders candidate documents by relevance to a query using a
// cross-encoder (bge-reranker-v2-m3). Implemented by AratiriEmbedder via the
// platform-api /rerank endpoint. Optional: a dense-only embedder won't implement it.
type Reranker interface {
	Rerank(query string, items []RerankItem, topN int) ([]RerankResult, error)
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

// ── Reranking (bge-reranker-v2-m3 cross-encoder) ─────────────────────────────
//
// Endpoint contract (platform-api routers/public_v1.py):
//   POST {baseURL}/rerank
//   body: {"query": "...", "candidates": [{"id":"..","text":".."}], "top_n": N?}
//   resp: {"ranked": [{"id":"..","score":0.9,"index":0}, ...]}  (best-first)
//   425  -> reranker GPU pod warming; caller falls back to its existing order.

type aratiriRerankCandidate struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type aratiriRerankRequest struct {
	Query      string                   `json:"query"`
	Candidates []aratiriRerankCandidate `json:"candidates"`
	TopN       *int                     `json:"top_n,omitempty"`
}

type aratiriRankedItem struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Index int     `json:"index"`
}

type aratiriRerankResponse struct {
	Ranked []aratiriRankedItem `json:"ranked"`
}

// Rerank reorders items by relevance to query via the bge-reranker-v2-m3 endpoint.
// Returns ErrRerankerWarming on a 425 (cold GPU pod) so callers can fall back.
func (e *AratiriEmbedder) Rerank(query string, items []RerankItem, topN int) ([]RerankResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	cands := make([]aratiriRerankCandidate, len(items))
	for i, it := range items {
		cands[i] = aratiriRerankCandidate{ID: it.ID, Text: it.Text}
	}
	reqBody := aratiriRerankRequest{Query: query, Candidates: cands}
	if topN > 0 {
		reqBody.TopN = &topN
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request: %w", err)
	}

	req, err := http.NewRequest("POST", e.baseURL+"/rerank", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank API call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooEarly {
		return nil, ErrRerankerWarming
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank API returned %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result aratiriRerankResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal rerank response: %w", err)
	}
	out := make([]RerankResult, 0, len(result.Ranked))
	for _, r := range result.Ranked {
		out = append(out, RerankResult{ID: r.ID, Score: r.Score, Index: r.Index})
	}
	return out, nil
}

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
