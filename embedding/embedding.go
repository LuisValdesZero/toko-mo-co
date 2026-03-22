// Package embedding provides vector embedding generation for semantic cache
// and memory features. It abstracts the embedding provider so the rest of
// the system works against an interface.
//
// Currently supported: OpenAI text-embedding-3-small (default).
// Future: Ollama, Vertex AI, etc.
package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Embedder generates vector embeddings from text.
type Embedder interface {
	// Embed returns a float32 vector for the given text.
	Embed(text string) ([]float32, error)

	// Dimensions returns the expected vector dimensionality.
	Dimensions() int
}

// ── OpenAI Embedder ─────────────────────────────────────────────────────────

// OpenAIEmbedder calls the OpenAI embeddings API.
type OpenAIEmbedder struct {
	apiKey     string
	model      string
	dims       int
	baseURL    string
	httpClient *http.Client
	mu         sync.RWMutex
}

// OpenAIOption configures an OpenAIEmbedder.
type OpenAIOption func(*OpenAIEmbedder)

// WithModel sets the embedding model (default: text-embedding-3-small).
func WithModel(model string) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.model = model }
}

// WithDimensions sets the expected vector dimensions (default: 1536).
func WithDimensions(dims int) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.dims = dims }
}

// WithBaseURL overrides the OpenAI base URL (for proxies or compatible APIs).
func WithBaseURL(url string) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.baseURL = url }
}

// WithAPIKey explicitly sets the API key (overrides OPENAI_API_KEY env var).
func WithAPIKey(key string) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.apiKey = key }
}

// NewOpenAIEmbedder creates an OpenAI embedding client.
// If no API key is provided, it reads from OPENAI_API_KEY env var.
func NewOpenAIEmbedder(opts ...OpenAIOption) (*OpenAIEmbedder, error) {
	e := &OpenAIEmbedder{
		model:   "text-embedding-3-small",
		dims:    1536,
		baseURL: "https://api.openai.com",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(e)
	}

	if e.apiKey == "" {
		e.apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if e.apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key required: set OPENAI_API_KEY or use WithAPIKey()")
	}

	return e, nil
}

// embeddingRequest is the OpenAI embeddings API request body.
type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// embeddingResponse is the OpenAI embeddings API response.
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed generates an embedding vector for the given text.
func (e *OpenAIEmbedder) Embed(text string) ([]float32, error) {
	reqBody := embeddingRequest{
		Model: e.model,
		Input: text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := e.baseURL + "/v1/embeddings"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API returned %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("embedding API error: %s", result.Error.Message)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding in response")
	}

	return result.Data[0].Embedding, nil
}

// Dimensions returns the configured vector dimensionality.
func (e *OpenAIEmbedder) Dimensions() int {
	return e.dims
}

// truncateStr truncates a string to n characters.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
