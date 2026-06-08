// Package vectorstore provides a vector similarity store for semantic caching.
//
// Two backends, chosen by the active SQL dialect:
//   - SQLite: vectors are float32 BLOBs; the whole set is warm-loaded into an
//     in-memory map and searched with brute-force cosine similarity in Go.
//   - Postgres: vectors live in a native pgvector column with an HNSW index;
//     search runs in SQL via the cosine-distance operator (<=>). No in-memory
//     mirror is kept.
//
// Hybrid scoring: when an entry carries a sparse (lexical) vector — e.g. bge-m3's
// sparse output — Store/Search variants combine dense cosine with sparse cosine
// as `(1-w)*dense + w*sparse`. The dense-only Store/Search wrappers keep working
// unchanged (sparse nil, weight 0) for providers like OpenAI.
package vectorstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"

	"tokomoco/store"
)

// Entry is a stored vector with its associated cache hash.
type Entry struct {
	CacheHash string            // Links back to the response_cache entry
	Vector    []float32         // Dense embedding vector
	Sparse    map[int32]float32 // Optional lexical vector (bge-m3 hybrid); nil for dense-only providers
	Provider  string            // For scoping searches
	Model     string            // For scoping searches
	CreatedAt time.Time
}

// SearchResult is a match from a similarity search.
type SearchResult struct {
	CacheHash  string
	Similarity float64
}

// VectorStore provides cosine similarity search over stored embeddings.
type VectorStore struct {
	mu         sync.RWMutex
	entries    map[string]*Entry // cacheHash → entry (SQLite only)
	dims       int
	maxEntries int
	db         store.Querier
	pg         bool // true when the backend is Postgres (pgvector SQL search)
}

// New creates a vector store backed by the given database handle.
// dims is the expected embedding dimensionality (e.g. 1536 for OpenAI, 1024 for bge-m3).
func New(db store.Querier, dims, maxEntries int) (*VectorStore, error) {
	vs := &VectorStore{
		entries:    make(map[string]*Entry),
		dims:       dims,
		maxEntries: maxEntries,
		db:         db,
		pg:         db.Dialect() == store.Postgres,
	}

	if err := vs.migrate(); err != nil {
		return nil, err
	}
	vs.ensureDims() // recreate the table if the stored dimensionality changed (provider switch)

	if !vs.pg {
		vs.warmLoad() // SQLite mirrors vectors in RAM; Postgres queries pgvector directly
	}
	return vs, nil
}

// ensureDims reconciles the Postgres pgvector column dimensionality with the
// active embedder. CREATE TABLE IF NOT EXISTS won't change an existing column's
// dimension, so when the embedding provider changes (e.g. OpenAI 1536 -> bge-m3
// 1024) the stored vectors and column would mismatch. This is a cache, so the
// table is simply dropped and recreated at the new dimension. SQLite stores
// vectors as BLOBs (any length) and warmLoad already skips mismatched rows, so
// only Postgres needs this.
func (vs *VectorStore) ensureDims() {
	if !vs.pg {
		return
	}
	var got int
	if err := vs.db.QueryRow(`SELECT vector_dims(vector) FROM semantic_cache_vectors LIMIT 1`).Scan(&got); err != nil {
		return // empty table or unavailable -> nothing to reconcile
	}
	if got != vs.dims {
		log.Printf("[VECTORSTORE] embedding dimension changed %d -> %d; recreating semantic_cache_vectors", got, vs.dims)
		vs.db.Exec(`DROP TABLE IF EXISTS semantic_cache_vectors`) //nolint:errcheck
		if err := vs.migrate(); err != nil {
			log.Printf("[VECTORSTORE] recreate after dim change failed: %v", err)
		}
	}
}

// migrate creates the semantic_cache_vectors table if it doesn't exist and ensures
// the optional `sparse` column is present (added idempotently for existing tables).
func (vs *VectorStore) migrate() error {
	var stmts []string
	if vs.pg {
		stmts = []string{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS semantic_cache_vectors (
				cache_hash TEXT PRIMARY KEY,
				vector     vector(%d) NOT NULL,
				sparse     TEXT,
				provider   TEXT NOT NULL DEFAULT '',
				model      TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`, vs.dims),
			`CREATE INDEX IF NOT EXISTS idx_scv_provider_model ON semantic_cache_vectors(provider, model)`,
			// Approximate-nearest-neighbour index for cosine distance.
			`CREATE INDEX IF NOT EXISTS idx_scv_vector ON semantic_cache_vectors USING hnsw (vector vector_cosine_ops)`,
		}
	} else {
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS semantic_cache_vectors (
				cache_hash TEXT PRIMARY KEY,
				vector     BLOB NOT NULL,
				sparse     TEXT,
				provider   TEXT NOT NULL DEFAULT '',
				model      TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_scv_provider_model ON semantic_cache_vectors(provider, model)`,
		}
	}
	for _, s := range stmts {
		if _, err := vs.db.Exec(s); err != nil {
			return err
		}
	}
	// Backfill the sparse column on tables created before hybrid support. Both
	// dialects error if it already exists; that error is benign and ignored.
	vs.db.Exec(`ALTER TABLE semantic_cache_vectors ADD COLUMN sparse TEXT`) //nolint:errcheck
	return nil
}

// warmLoad loads all vectors from the database into memory (SQLite only).
func (vs *VectorStore) warmLoad() {
	rows, err := vs.db.Query(`
		SELECT cache_hash, vector, sparse, provider, model, created_at
		FROM semantic_cache_vectors
		ORDER BY created_at DESC
		LIMIT ?`, vs.maxEntries)
	if err != nil {
		log.Printf("[VECTORSTORE] warm-load failed: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var hash, provider, model string
		var vecBytes []byte
		var sparseJSON sql.NullString
		var createdAt int64

		if err := rows.Scan(&hash, &vecBytes, &sparseJSON, &provider, &model, &createdAt); err != nil {
			log.Printf("[VECTORSTORE] scan error: %v", err)
			continue
		}

		vec := bytesToFloat32(vecBytes)
		if len(vec) != vs.dims {
			continue // dimension mismatch, skip stale entry
		}

		vs.entries[hash] = &Entry{
			CacheHash: hash,
			Vector:    vec,
			Sparse:    jsonToSparse(sparseJSON.String),
			Provider:  provider,
			Model:     model,
			CreatedAt: time.Unix(createdAt, 0),
		}
		count++
	}

	if count > 0 {
		log.Printf("[VECTORSTORE] warm-loaded %d vectors", count)
	}
}

// Store adds or updates a dense vector for a cache hash (dense-only providers).
func (vs *VectorStore) Store(cacheHash string, vector []float32, provider, model string) {
	vs.StoreHybrid(cacheHash, vector, nil, provider, model)
}

// StoreHybrid adds or updates a dense vector plus an optional sparse lexical
// vector for a cache hash.
func (vs *VectorStore) StoreHybrid(cacheHash string, vector []float32, sparse map[int32]float32, provider, model string) {
	if vs.pg {
		go vs.persistPG(cacheHash, vector, sparse, provider, model, time.Now())
		return
	}

	vs.mu.Lock()
	// Evict oldest if at capacity
	if len(vs.entries) >= vs.maxEntries {
		if _, exists := vs.entries[cacheHash]; !exists {
			vs.evictOldest()
		}
	}
	entry := &Entry{
		CacheHash: cacheHash,
		Vector:    vector,
		Sparse:    sparse,
		Provider:  provider,
		Model:     model,
		CreatedAt: time.Now(),
	}
	vs.entries[cacheHash] = entry
	vs.mu.Unlock()

	go vs.persistToDB(entry) // Async persist to SQLite
}

// Search finds the most similar dense vector to the query (dense-only).
func (vs *VectorStore) Search(queryVec []float32, provider, model string, threshold float64) *SearchResult {
	return vs.SearchHybrid(queryVec, nil, provider, model, threshold, 0)
}

// SearchHybrid finds the best match scoping by provider+model, scoring with a
// blend of dense cosine and sparse cosine: (1-w)*dense + w*sparse. When
// sparseWeight is 0 (or the query/entry has no sparse vector) it degrades to a
// pure dense search, identical to the old Search behaviour.
func (vs *VectorStore) SearchHybrid(queryVec []float32, querySparse map[int32]float32, provider, model string, threshold, sparseWeight float64) *SearchResult {
	if sparseWeight < 0 {
		sparseWeight = 0
	}
	if sparseWeight > 1 {
		sparseWeight = 1
	}
	useSparse := sparseWeight > 0 && len(querySparse) > 0

	if vs.pg {
		return vs.searchPG(queryVec, querySparse, provider, model, threshold, sparseWeight, useSparse)
	}

	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var bestHash string
	var bestSim float64

	for hash, entry := range vs.entries {
		if entry.Provider != provider || entry.Model != model {
			continue
		}
		sim := cosineSimilarity(queryVec, entry.Vector)
		if useSparse && len(entry.Sparse) > 0 {
			sim = (1-sparseWeight)*sim + sparseWeight*sparseCosine(querySparse, entry.Sparse)
		}
		if sim > bestSim {
			bestSim = sim
			bestHash = hash
		}
	}

	if bestSim >= threshold {
		return &SearchResult{CacheHash: bestHash, Similarity: bestSim}
	}
	return nil
}

// searchPG runs the nearest-neighbour search in Postgres via pgvector. The
// cosine-distance operator (<=>) returns 1 - cosine_similarity. For hybrid
// scoring we pull the top-K dense candidates and re-rank them in Go with the
// sparse vector; for pure dense we keep the single-row fast path.
func (vs *VectorStore) searchPG(queryVec []float32, querySparse map[int32]float32, provider, model string, threshold, sparseWeight float64, useSparse bool) *SearchResult {
	q := pgvector.NewVector(queryVec)

	if !useSparse {
		var hash string
		var sim float64
		err := vs.db.QueryRow(`
			SELECT cache_hash, 1 - (vector <=> ?) AS sim
			FROM semantic_cache_vectors
			WHERE provider = ? AND model = ?
			ORDER BY vector <=> ?
			LIMIT 1`, q, provider, model, q).Scan(&hash, &sim)
		if err != nil {
			if err != sql.ErrNoRows {
				log.Printf("[VECTORSTORE] pg search failed: %v", err)
			}
			return nil
		}
		if sim >= threshold {
			return &SearchResult{CacheHash: hash, Similarity: sim}
		}
		return nil
	}

	// Hybrid: fetch top-K dense neighbours, re-rank with sparse cosine.
	const topK = 20
	rows, err := vs.db.Query(`
		SELECT cache_hash, 1 - (vector <=> ?) AS sim, sparse
		FROM semantic_cache_vectors
		WHERE provider = ? AND model = ?
		ORDER BY vector <=> ?
		LIMIT ?`, q, provider, model, q, topK)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[VECTORSTORE] pg hybrid search failed: %v", err)
		}
		return nil
	}
	defer rows.Close()

	var bestHash string
	var bestSim float64
	for rows.Next() {
		var hash string
		var denseSim float64
		var sparseJSON sql.NullString
		if err := rows.Scan(&hash, &denseSim, &sparseJSON); err != nil {
			continue
		}
		score := denseSim
		if es := jsonToSparse(sparseJSON.String); len(es) > 0 {
			score = (1-sparseWeight)*denseSim + sparseWeight*sparseCosine(querySparse, es)
		}
		if score > bestSim {
			bestSim = score
			bestHash = hash
		}
	}
	if bestSim >= threshold {
		return &SearchResult{CacheHash: bestHash, Similarity: bestSim}
	}
	return nil
}

// Delete removes a vector by its cache hash.
func (vs *VectorStore) Delete(cacheHash string) {
	if !vs.pg {
		vs.mu.Lock()
		delete(vs.entries, cacheHash)
		vs.mu.Unlock()
	}
	go func() {
		vs.db.Exec(`DELETE FROM semantic_cache_vectors WHERE cache_hash = ?`, cacheHash) //nolint:errcheck
	}()
}

// Flush removes all vectors from memory and DB.
func (vs *VectorStore) Flush() {
	if !vs.pg {
		vs.mu.Lock()
		vs.entries = make(map[string]*Entry)
		vs.mu.Unlock()
	}
	vs.db.Exec(`DELETE FROM semantic_cache_vectors`) //nolint:errcheck
}

// Count returns the number of stored vectors.
func (vs *VectorStore) Count() int {
	if vs.pg {
		var n int
		vs.db.QueryRow(`SELECT COUNT(*) FROM semantic_cache_vectors`).Scan(&n) //nolint:errcheck
		return n
	}
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return len(vs.entries)
}

// evictOldest removes the oldest entry. Must be called under write lock (SQLite).
func (vs *VectorStore) evictOldest() {
	var oldestHash string
	var oldestTime time.Time

	for hash, entry := range vs.entries {
		if oldestHash == "" || entry.CreatedAt.Before(oldestTime) {
			oldestHash = hash
			oldestTime = entry.CreatedAt
		}
	}

	if oldestHash != "" {
		delete(vs.entries, oldestHash)
		go func() {
			vs.db.Exec(`DELETE FROM semantic_cache_vectors WHERE cache_hash = ?`, oldestHash) //nolint:errcheck
		}()
	}
}

// persistToDB writes a vector entry to SQLite (BLOB-encoded dense + JSON sparse).
func (vs *VectorStore) persistToDB(entry *Entry) {
	vecBytes := float32ToBytes(entry.Vector)
	_, err := vs.db.Exec(`
		INSERT INTO semantic_cache_vectors (cache_hash, vector, sparse, provider, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_hash) DO UPDATE SET
			vector = excluded.vector,
			sparse = excluded.sparse,
			provider = excluded.provider,
			model = excluded.model`,
		entry.CacheHash, vecBytes, sparseToJSON(entry.Sparse), entry.Provider, entry.Model, entry.CreatedAt.Unix(),
	)
	if err != nil {
		log.Printf("[VECTORSTORE] persist failed: %v", err)
	}
}

// persistPG writes a vector entry to Postgres (native pgvector dense + JSON sparse).
func (vs *VectorStore) persistPG(cacheHash string, vector []float32, sparse map[int32]float32, provider, model string, created time.Time) {
	_, err := vs.db.Exec(`
		INSERT INTO semantic_cache_vectors (cache_hash, vector, sparse, provider, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_hash) DO UPDATE SET
			vector = excluded.vector,
			sparse = excluded.sparse,
			provider = excluded.provider,
			model = excluded.model`,
		cacheHash, pgvector.NewVector(vector), sparseToJSON(sparse), provider, model, created.Unix(),
	)
	if err != nil {
		log.Printf("[VECTORSTORE] pg persist failed: %v", err)
	}
}

// ── Math utilities ─────────────────────────────────────────────────────────

// cosineSimilarity computes the cosine similarity between two dense vectors.
// Returns a value in [-1, 1] where 1 means identical direction.
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

// sparseCosine computes cosine similarity between two sparse vectors
// (token-id -> weight). Returns a value in [0, 1] for non-negative weights.
func sparseCosine(a, b map[int32]float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Iterate the smaller map for the shared-key dot product.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	var dot float64
	for k, av := range small {
		if bv, ok := large[k]; ok {
			dot += float64(av) * float64(bv)
		}
	}
	if dot == 0 {
		return 0
	}
	var normA, normB float64
	for _, v := range a {
		normA += float64(v) * float64(v)
	}
	for _, v := range b {
		normB += float64(v) * float64(v)
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// ── Sparse (de)serialization (stored as a JSON object of token-id -> weight) ──

func sparseToJSON(s map[int32]float32) interface{} {
	if len(s) == 0 {
		return nil // store NULL for dense-only entries
	}
	m := make(map[string]float32, len(s))
	for k, v := range s {
		m[strconv.FormatInt(int64(k), 10)] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

func jsonToSparse(j string) map[int32]float32 {
	if j == "" {
		return nil
	}
	var m map[string]float32
	if err := json.Unmarshal([]byte(j), &m); err != nil || len(m) == 0 {
		return nil
	}
	out := make(map[int32]float32, len(m))
	for k, v := range m {
		id, err := strconv.ParseInt(k, 10, 32)
		if err != nil {
			continue
		}
		out[int32(id)] = v
	}
	return out
}

// ── Serialization helpers (SQLite BLOB) ─────────────────────────────────────

// float32ToBytes converts a float32 slice to a byte slice for SQLite BLOB storage.
// Uses IEEE 754 little-endian encoding (4 bytes per float).
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

// bytesToFloat32 converts a byte slice back to a float32 slice.
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
