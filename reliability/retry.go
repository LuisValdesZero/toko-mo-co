package reliability

import (
	"fmt"
	"log"
	"math"
	"time"
)

// RetryConfig defines retry behavior
type RetryConfig struct {
	Enabled       bool
	MaxAttempts   int
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	Multiplier    float64 // Exponential backoff multiplier (default: 2.0)
}

// RetryResult contains the outcome of a retry operation
type RetryResult struct {
	Attempts      int
	TotalDuration time.Duration
	Success       bool
	LastError     error
}

// IsRetryableError determines if an error should trigger a retry
func IsRetryableError(statusCode int, err error) bool {
	// Retry on server errors and rate limits
	switch statusCode {
	case 429: // Too Many Requests
		return true
	case 500, 502, 503, 504: // Server errors
		return true
	case 529: // Anthropic overloaded
		return true
	}

	// Timeout errors (even if status is 0)
	if err != nil && (statusCode == 0 || statusCode == 408) {
		return true
	}

	return false
}

// RetryWithBackoff executes a function with exponential backoff
func RetryWithBackoff(
	config RetryConfig,
	operation func() (int, error), // Returns (statusCode, error)
	logPrefix string,
) (int, error, RetryResult) {
	result := RetryResult{
		Attempts: 0,
		Success:  false,
	}

	if !config.Enabled {
		statusCode, err := operation()
		result.Attempts = 1
		result.Success = err == nil
		result.LastError = err
		return statusCode, err, result
	}

	startTime := time.Now()
	delay := config.InitialDelay

	for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
		result.Attempts = attempt

		statusCode, err := operation()

		if err == nil {
			result.Success = true
			result.TotalDuration = time.Since(startTime)
			if attempt > 1 {
				log.Printf("[RETRY] %s: Success on attempt %d/%d (total: %v)",
					logPrefix, attempt, config.MaxAttempts, result.TotalDuration)
			}
			return statusCode, nil, result
		}

		result.LastError = err

		// Check if error is retryable
		if !IsRetryableError(statusCode, err) {
			log.Printf("[RETRY] %s: Non-retryable error (status=%d): %v", logPrefix, statusCode, err)
			result.TotalDuration = time.Since(startTime)
			return statusCode, err, result
		}

		// Last attempt failed
		if attempt == config.MaxAttempts {
			log.Printf("[RETRY] %s: All %d attempts failed (total: %v). Last error: %v",
				logPrefix, config.MaxAttempts, time.Since(startTime), err)
			result.TotalDuration = time.Since(startTime)
			return statusCode, err, result
		}

		// Wait before next retry with exponential backoff
		log.Printf("[RETRY] %s: Attempt %d/%d failed (status=%d): %v. Retrying in %v...",
			logPrefix, attempt, config.MaxAttempts, statusCode, err, delay)

		time.Sleep(delay)

		// Calculate next delay with exponential backoff
		delay = time.Duration(float64(delay) * config.Multiplier)
		if delay > config.MaxDelay {
			delay = config.MaxDelay
		}
	}

	result.TotalDuration = time.Since(startTime)
	return 0, result.LastError, result
}

// CalculateBackoffDelay calculates the delay for a given attempt
func CalculateBackoffDelay(attempt int, initialDelay, maxDelay time.Duration, multiplier float64) time.Duration {
	delay := time.Duration(float64(initialDelay) * math.Pow(multiplier, float64(attempt-1)))
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// NewRetryConfig creates a retry configuration from individual parameters
func NewRetryConfig(enabled bool, maxAttempts int, initialDelayMs, maxDelayMs int) RetryConfig {
	return RetryConfig{
		Enabled:      enabled,
		MaxAttempts:  maxAttempts,
		InitialDelay: time.Duration(initialDelayMs) * time.Millisecond,
		MaxDelay:     time.Duration(maxDelayMs) * time.Millisecond,
		Multiplier:   2.0, // Standard exponential backoff
	}
}

// FormatRetryInfo creates a human-readable retry summary
func FormatRetryInfo(result RetryResult) string {
	if result.Attempts <= 1 {
		return ""
	}

	if result.Success {
		return fmt.Sprintf("succeeded after %d attempts (%v)",
			result.Attempts, result.TotalDuration.Round(time.Millisecond))
	}

	return fmt.Sprintf("failed after %d attempts (%v)",
		result.Attempts, result.TotalDuration.Round(time.Millisecond))
}
