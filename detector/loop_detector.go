package detector

import (
	"fmt"
	"log"
)

// LoopDetectionResult represents the result of loop detection
type LoopDetectionResult struct {
	LoopDetected  bool
	SimilarCount  int
	Severity      string // "low", "medium", "high"
	WarningLevel  int    // 1, 2, or 3
}

// LoopDetector detects looping patterns in requests
type LoopDetector struct {
	store               *RequestStore
	threshold           int     // Number of similar requests to trigger
	similarityThreshold float64 // Similarity threshold (0.0 to 1.0)
}

// NewLoopDetector creates a new loop detector
func NewLoopDetector(store *RequestStore, threshold int, similarityThreshold float64) *LoopDetector {
	return &LoopDetector{
		store:               store,
		threshold:           threshold,
		similarityThreshold: similarityThreshold,
	}
}

// DetectLoop checks if a prompt is part of a loop
func (ld *LoopDetector) DetectLoop(prompt, sessionID string) LoopDetectionResult {
	// Store this request before comparing
	ld.store.Add(prompt, sessionID)

	result := LoopDetectionResult{
		LoopDetected: false,
		SimilarCount: 0,
	}

	// Get recent requests (includes the one we just added)
	recentRequests := ld.store.GetRecent(sessionID)
	log.Printf("[LOOP] session=%s stored=%d threshold=%d prompt=%.40q",
		sessionID, len(recentRequests), ld.threshold, prompt)

	if len(recentRequests) < ld.threshold {
		return result
	}

	// Count similar requests
	similarCount := 0
	for _, req := range recentRequests {
		similarity := CalculateSimilarity(prompt, req.Prompt)
		if similarity >= ld.similarityThreshold {
			similarCount++
		}
	}

	result.SimilarCount = similarCount
	log.Printf("[LOOP] similarCount=%d/%d detected=%v", similarCount, ld.threshold, similarCount >= ld.threshold)

	// Check if threshold is met
	if similarCount >= ld.threshold {
		result.LoopDetected = true

		// Determine severity based on count
		if similarCount >= 10 {
			result.Severity = "high"
			result.WarningLevel = 3
		} else if similarCount >= 5 {
			result.Severity = "medium"
			result.WarningLevel = 2
		} else {
			result.Severity = "low"
			result.WarningLevel = 1
		}
	}

	return result
}

// GenerateWarningMessage generates a warning message based on detection result
func GenerateWarningMessage(result LoopDetectionResult, sessionCost float64) string {
	if !result.LoopDetected {
		return ""
	}

	switch result.WarningLevel {
	case 1:
		return fmt.Sprintf("\n\n💰 Cost Note: Similar requests detected (%d occurrences). Current session: $%.2f", result.SimilarCount, sessionCost)
	case 2:
		return fmt.Sprintf("\n\n⚠️ Budget Advisory: Potential loop detected (%d similar requests). Session cost: $%.2f", result.SimilarCount, sessionCost)
	case 3:
		return fmt.Sprintf("\n\n🚨 Cost Alert: Repeated loop pattern (%d occurrences). Session burn: $%.2f. Consider reviewing task approach.", result.SimilarCount, sessionCost)
	default:
		return fmt.Sprintf("\n\n💰 Similar requests detected. Session cost: $%.2f", sessionCost)
	}
}
