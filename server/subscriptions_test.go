package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func pageLinkContent(targetID string) string {
	b, _ := json.Marshal([]map[string]any{{
		"type": "paragraph",
		"content": []map[string]any{{
			"type": "pageLink", "props": map[string]any{"pageId": targetID},
		}},
	}})
	return string(b)
}

func setContentAndReindex(t *testing.T, s *Server, pageID, content string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, content, pageID); err != nil {
		t.Fatalf("set content: %v", err)
	}
	if err := s.reindexPage(pageID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
}

type testNotice struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	PageID     string `json:"pageId"`
	FollowedID string `json:"followedId"`
	Seen       bool   `json:"seen"`
}

func fetchNotices(t *testing.T, s *Server, cookie string) []testNotice {
	t.Helper()
	rec := proposalRequest(t, s, cookie, http.MethodGet, "/api/notifications", "")
	if rec.Code != 200 {
		t.Fatalf("notifications: status %d: %s", rec.Code, rec.Body.String())
	}
	var out []testNotice
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode notifications: %v", err)
	}
	return out
}

// A subscription notice fires only for a link that is genuinely NEW —
// re-saving a page that already linked to a followed target must not spam a
// fresh notice on every edit, and two DIFFERENT sources linking the same
// target must each get their own (unlike mentions, which collapse by page).
func TestSubscriptionNoticeOnlyOnNewLink(t *testing.T) {
	s := testServer(t)
	subscriber, subCookie := signedIn(t, s, "subscriber@example.test")
	ws := s.firstWorkspaceOf(t, subscriber)
	feature := s.makePage(t, ws, subscriber, "", "Feature X", `{}`)

	if rec := proposalRequest(t, s, subCookie, http.MethodPost, "/api/pages/"+feature+"/subscribe", ""); rec.Code != 200 {
		t.Fatalf("subscribe: status %d: %s", rec.Code, rec.Body.String())
	}

	source1 := s.makePage(t, ws, subscriber, "", "New Spec", `{}`)
	setContentAndReindex(t, s, source1, pageLinkContent(feature))

	notices := fetchNotices(t, s, subCookie)
	if len(notices) != 1 || notices[0].Kind != "subscription" || notices[0].PageID != source1 || notices[0].FollowedID != feature {
		t.Fatalf("expected one subscription notice for source1, got %+v", notices)
	}

	// Re-saving the SAME link must not create a second notice.
	setContentAndReindex(t, s, source1, pageLinkContent(feature))
	if notices := fetchNotices(t, s, subCookie); len(notices) != 1 {
		t.Fatalf("re-saving an unchanged link must not duplicate the notice, got %d", len(notices))
	}

	// A second, DIFFERENT source linking the same followed page is a second,
	// real notice — proposals collapse by (page,user); this must not.
	source2 := s.makePage(t, ws, subscriber, "", "Another Spec", `{}`)
	setContentAndReindex(t, s, source2, pageLinkContent(feature))
	notices = fetchNotices(t, s, subCookie)
	if len(notices) != 2 {
		t.Fatalf("expected 2 distinct notices from 2 different sources, got %d: %+v", len(notices), notices)
	}
}

// Being subscribed is not itself permission to read the new document — the
// same rule mentions already follow. A private page's own author subscribing
// nobody else can read still must not leak it to a subscriber without access.
func TestSubscriptionNoticeHiddenWithoutReadPermission(t *testing.T) {
	s := testServer(t)
	owner, _ := signedIn(t, s, "owner2@example.test")
	outsider, outCookie := signedIn(t, s, "outsider2@example.test")
	ws := s.firstWorkspaceOf(t, owner)
	s.addMember(t, ws, outsider, "member")

	feature := s.makePage(t, ws, owner, "", "Feature Y", `{}`)
	if _, err := s.db.Exec(`INSERT INTO page_subscriptions (page_id, user_id, created_at) VALUES (?, ?, ?)`,
		feature, outsider, now()); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	source := s.makePage(t, ws, owner, "", "Private Spec", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET visibility = 'private' WHERE id = ?`, source); err != nil {
		t.Fatalf("make private: %v", err)
	}
	setContentAndReindex(t, s, source, pageLinkContent(feature))

	if notices := fetchNotices(t, s, outCookie); len(notices) != 0 {
		t.Fatalf("outsider must not see a notice about a page they cannot read, got %+v", notices)
	}
}

// Marking one notice read must not touch the other kind, or a different
// notice of the same kind — this is what the "mention:"/"sub:" id prefix
// exists to make possible.
func TestNotificationsReadIsPerNoticeNotPerKind(t *testing.T) {
	s := testServer(t)
	subscriber, subCookie := signedIn(t, s, "reader@example.test")
	ws := s.firstWorkspaceOf(t, subscriber)
	feature := s.makePage(t, ws, subscriber, "", "Feature Z", `{}`)
	proposalRequest(t, s, subCookie, http.MethodPost, "/api/pages/"+feature+"/subscribe", "")

	source := s.makePage(t, ws, subscriber, "", "Spec Z", `{}`)
	setContentAndReindex(t, s, source, pageLinkContent(feature))

	notices := fetchNotices(t, s, subCookie)
	if len(notices) != 1 || notices[0].Seen {
		t.Fatalf("expected one unread notice, got %+v", notices)
	}
	noticeID := notices[0].ID

	body, _ := json.Marshal(map[string]string{"id": noticeID})
	if rec := proposalRequest(t, s, subCookie, http.MethodPost, "/api/notifications/read", string(body)); rec.Code != 200 {
		t.Fatalf("mark read: status %d: %s", rec.Code, rec.Body.String())
	}
	notices = fetchNotices(t, s, subCookie)
	if len(notices) != 1 || !notices[0].Seen {
		t.Fatalf("expected the notice to be marked read, got %+v", notices)
	}
}

// GET /api/subscriptions must actually return what was subscribed — the
// regression test for a real production incident: canRead was called INSIDE
// the rows.Next() loop of the listing query, and with SetMaxOpenConns(1) that
// second query blocked forever behind the still-open first cursor, hanging
// every DB-touching request on the instance until the container was
// restarted. Not caught by other tests because none of them called this
// endpoint with a real row to iterate over.
func TestListSubscriptionsReturnsSubscribedPages(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "listsub@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	feature := s.makePage(t, ws, uid, "", "Feature V", `{}`)

	if rec := proposalRequest(t, s, cookie, http.MethodPost, "/api/pages/"+feature+"/subscribe", ""); rec.Code != 200 {
		t.Fatalf("subscribe: status %d: %s", rec.Code, rec.Body.String())
	}
	rec := proposalRequest(t, s, cookie, http.MethodGet, "/api/subscriptions", "")
	if rec.Code != 200 {
		t.Fatalf("list subscriptions: status %d: %s", rec.Code, rec.Body.String())
	}
	var ids []string
	if err := json.Unmarshal(rec.Body.Bytes(), &ids); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ids) != 1 || ids[0] != feature {
		t.Fatalf("expected [%q], got %v", feature, ids)
	}
}

// Unsubscribing stops FUTURE notices; it does not need to (and does not)
// retroactively delete ones already delivered.
func TestUnsubscribeStopsFutureNotices(t *testing.T) {
	s := testServer(t)
	subscriber, subCookie := signedIn(t, s, "unsub@example.test")
	ws := s.firstWorkspaceOf(t, subscriber)
	feature := s.makePage(t, ws, subscriber, "", "Feature W", `{}`)
	proposalRequest(t, s, subCookie, http.MethodPost, "/api/pages/"+feature+"/subscribe", "")

	if rec := proposalRequest(t, s, subCookie, http.MethodDelete, "/api/pages/"+feature+"/subscribe", ""); rec.Code != 200 {
		t.Fatalf("unsubscribe: status %d: %s", rec.Code, rec.Body.String())
	}

	source := s.makePage(t, ws, subscriber, "", "Late Spec", `{}`)
	setContentAndReindex(t, s, source, pageLinkContent(feature))

	if notices := fetchNotices(t, s, subCookie); len(notices) != 0 {
		t.Fatalf("no notice should fire after unsubscribing, got %+v", notices)
	}
}
