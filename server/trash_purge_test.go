package server

import (
	"testing"
	"time"
)

// The trash empties itself after trashRetentionDays, on a 30-minute tick that
// nobody watches. That made it the worst possible place for the one thing it
// got wrong: it deleted rows out of pages and left their words in the search
// index, because a virtual table has no foreign keys and nothing about
// `DELETE FROM pages` suggests an index is involved.
//
// The leak was invisible for as long as the index stored its own copy of the
// text — searchChunks JOINs against pages and silently drops what it cannot
// resolve, so the entries only accumulated. With chunks_fts reading its content
// out of page_chunks by an INTEGER PRIMARY KEY that SQLite hands to the next
// insert, they stop being inert: an orphan from a purged page starts describing
// whatever row takes its number.

// oldTrash inserts a page that has been in the trash long enough to be purged.
func oldTrash(t *testing.T, s *Server, ws, uid, id, title, body string, days int) {
	t.Helper()
	indexedPage(t, s, ws, uid, id, title, body)
	when := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE pages SET trashed_at = ? WHERE id = ?`, when, id); err != nil {
		t.Fatalf("trash %s: %v", id, err)
	}
}

func TestAutoPurgeTakesTheSearchIndexWithIt(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)

	oldTrash(t, s, ws, uid, "old", "Long gone", "A paragraph about interest rate swaps.", 60)
	indexedPage(t, s, ws, uid, "live", "Still here", "A paragraph about lunch.")

	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	n, err := s.PurgeTrashedBefore(cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d pages, want 1", n)
	}

	if got := termHits(t, s, "swaps"); got != 0 {
		t.Errorf("the purged page's words are still in the index (%d hits)", got)
	}
	var inPagesFTS int
	s.db.QueryRow(`SELECT COUNT(*) FROM pages_fts WHERE id = 'old'`).Scan(&inPagesFTS)
	if inPagesFTS != 0 {
		t.Errorf("the purged page is still in pages_fts")
	}
	if got := termHits(t, s, "lunch"); got != 1 {
		t.Errorf("the surviving page lost its index entry (%d hits, want 1)", got)
	}
}

// What is inside the window stays — a purge that took a page trashed yesterday
// would be destroying work somebody can still ask for back.
func TestAutoPurgeLeavesRecentTrashAlone(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	oldTrash(t, s, ws, uid, "recent", "Trashed on Tuesday", "Something deleted last week.", 5)

	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	if n, err := s.PurgeTrashedBefore(cutoff); err != nil || n != 0 {
		t.Fatalf("purged %d pages (err %v) — the trash is not a shredder", n, err)
	}
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM pages WHERE id = 'recent'`).Scan(&left)
	if left != 1 {
		t.Error("a page trashed five days ago was permanently deleted")
	}
}

// The DELETE cascades through parent_id, so a child the cutoff never selected
// disappears with its parent. Its index entries have to go too, and they are
// the easiest ones to miss: nothing in the statement mentions them.
func TestAutoPurgeFollowsTheCascadeIntoChildren(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)

	oldTrash(t, s, ws, uid, "parent", "Old project", "The project overview.", 60)
	indexedPage(t, s, ws, uid, "child", "Sub-page", "A note about debentures.")
	// Trashed at the same moment but recorded a day later, so the cutoff picks
	// up the parent and not the child — the cascade is what removes it.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE pages SET parent_id = 'parent', trashed_at = ? WHERE id = 'child'`, yesterday); err != nil {
		t.Fatalf("reparent: %v", err)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	if _, err := s.PurgeTrashedBefore(cutoff); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var childLeft int
	s.db.QueryRow(`SELECT COUNT(*) FROM pages WHERE id = 'child'`).Scan(&childLeft)
	if childLeft != 0 {
		t.Fatalf("the cascade did not reach the child — this test is not testing what it thinks")
	}
	if got := termHits(t, s, "debentures"); got != 0 {
		t.Errorf("the cascaded child's words survived in the index (%d hits)", got)
	}
}
