package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

type testEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
}

func fetchGraphEdges(t *testing.T, s *Server, cookie string) []testEdge {
	t.Helper()
	rec := proposalRequest(t, s, cookie, http.MethodGet, "/api/graph", "")
	if rec.Code != 200 {
		t.Fatalf("graph: status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Edges []testEdge `json:"edges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	return out.Edges
}

// A "relation" database property is a real, human-named connection between
// two rows — the closest thing this workspace has to a typed knowledge-graph
// edge — and /api/graph must surface it labeled with whatever the team named
// that column, not silently drop it as it did before.
func TestGraphIncludesLabeledRelationEdge(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "relgraph@example.test")
	ws := s.firstWorkspaceOf(t, uid)

	metrics := s.makeCollection(t, ws, uid, "Metrics", `[]`)
	metricRow := s.makeRow(t, ws, uid, metrics, "Conversion Rate", `{}`)

	features := s.makeCollection(t, ws, uid, "Features",
		`[{"id":"impacts","name":"Impacts Metric","type":"relation","relationCollection":"`+metrics+`"}]`)
	featureRow := s.makeRow(t, ws, uid, features, "Checkout Redesign",
		`{"impacts":["`+metricRow+`"]}`)

	edges := fetchGraphEdges(t, s, cookie)
	var found *testEdge
	for i := range edges {
		if edges[i].Kind == "relation" && edges[i].Source == featureRow && edges[i].Target == metricRow {
			found = &edges[i]
		}
	}
	if found == nil {
		t.Fatalf("no relation edge Feature->Metric found in %+v", edges)
	}
	if found.Label != "Impacts Metric" {
		t.Errorf("expected label %q, got %q", "Impacts Metric", found.Label)
	}
}

// A "backrelation" field holds no data of its own — it is the SAME edge read
// from the other side (see propDef in derived.go) — so a collection that
// declares one alongside the owning relation must not draw the connection
// twice.
func TestGraphDoesNotDuplicateBackrelationEdge(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "backrel@example.test")
	ws := s.firstWorkspaceOf(t, uid)

	features := s.makeCollection(t, ws, uid, "Features2", `[]`)
	metrics := s.makeCollection(t, ws, uid, "Metrics2",
		`[{"id":"impactedBy","name":"Impacted By","type":"backrelation","backrelationCollection":"`+features+`","backrelationProp":"impacts"}]`)
	metricRow := s.makeRow(t, ws, uid, metrics, "Revenue", `{}`)

	if _, err := s.db.Exec(`UPDATE collections SET schema = ? WHERE page_id = ?`,
		`[{"id":"impacts","name":"Impacts Metric","type":"relation","relationCollection":"`+metrics+`"}]`, features); err != nil {
		t.Fatalf("set features schema: %v", err)
	}
	featureRow := s.makeRow(t, ws, uid, features, "Pricing Change", `{"impacts":["`+metricRow+`"]}`)

	edges := fetchGraphEdges(t, s, cookie)
	count := 0
	for _, e := range edges {
		if e.Kind == "relation" && e.Source == featureRow && e.Target == metricRow {
			count++
		}
		if e.Kind == "relation" && e.Source == metricRow && e.Target == featureRow {
			t.Errorf("backrelation must not produce its own reverse edge: %+v", e)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 relation edge, got %d in %+v", count, edges)
	}
}
