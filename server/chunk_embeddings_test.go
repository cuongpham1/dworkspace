package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

type fakeEmbedder struct {
	signature EmbedderSignature
	vector    []float32
	err       error
	calls     int
}

func (f *fakeEmbedder) Signature() EmbedderSignature {
	return f.signature
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]float32(nil), f.vector...), nil
}

func newEmbeddingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "embedding-test.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return db
}

func seedEmbeddingChunk(t *testing.T, db *sql.DB, pageID, chunkID, text string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO pages (id, content, created_at, updated_at) VALUES (?, '[]', ?, ?)`, pageID, now(), now()); err != nil {
		t.Fatalf("insert embedding test page: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO page_chunks
		(id, page_id, workspace_id, ord, kind, heading, text)
		VALUES (?, ?, 'workspace', 0, 'body', '', ?)`, chunkID, pageID, text); err != nil {
		t.Fatalf("insert embedding test chunk: %v", err)
	}
}

func testEmbedderSignature(t *testing.T, model string, dimensions int) EmbedderSignature {
	t.Helper()
	signature, err := NewEmbedderSignature("test", model, "v1", dimensions)
	if err != nil {
		t.Fatalf("create embedder signature: %v", err)
	}
	return signature
}

func TestChunkEmbeddingCache_reusesSameChunkTextAndModel(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "durable text")
	cache := newChunkEmbeddingCache(db)
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 2),
		vector:    []float32{0.25, -0.5},
	}

	// When
	first, firstHit, err := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)
	if err != nil {
		t.Fatalf("first embedding: %v", err)
	}
	second, secondHit, err := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)

	// Then
	if err != nil {
		t.Fatalf("cached embedding: %v", err)
	}
	if firstHit || !secondHit || embedder.calls != 1 {
		t.Fatalf("cache hits = (%v, %v), embedder calls = %d", firstHit, secondHit, embedder.calls)
	}
	if len(first) != 2 || first[0] != second[0] || first[1] != second[1] {
		t.Fatalf("cached vector = %v, first vector = %v", second, first)
	}
}

func TestChunkEmbeddingCache_missesChangedTextAndModel(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "original text")
	cache := newChunkEmbeddingCache(db)
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 2),
		vector:    []float32{1, 2},
	}
	_, _, err := cache.GetOrEmbed(context.Background(), "chunk-1", "original text", embedder)
	if err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	// When
	_, changedTextHit, err := cache.GetOrEmbed(context.Background(), "chunk-1", "changed text", embedder)
	if err != nil {
		t.Fatalf("changed text embedding: %v", err)
	}
	otherModel := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-b", 2),
		vector:    []float32{3, 4},
	}
	_, changedModelHit, err := cache.GetOrEmbed(context.Background(), "chunk-1", "changed text", otherModel)

	// Then
	if err != nil {
		t.Fatalf("changed model embedding: %v", err)
	}
	if changedTextHit || changedModelHit || embedder.calls != 2 || otherModel.calls != 1 {
		t.Fatalf("hits = (%v, %v), calls = (%d, %d)", changedTextHit, changedModelHit, embedder.calls, otherModel.calls)
	}
}

func TestChunkEmbeddingCache_rejectsEmbedderDimensionMismatch(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "durable text")
	cache := newChunkEmbeddingCache(db)
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 3),
		vector:    []float32{1, 2},
	}

	// When
	_, _, err := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)

	// Then
	if !errors.Is(err, ErrEmbeddingDimensionMismatch) {
		t.Fatalf("dimension mismatch error = %v", err)
	}
	if embedder.calls != 1 {
		t.Fatalf("embedder calls = %d, want 1", embedder.calls)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings`).Scan(&count); err != nil {
		t.Fatalf("count cached embeddings: %v", err)
	}
	if count != 0 {
		t.Fatalf("cached embeddings = %d, want 0", count)
	}
}

func TestChunkEmbeddingCache_reportsEmbedderFailureWithoutCacheRow(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "durable text")
	cache := newChunkEmbeddingCache(db)
	failure := errors.New("fake model unavailable")
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 2),
		err:       failure,
	}

	// When
	_, _, err := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)

	// Then
	var embeddingErr *EmbedderError
	if !errors.As(err, &embeddingErr) || !errors.Is(err, failure) {
		t.Fatalf("embedder error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings`).Scan(&count); err != nil {
		t.Fatalf("count cached embeddings: %v", err)
	}
	if count != 0 {
		t.Fatalf("cached embeddings = %d, want 0", count)
	}
}

func TestChunkEmbeddingCache_rejectsCorruptAndWrongDimensionRows(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "durable text")
	cache := newChunkEmbeddingCache(db)
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 2),
		vector:    []float32{1, 2},
	}
	if _, _, err := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	// When
	if _, err := db.Exec(`UPDATE chunk_embeddings SET vector = ?`, []byte{1, 2, 3}); err != nil {
		t.Fatalf("corrupt vector: %v", err)
	}
	_, _, corruptErr := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)
	if _, err := db.Exec(`UPDATE chunk_embeddings SET vector = ?, dimension = 3`, []byte{0, 0, 128, 63, 0, 0, 0, 64}); err != nil {
		t.Fatalf("wrong dimension vector: %v", err)
	}
	_, _, dimensionErr := cache.GetOrEmbed(context.Background(), "chunk-1", "durable text", embedder)

	// Then
	if !errors.Is(corruptErr, ErrCorruptEmbedding) || !errors.Is(dimensionErr, ErrCorruptEmbedding) {
		t.Fatalf("corrupt error = %v, dimension error = %v", corruptErr, dimensionErr)
	}
}

func TestChunkEmbeddingCache_dropPreservesCanonicalChunkText(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "canonical text")
	cache := newChunkEmbeddingCache(db)
	embedder := &fakeEmbedder{
		signature: testEmbedderSignature(t, "model-a", 1),
		vector:    []float32{1},
	}
	if _, _, err := cache.GetOrEmbed(context.Background(), "chunk-1", "canonical text", embedder); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	// When
	if err := cache.Drop(); err != nil {
		t.Fatalf("drop embedding cache: %v", err)
	}

	// Then
	var text string
	if err := db.QueryRow(`SELECT text FROM page_chunks WHERE id = 'chunk-1'`).Scan(&text); err != nil {
		t.Fatalf("read canonical chunk: %v", err)
	}
	if text != "canonical text" {
		t.Fatalf("canonical chunk text = %q", text)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings`).Scan(&count); err != nil {
		t.Fatalf("count dropped embeddings: %v", err)
	}
	if count != 0 {
		t.Fatalf("dropped embeddings = %d, want 0", count)
	}
}

func TestOpenDB_migratesChunkEmbeddingCacheWithoutChangingCanonicalChunks(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE pages (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy pages: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE page_chunks (
		id TEXT PRIMARY KEY, page_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
		ord INTEGER NOT NULL, heading TEXT NOT NULL, text TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy chunks: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO pages (id, created_at, updated_at) VALUES ('page-1', ?, ?)`, now(), now()); err != nil {
		t.Fatalf("insert legacy page: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO page_chunks (id, page_id, workspace_id, ord, heading, text)
		VALUES ('chunk-1', 'page-1', 'workspace', 0, '', 'legacy canonical text')`); err != nil {
		t.Fatalf("insert legacy chunk: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	// When
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Then
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'chunk_embeddings'`).Scan(&table); err != nil {
		t.Fatalf("chunk embedding cache table: %v", err)
	}
	var kind string
	if err := db.QueryRow(`SELECT kind FROM page_chunks WHERE id = 'chunk-1'`).Scan(&kind); err != nil {
		t.Fatalf("migrated chunk kind: %v", err)
	}
	var text string
	if err := db.QueryRow(`SELECT text FROM page_chunks WHERE id = 'chunk-1'`).Scan(&text); err != nil {
		t.Fatalf("migrated chunk text: %v", err)
	}
	if kind != chunkKindBody || text != "legacy canonical text" {
		t.Fatalf("migrated canonical chunk = kind %q text %q", kind, text)
	}
}
