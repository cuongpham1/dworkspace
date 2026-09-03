package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyProposalMigrationIsAtomicAndIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE pages (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', content TEXT NOT NULL DEFAULT '[]', type TEXT NOT NULL DEFAULT 'doc', icon TEXT NOT NULL DEFAULT '', cover TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', tags TEXT NOT NULL DEFAULT '[]', props TEXT NOT NULL DEFAULT '{}');
		CREATE TABLE page_change_proposals (
			id TEXT PRIMARY KEY, page_id TEXT NOT NULL, base_hash TEXT NOT NULL,
			proposed_content TEXT NOT NULL, proposed_title TEXT NOT NULL DEFAULT '',
			creator_id TEXT NOT NULL DEFAULT '', creator_type TEXT NOT NULL DEFAULT 'human',
			creator_name TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			status TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '', published_at TEXT,
			published_by TEXT, rejected_at TEXT, rejected_by TEXT
		);
		CREATE INDEX idx_proposal_page_status ON page_change_proposals(page_id, status, created_at);
		CREATE INDEX idx_proposal_status_created ON page_change_proposals(status, created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO page_change_proposals (id, page_id, base_hash, proposed_content, created_at, updated_at, status) VALUES ('legacy', 'page', 'hash', '[]', 'now', 'now', 'pending')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateProposalTable(db); err != nil {
		t.Fatalf("migrate legacy proposals: %v", err)
	}
	if err := migrateProposalTable(db); err != nil {
		t.Fatalf("repeat proposal migration: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pages (id, title, content) VALUES ('page', 'Legacy page', '[]')`); err != nil {
		t.Fatal(err)
	}
	if err := backfillEditProposalMetadata(db); err != nil {
		t.Fatalf("backfill legacy proposal metadata: %v", err)
	}
	if err := backfillEditProposalMetadata(db); err != nil {
		t.Fatalf("repeat legacy metadata backfill: %v", err)
	}
	var notNull int
	if err := db.QueryRow(`SELECT "notnull" FROM pragma_table_info('page_change_proposals') WHERE name = 'page_id'`).Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if notNull != 0 {
		t.Fatal("migrated page_id is still NOT NULL")
	}
	var count int
	var baseHash, snapshot string
	if err := db.QueryRow(`SELECT COUNT(*), base_hash, original_snapshot FROM page_change_proposals WHERE id = 'legacy'`).Scan(&count, &baseHash, &snapshot); err != nil || count != 1 {
		t.Fatalf("legacy proposal count = %d, %v", count, err)
	}
	if baseHash != "legacy-content:hash" || snapshot != `{"legacyBackfilled":true}` {
		t.Fatalf("legacy provenance marker = %q/%q", baseHash, snapshot)
	}
}

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

func TestMCPCreatePageCreatesProposalBeforeCanonicalPage(t *testing.T) {
	// Given
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-create@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pages`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// When
	result, err := callTool(t, s, u, "create_page", `{"title":"Agent-created BRD","workspace_id":"`+ws+`","markdown":"three acceptance criteria"}`)

	// Then
	if err != nil {
		t.Fatalf("create_page: %v", err)
	}
	if !strings.Contains(result, "CREATED PROPOSAL") || !strings.Contains(result, "awaiting human review") {
		t.Fatalf("create_page response did not describe the gate: %s", result)
	}
	var payload struct {
		Proposal struct {
			Kind   string `json:"kind"`
			PageID string `json:"pageId"`
			Status string `json:"status"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatalf("proposal response: %v (%s)", err, result)
	}
	if payload.Proposal.Kind != "create" || payload.Proposal.PageID != "" || payload.Proposal.Status != proposalStatusPending {
		t.Fatalf("create proposal = %#v", payload.Proposal)
	}
	var after int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pages`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("agent create inserted a canonical page: before=%d after=%d", before, after)
	}
	var indexed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pages_fts WHERE title = ?`, "Agent-created BRD").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 0 {
		t.Fatal("create proposal was indexed before publication")
	}
}

func TestCreateProposalReviewPublishesWithGapsAndSelectedRelatedLinks(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-create-review@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	related := s.makePage(t, ws, uid, "", "Related source", `{}`)
	unchecked := s.makePage(t, ws, uid, "", "Unchecked source", `{}`)
	review := `{"constraints":[{"id":"acceptance_criteria","label":"Acceptance Criteria","required":5}],"facts":[{"id":"ac-1","constraintId":"acceptance_criteria","label":"AC-1","value":"One","category":"provided"},{"id":"ac-2","constraintId":"acceptance_criteria","label":"AC-2","value":"Two","category":"provided"},{"id":"ac-3","constraintId":"acceptance_criteria","label":"AC-3","value":"Three","category":"provided"}]}`
	candidates := `[{"pageId":"` + related + `","title":"Related source","rationale":"Same customer topic","rank":1},{"pageId":"` + unchecked + `","title":"Unchecked source","rationale":"Nearby topic","rank":2}]`

	// When
	result, err := callTool(t, s, u, "create_page", `{"title":"Reviewable BRD","workspace_id":"`+ws+`","markdown":"agent draft","fact_review":`+review+`,"related_candidates":`+candidates+`}`)
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}
	p := payload.Proposal
	if p.Kind != proposalKindCreate || p.PageID != "" {
		t.Fatalf("proposal kind/page = %q/%q", p.Kind, p.PageID)
	}
	if p.FactReview.Validation != "unknown" || p.FactReview.Provided != 0 || p.FactReview.Required != 0 || len(p.FactReview.Gaps) != 0 {
		t.Fatalf("agent fact claims must remain unverified: %#v", p.FactReview)
	}
	if len(p.Related) != 2 || len(p.SelectedRelated) != 0 {
		t.Fatalf("related review = %#v selected=%#v", p.Related, p.SelectedRelated)
	}
	listed := proposalRequest(t, s, cookie, http.MethodGet, "/api/proposals", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), p.ID) {
		t.Fatalf("create proposal list = %d %s", listed.Code, listed.Body.String())
	}
	got := proposalRequest(t, s, cookie, http.MethodGet, "/api/proposals/"+p.ID, "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"kind":"create"`) {
		t.Fatalf("create proposal get = %d %s", got.Code, got.Body.String())
	}
	var pagesBefore int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pages WHERE title = 'Reviewable BRD'`).Scan(&pagesBefore); err != nil {
		t.Fatal(err)
	}
	if pagesBefore != 0 {
		t.Fatal("create proposal inserted a page")
	}

	patch := proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+p.ID, `{"selectedRelatedIds":["`+related+`"]}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("select candidate: %d %s", patch.Code, patch.Body.String())
	}
	var selected pageChangeProposal
	if err := json.Unmarshal(patch.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if selected.FactReview.Validation != "unknown" || selected.FactReview.Provided != 0 || len(selected.FactReview.Gaps) != 0 {
		t.Fatalf("candidate filled a fact gap: %#v", selected.FactReview)
	}
	staleEdit := proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+p.ID, `{"updatedAt":"`+p.UpdatedAt+`","proposedTitle":"stale write"}`)
	if staleEdit.Code != http.StatusConflict {
		t.Fatalf("stale proposal edit = %d %s", staleEdit.Code, staleEdit.Body.String())
	}

	partialReview := `{"constraints":[{"id":"acceptance_criteria","label":"Acceptance Criteria","required":5}],"facts":[{"id":"ac-1","constraintId":"acceptance_criteria","value":"One","category":"provided"},{"id":"ac-2","constraintId":"acceptance_criteria","value":"Two","category":"provided"},{"id":"ac-3","constraintId":"acceptance_criteria","value":"Three","category":"provided"}]}`
	patch = proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+p.ID, `{"factReview":`+partialReview+`,"selectedRelatedIds":["`+related+`"]}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("confirm supplied facts: %d %s", patch.Code, patch.Body.String())
	}
	var partial pageChangeProposal
	if err := json.Unmarshal(patch.Body.Bytes(), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.FactReview.Validation != "deterministic" || partial.FactReview.Provided != 3 || partial.FactReview.Required != 5 || len(partial.FactReview.Gaps) != 2 {
		t.Fatalf("expected deterministic 3/5 with two gaps: %#v", partial.FactReview)
	}

	completedReview := `{"constraints":[{"id":"acceptance_criteria","label":"Acceptance Criteria","required":5}],"facts":[{"id":"ac-1","constraintId":"acceptance_criteria","value":"One","category":"provided"},{"id":"ac-2","constraintId":"acceptance_criteria","value":"Two","category":"provided"},{"id":"ac-3","constraintId":"acceptance_criteria","value":"Three","category":"provided"},{"id":"ac-4","constraintId":"acceptance_criteria","value":"Four","category":"provided"},{"id":"ac-5","constraintId":"acceptance_criteria","value":"Five","category":"provided"}]}`
	patch = proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+p.ID, `{"proposedTitle":"Human-reviewed BRD","markdown":"human final","factReview":`+completedReview+`,"selectedRelatedIds":["`+related+`"]}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("edit create proposal: %d %s", patch.Code, patch.Body.String())
	}
	if !strings.Contains(patch.Body.String(), "Human-reviewed BRD") {
		t.Fatal("human edit was not returned")
	}

	published := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+p.ID+"/publish", `{}`)
	if published.Code != http.StatusOK {
		t.Fatalf("publish create proposal: %d %s", published.Code, published.Body.String())
	}
	var pageID string
	if err := s.db.QueryRow(`SELECT id FROM pages WHERE title = 'Human-reviewed BRD'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	var linkCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM links WHERE source_id = ? AND target_id = ?`, pageID, related).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if linkCount != 1 {
		t.Fatalf("selected related link count = %d", linkCount)
	}
	var uncheckedLinkCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM links WHERE source_id = ? AND target_id = ?`, pageID, unchecked).Scan(&uncheckedLinkCount); err != nil {
		t.Fatal(err)
	}
	if uncheckedLinkCount != 0 {
		t.Fatalf("unchecked related link count = %d", uncheckedLinkCount)
	}
	var status, publishedPageID string
	if err := s.db.QueryRow(`SELECT status, page_id FROM page_change_proposals WHERE id = ?`, p.ID).Scan(&status, &publishedPageID); err != nil {
		t.Fatal(err)
	}
	if status != proposalStatusPublished || publishedPageID != pageID {
		t.Fatalf("published proposal = %q/%q", status, publishedPageID)
	}
	var proposalAudit int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'proposal_published' AND page_id = ? AND actor_type = 'human'`, pageID).Scan(&proposalAudit); err != nil || proposalAudit != 1 {
		t.Fatalf("publisher audit = %d, %v", proposalAudit, err)
	}
	if second := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+p.ID+"/publish", `{}`); second.Code != http.StatusOK {
		t.Fatalf("double publish = %d %s", second.Code, second.Body.String())
	}
}

func TestAgentFactClaimsCannotBecomeDeterministicWithoutHumanConfirmation(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-trust-boundary@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	facts := []map[string]any{}
	for i := 1; i <= 5; i++ {
		facts = append(facts, map[string]any{
			"id": fmt.Sprintf("ac-%d", i), "constraintId": "acceptance_criteria", "label": fmt.Sprintf("AC-%d", i),
			"value": fmt.Sprintf("fabricated %d", i), "category": "provided",
		})
	}
	review, _ := json.Marshal(map[string]any{
		"constraints": []map[string]any{{"id": "acceptance_criteria", "label": "Acceptance Criteria", "required": 5}},
		"facts":       facts,
	})
	result, err := callTool(t, s, u, "create_page", `{"title":"Untrusted facts","workspace_id":"`+ws+`","fact_review":`+string(review)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Proposal.FactReview.Validation != "unknown" || payload.Proposal.FactReview.Provided == 5 || payload.Proposal.FactReview.Required == 5 {
		t.Fatalf("agent claims became trusted: %#v", payload.Proposal.FactReview)
	}
	for _, fact := range payload.Proposal.FactReview.Facts {
		if fact.Category == "provided" || fact.Confirmed {
			t.Fatalf("agent fact retained trusted category: %#v", fact)
		}
	}

	confirmed := review
	patch := proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+payload.Proposal.ID, `{"factReview":`+string(confirmed)+`}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("human confirmation: %d %s", patch.Code, patch.Body.String())
	}
	var edited pageChangeProposal
	if err := json.Unmarshal(patch.Body.Bytes(), &edited); err != nil {
		t.Fatal(err)
	}
	if edited.FactReview.Validation != "deterministic" || edited.FactReview.Provided != 5 || edited.FactReview.Required != 5 || len(edited.FactReview.Gaps) != 0 {
		t.Fatalf("human confirmation did not validate deterministically: %#v", edited.FactReview)
	}
}

func TestAgentCannotPreselectRelatedDocuments(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-related-control@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	related := s.makePage(t, ws, uid, "", "Candidate", `{}`)
	result, err := callTool(t, s, u, "create_page", `{"title":"Candidate control","workspace_id":"`+ws+`","related_candidates":[{"pageId":"`+related+`","title":"FABRICATED CANDIDATE","snippet":"fabricated"}],"selected_related_ids":["`+related+`"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Proposal.SelectedRelated) != 0 || payload.Proposal.Related[0].Selected || payload.Proposal.Related[0].Title != "Candidate" || payload.Proposal.Related[0].Snippet != "" {
		t.Fatalf("agent preselected a related document: %#v", payload.Proposal)
	}
}

func TestCreateProposalsUnderSameParentRemainPendingTogether(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-coexist@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	parent := s.makePage(t, ws, uid, "", "Parent", `{}`)
	for _, title := range []string{"First pending", "Second pending"} {
		if _, err := callTool(t, s, u, "create_page", `{"title":"`+title+`","workspace_id":"`+ws+`","parent_id":"`+parent+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM page_change_proposals WHERE target_parent_id = ? AND status = ?`, parent, proposalStatusPending).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("pending proposals under same parent = %d", pending)
	}
}

func TestHumanProposalEditPreservesStructuredBlocks(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-structure@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Structured", `{}`)
	original := `[{"id":"heading-1","type":"heading","props":{"level":1},"content":[{"type":"text","text":"Original heading","styles":{}}]},{"id":"list-1","type":"bulletListItem","props":{},"content":[{"type":"text","text":"Original item","styles":{}}]},{"id":"paragraph-1","type":"paragraph","props":{},"content":[{"type":"text","text":"Original paragraph","styles":{}}]}]`
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, original, page); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"draft","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	updated := `[{"id":"heading-1","type":"heading","props":{"level":1},"content":[{"type":"text","text":"Edited heading","styles":{}}]},{"id":"list-1","type":"bulletListItem","props":{},"content":[{"type":"text","text":"Edited item","styles":{}}]},{"id":"paragraph-1","type":"paragraph","props":{},"content":[{"type":"text","text":"Edited paragraph","styles":{}}]}]`
	patch := proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+id, `{"content":`+updated+`}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("structured proposal edit: %d %s", patch.Code, patch.Body.String())
	}
	var got string
	if err := s.db.QueryRow(`SELECT proposed_content FROM page_change_proposals WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(got), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 || blocks[0]["type"] != "heading" || blocks[1]["type"] != "bulletListItem" || blocks[2]["type"] != "paragraph" || blocks[0]["id"] != "heading-1" {
		t.Fatalf("structured blocks were flattened: %#v", blocks)
	}
}

func TestEditProposalPublishPreservesCanonicalMetadata(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-metadata@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Metadata", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET type = 'database', icon = '📌', cover = 'gradient:linear-gradient(red,blue)', description = 'keep me', tags = '["important"]', props = '{"schema":true}' WHERE id = ?`, page); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"new body","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	if rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+id+"/publish", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	var typ, icon, cover, description, tags, props string
	if err := s.db.QueryRow(`SELECT type, icon, cover, description, tags, props FROM pages WHERE id = ?`, page).Scan(&typ, &icon, &cover, &description, &tags, &props); err != nil {
		t.Fatal(err)
	}
	if typ != "database" || icon != "📌" || cover == "" || description != "keep me" || tags != `["important"]` || props != `{"schema":true}` {
		t.Fatalf("canonical metadata was lost: %q %q %q %q %q %q", typ, icon, cover, description, tags, props)
	}
}

func TestEditProposalStaleMetadataCannotBeOverwritten(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-stale-metadata@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Metadata race", `{}`)
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"new body","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	if _, err := s.db.Exec(`UPDATE pages SET description = 'newer metadata' WHERE id = ?`, page); err != nil {
		t.Fatal(err)
	}
	rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+id+"/publish", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("metadata-stale publish = %d %s", rec.Code, rec.Body.String())
	}
	var description string
	if err := s.db.QueryRow(`SELECT description FROM pages WHERE id = ?`, page).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "newer metadata" {
		t.Fatalf("metadata was overwritten: %q", description)
	}
}

func TestPublishedProposalRetryHealsSearchIndexWithoutRepublishing(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-index-heal@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	result, err := callTool(t, s, u, "create_page", `{"title":"Heal me","workspace_id":"`+ws+`","markdown":"indexed final"}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}
	first := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+payload.Proposal.ID+"/publish", `{}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first publish: %d %s", first.Code, first.Body.String())
	}
	var pageID string
	if err := s.db.QueryRow(`SELECT page_id FROM page_change_proposals WHERE id = ?`, payload.Proposal.ID).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM pages_fts WHERE id = ?`, pageID); err != nil {
		t.Fatal(err)
	}
	second := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+payload.Proposal.ID+"/publish", `{}`)
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent publish after index loss: %d %s", second.Code, second.Body.String())
	}
	var indexed string
	if err := s.db.QueryRow(`SELECT body FROM pages_fts WHERE id = ?`, pageID).Scan(&indexed); err != nil || !strings.Contains(indexed, "indexed final") {
		t.Fatalf("retry did not heal index: %q %v", indexed, err)
	}
}

func TestCreateProposalRejectAndAgentApprovalAreSafe(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-create-reject@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	result, err := callTool(t, s, u, "create_page", `{"title":"Rejected canonical","workspace_id":"`+ws+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}

	// When
	rejected := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+payload.Proposal.ID+"/reject", `{}`)

	// Then
	if rejected.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rejected.Code, rejected.Body.String())
	}
	if repeated := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+payload.Proposal.ID+"/reject", `{}`); repeated.Code != http.StatusOK {
		t.Fatalf("double reject: %d", repeated.Code)
	}
	var pages int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pages WHERE title = 'Rejected canonical'`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 0 {
		t.Fatal("reject left a canonical page")
	}
	approval := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/proposals/"+payload.Proposal.ID+"/publish", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token-not-used")
	s.ServeHTTP(approval, req)
	if approval.Code != http.StatusUnauthorized {
		t.Fatalf("agent approval without token record = %d", approval.Code)
	}
}

func TestHumanEditingEditProposalPreservesBaseHashAndStaleProtection(t *testing.T) {
	// Given
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-edit-workspace@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Canonical", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, `[ {"type":"paragraph","content":[{"type":"text","text":"old"}]} ]`, page); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"agent draft","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	id := proposalID(t, s, page)
	var originalHash string
	if err := s.db.QueryRow(`SELECT base_hash FROM page_change_proposals WHERE id = ?`, id).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}

	// When
	updated := proposalRequest(t, s, cookie, http.MethodPatch, "/api/proposals/"+id, `{"proposedTitle":"Human title","markdown":"human proposal"}`)

	// Then
	if updated.Code != http.StatusOK {
		t.Fatalf("edit proposal: %d %s", updated.Code, updated.Body.String())
	}
	var editedHash, lastEditor string
	if err := s.db.QueryRow(`SELECT base_hash, last_human_editor FROM page_change_proposals WHERE id = ?`, id).Scan(&editedHash, &lastEditor); err != nil {
		t.Fatal(err)
	}
	if editedHash != originalHash || lastEditor != uid {
		t.Fatalf("proposal provenance/base = %q/%q", editedHash, lastEditor)
	}
	if _, err := s.db.Exec(`UPDATE pages SET title = ?, content = ? WHERE id = ?`, "Newer human", `[ {"type":"paragraph","content":[{"type":"text","text":"newer"}]} ]`, page); err != nil {
		t.Fatal(err)
	}
	stale := proposalRequest(t, s, cookie, http.MethodPost, "/api/proposals/"+id+"/publish", `{}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale edited proposal = %d %s", stale.Code, stale.Body.String())
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

func TestMCPCreatePageReturnsStructuredProposalAndReviewLinkContract(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "proposal-mcp-create@example.test")
	token := proposalMCPToken(t, s, uid)
	ws := s.firstWorkspaceOf(t, uid)
	rec := mcpRequest(t, s, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_page","arguments":{"title":"MCP create","workspace_id":"`+ws+`","markdown":"draft"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("MCP create_page: %d %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	proposal, ok := envelope.Result.Structured["proposal"].(map[string]any)
	if !ok || proposal["kind"] != proposalKindCreate || proposal["status"] != proposalStatusPending {
		t.Fatalf("structured create proposal = %#v", envelope.Result.Structured)
	}
	if pageID, ok := envelope.Result.Structured["pageId"].(string); !ok || pageID != "" {
		t.Fatalf("create proposal exposed a canonical page id: %#v", envelope.Result.Structured["pageId"])
	}
	if len(envelope.Result.Content) != 1 || !strings.Contains(envelope.Result.Content[0].Text, "CREATED PROPOSAL") || !strings.Contains(envelope.Result.Content[0].Text, "awaiting human review") {
		t.Fatalf("MCP text fallback = %#v", envelope.Result.Content)
	}

	for _, candidate := range mcpTools {
		if candidate["name"] == "create_page" {
			if candidate["_meta"] == nil || candidate["outputSchema"] == nil {
				t.Fatalf("create_page Apps contract = %#v", candidate)
			}
		}
		if candidate["name"] == "proposals" {
			schema := candidate["inputSchema"].(map[string]any)
			required := schema["required"].([]string)
			if len(required) != 0 {
				t.Fatalf("proposals still requires page_id for list/get contract: %#v", required)
			}
		}
	}
}

func TestProposalReadsHideCandidateThatLosesReadPermission(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "proposal-candidate-visibility@example.test")
	u := &user{ID: uid, Name: "Proposal Agent", TokenScope: "write", TokenKind: tokenKindAPI}
	ws := s.firstWorkspaceOf(t, uid)
	candidate := s.makePage(t, ws, uid, "", "Sensitive candidate", `{}`)
	result, err := callTool(t, s, u, "create_page", `{"title":"Visibility proposal","workspace_id":"`+ws+`","related_candidates":[{"pageId":"`+candidate+`","title":"Sensitive candidate"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Proposal pageChangeProposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(stripMarkers(result)), &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pages SET workspace_id = ? WHERE id = ?`, newID(), candidate); err != nil {
		t.Fatal(err)
	}
	got := proposalRequest(t, s, cookie, http.MethodGet, "/api/proposals/"+payload.Proposal.ID, "")
	if got.Code != http.StatusOK {
		t.Fatalf("proposal read after candidate access changed: %d %s", got.Code, got.Body.String())
	}
	var visible pageChangeProposal
	if err := json.Unmarshal(got.Body.Bytes(), &visible); err != nil {
		t.Fatal(err)
	}
	if len(visible.Related) != 0 || len(visible.SelectedRelated) != 0 || strings.Contains(string(visible.OriginalSnapshot), candidate) {
		t.Fatalf("inaccessible candidate leaked through proposal: %#v", visible.Related)
	}
}
