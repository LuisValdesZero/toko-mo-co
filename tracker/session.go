package tracker

import (
	"log"
	"sync"
	"time"
)

// Session represents a tracking session
type Session struct {
	ID           string
	StartTime    time.Time
	LastSeen     time.Time
	TotalCost    float64
	InputTokens  int
	OutputTokens int
	RequestCount int
	mu           sync.RWMutex
}

// SessionTracker manages multiple sessions with bounded memory.
//
// Eviction policy:
//   - Background ticker evicts sessions idle for longer than maxAge.
//   - If the live session count exceeds maxSize, the oldest-seen sessions
//     are evicted first (simple O(n) sweep — runs infrequently).
//
// All public methods are safe for concurrent use.
type SessionTracker struct {
	sessions map[string]*Session
	maxAge   time.Duration // sessions idle longer than this are evicted
	maxSize  int           // hard cap on in-memory session count
	mu       sync.RWMutex
}

// NewSessionTracker creates a new SessionTracker.
// maxAge: idle TTL before eviction (e.g. 24 * time.Hour)
// maxSize: maximum number of sessions kept in memory (e.g. 10_000)
func NewSessionTracker(maxAge time.Duration, maxSize int) *SessionTracker {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	if maxSize <= 0 {
		maxSize = 10_000
	}
	st := &SessionTracker{
		sessions: make(map[string]*Session),
		maxAge:   maxAge,
		maxSize:  maxSize,
	}
	go st.evictLoop()
	return st
}

// defaultSessionID is used for all requests that don't supply an X-Session-ID header.
// Grouping them under one session lets loop detection work across sequential requests
// from the same client process (the common case for agent testing).
const defaultSessionID = "default"

// GetOrCreateSession retrieves or creates a session.
// If sessionID is empty, requests are grouped under the shared "default" session
// so that loop detection can see patterns across multiple requests.
func (st *SessionTracker) GetOrCreateSession(sessionID string) *Session {
	if sessionID == "" {
		sessionID = defaultSessionID
	}

	// Fast path: already exists
	st.mu.RLock()
	if session, exists := st.sessions[sessionID]; exists {
		st.mu.RUnlock()
		session.touch()
		return session
	}
	st.mu.RUnlock()

	// Slow path: create
	st.mu.Lock()
	defer st.mu.Unlock()

	// Double-check after acquiring write lock
	if session, exists := st.sessions[sessionID]; exists {
		session.touch()
		return session
	}

	// Enforce size cap before inserting — evict oldest if needed
	if len(st.sessions) >= st.maxSize {
		st.evictOldest(1)
	}

	now := time.Now()
	session := &Session{
		ID:        sessionID,
		StartTime: now,
		LastSeen:  now,
	}
	st.sessions[sessionID] = session
	return session
}

// RestoreSession loads a persisted session back into memory at startup.
func (st *SessionTracker) RestoreSession(sessionID string, totalCost float64, inputTokens, outputTokens, requestCount int, startTime time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.sessions[sessionID] = &Session{
		ID:           sessionID,
		StartTime:    startTime,
		LastSeen:     time.Now(),
		TotalCost:    totalCost,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		RequestCount: requestCount,
	}
}

// evictLoop runs on an hourly ticker to remove idle/excess sessions.
func (st *SessionTracker) evictLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		st.evictStale()
	}
}

// evictStale removes sessions that have been idle longer than maxAge.
// Uses a two-phase approach: collect stale IDs under read lock, then delete
// under write lock — minimises the time the write lock is held.
func (st *SessionTracker) evictStale() {
	cutoff := time.Now().Add(-st.maxAge)

	// Phase 1: collect candidates under read lock
	st.mu.RLock()
	var staleIDs []string
	for id, s := range st.sessions {
		if s.lastSeenTime().Before(cutoff) {
			staleIDs = append(staleIDs, id)
		}
	}
	st.mu.RUnlock()

	if len(staleIDs) == 0 {
		return
	}

	// Phase 2: delete under write lock (fast — only iterates stale list)
	st.mu.Lock()
	evicted := 0
	for _, id := range staleIDs {
		// Re-check under write lock in case the session was touched between phases
		if s, exists := st.sessions[id]; exists && s.lastSeenTime().Before(cutoff) {
			delete(st.sessions, id)
			evicted++
		}
	}
	remaining := len(st.sessions)
	st.mu.Unlock()

	if evicted > 0 {
		log.Printf("[SESSION] evicted %d stale sessions (remaining: %d)", evicted, remaining)
	}
}

// evictOldest removes the n sessions with the oldest LastSeen timestamps.
// Must be called with the write lock held.
func (st *SessionTracker) evictOldest(n int) {
	type entry struct {
		id   string
		seen time.Time
	}
	// Collect candidates — small enough to sort inline for typical cap sizes.
	candidates := make([]entry, 0, len(st.sessions))
	for id, s := range st.sessions {
		candidates = append(candidates, entry{id, s.lastSeenTime()})
	}
	// Partial selection sort for n — O(n * total) but n is always 1 here.
	for i := 0; i < n && i < len(candidates); i++ {
		minIdx := i
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].seen.Before(candidates[minIdx].seen) {
				minIdx = j
			}
		}
		candidates[i], candidates[minIdx] = candidates[minIdx], candidates[i]
		delete(st.sessions, candidates[i].id)
	}
}

// ActiveSessionCount returns the number of in-memory sessions.
func (st *SessionTracker) ActiveSessionCount() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.sessions)
}

// touch updates the session's LastSeen timestamp (called without holding the outer lock).
func (s *Session) touch() {
	s.mu.Lock()
	s.LastSeen = time.Now()
	s.mu.Unlock()
}

// lastSeenTime returns LastSeen safely.
func (s *Session) lastSeenTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastSeen
}

// AddCost adds cost to a session
func (s *Session) AddCost(cost float64, inputTokens, outputTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalCost += cost
	s.InputTokens += inputTokens
	s.OutputTokens += outputTokens
	s.RequestCount++
	s.LastSeen = time.Now()
}

// GetStats returns session statistics
func (s *Session) GetStats() (totalCost float64, inputTokens, outputTokens, requestCount int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.TotalCost, s.InputTokens, s.OutputTokens, s.RequestCount
}

// GetDuration returns how long the session has been running
func (s *Session) GetDuration() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return time.Since(s.StartTime)
}
