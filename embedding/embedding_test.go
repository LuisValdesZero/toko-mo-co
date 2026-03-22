package embedding

import (
	"math"
	"testing"
)

func TestMockEmbedder_Deterministic(t *testing.T) {
	emb := NewMockEmbedder(64)

	v1, err := emb.Embed("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v2, err := emb.Embed("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(v1) != 64 {
		t.Errorf("expected 64 dimensions, got %d", len(v1))
	}

	// Same input must produce identical output
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("non-deterministic: v1[%d]=%f v2[%d]=%f", i, v1[i], i, v2[i])
		}
	}
}

func TestMockEmbedder_DifferentInputs(t *testing.T) {
	emb := NewMockEmbedder(64)

	v1, _ := emb.Embed("hello world")
	v2, _ := emb.Embed("goodbye world")

	// Different inputs should produce different vectors
	same := true
	for i := range v1 {
		if v1[i] != v2[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different inputs should produce different embeddings")
	}
}

func TestMockEmbedder_UnitVector(t *testing.T) {
	emb := NewMockEmbedder(128)
	v, _ := emb.Embed("test input")

	// Verify the vector is normalized (magnitude ≈ 1.0)
	var norm float64
	for _, val := range v {
		norm += float64(val) * float64(val)
	}
	norm = math.Sqrt(norm)

	if math.Abs(norm-1.0) > 0.001 {
		t.Errorf("expected unit vector (norm ≈ 1.0), got %f", norm)
	}
}

func TestMockEmbedder_Dimensions(t *testing.T) {
	dims := []int{64, 128, 256, 1536}
	for _, d := range dims {
		emb := NewMockEmbedder(d)
		if emb.Dimensions() != d {
			t.Errorf("expected Dimensions()=%d, got %d", d, emb.Dimensions())
		}
		v, _ := emb.Embed("test")
		if len(v) != d {
			t.Errorf("expected vector length %d, got %d", d, len(v))
		}
	}
}

func TestFixedEmbedder(t *testing.T) {
	vec := []float32{0.1, 0.2, 0.3}
	emb := NewFixedEmbedder(vec)

	if emb.Dimensions() != 3 {
		t.Errorf("expected 3 dimensions, got %d", emb.Dimensions())
	}

	v1, _ := emb.Embed("anything")
	v2, _ := emb.Embed("something else")

	for i := range v1 {
		if v1[i] != v2[i] || v1[i] != vec[i] {
			t.Errorf("FixedEmbedder should always return same vector")
		}
	}
}

func TestOpenAIEmbedder_NoKey(t *testing.T) {
	// Ensure it fails gracefully when no API key is provided
	t.Setenv("OPENAI_API_KEY", "")
	_, err := NewOpenAIEmbedder()
	if err == nil {
		t.Error("expected error when no API key is provided")
	}
}
