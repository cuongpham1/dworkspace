package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type vusEvaluationCase struct {
	name     string
	query    string
	expected string
}

var vusEvaluationCorpus = []vusEvaluationCase{
	{name: "exact-name-id", query: "VUS-4242", expected: "vus-exact"},
	{name: "semantic-discovery", query: "vacation time off", expected: "vus-semantic"},
	{name: "passage-heading", query: "deployment rollback", expected: "vus-passage"},
	{name: "multilingual", query: "Vertrags Kündigung", expected: "vus-multilingual"},
	{name: "permission-sensitive", query: "private launch plan", expected: "vus-permitted"},
}

func TestVUSBenchmark_reportsLexicalSemanticAndHybridMetrics(t *testing.T) {
	// Given
	s := testServer(t)
	alice, _ := signedIn(t, s, "vus-alice@example.test")
	bob, _ := signedIn(t, s, "vus-bob@example.test")
	ws := s.firstWorkspaceOf(t, alice)
	addWorkspaceMember(t, s, workspaceMember{ws, bob})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-exact", "VUS-4242 Migration Plan", "Migration runbook", ws, bob, "workspace"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-semantic", "Time Off Policy", "Annual leave and vacation time off policy for the team", ws, bob, "workspace"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-passage", "Deployments", "Operations runbook", ws, bob, "workspace"})
	setVUSBody(t, s, "vus-passage", `[{"type":"heading","props":{"level":1},"content":[{"type":"text","text":"Release safety"}]},{"type":"paragraph","content":[{"type":"text","text":"The deployment rollback procedure restores the previous version."}]}]`)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-multilingual", "Vertragsrecht", "Vertrags Kündigung und Kündigungsfristen", ws, bob, "workspace"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-private", "Private launch", "private launch plan", ws, alice, "private"})
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-permitted", "Launch notes", "private launch plan for the shared release", ws, bob, "workspace"})

	// When
	metrics := map[string]vusMetrics{}
	for _, evalCase := range vusEvaluationCorpus {
		lexical := s.searchChunks(bob, ftsMatch(evalCase.query), []string{ws}, 10)
		if len(lexical) == 0 {
			lexical = s.searchPagesFallback(bob, ftsMatch(evalCase.query), []string{ws}, 10)
		}
		semantic := make([]searchResult, 0)
		for _, result := range s.searchSemantic(context.TODO(), bob, evalCase.query, []string{ws}, 10) {
			semantic = append(semantic, result.searchResult)
		}
		hybrid := s.searchHybrid(context.TODO(), bob, evalCase.query, ftsMatch(evalCase.query), []string{ws}, 10)
		metrics[evalCase.name] = vusMetrics{Lexical: rankMetric(lexical, evalCase.expected, 3), Semantic: rankMetric(semantic, evalCase.expected, 3), Hybrid: rankMetric(hybrid, evalCase.expected, 3)}
		t.Logf("%s lexical=%s semantic=%s hybrid=%s", evalCase.name, ids(lexical), ids(semantic), ids(hybrid))
		if evalCase.name == "permission-sensitive" {
			assertNoID(t, lexical, "vus-private")
			assertNoID(t, semantic, "vus-private")
			assertNoID(t, hybrid, "vus-private")
		}
	}

	// Then
	for _, mode := range []string{"Lexical", "Semantic", "Hybrid"} {
		metric := aggregateVUSMetrics(metrics, mode)
		t.Logf("VUS %s recall@3=%.2f MRR=%.2f", mode, metric.Recall, metric.MRR)
	}
	if metrics["exact-name-id"].Lexical.Recall == 0 || metrics["passage-heading"].Hybrid.Recall == 0 {
		t.Fatalf("baseline corpus regression: %+v", metrics)
	}
}

type vusMetric struct{ Recall, MRR float64 }

type vusMetrics struct{ Lexical, Semantic, Hybrid vusMetric }

func rankMetric(results []searchResult, expected string, k int) vusMetric {
	for i, result := range results {
		if i >= k {
			break
		}
		if result.ID == expected {
			return vusMetric{Recall: 1, MRR: 1 / float64(i+1)}
		}
	}
	return vusMetric{}
}

func aggregateVUSMetrics(metrics map[string]vusMetrics, mode string) vusMetric {
	var total vusMetric
	for _, metric := range metrics {
		var current vusMetric
		switch mode {
		case "Lexical":
			current = metric.Lexical
		case "Semantic":
			current = metric.Semantic
		case "Hybrid":
			current = metric.Hybrid
		default:
			continue
		}
		total.Recall += current.Recall
		total.MRR += current.MRR
	}
	if len(metrics) > 0 {
		total.Recall /= float64(len(metrics))
		total.MRR /= float64(len(metrics))
	}
	return total
}

func ids(results []searchResult) string {
	values := make([]string, 0, len(results))
	for _, result := range results {
		values = append(values, result.ID)
	}
	return strings.Join(values, ",")
}

func assertNoID(t *testing.T, results []searchResult, forbidden string) {
	t.Helper()
	for _, result := range results {
		if result.ID == forbidden {
			t.Fatalf("forbidden result %q in %s", forbidden, ids(results))
		}
	}
}

func setVUSBody(t *testing.T, s *Server, pageID, content string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, content, pageID); err != nil {
		t.Fatalf("set VUS body: %v", err)
	}
	if err := s.reindexPage(pageID); err != nil {
		t.Fatalf("reindex VUS body: %v", err)
	}
}

func TestVUSBenchmark_isReachableOverSearchHTTP(t *testing.T) {
	// Given
	s := testServer(t)
	userID, cookie := signedIn(t, s, "vus-http@example.test")
	ws := s.firstWorkspaceOf(t, userID)
	seedDescriptionSearchPage(t, s, descriptionSearchPage{"vus-http", "HTTP VUS", "observable provenance", ws, userID, "workspace"})

	// When
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape("observable provenance"), nil)
	req.Header.Set("Cookie", cookie)
	s.ServeHTTP(rec, req)

	// Then
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vus-http") || !strings.Contains(rec.Body.String(), "provenance") {
		t.Fatalf("VUS HTTP surface = %d %s", rec.Code, rec.Body.String())
	}
}
