package detector

import (
	"strings"

	"github.com/texttheater/golang-levenshtein/levenshtein"
)

// maxSimilarityLen caps the string length for Levenshtein comparison.
// Prompts longer than this are truncated — the first N chars are usually
// enough to determine similarity, and it bounds the O(n×m) cost.
const maxSimilarityLen = 500

// CalculateSimilarity calculates similarity between two strings (0.0 to 1.0).
// Includes early-exit optimizations:
//   - Exact match → 1.0 (no Levenshtein needed)
//   - Length ratio check → if lengths differ by more than the similarity
//     threshold allows, return 0 immediately
//   - Long strings are truncated to maxSimilarityLen to cap O(n×m) cost
func CalculateSimilarity(s1, s2 string) float64 {
	// Normalize strings
	s1 = normalizeString(s1)
	s2 = normalizeString(s2)

	if s1 == s2 {
		return 1.0
	}

	l1, l2 := len(s1), len(s2)
	if l1 == 0 && l2 == 0 {
		return 0.0
	}

	// Early exit: if lengths differ too much, similarity can't be high.
	// For similarity >= 0.8, the shorter must be at least 80% of the longer.
	if l1 > 0 && l2 > 0 {
		ratio := float64(min(l1, l2)) / float64(max(l1, l2))
		if ratio < 0.5 {
			return ratio // rough estimate, guaranteed below any useful threshold
		}
	}

	// Truncate long strings to cap Levenshtein cost
	if len(s1) > maxSimilarityLen {
		s1 = s1[:maxSimilarityLen]
	}
	if len(s2) > maxSimilarityLen {
		s2 = s2[:maxSimilarityLen]
	}

	// Calculate Levenshtein distance
	distance := levenshtein.DistanceForStrings([]rune(s1), []rune(s2), levenshtein.DefaultOptions)

	// Convert to similarity score
	maxLen := max(len(s1), len(s2))
	if maxLen == 0 {
		return 0.0
	}

	similarity := 1.0 - (float64(distance) / float64(maxLen))
	return similarity
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// normalizeString normalizes a string for comparison
func normalizeString(s string) string {
	// Convert to lowercase
	s = strings.ToLower(s)

	// Trim whitespace
	s = strings.TrimSpace(s)

	// Remove extra whitespace
	s = strings.Join(strings.Fields(s), " ")

	return s
}

// max returns the maximum of two integers
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
