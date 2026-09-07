package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// The HTTP API, through the real router — so the route patterns, the
// middlewares and the status codes are all in the path.
//
// The status codes are the part worth testing rather than assuming. The
// interface branches on them (and on the `code` beside them) to decide whether
// to keep the author's form open, offer a reload, or say "you cannot approve
// this" — so a handler that returns 500 where it means 409 turns a recoverable
// conflict into a dead end.

func TestTheAuthoringAndReviewLoopOverHTTP(t *testing.T) {
	f := newSkillFixture(t)

	// 1. An author creates a skill. The library shows it as a draft.
	create := fmt.Sprintf(`{"workspaceId":%q,"name":"Architecture review","slug":"architecture-review",
		"description":"How we review architectural changes here","deliveryMode":"remote",
		"instructions":"Check the dependency direction before approving.",
		"triggers":["architecture","dependency"],
		"scope":{"roles":["reviewer"],"taskTypes":["code-review"]},
		"dependencies":[{"type":"capability","id":"mcp","required":true}]}`, f.ws)
	rec := f.call(t, "POST", "/api/skills", f.memberCookie, create)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created skillDetail
	decodeInto(t, rec, &created)
	sk := created.Skill
	if created.Draft == nil || created.Draft.Version != 1 {
		t.Fatalf("no draft came back: %+v", created)
	}
	if sk.Slug != "architecture-review" {
		t.Errorf("slug %q", sk.Slug)
	}

	// 2. The library row answers the questions §7.2 asks WITHOUT a second
	// request, and it does not carry the instructions.
	rec = f.call(t, "GET", "/api/skills?workspace="+f.ws, f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dependency direction") {
		t.Error("the library list carries instruction bodies — a hundred skills would then be a hundred bodies on the wire")
	}
	var list []skillListEntry
	decodeInto(t, rec, &list)
	if len(list) != 1 {
		t.Fatalf("the library lists %d skills, want 1", len(list))
	}
	row := list[0]
	if row.Delivery != skillDeliveryRemote || row.DraftVersion != 1 || row.CurrentVersionID != "" {
		t.Errorf("the row does not describe the state: delivery %q draft v%d published %q",
			row.Delivery, row.DraftVersion, row.CurrentVersionID)
	}
	if len(row.ScopeSummary) == 0 {
		t.Error("no scope summary, so the list cannot answer \"where does this apply\"")
	}
	if row.CanReview {
		t.Error("an ordinary member is offered review actions")
	}
	if !row.CanAuthor {
		t.Error("an ordinary member is not offered authoring")
	}

	// 3. Saving a draft again. The optimistic lock travels as updatedAt.
	patch := fmt.Sprintf(`{"instructions":"Check the dependency direction, then the module boundaries.","updatedAt":%q}`,
		created.Draft.UpdatedAt)
	rec = f.call(t, "PATCH", "/api/skills/"+sk.ID+"/versions/"+created.Draft.ID, f.memberCookie, patch)
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		Skill   skill        `json:"skill"`
		Version skillVersion `json:"version"`
	}
	decodeInto(t, rec, &saved)
	if !strings.Contains(saved.Version.Instructions, "module boundaries") {
		t.Errorf("the save did not land: %q", saved.Version.Instructions)
	}

	// A STALE save is a 409 with a code the interface can branch on, and the
	// author's text is not lost — the browser still has it.
	rec = f.call(t, "PATCH", "/api/skills/"+sk.ID+"/versions/"+created.Draft.ID, f.memberCookie, patch)
	if rec.Code != 409 {
		t.Errorf("a stale save: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "skill_conflict" {
		t.Errorf("stale save code %q, want skill_conflict", code)
	}

	// 4. Submit for review.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/1/submit", f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var pending skillVersion
	decodeInto(t, rec, &pending)
	if pending.Status != skillStatusPending {
		t.Fatalf("after submit: %q", pending.Status)
	}

	// 5. The review queue shows it, with the deterministic risk summary, to a
	// reviewer.
	rec = f.call(t, "GET", "/api/skills/review-queue?workspace="+f.ws, f.adminCookie, "")
	if rec.Code != 200 {
		t.Fatalf("queue: %d %s", rec.Code, rec.Body.String())
	}
	var queue []reviewQueueEntry
	decodeInto(t, rec, &queue)
	if len(queue) != 1 {
		t.Fatalf("the queue holds %d entries, want 1", len(queue))
	}
	entry := queue[0]
	if !entry.CanReview || !entry.FirstPublish {
		t.Errorf("the queue entry says canReview=%v firstPublish=%v", entry.CanReview, entry.FirstPublish)
	}
	if entry.Previous != nil {
		t.Error("a first publication has a previous version to diff against")
	}
	if !hasFlag(entry.Risks, "first_publish") || !hasFlag(entry.Risks, "first_remote_publish") {
		t.Errorf("risk flags %v — a first Remote publication is both of those", entry.Risks)
	}
	// This one IS scoped (roles + task types), so it must NOT be flagged as
	// reaching the whole workspace. A risk flag that fires on everything is
	// noise a reviewer learns to click past.
	if hasFlag(entry.Risks, "workspace_wide_scope") {
		t.Errorf("risk flags %v claim workspace-wide scope, but the skill is scoped to a role and a task type",
			entry.Risks)
	}

	// A member sees the queue too (their own submission is in it) but is not
	// offered the decision.
	rec = f.call(t, "GET", "/api/skills/review-queue?workspace="+f.ws, f.memberCookie, "")
	decodeInto(t, rec, &queue)
	if len(queue) != 1 || queue[0].CanReview {
		t.Errorf("a member's view of the queue: %d entries, canReview=%v", len(queue), len(queue) > 0 && queue[0].CanReview)
	}

	// 6. A member cannot approve — 403 with a code, not a 500.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/1/approve", f.memberCookie, "")
	if rec.Code != 403 {
		t.Fatalf("a member's approve: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "skill_review_only" {
		t.Errorf("code %q — the interface says \"you cannot approve this\" from it, not a generic failure", code)
	}

	// 7. The reviewer publishes.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/1/approve", f.adminCookie, "")
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	var published struct {
		Skill   skill        `json:"skill"`
		Version skillVersion `json:"version"`
	}
	decodeInto(t, rec, &published)
	if published.Skill.CurrentVersionNumber != 1 || published.Version.Status != skillStatusApproved {
		t.Fatalf("after approve: pointer v%d, version %q",
			published.Skill.CurrentVersionNumber, published.Version.Status)
	}

	// 8. The approved version is read-only through the API too.
	rec = f.call(t, "PATCH", "/api/skills/"+sk.ID+"/versions/1", f.adminCookie, `{"instructions":"anything"}`)
	if rec.Code != 409 {
		t.Errorf("patching an approved version: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "skill_not_draft" {
		t.Errorf("code %q — the interface turns this into \"create a new version\"", code)
	}

	// 9. A new version starts from the approved one.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions", f.memberCookie, `{"fromVersion":"1"}`)
	if rec.Code != 200 {
		t.Fatalf("new version: %d %s", rec.Code, rec.Body.String())
	}
	var v2 skillVersion
	decodeInto(t, rec, &v2)
	if v2.Version != 2 || v2.Status != skillStatusDraft {
		t.Fatalf("the new version is v%d/%s", v2.Version, v2.Status)
	}

	// 10. Request changes sends it back with the reason.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/2/submit", f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("submit v2: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/2/request-changes", f.adminCookie,
		`{"note":"Say what to do when the boundaries are already wrong."}`)
	if rec.Code != 200 {
		t.Fatalf("request changes: %d %s", rec.Code, rec.Body.String())
	}
	var back skillVersion
	decodeInto(t, rec, &back)
	if back.Status != skillStatusDraft || back.ReviewNote == "" {
		t.Fatalf("after request-changes: %q, note %q", back.Status, back.ReviewNote)
	}

	// 11. The detail screen's payload, and the audit trail behind it.
	rec = f.call(t, "GET", "/api/skills/"+sk.ID, f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail skillDetail
	decodeInto(t, rec, &detail)
	if len(detail.Versions) != 2 {
		t.Errorf("the detail lists %d versions, want 2", len(detail.Versions))
	}
	if detail.Current == nil || detail.Current.Version != 1 {
		t.Error("the detail does not identify the published version")
	}
	if detail.Draft == nil || detail.Draft.Version != 2 {
		t.Error("the detail does not identify the open draft")
	}

	rec = f.call(t, "GET", "/api/skills/"+sk.ID+"/audit", f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body.String())
	}
	var audit []skillAuditEntry
	decodeInto(t, rec, &audit)
	if len(audit) == 0 {
		t.Fatal("the audit tab has nothing to show")
	}
	// Filters narrow it.
	rec = f.call(t, "GET", "/api/skills/"+sk.ID+"/audit?action=approved", f.memberCookie, "")
	decodeInto(t, rec, &audit)
	if len(audit) != 1 || audit[0].Action != skillActionApproved {
		t.Errorf("the action filter returned %d rows: %+v", len(audit), audit)
	}
	rec = f.call(t, "GET", "/api/skills/"+sk.ID+"/audit?version=2", f.memberCookie, "")
	decodeInto(t, rec, &audit)
	for _, e := range audit {
		if e.VersionNumber != 2 {
			t.Errorf("the version filter let through v%d", e.VersionNumber)
		}
	}
}

// The library's filters. Each one is what a chip in the header does.
func TestLibraryFilters(t *testing.T) {
	f := newSkillFixture(t)
	remote, rv := f.makeSkill(t, f.memberID, "Remote published", skillDeliveryRemote, "read me", nil)
	f.publish(t, remote, rv)
	client, cv := f.makeSkill(t, f.memberID, "Client published", skillDeliveryClient, "install me", nil)
	f.publish(t, client, cv)
	drafting, _ := f.makeSkill(t, f.adminID, "Still drafting", skillDeliveryRemote, "wip", nil)

	for _, c := range []struct {
		query string
		want  []string
	}{
		{"", []string{"Remote published", "Client published", "Still drafting"}},
		{"&delivery=remote", []string{"Remote published", "Still drafting"}},
		{"&delivery=client", []string{"Client published"}},
		{"&status=approved", []string{"Remote published", "Client published"}},
		{"&status=draft", []string{"Still drafting"}},
		{"&q=published", []string{"Remote published", "Client published"}},
		{"&q=nothing+matches+this", nil},
		{"&owner=" + f.adminID, []string{"Still drafting"}},
	} {
		rec := f.call(t, "GET", "/api/skills?workspace="+f.ws+c.query, f.memberCookie, "")
		if rec.Code != 200 {
			t.Fatalf("list%s: %d %s", c.query, rec.Code, rec.Body.String())
		}
		var list []skillListEntry
		decodeInto(t, rec, &list)
		got := map[string]bool{}
		for _, e := range list {
			got[e.Name] = true
		}
		if len(list) != len(c.want) {
			t.Errorf("list%s returned %d rows (%v), want %v", c.query, len(list), namesOf(got), c.want)
			continue
		}
		for _, want := range c.want {
			if !got[want] {
				t.Errorf("list%s is missing %q (has %v)", c.query, want, namesOf(got))
			}
		}
	}
	_ = drafting
}

// The Resolver test endpoint calls the SAME resolver as MCP. Proved by
// comparing the two answers for the same context — a frontend reimplementation
// could not keep that true.
func TestResolveTestEndpointAgreesWithMCP(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Architecture review", skillDeliveryRemote, "Check the direction.",
		func(in *skillDraftInput) {
			triggers := []string{"architecture"}
			in.Triggers = &triggers
			in.Scope = scopeOf(f.ws, func(sc *skillScope) {
				sc.Roles = []string{"reviewer"}
				sc.ProjectIDs = []string{"stockbook"}
			})
		})
	f.publish(t, sk, v)

	body := fmt.Sprintf(`{"workspaceId":%q,"task":"review the architecture","project":"stockbook",
		"role":"reviewer","agent":"chatgpt","capabilities":["mcp"]}`, f.ws)
	rec := f.call(t, "POST", "/api/skills/resolve-test", f.memberCookie, body)
	if rec.Code != 200 {
		t.Fatalf("resolve-test: %d %s", rec.Code, rec.Body.String())
	}
	var fromUI resolveResult
	decodeInto(t, rec, &fromUI)

	fromMCP, err := f.s.mcpSkillResolve(f.agentUser(t, f.memberID), resolveRequest{
		WorkspaceID: f.ws, Task: "review the architecture", Project: "stockbook",
		Role: "reviewer", Agent: "chatgpt", Capabilities: []string{"mcp"},
	})
	if err != nil {
		t.Fatalf("mcp resolve: %v", err)
	}
	var mcpAnswer resolveResult
	if err := json.Unmarshal([]byte(fromMCP), &mcpAnswer); err != nil {
		t.Fatalf("mcp answer is not JSON: %v", err)
	}
	if len(fromUI.Selected) != 1 || len(mcpAnswer.Selected) != 1 {
		t.Fatalf("UI selected %d, MCP selected %d", len(fromUI.Selected), len(mcpAnswer.Selected))
	}
	a, b := fromUI.Selected[0], mcpAnswer.Selected[0]
	if a.SkillID != b.SkillID || a.Version != b.Version || a.Score != b.Score || a.ContentHash != b.ContentHash {
		t.Errorf("the screen and the agent disagree:\n  UI  %+v\n  MCP %+v", a, b)
	}
	if len(a.Reasons) != len(b.Reasons) {
		t.Errorf("the breakdowns differ: %d vs %d lines", len(a.Reasons), len(b.Reasons))
	}
	// The dry run is recorded as a human resolution.
	entries, err := f.s.skillAudit(sk.ID, skillAuditFilter{Action: skillActionResolved})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d resolution rows after one dry run and one agent call, want 2", len(entries))
	}
	kinds := map[string]bool{entries[0].ActorType: true, entries[1].ActorType: true}
	if !kinds["human"] || !kinds["agent"] {
		t.Errorf("the audit does not distinguish the dry run from the agent call: %v", namesOf(kinds))
	}
}

// The resolver test screen has to be able to show WHY nothing was selected.
func TestResolveTestExplainsAnEmptyResult(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Needs a browser", skillDeliveryRemote, "Open it.",
		func(in *skillDraftInput) {
			triggers := []string{"screenshot"}
			in.Triggers = &triggers
			deps := []skillDependency{{Type: skillDepCapability, ID: "browser", Required: true}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)

	body := fmt.Sprintf(`{"workspaceId":%q,"task":"take a screenshot","capabilities":["mcp"]}`, f.ws)
	rec := f.call(t, "POST", "/api/skills/resolve-test", f.memberCookie, body)
	if rec.Code != 200 {
		t.Fatalf("resolve-test: %d %s", rec.Code, rec.Body.String())
	}
	var res resolveResult
	decodeInto(t, rec, &res)
	if len(res.Selected) != 0 {
		t.Fatal("a skill needing a browser was selected")
	}
	if len(res.Excluded) != 1 {
		t.Fatalf("nothing to show the user about why: %+v", res)
	}
	if res.Excluded[0].ReasonCode != "missing_required_dependency" || len(res.Excluded[0].Missing) != 1 {
		t.Errorf("the exclusion is not renderable: %+v", res.Excluded[0])
	}
	if res.Considered != 1 {
		t.Errorf("considered = %d — the screen distinguishes \"nothing published\" from \"nothing matched\"", res.Considered)
	}
}

// Every skill route refuses a caller from another workspace, and refuses it as
// NOT FOUND — telling a stranger that an id is real is itself information.
func TestEverySkillRouteIsWorkspaceIsolated(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Ours", skillDeliveryClient, "install me", nil)
	f.publish(t, sk, v)

	for _, probe := range []struct{ method, path, body string }{
		{"GET", "/api/skills/" + sk.ID, ""},
		{"GET", "/api/skills/" + sk.ID + "/versions", ""},
		{"GET", "/api/skills/" + sk.ID + "/versions/1", ""},
		{"GET", "/api/skills/" + sk.ID + "/audit", ""},
		{"GET", "/api/skills/" + sk.ID + "/versions/1/package", ""},
		{"GET", "/api/skills/" + sk.ID + "/versions/1/package-info", ""},
		{"POST", "/api/skills/" + sk.ID + "/versions", `{}`},
		{"PATCH", "/api/skills/" + sk.ID + "/versions/1", `{"instructions":"x"}`},
		{"POST", "/api/skills/" + sk.ID + "/versions/1/submit", ""},
		{"POST", "/api/skills/" + sk.ID + "/versions/1/approve", ""},
		{"POST", "/api/skills/" + sk.ID + "/versions/1/request-changes", `{"note":"n"}`},
		{"POST", "/api/skills/" + sk.ID + "/versions/1/deprecate", ""},
	} {
		rec := f.call(t, probe.method, probe.path, f.outsiderCk, probe.body)
		if rec.Code != 404 {
			t.Errorf("%s %s as an outsider: %d %s — want 404", probe.method, probe.path, rec.Code,
				strings.TrimSpace(rec.Body.String()))
		}
	}
	// And the outsider's own library and queue are empty rather than erroring.
	for _, path := range []string{"/api/skills", "/api/skills/review-queue"} {
		rec := f.call(t, "GET", path, f.outsiderCk, "")
		if rec.Code != 200 {
			t.Errorf("GET %s as an outsider: %d", path, rec.Code)
			continue
		}
		if strings.Contains(rec.Body.String(), sk.ID) {
			t.Errorf("GET %s leaked another workspace's skill", path)
		}
	}
}

// review-queue and resolve-test are literal path segments that sit beside
// {skillId}. If Go's ServeMux ever preferred the wildcard, "review-queue"
// would be looked up as a skill id and the screen would 404.
func TestLiteralSkillRoutesAreNotParsedAsIDs(t *testing.T) {
	f := newSkillFixture(t)
	if rec := f.call(t, "GET", "/api/skills/review-queue", f.adminCookie, ""); rec.Code != 200 {
		t.Errorf("GET /api/skills/review-queue: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, "POST", "/api/skills/resolve-test", f.adminCookie, `{"task":"x"}`); rec.Code != 200 {
		t.Errorf("POST /api/skills/resolve-test: %d %s", rec.Code, rec.Body.String())
	}
	// A genuinely unknown id is a 404, not a 500.
	if rec := f.call(t, "GET", "/api/skills/deadbeef", f.adminCookie, ""); rec.Code != 404 {
		t.Errorf("GET an unknown skill: %d %s", rec.Code, rec.Body.String())
	}
}

// Bad input is a 400 with a translatable code — never a 500, and never a
// sentence about canonical JSON that a person cannot act on.
func TestBadInputAnswersWithAnActionableCode(t *testing.T) {
	f := newSkillFixture(t)
	for _, c := range []struct {
		body     string
		wantCode string
	}{
		{`{`, "bad_json"},
		{fmt.Sprintf(`{"workspaceId":%q}`, f.ws), "skill_name_required"},
		{fmt.Sprintf(`{"workspaceId":%q,"name":"x","deliveryMode":"telepathy"}`, f.ws), "skill_bad_delivery"},
		{fmt.Sprintf(`{"workspaceId":%q,"name":"x","dependencies":[{"type":"sudo","id":"root"}]}`, f.ws), "skill_bad_dependency_type"},
		{fmt.Sprintf(`{"workspaceId":%q,"name":"x","references":[{"kind":"telepathy","target":"t"}]}`, f.ws), "skill_bad_reference_kind"},
		{fmt.Sprintf(`{"workspaceId":%q,"name":"√"}`, f.ws), "skill_slug_required"},
	} {
		rec := f.call(t, "POST", "/api/skills", f.memberCookie, c.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", c.body, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if got := errorCode(t, rec); got != c.wantCode {
			t.Errorf("%s: code %q, want %q", c.body, got, c.wantCode)
		}
	}
}

// Submitting an incomplete draft explains what is missing, and the draft is
// still there afterwards — the author's work is not thrown away by a failed
// submit.
func TestAFailedSubmitLeavesTheDraftIntact(t *testing.T) {
	f := newSkillFixture(t)
	rec := f.call(t, "POST", "/api/skills", f.memberCookie,
		fmt.Sprintf(`{"workspaceId":%q,"name":"Half done","deliveryMode":"remote"}`, f.ws))
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created skillDetail
	decodeInto(t, rec, &created)

	rec = f.call(t, "POST", "/api/skills/"+created.Skill.ID+"/versions/1/submit", f.memberCookie, "")
	if rec.Code != 400 {
		t.Fatalf("submit an empty draft: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "skill_description_required" {
		t.Errorf("code %q — the form highlights the field from this", code)
	}
	// The draft survived.
	stored, err := f.s.skillVersionByNumber(created.Skill.ID, 1)
	if err != nil {
		t.Fatalf("the draft is gone after a failed submit: %v", err)
	}
	if stored.Status != skillStatusDraft {
		t.Errorf("the draft is now %q", stored.Status)
	}
}

// The deprecation warning case: deprecating the only published version leaves
// the skill unusable, and the API says so in the skill it returns so the
// interface can warn before and confirm after.
func TestDeprecatingTheOnlyPublishedVersionOverHTTP(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Sole version", skillDeliveryRemote, "Do it.", nil)
	f.publish(t, sk, v)

	rec := f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/1/deprecate", f.adminCookie,
		`{"note":"the endpoint is gone"}`)
	if rec.Code != 200 {
		t.Fatalf("deprecate: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Skill   skill        `json:"skill"`
		Version skillVersion `json:"version"`
	}
	decodeInto(t, rec, &out)
	if out.Skill.CurrentVersionID != "" || out.Skill.LifecycleStatus != skillStatusDeprecated {
		t.Errorf("after deprecating the only version: pointer %q, status %q",
			out.Skill.CurrentVersionID, out.Skill.LifecycleStatus)
	}
	// Deprecating it twice is a conflict, not a second success.
	rec = f.call(t, "POST", "/api/skills/"+sk.ID+"/versions/1/deprecate", f.adminCookie, "")
	if rec.Code != 409 {
		t.Errorf("deprecating twice: %d, want 409", rec.Code)
	}
}

// Legacy adoption is admin-only and, again, produces a draft.
func TestAdoptingALegacyPageIsAdminOnly(t *testing.T) {
	f := newSkillFixture(t)
	page := f.s.makePage(t, f.ws, f.adminID, "", "agent-browser", `{}`)

	rec := f.call(t, "POST", "/api/skills/adopt-page", f.memberCookie,
		fmt.Sprintf(`{"pageId":%q}`, page))
	if rec.Code != 403 {
		t.Errorf("a member adopted a legacy page: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.call(t, "POST", "/api/skills/adopt-page", f.adminCookie,
		fmt.Sprintf(`{"pageId":%q}`, page))
	if rec.Code != 200 {
		t.Fatalf("admin adopt: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Skill   skill        `json:"skill"`
		Version skillVersion `json:"version"`
		Note    string       `json:"note"`
	}
	decodeInto(t, rec, &out)
	if out.Version.Status != skillStatusDraft {
		t.Errorf("adopted as %q, want draft", out.Version.Status)
	}
	if !strings.Contains(out.Note, "must pass review") {
		t.Errorf("the answer does not say the adoption still needs review: %q", out.Note)
	}
	// The page it came from is kept as a reference rather than swallowed.
	if len(out.Version.References) != 1 || out.Version.References[0].Target != page {
		t.Errorf("the source page is not referenced: %+v", out.Version.References)
	}
	// An unknown page is a 404, not a 500.
	rec = f.call(t, "POST", "/api/skills/adopt-page", f.adminCookie, `{"pageId":"deadbeef"}`)
	if rec.Code != 404 {
		t.Errorf("adopting an unknown page: %d", rec.Code)
	}
}

func TestSkillRoutesRequireAuthentication(t *testing.T) {
	f := newSkillFixture(t)
	for _, path := range []string{"/api/skills", "/api/skills/review-queue"} {
		if rec := f.call(t, "GET", path, "", ""); rec.Code != 401 {
			t.Errorf("GET %s anonymously: %d, want 401", path, rec.Code)
		}
	}
	if rec := f.call(t, "POST", "/api/skills", "", `{"name":"x"}`); rec.Code != 401 {
		t.Errorf("POST /api/skills anonymously: %d, want 401", rec.Code)
	}
}

// errorCode reads the machine-readable `code` beside a failure. The interface
// branches on it rather than on the sentence, so a test that asserts on the
// sentence would pass while the browser showed a raw English string.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	if body.Code == "" && body.Error == "" {
		t.Fatalf("neither a code nor a message in %q", rec.Body.String())
	}
	return body.Code
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// namesOf lists the keys of a presence set, for a failure message that says
// what was actually returned rather than only how many rows there were.
func namesOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
