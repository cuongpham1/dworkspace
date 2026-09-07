package server

import (
	"encoding/json"
	"net/http"
)

// Inline page links (@mentions) are stored inside BlockNote content as
// {"type":"pageLink","props":{"pageId":"…"}}. The links table is a derived
// index (source page → target page) kept in sync on every content change, so
// backlinks ("linked references") can be queried cheaply.

func extractLinks(content []byte) []string {
	var v any
	if json.Unmarshal(content, &v) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if t["type"] == "pageLink" {
				if props, ok := t["props"].(map[string]any); ok {
					if id, ok := props["pageId"].(string); ok && id != "" && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				}
			}
			for _, val := range t {
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	return out
}

// updateLinks rebuilds the outgoing links for a source page. A trashed page
// contributes no links. Only targets that exist and differ from the source
// are recorded. The delete+inserts run in one transaction so two concurrent
// reindexes of the same page can't interleave into a corrupted (mixed) set.
//
// Returns the valid target ids actually written, so a caller (reindexPage) can
// diff them against what was there before and tell which links are brand new
// — the signal page_subscriptions notices are built from.
func (s *Server) updateLinks(sourceID, content string, trashed bool) []string {
	tx, err := s.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM links WHERE source_id = ?`, sourceID)
	var written []string
	if !trashed {
		for _, target := range extractLinks([]byte(content)) {
			if target == sourceID {
				continue
			}
			var exists int
			if tx.QueryRow(`SELECT COUNT(*) FROM pages WHERE id = ?`, target).Scan(&exists); exists == 0 {
				continue
			}
			tx.Exec(`INSERT INTO links (source_id, target_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, sourceID, target)
			written = append(written, target)
		}
	}
	if tx.Commit() != nil {
		return nil
	}
	return written
}

type restGraphEdge struct{ src, tgt, kind, label string }

// relationGraphEdges reads every collection's schema in scope and, for each
// "relation"-typed property, turns its stored values into graph edges. A
// relation field holds a real, human-named connection between two rows
// (Feature "depends on" Feature, Feature "impacts" Metric — whatever the team
// called the column when they built the database) — the closest thing this
// workspace has to a typed knowledge-graph edge, and it already existed;
// nothing here was new to write except reading it as a graph.
//
// "backrelation" fields are deliberately skipped: they hold no data of their
// own (see propDef in derived.go) — they are the SAME edge read from the
// other side, and including them would draw every relation twice.
func (s *Server) relationGraphEdges(ws []string) []restGraphEdge {
	wargs := make([]any, len(ws))
	for i, v := range ws {
		wargs[i] = v
	}
	ph := placeholders(len(ws))
	rows, err := s.db.Query(`
		SELECT c.page_id, c.schema FROM collections c
		JOIN pages p ON p.id = c.page_id AND p.trashed_at IS NULL AND p.workspace_id IN (`+ph+`)`,
		wargs...)
	if err != nil {
		return nil
	}
	type coll struct{ id, schema string }
	var colls []coll
	for rows.Next() {
		var c coll
		if rows.Scan(&c.id, &c.schema) == nil {
			colls = append(colls, c)
		}
	}
	rows.Close()

	var edges []restGraphEdge
	for _, c := range colls {
		var schema []propDef
		if json.Unmarshal([]byte(c.schema), &schema) != nil {
			continue
		}
		var relFields []propDef
		for _, d := range schema {
			if d.Type == "relation" {
				relFields = append(relFields, d)
			}
		}
		if len(relFields) == 0 {
			continue
		}
		rowRows, err := s.db.Query(`SELECT id, props FROM pages WHERE parent_id = ? AND trashed_at IS NULL`, c.id)
		if err != nil {
			continue
		}
		type rowProps struct{ id, props string }
		var prows []rowProps
		for rowRows.Next() {
			var p rowProps
			if rowRows.Scan(&p.id, &p.props) == nil {
				prows = append(prows, p)
			}
		}
		rowRows.Close()
		for _, p := range prows {
			var props map[string]any
			if json.Unmarshal([]byte(p.props), &props) != nil {
				continue
			}
			for _, d := range relFields {
				for _, tgt := range relationIDs(props[d.ID]) {
					if tgt != p.id {
						edges = append(edges, restGraphEdge{src: p.id, tgt: tgt, kind: "relation", label: d.Name})
					}
				}
			}
		}
	}
	return edges
}

// handleGraph returns the edges among pages in the caller's visible
// workspaces, powering the "All pages" index and the graph view: markdown
// links (kind "link") and, since the graph became relation-aware, database
// relation fields (kind "relation", labeled with whatever the team named that
// column). Nodes come from the client's already-loaded page list; only edges
// whose BOTH endpoints are live pages in a visible workspace are returned.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	ws := s.scopeWorkspacesFor(requestUser(r), s.visibleWorkspaces(requestUser(r).ID))
	if len(ws) == 0 {
		writeJSON(w, map[string]any{"edges": []any{}})
		return
	}
	wargs := make([]any, len(ws))
	for i, v := range ws {
		wargs[i] = v
	}
	ph := placeholders(len(ws))
	rows, err := s.db.Query(`
		SELECT l.source_id, l.target_id FROM links l
		JOIN pages ps ON ps.id = l.source_id AND ps.trashed_at IS NULL AND ps.workspace_id IN (`+ph+`)
		JOIN pages pt ON pt.id = l.target_id AND pt.trashed_at IS NULL AND pt.workspace_id IN (`+ph+`)`,
		append(append([]any{}, wargs...), wargs...)...)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var scanned []restGraphEdge
	for rows.Next() {
		var e restGraphEdge
		if rows.Scan(&e.src, &e.tgt) == nil {
			e.kind = "link"
			scanned = append(scanned, e)
		}
	}
	rows.Close() // drain first, then check per row (one DB connection)
	scanned = append(scanned, s.relationGraphEdges(ws)...)

	// As with backlinks, check per page here too: the workspace filter alone
	// returned edges from and to other people's private pages. Titles were not
	// in them, but the ids were — and those were the missing piece for pointing
	// at somebody else's page through a relation or parentId.
	uid := requestUser(r).ID
	readable := map[string]bool{}
	canSee := func(id string) bool {
		if v, ok := readable[id]; ok {
			return v
		}
		v := s.canRead(uid, id)
		readable[id] = v
		return v
	}
	edges := []map[string]string{}
	for _, e := range scanned {
		if canSee(e.src) && canSee(e.tgt) {
			m := map[string]string{"source": e.src, "target": e.tgt, "kind": e.kind}
			if e.label != "" {
				m["label"] = e.label
			}
			edges = append(edges, m)
		}
	}
	writeJSON(w, map[string]any{"edges": edges})
}

type backlink struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Icon  string `json:"icon"`
}

func (s *Server) handleBacklinks(w http.ResponseWriter, r *http.Request) {
	if !s.canReadReq(r, r.PathValue("id")) {
		httpError(w, 404, "page not found")
		return
	}
	rows, err := s.db.Query(`
		SELECT p.id, p.title, p.icon FROM links l
		JOIN pages p ON p.id = l.source_id
		WHERE l.target_id = ? AND p.trashed_at IS NULL
		ORDER BY p.updated_at DESC`, r.PathValue("id"))
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var cand []backlink
	for rows.Next() {
		var b backlink
		if err := rows.Scan(&b.ID, &b.Title, &b.Icon); err != nil {
			rows.Close()
			httpError(w, 500, err.Error())
			return
		}
		cand = append(cand, b)
	}
	rows.Close()
	// Filter after draining the cursor (single DB connection — see handleSearch).
	list := []backlink{}
	for _, b := range cand {
		if s.canReadReq(r, b.ID) {
			list = append(list, b)
		}
	}
	writeJSON(w, list)
}
