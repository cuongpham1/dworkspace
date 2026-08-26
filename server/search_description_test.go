package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type descriptionSearchPage struct {
	id, title, description string
	workspaceID, ownerID   string
	visibility             string
}

func seedDescriptionSearchPage(t *testing.T, s *Server, page descriptionSearchPage) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO pages
		(id, title, description, content, position, created_at, updated_at, workspace_id, owner_id, visibility, type)
		VALUES (?, ?, ?, '[]', 0, ?, ?, ?, ?, ?, 'doc')`,
		page.id, page.title, page.description, now(), now(), page.workspaceID, page.ownerID, page.visibility); err != nil {
		t.Fatalf("insert page %s: %v", page.id, err)
	}
	if err := s.reindexPage(page.id); err != nil {
		t.Fatalf("reindex page %s: %v", page.id, err)
	}
}

type descriptionSearchRequest struct {
	query   string
	headers map[string]string
}

func searchDescriptionPages(t *testing.T, s *Server, request descriptionSearchRequest) []searchResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape(request.query), nil)
	for key, value := range request.headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %q: got %d %s", request.query, rec.Code, rec.Body.String())
	}
	var results []searchResult
	if err := json.NewDecoder(rec.Body).Decode(&results); err != nil {
		t.Fatalf("decode search %q: %v", request.query, err)
	}
	return results
}

func resultIDs(results []searchResult) map[string]bool {
	ids := make(map[string]bool, len(results))
	for _, result := range results {
		ids[result.ID] = true
	}
	return ids
}

func TestPageDescriptionUpdateReindexesAndClearsDescriptionSearch(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "description-owner@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	pageID := "description-update"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{pageID, "Stable title", "", ws, uid, "workspace"})

	req := httptest.NewRequest(http.MethodPatch, "/api/pages/"+pageID,
		strings.NewReader(`{"description":"violet description phrase"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("description update: got %d %s", rec.Code, rec.Body.String())
	}
	if ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"violet description phrase", map[string]string{"Cookie": cookie}})); !ids[pageID] {
		t.Fatalf("description-only search missed updated page: %v", ids)
	}

	var indexedDescription string
	if err := s.db.QueryRow(`SELECT description FROM pages_fts WHERE id = ?`, pageID).Scan(&indexedDescription); err != nil {
		t.Fatalf("read page FTS row: %v", err)
	}
	if indexedDescription != "violet description phrase" {
		t.Fatalf("indexed description = %q", indexedDescription)
	}
	var kind, text string
	var ord int
	if err := s.db.QueryRow(`SELECT kind, text, ord FROM page_chunks WHERE page_id = ? AND kind = 'description'`, pageID).Scan(&kind, &text, &ord); err != nil {
		t.Fatalf("read description chunk: %v", err)
	}
	if kind != "description" || ord != 0 || !strings.Contains(text, "Stable title") || !strings.Contains(text, "violet description phrase") {
		t.Fatalf("description chunk = kind %q ord %d text %q", kind, ord, text)
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/pages/"+pageID, strings.NewReader(`{"description":""}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("description clear: got %d %s", rec.Code, rec.Body.String())
	}
	if ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"violet description phrase", map[string]string{"Cookie": cookie}})); ids[pageID] {
		t.Fatalf("cleared description still searchable: %v", ids)
	}
	var descriptionChunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM page_chunks WHERE page_id = ? AND kind = 'description'`, pageID).Scan(&descriptionChunks); err != nil {
		t.Fatalf("count description chunks: %v", err)
	}
	if descriptionChunks != 0 {
		t.Fatalf("cleared description left %d chunks", descriptionChunks)
	}
	var bodyKind string
	var bodyOrd int
	if err := s.db.QueryRow(`SELECT kind, ord FROM page_chunks WHERE page_id = ?`, pageID).Scan(&bodyKind, &bodyOrd); err != nil {
		t.Fatalf("read title chunk after clear: %v", err)
	}
	if bodyKind != "body" || bodyOrd != 0 {
		t.Fatalf("title chunk = kind %q ord %d", bodyKind, bodyOrd)
	}
}

func TestLongDescriptionIsPreservedForFutureEmbeddings(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "description-length@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	pageID := "description-length"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{pageID, "Semantic abstract", "", ws, uid, "workspace"})

	description := strings.TrimSpace(strings.Repeat("This semantic abstract carries durable retrieval context. ", 80))
	payload, err := json.Marshal(map[string]string{"description": description})
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/pages/"+pageID, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("description update: got %d %s", rec.Code, rec.Body.String())
	}

	var stored string
	if err := s.db.QueryRow(`SELECT description FROM pages WHERE id = ?`, pageID).Scan(&stored); err != nil {
		t.Fatalf("read stored description: %v", err)
	}
	if stored != description {
		t.Fatalf("stored description length = %d, want %d", len(stored), len(description))
	}
	var chunkText string
	if err := s.db.QueryRow(`SELECT text FROM page_chunks WHERE page_id = ? AND kind = 'description'`, pageID).Scan(&chunkText); err != nil {
		t.Fatalf("read description chunk: %v", err)
	}
	if !strings.HasSuffix(chunkText, description) {
		t.Fatalf("description chunk lost content: got %d chars, want suffix of %d chars", len(chunkText), len(description))
	}
}

func TestSearchIndexMigrationRebuildsDescriptionChunks(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "description-migration@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	pageID := "description-migration"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{pageID, "Migration title", "Migration abstract", ws, uid, "workspace"})

	s.setSetting("fts_version", "3")
	if err := s.migrateSearchIndex(); err != nil {
		t.Fatalf("migrate search index: %v", err)
	}

	var indexedDescription string
	if err := s.db.QueryRow(`SELECT description FROM pages_fts WHERE id = ?`, pageID).Scan(&indexedDescription); err != nil {
		t.Fatalf("read rebuilt description: %v", err)
	}
	if indexedDescription != "Migration abstract" {
		t.Fatalf("rebuilt description = %q", indexedDescription)
	}
	var kind, text string
	var ord int
	if err := s.db.QueryRow(`SELECT kind, text, ord FROM page_chunks WHERE page_id = ? AND kind = 'description'`, pageID).Scan(&kind, &text, &ord); err != nil {
		t.Fatalf("read rebuilt description chunk: %v", err)
	}
	if kind != "description" || ord != 0 || !strings.Contains(text, "Migration abstract") {
		t.Fatalf("rebuilt description chunk = kind %q ord %d text %q", kind, ord, text)
	}
}
