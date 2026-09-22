package server

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// i18n-ok-file: the sample text is German on purpose. This index folds
// diacritics (remove_diacritics 2, see searchindex.go), so the words being
// indexed and matched here have to carry them — "Kündigungsfristen" is stored
// as "kundigungsfristen", and an English fixture would exercise none of that.
//
// chunks_fts is an external-content FTS5 table: it holds the inverted index and
// nothing else, and reads the words back out of page_chunks whenever it needs
// them. That buys back a full duplicate copy of every page (76 MB on the
// instance this was measured against) at the cost of one invariant that a plain
// fts5 table enforced for free:
//
//	a row must leave the index BEFORE it leaves page_chunks,
//	and its old values have to be handed back with the deletion.
//
// Get that wrong and nothing fails loudly at first. The index keeps terms
// pointing at a seq with no row behind it; the JOIN in searchChunks quietly
// drops them, so the search still looks correct while the index grows without
// bound. It stops looking correct later: seq is an INTEGER PRIMARY KEY, so the
// next insert after the highest row was deleted REUSES that number — and the
// orphaned terms of the old passage now describe a completely different one.
// Somebody searches a word from a page they deleted last month and lands on a
// page that never contained it.
//
// FTS5's own integrity-check does not see any of this (verified: it passes with
// every delete path disabled — it validates the index's internal structure, not
// whether the index still describes the content table). So these tests assert
// on the terms themselves: after a word leaves the content, it must stop
// matching.

// termHits counts index entries matching one word.
func termHits(t *testing.T, s *Server, word string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, `"`+word+`"`).Scan(&n); err != nil {
		t.Fatalf("match %q: %v", word, err)
	}
	return n
}

// ftsIntact is the cheap structural check on top. It cannot catch a missed
// delete, but it does catch a corrupted index — worth a line at each step.
func ftsIntact(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO chunks_fts(chunks_fts, rank) VALUES('integrity-check', 0)`); err != nil {
		t.Fatalf("chunks_fts is corrupt: %v", err)
	}
}

// indexedPage inserts a page and runs the real indexing path over it.
func indexedPage(t *testing.T, s *Server, ws, uid, id, title, body string) {
	t.Helper()
	content := `[{"type":"paragraph","content":[{"type":"text","text":"` + body + `"}]}]`
	if _, err := s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, 'workspace')`, id, title, content, now(), now(), ws, uid); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	if err := s.reindexPage(id); err != nil {
		t.Fatalf("reindexPage: %v", err)
	}
}

// The ordinary life of a page: written, rewritten, trashed. Every one of those
// goes through reindexChunks, and every one of them deletes rows.
func TestIndexStaysInStepThroughEditAndTrash(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	indexedPage(t, s, ws, uid, "p1", "Kündigungsfristen", "Die Frist beträgt drei Monate zum Quartalsende.")
	ftsIntact(t, s)
	if termHits(t, s, "quartalsende") != 1 {
		t.Fatalf("the page was not indexed at all")
	}

	// A rewrite: the old passages go, new ones arrive, and the number of them
	// changes — so the delete cannot just be "the same rowids again".
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = 'p1'`,
		`[{"type":"heading","props":{"level":1},"content":[{"type":"text","text":"Fristen"}]},
		  {"type":"paragraph","content":[{"type":"text","text":"Ein ganz anderer Text."}]}]`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.reindexPage("p1"); err != nil {
		t.Fatalf("reindexPage: %v", err)
	}
	ftsIntact(t, s)
	if n := termHits(t, s, "quartalsende"); n != 0 {
		t.Errorf("a word deleted from the page still matches (%d hits)", n)
	}
	if n := termHits(t, s, "anderer"); n != 1 {
		t.Errorf("the rewritten text was not indexed (%d hits, want 1)", n)
	}

	// Trashed: reindexPage clears the passages so the page stops answering
	// searches, without the row going anywhere.
	if _, err := s.db.Exec(`UPDATE pages SET trashed_at = ? WHERE id = 'p1'`, now()); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := s.reindexPage("p1"); err != nil {
		t.Fatalf("reindexPage: %v", err)
	}
	ftsIntact(t, s)
	if n := termHits(t, s, "anderer"); n != 0 {
		t.Errorf("a trashed page still answers searches (%d hits)", n)
	}

	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM page_chunks WHERE page_id = 'p1'`).Scan(&left)
	if left != 0 {
		t.Errorf("a trashed page kept %d passages", left)
	}
}

// The consequence the invariant actually exists to prevent: seq is reused, so a
// missed delete does not merely leak — it re-points old words at a new page.
func TestReusedSeqDoesNotInheritTheOldPagesWords(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	indexedPage(t, s, ws, uid, "first", "Erste", "Ein Absatz über Zinsswaps.")

	rec := requestAs(t, s, cookie, "DELETE", "/api/pages/first?permanent=1", "")
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// Takes the seq the deleted passage had just vacated.
	indexedPage(t, s, ws, uid, "second", "Zweite", "Ein Absatz über Mittagessen.")

	if n := termHits(t, s, "zinsswaps"); n != 0 {
		t.Fatalf("the deleted page's word survived onto the new row (%d hits) — "+
			"searching for it would open a page that never mentioned it", n)
	}
	if n := termHits(t, s, "mittagessen"); n != 1 {
		t.Errorf("the new page is not searchable (%d hits, want 1)", n)
	}
}

// The delete path is the dangerous one, because page_chunks disappears by
// foreign-key cascade — out from under the index, with nothing in the DELETE
// statement itself to suggest the index was involved at all.
func TestPermanentDeleteTakesThePassagesWithIt(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	indexedPage(t, s, ws, uid, "keep", "Bleibt", "Dieser Absatz bleibt bestehen.")
	indexedPage(t, s, ws, uid, "gone", "Verschwindet", "Dieser Absatz wird vernichtet.")
	rec := requestAs(t, s, cookie, "DELETE", "/api/pages/gone?permanent=1", "")
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	ftsIntact(t, s)

	// And the terms are really gone, not merely unreferenced.
	var hits int
	s.db.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, `"vernichtet"`).Scan(&hits)
	if hits != 0 {
		t.Errorf("the deleted page's words still match (%d hits)", hits)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, `"bestehen"`).Scan(&hits)
	if hits != 1 {
		t.Errorf("the surviving page lost its index entry (%d hits, want 1)", hits)
	}
}

// snippet() is the one feature that forces external content rather than a
// contentless table: it needs the original words back. If the content table and
// the index ever disagree about which column is which, this is where it shows —
// the column number moved when chunk_id was dropped.
func TestSnippetStillComesFromTheRightColumn(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	indexedPage(t, s, ws, uid, "p1", "Ein Titel", "Der Vertrag endet am Quartalsende.")

	var snip string
	if err := s.db.QueryRow(`
		SELECT snippet(chunks_fts, 2, char(1), char(2), '…', 18)
		FROM chunks_fts WHERE chunks_fts MATCH ?`, `"quartalsende"`).Scan(&snip); err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if !strings.Contains(snip, "\x01Quartalsende\x02") {
		t.Errorf("snippet did not highlight the match in the text column: %q", snip)
	}
}

// Why page_chunks.seq is an explicit INTEGER PRIMARY KEY and not the implicit
// rowid every other table here uses.
//
// VACUUM is allowed to renumber implicit rowids, and backup.go runs VACUUM INTO
// on every single backup. An implicit rowid would have survived every test in
// this file and then broken in the one place nobody looks: the copy somebody
// restores from after losing the original. Search would come back pointing at
// the wrong passages, or at none.
func TestSearchSurvivesTheVacuumThatEveryBackupRuns(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	// Several pages, so a renumbering would visibly shuffle rather than
	// coincidentally land on itself.
	for _, p := range [][2]string{
		{"p1", "The first paragraph mentions debentures."},
		{"p2", "The second paragraph mentions escrow."},
		{"p3", "The third paragraph mentions arbitrage."},
	} {
		indexedPage(t, s, ws, uid, p[0], "Page "+p[0], p[1])
	}

	snap := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.db.Exec(`VACUUM INTO ?`, snap); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}

	restored, err := sql.Open("sqlite", "file:"+snap)
	if err != nil {
		t.Fatalf("open the backup: %v", err)
	}
	defer restored.Close()

	var page, snippet string
	err = restored.QueryRow(`
		SELECT c.page_id, snippet(chunks_fts, 2, char(1), char(2), '…', 12)
		FROM chunks_fts JOIN page_chunks c ON c.seq = chunks_fts.rowid
		WHERE chunks_fts MATCH ?`, `"escrow"`).Scan(&page, &snippet)
	if err != nil {
		t.Fatalf("searching the backup: %v", err)
	}
	if page != "p2" {
		t.Errorf("the backup's index points at %s, not the page that has the word", page)
	}
	if !strings.Contains(snippet, "\x01escrow\x02") {
		t.Errorf("the backup's snippet came from the wrong row: %q", snippet)
	}
}
