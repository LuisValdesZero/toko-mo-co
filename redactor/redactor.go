package redactor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// Config holds the redaction configuration, typically built from the global config.
type Config struct {
	Enabled    bool
	Mode       string          // "redact" | "hash" | "placeholder"
	Categories map[string]bool // key → enabled
}

// Detection records a single redaction that was applied.
type Detection struct {
	Category string `json:"category"`
	Original string `json:"original,omitempty"` // for logging (first 20 chars)
	Position int    `json:"position"`
}

// Result is the output of a Redact call.
type Result struct {
	Text       string
	Detections []Detection
}

// Redact scans text for enabled PII/secret patterns and replaces matches
// according to the configured mode. Returns the (possibly modified) text and
// a list of detections.
func Redact(text string, cfg Config) Result {
	if !cfg.Enabled || len(cfg.Categories) == 0 {
		return Result{Text: text}
	}

	result := text
	var detections []Detection

	for _, cat := range AllCategories {
		if !cfg.Categories[cat.Key] {
			continue
		}

		for _, pat := range cat.Patterns {
			result = pat.ReplaceAllStringFunc(result, func(match string) string {
				// If there's a post-match validator, check it
				if cat.Validate != nil && !cat.Validate(match) {
					return match // false positive, leave unchanged
				}

				detections = append(detections, Detection{
					Category: cat.Key,
					Original: truncate(match, 20),
				})

				return replacement(match, cat, cfg.Mode)
			})
		}
	}

	return Result{Text: result, Detections: detections}
}

// replacement returns the replacement string for a match based on mode.
func replacement(match string, cat PatternCategory, mode string) string {
	switch mode {
	case "hash":
		h := sha256.Sum256([]byte(match))
		return fmt.Sprintf("[SHA:%x]", h[:4]) // first 8 hex chars
	case "placeholder":
		return cat.Placeholder
	default: // "redact"
		return cat.Tag
	}
}

// truncate returns the first n characters of s, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── Request body redaction ──────────────────────────────────────────────────

// RedactRequestBody parses a JSON request body, redacts message content strings,
// re-marshals, and returns the new body + total detection count.
//
// Supports both API formats:
//   - OpenAI:    messages[i].content is a string
//   - Anthropic: messages[i].content is an array of {type:"text", text:"..."}
//
// Also redacts the top-level "system" field (Anthropic system prompt).
// RedactRequestBody parses a JSON request body, redacts message content strings,
// re-marshals, and returns the new body, total detection count, per-category
// counts, and any error.
//
// The categoryCounts map keys are category keys ("email", "phone", etc.) and
// values are how many times that category was detected in this request.
func RedactRequestBody(bodyBytes []byte, apiFormat string, cfg Config) ([]byte, int, map[string]int, error) {
	if !cfg.Enabled {
		return bodyBytes, 0, nil, nil
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return bodyBytes, 0, nil, fmt.Errorf("json unmarshal: %w", err)
	}

	var allDetections []Detection

	// Redact messages array
	if msgs, ok := body["messages"]; ok {
		if msgArr, ok := msgs.([]interface{}); ok {
			for _, msg := range msgArr {
				msgMap, ok := msg.(map[string]interface{})
				if !ok {
					continue
				}
				detections := redactMessageContent(msgMap, cfg)
				allDetections = append(allDetections, detections...)
			}
		}
	}

	// Redact top-level "system" field (Anthropic format — can be string or array)
	if sys, ok := body["system"]; ok {
		switch v := sys.(type) {
		case string:
			r := Redact(v, cfg)
			if len(r.Detections) > 0 {
				body["system"] = r.Text
				allDetections = append(allDetections, r.Detections...)
			}
		case []interface{}:
			// Anthropic system can be an array of content blocks
			for _, block := range v {
				if blockMap, ok := block.(map[string]interface{}); ok {
					if text, ok := blockMap["text"].(string); ok {
						r := Redact(text, cfg)
						if len(r.Detections) > 0 {
							blockMap["text"] = r.Text
							allDetections = append(allDetections, r.Detections...)
						}
					}
				}
			}
		}
	}

	totalCount := len(allDetections)
	if totalCount == 0 {
		return bodyBytes, 0, nil, nil
	}

	// Build per-category counts
	catCounts := make(map[string]int)
	for _, d := range allDetections {
		catCounts[d.Category]++
	}

	newBytes, err := json.Marshal(body)
	if err != nil {
		return bodyBytes, 0, nil, fmt.Errorf("json marshal: %w", err)
	}

	return newBytes, totalCount, catCounts, nil
}

// redactMessageContent handles both OpenAI (content=string) and Anthropic
// (content=array of blocks) message formats. Also scans the "name" field
// to prevent PII from bypassing detection via field splitting.
// Returns detections found.
func redactMessageContent(msg map[string]interface{}, cfg Config) []Detection {
	var detections []Detection

	// ── Scan "name" field (prevents field-splitting bypass) ──────────
	if name, ok := msg["name"].(string); ok {
		r := Redact(name, cfg)
		if len(r.Detections) > 0 {
			msg["name"] = r.Text
			detections = append(detections, r.Detections...)
		}
	}

	// ── Scan "content" field ────────────────────────────────────────
	content, ok := msg["content"]
	if !ok {
		return detections
	}

	switch v := content.(type) {
	case string:
		// OpenAI format: content is a plain string
		r := Redact(v, cfg)
		if len(r.Detections) > 0 {
			msg["content"] = r.Text
			detections = append(detections, r.Detections...)
		}

	case []interface{}:
		// Anthropic format: content is array of {type:"text", text:"..."}
		for _, block := range v {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			// Only redact text blocks
			if blockMap["type"] != "text" {
				continue
			}
			text, ok := blockMap["text"].(string)
			if !ok {
				continue
			}
			r := Redact(text, cfg)
			if len(r.Detections) > 0 {
				blockMap["text"] = r.Text
				detections = append(detections, r.Detections...)
			}
		}
	}

	return detections
}

// ParseCategories converts a comma-separated string of category keys into a map.
func ParseCategories(csv string) map[string]bool {
	m := make(map[string]bool)
	if csv == "" {
		return m
	}
	for _, key := range strings.Split(csv, ",") {
		key = strings.TrimSpace(key)
		if key != "" {
			m[key] = true
		}
	}
	return m
}
