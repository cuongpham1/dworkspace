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

func mcpRequest(t *testing.T, s *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	s.ServeHTTP(rec, req)
	return rec
}

func proposalMCPToken(t *testing.T, s *Server, userID string) string {
	t.Helper()
	token := "proposal-mcp-token"
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, created_at) VALUES (?, ?, 'proposal MCP', ?, 'write', ?)`, newID(), userID, tokenHash(token), now()); err != nil {
		t.Fatal(err)
	}
	return token
}

func mcpResult(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("MCP response: %v (%s)", err, rec.Body.String())
	}
	if envelope.Error != nil {
		t.Fatalf("MCP error: %#v", envelope.Error)
	}
	return envelope.Result
}

func TestMCPAppsResourcesLifecycleAndInitializeCapability(t *testing.T) {
	// Given
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-mcp-resources@example.test")
	token := proposalMCPToken(t, s, uid)

	// When
	initialized := mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]}}},"clientInfo":{"name":"protocol-test","version":"1"}}}`)
	initResult := mcpResult(t, initialized)
	capabilities, ok := initResult["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("initialize capabilities = %#v", initResult["capabilities"])
	}
	if _, ok := capabilities["resources"].(map[string]any); !ok {
		t.Fatalf("resources capability missing: %#v", capabilities)
	}
	extensions, ok := capabilities["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("extension capability missing: %#v", capabilities)
	}
	uiCapability, ok := extensions["io.modelcontextprotocol/ui"].(map[string]any)
	if !ok || uiCapability["mimeTypes"].([]any)[0] != "text/html;profile=mcp-app" {
		t.Fatalf("MCP Apps capability = %#v", extensions["io.modelcontextprotocol/ui"])
	}
	listed := mcpResult(t, mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}`))
	resources, ok := listed["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("resources/list = %#v", listed)
	}
	resource, ok := resources[0].(map[string]any)
	if !ok || resource["uri"] != "ui://dworkspace/proposals/document-review.html" || resource["mimeType"] != "text/html;profile=mcp-app" {
		t.Fatalf("listed UI resource = %#v", resources[0])
	}
	read := mcpResult(t, mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://dworkspace/proposals/document-review.html"}}`))
	contents, ok := read["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("resources/read = %#v", read)
	}
	content, ok := contents[0].(map[string]any)
	if !ok || content["mimeType"] != "text/html;profile=mcp-app" {
		t.Fatalf("UI resource content = %#v", contents[0])
	}
	html := content["text"].(string)
	if strings.Contains(html, "clientInfo") || !strings.Contains(html, "appInfo") {
		t.Fatalf("UI initialize identity does not follow MCP Apps schema: %s", html)
	}
	for _, marker := range []string{"ui/initialize", "appInfo", "ui/notifications/initialized", "ui/notifications/tool-result", "ui/notifications/tool-cancelled", "ui/resource-teardown", "Changes only", "Proposed preview", "Canonical context", "Open VUS review", "aria-live"} {
		if !strings.Contains(html, marker) {
			t.Fatalf("UI resource is missing marker %q", marker)
		}
	}
	meta, ok := content["_meta"].(map[string]any)
	if !ok || meta["ui"] == nil {
		t.Fatalf("UI resource metadata = %#v", content["_meta"])
	}
	uiMeta := meta["ui"].(map[string]any)
	csp := uiMeta["csp"].(map[string]any)
	if len(csp["connectDomains"].([]any)) != 0 || len(csp["resourceDomains"].([]any)) != 0 || len(csp["frameDomains"].([]any)) != 0 {
		t.Fatalf("UI resource CSP is not restrictive: %#v", csp)
	}

	// Then
	if unauthorized := mcpRequest(t, s, "", `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}`); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized resources/list = %d", unauthorized.Code)
	}
	unknown := mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"ui://dworkspace/proposals/missing.html"}}`)
	if unknown.Code != http.StatusOK || !strings.Contains(unknown.Body.String(), "unknown UI resource") {
		t.Fatalf("unknown resource = %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestMCPProposalResultLinksWidgetAndKeepsHumanApprovalOutOfMCP(t *testing.T) {
	// Given
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-mcp-widget@example.test")
	token := proposalMCPToken(t, s, uid)
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "BRD", `[{"type":"paragraph","content":[{"type":"text","text":"canonical"}]}]`)

	// When
	toolsResult := mcpResult(t, mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	var proposalTool map[string]any
	for _, candidate := range toolsResult["tools"].([]any) {
		tool := candidate.(map[string]any)
		if tool["name"] == "proposals" {
			proposalTool = tool
		}
	}
	if proposalTool == nil {
		t.Fatal("proposals tool missing")
	}
	toolMeta := proposalTool["_meta"].(map[string]any)["ui"].(map[string]any)
	if toolMeta["resourceUri"] != "ui://dworkspace/proposals/document-review.html" {
		t.Fatalf("proposal tool UI link = %#v", toolMeta)
	}
	visibility := toolMeta["visibility"].([]any)
	if len(visibility) != 2 || visibility[0] != "model" || visibility[1] != "app" {
		t.Fatalf("proposal tool visibility = %#v", visibility)
	}
	created := mcpResult(t, mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"proposals","arguments":{"page_id":"`+page+`","action":"create","markdown":"proposed section","summary":"Agent review"}}}`))
	structured, ok := created["structuredContent"].(map[string]any)
	if !ok || structured["proposal"] == nil || structured["canonical"] == nil {
		t.Fatalf("proposal structured content = %#v", created["structuredContent"])
	}
	if structured["baseHash"] == nil || structured["proposedContent"] == nil || structured["proposedTitle"] == nil || structured["contentNote"] == nil {
		t.Fatalf("proposal compatibility fields = %#v", structured)
	}
	if _, hasURL := structured["reviewUrl"]; hasURL {
		t.Fatal("MCP result used an untrusted request Host as reviewUrl")
	}
	content := created["content"].([]any)[0].(map[string]any)["text"].(string)

	// Then
	if !strings.Contains(content, "awaiting human review") {
		t.Fatalf("text fallback lost proposal state: %s", content)
	}
	if !strings.Contains(content, "UNTRUSTED CONTENT") {
		t.Fatalf("text fallback did not frame document content: %s", content)
	}
	for _, candidate := range toolsResult["tools"].([]any) {
		name := candidate.(map[string]any)["name"]
		if name == "publish_proposal" || name == "reject_proposal" {
			t.Fatalf("approval tool exposed to MCP model: %v", name)
		}
	}
	approvalAttempt := mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"proposals","arguments":{"page_id":"`+page+`","action":"publish"}}}`)
	if approvalAttempt.Code != http.StatusOK || !strings.Contains(approvalAttempt.Body.String(), "publish and reject require the browser") {
		t.Fatalf("MCP approval attempt = %d %s", approvalAttempt.Code, approvalAttempt.Body.String())
	}
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
