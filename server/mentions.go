package server

import (
	"encoding/json"
	"net/http"
	"sort"
)

// User mentions. An "@name" in a page body is stored inside BlockNote content
// as {"type":"mention","props":{"userId":"…","label":"…"}} — the person
// equivalent of pageLink (see links.go). The mentions table is derived from
// that content on every reindex, plus one bit of real state: whether the
// mentioned person has seen it.
//
// Why a derived table and not an append-only event log: a page is
// re-materialized on nearly every editing burst, so an append-only log would
// grow one "you were mentioned" per save. Keying on (page_id, user_id) makes
// the sync idempotent — the same mention re-seen on the tenth save is the same
// row, not the tenth notification.

// mentionRef is one person named on a page, and the block they were named in.
// The block is what lets the notification scroll to the sentence rather than
// dropping the reader at the top of a long page to hunt for their own name.
type mentionRef struct {
	userID  string
	blockID string
}

// extractMentions walks the content and returns one ref per mentioned account.
// It carries the id of the nearest enclosing BLOCK down the walk: a mention is
// inline content, so the id lives on its parent, not on the mention itself.
// First mention wins — being named twice on a page is still one notification,
// and the first occurrence is the one worth scrolling to.
func extractMentions(content []byte) []mentionRef {
	var v any
	if json.Unmarshal(content, &v) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []mentionRef
	var walk func(n any, block string)
	walk = func(n any, block string) {
		switch t := n.(type) {
		case map[string]any:
			// A block carries its own id; inline content does not. Only descend
			// with a NEW block id when this node actually looks like a block,
			// so a nested structure cannot pin the mention to the wrong one.
			if id, ok := t["id"].(string); ok && id != "" {
				block = id
			}
			if t["type"] == "mention" {
				if props, ok := t["props"].(map[string]any); ok {
					if uid, ok := props["userId"].(string); ok && uid != "" && !seen[uid] {
						seen[uid] = true
						out = append(out, mentionRef{userID: uid, blockID: block})
					}
				}
			}
			for _, val := range t {
				walk(val, block)
			}
		case []any:
			for _, val := range t {
				walk(val, block)
			}
		}
	}
	walk(v, "")
	return out
}

// updateMentions brings the mentions of one page in line with its content.
//
// Adds are INSERT OR IGNORE (an existing mention keeps its first_seen_at and
// its seen_at — re-saving the page must not mark a read notification unread
// again). Removals are deleted, so taking an "@name" back out of a page also
// takes the notification away rather than leaving a pointer to text that is no
// longer there. A trashed page mentions nobody.
func (s *Server) updateMentions(pageID, content string, trashed bool) {
	var want []mentionRef
	if !trashed {
		want = extractMentions([]byte(content))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	keep := map[string]bool{}
	ts := now()
	for _, m := range want {
		// Only real accounts: a stale label left behind by a deleted user would
		// otherwise sit in somebody's unread count forever with no way to open it.
		var exists int
		if tx.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, m.userID).Scan(&exists); exists == 0 {
			continue
		}
		keep[m.userID] = true
		// block_id is the one field that MAY change on a re-sync: editing above
		// the mention gives its paragraph a different id, and the stored pointer
		// has to follow or the scroll lands nowhere. first_seen_at and seen_at
		// are deliberately left alone — moving a mention is not a new mention,
		// and must not turn a read notification unread again.
		tx.Exec(`INSERT INTO mentions (page_id, user_id, block_id, first_seen_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (page_id, user_id) DO UPDATE SET block_id = excluded.block_id`,
			pageID, m.userID, m.blockID, ts)
	}
	rows, err := tx.Query(`SELECT user_id FROM mentions WHERE page_id = ?`, pageID)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var uid string
		if rows.Scan(&uid) == nil && !keep[uid] {
			stale = append(stale, uid)
		}
	}
	rows.Close()
	for _, uid := range stale {
		tx.Exec(`DELETE FROM mentions WHERE page_id = ? AND user_id = ?`, pageID, uid)
	}
	tx.Commit()
}

// mentionNotice's ID is always "mention:"+pageId — a mention is keyed
// (page, user), so the page id already identifies it uniquely, unlike a
// subscription notice (see subscriptions.go), which needs its own row id.
type mentionNotice struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	PageID  string `json:"pageId"`
	BlockID string `json:"blockId"`
	Title   string `json:"title"`
	Icon    string `json:"icon"`
	At      string `json:"at"`
	Seen    bool   `json:"seen"`
}

func (s *Server) listMentionNotices(me string) []mentionNotice {
	rows, err := s.db.Query(`
		SELECT m.page_id, COALESCE(m.block_id, ''), p.title, COALESCE(p.icon, ''),
		       m.first_seen_at, m.seen_at
		FROM mentions m JOIN pages p ON p.id = m.page_id
		WHERE m.user_id = ? AND p.trashed_at IS NULL
		ORDER BY m.first_seen_at DESC
		LIMIT 100`, me)
	if err != nil {
		return nil
	}
	// Drain BEFORE the canRead calls below: on a single database connection a
	// query issued inside an open cursor blocks the server (same rule the
	// search path documents).
	type row struct {
		id, block, title, icon, at string
		seen                       *string
	}
	var raw []row
	for rows.Next() {
		var it row
		if rows.Scan(&it.id, &it.block, &it.title, &it.icon, &it.at, &it.seen) == nil {
			raw = append(raw, it)
		}
	}
	rows.Close()

	out := []mentionNotice{}
	for _, it := range raw {
		// Being mentioned is not itself permission to read: the page may be
		// private, or have moved to a workspace this account is not in.
		if !s.canRead(me, it.id) {
			continue
		}
		title := it.title
		if title == "" {
			title = "Untitled"
		}
		out = append(out, mentionNotice{
			ID: "mention:" + it.id, Kind: "mention",
			PageID: it.id, BlockID: it.block, Title: title, Icon: it.icon,
			At: it.at, Seen: it.seen != nil,
		})
	}
	return out
}

// handleNotifications merges mentions, comment mentions, and
// page-subscription notices (see comment_mentions.go, subscriptions.go) into
// one chronologically sorted feed, newest first — one bell answers "did
// anybody need me?" (in a document, or in a comment) and "did something I
// follow just change?" rather than asking a person to check three separate
// places. Read entries stay in the list (a notification you have seen is
// still how you find the page again) but stop counting towards the badge.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	me := requestUser(r).ID
	mentions := s.listMentionNotices(me)
	cmentions := s.listCommentMentionNotices(me)
	subs := s.listSubscriptionNotices(me)

	type notice struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		PageID     string `json:"pageId"`
		BlockID    string `json:"blockId,omitempty"`
		Title      string `json:"title"`
		Icon       string `json:"icon"`
		Author     string `json:"authorName,omitempty"`
		Body       string `json:"body,omitempty"`
		FollowedID string `json:"followedId,omitempty"`
		Followed   string `json:"followedTitle,omitempty"`
		At         string `json:"at"`
		Seen       bool   `json:"seen"`
	}
	out := make([]notice, 0, len(mentions)+len(cmentions)+len(subs))
	for _, m := range mentions {
		out = append(out, notice{
			ID: m.ID, Kind: m.Kind, PageID: m.PageID, BlockID: m.BlockID,
			Title: m.Title, Icon: m.Icon, At: m.At, Seen: m.Seen,
		})
	}
	for _, cm := range cmentions {
		out = append(out, notice{
			ID: cm.ID, Kind: "comment_mention", PageID: cm.PageID, BlockID: cm.BlockID,
			Title: cm.Title, Icon: cm.Icon, Author: cm.Author, Body: cm.Body, At: cm.At, Seen: cm.Seen,
		})
	}
	for _, sn := range subs {
		out = append(out, notice{
			ID: sn.ID, Kind: "subscription", PageID: sn.PageID, Title: sn.Title, Icon: sn.Icon,
			FollowedID: sn.FollowedID, Followed: sn.FollowedTtl, At: sn.At, Seen: sn.Seen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	writeJSON(w, out)
}

// handleNotificationsRead marks one notification read by its prefixed id
// ("mention:<pageId>" or "sub:<noticeId>"), or clears every unread notice of
// both kinds when no id is given — the "mark all as read" a full list needs
// to be dismissable.
func (s *Server) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	// An empty body is the "all" case, so a decode failure is not an error.
	_ = decodeJSON(w, r, &body)
	me := requestUser(r).ID
	var err error
	switch {
	case body.ID == "":
		if _, e := s.db.Exec(`UPDATE mentions SET seen_at = ? WHERE user_id = ? AND seen_at IS NULL`, now(), me); e != nil {
			err = e
		}
		if e := s.markAllCommentMentionNoticesRead(me); e != nil {
			err = e
		}
		if e := s.markAllSubscriptionNoticesRead(me); e != nil {
			err = e
		}
	default:
		if pageID, ok := stripPrefix(body.ID, "mention:"); ok {
			_, err = s.db.Exec(
				`UPDATE mentions SET seen_at = ? WHERE user_id = ? AND page_id = ? AND seen_at IS NULL`,
				now(), me, pageID)
		} else if noticeID, ok := stripPrefix(body.ID, "cmention:"); ok {
			err = s.markCommentMentionNoticeRead(me, noticeID)
		} else if noticeID, ok := stripPrefix(body.ID, "sub:"); ok {
			err = s.markSubscriptionNoticeRead(me, noticeID)
		}
	}
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
