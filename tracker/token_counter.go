package tracker

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// TokenCounter handles token counting for different models.
// Thread-safe: the encoders map is protected by a sync.RWMutex
// since getEncoder is called from concurrent HTTP handler goroutines.
type TokenCounter struct {
	mu       sync.RWMutex
	encoders map[string]*tiktoken.Tiktoken
}

// NewTokenCounter creates a new token counter
func NewTokenCounter() (*TokenCounter, error) {
	return &TokenCounter{
		encoders: make(map[string]*tiktoken.Tiktoken),
	}, nil
}

// getEncoder gets or creates an encoder for a model
func (tc *TokenCounter) getEncoder(model string) (*tiktoken.Tiktoken, error) {
	// Fast path: read lock for cache hit
	tc.mu.RLock()
	if enc, exists := tc.encoders[model]; exists {
		tc.mu.RUnlock()
		return enc, nil
	}
	tc.mu.RUnlock()

	// Slow path: write lock for cache miss
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Double-check after acquiring write lock
	if enc, exists := tc.encoders[model]; exists {
		return enc, nil
	}

	// Map model to encoding
	encoding := "cl100k_base" // Default for GPT-4, GPT-3.5-turbo
	if model == "gpt-3.5-turbo" || model == "gpt-4" || model == "gpt-4-turbo" || model == "gpt-4o" || model == "gpt-4o-mini" {
		encoding = "cl100k_base"
	}

	enc, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, fmt.Errorf("failed to get encoding: %w", err)
	}

	tc.encoders[model] = enc
	return enc, nil
}

// CountTokens counts tokens in a string
func (tc *TokenCounter) CountTokens(text string, model string) (int, error) {
	enc, err := tc.getEncoder(model)
	if err != nil {
		return 0, err
	}

	tokens := enc.Encode(text, nil, nil)
	return len(tokens), nil
}

// CountOpenAIMessages counts tokens in OpenAI message format
func (tc *TokenCounter) CountOpenAIMessages(messages []map[string]interface{}, model string) (int, error) {
	total := 0

	for _, msg := range messages {
		// Add tokens for role and content
		if role, ok := msg["role"].(string); ok {
			roleTokens, err := tc.CountTokens(role, model)
			if err != nil {
				return 0, err
			}
			total += roleTokens
		}

		if content, ok := msg["content"].(string); ok {
			contentTokens, err := tc.CountTokens(content, model)
			if err != nil {
				return 0, err
			}
			total += contentTokens
		}

		// Add overhead per message (approximately 3-4 tokens)
		total += 4
	}

	// Add overhead for response priming
	total += 3

	return total, nil
}

// CountAnthropicMessages counts tokens for Anthropic format
func (tc *TokenCounter) CountAnthropicMessages(messages []map[string]interface{}, model string) (int, error) {
	// Anthropic uses similar token counting
	// For simplicity, we'll use the same logic
	return tc.CountOpenAIMessages(messages, model)
}

// ExtractMessagesFromRequest extracts messages from request body
func ExtractMessagesFromRequest(body []byte, provider string) ([]map[string]interface{}, error) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	messages, ok := data["messages"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no messages found in request")
	}

	result := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}

	return result, nil
}
