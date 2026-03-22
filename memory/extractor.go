package memory

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractFacts extracts factual statements from a conversation exchange.
// It uses a simple heuristic approach that doesn't require an additional LLM call.
//
// The extraction logic:
//  1. Looks at user messages for stated preferences, instructions, and context
//  2. Looks at assistant responses for confirmed facts and decisions
//  3. Filters out generic/trivial content
//
// This avoids the cost overhead of making a separate LLM call for extraction
// (which mem0 does). For a proxy, keeping extraction costs at zero is important
// since every request would trigger extraction.
func ExtractFacts(provider string, bodyBytes []byte, responseBytes []byte) []string {
	var facts []string

	// Extract from request messages
	requestFacts := extractFromRequest(bodyBytes)
	facts = append(facts, requestFacts...)

	// Extract from response (confirmed facts, decisions)
	if len(responseBytes) > 0 {
		responseFacts := extractFromResponse(provider, responseBytes)
		facts = append(facts, responseFacts...)
	}

	// Deduplicate and filter
	return filterFacts(facts)
}

// extractFromRequest pulls factual content from the user's messages.
func extractFromRequest(bodyBytes []byte) []string {
	var reqData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqData); err != nil {
		return nil
	}

	var facts []string

	// OpenAI / Anthropic messages format
	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			msgMap, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msgMap["role"].(string)

			// Only extract from user messages (not system prompts)
			if role != "user" {
				continue
			}

			content := extractMessageContent(msgMap)
			if content == "" {
				continue
			}

			// Extract factual statements from user messages
			extracted := extractFactualStatements(content)
			facts = append(facts, extracted...)
		}
	}

	// Gemini contents format
	if contents, ok := reqData["contents"].([]interface{}); ok {
		for _, c := range contents {
			cMap, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := cMap["role"].(string)
			if role != "user" {
				continue
			}
			if partsArr, ok := cMap["parts"].([]interface{}); ok {
				for _, part := range partsArr {
					pMap, ok := part.(map[string]interface{})
					if !ok {
						continue
					}
					if text, ok := pMap["text"].(string); ok {
						extracted := extractFactualStatements(text)
						facts = append(facts, extracted...)
					}
				}
			}
		}
	}

	return facts
}

// extractFromResponse extracts confirmed information from assistant responses.
// Handles both text content and structured outputs (tool_use/function_call).
func extractFromResponse(provider string, responseBytes []byte) []string {
	var data map[string]interface{}
	if err := json.Unmarshal(responseBytes, &data); err != nil {
		return nil
	}

	var facts []string

	// ── OpenAI format ──────────────────────────────────────────────────────
	if choices, ok := data["choices"].([]interface{}); ok && len(choices) > 0 {
		if msg, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{}); ok {
			// Text content
			if content, ok := msg["content"].(string); ok && len(content) >= 30 {
				facts = append(facts, extractFactualStatements(content)...)
			}
			// OpenAI tool_calls (structured output)
			if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
				for _, tc := range toolCalls {
					tcMap, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := tcMap["function"].(map[string]interface{})
					if fn == nil {
						continue
					}
					fnName, _ := fn["name"].(string)
					argsStr, _ := fn["arguments"].(string)
					facts = append(facts, extractFactsFromToolCall(fnName, argsStr)...)
				}
			}
			// Legacy OpenAI function_call (deprecated but still in use)
			if fnCall, ok := msg["function_call"].(map[string]interface{}); ok {
				fnName, _ := fnCall["name"].(string)
				argsStr, _ := fnCall["arguments"].(string)
				facts = append(facts, extractFactsFromToolCall(fnName, argsStr)...)
			}
		}
	}

	// ── Anthropic format ───────────────────────────────────────────────────
	if contentArr, ok := data["content"].([]interface{}); ok {
		for _, block := range contentArr {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := blockMap["type"].(string)

			switch blockType {
			case "text":
				if text, ok := blockMap["text"].(string); ok && len(text) >= 30 {
					facts = append(facts, extractFactualStatements(text)...)
				}
			case "tool_use":
				toolName, _ := blockMap["name"].(string)
				// Anthropic tool_use input is already a JSON object (not a string)
				if input, ok := blockMap["input"].(map[string]interface{}); ok {
					inputBytes, _ := json.Marshal(input)
					facts = append(facts, extractFactsFromToolCall(toolName, string(inputBytes))...)
				}
			}
		}
	}

	if len(facts) == 0 {
		return nil
	}
	return facts
}

// extractFactsFromToolCall extracts memorable facts from a tool call's name and arguments.
// This handles structured output from agents that use tool_use/function_calling.
//
// Strategy: Parse the JSON arguments and look for decision-relevant fields.
// Common patterns across agent frameworks:
//   - {"action": "buy", "reasoning": "R:R ratio favorable"}
//   - {"recommendation": "hold", "confidence": 0.85, "factors": [...]}
//   - {"result": "positive", "explanation": "Strong earnings beat"}
func extractFactsFromToolCall(fnName, argsStr string) []string {
	if argsStr == "" && fnName == "" {
		return nil
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
		return nil
	}

	var facts []string

	// Keys that commonly contain decision-relevant information
	decisionKeys := map[string]bool{
		"action": true, "decision": true, "recommendation": true, "verdict": true,
		"reasoning": true, "rationale": true, "explanation": true, "analysis": true,
		"conclusion": true, "summary": true, "assessment": true, "evaluation": true,
		"result": true, "outcome": true, "status": true,
	}

	// Build a fact from the tool name + key decision fields
	var parts []string
	if fnName != "" {
		parts = append(parts, fmt.Sprintf("Called %s", fnName))
	}

	for key, val := range args {
		lowerKey := strings.ToLower(key)

		// Check if this is a decision-relevant field
		if !decisionKeys[lowerKey] {
			continue
		}

		switch v := val.(type) {
		case string:
			if len(v) >= 2 && len(v) <= 500 {
				parts = append(parts, fmt.Sprintf("%s: %s", key, v))
			}
		case float64:
			parts = append(parts, fmt.Sprintf("%s: %.2f", key, v))
		case bool:
			parts = append(parts, fmt.Sprintf("%s: %v", key, v))
		case []interface{}:
			// Arrays of strings (e.g., factors, reasons)
			var items []string
			for _, item := range v {
				if s, ok := item.(string); ok && len(s) > 3 {
					items = append(items, s)
				}
				if len(items) >= 3 {
					break
				}
			}
			if len(items) > 0 {
				parts = append(parts, fmt.Sprintf("%s: %s", key, strings.Join(items, "; ")))
			}
		}
	}

	if len(parts) >= 2 {
		// Combine into a single fact: "Called execute_trade — action: buy, reasoning: R:R favorable"
		fact := parts[0] + " — " + strings.Join(parts[1:], ", ")
		if len(fact) <= 500 {
			facts = append(facts, fact)
		}
	}

	return facts
}

// extractFactualStatements identifies factual statements from text.
// Uses heuristic patterns that indicate factual or preference-based content.
func extractFactualStatements(text string) []string {
	if len(text) < 15 || len(text) > 5000 {
		return nil
	}

	var facts []string

	sentences := splitSentences(text)
	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		if len(sentence) < 15 || len(sentence) > 500 {
			continue
		}

		// Skip questions
		if strings.HasSuffix(sentence, "?") {
			continue
		}

		// Skip generic/trivial content
		if isTrivial(sentence) {
			continue
		}

		// Look for factual patterns
		lower := strings.ToLower(sentence)
		isFactual := false

		// Preference indicators
		prefPatterns := []string{
			"i prefer", "i like", "i want", "i need", "i use",
			"i work with", "i'm using", "i am using",
			"my ", "we use", "we're using", "we are using",
			"our ", "the project uses", "the app uses",
			"i always", "i never", "i usually",
		}
		for _, p := range prefPatterns {
			if strings.Contains(lower, p) {
				isFactual = true
				break
			}
		}

		// Technical context indicators
		techPatterns := []string{
			"running on", "deployed on", "built with",
			"written in", "configured with", "using version",
			"database is", "framework is", "language is",
			"architecture", "stack", "infrastructure",
		}
		if !isFactual {
			for _, p := range techPatterns {
				if strings.Contains(lower, p) {
					isFactual = true
					break
				}
			}
		}

		// Decision/instruction patterns
		decisionPatterns := []string{
			"should always", "should never", "must be",
			"don't use", "do not use", "avoid",
			"make sure", "ensure that", "remember to",
			"important:", "note:", "rule:",
		}
		if !isFactual {
			for _, p := range decisionPatterns {
				if strings.Contains(lower, p) {
					isFactual = true
					break
				}
			}
		}

		if isFactual {
			facts = append(facts, sentence)
		}
	}

	return facts
}

// extractMessageContent extracts text content from a message map.
func extractMessageContent(msgMap map[string]interface{}) string {
	switch content := msgMap["content"].(type) {
	case string:
		return content
	case []interface{}:
		// Anthropic content blocks
		var parts []string
		for _, block := range content {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := blockMap["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// splitSentences splits text into sentences at common delimiters.
func splitSentences(text string) []string {
	// Simple sentence splitting — handles ". ", "! ", "\n"
	var sentences []string
	current := strings.Builder{}

	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		current.WriteRune(ch)

		// Split on sentence-ending punctuation followed by space/newline
		if (ch == '.' || ch == '!' || ch == '\n') && current.Len() > 10 {
			if i+1 >= len(runes) || runes[i+1] == ' ' || runes[i+1] == '\n' || ch == '\n' {
				s := strings.TrimSpace(current.String())
				if s != "" {
					sentences = append(sentences, s)
				}
				current.Reset()
			}
		}
	}

	// Remaining text
	if s := strings.TrimSpace(current.String()); s != "" {
		sentences = append(sentences, s)
	}

	return sentences
}

// isTrivial returns true if the sentence is too generic to be a useful memory.
func isTrivial(sentence string) bool {
	lower := strings.ToLower(sentence)

	trivialPhrases := []string{
		"hello", "hi there", "hey", "good morning", "good afternoon",
		"thank you", "thanks", "please help", "can you help",
		"sure", "okay", "yes", "no", "got it",
		"here is", "here's", "let me", "i'll",
		"that sounds good", "sounds good", "perfect",
		"i understand", "i see", "right",
	}

	for _, phrase := range trivialPhrases {
		if lower == phrase || strings.HasPrefix(lower, phrase+".") || strings.HasPrefix(lower, phrase+"!") {
			return true
		}
	}

	// Too short to be meaningful
	words := strings.Fields(sentence)
	if len(words) < 4 {
		return true
	}

	return false
}

// filterFacts removes duplicates and very similar facts.
func filterFacts(facts []string) []string {
	if len(facts) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var unique []string

	for _, f := range facts {
		normalized := strings.ToLower(strings.TrimSpace(f))
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		unique = append(unique, f)
	}

	// Cap at 5 facts per extraction to avoid noise
	if len(unique) > 5 {
		unique = unique[:5]
	}

	return unique
}

// BuildMemoryContext formats retrieved memories into a system prompt section.
func BuildMemoryContext(memories []SearchResult) string {
	if len(memories) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[Memory Context — relevant facts from previous conversations]\n")
	for i, m := range memories {
		b.WriteString(fmt.Sprintf("- %s (confidence: %.0f%%)\n", m.Entry.Fact, m.Similarity*100))
		if i >= 9 { // max 10 memories in context
			break
		}
	}
	return b.String()
}
