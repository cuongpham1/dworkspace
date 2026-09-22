package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// scopeFixture builds one small tree that every scope below is asked about, so
// the scopes are compared against the same world rather than each against a
// convenient one.
//
//	root-kids  ── child-a, child-b
//	root-bare
//	root-gone                        (trashed)
//	db1        ── row-bare           (a bare database row: not a tree page)
//	tpl                              (a template, with tpl-kid under it)
type scopeFixture struct {
	s      *Server
	cookie string
	ws     string
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	s := testServer(t)
	uid, cookie := signedIn(t, s, "a@example.com")
	ws := makeWorkspace(t, s, uid)

	mk := func(id, parent, typ, title string, trashed, template bool) {
		t.Helper()
		var par, tr any
		if parent != "" {
			par = parent
		}
		if trashed {
			tr = now()
		}
		tpl := 0
		if template {
			tpl = 1
		}
		if _, err := s.db.Exec(`INSERT INTO pages (id, parent_id, title, content, position, created_at, updated_at, trashed_at, workspace_id, owner_id, visibility, type, is_template)
			VALUES (?, ?, ?, '[]', 0, ?, ?, ?, ?, ?, 'workspace', ?, ?)`,
			id, par, title, now(), now(), tr, ws, uid, typ, tpl); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk("root-kids", "", "doc", "Has children", false, false)
	mk("child-a", "root-kids", "doc", "Child A", false, false)
	mk("child-b", "root-kids", "doc", "Child B", false, false)
	mk("root-bare", "", "doc", "No children", false, false)
	mk("root-gone", "", "doc", "In the bin", true, false)
	mk("db1", "", "collection", "Pipeline", false, false)
	mk("row-bare", "db1", "doc", "A row", false, false)
	mk("tpl", "", "doc", "Template", false, true)
	mk("tpl-kid", "tpl", "doc", "Under a template", false, false)
	return scopeFixture{s: s, cookie: cookie, ws: ws}
}

func (f scopeFixture) list(t *testing.T, query string) []pageMeta {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/pages"+query, nil)
	req.Header.Set("Cookie", f.cookie)
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/pages%s: %d", query, rec.Code)
	}
	var list []pageMeta
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode %s: %v", query, err)
	}
	return list
}

func ids(list []pageMeta) map[string]bool {
	out := map[string]bool{}
	for _, p := range list {
		out[p.ID] = true
	}
	return out
}

// The change that moved the bytes: the bin was in every answer, and it was 88%
// of them, for a section the sidebar keeps collapsed.
func TestListPagesDefaultScopeLeavesTheBinBehind(t *testing.T) {
	f := newScopeFixture(t)
	got := ids(f.list(t, ""))
	if got["root-gone"] {
		t.Errorf("trashed page came back in the default scope")
	}
	for _, want := range []string{"root-kids", "child-a", "child-b", "root-bare", "db1"} {
		if !got[want] {
			t.Errorf("%s missing from the default scope", want)
		}
	}
	// Unchanged by this work, and worth pinning: a template and its children are
	// not pages in anybody's tree, and a bare database row is not one either.
	for _, gone := range []string{"tpl", "tpl-kid", "row-bare"} {
		if got[gone] {
			t.Errorf("%s leaked into the default scope", gone)
		}
	}
}

func TestListPagesTrashedScopeIsTheBinOnly(t *testing.T) {
	f := newScopeFixture(t)
	list := f.list(t, "?trashed=1")
	got := ids(list)
	if !got["root-gone"] {
		t.Errorf("the trashed page is missing from ?trashed=1")
	}
	for _, p := range list {
		if !p.Trashed {
			t.Errorf("%s is live and came back from ?trashed=1", p.ID)
		}
	}
}

func TestListPagesRootScopeIsRootsWithChevrons(t *testing.T) {
	f := newScopeFixture(t)
	list := f.list(t, "?scope=roots")
	got := ids(list)
	for _, want := range []string{"root-kids", "root-bare", "db1"} {
		if !got[want] {
			t.Errorf("%s missing from ?scope=roots", want)
		}
	}
	// The whole point of the scope: children do not travel with it.
	for _, gone := range []string{"child-a", "child-b", "root-gone", "tpl"} {
		if got[gone] {
			t.Errorf("%s came back from ?scope=roots", gone)
		}
	}
	for _, p := range list {
		if p.HasChildren == nil {
			t.Fatalf("%s arrived without hasChildren — the sidebar cannot draw a chevron", p.ID)
		}
		want := p.ID == "root-kids"
		if *p.HasChildren != want {
			t.Errorf("%s hasChildren = %v, want %v", p.ID, *p.HasChildren, want)
		}
	}
}

func TestListPagesParentScopeIsOneLevel(t *testing.T) {
	f := newScopeFixture(t)
	got := ids(f.list(t, "?parent=root-kids"))
	for _, want := range []string{"child-a", "child-b"} {
		if !got[want] {
			t.Errorf("%s missing from ?parent=root-kids", want)
		}
	}
	if got["root-kids"] || got["root-bare"] {
		t.Errorf("?parent= returned something other than the children")
	}
}

// The invariant the sidebar rests on: a chevron always opens onto something. If
// hasChildren and the parent scope ever disagreed, the tree would offer an
// arrow that unfolds to nothing, or hide a subtree behind a missing one.
func TestChevronAgreesWithWhatUnfoldingReturns(t *testing.T) {
	f := newScopeFixture(t)
	for _, p := range f.list(t, "?scope=roots") {
		kids := f.list(t, "?parent="+p.ID)
		if p.HasChildren == nil {
			t.Fatalf("%s: no hasChildren", p.ID)
		}
		if *p.HasChildren != (len(kids) > 0) {
			t.Errorf("%s: hasChildren = %v but unfolding returned %d",
				p.ID, *p.HasChildren, len(kids))
		}
	}
}

// Open tabs and the current page's parent are needed by id, whatever they are.
// A database row is deliberately not a tree page and can still be open in a
// tab, so this scope must not apply the tree rules.
func TestListPagesIDScopeIgnoresTreeRules(t *testing.T) {
	f := newScopeFixture(t)
	got := ids(f.list(t, "?ids=row-bare,child-a"))
	for _, want := range []string{"row-bare", "child-a"} {
		if !got[want] {
			t.Errorf("%s missing from ?ids=", want)
		}
	}
	if len(got) != 2 {
		t.Errorf("?ids= returned %d pages, want exactly the 2 asked for", len(got))
	}
}

// A workspace the caller is not a member of must not become readable by naming
// its pages directly — the id scope skips the tree rules, not the permissions.
func TestListPagesIDScopeStaysInsideVisibleWorkspaces(t *testing.T) {
	f := newScopeFixture(t)
	other, _ := signedIn(t, f.s, "b@example.com")
	ws2 := makeWorkspace(t, f.s, other)
	if _, err := f.s.db.Exec(`INSERT INTO pages (id, parent_id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility, type)
		VALUES ('secret', NULL, 'Not yours', '[]', 0, ?, ?, ?, ?, 'workspace', 'doc')`,
		now(), now(), ws2, other); err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	if ids(f.list(t, "?ids=secret,child-a"))["secret"] {
		t.Errorf("a page from another workspace came back from ?ids=")
	}
}
