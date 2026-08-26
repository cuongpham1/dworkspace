package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const embeddingVectorSerializationFormat = "float32-le-v1"

var (
	ErrInvalidEmbedderSignature   = errors.New("invalid embedder signature")
	ErrEmbeddingDimensionMismatch = errors.New("embedding dimension mismatch")
	ErrInvalidEmbeddingVector     = errors.New("invalid embedding vector")
	ErrCorruptEmbedding           = errors.New("corrupt cached embedding")
)

// Embedder is the model boundary. Implementations may be local or otherwise
// supplied by a later phase; callers must use Signature to keep vector spaces
// separate.
type Embedder interface {
	Signature() EmbedderSignature
	Embed(context.Context, string) ([]float32, error)
}

// EmbedderSignature identifies one stable vector space, including its
// dimension. Provider/model/revision values are encoded into String and are
// stored with every derived vector.
type EmbedderSignature struct {
	canonical  string
	dimensions int
}

func NewEmbedderSignature(provider, model, revision string, dimensions int) (EmbedderSignature, error) {
	parts := []string{strings.TrimSpace(provider), strings.TrimSpace(model), strings.TrimSpace(revision)}
	for _, part := range parts {
		if part == "" || strings.ContainsAny(part, "\x00\x1f") {
			return EmbedderSignature{}, fmt.Errorf("%w: provider, model, and revision must be non-empty and delimiter-free", ErrInvalidEmbedderSignature)
		}
	}
	if dimensions <= 0 {
		return EmbedderSignature{}, fmt.Errorf("%w: dimensions must be positive", ErrInvalidEmbedderSignature)
	}
	canonical := strings.Join(parts, "\x1f") + "\x1f" + strconv.Itoa(dimensions)
	return EmbedderSignature{canonical: canonical, dimensions: dimensions}, nil
}

func (s EmbedderSignature) String() string { return s.canonical }

func (s EmbedderSignature) Dimensions() int { return s.dimensions }

func (s EmbedderSignature) validate() error {
	if s.canonical == "" || s.dimensions <= 0 {
		return fmt.Errorf("%w: signature is not constructed by NewEmbedderSignature", ErrInvalidEmbedderSignature)
	}
	return nil
}

// EmbedderError keeps model failures explicit. The cache does not turn these
// into lexical-search failures; callers can choose the lexical fallback.
type EmbedderError struct {
	Signature EmbedderSignature
	Err       error
}

func (e *EmbedderError) Error() string {
	return fmt.Sprintf("embed with %s: %v", e.Signature, e.Err)
}

func (e *EmbedderError) Unwrap() error { return e.Err }

func serializeEmbeddingVector(vector []float32, dimensions int) ([]byte, error) {
	if err := validateEmbeddingVector(vector, dimensions); err != nil {
		return nil, err
	}
	encoded := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(encoded[i*4:], math.Float32bits(value))
	}
	return encoded, nil
}

func deserializeEmbeddingVector(encoded []byte, dimensions int) ([]float32, error) {
	if dimensions <= 0 || len(encoded)%4 != 0 || len(encoded)/4 != dimensions {
		return nil, fmt.Errorf("%w: expected %d float32 values, got %d bytes", ErrCorruptEmbedding, dimensions, len(encoded))
	}
	vector := make([]float32, dimensions)
	for i := range vector {
		value := math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: non-finite value at index %d", ErrCorruptEmbedding, i)
		}
		vector[i] = value
	}
	return vector, nil
}

func validateEmbeddingVector(vector []float32, dimensions int) error {
	if len(vector) != dimensions {
		return fmt.Errorf("%w: got %d values, want %d", ErrEmbeddingDimensionMismatch, len(vector), dimensions)
	}
	for i, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: non-finite value at index %d", ErrInvalidEmbeddingVector, i)
		}
	}
	return nil
}
