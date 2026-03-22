package detector

import (
	"math"
	"strings"
	"testing"
	"time"
)

// ── CalculateSimilarity tests ─────────────────────────────────────────────

func TestSimilarity_ExactMatch(t *testing.T) {
	s := CalculateSimilarity("hello world", "hello world")
	if s != 1.0 {
		t.Errorf("exact match: got %f, want 1.0", s)
	}
}

func TestSimilarity_CaseInsensitive(t *testing.T) {
	s := CalculateSimilarity("HELLO WORLD", "hello world")
	if s != 1.0 {
		t.Errorf("case insensitive: got %f, want 1.0", s)
	}
}

func TestSimilarity_WhitespaceNormalized(t *testing.T) {
	s := CalculateSimilarity("hello   world", "hello world")
	if s != 1.0 {
		t.Errorf("whitespace normalized: got %f, want 1.0", s)
	}
}

func TestSimilarity_BothEmpty(t *testing.T) {
	// After normalization both empty strings are identical → 1.0
	s := CalculateSimilarity("", "")
	if s != 1.0 {
		t.Errorf("both empty (normalized identical): got %f, want 1.0", s)
	}
}

func TestSimilarity_SimilarStrings(t *testing.T) {
	s := CalculateSimilarity("hello world", "hello earth")
	// Levenshtein: "world" vs "earth" = 4 edits out of 11 chars → ~0.27
	if s < 0.1 || s > 0.9 {
		t.Errorf("similar strings: got %f, expected between 0.1 and 0.9", s)
	}
}

func TestSimilarity_CompletelyDifferent(t *testing.T) {
	s := CalculateSimilarity("abc", "xyz")
	if s >= 0.5 {
		t.Errorf("completely different: got %f, expected < 0.5", s)
	}
}

func TestSimilarity_LengthRatioEarlyExit(t *testing.T) {
	// Short vs very long — should early-exit with low score
	s := CalculateSimilarity("hi", "this is a very long string that has nothing to do with anything")
	if s >= 0.5 {
		t.Errorf("length ratio mismatch: got %f, expected < 0.5", s)
	}
}

func TestSimilarity_LongStrings_Truncated(t *testing.T) {
	// Strings longer than maxSimilarityLen (500) should be truncated
	long1 := strings.Repeat("a", 600)
	long2 := strings.Repeat("a", 600)
	s := CalculateSimilarity(long1, long2)
	if s != 1.0 {
		t.Errorf("identical long strings: got %f, want 1.0", s)
	}
}

func TestSimilarity_PromptLikeStrings(t *testing.T) {
	prompt1 := "Write a function to sort an array of integers"
	prompt2 := "Write a function to sort an array of strings"
	s := CalculateSimilarity(prompt1, prompt2)
	if s < 0.8 {
		t.Errorf("similar prompts: got %f, expected >= 0.8", s)
	}
}

// ── RequestStore tests ────────────────────────────────────────────────────

func TestRequestStore_AddAndGetRecent(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}

	rs.Add("prompt1", "session-1")
	rs.Add("prompt2", "session-1")

	recent := rs.GetRecent("session-1")
	if len(recent) != 2 {
		t.Errorf("expected 2 recent requests, got %d", len(recent))
	}
}

func TestRequestStore_SessionIsolation(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}

	rs.Add("prompt-a", "session-A")
	rs.Add("prompt-b", "session-B")

	recentA := rs.GetRecent("session-A")
	recentB := rs.GetRecent("session-B")
	if len(recentA) != 1 {
		t.Errorf("session-A: expected 1, got %d", len(recentA))
	}
	if len(recentB) != 1 {
		t.Errorf("session-B: expected 1, got %d", len(recentB))
	}
}

func TestRequestStore_UnknownSession(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}

	recent := rs.GetRecent("nonexistent")
	if recent != nil {
		t.Errorf("unknown session should return nil, got %v", recent)
	}
}

func TestRequestStore_TTLEviction(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      100 * time.Millisecond,
	}

	rs.Add("old-prompt", "session-1")

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	recent := rs.GetRecent("session-1")
	if len(recent) != 0 {
		t.Errorf("expired entries should be excluded, got %d", len(recent))
	}
}

func TestRequestStore_InlineTrimming(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      100 * time.Millisecond,
	}

	rs.Add("old-prompt", "session-1")
	time.Sleep(150 * time.Millisecond)

	// Add new prompt — should trim the old one inline
	rs.Add("new-prompt", "session-1")

	recent := rs.GetRecent("session-1")
	if len(recent) != 1 {
		t.Errorf("expected 1 after trim, got %d", len(recent))
	}
	if recent[0].Prompt != "new-prompt" {
		t.Errorf("expected 'new-prompt', got %q", recent[0].Prompt)
	}
}

func TestRequestStore_GetRecentRetursCopy(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}

	rs.Add("prompt", "session-1")
	recent := rs.GetRecent("session-1")

	// Modifying the returned slice should not affect the store
	recent[0].Prompt = "modified"
	original := rs.GetRecent("session-1")
	if original[0].Prompt == "modified" {
		t.Error("GetRecent should return a copy, not a reference")
	}
}

// ── LoopDetector tests ────────────────────────────────────────────────────

func TestLoopDetector_NoLoop(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}
	ld := NewLoopDetector(rs, 3, 0.8)

	result := ld.DetectLoop("unique prompt 1", "session-1")
	if result.LoopDetected {
		t.Error("single request should not trigger loop")
	}
}

func TestLoopDetector_LoopDetected(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}
	ld := NewLoopDetector(rs, 3, 0.8)

	// Send the same prompt 3 times
	ld.DetectLoop("What is the weather?", "session-1")
	ld.DetectLoop("What is the weather?", "session-1")
	result := ld.DetectLoop("What is the weather?", "session-1")

	if !result.LoopDetected {
		t.Error("3 identical prompts should trigger loop detection")
	}
	if result.SimilarCount < 3 {
		t.Errorf("SimilarCount: got %d, want >= 3", result.SimilarCount)
	}
}

func TestLoopDetector_SimilarPrompts(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}
	// Use a lower similarity threshold so these nearly-identical prompts match
	ld := NewLoopDetector(rs, 3, 0.7)

	// Very similar prompts (only the last word differs)
	ld.DetectLoop("Write a Python function to sort a list of integers using quicksort", "session-1")
	ld.DetectLoop("Write a Python function to sort a list of integers using mergesort", "session-1")
	result := ld.DetectLoop("Write a Python function to sort a list of integers using bubblesort", "session-1")

	if !result.LoopDetected {
		t.Error("similar prompts should trigger loop detection")
	}
}

func TestLoopDetector_DifferentPrompts(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}
	ld := NewLoopDetector(rs, 3, 0.8)

	ld.DetectLoop("What is the capital of France?", "session-1")
	ld.DetectLoop("How do I bake chocolate cake?", "session-1")
	result := ld.DetectLoop("Explain quantum entanglement", "session-1")

	if result.LoopDetected {
		t.Error("completely different prompts should not trigger loop")
	}
}

func TestLoopDetector_Severity(t *testing.T) {
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      5 * time.Minute,
	}
	ld := NewLoopDetector(rs, 3, 0.8)

	// Send 3 identical — low severity
	for i := 0; i < 3; i++ {
		ld.DetectLoop("repeated prompt", "session-1")
	}
	result3 := ld.DetectLoop("repeated prompt", "session-1")
	if result3.Severity != "low" {
		t.Errorf("4 similar: severity=%q, want 'low'", result3.Severity)
	}

	// Continue to 6 — medium
	for i := 0; i < 2; i++ {
		ld.DetectLoop("repeated prompt", "session-1")
	}
	result6 := ld.DetectLoop("repeated prompt", "session-1")
	if result6.Severity != "medium" {
		t.Errorf("7 similar: severity=%q, want 'medium'", result6.Severity)
	}

	// Continue to 11 — high
	for i := 0; i < 4; i++ {
		ld.DetectLoop("repeated prompt", "session-1")
	}
	result11 := ld.DetectLoop("repeated prompt", "session-1")
	if result11.Severity != "high" {
		t.Errorf("12 similar: severity=%q, want 'high'", result11.Severity)
	}
}

// ── Warning message tests ─────────────────────────────────────────────────

func TestGenerateWarningMessage_NoLoop(t *testing.T) {
	result := LoopDetectionResult{LoopDetected: false}
	msg := GenerateWarningMessage(result, 1.50)
	if msg != "" {
		t.Errorf("no loop should produce empty message, got %q", msg)
	}
}

func TestGenerateWarningMessage_Levels(t *testing.T) {
	tests := []struct {
		level    int
		contains string
	}{
		{1, "Cost Note"},
		{2, "Budget Advisory"},
		{3, "Cost Alert"},
	}

	for _, tt := range tests {
		result := LoopDetectionResult{
			LoopDetected: true,
			SimilarCount: 5,
			WarningLevel: tt.level,
		}
		msg := GenerateWarningMessage(result, 2.50)
		if !strings.Contains(msg, tt.contains) {
			t.Errorf("level %d: expected %q in message, got %q", tt.level, tt.contains, msg)
		}
	}
}

// ── normalizeString tests ─────────────────────────────────────────────────

func TestNormalizeString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "hello world"},
		{"  hello  ", "hello"},
		{"hello   world", "hello world"},
		{"", ""},
		{"  MIXED  cAsE  sTrInG  ", "mixed case string"},
	}

	for _, tt := range tests {
		got := normalizeString(tt.input)
		if got != tt.want {
			t.Errorf("normalizeString(%q): got %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── min/max helpers ───────────────────────────────────────────────────────

func TestMinMax(t *testing.T) {
	if min(3, 5) != 3 {
		t.Error("min(3,5) should be 3")
	}
	if min(5, 3) != 3 {
		t.Error("min(5,3) should be 3")
	}
	if max(3, 5) != 5 {
		t.Error("max(3,5) should be 5")
	}
	if max(5, 3) != 5 {
		t.Error("max(5,3) should be 5")
	}
}

const float64Epsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < float64Epsilon
}
