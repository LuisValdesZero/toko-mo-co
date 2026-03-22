package detector

import (
	"sync"
	"time"
)

// StoredRequest represents a stored request
type StoredRequest struct {
	Prompt    string
	Timestamp time.Time
	SessionID string
}

// maxSessions caps the total number of tracked sessions to prevent unbounded
// memory growth. When the cap is reached, the oldest session (by most recent
// request timestamp) is evicted.
const maxSessions = 10000

// RequestStore stores recent requests per session with TTL-based eviction.
//
// Design: map[sessionID][]StoredRequest instead of a flat global slice.
//   - Add:       O(1) amortized (append to session slice)
//   - GetRecent: O(k) where k = requests in THIS session within the window
//                (was O(n) total across ALL sessions)
//   - Cleanup:   O(sessions * k_avg) — same as before but better cache locality
//
// The ttl window is configurable so it matches the config.LoopWindowMinutes setting.
type RequestStore struct {
	// sessions maps session ID → ring of recent prompts within the TTL window.
	// Only the calling session's slice is touched on Add/GetRecent.
	sessions map[string][]StoredRequest
	ttl      time.Duration
	mu       sync.RWMutex
}

// NewRequestStore creates a new RequestStore with the given TTL window.
// Pass 0 to use the default 5-minute window.
func NewRequestStore(ttl time.Duration) *RequestStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	rs := &RequestStore{
		sessions: make(map[string][]StoredRequest),
		ttl:      ttl,
	}
	go rs.cleanup()
	return rs
}

// Add appends a prompt to the session's recent-request list.
// Old requests outside the TTL window are trimmed inline so each session's
// slice stays small without waiting for the background cleaner.
func (rs *RequestStore) Add(prompt, sessionID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rs.ttl)

	// Trim stale entries for this session before appending.
	existing, isKnown := rs.sessions[sessionID]
	start := 0
	for start < len(existing) && existing[start].Timestamp.Before(cutoff) {
		start++
	}
	trimmed := existing[start:]

	// Evict oldest session if at capacity and this is a new session
	if !isKnown && len(rs.sessions) >= maxSessions {
		var oldestSID string
		var oldestTime time.Time
		for sid, entries := range rs.sessions {
			if len(entries) == 0 {
				oldestSID = sid
				break
			}
			lastEntry := entries[len(entries)-1].Timestamp
			if oldestSID == "" || lastEntry.Before(oldestTime) {
				oldestSID = sid
				oldestTime = lastEntry
			}
		}
		if oldestSID != "" {
			delete(rs.sessions, oldestSID)
		}
	}

	rs.sessions[sessionID] = append(trimmed, StoredRequest{
		Prompt:    prompt,
		Timestamp: now,
		SessionID: sessionID,
	})
}

// GetRecent returns all requests for sessionID that fall within the TTL window.
// The returned slice is a copy — safe to read without holding the lock.
func (rs *RequestStore) GetRecent(sessionID string) []StoredRequest {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	entries, ok := rs.sessions[sessionID]
	if !ok {
		return nil
	}

	cutoff := time.Now().Add(-rs.ttl)
	// Entries are in insertion order (oldest first); find the first live one.
	start := 0
	for start < len(entries) && entries[start].Timestamp.Before(cutoff) {
		start++
	}

	live := entries[start:]
	if len(live) == 0 {
		return nil
	}
	result := make([]StoredRequest, len(live))
	copy(result, live)
	return result
}

// cleanup runs on a ticker, evicting fully-expired sessions from the map.
// Individual session slices are already trimmed inline in Add(), so this
// mainly reclaims the map entries for sessions that have gone quiet.
func (rs *RequestStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rs.mu.Lock()
		cutoff := time.Now().Add(-rs.ttl)
		for sessionID, entries := range rs.sessions {
			// Find first entry that's still within the window.
			start := 0
			for start < len(entries) && entries[start].Timestamp.Before(cutoff) {
				start++
			}
			if start == len(entries) {
				// All entries expired — remove the session entirely.
				delete(rs.sessions, sessionID)
			} else {
				rs.sessions[sessionID] = entries[start:]
			}
		}
		rs.mu.Unlock()
	}
}
