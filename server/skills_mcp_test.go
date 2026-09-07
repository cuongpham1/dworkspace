package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// The MCP surface, exercised through s.mcpCall — the same entry point a real
// agent's tools/call lands in, so the argument decoding, the read-only token
// rule and the workspace policy are all in the path.

func mcpSkillCall(t *testing.T, f *skillFixture, u *user, name, args string) (string, error) {
	t.Helper()
	return f.s.mcpCall(u, name, json.RawMessage(args), "https://vus.example.com")
}

// Progressive disclosure, and the reason it is a TYPE rather than a
// convention: there is nowhere in catalogEntry to put an instruction body.
func TestCatalogReturnsMetadataAndNeverInstructions(t *testing.T) {
	f := newSkillFixture(t)
	secret := "STEP ONE: rotate the signing key using the procedure in the vault."
	sk, v := f.makeSkill(t, f.memberID, "Key rotation", skillDeliveryRemote, secret, func(in *skillDraftInput) {
		triggers := []string{"key", "rotation"}
		in.Triggers = &triggers
		deps := []skillDependency{{Type: skillDepCapability, ID: "shell", Required: true}}
		in.Dependencies = &deps
	})
	f.publish(t, sk, v)

	agent := f.agentUser(t, f.memberID)
	out, err := mcpSkillCall(t, f, agent, "skill_catalog", `{"workspace_id":"`+f.ws+`"}`)
	if err != nil {
		t.Fatalf("skill_catalog: %v", err)
	}
	if strings.Contains(out, "rotate the signing key") || strings.Contains(out, "STEP ONE") {
		t.Fatalf("the catalogue leaked the instruction body:\n%s", out)
	}
	// It does carry what an agent needs to decide what to ask for next.
	for _, want := range []string{"key-rotation", "Key rotation", "\"version\": 1", "capability:shell", "content_hash"} {
		if !strings.Contains(out, want) {
			t.Errorf("the catalogue never mentions %q:\n%s", want, out)
		}
	}
}

func TestResolveThenGetIsTheWholeLoop(t *testing.T) {
	f := newSkillFixture(t)
	body := "Read the diff before you comment. Name the file and the line."
	sk, v := f.makeSkill(t, f.memberID, "Architecture review", skillDeliveryRemote, body,
		func(in *skillDraftInput) {
			triggers := []string{"architecture"}
			in.Triggers = &triggers
			in.Scope = scopeOf(f.ws, func(sc *skillScope) { sc.Roles = []string{"reviewer"} })
		})
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	resolved, err := mcpSkillCall(t, f, agent, "skill_resolve",
		`{"task":"review the architecture of the billing service","role":"reviewer","agent":"chatgpt","workspace_id":"`+f.ws+`"}`)
	if err != nil {
		t.Fatalf("skill_resolve: %v", err)
	}
	var answer struct {
		Selected []struct {
			SkillID string `json:"skillId"`
			Version int    `json:"version"`
			Score   int    `json:"score"`
			Hash    string `json:"contentHash"`
			Reasons []struct {
				Label  string `json:"label"`
				Points int    `json:"points"`
			} `json:"reasons"`
		} `json:"selected"`
	}
	if err := json.Unmarshal([]byte(resolved), &answer); err != nil {
		t.Fatalf("skill_resolve did not answer JSON: %v\n%s", err, resolved)
	}
	if len(answer.Selected) != 1 {
		t.Fatalf("selected %d skills, want 1:\n%s", len(answer.Selected), resolved)
	}
	sel := answer.Selected[0]
	if sel.Score != scoreRoleMatch+scoreTrigger {
		t.Errorf("score %d, want %d (role + one trigger)", sel.Score, scoreRoleMatch+scoreTrigger)
	}
	if len(sel.Reasons) == 0 {
		t.Error("no scoring breakdown — the Resolver test screen renders these")
	}
	// The resolution must not carry the instructions either.
	if strings.Contains(resolved, "Read the diff") {
		t.Fatalf("skill_resolve leaked the instruction body:\n%s", resolved)
	}

	// And now the exact version it named.
	got, err := mcpSkillCall(t, f, agent, "skill_get",
		`{"skill_id":"`+sel.SkillID+`","version":"1"}`)
	if err != nil {
		t.Fatalf("skill_get: %v", err)
	}
	for _, want := range []string{
		"REMOTE SKILL — APPROVED", "skill: architecture-review", "version: 1",
		"content_hash: " + sel.Hash, "BEGIN APPROVED REMOTE SKILL", body, "END APPROVED REMOTE SKILL",
		"subordinate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the trusted envelope is missing %q:\n%s", want, got)
		}
	}
	// The fetch was audited.
	entries, err := f.s.skillAudit(sk.ID, skillAuditFilter{Action: skillActionFetched})
	if err != nil || len(entries) != 1 {
		t.Fatalf("`fetched` audit rows: %d (%v), want 1", len(entries), err)
	}
	if entries[0].ContentHash != sel.Hash {
		t.Error("the audit row's hash does not match the delivered version")
	}
}

// There is no "latest". An agent that asks for a skill without naming a version
// is refused, because otherwise the version the audit records and the version
// that ran could differ.
func TestSkillGetRefusesToGuessAVersion(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Pinned", skillDeliveryRemote, "Exactly this.", nil)
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	if _, err := mcpSkillCall(t, f, agent, "skill_get", `{"skill_id":"`+sk.ID+`"}`); err == nil {
		t.Fatal("skill_get without a version returned something")
	}
	if _, err := mcpSkillCall(t, f, agent, "skill_get", `{"version":"1"}`); err == nil {
		t.Fatal("skill_get without a skill returned something")
	}
	// A version that does not exist is a clear refusal, not the nearest one.
	out, err := mcpSkillCall(t, f, agent, "skill_get", `{"skill_id":"`+sk.ID+`","version":"9"}`)
	if err == nil {
		t.Fatalf("version 9 of a one-version skill returned:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no version 9") {
		t.Errorf("the refusal reads %q; it should name the version that is missing", err.Error())
	}
}

// A client sending `"version": 4` as a NUMBER must work. Typing the field as a
// string would fail the whole argument decode and tell the agent only
// "invalid arguments".
func TestSkillGetAcceptsAVersionSentAsANumber(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Numeric", skillDeliveryRemote, "Numbers are fine.", nil)
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	out, err := mcpSkillCall(t, f, agent, "skill_get", `{"skill_id":"`+sk.ID+`","version":1}`)
	if err != nil {
		t.Fatalf("a numeric version was refused: %v", err)
	}
	if !strings.Contains(out, "Numbers are fine.") {
		t.Errorf("unexpected answer:\n%s", out)
	}
}

// Fetching by slug works too — an agent that read `architecture-review@4`
// somewhere has the slug, not the opaque id.
func TestSkillGetAcceptsASlug(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Slug lookup", skillDeliveryRemote, "Found by slug.", nil)
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	out, err := mcpSkillCall(t, f, agent, "skill_get", `{"slug":"slug-lookup","version":"1"}`)
	if err != nil {
		t.Fatalf("slug lookup: %v", err)
	}
	if !strings.Contains(out, "Found by slug.") {
		t.Errorf("unexpected answer:\n%s", out)
	}
	_ = sk
	// A slug in a workspace the caller cannot see is not found.
	outsider := f.agentUser(t, f.outsiderID)
	if _, err := mcpSkillCall(t, f, outsider, "skill_get", `{"slug":"slug-lookup","version":"1"}`); err == nil {
		t.Fatal("an outsider resolved a slug from another workspace")
	}
}

func TestClientPackageOverMCPDescribesRatherThanShips(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Browser driver", skillDeliveryClient,
		"Run the driver against the target URL.", func(in *skillDraftInput) {
			pkg := skillPackageMetadata{
				Entrypoint: "scripts/drive.sh",
				Scripts:    []skillPackedFile{{Name: "drive.sh", Content: "#!/bin/sh\necho driving\n"}},
			}
			in.Package = &pkg
			deps := []skillDependency{{Type: skillDepCapability, ID: "shell", Required: true}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	out, err := mcpSkillCall(t, f, agent, "skill_client_package", `{"skill_id":"`+sk.ID+`","version":"1"}`)
	if err != nil {
		t.Fatalf("skill_client_package: %v", err)
	}
	var answer struct {
		Package skillPackageInfo `json:"package"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	p := answer.Package
	if p.Manifest.ContentHash != v.ContentHash && p.Manifest.Version != 1 {
		t.Errorf("the manifest does not pin the version: %+v", p.Manifest)
	}
	if p.ArchiveHash == "" || p.SizeBytes == 0 {
		t.Error("no archive hash or size — an agent cannot tell whether it already has this")
	}
	if !strings.HasPrefix(p.DownloadURL, "https://vus.example.com/api/skills/") {
		t.Errorf("download URL %q does not point at this instance", p.DownloadURL)
	}
	wantFiles := map[string]bool{
		"browser-driver/SKILL.md": true, "browser-driver/manifest.json": true,
		"browser-driver/scripts/drive.sh": true,
	}
	for _, file := range p.Files {
		delete(wantFiles, file.Name)
	}
	if len(wantFiles) > 0 {
		t.Errorf("the package is missing %v (got %+v)", wantFiles, p.Files)
	}
	// The BYTES do not travel through the tool result.
	if strings.Contains(out, "echo driving") {
		t.Error("the script's contents were inlined into the MCP answer — that is an agent's context spent on a file it will not read")
	}
	if !strings.Contains(out, "Nothing is installed for you") {
		t.Error("the answer does not say that VUS installs nothing — an agent should ask before writing to somebody's machine")
	}
	// Describing a package is not downloading one, so no download row is
	// written here. The split is deliberate: the audit trail should answer "a
	// package left the workspace", and an agent comparing hashes to find out
	// that it already has the right version has not taken anything.
	if containsAction(f.auditActions(t, sk.ID), skillActionPackageDownloaded) {
		t.Error("merely describing a package recorded a download — the audit would then overcount")
	}
}

// The MCP tools are visible in tools/list, so a client can find them at all.
func TestSkillToolsAreAdvertised(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range mcpTools {
		if n, ok := tool["name"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{"skill_catalog", "skill_resolve", "skill_get", "skill_client_package"} {
		if !names[want] {
			t.Errorf("%s is not in the advertised tool list", want)
		}
	}
}

// Every skill tool must be reachable through the real dispatch — a tool
// advertised but not wired up is worse than one that does not exist.
func TestEverySkillToolIsWiredUp(t *testing.T) {
	f := newSkillFixture(t)
	agent := f.agentUser(t, f.memberID)
	for _, c := range []struct{ name, args string }{
		{"skill_catalog", `{}`},
		{"skill_resolve", `{"task":"anything"}`},
	} {
		if _, err := mcpSkillCall(t, f, agent, c.name, c.args); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// The two that need a target answer with a REFUSAL, not "unknown tool".
	for _, c := range []struct{ name, args string }{
		{"skill_get", `{"skill_id":"nope","version":"1"}`},
		{"skill_client_package", `{"skill_id":"nope","version":"1"}`},
	} {
		_, err := mcpSkillCall(t, f, agent, c.name, c.args)
		if err == nil {
			t.Errorf("%s with a bogus id returned success", c.name)
			continue
		}
		if strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("%s is advertised but not dispatched", c.name)
		}
	}
}

// Cross-workspace, over the MCP surface this time: knowing an id is not access.
func TestMCPRefusesASkillFromAnotherWorkspace(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Ours alone", skillDeliveryRemote, "Internal only.", nil)
	f.publish(t, sk, v)

	outsider := f.agentUser(t, f.outsiderID)
	out, err := mcpSkillCall(t, f, outsider, "skill_get", `{"skill_id":"`+sk.ID+`","version":"1"}`)
	if err == nil {
		t.Fatalf("an outsider fetched another workspace's skill:\n%s", out)
	}
	if strings.Contains(err.Error(), "Internal only") {
		t.Error("the refusal echoed the instructions")
	}
	catalog, err := mcpSkillCall(t, f, outsider, "skill_catalog", `{}`)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if strings.Contains(catalog, "ours-alone") {
		t.Fatalf("another workspace's skill is in the outsider's catalogue:\n%s", catalog)
	}
	// Asking for the workspace by name is refused rather than silently widened.
	if _, err := mcpSkillCall(t, f, outsider, "skill_catalog", `{"workspace_id":"`+f.ws+`"}`); err == nil {
		t.Fatal("an outsider listed a workspace they are not a member of")
	}
}

// An empty library answers with a sentence, not an empty array an agent has to
// interpret.
func TestAnEmptyLibrarySaysSo(t *testing.T) {
	f := newSkillFixture(t)
	agent := f.agentUser(t, f.memberID)
	out, err := mcpSkillCall(t, f, agent, "skill_catalog", `{"workspace_id":"`+f.ws+`"}`)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if !strings.Contains(out, "No approved skills") {
		t.Errorf("an empty catalogue answered %q", out)
	}
	resolved, err := mcpSkillCall(t, f, agent, "skill_resolve", `{"task":"anything","workspace_id":"`+f.ws+`"}`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved, "No approved skill is published") {
		t.Errorf("resolving against an empty library answered:\n%s", resolved)
	}
}

// Missing dependencies are lifted to the top level of the answer as well, so an
// agent reading only the summary cannot miss why a skill it expected is absent.
func TestResolveSurfacesMissingDependenciesAtTheTopLevel(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Needs browser", skillDeliveryRemote, "Open the page.",
		func(in *skillDraftInput) {
			triggers := []string{"page"}
			in.Triggers = &triggers
			deps := []skillDependency{{Type: skillDepCapability, ID: "browser", Required: true}}
			in.Dependencies = &deps
		})
	f.publish(t, sk, v)
	agent := f.agentUser(t, f.memberID)

	out, err := mcpSkillCall(t, f, agent, "skill_resolve",
		`{"task":"open the page and check it","workspace_id":"`+f.ws+`","capabilities":["mcp"]}`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var answer struct {
		Selected []any `json:"selected"`
		Missing  []struct {
			ID     string `json:"id"`
			Detail string `json:"detail"`
		} `json:"missing_dependencies"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(answer.Selected) != 0 {
		t.Fatal("a skill requiring a browser was selected for a runtime without one")
	}
	if len(answer.Missing) != 1 || answer.Missing[0].ID != "browser" {
		t.Fatalf("missing_dependencies = %+v, want the browser named", answer.Missing)
	}
	if answer.Missing[0].Detail == "" {
		t.Error("the missing dependency has no human-readable detail")
	}
	if !strings.Contains(answer.Note, "excluded") {
		t.Errorf("the note %q does not point at the exclusions", answer.Note)
	}
}
