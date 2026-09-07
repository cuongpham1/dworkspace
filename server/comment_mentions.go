package server

import (
	"regexp"
)

// @-mentions inside comments. Comments are plain text (see the "comments"
// table), not BlockNote JSON like page content, so there is no {"type":
// "mention"} node to walk — the composer instead inserts a small inline
// token, @[Display Name](userId), when someone is picked from the "@" list.
// commentMentionRe is the one place that token format is decoded.
var commentMentionRe = regexp.MustCompile(`@\[[^\]]*\]\(([A-Za-z0-9_-]+)\)`)

// extractCommentMentionIDs returns the distinct user ids named in a comment
// body, in first-occurrence order. Being named twice in one comment is still
// one notification — same rule as a page mentioning someone twice (see
// extractMentions in mentions.go).
func extractCommentMentionIDs(body string) []string {
	matches := commentMentionRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		id := m[1]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// createComment is the one place a comment is actually inserted — used by
// both the REST handler and the MCP "comments" tool, so an agent adding a
// comment over MCP notifies a mentioned person exactly like a human typing in
// the panel does, rather than the two paths quietly drifting apart.
func (s *Server) createComment(pageID, blockID, authorID, authorName, body string) (string, error) {
	id := newID()
	if _, err := s.db.Exec(`INSERT INTO comments (id, page_id, block_id, author_id, author_name, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, pageID, blockID, authorID, authorName, body, now()); err != nil {
		return "", err
	}
	s.recordCommentMentions(id, pageID, blockID, authorID, body)
	return id, nil
}

// recordCommentMentions inserts one comment_mentions row per person named in
// the comment — except the author themself: mentioning your own name in your
// own comment is not a notification to send yourself. Errors are swallowed
// (best-effort, same as the rest of the mention/notification plumbing) so a
// malformed or unknown id in the token never blocks the comment itself from
// being posted.
func (s *Server) recordCommentMentions(commentID, pageID, blockID, authorID, body string) {
	ids := extractCommentMentionIDs(body)
	if len(ids) == 0 {
		return
	}
	ts := now()
	for _, uid := range ids {
		if uid == authorID {
			continue
		}
		// Only a real, still-existing account — a stale id left in an old
		// comment's token must not sit in nobody's unread count forever.
		var exists int
		if s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, uid).Scan(&exists); exists == 0 {
			continue
		}
		s.db.Exec(`INSERT INTO comment_mentions (id, comment_id, page_id, user_id, block_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			newID(), commentID, pageID, uid, blockID, ts)
	}
}

// commentMentionNotice mirrors mentionNotice's and subscriptionNotice's shape
// (see mentions.go, subscriptions.go) so the bell can merge all three kinds
// into one chronologically sorted list. Unlike a content mention (keyed
// page+user, one row resynced from the whole page on every save), this is
// keyed by the individual comment — two different comments mentioning the
// same person on the same page are two real, separate notices.
type commentMentionNotice struct {
	ID      string `json:"id"`
	PageID  string `json:"pageId"`
	BlockID string `json:"blockId"`
	Title   string `json:"title"`
	Icon    string `json:"icon"`
	Author  string `json:"authorName"`
	Body    string `json:"body"`
	At      string `json:"at"`
	Seen    bool   `json:"seen"`
}

func (s *Server) listCommentMentionNotices(userID string) []commentMentionNotice {
	rows, err := s.db.Query(`
		SELECT cm.id, cm.page_id, cm.block_id, p.title, COALESCE(p.icon, ''),
		       c.author_name, c.body, cm.created_at, cm.seen_at
		FROM comment_mentions cm
		JOIN pages p ON p.id = cm.page_id AND p.trashed_at IS NULL
		JOIN comments c ON c.id = cm.comment_id
		WHERE cm.user_id = ?
		ORDER BY cm.created_at DESC
		LIMIT 100`, userID)
	if err != nil {
		return nil
	}
	// Drain BEFORE the canRead calls below — the same rule this codebase keeps
	// re-documenting at every one of these joins (mentions.go, subscriptions.go,
	// derived.go): a query issued while this cursor is still open blocks the
	// whole server behind the single shared connection.
	type row struct {
		id, pageID, block, title, icon, author, body, at string
		seen                                             *string
	}
	var raw []row
	for rows.Next() {
		var it row
		if rows.Scan(&it.id, &it.pageID, &it.block, &it.title, &it.icon, &it.author, &it.body, &it.at, &it.seen) == nil {
			raw = append(raw, it)
		}
	}
	rows.Close()

	out := []commentMentionNotice{}
	for _, it := range raw {
		// Being mentioned is not itself permission to read — same rule as a
		// content mention: the page may have moved workspace, or gone private,
		// since the comment was posted.
		if !s.canRead(userID, it.pageID) {
			continue
		}
		title := it.title
		if title == "" {
			title = "Untitled"
		}
		out = append(out, commentMentionNotice{
			ID: "cmention:" + it.id, PageID: it.pageID, BlockID: it.block,
			Title: title, Icon: it.icon, Author: it.author, Body: it.body,
			At: it.at, Seen: it.seen != nil,
		})
	}
	return out
}

func (s *Server) markCommentMentionNoticeRead(userID, noticeID string) error {
	_, err := s.db.Exec(`UPDATE comment_mentions SET seen_at = ? WHERE id = ? AND user_id = ? AND seen_at IS NULL`,
		now(), noticeID, userID)
	return err
}

func (s *Server) markAllCommentMentionNoticesRead(userID string) error {
	_, err := s.db.Exec(`UPDATE comment_mentions SET seen_at = ? WHERE user_id = ? AND seen_at IS NULL`, now(), userID)
	return err
}
