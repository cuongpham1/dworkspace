package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Page history is stored compressed (revisions_store.go). Two things have to
// stay true for that to be a free win rather than a quiet loss:
//
//   - a revision reads back exactly as it was written, and
//   - the upload cleanup can still tell that a revision references a file.
//
// The second is the one with teeth. removeUnreferencedFiles asks "does any
// revision still mention this upload?" with a LIKE over the content column, and
// a gzip blob answers no to every LIKE there is. Without the assets column that
// question silently flips to "delete it" for every compressed revision, and the
// images vanish out from under restorable history — on disk, irreversibly, in a
// cleanup nobody was watching.

// bigDoc builds a revision large and repetitive enough to actually compress,
// the way a real page is.
func bigDoc(text string) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"paragraph","props":{"textColor":"default","backgroundColor":"default","textAlignment":"left"},"content":[{"type":"text","text":"` + text + `","styles":{}}]}`)
	}
	b.WriteString("]")
	return b.String()
}

// revisionsContaining counts a page's revisions whose content holds needle,
// reading them the way the application does rather than with a LIKE — the
// column a LIKE would search is compressed.
func revisionsContaining(t *testing.T, s *Server, pageID, needle string) int {
	t.Helper()
	rows, err := s.db.Query(`SELECT content, content_gz FROM page_revisions WHERE page_id = ?`, pageID)
	if err != nil {
		t.Fatalf("read revisions: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var content string
		var gz []byte
		if rows.Scan(&content, &gz) != nil {
			continue
		}
		if strings.Contains(decodeRevision(content, gz), needle) {
			n++
		}
	}
	return n
}

func TestRevisionReadsBackExactly(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	doc := bigDoc("The notice period is three months to the end of the quarter.")
	if _, err := s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES ('p1', 'Contract', ?, 0, ?, ?, ?, ?, 'workspace')`, doc, now(), now(), ws, uid); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	s.snapshotRevision("p1", uid, "Test")

	// It really is stored compressed — otherwise the rest of this proves nothing.
	var plain string
	var gz []byte
	if err := s.db.QueryRow(`SELECT content, content_gz FROM page_revisions WHERE page_id = 'p1'`).Scan(&plain, &gz); err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if len(gz) == 0 {
		t.Fatalf("a %d-byte revision was stored uncompressed", len(doc))
	}
	if plain != "" {
		t.Errorf("the plain column still holds %d bytes of the same thing", len(plain))
	}

	// And it comes back out of the endpoint byte for byte.
	var revID string
	s.db.QueryRow(`SELECT id FROM page_revisions WHERE page_id = 'p1'`).Scan(&revID)
	rec := requestAs(t, s, cookie, "GET", "/api/pages/p1/revisions/"+revID, "")
	if rec.Code != 200 {
		t.Fatalf("get revision: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Content) != doc {
		t.Errorf("the revision came back changed:\n got %.120s…\nwant %.120s…", got.Content, doc)
	}
}

// The data-loss case: a file that ONLY a compressed revision still points at.
func TestCleanupKeepsFilesACompressedRevisionStillNeeds(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)

	name := "abc123.png"
	dir := filepath.Join(s.dataDir, "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not really a png"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// The page that used to show the image no longer does; only the revision has it.
	doc := bigDoc("see the attachment")
	doc = strings.Replace(doc, "see the attachment", "see /files/"+name+" in the attachment", 1)
	if _, err := s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES ('p1', 'Contract', ?, 0, ?, ?, ?, ?, 'workspace')`, doc, now(), now(), ws, uid); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	s.snapshotRevision("p1", uid, "Test")
	if _, err := s.db.Exec(`UPDATE pages SET content = '[]' WHERE id = 'p1'`); err != nil {
		t.Fatalf("clear page: %v", err)
	}

	var gz []byte
	s.db.QueryRow(`SELECT content_gz FROM page_revisions WHERE page_id = 'p1'`).Scan(&gz)
	if len(gz) == 0 {
		t.Fatalf("the revision was not compressed — this test would pass for the wrong reason")
	}

	s.removeUnreferencedFiles(map[string]bool{name: true})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the cleanup deleted a file that a restorable revision still points at: %v", err)
	}
}

// Nothing to gain, nothing spent: a two-line revision is smaller as it is.
func TestTinyRevisionStaysPlain(t *testing.T) {
	content, gz, assets := encodeRevision(`[{"type":"paragraph"}]`)
	if len(gz) != 0 {
		t.Errorf("a 22-byte revision was compressed into %d bytes", len(gz))
	}
	if content != `[{"type":"paragraph"}]` {
		t.Errorf("content = %q, want it untouched", content)
	}
	if assets != "" {
		t.Errorf("assets = %q, want empty — there are no files in it", assets)
	}
}

// The one-time pass over revisions written before the column existed.
func TestBackfillCompressesOldRevisionsAndStopsThere(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)
	if _, err := s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES ('p1', 'Contract', '[]', 0, ?, ?, ?, ?, 'workspace')`, now(), now(), ws, uid); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	doc := bigDoc("An old entry from before compression existed.")
	for _, id := range []string{"r1", "r2", "r3"} {
		if _, err := s.db.Exec(`INSERT INTO page_revisions (id, page_id, created_at, author_id, author_name, title, content)
			VALUES (?, 'p1', ?, '', 'Test', 'T', ?)`, id, now(), doc); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// testServer already ran the pass on an empty table and recorded it as done.
	s.setSetting("revisions_compressed", "0")
	if err := s.compressRevisions(); err != nil {
		t.Fatalf("compressRevisions: %v", err)
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		var content string
		var gz []byte
		s.db.QueryRow(`SELECT content, content_gz FROM page_revisions WHERE id = ?`, id).Scan(&content, &gz)
		if len(gz) == 0 {
			t.Errorf("%s was not compressed", id)
		}
		if got := decodeRevision(content, gz); got != doc {
			t.Errorf("%s did not survive the pass intact", id)
		}
	}

	// Running it again must not touch what it already did — the guard is the
	// only thing standing between this and re-gzipping a gzip.
	s.setSetting("revisions_compressed", "0")
	if err := s.compressRevisions(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	var content string
	var gz []byte
	s.db.QueryRow(`SELECT content, content_gz FROM page_revisions WHERE id = 'r1'`).Scan(&content, &gz)
	if got := decodeRevision(content, gz); got != doc {
		t.Errorf("a second pass corrupted an already-compressed revision")
	}
}
