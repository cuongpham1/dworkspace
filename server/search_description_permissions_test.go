package server

import (
	"strings"
	"testing"
)

type workspaceMember struct {
	workspaceID, userID string
}

func addWorkspaceMember(t *testing.T, s *Server, member workspaceMember) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'member')`, member.workspaceID, member.userID); err != nil {
		t.Fatalf("add workspace member: %v", err)
	}
}

func TestDescriptionSearchIsolatesPrivatePagesButShowsOwner(t *testing.T) {
	s := testServer(t)
	admin, _ := signedIn(t, s, "description-admin@example.test")
	ws := makeWorkspace(t, s, admin)
	alice, aliceCookie := signedIn(t, s, "description-alice@example.test")
	bob, bobCookie := signedIn(t, s, "description-bob@example.test")
	addWorkspaceMember(t, s, workspaceMember{ws, alice})
	addWorkspaceMember(t, s, workspaceMember{ws, bob})
	pageID := "private-description"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{pageID, "Alice private", "private-only-description", ws, alice, "private"})

	if ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"private-only-description", map[string]string{"Cookie": bobCookie}})); ids[pageID] {
		t.Fatalf("same-workspace member found another owner's private description: %v", ids)
	}
	if ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"private-only-description", map[string]string{"Cookie": aliceCookie}})); !ids[pageID] {
		t.Fatalf("page owner could not find private description: %v", ids)
	}
}

func TestDescriptionSearchHonorsTokenWorkspaceScope(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "description-token@example.test")
	wsAllowed := s.firstWorkspaceOf(t, uid)
	wsDenied := makeWorkspace(t, s, uid)
	allowedID := "scoped-allowed"
	deniedID := "scoped-denied"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{allowedID, "Allowed", "workspace-scope-description", wsAllowed, uid, "workspace"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{deniedID, "Denied", "workspace-scope-description", wsDenied, uid, "workspace"})

	rawToken := "description-scope-token"
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, workspace_scope, created_at)
		VALUES (?, ?, 'description search', ?, 'read', ?, ?)`, newID(), uid, tokenHash(rawToken), wsAllowed, now()); err != nil {
		t.Fatalf("insert scoped token: %v", err)
	}
	ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"workspace-scope-description", map[string]string{"Authorization": "Bearer " + rawToken}}))
	if !ids[allowedID] {
		t.Fatalf("scoped token missed allowed workspace page: %v", ids)
	}
	if ids[deniedID] {
		t.Fatalf("scoped token found denied workspace page: %v", ids)
	}
}

func TestDescriptionSearchOversamplesInaccessibleHits(t *testing.T) {
	s := testServer(t)
	alice, _ := signedIn(t, s, "description-oversample-alice@example.test")
	bob, bobCookie := signedIn(t, s, "description-oversample-bob@example.test")
	ws := s.firstWorkspaceOf(t, alice)
	addWorkspaceMember(t, s, workspaceMember{ws, bob})
	for i := 0; i < 60; i++ {
		seedDescriptionSearchPage(t, s, descriptionSearchPage{newID(), "Private rank", strings.Repeat("oversample-description ", 6), ws, alice, "private"})
	}
	accessibleID := "accessible-after-private-ranks"
	seedDescriptionSearchPage(t, s, descriptionSearchPage{accessibleID, "Accessible rank", "oversample-description", ws, bob, "workspace"})

	ids := resultIDs(searchDescriptionPages(t, s, descriptionSearchRequest{"oversample-description", map[string]string{"Cookie": bobCookie}}))
	if !ids[accessibleID] {
		t.Fatalf("accessible description disappeared behind inaccessible ranked hits: %v", ids)
	}
}
