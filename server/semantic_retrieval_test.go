package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSemanticLifecycle_reusesUnchangedBodyEmbeddingAndRemovesObsoleteChunks(t *testing.T) {
	// Given
	s := testServer(t)
	counted, err := NewEmbedderSignature("test", "counting", "v1", localEmbeddingDimensions)
	if err != nil {
		t.Fatalf("counting signature: %v", err)
	}
	embedder := &countingEmbedder{signature: counted}
	if err := s.SetSemanticEmbedder(embedder); err != nil {
		t.Fatalf("set counting embedder: %v", err)
	}
	userID, _ := signedIn(t, s, "semantic-lifecycle@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	pageID := "semantic-lifecycle"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{pageID, "Lifecycle", "stable identity", ws, userID, "workspace"})
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[{"type":"paragraph","content":[{"type":"text","text":"stable body"}]}]`, pageID); err != nil {
		t.Fatalf("set body: %v", err)
	}
	if err := s.reindexPage(pageID); err != nil {
		t.Fatalf("index initial body: %v", err)
	}
	oldID := chunkIDForText(t, s, pageID, "stable body")
	initialCalls := s.semanticEmbedder.(*countingEmbedder).calls

	// When
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[{"type":"paragraph","content":[{"type":"text","text":"stable body"}]},{"type":"paragraph","content":[{"type":"text","text":"new passage"}]}]`, pageID); err != nil {
		t.Fatalf("append body: %v", err)
	}
	if err := s.reindexPage(pageID); err != nil {
		t.Fatalf("reindex appended body: %v", err)
	}

	// Then
	if got := chunkIDForText(t, s, pageID, "stable body"); got != oldID {
		t.Fatalf("unchanged chunk id = %q, want reused %q", got, oldID)
	}
	if calls := s.semanticEmbedder.(*countingEmbedder).calls; calls != initialCalls+1 {
		t.Fatalf("embed calls after append = %d, want %d", calls, initialCalls+1)
	}
	obsoleteID := chunkIDForText(t, s, pageID, "new passage")
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[{"type":"paragraph","content":[{"type":"text","text":"stable body"}]}]`, pageID); err != nil {
		t.Fatalf("remove body passage: %v", err)
	}
	if err := s.reindexPage(pageID); err != nil {
		t.Fatalf("reindex removed body: %v", err)
	}
	var stale int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings WHERE chunk_id IN (?, ?)`, oldID, obsoleteID).Scan(&stale); err != nil {
		t.Fatalf("count lifecycle embeddings: %v", err)
	}
	if stale != 1 {
		t.Fatalf("lifecycle embedding rows = %d, want 1", stale)
	}
}

func TestSemanticSearch_filtersPrivateVectorsBeforeRankingAndExposesProvenance(t *testing.T) {
	// Given
	s := testServer(t)
	alice, _ := signedIn(t, s, "semantic-private-alice@example.test")
	bob, cookie := signedIn(t, s, "semantic-private-bob@example.test")
	ws := s.firstWorkspaceOf(t, alice)
	addWorkspaceMember(t, s, workspaceMember{ws, bob})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-private", "Private roadmap", "quantum-private-roadmap", ws, alice, "private"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-public", "Public roadmap", "quantum-public-roadmap", ws, bob, "workspace"})

	// When
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape("quantum roadmap"), nil)
	req.Header.Set("Cookie", cookie)
	s.ServeHTTP(rec, req)

	// Then
	if rec.Code != http.StatusOK {
		t.Fatalf("semantic search status = %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "semantic-private") {
		t.Fatalf("private semantic page leaked: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provenance"`) || !strings.Contains(rec.Body.String(), `"kind":"description"`) {
		t.Fatalf("semantic provenance missing: %s", rec.Body.String())
	}
}

func TestSemanticSearch_fallsBackToFTSWhenEmbeddingFails(t *testing.T) {
	// Given
	s := testServer(t)
	userID, cookie := signedIn(t, s, "semantic-fallback@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-fallback", "Fallback title", "ordinary lexical phrase", ws, userID, "workspace"})
	s.semanticEmbedder = &failingEmbedder{signature: s.semanticEmbedder.Signature()}

	// When
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape("ordinary lexical phrase"), nil)
	req.Header.Set("Cookie", cookie)
	s.ServeHTTP(rec, req)

	// Then
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "semantic-fallback") {
		t.Fatalf("lexical fallback response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSemanticSearch_identityBoostIsConfigurable(t *testing.T) {
	// Given
	s := testServer(t)
	userID, _ := signedIn(t, s, "semantic-boost@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-boost-description", "Identity", "shared concept", ws, userID, "workspace"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-boost-body", "Body", "unrelated", ws, userID, "workspace"})
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[{"type":"paragraph","content":[{"type":"text","text":"shared concept"}]}]`, "semantic-boost-body"); err != nil {
		t.Fatalf("set body: %v", err)
	}
	if err := s.reindexPage("semantic-boost-body"); err != nil {
		t.Fatalf("reindex body: %v", err)
	}
	config := s.semanticConfig
	config.DescriptionBoost = 2
	if err := s.SetSemanticSearchConfig(config); err != nil {
		t.Fatalf("set semantic config: %v", err)
	}

	// When
	results := s.searchSemantic(context.Background(), userID, "shared concept", []string{ws}, 10)

	// Then
	if len(results) == 0 || results[0].ID != "semantic-boost-description" {
		t.Fatalf("identity boost ranking = %+v", results)
	}
}

func TestChunkEmbeddingCache_removesObsoleteModelRows(t *testing.T) {
	// Given
	db := newEmbeddingTestDB(t)
	seedEmbeddingChunk(t, db, "page-1", "chunk-1", "durable text")
	cache := newChunkEmbeddingCache(db)
	old := testEmbedderSignature(t, "model-a", 1)
	current := testEmbedderSignature(t, "model-b", 1)
	if err := cache.Store("chunk-1", "durable text", old, []float32{1}); err != nil {
		t.Fatalf("store old model: %v", err)
	}
	if err := cache.Store("chunk-1", "durable text", current, []float32{1}); err != nil {
		t.Fatalf("store current model: %v", err)
	}

	// When
	if err := cache.DropOtherModels(current); err != nil {
		t.Fatalf("drop obsolete models: %v", err)
	}

	// Then
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings`).Scan(&count); err != nil {
		t.Fatalf("count model rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("model rows = %d, want 1", count)
	}
}

func TestSemanticLifecycle_trashRestoreAndPermanentDeleteCleanDerivedRows(t *testing.T) {
	// Given
	s := testServer(t)
	userID, cookie := signedIn(t, s, "semantic-trash@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-trash", "Trash target", "trash lifecycle phrase", ws, userID, "workspace"})
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings WHERE chunk_id IN (SELECT id FROM page_chunks WHERE page_id = 'semantic-trash')`).Scan(&before); err != nil {
		t.Fatalf("count initial embeddings: %v", err)
	}

	// When
	trash := httptest.NewRecorder()
	trashReq := httptest.NewRequest(http.MethodDelete, "/api/pages/semantic-trash", nil)
	trashReq.Header.Set("Cookie", cookie)
	s.ServeHTTP(trash, trashReq)
	if trash.Code != http.StatusOK {
		t.Fatalf("trash status = %d %s", trash.Code, trash.Body.String())
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM page_chunks WHERE page_id = 'semantic-trash'`).Scan(&chunks); err != nil {
		t.Fatalf("count trashed chunks: %v", err)
	}
	if chunks == 0 {
		t.Fatal("trashing removed canonical chunks")
	}
	restore := httptest.NewRecorder()
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/pages/semantic-trash/restore", nil)
	restoreReq.Header.Set("Cookie", cookie)
	s.ServeHTTP(restore, restoreReq)
	if restore.Code != http.StatusOK {
		t.Fatalf("restore status = %d %s", restore.Code, restore.Body.String())
	}
	permanent := httptest.NewRecorder()
	permanentReq := httptest.NewRequest(http.MethodDelete, "/api/pages/semantic-trash?permanent=1", nil)
	permanentReq.Header.Set("Cookie", cookie)
	s.ServeHTTP(permanent, permanentReq)

	// Then
	if permanent.Code != http.StatusOK {
		t.Fatalf("permanent delete status = %d %s", permanent.Code, permanent.Body.String())
	}
	var after int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings e LEFT JOIN page_chunks c ON c.id = e.chunk_id
		WHERE c.page_id = 'semantic-trash' OR c.id IS NULL`).Scan(&after); err != nil {
		t.Fatalf("count deleted embeddings: %v", err)
	}
	if before == 0 || after != 0 {
		t.Fatalf("embedding lifecycle counts = (%d, %d)", before, after)
	}
}

func TestSemanticLifecycle_workspacePurgeRemovesEmbeddingContent(t *testing.T) {
	// Given
	s := testServer(t)
	userID, _ := signedIn(t, s, "semantic-purge@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"semantic-purge", "Purge target", "purge-only semantic content", ws, userID, "workspace"})
	var embeddings int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings WHERE chunk_id IN (SELECT id FROM page_chunks WHERE page_id = 'semantic-purge')`).Scan(&embeddings); err != nil {
		t.Fatalf("count purge embeddings: %v", err)
	}

	// When
	if err := s.purgeWorkspace(ws); err != nil {
		t.Fatalf("purge workspace: %v", err)
	}

	// Then
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunk_embeddings`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining embeddings: %v", err)
	}
	if embeddings == 0 || remaining != 0 {
		t.Fatalf("purged embedding counts = (%d, %d)", embeddings, remaining)
	}
}

type countingEmbedder struct {
	signature EmbedderSignature
	calls     int
}

func (e *countingEmbedder) Signature() EmbedderSignature { return e.signature }

func (e *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.calls++
	return hashEmbedding(text, e.signature.Dimensions()), nil
}

type failingEmbedder struct{ signature EmbedderSignature }

func (e *failingEmbedder) Signature() EmbedderSignature { return e.signature }

func (e *failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("semantic model unavailable")
}

func chunkIDForText(t *testing.T, s *Server, pageID, text string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM page_chunks WHERE page_id = ? AND text = ?`, pageID, text).Scan(&id); err != nil {
		t.Fatalf("chunk %q: %v", text, err)
	}
	return id
}
