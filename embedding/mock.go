package embedding

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// MockEmbedder generates deterministic embeddings for testing.
// It hashes the input text to produce a reproducible vector, so identical
// texts produce identical embeddings and similar texts produce somewhat
// similar embeddings (not guaranteed, but good enough for testing cache logic).
type MockEmbedder struct {
	dims int
}

// NewMockEmbedder creates a mock embedder with the given dimensionality.
func NewMockEmbedder(dims int) *MockEmbedder {
	return &MockEmbedder{dims: dims}
}

// Embed generates a deterministic pseudo-embedding from the text hash.
func (m *MockEmbedder) Embed(text string) ([]float32, error) {
	h := sha256.Sum256([]byte(text))
	vec := make([]float32, m.dims)

	// Generate a full vector by repeating and varying the hash bytes
	for i := 0; i < m.dims; i++ {
		// Mix hash bytes deterministically
		idx := i % 32
		seed := float64(h[idx]) + float64(i)*0.1
		vec[i] = float32(math.Sin(seed) * 0.5)
	}

	// Normalize to unit vector (cosine similarity works on unit vectors)
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}

	return vec, nil
}

// Dimensions returns the configured vector dimensionality.
func (m *MockEmbedder) Dimensions() int {
	return m.dims
}

// FixedEmbedder always returns the same pre-set embedding.
// Useful for testing exact similarity scenarios.
type FixedEmbedder struct {
	Vector []float32
}

// NewFixedEmbedder creates an embedder that always returns the given vector.
func NewFixedEmbedder(vec []float32) *FixedEmbedder {
	return &FixedEmbedder{Vector: vec}
}

// Embed always returns the pre-configured vector.
func (f *FixedEmbedder) Embed(_ string) ([]float32, error) {
	return f.Vector, nil
}

// Dimensions returns the length of the pre-configured vector.
func (f *FixedEmbedder) Dimensions() int {
	return len(f.Vector)
}

// hashToFloat32 converts 4 bytes of hash into a float32 in [-1, 1].
func hashToFloat32(h [32]byte, offset int) float32 {
	b := h[offset : offset+4]
	bits := binary.LittleEndian.Uint32(b)
	// Map to [-1, 1]
	return float32(bits)/float32(math.MaxUint32)*2 - 1
}
