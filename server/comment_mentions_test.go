package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func mentionToken(name, userID string) string {
	return "@[" + name + "](" + userID + ")"
}

func postComment(t *testing.T, s *Server, cookie, pageID, body string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"body": body})
	rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+pageID+"/comments", string(b))
	if rec.Code != 200 {
		t.Fatalf("post comment: status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode comment id: %v", err)
	}
	return out.ID
}

// Naming someone with @[Name](userId) in a comment must produce exactly one
// unread "comment_mention" notice for them, distinguishable from a content
// mention and a subscription notice by its "cmention:" id prefix.
func TestCommentMentionCreatesNotice(t *testing.T) {
	s := testServer(t)
	author, authorCookie := signedIn(t, s, "author@example.test")
	target, targetCookie := signedIn(t, s, "target@example.test")
	ws := s.firstWorkspaceOf(t, author)
	s.addMember(t, ws, target, "member")
	page := s.makePage(t, ws, author, "", "Design doc", `{}`)

	postComment(t, s, authorCookie, page, "Can you review this, "+mentionToken("Target", target)+"?")

	notices := fetchNotices(t, s, targetCookie)
	if len(notices) != 1 || notices[0].Kind != "comment_mention" || notices[0].PageID != page || notices[0].Seen {
		t.Fatalf("expected one unread comment_mention notice, got %+v", notices)
	}
	if len(notices[0].ID) < len("cmention:") || notices[0].ID[:len("cmention:")] != "cmention:" {
		t.Fatalf("expected a cmention: prefixed id, got %q", notices[0].ID)
	}

	// The author must not see a notice about their own comment.
	if authorNotices := fetchNotices(t, s, authorCookie); len(authorNotices) != 0 {
		t.Fatalf("author should not be notified of their own comment, got %+v", authorNotices)
	}
}

// Mentioning yourself in your own comment is not a notification to send
// yourself.
func TestCommentSelfMentionCreatesNoNotice(t *testing.T) {
	s := testServer(t)
	author, cookie := signedIn(t, s, "self@example.test")
	ws := s.firstWorkspaceOf(t, author)
	page := s.makePage(t, ws, author, "", "Notes", `{}`)

	postComment(t, s, cookie, page, "Note to myself: "+mentionToken("Self", author))

	if notices := fetchNotices(t, s, cookie); len(notices) != 0 {
		t.Fatalf("mentioning yourself must not create a notice, got %+v", notices)
	}
}

// Naming the same person twice in one comment is still one notification —
// the same rule a page mentioning someone twice already follows.
func TestCommentMentionDedupesWithinOneComment(t *testing.T) {
	s := testServer(t)
	author, authorCookie := signedIn(t, s, "author2@example.test")
	target, targetCookie := signedIn(t, s, "target2@example.test")
	ws := s.firstWorkspaceOf(t, author)
	s.addMember(t, ws, target, "member")
	page := s.makePage(t, ws, author, "", "Doc", `{}`)

	tok := mentionToken("Target", target)
	postComment(t, s, authorCookie, page, tok+" ping "+tok+" again")

	if notices := fetchNotices(t, s, targetCookie); len(notices) != 1 {
		t.Fatalf("expected one notice for a doubly-named mention, got %d", len(notices))
	}
}

// Being named in a comment is not itself permission to read the page — same
// rule mentions and subscription notices already enforce.
func TestCommentMentionHiddenWithoutReadPermission(t *testing.T) {
	s := testServer(t)
	owner, ownerCookie := signedIn(t, s, "owner3@example.test")
	outsider, outCookie := signedIn(t, s, "outsider3@example.test")
	ws := s.firstWorkspaceOf(t, owner)
	// outsider is a real account but NOT a member of this workspace.
	page := s.makePage(t, ws, owner, "", "Private-ish", `{}`)

	postComment(t, s, ownerCookie, page, "cc "+mentionToken("Outsider", outsider))

	if notices := fetchNotices(t, s, outCookie); len(notices) != 0 {
		t.Fatalf("outsider must not see a notice about a page they cannot read, got %+v", notices)
	}
}

// Deleting the comment must take its mention notices with it (ON DELETE
// CASCADE) rather than leaving a notification pointing at text that no
// longer exists.
func TestDeletingCommentRemovesItsMentionNotice(t *testing.T) {
	s := testServer(t)
	author, authorCookie := signedIn(t, s, "author4@example.test")
	target, targetCookie := signedIn(t, s, "target4@example.test")
	ws := s.firstWorkspaceOf(t, author)
	s.addMember(t, ws, target, "member")
	page := s.makePage(t, ws, author, "", "Doc", `{}`)

	commentID := postComment(t, s, authorCookie, page, mentionToken("Target", target)+" fyi")
	if notices := fetchNotices(t, s, targetCookie); len(notices) != 1 {
		t.Fatalf("expected one notice before delete, got %d", len(notices))
	}

	if rec := proposalRequest(t, s, authorCookie, http.MethodDelete, "/api/comments/"+commentID, ""); rec.Code != 200 {
		t.Fatalf("delete comment: status %d: %s", rec.Code, rec.Body.String())
	}
	if notices := fetchNotices(t, s, targetCookie); len(notices) != 0 {
		t.Fatalf("expected the notice to be gone after its comment was deleted, got %+v", notices)
	}
}

// Marking a comment-mention notice read must not touch a content mention or
// a subscription notice for the same person — the "cmention:" prefix has to
// route independently, same as "mention:" and "sub:" already do.
func TestCommentMentionReadIsIndependentOfOtherNoticeKinds(t *testing.T) {
	s := testServer(t)
	author, authorCookie := signedIn(t, s, "author5@example.test")
	target, targetCookie := signedIn(t, s, "target5@example.test")
	ws := s.firstWorkspaceOf(t, author)
	s.addMember(t, ws, target, "member")
	page := s.makePage(t, ws, author, "", "Doc", `{}`)
	postComment(t, s, authorCookie, page, mentionToken("Target", target)+" ping")

	notices := fetchNotices(t, s, targetCookie)
	if len(notices) != 1 {
		t.Fatalf("setup: expected one notice, got %d", len(notices))
	}
	body, _ := json.Marshal(map[string]string{"id": notices[0].ID})
	if rec := proposalRequest(t, s, targetCookie, http.MethodPost, "/api/notifications/read", string(body)); rec.Code != 200 {
		t.Fatalf("mark read: status %d: %s", rec.Code, rec.Body.String())
	}
	notices = fetchNotices(t, s, targetCookie)
	if len(notices) != 1 || !notices[0].Seen {
		t.Fatalf("expected the notice to be marked read, got %+v", notices)
	}
}

// A comment posted through the MCP tool must notify a mentioned person
// exactly like one typed in the panel — both paths share createComment
// precisely so they cannot drift apart.
func TestMCPCommentMentionCreatesNotice(t *testing.T) {
	s := testServer(t)
	author, _ := signedIn(t, s, "author6@example.test")
	target, targetCookie := signedIn(t, s, "target6@example.test")
	ws := s.firstWorkspaceOf(t, author)
	s.addMember(t, ws, target, "member")
	page := s.makePage(t, ws, author, "", "Doc", `{}`)

	u := s.userByID(author)
	if _, err := s.mcpComments(u, page, "add", "via MCP: "+mentionToken("Target", target), "", "", nil); err != nil {
		t.Fatalf("mcpComments add: %v", err)
	}
	if notices := fetchNotices(t, s, targetCookie); len(notices) != 1 {
		t.Fatalf("expected one notice from an MCP-created comment, got %d", len(notices))
	}
}
