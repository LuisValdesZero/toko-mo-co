package rules

import (
	"fmt"
	"sync"
	"time"
)

// bucketKey uniquely identifies one rate-limit counter.
// Format: "req:<scope>:<agentID>:<ruleID>" or "cost:<scope>:<agentID>:<ruleID>"
type bucketKey string

// tokenBucket is a single per-key counter.
// It supports two modes:
//   - Request-rate (windowSec > 0, maxTokens > 0): token-bucket refill
//   - Cost/token quota (quota > 0, periodSec > 0): fixed-period accumulator
type tokenBucket struct {
	mu sync.Mutex

	// ── Request-rate mode ─────────────────────────────────────────────────
	windowSec  int
	maxTokens  int
	tokens     int
	lastRefill time.Time

	// ── Quota mode (cost / daily tokens) ─────────────────────────────────
	quota     float64   // max allowed per period
	used      float64   // accumulated since resetAt
	resetAt   time.Time // when to zero `used`
	periodSec int       // 86400 for daily, 2592000 for ~monthly
}

// consumeOne refills the bucket proportionally to elapsed time, then
// tries to consume one token. Returns false if none remain.
func (b *tokenBucket) consumeOne() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	refillRate := float64(b.maxTokens) / float64(b.windowSec) // tokens/sec
	refill := int(elapsed * refillRate)
	if refill > 0 {
		b.tokens += refill
		if b.tokens > b.maxTokens {
			b.tokens = b.maxTokens
		}
		b.lastRefill = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// consumeQuota checks and accumulates a floating-point cost/token value
// against a fixed-period quota. Returns false if the quota would be exceeded.
func (b *tokenBucket) consumeQuota(amount float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	// Reset if the period has rolled over
	if now.After(b.resetAt) {
		b.used = 0
		b.resetAt = now.Add(time.Duration(b.periodSec) * time.Second)
	}
	if b.used+amount > b.quota {
		return false
	}
	b.used += amount
	return true
}

// ── RateLimiter ───────────────────────────────────────────────────────────────

// RateLimiter manages all per-key token buckets.
// Goroutine-safe. Buckets are created lazily.
// The outer RWMutex protects only the map; each bucket has its own Mutex.
type RateLimiter struct {
	mu      sync.RWMutex
	buckets map[bucketKey]*tokenBucket
}

// NewRateLimiter creates an empty rate limiter with background cleanup.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		buckets: make(map[bucketKey]*tokenBucket),
	}
	go rl.cleanupLoop()
	return rl
}

// cleanupLoop periodically evicts stale buckets that haven't been used
// for longer than their window/period, preventing unbounded map growth.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		for key, b := range rl.buckets {
			b.mu.Lock()
			stale := false
			if b.windowSec > 0 {
				// Request-rate bucket: stale if no refill for 2× the window
				stale = now.Sub(b.lastRefill) > 2*time.Duration(b.windowSec)*time.Second
			} else if b.periodSec > 0 {
				// Quota bucket: stale if past the reset time and no usage
				stale = now.After(b.resetAt) && b.used == 0
			}
			b.mu.Unlock()
			if stale {
				delete(rl.buckets, key)
			}
		}
		rl.mu.Unlock()
	}
}

// getOrCreate returns the bucket for key, creating it with initFn if absent.
// Uses a read-then-write double-check pattern to minimise lock contention.
func (rl *RateLimiter) getOrCreate(key bucketKey, initFn func() *tokenBucket) *tokenBucket {
	rl.mu.RLock()
	b := rl.buckets[key]
	rl.mu.RUnlock()
	if b != nil {
		return b
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if b = rl.buckets[key]; b != nil {
		return b
	}
	b = initFn()
	rl.buckets[key] = b
	return b
}

// PeekRequest checks whether the next request from agentID would be allowed
// under the given rate limit WITHOUT consuming a token from the bucket.
// Used by requestCountCondition.Evaluate() so the condition is side-effect-free
// and the rateLimitAction.Apply() remains the sole consumer.
func (rl *RateLimiter) PeekRequest(scope, agentID string, ruleID int64, maxRequests, windowSec int) bool {
	key := bucketKey(fmt.Sprintf("req:%s:%s:%d", scope, agentID, ruleID))

	rl.mu.RLock()
	b := rl.buckets[key]
	rl.mu.RUnlock()

	if b == nil {
		// Bucket not yet created → limit not yet hit → allowed
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Compute how many tokens would be available after a time-based refill
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	refillRate := float64(b.maxTokens) / float64(b.windowSec)
	available := b.tokens + int(elapsed*refillRate)
	if available > b.maxTokens {
		available = b.maxTokens
	}
	return available > 0
}

// CheckAndConsumeRequest enforces a sliding-window request-rate limit.
// Returns true (allowed) or false (rate limited).
// scope: "agent" or "global"; agentID: the agent identifier; ruleID: rule's DB id.
func (rl *RateLimiter) CheckAndConsumeRequest(
	scope, agentID string, ruleID int64,
	maxRequests, windowSec int,
) bool {
	key := bucketKey(fmt.Sprintf("req:%s:%s:%d", scope, agentID, ruleID))
	b := rl.getOrCreate(key, func() *tokenBucket {
		return &tokenBucket{
			windowSec:  windowSec,
			maxTokens:  maxRequests,
			tokens:     maxRequests,
			lastRefill: time.Now(),
		}
	})
	return b.consumeOne()
}

// CheckAndConsumeCost enforces a fixed-period cost quota (in USD).
// Returns true (allowed) or false (quota exceeded).
// periodSec: 86400 for daily, 2592000 for ~monthly.
func (rl *RateLimiter) CheckAndConsumeCost(
	scope, agentID string, ruleID int64,
	addCost, maxCost float64, periodSec int,
) bool {
	key := bucketKey(fmt.Sprintf("cost:%s:%s:%d", scope, agentID, ruleID))
	b := rl.getOrCreate(key, func() *tokenBucket {
		return &tokenBucket{
			quota:     maxCost,
			resetAt:   time.Now().Add(time.Duration(periodSec) * time.Second),
			periodSec: periodSec,
		}
	})
	return b.consumeQuota(addCost)
}

// CheckAndConsumeTokens enforces a fixed-period token quota.
// Returns true (allowed) or false (quota exceeded).
func (rl *RateLimiter) CheckAndConsumeTokens(
	scope, agentID string, ruleID int64,
	addTokens, maxTokens, periodSec int,
) bool {
	key := bucketKey(fmt.Sprintf("tok:%s:%s:%d", scope, agentID, ruleID))
	b := rl.getOrCreate(key, func() *tokenBucket {
		return &tokenBucket{
			quota:     float64(maxTokens),
			resetAt:   time.Now().Add(time.Duration(periodSec) * time.Second),
			periodSec: periodSec,
		}
	})
	return b.consumeQuota(float64(addTokens))
}
