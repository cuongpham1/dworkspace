package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func proposalID(t *testing.T, s *Server, pageID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM page_change_proposals WHERE page_id = ?`, pageID).Scan(&id); err != nil {
		t.Fatalf("proposal id: %v", err)
	}
	return id
}

func proposalRequest(t *testing.T, s *Server, cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	s.ServeHTTP(rec, req)
	return rec
}

func TestMCPReplaceContentCreatesProposalWithoutCanonicalSideEffects(t *testing.T) {
	// Given
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-agent@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "BRD", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET content = ?, updated_at = ? WHERE id = ?`, `[ {"type":"paragraph","content":[{"type":"text","text":"old"}]} ]`, now(), page); err != nil {
		t.Fatal(err)
	}
	if err := s.reindexPage(page); err != nil {
		t.Fatal(err)
	}
	var before, oldUpdated string
	if err := s.db.QueryRow(`SELECT content, updated_at FROM pages WHERE id = ?`, page).Scan(&before, &oldUpdated); err != nil {
		t.Fatal(err)
	}

	// When
	message, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"new proposal","mode":"replace"}`)

	// Then
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !strings.Contains(message, "awaiting human review") {
		t.Fatalf("replace message did not describe proposal semantics: %s", message)
	}
	var got, updated string
	if err := s.db.QueryRow(`SELECT content, updated_at FROM pages WHERE id = ?`, page).Scan(&got, &updated); err != nil {
		t.Fatal(err)
	}
	if got != before || updated != oldUpdated {
		t.Fatalf("canonical page changed while proposing: content=%q updated=%q", got, updated)
	}
	var status, proposed, baseHash string
	if err := s.db.QueryRow(`SELECT status, proposed_content, base_hash FROM page_change_proposals WHERE id = ?`, proposalID(t, s, page)).Scan(&status, &proposed, &baseHash); err != nil {
		t.Fatal(err)
	}
	if status != proposalStatusPending || !strings.Contains(proposed, "new proposal") || baseHash == "" {
		t.Fatalf("proposal row = status=%q content=%q base=%q", status, proposed, baseHash)
	}
	var indexed string
	if err := s.db.QueryRow(`SELECT body FROM pages_fts WHERE id = ?`, page).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(indexed, "new proposal") {
		t.Fatal("proposal content was indexed before publication")
	}
}

func TestProposalPublishIsAtomicAndUndoable(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-human@example.test")
	u := &user{ID: uid, Name: "Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "PRD", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[ {"type":"paragraph","content":[{"type":"text","text":"before"}]} ]`, page); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"after","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)

	// When
	rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/publish", `{}`)

	// Then
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	var status, content string
	if err := s.db.QueryRow(`SELECT p.status, pg.content FROM page_change_proposals p JOIN pages pg ON pg.id = p.page_id WHERE p.id = ?`, id).Scan(&status, &content); err != nil {
		t.Fatal(err)
	}
	if status != proposalStatusPublished || !strings.Contains(content, "after") {
		t.Fatalf("published state = %q content=%q", status, content)
	}
	var revisions int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM page_revisions WHERE page_id = ? AND content LIKE '%before%'`, page).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 1 {
		t.Fatal("publish did not snapshot the canonical state")
	}
	var indexed string
	if err := s.db.QueryRow(`SELECT body FROM pages_fts WHERE id = ?`, page).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexed, "after") {
		t.Fatal("published content was not indexed")
	}
	if second := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/publish", `{}`); second.Code != http.StatusOK {
		t.Fatalf("publishing an already published proposal should be idempotent: %d", second.Code)
	}
}

func TestProposalPublishRejectsStaleBaseWithoutOverwrite(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-stale@example.test")
	u := &user{ID: uid, Name: "Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Doc", `{}`)
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"agent version","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	if _, err := s.db.Exec(`UPDATE pages SET title = ?, content = ?, updated_at = ? WHERE id = ?`, "Human title", `[{"type":"paragraph","content":[{"type":"text","text":"human version"}]}]`, now(), page); err != nil {
		t.Fatal(err)
	}

	// When
	rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/publish", `{}`)

	// Then
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since") {
		t.Fatalf("stale publish = %d %s", rec.Code, rec.Body.String())
	}
	var status, title, content string
	if err := s.db.QueryRow(`SELECT p.status, pg.title, pg.content FROM page_change_proposals p JOIN pages pg ON pg.id = p.page_id WHERE p.id = ?`, id).Scan(&status, &title, &content); err != nil {
		t.Fatal(err)
	}
	if status != proposalStatusPending || title != "Human title" || !strings.Contains(content, "human version") {
		t.Fatalf("stale publish overwrote canonical state: status=%q title=%q content=%q", status, title, content)
	}
}

func TestProposalRejectAndApprovalAuthorization(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-auth@example.test")
	u := &user{ID: uid, Name: "Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Doc", `{}`)
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"pending","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	rawToken := "proposal-bearer"
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, created_at) VALUES (?, ?, 'agent', ?, 'write', ?)`, newID(), uid, tokenHash(rawToken), now()); err != nil {
		t.Fatal(err)
	}

	// When
	anonymous := proposalRequest(t, s, "", http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/publish", `{}`)
	bearer := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/publish", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	s.ServeHTTP(bearer, req)

	// Then
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), "session") {
		t.Fatalf("API token approval = %d %s", bearer.Code, bearer.Body.String())
	}
	direct := httptest.NewRecorder()
	directReq := httptest.NewRequest(http.MethodPatch, "/api/pages/"+page, strings.NewReader(`{"content":[]}`))
	directReq.Header.Set("Authorization", "Bearer "+rawToken)
	s.ServeHTTP(direct, directReq)
	if direct.Code != http.StatusForbidden || !strings.Contains(direct.Body.String(), "proposal_required") {
		t.Fatalf("old replace endpoint bypass = %d %s", direct.Code, direct.Body.String())
	}
	restore := httptest.NewRecorder()
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/pages/"+page+"/revisions/missing/restore", strings.NewReader(`{}`))
	restoreReq.Header.Set("Authorization", "Bearer "+rawToken)
	s.ServeHTTP(restore, restoreReq)
	if restore.Code != http.StatusForbidden || !strings.Contains(restore.Body.String(), "session_required") {
		t.Fatalf("old restore endpoint bypass = %d %s", restore.Code, restore.Body.String())
	}
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approval = %d", anonymous.Code)
	}
	if rejected := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/reject", `{}`); rejected.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rejected.Code, rejected.Body.String())
	}
	if repeated := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+page+"/proposals/"+id+"/reject", `{}`); repeated.Code != http.StatusOK {
		t.Fatalf("reject should be idempotent: %d", repeated.Code)
	}
	var status, content string
	if err := s.db.QueryRow(`SELECT p.status, pg.content FROM page_change_proposals p JOIN pages pg ON pg.id = p.page_id WHERE p.id = ?`, id).Scan(&status, &content); err != nil {
		t.Fatal(err)
	}
	if status != proposalStatusRejected || content != "[]" {
		t.Fatalf("reject touched canonical state: status=%q content=%q", status, content)
	}
}

func TestProposalMCPReadListAndCrossPageIDProtection(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-list@example.test")
	u := &user{ID: uid, Name: "Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Doc", `{}`)
	other := s.makePage(t, ws, uid, "", "Other", `{}`)
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"proposal body","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}

	// When
	list, err := callTool(t, s, u, "proposals", `{"page_id":"`+page+`","action":"list"}`)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Proposals []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(list)), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Proposals) != 1 || decoded.Proposals[0].Status != proposalStatusPending {
		t.Fatalf("proposal list = %s", list)
	}
	rawToken := "proposal-structured"
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, created_at) VALUES (?, ?, 'agent', ?, 'read', ?)`, newID(), uid, tokenHash(rawToken), now()); err != nil {
		t.Fatal(err)
	}
	rpc := httptest.NewRecorder()
	rpcReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"proposals","arguments":{"page_id":"`+page+`","action":"list"}}}`))
	rpcReq.Header.Set("Authorization", "Bearer "+rawToken)
	s.ServeHTTP(rpc, rpcReq)
	var envelope struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rpc.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("proposal MCP result had no structuredContent: %s", rpc.Body.String())
	}
	id := decoded.Proposals[0].ID
	wrongPage := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+other+"/proposals/"+id+"/reject", `{}`)

	// Then
	if wrongPage.Code != http.StatusNotFound {
		t.Fatalf("cross-page proposal id = %d %s", wrongPage.Code, wrongPage.Body.String())
	}
}

func TestProposalToolAdvertisesStructuredOutputContract(t *testing.T) {
	var tool map[string]any
	for _, candidate := range mcpTools {
		if candidate["name"] == "proposals" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("proposals tool is not advertised")
	}
	schema, ok := tool["outputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("proposal output schema = %#v", tool["outputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["proposals"] == nil || properties["status"] == nil {
		t.Fatalf("proposal output schema properties = %#v", schema["properties"])
	}
}
