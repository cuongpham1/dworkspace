package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shared fixtures for the skill control plane tests.
//
// The tests deliberately go through the real entry points — s.ServeHTTP for the
// interface and s.mcpCall for agents — rather than calling the service
// directly, wherever the thing under test is a RULE rather than an algorithm.
// A permission check that is only ever exercised by calling the function it
// guards proves nothing about whether the route reaches it.

// skillFixture is one instance with an admin, a plain member, an outsider and a
// second workspace — everything the isolation and permission tests need.
type skillFixture struct {
	s *Server
	// The workspace under test, plus a second one nobody in it can see.
	ws, otherWS string
	// admin: workspace admin, therefore the reviewer.
	// member: ordinary member — may author, may not approve.
	// outsider: member of otherWS only.
	adminID, adminCookie   string
	memberID, memberCookie string
	outsiderID, outsiderCk string
}

func newSkillFixture(t *testing.T) *skillFixture {
	t.Helper()
	s := testServer(t)
	adminID, adminCookie := signedIn(t, s, "skill-admin@example.test")
	ws := s.firstWorkspaceOf(t, adminID)
	if _, err := s.db.Exec(
		`UPDATE workspace_members SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, ws, adminID); err != nil {
		t.Fatalf("make admin: %v", err)
	}

	memberID, memberCookie := signedIn(t, s, "skill-member@example.test")
	if _, err := s.db.Exec(
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws, memberID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	otherWS := newID()
	if _, err := s.db.Exec(`INSERT INTO workspaces (id, name, created_at) VALUES (?, 'Elsewhere', ?)`,
		otherWS, now()); err != nil {
		t.Fatalf("second workspace: %v", err)
	}
	outsiderID, outsiderCk := signedIn(t, s, "skill-outsider@example.test")
	if _, err := s.db.Exec(
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, otherWS, outsiderID); err != nil {
		t.Fatalf("add outsider: %v", err)
	}

	return &skillFixture{
		s: s, ws: ws, otherWS: otherWS,
		adminID: adminID, adminCookie: adminCookie,
		memberID: memberID, memberCookie: memberCookie,
		outsiderID: outsiderID, outsiderCk: outsiderCk,
	}
}

func (f *skillFixture) userOf(t *testing.T, id string) *user {
	t.Helper()
	u := f.s.userByID(id)
	if u == nil {
		t.Fatalf("load user %s: not found", id)
	}
	return u
}

// agentUser is the same account arriving with a write-scoped API TOKEN rather
// than a browser session. That difference is what most of the trust-boundary
// tests turn on, so it gets a helper.
func (f *skillFixture) agentUser(t *testing.T, id string) *user {
	t.Helper()
	u := f.userOf(t, id)
	copied := *u
	copied.TokenScope = "write"
	copied.TokenKind = "api"
	return &copied
}

// call issues an HTTP request through the real router.
func (f *skillFixture) call(t *testing.T, method, path, cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, r)
	return rec
}

// token mints a write-scoped API token for an account and returns the bearer
// header value.
func (f *skillFixture) token(t *testing.T, userID, scope string) string {
	t.Helper()
	raw := newID()
	if _, err := f.s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, created_at)
		VALUES (?, ?, 'probe', ?, ?, ?)`, newID(), userID, tokenHash(raw), scope, now()); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	return "Bearer " + raw
}

func (f *skillFixture) callWithToken(t *testing.T, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, r)
	return rec
}

// decode unmarshals a recorder body, failing the test with the body text when
// it is not the shape expected — a 400 decoded into a success struct otherwise
// shows up as a confusing zero value ten lines later.
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
}

// makeSkill creates a skill straight through the service. Used where the test
// is about something LATER in the lifecycle and the creation itself is not the
// subject.
func (f *skillFixture) makeSkill(t *testing.T, actorID, name, delivery, instructions string, mutate func(*skillDraftInput)) (skill, skillVersion) {
	t.Helper()
	description := name + " — what it is for"
	scope := skillScope{WorkspaceID: f.ws}
	in := skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery,
		Instructions: &instructions, Scope: &scope,
	}
	if mutate != nil {
		mutate(&in)
	}
	sk, v, err := f.s.createSkill(f.userOf(t, actorID), f.ws, in)
	if err != nil {
		t.Fatalf("createSkill %q: %v", name, err)
	}
	return sk, v
}

// publish walks a draft all the way to published, which several tests need as a
// starting point rather than as their subject.
func (f *skillFixture) publish(t *testing.T, sk skill, v skillVersion) (skill, skillVersion) {
	t.Helper()
	member := f.userOf(t, v.CreatedBy)
	submitted, err := f.s.submitForReview(member, sk, v)
	if err != nil {
		t.Fatalf("submit %s v%d: %v", sk.Slug, v.Version, err)
	}
	nextSkill, approved, err := f.s.approveVersion(f.userOf(t, f.adminID), sk, submitted)
	if err != nil {
		t.Fatalf("approve %s v%d: %v", sk.Slug, v.Version, err)
	}
	return nextSkill, approved
}

// auditActions lists the audit verbs recorded for a skill, oldest first, so a
// test can assert on the trail as a sequence.
func (f *skillFixture) auditActions(t *testing.T, skillID string) []string {
	t.Helper()
	entries, err := f.s.skillAudit(skillID, skillAuditFilter{Limit: 500})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	out := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- { // skillAudit returns newest first
		out = append(out, entries[i].Action)
	}
	return out
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}
