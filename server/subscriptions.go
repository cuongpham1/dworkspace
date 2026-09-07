package server

import (
	"net/http"
	"strings"
)

// Following a page (a Feature/Metric profile, or any document somebody cares
// about) and being told when something new links to it. Distinct from
// favorites: a favorite is a shortcut to somewhere YOU go often; a
// subscription is a promise that something ELSE will come tell you.
//
// Distinct from mentions, too, and for a real reason rather than just naming:
// a mention is (page, user) — being named twice on the same page is still one
// notification. A subscription notice is (subscriber, NEW source page) — two
// different new documents linking to the same watched Feature are two
// separate, real, useful notices, and collapsing them the way mentions
// collapse re-saves would silently drop all but the first. See
// subscription_notices in db.go.

func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID := requestUser(r).ID
	rows, err := s.db.Query(`
		SELECT s.page_id FROM page_subscriptions s
		JOIN pages p ON p.id = s.page_id AND p.trashed_at IS NULL
		WHERE s.user_id = ?`, userID)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	// Drain BEFORE the canRead calls below: on a single database connection a
	// query issued inside an open cursor blocks the server (same rule
	// handleCommentCounts and listMentionNotices already document — missed
	// here once, and it hung the whole instance behind the one shared
	// connection until a restart cleared it).
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := []string{}
	for _, id := range ids {
		if s.canRead(userID, id) {
			out = append(out, id)
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	pageID := r.PathValue("id")
	if !s.canReadReq(r, pageID) {
		httpError(w, 404, "page not found")
		return
	}
	if _, err := s.db.Exec(`INSERT INTO page_subscriptions (page_id, user_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT(page_id, user_id) DO NOTHING`, pageID, requestUser(r).ID, now()); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(`DELETE FROM page_subscriptions WHERE page_id = ? AND user_id = ?`,
		r.PathValue("id"), requestUser(r).ID); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// notifySubscribers is called from reindexPage with the outgoing-link targets
// a page had before this save and after. Only a target that is NEW — present
// after, absent before — creates a notice: an unrelated edit to a page that
// already linked here must not re-notify everyone who follows it on every
// save, the same failure mode the mentions table's ON CONFLICT guards
// against, solved here by only acting on the diff instead of on every write.
func (s *Server) notifySubscribers(sourceID string, oldTargets, newTargets []string) {
	old := map[string]bool{}
	for _, t := range oldTargets {
		old[t] = true
	}
	added := map[string]bool{}
	for _, t := range newTargets {
		if !old[t] && !added[t] {
			added[t] = true
			s.notifySubscribersOfTarget(sourceID, t)
		}
	}
}

func (s *Server) notifySubscribersOfTarget(sourceID, targetID string) {
	rows, err := s.db.Query(`SELECT user_id FROM page_subscriptions WHERE page_id = ?`, targetID)
	if err != nil {
		return
	}
	var subscribers []string
	for rows.Next() {
		var uid string
		if rows.Scan(&uid) == nil {
			subscribers = append(subscribers, uid)
		}
	}
	rows.Close()
	ts := now()
	for _, uid := range subscribers {
		s.db.Exec(`INSERT INTO subscription_notices (id, page_id, source_id, user_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			newID(), targetID, sourceID, uid, ts)
	}
}

// subscriptionNotice mirrors mentionNotice's shape (see mentions.go) so the
// bell can render one merged, chronologically sorted list without a client
// side union type — ID carries which table a read-receipt belongs to.
type subscriptionNotice struct {
	ID          string `json:"id"`
	PageID      string `json:"pageId"` // the NEW document — where a click goes
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	FollowedID  string `json:"followedId"`
	FollowedTtl string `json:"followedTitle"`
	At          string `json:"at"`
	Seen        bool   `json:"seen"`
}

func (s *Server) listSubscriptionNotices(userID string) []subscriptionNotice {
	rows, err := s.db.Query(`
		SELECT n.id, n.source_id, src.title, COALESCE(src.icon, ''),
		       n.page_id, followed.title, n.created_at, n.seen_at
		FROM subscription_notices n
		JOIN pages src ON src.id = n.source_id AND src.trashed_at IS NULL
		JOIN pages followed ON followed.id = n.page_id AND followed.trashed_at IS NULL
		WHERE n.user_id = ?
		ORDER BY n.created_at DESC
		LIMIT 100`, userID)
	if err != nil {
		return nil
	}
	type row struct {
		id, sourceID, sourceTitle, sourceIcon, followedID, followedTitle, at string
		seen                                                                 *string
	}
	var raw []row
	for rows.Next() {
		var it row
		if rows.Scan(&it.id, &it.sourceID, &it.sourceTitle, &it.sourceIcon,
			&it.followedID, &it.followedTitle, &it.at, &it.seen) == nil {
			raw = append(raw, it)
		}
	}
	rows.Close()

	out := []subscriptionNotice{}
	for _, it := range raw {
		// Being subscribed is not itself permission to read the new document —
		// same rule as mentions: it may have moved to a workspace, or a private
		// subtree, this account cannot see.
		if !s.canRead(userID, it.sourceID) {
			continue
		}
		title := it.sourceTitle
		if title == "" {
			title = "Untitled"
		}
		followedTitle := it.followedTitle
		if followedTitle == "" {
			followedTitle = "Untitled"
		}
		out = append(out, subscriptionNotice{
			ID: "sub:" + it.id, PageID: it.sourceID, Title: title, Icon: it.sourceIcon,
			FollowedID: it.followedID, FollowedTtl: followedTitle,
			At: it.at, Seen: it.seen != nil,
		})
	}
	return out
}

// markSubscriptionNoticeRead marks one notice (id without the "sub:" prefix)
// read, scoped to the caller — a stranger's notice id must not let them
// silence someone else's badge.
func (s *Server) markSubscriptionNoticeRead(userID, noticeID string) error {
	_, err := s.db.Exec(`UPDATE subscription_notices SET seen_at = ? WHERE id = ? AND user_id = ? AND seen_at IS NULL`,
		now(), noticeID, userID)
	return err
}

func (s *Server) markAllSubscriptionNoticesRead(userID string) error {
	_, err := s.db.Exec(`UPDATE subscription_notices SET seen_at = ? WHERE user_id = ? AND seen_at IS NULL`, now(), userID)
	return err
}

// stripPrefix is a tiny local helper so subscriptions.go does not need to pull
// in strconv/strings choices beyond what it already uses.
func stripPrefix(id, prefix string) (string, bool) {
	if strings.HasPrefix(id, prefix) {
		return id[len(prefix):], true
	}
	return "", false
}
