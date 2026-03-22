package injector

import (
	"encoding/json"
	"fmt"
)

// InjectionMode defines how warnings are delivered
type InjectionMode string

const (
	ModeContent  InjectionMode = "content"   // Inject into response text
	ModeMetadata InjectionMode = "metadata"  // Add to proxy_metadata field
	ModeHybrid   InjectionMode = "hybrid"    // Both
)

// InjectionConfig holds injection configuration
type InjectionConfig struct {
	Mode                 InjectionMode
	ContentThreshold     float64 // Cost threshold to switch to content injection (e.g., $10)
	InjectMetadata       bool
	InjectContent        bool
}

// Metadata represents proxy metadata to inject
type Metadata struct {
	Warning      string  `json:"warning,omitempty"`
	Severity     string  `json:"severity,omitempty"`
	SessionCost  float64 `json:"session_cost"`
	LoopDetected bool    `json:"loop_detected"`
	WarningLevel int     `json:"warning_level,omitempty"`
}

// InjectIntoOpenAIChunk injects warning into an OpenAI SSE chunk
func InjectIntoOpenAIChunk(chunkData []byte, warning string, metadata Metadata, config InjectionConfig) ([]byte, error) {
	var chunk map[string]interface{}
	if err := json.Unmarshal(chunkData, &chunk); err != nil {
		return chunkData, err
	}

	// Check if we should inject into content
	shouldInjectContent := config.InjectContent || 
		(config.Mode == ModeHybrid && metadata.SessionCost >= config.ContentThreshold)

	// Inject into content if needed
	if shouldInjectContent && warning != "" {
		choices, ok := chunk["choices"].([]interface{})
		if ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					// Append warning to content
					if content, exists := delta["content"]; exists {
						delta["content"] = fmt.Sprintf("%s%s", content, warning)
					} else {
						delta["content"] = warning
					}
				}
			}
		}
	}

	// Inject metadata if needed
	if config.InjectMetadata {
		chunk["proxy_metadata"] = metadata
	}

	return json.Marshal(chunk)
}

// InjectIntoAnthropicChunk injects warning into an Anthropic SSE chunk
func InjectIntoAnthropicChunk(chunkData []byte, warning string, metadata Metadata, config InjectionConfig) ([]byte, error) {
	var chunk map[string]interface{}
	if err := json.Unmarshal(chunkData, &chunk); err != nil {
		return chunkData, err
	}

	shouldInjectContent := config.InjectContent || 
		(config.Mode == ModeHybrid && metadata.SessionCost >= config.ContentThreshold)

	// Inject into content if needed
	if shouldInjectContent && warning != "" {
		if chunkType, ok := chunk["type"].(string); ok && chunkType == "content_block_delta" {
			if delta, ok := chunk["delta"].(map[string]interface{}); ok {
				if text, exists := delta["text"]; exists {
					delta["text"] = fmt.Sprintf("%s%s", text, warning)
				} else {
					delta["text"] = warning
				}
			}
		}
	}

	// Inject metadata
	if config.InjectMetadata {
		chunk["proxy_metadata"] = metadata
	}

	return json.Marshal(chunk)
}

// InjectIntoNonStreamingResponse injects into a non-streaming response
func InjectIntoNonStreamingResponse(responseData []byte, warning string, metadata Metadata, config InjectionConfig, provider string) ([]byte, error) {
	var response map[string]interface{}
	if err := json.Unmarshal(responseData, &response); err != nil {
		return responseData, err
	}

	shouldInjectContent := config.InjectContent || 
		(config.Mode == ModeHybrid && metadata.SessionCost >= config.ContentThreshold)

	// Inject into content based on provider
	if shouldInjectContent && warning != "" {
		if provider == "openai" {
			choices, ok := response["choices"].([]interface{})
			if ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if message, ok := choice["message"].(map[string]interface{}); ok {
						if content, exists := message["content"]; exists {
							message["content"] = fmt.Sprintf("%s%s", content, warning)
						}
					}
				}
			}
		} else if provider == "anthropic" {
			if content, ok := response["content"].([]interface{}); ok && len(content) > 0 {
				if contentBlock, ok := content[0].(map[string]interface{}); ok {
					if text, exists := contentBlock["text"]; exists {
						contentBlock["text"] = fmt.Sprintf("%s%s", text, warning)
					}
				}
			}
		}
	}

	// Inject metadata
	if config.InjectMetadata {
		response["proxy_metadata"] = metadata
	}

	return json.Marshal(response)
}
