package server

import (
	"fmt"
	"strings"
	"testing"
)

// The resolver. Every test here is about the same property: given the same
// input, the answer is the same, and the answer explains itself.

// scopeOf and depsOf keep the fixtures readable.
func scopeOf(ws string, mutate func(*skillScope)) *skillScope {
	sc := skillScope{WorkspaceID: ws}
	mutate(&sc)
	return &sc
}

func selectedSlugs(res resolveResult) []string {
	out := make([]string, 0, len(res.Selected))
	for _, s := range res.Selected {
		out = append(out, fmt.Sprintf("%s@%d", s.Slug, s.Version))
	}
	return out
}

func excludedReason(res resolveResult, slug string) (string, string, bool) {
	for _, e := range res.Excluded {
		if e.Slug == slug {
			return e.ReasonCode, e.Reason, true
		}
	}
	return "", "", false
}

// The scoring from §20, checked as arithmetic rather than as an ordering: if
// the numbers are right the ordering follows, and a test on the ordering alone
// passes for the wrong reasons.
func TestResolverScoresTheWayItSaysItDoes(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Architecture review", skillDeliveryRemote,
		"Check the dependency direction.", func(in *skillDraftInput) {
			triggers := []string{"architecture", "dependency"}
			in.Triggers = &triggers
			in.Scope = scopeOf(f.ws, func(sc *skillScope) {
				sc.ProjectIDs = []string{"stockbook"}
				sc.Roles = []string{"reviewer"}
				sc.TaskTypes = []string{"code-review"}
				sc.AgentTypes = []string{"chatgpt"}
			})
		})
	f.publish(t, sk, v)

	res, err := f.s.resolveSkills(f.userOf(t, f.memberID), resolveRequest{
		WorkspaceID: f.ws, Task: "review the architecture and the dependency direction",
		Project: "stockbook", Role: "reviewer", TaskType: "code-review", Agent: "chatgpt",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Selected) != 1 {
		t.Fatalf("selected %v, want exactly architecture-review@1", selectedSlugs(res))
	}
	got := res.Selected[0]
	// 100 project + 40 role + 40 task type + 20+20 triggers + 10 agent = 230.
	want := scoreProjectMatch + scoreRoleMatch + scoreTaskType + 2*scoreTrigger + scoreAgentMatch
	if got.Score != want {
		breakdown := ""
		for _, r := range got.Reasons {
			breakdown += fmt.Sprintf("\n  +%d %s", r.Points, r.Label)
		}
		t.Errorf("score = %d, want %d. Breakdown:%s", got.Score, want, breakdown)
	}
	if len(got.Reasons) != 6 {
		t.Errorf("the breakdown has %d lines, want 6 — the Resolver test screen renders these", len(got.Reasons))
	}
	if got.ContentHash == "" || got.VersionID == "" {
		t.Error("a selection must name the exact version and its hash, or the run is not reproducible")
	}
}

// A trigger fires on a whole word, not on a substring. "api" inside "rapid" is
// the kind of match that makes a resolver look random.
func TestTriggersMatchWholeWords(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "API guidelines", skillDeliveryRemote, "Version your endpoints.",
		func(in *skillDraftInput) {
			triggers := []string{"api"}
			in.Triggers = &triggers
		})
	f.publish(t, sk, v)
	u := f.userOf(t, f.memberID)

	hit, err := f.s.resolveSkills(u, resolveRequest{WorkspaceID: f.ws, Task: "design the API for billing"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(hit.Selected) != 1 {
		t.Errorf("a task naming the API selected %v, want the skill", selectedSlugs(hit))
	}
	miss, err := f.s.resolveSkills(u, resolveRequest{WorkspaceID: f.ws, Task: "make rapid progress on therapies"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(miss.Selected) != 0 {
		t.Errorf("`rapid`/`therapies` matched the trigger `api`: %v", selectedSlugs(miss))
	}
}

// A candidate with nothing in its favour is not a match. Without this, a
// workspace-wide skill with no triggers would be handed to every task.
func TestASkillWithNoSignalIsNotSelected(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Generic advice", skillDeliveryRemote, "Be nice.", nil)
	f.publish(t, sk, v)

	res, err := f.s.resolveSkills(f.userOf(t, f.memberID), resolveRequest{
		WorkspaceID: f.ws, Task: "reconcile the ledger",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Selected) != 0 {
		t.Fatalf("a skill with no matching trigger or scope was selected: %v", selectedSlugs(res))
	}
	code, reason, ok := excludedReason(res, "generic-advice")
	if !ok {
		t.Fatal("the candidate was dropped without appearing in `excluded` — \"nothing matched\" with no reason is the least useful answer possible")
	}
	if code != "no_signal" {
		t.Errorf("exclusion code %q (%s), want no_signal", code, reason)
	}
	if res.Considered != 1 {
		t.Errorf("considered = %d, want 1 — the interface distinguishes \"none published\" from \"none matched\"", res.Considered)
	}
}

// A restriction the caller cannot answer is a MISS. Otherwise a narrow scope
// would stop meaning anything the moment a client forgot a field.
func TestAnUnansweredScopeRestrictionExcludes(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Stockbook only", skillDeliveryRemote, "Mind the ledger.",
		func(in *skillDraftInput) {
			triggers := []string{"ledger"}
			in.Triggers = &triggers
			in.Scope = scopeOf(f.ws, func(sc *skillScope) { sc.ProjectIDs = []string{"stockbook"} })
		})
	f.publish(t, sk, v)
	u := f.userOf(t, f.memberID)

	// No project named: excluded, and it says why.
	res, err := f.s.resolveSkills(u, resolveRequest{WorkspaceID: f.ws, Task: "fix the ledger"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Selected) != 0 {
		t.Fatalf("a project-scoped skill was selected for a context with no project: %v", selectedSlugs(res))
	}
	if code, _, _ := excludedReason(res, "stockbook-only"); code != "scope_project_unknown" {
		t.Errorf("exclusion code %q, want scope_project_unknown", code)
	}

	// The wrong project: also excluded, with a different reason.
	res, _ = f.s.resolveSkills(u, resolveRequest{WorkspaceID: f.ws, Task: "fix the ledger", Project: "warehouse"})
	if code, _, _ := excludedReason(res, "stockbook-only"); code != "scope_project" {
		t.Errorf("exclusion code %q for the wrong project, want scope_project", code)
	}

	// The right project: selected.
	res, _ = f.s.resolveSkills(u, resolveRequest{WorkspaceID: f.ws, Task: "fix the ledger", Project: "stockbook"})
	if len(res.Selected) != 1 {
		t.Fatalf("the right project selected %v, want the skill", selectedSlugs(res))
	}
}

func TestRepositoryPatternsMatchWithOneStar(t *testing.T) {
	for _, c := range []struct {
		pattern, value string
		want           bool
	}{
		{"acme/web", "acme/web", true},
		{"acme/web", "acme/api", false},
		{"acme/*", "acme/web", true},
		{"acme/*", "other/web", false},
		{"*-service", "billing-service", true},
		{"*-service", "billing-worker", false},
		{"infra/*/charts", "infra/prod/charts", true},
		{"infra/*/charts", "infra/prod/values", false},
		{"ACME/WEB", "acme/web", true}, // case-insensitive
		{"acme/web", "", false},
		{"", "acme/web", false},
	} {
		if got := matchesPattern(c.pattern, c.value); got != c.want {
			t.Errorf("matchesPattern(%q, %q) = %v, want %v", c.pattern, c.value, got, c.want)
		}
	}
}

// A missing REQUIRED dependency with no fallback excludes the skill, and the
// answer names the dependency. Never a silent drop, and never a pretence that
// the runtime has something it does not.
func TestAMissingRequiredDependencyExcludesAndExplains(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "UI review", skillDeliveryRemote, "Open the page and compare.",
		func(in *skillDraftInput) {
			triggers := []string{"ui"}
			in.Triggers = &triggers
			deps := []skillDependency{{Type: skillDepCapability, ID: "browser", Required: true}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)
	u := f.userOf(t, f.memberID)

	without, err := f.s.resolveSkills(u, resolveRequest{
		WorkspaceID: f.ws, Task: "review the UI", Capabilities: []string{"mcp"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(without.Selected) != 0 {
		t.Fatalf("a skill needing a browser was selected for a runtime without one: %v", selectedSlugs(without))
	}
	code, reason, ok := excludedReason(without, "ui-review")
	if !ok || code != "missing_required_dependency" {
		t.Fatalf("exclusion is (%q, %q), want missing_required_dependency", code, reason)
	}
	if len(without.Excluded[0].Missing) != 1 || without.Excluded[0].Missing[0].ID != "browser" {
		t.Errorf("the missing dependency is not named: %+v", without.Excluded[0].Missing)
	}

	with, _ := f.s.resolveSkills(u, resolveRequest{
		WorkspaceID: f.ws, Task: "review the UI", Capabilities: []string{"mcp", "browser"},
	})
	if len(with.Selected) != 1 {
		t.Fatalf("a runtime WITH a browser selected %v, want the skill", selectedSlugs(with))
	}
	if len(with.Selected[0].Missing) != 0 {
		t.Errorf("a satisfied dependency was reported as missing: %+v", with.Selected[0].Missing)
	}
}

// A declared fallback keeps the skill selectable, and the missing dependency is
// still reported alongside — the agent gets the instructions AND is told what
// it has not got.
func TestADeclaredFallbackKeepsTheSkillWithTheGapReported(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Design check", skillDeliveryRemote, "Compare against the mock.",
		func(in *skillDraftInput) {
			triggers := []string{"design"}
			in.Triggers = &triggers
			deps := []skillDependency{{
				Type: skillDepCapability, ID: "browser", Required: true,
				Fallback: "describe the difference from the screenshots instead of opening the page",
			}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)

	res, err := f.s.resolveSkills(f.userOf(t, f.memberID), resolveRequest{
		WorkspaceID: f.ws, Task: "check the design", Capabilities: []string{"mcp"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Selected) != 1 {
		t.Fatalf("a fallback did not keep the skill selectable: %v / excluded %+v", selectedSlugs(res), res.Excluded)
	}
	missing := res.Selected[0].Missing
	if len(missing) != 1 {
		t.Fatalf("the gap was not reported alongside the selection: %+v", missing)
	}
	if missing[0].Fallback == "" {
		t.Error("the fallback text is not passed on, so the agent cannot act on it")
	}
}

// Client-skill dependencies carry a version constraint, and an installed
// version that does not satisfy it is a MISS with the numbers named.
func TestClientSkillVersionConstraintsAreChecked(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Browser flow", skillDeliveryRemote, "Drive the browser.",
		func(in *skillDraftInput) {
			triggers := []string{"browser"}
			in.Triggers = &triggers
			deps := []skillDependency{{
				Type: skillDepClientSkill, ID: "agent-browser", VersionConstraint: ">=2.0.0", Required: true,
			}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)
	u := f.userOf(t, f.memberID)

	for _, c := range []struct {
		installed string
		want      bool
	}{
		{"2.1.0", true},
		{"2.0.0", true},
		{"3.0", true},
		{"1.9.9", false},
		{"", false},       // a runtime that will not name its version
		{"latest", false}, // …or names something unparseable
	} {
		res, err := f.s.resolveSkills(u, resolveRequest{
			WorkspaceID: f.ws, Task: "drive the browser",
			InstalledClientSkill: []installedClientSkill{{ID: "agent-browser", Version: c.installed}},
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		got := len(res.Selected) == 1
		if got != c.want {
			t.Errorf("installed agent-browser %q against >=2.0.0: selected = %v, want %v (%+v)",
				c.installed, got, c.want, res.Excluded)
		}
	}
}

func TestVersionConstraintForms(t *testing.T) {
	for _, c := range []struct {
		have, constraint string
		want             bool
	}{
		{"2.1.0", "", true}, // no constraint: anything satisfies it
		{"2.1.0", "2.1.0", true},
		{"2.1.1", "2.1.0", false},
		{"2.1.0", "=2.1.0", true},
		{"2.1.0", ">=2.1.0", true},
		{"2.0.9", ">=2.1.0", false},
		{"2.1.1", ">2.1.0", true},
		{"2.1.0", ">2.1.0", false},
		{"2.1", ">=2.1.0", true},   // shorter is padded with zeros
		{"2.1.0", "~2.1.0", false}, // a form we deliberately do not understand
		{"bananas", ">=1.0.0", false},
	} {
		if got := versionSatisfies(c.have, c.constraint); got != c.want {
			t.Errorf("versionSatisfies(%q, %q) = %v, want %v", c.have, c.constraint, got, c.want)
		}
	}
}

// Determinism, and the tie-breakers that make it real. Two identical calls, and
// two candidates with the same score.
func TestResolutionIsDeterministicIncludingTies(t *testing.T) {
	f := newSkillFixture(t)
	// Three skills that all score exactly one trigger, so only the tie-breaker
	// decides — and the tie-breaker must not be the map iteration order.
	for _, name := range []string{"Zebra check", "Alpha check", "Middle check"} {
		sk, v := f.makeSkill(t, f.memberID, name, skillDeliveryRemote, "Check it.", func(in *skillDraftInput) {
			triggers := []string{"audit"}
			in.Triggers = &triggers
		})
		f.publish(t, sk, v)
	}
	u := f.userOf(t, f.memberID)
	req := resolveRequest{WorkspaceID: f.ws, Task: "run the audit"}

	first, err := f.s.resolveSkills(u, req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"alpha-check@1", "middle-check@1", "zebra-check@1"}
	got := selectedSlugs(first)
	if len(got) != 3 {
		t.Fatalf("selected %v, want three tied candidates", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tied candidates came back as %v, want %v — ties must break on the slug, not on chance", got, want)
		}
	}
	for i := 0; i < 8; i++ {
		if again := selectedSlugs(mustResolve(t, f, u, req)); !equalStrings(again, got) {
			t.Fatalf("run %d returned %v, the first run returned %v", i+2, again, got)
		}
	}
}

// Top 1–3 by default, so a library of fifty skills does not answer with fifty.
func TestResolutionReturnsAtMostThree(t *testing.T) {
	f := newSkillFixture(t)
	for i := 0; i < 6; i++ {
		sk, v := f.makeSkill(t, f.memberID, fmt.Sprintf("Check %d", i), skillDeliveryRemote, "Check it.",
			func(in *skillDraftInput) {
				triggers := []string{"audit"}
				in.Triggers = &triggers
			})
		f.publish(t, sk, v)
	}
	u := f.userOf(t, f.memberID)
	res := mustResolve(t, f, u, resolveRequest{WorkspaceID: f.ws, Task: "audit"})
	if len(res.Selected) != 3 {
		t.Errorf("selected %d of 6 candidates, want the default cap of 3", len(res.Selected))
	}
	res = mustResolve(t, f, u, resolveRequest{WorkspaceID: f.ws, Task: "audit", Limit: 1})
	if len(res.Selected) != 1 {
		t.Errorf("limit 1 returned %d", len(res.Selected))
	}
	// A silly limit is clamped rather than honoured.
	res = mustResolve(t, f, u, resolveRequest{WorkspaceID: f.ws, Task: "audit", Limit: 500})
	if len(res.Selected) != 3 {
		t.Errorf("limit 500 returned %d, want the cap of 3", len(res.Selected))
	}
}

// A higher score wins, and the pinned version is the PUBLISHED one — not the
// newest row in the table.
func TestResolutionPinsThePublishedVersionNotTheNewest(t *testing.T) {
	f := newSkillFixture(t)
	sk, v1 := f.makeSkill(t, f.memberID, "Pinning", skillDeliveryRemote, "Version one.",
		func(in *skillDraftInput) {
			triggers := []string{"pin"}
			in.Triggers = &triggers
		})
	sk, approved1 := f.publish(t, sk, v1)
	// v2 exists as a draft with different text. It must be invisible.
	v2, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved1)
	if err != nil {
		t.Fatalf("new version: %v", err)
	}
	changed := "Version two, unreviewed."
	if _, err := f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, v2,
		skillDraftInput{Instructions: &changed}, v2.UpdatedAt); err != nil {
		t.Fatalf("edit draft: %v", err)
	}

	res := mustResolve(t, f, f.userOf(t, f.memberID), resolveRequest{WorkspaceID: f.ws, Task: "pin this"})
	if len(res.Selected) != 1 || res.Selected[0].Version != 1 {
		t.Fatalf("selected %v, want pinning@1 — the draft v2 must not be resolvable", selectedSlugs(res))
	}
	if res.Selected[0].ContentHash != approved1.ContentHash {
		t.Error("the pinned hash is not the approved version's hash")
	}
}

// AC-MCP-5: a resolution that pinned v1 keeps working after v2 is published.
// The old approved version stays approved and stays fetchable; only what a NEW
// resolution selects has changed.
func TestAnOlderApprovedVersionStaysFetchableAfterANewOne(t *testing.T) {
	f := newSkillFixture(t)
	sk, v1 := f.makeSkill(t, f.memberID, "Two versions", skillDeliveryRemote, "Do it the first way.",
		func(in *skillDraftInput) {
			triggers := []string{"either"}
			in.Triggers = &triggers
		})
	sk, approved1 := f.publish(t, sk, v1)
	v2, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved1)
	if err != nil {
		t.Fatalf("new version: %v", err)
	}
	second := "Do it the second way."
	v2, err = f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, v2,
		skillDraftInput{Instructions: &second}, v2.UpdatedAt)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	sk, _ = f.publish(t, sk, v2)

	agent := f.agentUser(t, f.memberID)
	// The pinned v1 still delivers v1's text, byte for byte.
	_, got, err := f.s.approvedRemoteVersion(agent, sk.ID, "1")
	if err != nil {
		t.Fatalf("the pinned older version is no longer fetchable: %v", err)
	}
	if got.Instructions != "Do it the first way." {
		t.Errorf("v1 now returns %q — an immutable version changed underneath a pinned resolution", got.Instructions)
	}
	// A NEW resolution gets v2.
	res := mustResolve(t, f, f.userOf(t, f.memberID), resolveRequest{WorkspaceID: f.ws, Task: "either way"})
	if len(res.Selected) != 1 || res.Selected[0].Version != 2 {
		t.Fatalf("a new resolution selected %v, want two-versions@2", selectedSlugs(res))
	}
}

// Resolving writes an audit row per selection, with the reason and WITHOUT the
// task text.
func TestResolvingIsAuditedWithoutTheTaskText(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Audited", skillDeliveryRemote, "Do it.", func(in *skillDraftInput) {
		triggers := []string{"secret"}
		in.Triggers = &triggers
	})
	f.publish(t, sk, v)

	secretTask := "handle the secret merger with Contoso for 4.2 million"
	agent := f.agentUser(t, f.memberID)
	if _, err := f.s.mcpSkillResolve(agent, resolveRequest{WorkspaceID: f.ws, Task: secretTask}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	entries, err := f.s.skillAudit(sk.ID, skillAuditFilter{Action: skillActionResolved})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d `resolved` audit rows, want 1", len(entries))
	}
	e := entries[0]
	if e.ResolutionReason == "" {
		t.Error("the audit row records no reason, so \"why was this selected\" is unanswerable afterwards")
	}
	if e.ContentHash == "" || e.VersionNumber != 1 {
		t.Errorf("the audit row does not pin the version: v%d hash %q", e.VersionNumber, e.ContentHash)
	}
	if e.ActorType != "agent" {
		t.Errorf("actor type %q, want agent", e.ActorType)
	}
	// The task is a digest, and the digest is stable for the same task.
	if e.TaskRef == "" {
		t.Error("no task reference at all — \"which task used this skill\" is then unanswerable")
	}
	for _, leak := range []string{"Contoso", "merger", "4.2"} {
		if strings.Contains(e.TaskRef, leak) || strings.Contains(e.ResolutionReason, leak) {
			t.Errorf("the audit row carries %q from the task text — prompts are not logged by default", leak)
		}
	}
	if e.TaskRef != taskFingerprint(secretTask) {
		t.Error("the fingerprint is not reproducible from the task, so two runs of the same task cannot be matched up")
	}
}

// Stored JSON that has somehow been corrupted must not crash the resolver or,
// worse, become a match. It degrades to an empty value, which cannot score.
func TestMalformedStoredScopeDegradesSafely(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Corrupt", skillDeliveryRemote, "Do it.", func(in *skillDraftInput) {
		triggers := []string{"corrupt"}
		in.Triggers = &triggers
	})
	sk, approved := f.publish(t, sk, v)
	if _, err := f.s.db.Exec(
		`UPDATE skill_versions SET scope = 'not json at all', dependencies = '{{{', triggers = 'nope' WHERE id = ?`,
		approved.ID); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	loaded, err := f.s.skillVersionByID(approved.ID)
	if err != nil {
		t.Fatalf("a corrupted row could not be read at all: %v — it must stay listable and deprecatable", err)
	}
	if loaded.Scope.WorkspaceID != "" || len(loaded.Dependencies) != 0 || len(loaded.Triggers) != 0 {
		t.Errorf("corrupted JSON produced values: %+v", loaded)
	}
	res, err := f.s.resolveSkills(f.userOf(t, f.memberID), resolveRequest{WorkspaceID: f.ws, Task: "corrupt"})
	if err != nil {
		t.Fatalf("resolve over a corrupted row: %v", err)
	}
	if len(res.Selected) != 0 {
		t.Errorf("a row with unreadable scope was selected anyway: %v", selectedSlugs(res))
	}
}

func mustResolve(t *testing.T, f *skillFixture, u *user, req resolveRequest) resolveResult {
	t.Helper()
	res, err := f.s.resolveSkills(u, req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
