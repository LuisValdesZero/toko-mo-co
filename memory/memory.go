// Package memory provides a mem0-style memory layer for the Toko-Mo-Co proxy.
//
// It extracts factual memories from LLM conversations, embeds them as vectors,
// and retrieves relevant memories for future requests. This allows agents to be
// stateful across sessions without any client-side changes.
//
// Architecture:
//
//  1. After each successful LLM response, extract key facts asynchronously
//  2. Embed each fact and store in SQLite (vector BLOB + metadata)
//  3. Before each request, search for relevant memories by embedding the prompt
//  4. Inject relevant memories as system context (transparent to the client)
//
// Memories are scoped by agent_id so different agents maintain separate knowledge.
package memory

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pgvector/pgvector-go"

	"tokomoco/embedding"
	"tokomoco/store"
)

// Entry is a single stored memory fact.
type Entry struct {
	ID           int64     `json:"id"`
	AgentID      string    `json:"agent_id"`   // Scoped to agent
	SessionID    string    `json:"session_id"` // Session it was extracted from
	Fact         string    `json:"fact"`       // The extracted fact text
	Vector       []float32 `json:"-"`          // Embedding vector (not exposed in JSON)
	Provider     string    `json:"provider"`   // Provider context
	Model        string    `json:"model"`      // Model context
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"` // Updated on every search hit
	AccessCount  int64     `json:"access_count"`  // Incremented on each retrieval
	UpdatedAt    time.Time `json:"updated_at"`    // Set when fact is replaced via conflict resolution
}

// SearchResult is a memory match from a similarity search.
type SearchResult struct {
	Entry      *Entry  `json:"entry"`
	Similarity float64 `json:"similarity"` // Raw cosine similarity
	Score      float64 `json:"score"`      // Recency-weighted ranking score
}

// Store provides embedding-based memory storage and retrieval.
// Thread-safe with read-write locking.
type Store struct {
	mu         sync.RWMutex
	entries    []*Entry // in-memory for fast search
	embedder   embedding.Embedder
	db         store.Querier
	pg         bool // true when the backend is Postgres (native pgvector storage)
	dims       int
	maxEntries int
	threshold  float64 // similarity threshold for retrieval (default: 0.7)
	enabled    bool

	// Enhanced memory parameters
	recencyLambda  float64 // decay rate for recency weighting (default: 0.01)
	conflictThresh float64 // similarity threshold for conflict detection (default: 0.85)
	ttlDays        int     // days before unused memories are eviction candidates (default: 90)

	// Atomic metrics
	lookups atomic.Int64
	hits    atomic.Int64
	stored  atomic.Int64
	updated atomic.Int64 // conflict-resolution updates
	evicted atomic.Int64 // eviction count
}

// StoreOption configures optional parameters for the memory Store.
type StoreOption func(*Store)

// WithRecencyLambda sets the decay rate for recency-weighted scoring.
func WithRecencyLambda(lambda float64) StoreOption {
	return func(s *Store) {
		if lambda > 0 && lambda <= 0.1 {
			s.recencyLambda = lambda
		}
	}
}

// WithConflictThreshold sets the similarity threshold for conflict detection.
func WithConflictThreshold(thresh float64) StoreOption {
	return func(s *Store) {
		if thresh >= 0.5 && thresh < 0.95 {
			s.conflictThresh = thresh
		}
	}
}

// WithTTLDays sets the number of days before unused memories become eviction candidates.
func WithTTLDays(days int) StoreOption {
	return func(s *Store) {
		if days >= 7 && days <= 365 {
			s.ttlDays = days
		}
	}
}

// NewStore creates a memory store backed by SQLite.
func NewStore(db store.Querier, embedder embedding.Embedder, maxEntries int, threshold float64, enabled bool, opts ...StoreOption) (*Store, error) {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.7 // lower than semantic cache — memories are more loosely related
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}

	s := &Store{
		entries:        make([]*Entry, 0),
		embedder:       embedder,
		db:             db,
		pg:             db.Dialect() == store.Postgres,
		dims:           embedder.Dimensions(),
		maxEntries:     maxEntries,
		threshold:      threshold,
		enabled:        enabled,
		recencyLambda:  0.01, // 30-day-old memory retains ~74% weight
		conflictThresh: 0.85, // similarity threshold for conflict detection
		ttlDays:        90,   // memories not accessed in 90 days are eviction candidates
	}
	for _, opt := range opts {
		opt(s)
	}

	if err := s.migrate(); err != nil {
		return nil, err
	}

	s.warmLoad()
	return s, nil
}

// migrate creates the memory_entries table if it doesn't exist.
func (s *Store) migrate() error {
	dl := s.db.Dialect()
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memory_entries (
			%s,
			agent_id   TEXT    NOT NULL DEFAULT '',
			session_id TEXT    NOT NULL DEFAULT '',
			fact       TEXT    NOT NULL,
			vector     %s     NOT NULL,
			provider   TEXT   NOT NULL DEFAULT '',
			model      TEXT   NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`, dl.AutoIncPK(), dl.VectorCol(s.dims)),
		`CREATE INDEX IF NOT EXISTS idx_memory_agent   ON memory_entries(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_session ON memory_entries(session_id)`,
	}
	if s.pg {
		// Cosine ANN index — available for future SQL-side search (warm-load +
		// in-Go scoring is still used at runtime for the recency/conflict logic).
		stmts = append(stmts, `CREATE INDEX IF NOT EXISTS idx_memory_vector ON memory_entries USING hnsw (vector vector_cosine_ops)`)
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}

	// Add columns for the enhanced memory layer (idempotent).
	addCol := dl.AddColumn()
	for _, col := range []struct{ name, def string }{
		{"last_accessed", "INTEGER NOT NULL DEFAULT 0"},
		{"access_count", "INTEGER NOT NULL DEFAULT 0"},
		{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		s.db.Exec(`ALTER TABLE memory_entries ` + addCol + ` ` + col.name + ` ` + col.def) //nolint:errcheck
	}

	// Index for TTL-based eviction queries
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_last_accessed ON memory_entries(last_accessed)`) //nolint:errcheck

	return nil
}

// warmLoad loads all memory entries from the database into memory.
func (s *Store) warmLoad() {
	rows, err := s.db.Query(`
		SELECT id, agent_id, session_id, fact, vector, provider, model, created_at,
		       COALESCE(last_accessed, 0), COALESCE(access_count, 0), COALESCE(updated_at, 0)
		FROM memory_entries
		ORDER BY created_at DESC
		LIMIT ?`, s.maxEntries)
	if err != nil {
		log.Printf("[MEMORY] warm-load failed: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var e Entry
		var vecBytes []byte
		var vecPG pgvector.Vector
		var createdAt, lastAccessed, accessCount, updatedAt int64

		// Scan the vector column into the right target for the backend.
		vecTarget := any(&vecBytes)
		if s.pg {
			vecTarget = &vecPG
		}
		if err := rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Fact, vecTarget, &e.Provider, &e.Model,
			&createdAt, &lastAccessed, &accessCount, &updatedAt); err != nil {
			log.Printf("[MEMORY] scan error: %v", err)
			continue
		}

		if s.pg {
			e.Vector = vecPG.Slice()
		} else {
			e.Vector = bytesToFloat32(vecBytes)
		}
		if len(e.Vector) != s.dims {
			continue // dimension mismatch, skip stale entry
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		e.LastAccessed = time.Unix(lastAccessed, 0)
		e.AccessCount = accessCount
		e.UpdatedAt = time.Unix(updatedAt, 0)
		s.entries = append(s.entries, &e)
		count++
	}

	if count > 0 {
		log.Printf("[MEMORY] warm-loaded %d memories", count)
	}
}

// IsEnabled returns whether the memory layer is active.
func (s *Store) IsEnabled() bool {
	return s.enabled
}

// SetEnabled toggles the memory layer on/off at runtime.
func (s *Store) SetEnabled(enabled bool) {
	s.enabled = enabled
}

// Threshold returns the current similarity threshold.
func (s *Store) Threshold() float64 {
	return s.threshold
}

// SetThreshold updates the similarity threshold at runtime.
func (s *Store) SetThreshold(t float64) {
	if t > 0 && t <= 1 {
		s.threshold = t
	}
}

// SetRecencyLambda updates the recency decay rate at runtime.
func (s *Store) SetRecencyLambda(lambda float64) {
	if lambda > 0 && lambda <= 0.1 {
		s.recencyLambda = lambda
	}
}

// SetConflictThreshold updates the conflict detection threshold at runtime.
func (s *Store) SetConflictThreshold(thresh float64) {
	if thresh >= 0.5 && thresh < 0.95 {
		s.conflictThresh = thresh
	}
}

// SetTTLDays updates the TTL for unused memories at runtime.
func (s *Store) SetTTLDays(days int) {
	if days >= 7 && days <= 365 {
		s.ttlDays = days
	}
}

// Search finds memories relevant to the query text, scoped by agent.
// Returns up to maxResults matches above the threshold, sorted by similarity (descending).
func (s *Store) Search(queryText, agentID string, maxResults int) ([]SearchResult, error) {
	if !s.enabled || s.embedder == nil {
		return nil, nil
	}

	s.lookups.Add(1)

	queryVec, err := s.embedder.Embed(queryText)
	if err != nil {
		return nil, err
	}

	return s.SearchByVector(queryVec, agentID, maxResults), nil
}

// SearchByVector finds memories similar to the given vector, scoped by agent.
// Results are ranked by recency-weighted score (similarity * time decay).
// Access metadata (LastAccessed, AccessCount) is updated on each match.
func (s *Store) SearchByVector(queryVec []float32, agentID string, maxResults int) []SearchResult {
	if maxResults <= 0 {
		maxResults = 5
	}

	type scored struct {
		entry      *Entry
		sim        float64 // raw cosine similarity
		finalScore float64 // recency-weighted score
	}

	// Phase 1: Read lock — find matches
	s.mu.RLock()
	var matches []scored
	for _, e := range s.entries {
		// Scope to same agent (empty agentID matches all)
		if agentID != "" && e.AgentID != agentID {
			continue
		}

		sim := cosineSimilarity(queryVec, e.Vector)
		if sim >= s.threshold {
			// Apply recency weighting: finalScore = sim * exp(-lambda * daysSinceCreation)
			daysSince := time.Since(e.CreatedAt).Hours() / 24.0
			if daysSince < 0 {
				daysSince = 0
			}
			recencyMultiplier := math.Exp(-s.recencyLambda * daysSince)
			finalScore := sim * recencyMultiplier
			matches = append(matches, scored{entry: e, sim: sim, finalScore: finalScore})
		}
	}
	s.mu.RUnlock()

	// Sort by recency-weighted score descending (simple insertion sort — small N)
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].finalScore > matches[j-1].finalScore; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}

	// Limit results
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	if len(matches) > 0 {
		s.hits.Add(1)

		// Phase 2: Write lock — update access metadata on matched entries
		now := time.Now()
		matchedIDs := make([]int64, len(matches))
		for i, m := range matches {
			matchedIDs[i] = m.entry.ID
		}

		s.mu.Lock()
		for _, e := range s.entries {
			for _, id := range matchedIDs {
				if e.ID == id {
					e.LastAccessed = now
					e.AccessCount++
					break
				}
			}
		}
		s.mu.Unlock()

		// Async DB update for access metadata
		go func() {
			for _, id := range matchedIDs {
				s.db.Exec(`UPDATE memory_entries SET last_accessed = ?, access_count = access_count + 1 WHERE id = ?`,
					now.Unix(), id) //nolint:errcheck
			}
		}()
	}

	results := make([]SearchResult, len(matches))
	for i, m := range matches {
		results[i] = SearchResult{Entry: m.entry, Similarity: m.sim, Score: m.finalScore}
	}
	return results
}

// StoreFact embeds and persists a single memory fact.
func (s *Store) StoreFact(agentID, sessionID, fact, provider, model string) error {
	if !s.enabled || s.embedder == nil {
		return nil
	}

	vec, err := s.embedder.Embed(fact)
	if err != nil {
		return err
	}

	return s.StoreFactWithVector(agentID, sessionID, fact, vec, provider, model)
}

// StoreFactWithVector stores a fact with a pre-computed embedding vector.
// Implements conflict resolution: if a similar fact exists (0.85–0.95 similarity),
// it may be updated rather than duplicated (AUDN pattern from mem0).
func (s *Store) StoreFactWithVector(agentID, sessionID, fact string, vector []float32, provider, model string) error {
	// Check for duplicate / conflicting facts (same agent + similar vector)
	s.mu.RLock()
	for _, e := range s.entries {
		if e.AgentID == agentID {
			sim := cosineSimilarity(vector, e.Vector)
			if sim > 0.95 {
				// IDENTICAL meaning — skip (preserve existing behavior)
				s.mu.RUnlock()
				log.Printf("[MEMORY] skipping duplicate fact (sim=%.4f): %s", sim, truncate(fact, 60))
				return nil
			}
			if sim > s.conflictThresh {
				// CONFLICTING fact detected — check if new fact should replace old
				if shouldReplace(fact, e.Fact) {
					s.mu.RUnlock()
					log.Printf("[MEMORY] conflict detected (sim=%.4f), updating: %s → %s",
						sim, truncate(e.Fact, 40), truncate(fact, 40))
					return s.updateExistingEntry(e, fact, vector)
				}
				// Similar but not clearly a replacement — skip (conservative)
				s.mu.RUnlock()
				log.Printf("[MEMORY] similar fact exists (sim=%.4f), keeping existing: %s", sim, truncate(e.Fact, 60))
				return nil
			}
		}
	}
	s.mu.RUnlock()

	entry := &Entry{
		AgentID:   agentID,
		SessionID: sessionID,
		Fact:      fact,
		Vector:    vector,
		Provider:  provider,
		Model:     model,
		CreatedAt: time.Now(),
	}

	// Persist to DB first (to get auto-increment ID)
	id, err := s.persistToDB(entry)
	if err != nil {
		return err
	}
	entry.ID = id

	s.mu.Lock()
	// Evict if at capacity — per-agent quota check
	if len(s.entries) >= s.maxEntries {
		agentCount := s.countAgentEntriesLocked(agentID)
		numAgents := s.countDistinctAgentsLocked()
		perAgentQuota := s.maxEntries / max(numAgents, 1)
		if perAgentQuota < 100 {
			perAgentQuota = 100
		}

		if agentCount >= perAgentQuota {
			// This agent is at or over quota — evict from this agent
			s.evictForAgent(agentID)
		} else {
			// Global eviction — pick the best candidate across all agents
			s.evictForAgent("")
		}
	}
	s.entries = append(s.entries, entry)
	s.mu.Unlock()

	s.stored.Add(1)
	log.Printf("[MEMORY] stored fact for agent=%s: %s", agentID, truncate(fact, 80))
	return nil
}

// shouldReplace determines if newFact should replace oldFact.
// Heuristics:
//  1. New fact contains temporal update signals ("now", "switched to", "changed to")
//  2. New fact contains negation that old fact lacks ("not", "no longer")
//  3. New fact is significantly more specific (20%+ longer and shares key words)
func shouldReplace(newFact, oldFact string) bool {
	newLower := strings.ToLower(newFact)
	oldLower := strings.ToLower(oldFact)

	// Temporal update keywords — strong signal for replacement
	updateSignals := []string{"now ", "switched to", "changed to", "no longer", "instead of", "moved to", "replaced"}
	for _, sig := range updateSignals {
		if strings.Contains(newLower, sig) {
			return true
		}
	}

	// Negation of existing preference
	negationSignals := []string{"don't", "do not", "not ", "stopped", "quit"}
	for _, sig := range negationSignals {
		if strings.Contains(newLower, sig) && !strings.Contains(oldLower, sig) {
			return true
		}
	}

	// New fact is significantly more specific (longer by 20%+)
	if len(newFact) > len(oldFact)*120/100 {
		// Verify it's not just more words on a different topic
		// by checking that key words from old fact appear in new fact
		oldWords := strings.Fields(oldLower)
		matchCount := 0
		for _, w := range oldWords {
			if len(w) > 3 && strings.Contains(newLower, w) {
				matchCount++
			}
		}
		if len(oldWords) > 0 && matchCount >= len(oldWords)/3 {
			return true
		}
	}

	return false
}

// updateExistingEntry replaces the fact text and vector of an existing memory entry.
// Used during conflict resolution when a new fact supersedes an old one.
func (s *Store) updateExistingEntry(existing *Entry, newFact string, newVector []float32) error {
	now := time.Now()

	// Update in-memory
	s.mu.Lock()
	existing.Fact = newFact
	existing.Vector = newVector
	existing.UpdatedAt = now
	s.mu.Unlock()

	// Persist to DB
	_, err := s.db.Exec(`UPDATE memory_entries SET fact = ?, vector = ?, updated_at = ? WHERE id = ?`,
		newFact, s.encVec(newVector), now.Unix(), existing.ID)
	if err != nil {
		return err
	}

	s.updated.Add(1)
	log.Printf("[MEMORY] updated existing fact id=%d: %s", existing.ID, truncate(newFact, 80))
	return nil
}

// Delete removes a memory entry by ID.
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM memory_entries WHERE id = ?`, id)
	return err
}

// DeleteByAgent removes all memory entries for a given agent.
func (s *Store) DeleteByAgent(agentID string) (int, error) {
	s.mu.Lock()
	kept := make([]*Entry, 0, len(s.entries))
	removed := 0
	for _, e := range s.entries {
		if e.AgentID == agentID {
			removed++
		} else {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM memory_entries WHERE agent_id = ?`, agentID)
	return removed, err
}

// ListByAgent returns all memories for a given agent (most recent first).
func (s *Store) ListByAgent(agentID string, limit int) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}

	var results []*Entry
	// entries are stored newest-last from warmLoad (DESC) but new additions are appended
	// Iterate backwards to get newest first
	for i := len(s.entries) - 1; i >= 0 && len(results) < limit; i-- {
		e := s.entries[i]
		if agentID == "" || e.AgentID == agentID {
			results = append(results, e)
		}
	}
	return results
}

// Flush removes all memories from memory and DB.
func (s *Store) Flush() {
	s.mu.Lock()
	s.entries = make([]*Entry, 0)
	s.mu.Unlock()

	s.db.Exec(`DELETE FROM memory_entries`) //nolint:errcheck
	s.lookups.Store(0)
	s.hits.Store(0)
	s.stored.Store(0)
	s.updated.Store(0)
	s.evicted.Store(0)
}

// Count returns the total number of stored memories.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// CountByAgent returns the number of memories for a specific agent.
func (s *Store) CountByAgent(agentID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, e := range s.entries {
		if e.AgentID == agentID {
			count++
		}
	}
	return count
}

// Stats holds memory layer metrics.
type Stats struct {
	Enabled    bool    `json:"enabled"`
	Memories   int     `json:"memories"`
	Threshold  float64 `json:"threshold"`
	Lookups    int64   `json:"lookups"`
	Hits       int64   `json:"hits"`
	Stored     int64   `json:"stored"`
	HitRate    float64 `json:"hit_rate"`
	MaxEntries int     `json:"max_entries"`

	// Enhanced analytics
	Updated        int64              `json:"updated"`         // Conflict resolution updates
	Evicted        int64              `json:"evicted"`         // Eviction count
	AgentBreakdown []AgentMemoryStats `json:"agent_breakdown"` // Per-agent stats
	TopMemories    []MemoryDetail     `json:"top_memories"`    // Most-accessed memories (top 10)
	StaleCount     int                `json:"stale_count"`     // Memories not accessed in TTL
}

// AgentMemoryStats holds per-agent memory statistics.
type AgentMemoryStats struct {
	AgentID      string `json:"agent_id"`
	MemoryCount  int    `json:"memory_count"`
	TotalAccess  int64  `json:"total_access"`
	LastActivity int64  `json:"last_activity"` // unix timestamp
}

// MemoryDetail is a single memory with access metadata, for analytics display.
type MemoryDetail struct {
	ID           int64  `json:"id"`
	AgentID      string `json:"agent_id"`
	Fact         string `json:"fact"`
	AccessCount  int64  `json:"access_count"`
	CreatedAt    int64  `json:"created_at"`
	LastAccessed int64  `json:"last_accessed"`
}

// GetStats returns current memory layer metrics including per-agent analytics.
func (s *Store) GetStats() Stats {
	lookups := s.lookups.Load()
	hits := s.hits.Load()
	var hitRate float64
	if lookups > 0 {
		hitRate = float64(hits) / float64(lookups)
	}

	s.mu.RLock()

	// Per-agent breakdown
	agentMap := make(map[string]*AgentMemoryStats)
	staleCount := 0
	ttlCutoff := time.Now().AddDate(0, 0, -s.ttlDays)

	for _, e := range s.entries {
		as, ok := agentMap[e.AgentID]
		if !ok {
			as = &AgentMemoryStats{AgentID: e.AgentID}
			agentMap[e.AgentID] = as
		}
		as.MemoryCount++
		as.TotalAccess += e.AccessCount

		lastAct := e.CreatedAt.Unix()
		if e.LastAccessed.Unix() > lastAct {
			lastAct = e.LastAccessed.Unix()
		}
		if lastAct > as.LastActivity {
			as.LastActivity = lastAct
		}

		effectiveLast := e.LastAccessed
		if effectiveLast.IsZero() || effectiveLast.Unix() <= 0 {
			effectiveLast = e.CreatedAt
		}
		if effectiveLast.Before(ttlCutoff) {
			staleCount++
		}
	}

	// Top 10 most accessed memories
	type accessEntry struct {
		idx   int
		count int64
	}
	var sorted []accessEntry
	for i, e := range s.entries {
		if e.AccessCount > 0 {
			sorted = append(sorted, accessEntry{i, e.AccessCount})
		}
	}
	// Simple insertion sort (small N for top-10)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].count > sorted[j-1].count; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	topN := 10
	if len(sorted) < topN {
		topN = len(sorted)
	}
	topMemories := make([]MemoryDetail, topN)
	for i := 0; i < topN; i++ {
		e := s.entries[sorted[i].idx]
		topMemories[i] = MemoryDetail{
			ID:           e.ID,
			AgentID:      e.AgentID,
			Fact:         e.Fact,
			AccessCount:  e.AccessCount,
			CreatedAt:    e.CreatedAt.Unix(),
			LastAccessed: e.LastAccessed.Unix(),
		}
	}
	s.mu.RUnlock()

	agents := make([]AgentMemoryStats, 0, len(agentMap))
	for _, as := range agentMap {
		agents = append(agents, *as)
	}

	return Stats{
		Enabled:        s.enabled,
		Memories:       s.Count(),
		Threshold:      s.threshold,
		Lookups:        lookups,
		Hits:           hits,
		Stored:         s.stored.Load(),
		HitRate:        hitRate,
		MaxEntries:     s.maxEntries,
		Updated:        s.updated.Load(),
		Evicted:        s.evicted.Load(),
		AgentBreakdown: agents,
		TopMemories:    topMemories,
		StaleCount:     staleCount,
	}
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// persistToDB writes a memory entry to SQLite and returns the auto-increment ID.
func (s *Store) persistToDB(entry *Entry) (int64, error) {
	return s.db.InsertReturningID(`
		INSERT INTO memory_entries (agent_id, session_id, fact, vector, provider, model, created_at, last_accessed, access_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.AgentID, entry.SessionID, entry.Fact, s.encVec(entry.Vector), entry.Provider, entry.Model,
		entry.CreatedAt.Unix(), entry.LastAccessed.Unix(), entry.AccessCount, entry.UpdatedAt.Unix(),
	)
}

// encVec encodes a vector for the active backend: a native pgvector value for
// Postgres, or float32 BLOB bytes for SQLite.
func (s *Store) encVec(v []float32) any {
	if s.pg {
		return pgvector.NewVector(v)
	}
	return float32ToBytes(v)
}

// evictForAgent removes the best eviction candidate for the given agent.
// If agentID is empty, considers all agents (global eviction).
// Priority: stale (not accessed in TTL) > old+low-frequency > active+popular.
// Must be called under write lock.
func (s *Store) evictForAgent(agentID string) {
	if len(s.entries) == 0 {
		return
	}

	now := time.Now()
	ttlCutoff := now.AddDate(0, 0, -s.ttlDays)

	bestIdx := -1
	bestScore := math.MaxFloat64

	for i, e := range s.entries {
		if agentID != "" && e.AgentID != agentID {
			continue
		}

		// Determine effective last access time
		lastAccess := e.LastAccessed
		if lastAccess.IsZero() || lastAccess.Unix() == 0 {
			lastAccess = e.CreatedAt
		}

		var score float64
		if lastAccess.Before(ttlCutoff) {
			// Stale: score is very negative (guaranteed eviction over non-stale)
			score = -float64(now.Sub(lastAccess).Hours())
		} else {
			// Active: score = (1 + accessCount) * recencyFactor
			daysSince := now.Sub(lastAccess).Hours() / 24.0
			score = float64(1+e.AccessCount) * math.Exp(-0.05*daysSince)
		}

		if score < bestScore {
			bestScore = score
			bestIdx = i
		}
	}

	if bestIdx < 0 {
		// Fallback: evict globally oldest
		s.evictGlobalOldest()
		return
	}

	id := s.entries[bestIdx].ID
	s.entries = append(s.entries[:bestIdx], s.entries[bestIdx+1:]...)
	s.evicted.Add(1)

	go func() {
		s.db.Exec(`DELETE FROM memory_entries WHERE id = ?`, id) //nolint:errcheck
	}()
}

// evictGlobalOldest removes the oldest entry across all agents.
// Fallback eviction strategy. Must be called under write lock.
func (s *Store) evictGlobalOldest() {
	if len(s.entries) == 0 {
		return
	}

	oldest := 0
	for i, e := range s.entries {
		if e.CreatedAt.Before(s.entries[oldest].CreatedAt) {
			oldest = i
		}
	}

	id := s.entries[oldest].ID
	s.entries = append(s.entries[:oldest], s.entries[oldest+1:]...)
	s.evicted.Add(1)

	go func() {
		s.db.Exec(`DELETE FROM memory_entries WHERE id = ?`, id) //nolint:errcheck
	}()
}

// countAgentEntriesLocked returns the number of entries for a specific agent.
// Must be called under read or write lock.
func (s *Store) countAgentEntriesLocked(agentID string) int {
	count := 0
	for _, e := range s.entries {
		if e.AgentID == agentID {
			count++
		}
	}
	return count
}

// countDistinctAgentsLocked returns the number of distinct agents in the store.
// Must be called under read or write lock.
func (s *Store) countDistinctAgentsLocked() int {
	agents := make(map[string]struct{})
	for _, e := range s.entries {
		agents[e.AgentID] = struct{}{}
	}
	return len(agents)
}

// ── Math + serialization (shared with vectorstore) ────────────────────────────

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		bits := math.Float32bits(f)
		buf[i*4+0] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}

func bytesToFloat32(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		bits := uint32(b[i*4]) |
			uint32(b[i*4+1])<<8 |
			uint32(b[i*4+2])<<16 |
			uint32(b[i*4+3])<<24
		v[i] = math.Float32frombits(bits)
	}
	return v
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
