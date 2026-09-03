package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// ChunkEmbeddingCache is the lifecycle seam for derived chunk vectors. It
// intentionally has no query-ranking behavior: future incremental embedding
// can call GetOrEmbed after a chunk is materialized and invalidate rows when a
// chunk is replaced.
type ChunkEmbeddingCache interface {
	GetOrEmbed(context.Context, string, string, Embedder) ([]float32, bool, error)
	Lookup(string, string, EmbedderSignature) ([]float32, bool, error)
	Store(string, string, EmbedderSignature, []float32) error
	InvalidateChunk(string) error
	DropPage(string) error
	DropModel(EmbedderSignature) error
	DropOtherModels(EmbedderSignature) error
	CleanupStale() error
	Drop() error
}

type chunkEmbeddingCache struct {
	db *sql.DB
}

func newChunkEmbeddingCache(db *sql.DB) ChunkEmbeddingCache {
	return &chunkEmbeddingCache{db: db}
}

func (c *chunkEmbeddingCache) GetOrEmbed(ctx context.Context, chunkID, text string, embedder Embedder) ([]float32, bool, error) {
	signature := embedder.Signature()
	vector, hit, err := c.Lookup(chunkID, text, signature)
	if err != nil || hit {
		return vector, hit, err
	}
	vector, err = embedder.Embed(ctx, text)
	if err != nil {
		return nil, false, &EmbedderError{Signature: signature, Err: err}
	}
	if err := c.Store(chunkID, text, signature, vector); err != nil {
		return nil, false, fmt.Errorf("store embedding for chunk %s: %w", chunkID, err)
	}
	return vector, false, nil
}

func (c *chunkEmbeddingCache) Lookup(chunkID, text string, signature EmbedderSignature) ([]float32, bool, error) {
	if err := signature.validate(); err != nil {
		return nil, false, err
	}
	var contentHash, encoded []byte
	var dimension int
	var format string
	err := c.db.QueryRow(`SELECT content_hash, dimension, serialization_format, vector
		FROM chunk_embeddings WHERE chunk_id = ? AND model_signature = ?`, chunkID, signature.String()).Scan(
		&contentHash, &dimension, &format, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup embedding for chunk %s: %w", chunkID, err)
	}
	wantHash := sha256.Sum256([]byte(text))
	if !bytes.Equal(contentHash, wantHash[:]) {
		return nil, false, nil
	}
	if dimension != signature.Dimensions() {
		return nil, false, fmt.Errorf("%w: stored %d dimensions for %s, want %d", ErrCorruptEmbedding, dimension, signature, signature.Dimensions())
	}
	if format != embeddingVectorSerializationFormat {
		return nil, false, fmt.Errorf("%w: unsupported serialization format %q", ErrCorruptEmbedding, format)
	}
	vector, err := deserializeEmbeddingVector(encoded, dimension)
	if err != nil {
		return nil, false, err
	}
	return vector, true, nil
}

func (c *chunkEmbeddingCache) Store(chunkID, text string, signature EmbedderSignature, vector []float32) error {
	if err := signature.validate(); err != nil {
		return err
	}
	encoded, err := serializeEmbeddingVector(vector, signature.Dimensions())
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(text))
	_, err = c.db.Exec(`INSERT INTO chunk_embeddings
		(chunk_id, model_signature, content_hash, dimension, serialization_format, vector, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chunk_id, model_signature) DO UPDATE SET
		content_hash = excluded.content_hash,
		dimension = excluded.dimension,
		serialization_format = excluded.serialization_format,
		vector = excluded.vector,
		updated_at = excluded.updated_at`,
		chunkID, signature.String(), hash[:], signature.Dimensions(), embeddingVectorSerializationFormat, encoded, now())
	if err != nil {
		return fmt.Errorf("write chunk embedding: %w", err)
	}
	return nil
}

func (c *chunkEmbeddingCache) InvalidateChunk(chunkID string) error {
	if _, err := c.db.Exec(`DELETE FROM chunk_embeddings WHERE chunk_id = ?`, chunkID); err != nil {
		return fmt.Errorf("invalidate embeddings for chunk %s: %w", chunkID, err)
	}
	return nil
}

func (c *chunkEmbeddingCache) DropPage(pageID string) error {
	if _, err := c.db.Exec(`DELETE FROM chunk_embeddings WHERE chunk_id IN
		(SELECT id FROM page_chunks WHERE page_id = ?)`, pageID); err != nil {
		return fmt.Errorf("drop embeddings for page %s: %w", pageID, err)
	}
	return nil
}

func (c *chunkEmbeddingCache) DropModel(signature EmbedderSignature) error {
	if err := signature.validate(); err != nil {
		return err
	}
	if _, err := c.db.Exec(`DELETE FROM chunk_embeddings WHERE model_signature = ?`, signature.String()); err != nil {
		return fmt.Errorf("drop embeddings for model %s: %w", signature, err)
	}
	return nil
}

func (c *chunkEmbeddingCache) DropOtherModels(signature EmbedderSignature) error {
	if err := signature.validate(); err != nil {
		return err
	}
	if _, err := c.db.Exec(`DELETE FROM chunk_embeddings WHERE model_signature <> ?`, signature.String()); err != nil {
		return fmt.Errorf("drop obsolete embedding models: %w", err)
	}
	return nil
}

func (c *chunkEmbeddingCache) CleanupStale() error {
	rows, err := c.db.Query(`SELECT e.chunk_id, e.model_signature, e.content_hash, COALESCE(c.text, '')
		FROM chunk_embeddings e LEFT JOIN page_chunks c ON c.id = e.chunk_id`)
	if err != nil {
		return fmt.Errorf("scan stale embeddings: %w", err)
	}
	type staleRow struct{ chunkID, model string }
	var stale []staleRow
	for rows.Next() {
		var chunkID, model, text string
		var contentHash []byte
		if err := rows.Scan(&chunkID, &model, &contentHash, &text); err != nil {
			rows.Close()
			return fmt.Errorf("read embedding cache row: %w", err)
		}
		want := sha256.Sum256([]byte(text))
		if text == "" || !bytes.Equal(contentHash, want[:]) {
			stale = append(stale, staleRow{chunkID: chunkID, model: model})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("scan stale embeddings: %w", err)
	}
	rows.Close()
	for _, row := range stale {
		if _, err := c.db.Exec(`DELETE FROM chunk_embeddings WHERE chunk_id = ? AND model_signature = ?`, row.chunkID, row.model); err != nil {
			return fmt.Errorf("delete stale embedding for chunk %s: %w", row.chunkID, err)
		}
	}
	return nil
}

func (c *chunkEmbeddingCache) Drop() error {
	if _, err := c.db.Exec(`DELETE FROM chunk_embeddings`); err != nil {
		return fmt.Errorf("drop chunk embedding cache: %w", err)
	}
	return nil
}
