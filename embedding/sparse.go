package embedding

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/pgvector/pgvector-go"
)

// SparseDim is the sparsevec dimensionality for bge-m3 (BAAI/bge-m3 uses the
// XLM-RoBERTa vocabulary of 250002 tokens). Sparse keys are token ids in
// [0, SparseDim-1]. pgvector's sparsevec is 1-indexed; pgvector-go's String/Parse
// handle the offset (key+1 on write, -1 on read), so we keep the token id as the
// 0-based map key everywhere in Go.
const SparseDim int32 = 250002

// SparseColumnType returns the SQL column type for the sparse lexical vector per
// backend: a native pgvector `sparsevec(N)` on Postgres, a JSON-in-TEXT column on
// SQLite (which has no vector types — dev/test only).
func SparseColumnType(pg bool) string {
	if pg {
		return fmt.Sprintf("sparsevec(%d)", SparseDim)
	}
	return "TEXT"
}

// SparseSelectExpr returns the SELECT expression that yields a text rendering of the
// sparse column. Postgres casts the native sparsevec to its text form
// (`{i:v,...}/dim`); SQLite stores text already.
func SparseSelectExpr(pg bool) string {
	if pg {
		return "sparse::text"
	}
	return "sparse"
}

// EncodeSparse prepares a sparse vector as an INSERT/UPDATE bind arg per backend.
// Returns nil (-> SQL NULL) when empty. Postgres gets a native pgvector.SparseVector
// (driver.Valuer); SQLite gets a compact JSON object string. Token ids outside
// [0, SparseDim) are dropped defensively so they can't violate the sparsevec bound.
func EncodeSparse(sp SparseVector, pg bool) any {
	if len(sp) == 0 {
		return nil
	}
	if pg {
		clean := make(map[int32]float32, len(sp))
		for k, v := range sp {
			if v != 0 && k >= 0 && k < SparseDim {
				clean[k] = v
			}
		}
		if len(clean) == 0 {
			return nil
		}
		return pgvector.NewSparseVectorFromMap(clean, SparseDim)
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

// DecodeSparse parses a text-rendered sparse column value back into a SparseVector,
// handling both the pgvector sparsevec text form (`{i:v,...}/dim`) and the SQLite
// JSON form. Returns nil for empty / unparseable input.
func DecodeSparse(text string, pg bool) SparseVector {
	if text == "" {
		return nil
	}
	if pg {
		var sv pgvector.SparseVector
		if err := sv.Parse(text); err != nil {
			return nil
		}
		idx := sv.Indices()
		val := sv.Values()
		out := make(SparseVector, len(idx))
		for i := range idx {
			out[idx[i]] = val[i] // Parse already restored 0-based token ids
		}
		return out
	}
	var m map[string]float32
	if err := json.Unmarshal([]byte(text), &m); err != nil || len(m) == 0 {
		return nil
	}
	out := make(SparseVector, len(m))
	for k, v := range m {
		id, err := strconv.ParseInt(k, 10, 32)
		if err != nil {
			continue
		}
		out[int32(id)] = v
	}
	return out
}
