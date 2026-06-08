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
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
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
	Vector       []float32 `json:"-"`          // Dense embedding (bge-m3: 1024-d; not exposed in JSON)
	Sparse       embedding.SparseVector `json:"-"` // Optional lexical/sparse vector (bge-m3 hybrid); nil for dense-only providers
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

	// Hybrid + rerank (bge-m3 dense+sparse). sparseWeight blends the lexical score
	// into the similarity: score = (1-w)*denseCosine + w*sparseCosine. reranker, when
	// set, reorders the over-fetched candidates by a cross-encoder for higher precision
	// (best-effort: a warming/erroring reranker falls back to the hybrid order).
	sparseWeight float64
	reranker     embedding.Reranker

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

// WithSparseWeight sets the hybrid blend weight for the lexical (sparse) score
// (0 = dense-only; bge-m3 only). Clamped to [0,1].
func WithSparseWeight(w float64) StoreOption {
	return func(s *Store) {
		if w < 0 {
			w = 0
		}
		if w > 1 {
			w = 1
		}
		s.sparseWeight = w
	}
}

// WithReranker attaches a cross-encoder reranker used to reorder recalled
// candidates by relevance (best-effort).
func WithReranker(r embedding.Reranker) StoreOption {
	return func(s *Store) { s.reranker = r }
}

// SetSparseWeight updates the hybrid blend weight at runtime (clamped to [0,1]).
func (s *Store) SetSparseWeight(w float64) {
	if w < 0 {
		w = 0
	}
	if w > 1 {
		w = 1
	}
	s.sparseWeight = w
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

	// Add columns for the enhanced memory layer (idempotent). `sparse` holds the
	// bge-m3 lexical vector as a JSON object {token_id: weight}; NULL for dense-only
	// providers (backfilled on tables created before hybrid support).
	addCol := dl.AddColumn()
	for _, col := range []struct{ name, def string }{
		{"last_accessed", "INTEGER NOT NULL DEFAULT 0"},
		{"access_count", "INTEGER NOT NULL DEFAULT 0"},
		{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
		{"sparse", "TEXT"},
	} {
		s.db.Exec(`ALTER TABLE memory_entries ` + addCol + ` ` + col.name + ` ` + col.def) //nolint:errcheck
	}

	// Widen the unix-second timestamp columns to 64-bit on Postgres. The original
	// schema used INTEGER (int4) which overflows on the zero-time sentinel
	// (-62135596800, year 1) and on any post-2038 timestamp — this caused every
	// memory write to fail with "less than minimum value for int4". SQLite's INTEGER
	// is already 64-bit, so this is Postgres-only. Idempotent + best-effort.
	if s.pg {
		for _, col := range []string{"created_at", "last_accessed", "updated_at"} {
			s.db.Exec(`ALTER TABLE memory_entries ALTER COLUMN ` + col + ` TYPE BIGINT`) //nolint:errcheck
		}
	}

	// Index for TTL-based eviction queries
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_last_accessed ON memory_entries(last_accessed)`) //nolint:errcheck

	return nil
}

// warmLoad loads all memory entries from the database into memory.
func (s *Store) warmLoad() {
	rows, err := s.db.Query(`
		SELECT id, agent_id, session_id, fact, vector, provider, model, created_at,
		       COALESCE(last_accessed, 0), COALESCE(access_count, 0), COALESCE(updated_at, 0),
		       COALESCE(sparse, '')
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
		var sparseJSON string

		// Scan the vector column into the right target for the backend.
		vecTarget := any(&vecBytes)
		if s.pg {
			vecTarget = &vecPG
		}
		if err := rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Fact, vecTarget, &e.Provider, &e.Model,
			&createdAt, &lastAccessed, &accessCount, &updatedAt, &sparseJSON); err != nil {
			log.Printf("[MEMORY] scan error: %v", err)
			continue
		}

		if s.pg {
			e.Vector = vecPG.Slice()
		} else {
			e.Vector = bytesToFloat32(vecBytes)
		}
		e.Sparse = jsonToSparse(sparseJSON)
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

// scoredMatch is an internal scored candidate (entry + similarity + ranking score).
type scoredMatch struct {
	entry      *Entry
	sim        float64 // hybrid similarity ((1-w)*dense + w*sparse), or the rerank score
	finalScore float64 // recency-weighted score
}

// Search finds memories relevant to the query text, scoped by agent. The query is
// embedded (bge-m3 dense + sparse when available); candidates are scored by the hybrid
// similarity with recency weighting. When a reranker is attached, the top candidates
// are over-fetched and reordered by a cross-encoder for higher precision — best-effort,
// so a warming/erroring reranker falls back to the hybrid order.
func (s *Store) Search(queryText, agentID string, maxResults int) ([]SearchResult, error) {
	if !s.enabled || s.embedder == nil {
		return nil, nil
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	s.lookups.Add(1)

	dense, sparse, err := s.embedHybrid(queryText)
	if err != nil {
		return nil, err
	}

	// Over-fetch when a reranker is present so it has a richer pool to reorder.
	fetch := maxResults
	if s.reranker != nil {
		fetch = maxResults * 4
		if fetch < 10 {
			fetch = 10
		}
	}
	matches := s.scoreCandidates(dense, sparse, agentID, fetch)
	matches = s.rerankMatches(queryText, matches, maxResults)
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	s.touchMatches(matches)
	return matchesToResults(matches), nil
}

// SearchByVector finds memories similar to the given dense vector, scoped by agent
// (dense-only; no sparse blend, no rerank). Retained for callers holding a vector.
func (s *Store) SearchByVector(queryVec []float32, agentID string, maxResults int) []SearchResult {
	if maxResults <= 0 {
		maxResults = 5
	}
	matches := s.scoreCandidates(queryVec, nil, agentID, maxResults)
	s.touchMatches(matches)
	return matchesToResults(matches)
}

// scoreCandidates scores in-memory entries (scoped by agent) against the query vectors
// using the hybrid similarity ((1-w)*dense + w*sparse) plus recency weighting, sorts
// best-first, and caps to `limit`. Pure read — no side effects.
func (s *Store) scoreCandidates(denseVec []float32, sparseVec embedding.SparseVector, agentID string, limit int) []scoredMatch {
	w := s.sparseWeight
	useSparse := w > 0 && len(sparseVec) > 0

	s.mu.RLock()
	var matches []scoredMatch
	for _, e := range s.entries {
		if agentID != "" && e.AgentID != agentID {
			continue
		}
		sim := cosineSimilarity(denseVec, e.Vector)
		if useSparse && len(e.Sparse) > 0 {
			sim = (1-w)*sim + w*sparseCosine(sparseVec, e.Sparse)
		}
		if sim >= s.threshold {
			daysSince := time.Since(e.CreatedAt).Hours() / 24.0
			if daysSince < 0 {
				daysSince = 0
			}
			finalScore := sim * math.Exp(-s.recencyLambda*daysSince)
			matches = append(matches, scoredMatch{entry: e, sim: sim, finalScore: finalScore})
		}
	}
	s.mu.RUnlock()

	// Sort by recency-weighted score descending (insertion sort — small N).
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].finalScore > matches[j-1].finalScore; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// rerankMatches reorders candidates by a cross-encoder relevance score for the query.
// Best-effort: on a warming/erroring reranker (or <2 candidates) it returns the input
// order unchanged. The reranker's score replaces sim on the kept entries.
func (s *Store) rerankMatches(queryText string, matches []scoredMatch, topN int) []scoredMatch {
	if s.reranker == nil || len(matches) < 2 {
		return matches
	}
	items := make([]embedding.RerankItem, len(matches))
	for i, m := range matches {
		items[i] = embedding.RerankItem{ID: strconv.Itoa(i), Text: m.entry.Fact}
	}
	ranked, err := s.reranker.Rerank(queryText, items, topN)
	if err != nil || len(ranked) == 0 {
		if err != nil {
			log.Printf("[MEMORY] rerank skipped (%v) — using hybrid order", err)
		}
		return matches
	}
	out := make([]scoredMatch, 0, len(ranked))
	for _, r := range ranked {
		idx := r.Index
		if idx < 0 || idx >= len(matches) {
			if n, perr := strconv.Atoi(r.ID); perr == nil && n >= 0 && n < len(matches) {
				idx = n
			} else {
				continue
			}
		}
		m := matches[idx]
		m.sim = r.Score // cross-encoder relevance
		out = append(out, m)
	}
	if len(out) == 0 {
		return matches
	}
	return out
}

// touchMatches updates access metadata (LastAccessed, AccessCount) for matched entries
// — in-memory immediately, persisted asynchronously.
func (s *Store) touchMatches(matches []scoredMatch) {
	if len(matches) == 0 {
		return
	}
	s.hits.Add(1)
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
	go func() {
		for _, id := range matchedIDs {
			s.db.Exec(`UPDATE memory_entries SET last_accessed = ?, access_count = access_count + 1 WHERE id = ?`,
				now.Unix(), id) //nolint:errcheck
		}
	}()
}

func matchesToResults(matches []scoredMatch) []SearchResult {
	results := make([]SearchResult, len(matches))
	for i, m := range matches {
		results[i] = SearchResult{Entry: m.entry, Similarity: m.sim, Score: m.finalScore}
	}
	return results
}

// embedHybrid embeds text into a dense vector plus an optional sparse vector — the
// HybridEmbedder path (bge-m3 dense+sparse) when available, else dense-only.
func (s *Store) embedHybrid(text string) ([]float32, embedding.SparseVector, error) {
	if he, ok := s.embedder.(embedding.HybridEmbedder); ok {
		return he.EmbedHybrid(text)
	}
	dense, err := s.embedder.Embed(text)
	return dense, nil, err
}

// StoreFact embeds (dense + sparse when available) and persists a single memory fact.
func (s *Store) StoreFact(agentID, sessionID, fact, provider, model string) error {
	if !s.enabled || s.embedder == nil {
		return nil
	}

	dense, sparse, err := s.embedHybrid(fact)
	if err != nil {
		return err
	}

	return s.StoreFactWithVectors(agentID, sessionID, fact, dense, sparse, provider, model)
}

// StoreFactWithVector stores a fact with a pre-computed dense vector (no sparse).
// Kept for backward compatibility; delegates to the hybrid StoreFactWithVectors.
func (s *Store) StoreFactWithVector(agentID, sessionID, fact string, vector []float32, provider, model string) error {
	return s.StoreFactWithVectors(agentID, sessionID, fact, vector, nil, provider, model)
}

// StoreFactWithVectors stores a fact with pre-computed dense + optional sparse vectors.
// Implements conflict resolution: if a similar fact exists (0.85–0.95 similarity),
// it may be updated rather than duplicated (AUDN pattern from mem0). Conflict similarity
// uses the same hybrid blend as retrieval.
func (s *Store) StoreFactWithVectors(agentID, sessionID, fact string, vector []float32, sparse embedding.SparseVector, provider, model string) error {
	w := s.sparseWeight
	useSparse := w > 0 && len(sparse) > 0
	// Check for duplicate / conflicting facts (same agent + similar vector)
	s.mu.RLock()
	for _, e := range s.entries {
		if e.AgentID == agentID {
			sim := cosineSimilarity(vector, e.Vector)
			if useSparse && len(e.Sparse) > 0 {
				sim = (1-w)*sim + w*sparseCosine(sparse, e.Sparse)
			}
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
					return s.updateExistingEntry(e, fact, vector, sparse)
				}
				// Similar but not clearly a replacement — skip (conservative)
				s.mu.RUnlock()
				log.Printf("[MEMORY] similar fact exists (sim=%.4f), keeping existing: %s", sim, truncate(e.Fact, 60))
				return nil
			}
		}
	}
	s.mu.RUnlock()

	now := time.Now()
	entry := &Entry{
		AgentID:   agentID,
		SessionID: sessionID,
		Fact:      fact,
		Vector:    vector,
		Sparse:    sparse,
		Provider:  provider,
		Model:     model,
		CreatedAt: now,
		// Seed access timestamps to creation time. Leaving them as the zero time
		// makes .Unix() = -62135596800, which overflows Postgres int4 (the bug that
		// failed every memory write before the BIGINT migration + this seed).
		LastAccessed: now,
		UpdatedAt:    now,
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

// updateExistingEntry replaces the fact text + vectors of an existing memory entry.
// Used during conflict resolution when a new fact supersedes an old one.
func (s *Store) updateExistingEntry(existing *Entry, newFact string, newVector []float32, newSparse embedding.SparseVector) error {
	now := time.Now()

	// Update in-memory
	s.mu.Lock()
	existing.Fact = newFact
	existing.Vector = newVector
	existing.Sparse = newSparse
	existing.UpdatedAt = now
	s.mu.Unlock()

	// Persist to DB
	_, err := s.db.Exec(`UPDATE memory_entries SET fact = ?, vector = ?, sparse = ?, updated_at = ? WHERE id = ?`,
		newFact, s.encVec(newVector), sparseToJSON(newSparse), now.Unix(), existing.ID)
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
		INSERT INTO memory_entries (agent_id, session_id, fact, vector, sparse, provider, model, created_at, last_accessed, access_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.AgentID, entry.SessionID, entry.Fact, s.encVec(entry.Vector), sparseToJSON(entry.Sparse), entry.Provider, entry.Model,
		entry.CreatedAt.Unix(), entry.LastAccessed.Unix(), entry.AccessCount, entry.UpdatedAt.Unix(),
	)
}

// ── Sparse (de)serialization + cosine (bge-m3 lexical vector) ────────────────
// Stored as a JSON object of token-id -> weight, NULL for dense-only entries.

func sparseToJSON(sp embedding.SparseVector) any {
	if len(sp) == 0 {
		return nil
	}
	m := make(map[string]float32, len(sp))
	for k, v := range sp {
		m[strconv.FormatInt(int64(k), 10)] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

func jsonToSparse(j string) embedding.SparseVector {
	if j == "" {
		return nil
	}
	var m map[string]float32
	if err := json.Unmarshal([]byte(j), &m); err != nil || len(m) == 0 {
		return nil
	}
	out := make(embedding.SparseVector, len(m))
	for k, v := range m {
		id, err := strconv.ParseInt(k, 10, 32)
		if err != nil {
			continue
		}
		out[int32(id)] = v
	}
	return out
}

// sparseCosine computes the cosine similarity between two sparse vectors (token-id ->
// weight). Iterates the smaller map for the dot product.
func sparseCosine(a, b embedding.SparseVector) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	var dot, na, nb float64
	for _, v := range a {
		na += float64(v) * float64(v)
	}
	for k, v := range b {
		nb += float64(v) * float64(v)
		if av, ok := a[k]; ok {
			dot += float64(av) * float64(v)
		}
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
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
