package proxy

import (
	"testing"
)

func TestExtractUsage(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		provider       string
		wantIn         int
		wantCached     int
		wantOut        int
		wantOK         bool
	}{
		{
			name:       "OpenAI non-streaming response with usage field",
			body:       `{"usage": {"prompt_tokens": 150, "completion_tokens": 42}}`,
			provider:   "openai",
			wantIn:     150,
			wantCached: 0,
			wantOut:    42,
			wantOK:     true,
		},
		{
			name:       "OpenAI with cached tokens",
			body:       `{"usage": {"prompt_tokens": 200, "completion_tokens": 50, "prompt_tokens_details": {"cached_tokens": 80}}}`,
			provider:   "openai",
			wantIn:     120,
			wantCached: 80,
			wantOut:    50,
			wantOK:     true,
		},
		{
			name:       "Anthropic response",
			body:       `{"usage": {"input_tokens": 100, "output_tokens": 35}}`,
			provider:   "anthropic",
			wantIn:     100,
			wantCached: 0,
			wantOut:    35,
			wantOK:     true,
		},
		{
			name:       "Anthropic with cache read",
			body:       `{"usage": {"input_tokens": 300, "output_tokens": 60, "cache_read_input_tokens": 200}}`,
			provider:   "anthropic",
			wantIn:     300, // Anthropic's input_tokens already excludes cached tokens
			wantCached: 200,
			wantOut:    60,
			wantOK:     true,
		},
		{
			name:       "Gemini response",
			body:       `{"usageMetadata": {"promptTokenCount": 180, "candidatesTokenCount": 45}}`,
			provider:   "gemini",
			wantIn:     180,
			wantCached: 0,
			wantOut:    45,
			wantOK:     true,
		},
		{
			name:       "Empty/no usage field (OpenAI streaming content chunk)",
			body:       `{"choices": [{"delta": {"content": "hello"}}]}`,
			provider:   "openai",
			wantIn:     0,
			wantCached: 0,
			wantOut:    0,
			wantOK:     false,
		},
		{
			name:       "Invalid JSON",
			body:       `not json`,
			provider:   "openai",
			wantIn:     0,
			wantCached: 0,
			wantOut:    0,
			wantOK:     false,
		},
		{
			name:       "OpenAI SSE final streaming chunk with usage",
			body:       `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":30}}`,
			provider:   "openai",
			wantIn:     120,
			wantCached: 0,
			wantOut:    30,
			wantOK:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotIn, gotCached, gotOut, gotOK := extractUsageFromResponse([]byte(tc.body), tc.provider)

			if gotOK != tc.wantOK {
				t.Errorf("ok: got %v, want %v", gotOK, tc.wantOK)
			}
			if gotIn != tc.wantIn {
				t.Errorf("inputTokens: got %d, want %d", gotIn, tc.wantIn)
			}
			if gotCached != tc.wantCached {
				t.Errorf("cachedInputTokens: got %d, want %d", gotCached, tc.wantCached)
			}
			if gotOut != tc.wantOut {
				t.Errorf("outputTokens: got %d, want %d", gotOut, tc.wantOut)
			}
		})
	}
}
