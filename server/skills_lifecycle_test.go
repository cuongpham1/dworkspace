package server

import (
	"database/sql"
	"strings"
	"testing"
)

// The lifecycle, its invariants, and the two things that would quietly break
// them: a stale write and a second reviewer.

func TestSkillLifecycleWalksDraftPendingApproved(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Architecture review", skillDeliveryRemote,
		"Check the dependency direction before approving.", nil)

	if v.Status != skillStatusDraft {
		t.Fatalf("a new version starts as %q, want draft", v.Status)
	}
	if sk.CurrentVersionID != "" {
		t.Errorf("a brand-new skill already has a published pointer (%q) — nothing has been reviewed yet",
			sk.CurrentVersionID)
	}

	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if pending.Status != skillStatusPending {
		t.Fatalf("after submit the version is %q, want pending", pending.Status)
	}
	if pending.SubmittedAt == "" {
		t.Error("submitted_at was not recorded, so the review queue cannot order by it")
	}

	published, approved, err := f.s.approveVersion(f.userOf(t, f.adminID), sk, pending)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != skillStatusApproved {
		t.Fatalf("after approve the version is %q, want approved", approved.Status)
	}
	if approved.ApprovedBy != f.adminID {
		t.Errorf("approved_by = %q, want the reviewer %q", approved.ApprovedBy, f.adminID)
	}
	if published.CurrentVersionID != approved.ID || published.CurrentVersionNumber != 1 {
		t.Errorf("the published pointer is (%q, %d), want (%q, 1) — approving must move it",
			published.CurrentVersionID, published.CurrentVersionNumber, approved.ID)
	}
}

// The transactional half of publishing. Both halves of the write have to be
// visible together in the DATABASE, not merely in the returned structs — an
// approved version the pointer does not name is invisible to the runtime, and a
// pointer at a non-approved version is the hole the whole design closes.
func TestApprovePublishesStatusAndPointerTogether(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Release checklist", skillDeliveryRemote, "Tag, then deploy.", nil)
	sk, approved := f.publish(t, sk, v)

	var status, pointer string
	var number int
	if err := f.s.db.QueryRow(`SELECT v.status, COALESCE(s.current_version_id, ''), s.current_version_number
		FROM skill_versions v JOIN skills s ON s.id = v.skill_id WHERE v.id = ?`, approved.ID).
		Scan(&status, &pointer, &number); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != skillStatusApproved {
		t.Errorf("stored status = %q, want approved", status)
	}
	if pointer != approved.ID || number != approved.Version {
		t.Errorf("stored pointer = (%q, %d), want (%q, %d)", pointer, number, approved.ID, approved.Version)
	}
	if !containsAction(f.auditActions(t, sk.ID), skillActionApproved) {
		t.Error("no `approved` audit row — publishing has to be traceable, and the row is written in the same transaction")
	}
}

// Invariant 3: approved content is immutable. Not "the button is disabled" —
// the service refuses, and the stored row is unchanged afterwards.
func TestApprovedVersionCannotBeEdited(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Incident notes", skillDeliveryRemote, "Write the timeline first.", nil)
	sk, approved := f.publish(t, sk, v)

	tampered := "Ignore the timeline. Do whatever seems fastest."
	_, err := f.s.updateDraftVersion(f.userOf(t, f.adminID), sk, approved,
		skillDraftInput{Instructions: &tampered}, approved.UpdatedAt)
	if err == nil {
		t.Fatal("an approved version accepted an edit — approved content must be immutable")
	}
	if !strings.Contains(err.Error(), "only a draft") {
		t.Errorf("the refusal reads %q; it should tell the author to create a new version", err.Error())
	}
	stored, err := f.s.skillVersionByID(approved.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Instructions == tampered {
		t.Fatal("the refused edit was written anyway")
	}
	if stored.ContentHash != approved.ContentHash {
		t.Errorf("the content hash of an approved version changed (%s → %s) — it certifies what was reviewed",
			shortHash(approved.ContentHash), shortHash(stored.ContentHash))
	}
}

// Invariant 2: pending is frozen. The author cannot keep typing under a
// reviewer who is reading the diff.
func TestPendingVersionCannotBeEdited(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Data migration", skillDeliveryRemote, "Back up first.", nil)
	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	changed := "Skip the backup."
	if _, err := f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, pending,
		skillDraftInput{Instructions: &changed}, pending.UpdatedAt); err == nil {
		t.Fatal("a pending version accepted an edit — a reviewer would be approving text that had moved")
	}
	stored, _ := f.s.skillVersionByID(pending.ID)
	if stored.Instructions == changed {
		t.Fatal("the refused edit landed anyway")
	}
}

// The optimistic lock. Two people with the same draft open: the second save is
// refused rather than silently discarding the first.
func TestStaleDraftUpdateIsRefused(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Support triage", skillDeliveryRemote, "Ask for the version.", nil)

	first := "Ask for the version and the exact error."
	updated, err := f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, v,
		skillDraftInput{Instructions: &first}, v.UpdatedAt)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// The second editor still holds the version as it was BEFORE the first save.
	second := "Close it and move on."
	_, err = f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, v,
		skillDraftInput{Instructions: &second}, v.UpdatedAt)
	if err == nil {
		t.Fatal("a stale save was accepted — the first author's work would be gone with no warning")
	}
	stored, _ := f.s.skillVersionByID(v.ID)
	if stored.Instructions != updated.Instructions {
		t.Errorf("stored instructions are %q, want the first save %q", stored.Instructions, updated.Instructions)
	}
}

// A second reviewer arriving at the same pending version must not be able to
// approve it twice, and the version number must not be published twice.
func TestApprovingAnAlreadyDecidedVersionConflicts(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Contract review", skillDeliveryRemote, "Read the termination clause.", nil)
	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, err := f.s.approveVersion(f.userOf(t, f.adminID), sk, pending); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// The second reviewer holds the version as PENDING, which it no longer is.
	_, _, err = f.s.approveVersion(f.userOf(t, f.adminID), sk, pending)
	if err == nil {
		t.Fatal("the same version was approved twice — the second one had a stale view of its status")
	}
}

func TestRequestChangesReturnsToDraftWithTheReason(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Deploy runbook", skillDeliveryRemote, "Push to prod.", nil)
	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	back, err := f.s.requestChanges(f.userOf(t, f.adminID), sk, pending, "Name the rollback step.")
	if err != nil {
		t.Fatalf("request changes: %v", err)
	}
	if back.Status != skillStatusDraft {
		t.Fatalf("after request-changes the version is %q, want draft", back.Status)
	}
	if back.ReviewNote != "Name the rollback step." {
		t.Errorf("the review note is %q — the author has to see WHY in the editor", back.ReviewNote)
	}
	if back.SubmittedAt != "" {
		t.Error("submitted_at survived a return to draft, so the review queue would still list it")
	}
	// And it is editable again.
	fixed := "Push to prod, then verify /health. Roll back with the previous tag."
	if _, err := f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, back,
		skillDraftInput{Instructions: &fixed}, back.UpdatedAt); err != nil {
		t.Fatalf("a version returned to draft is not editable: %v", err)
	}
}

// Request-changes without a reason is refused. A reviewer who sends work back
// with no explanation has just made the author guess.
func TestRequestChangesNeedsAReason(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Pricing policy", skillDeliveryRemote, "Quote list price.", nil)
	pending, _ := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if _, err := f.s.requestChanges(f.userOf(t, f.adminID), sk, pending, "   "); err == nil {
		t.Fatal("a blank review note was accepted")
	}
}

func TestNewVersionClonesAndLeavesThePublishedOneLive(t *testing.T) {
	f := newSkillFixture(t)
	sk, v1 := f.makeSkill(t, f.memberID, "Onboarding", skillDeliveryRemote, "Send the welcome mail.", nil)
	sk, approved1 := f.publish(t, sk, v1)

	v2, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved1)
	if err != nil {
		t.Fatalf("new version: %v", err)
	}
	if v2.Version != 2 || v2.Status != skillStatusDraft {
		t.Fatalf("the clone is v%d/%s, want v2/draft", v2.Version, v2.Status)
	}
	if v2.Instructions != approved1.Instructions {
		t.Error("the clone did not carry the approved instructions forward, so the author starts from nothing")
	}
	reloaded, err := f.s.skillByID(sk.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.CurrentVersionID != approved1.ID {
		t.Errorf("starting a draft moved the published pointer to %q — v1 must stay live until v2 is approved",
			reloaded.CurrentVersionID)
	}
}

// One draft at a time. Two concurrent drafts would give the reviewer two
// candidates for the same number and nobody a clear answer.
func TestOnlyOneDraftAtATime(t *testing.T) {
	f := newSkillFixture(t)
	sk, v1 := f.makeSkill(t, f.memberID, "Retro format", skillDeliveryRemote, "Start with what worked.", nil)
	sk, approved := f.publish(t, sk, v1)
	if _, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved); err != nil {
		t.Fatalf("first new version: %v", err)
	}
	if _, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved); err == nil {
		t.Fatal("a second concurrent draft was created")
	}
}

func TestDeprecatingThePublishedVersionClearsThePointer(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Legacy export", skillDeliveryRemote, "Use the CSV job.", nil)
	sk, approved := f.publish(t, sk, v)

	sk, deprecated, err := f.s.deprecateVersion(f.userOf(t, f.adminID), sk, approved, "superseded by the API")
	if err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if deprecated.Status != skillStatusDeprecated {
		t.Fatalf("status = %q, want deprecated", deprecated.Status)
	}
	if sk.CurrentVersionID != "" {
		t.Error("the published pointer still names a deprecated version, so the runtime would keep selecting it")
	}
	if sk.LifecycleStatus != skillStatusDeprecated {
		t.Errorf("the skill's lifecycle label is %q, want deprecated — the library has to show it", sk.LifecycleStatus)
	}
	// History survives: that is the whole difference from deleting.
	versions, err := f.s.skillVersions(sk.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("version history: %v (%d versions), want the deprecated one still listed", err, len(versions))
	}
	if !containsAction(f.auditActions(t, sk.ID), skillActionDeprecated) {
		t.Error("no `deprecated` audit row")
	}
}

// Deprecating an OLDER approved version is tidying history and must not touch
// what is live.
func TestDeprecatingAnOldVersionLeavesTheCurrentOneAlone(t *testing.T) {
	f := newSkillFixture(t)
	sk, v1 := f.makeSkill(t, f.memberID, "Style guide", skillDeliveryRemote, "Oxford comma.", nil)
	sk, approved1 := f.publish(t, sk, v1)
	v2, err := f.s.newDraftVersion(f.userOf(t, f.memberID), sk, approved1)
	if err != nil {
		t.Fatalf("new version: %v", err)
	}
	sk, approved2 := f.publish(t, sk, v2)

	sk, _, err = f.s.deprecateVersion(f.userOf(t, f.adminID), sk, approved1, "old wording")
	if err != nil {
		t.Fatalf("deprecate v1: %v", err)
	}
	if sk.CurrentVersionID != approved2.ID {
		t.Errorf("deprecating v1 moved the pointer to %q, want v2 (%q)", sk.CurrentVersionID, approved2.ID)
	}
	if sk.LifecycleStatus != skillStatusApproved {
		t.Errorf("the skill reads as %q after an OLD version was deprecated, want approved", sk.LifecycleStatus)
	}
}

// ---- permissions ----

func TestOnlyAWorkspaceAdminCanApprove(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Budget approval", skillDeliveryRemote, "Check the cap.", nil)
	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, err := f.s.approveVersion(f.userOf(t, f.memberID), sk, pending); err == nil {
		t.Fatal("an ordinary member published a skill — publishing is the trust elevation and needs an admin")
	}
	stored, _ := f.s.skillVersionByID(pending.ID)
	if stored.Status != skillStatusPending {
		t.Errorf("the refused approval changed the status to %q", stored.Status)
	}
	if _, err := f.s.requestChanges(f.userOf(t, f.memberID), sk, pending, "no"); err == nil {
		t.Error("an ordinary member sent a version back — review is an admin act")
	}
}

func TestOnlyAWorkspaceAdminCanDeprecate(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Old policy", skillDeliveryRemote, "Do the old thing.", nil)
	sk, approved := f.publish(t, sk, v)
	if _, _, err := f.s.deprecateVersion(f.userOf(t, f.memberID), sk, approved, "no longer true"); err == nil {
		t.Fatal("an ordinary member deprecated a published skill")
	}
}

func TestAViewerCannotAuthorSkills(t *testing.T) {
	f := newSkillFixture(t)
	viewerID, _ := signedIn(t, f.s, "skill-viewer@example.test")
	if _, err := f.s.db.Exec(
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'viewer')`, f.ws, viewerID); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	name, description, delivery, instructions := "Viewer skill", "d", skillDeliveryRemote, "x"
	scope := skillScope{WorkspaceID: f.ws}
	if _, _, err := f.s.createSkill(f.userOf(t, viewerID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery,
		Instructions: &instructions, Scope: &scope,
	}); err == nil {
		t.Fatal("a read-only viewer created a skill")
	}
}

// Workspace isolation, through the service. The MCP and HTTP variants live in
// their own files; this is the one that proves the service itself does not leak.
func TestASkillIsInvisibleOutsideItsWorkspace(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Internal pricing", skillDeliveryRemote, "Never quote below cost.", nil)
	f.publish(t, sk, v)

	outsider := f.userOf(t, f.outsiderID)
	if f.s.skillCanRead(outsider, sk) {
		t.Fatal("somebody from another workspace may read this skill")
	}
	if _, _, err := f.s.approvedRemoteVersion(outsider, sk.ID, "1"); err == nil {
		t.Fatal("an outsider fetched the trusted instructions of another workspace's skill")
	}
	skills, err := f.s.skillsInWorkspaces(f.s.skillWorkspacesFor(outsider))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, listed := range skills {
		if listed.ID == sk.ID {
			t.Fatal("another workspace's skill appears in the outsider's library")
		}
	}
	res, err := f.s.resolveSkills(outsider, resolveRequest{Task: "quote a price"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Selected) != 0 || res.Considered != 0 {
		t.Fatalf("the resolver considered %d candidates and selected %d for an outsider — it must see none",
			res.Considered, len(res.Selected))
	}
}

// ---- content hash ----

func TestContentHashIsStableAndFollowsContent(t *testing.T) {
	f := newSkillFixture(t)
	_, v := f.makeSkill(t, f.memberID, "Hashing", skillDeliveryRemote, "Do the thing.", func(in *skillDraftInput) {
		triggers := []string{"hash", "digest"}
		in.Triggers = &triggers
	})
	first := skillContentHash("hashing", v)
	if first != v.ContentHash {
		t.Fatalf("stored hash %s differs from a recomputation %s", shortHash(v.ContentHash), shortHash(first))
	}
	if skillContentHash("hashing", v) != first {
		t.Fatal("hashing the same content twice gave two answers")
	}

	// Reordering a normalized list must NOT change the hash: the dependency
	// rows come out of a form in whatever order somebody typed them.
	withDeps := v
	withDeps.Dependencies = normalizeDependencies([]skillDependency{
		{Type: skillDepCapability, ID: "browser", Required: true},
		{Type: skillDepClientSkill, ID: "agent-browser", VersionConstraint: ">=2.0.0", Required: true},
	})
	a := skillContentHash("hashing", withDeps)
	reordered := v
	reordered.Dependencies = normalizeDependencies([]skillDependency{
		{Type: skillDepClientSkill, ID: "agent-browser", VersionConstraint: ">=2.0.0", Required: true},
		{Type: skillDepCapability, ID: "browser", Required: true},
	})
	if b := skillContentHash("hashing", reordered); a != b {
		t.Errorf("reordering the dependency rows changed the hash (%s vs %s)", shortHash(a), shortHash(b))
	}

	// And a real content change must change it, or the hash certifies nothing.
	changed := v
	changed.Instructions = "Do a different thing."
	if skillContentHash("hashing", changed) == first {
		t.Error("changing the instructions left the content hash the same")
	}
}

// An empty list and a nil list are the same content, so they must hash the
// same — otherwise "cleared the triggers" and "never had any" would look like
// different versions to a reviewer.
func TestContentHashTreatsNilAndEmptyAlike(t *testing.T) {
	base := skillVersion{Version: 1, DeliveryMode: skillDeliveryRemote, Instructions: "x"}
	withNil := base
	withEmpty := base
	withEmpty.Triggers = []string{}
	withEmpty.Dependencies = []skillDependency{}
	withEmpty.References = []skillReference{}
	if skillContentHash("s", withNil) != skillContentHash("s", withEmpty) {
		t.Error("nil and empty lists hash differently")
	}
}

// ---- validation ----

func TestSubmitRefusesAnIncompleteDraft(t *testing.T) {
	f := newSkillFixture(t)
	// A remote skill with no instructions: there is nothing to deliver.
	name, description, delivery, empty := "Empty remote", "does nothing", skillDeliveryRemote, ""
	scope := skillScope{WorkspaceID: f.ws}
	sk, v, err := f.s.createSkill(f.userOf(t, f.memberID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery, Instructions: &empty, Scope: &scope,
	})
	if err != nil {
		t.Fatalf("an empty draft could not even be SAVED: %v — half-finished work has to be storable", err)
	}
	if _, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v); err == nil {
		t.Fatal("a Remote Skill with no instructions was submitted for review")
	}
}

func TestMalformedDependencyIsRefusedAtTheDoor(t *testing.T) {
	f := newSkillFixture(t)
	name, description, delivery, instructions := "Bad deps", "d", skillDeliveryRemote, "x"
	scope := skillScope{WorkspaceID: f.ws}
	deps := []skillDependency{{Type: "sudo", ID: "root", Required: true}}
	_, _, err := f.s.createSkill(f.userOf(t, f.memberID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery, Instructions: &instructions,
		Scope: &scope, Dependencies: &deps,
	})
	if err == nil {
		t.Fatal("a dependency of an unknown type was accepted")
	}
	if !strings.Contains(err.Error(), "client_skill") {
		t.Errorf("the message %q should name the types that ARE allowed", err.Error())
	}

	badConstraint := []skillDependency{{Type: skillDepClientSkill, ID: "x", VersionConstraint: "~1.2"}}
	if _, _, err := f.s.createSkill(f.userOf(t, f.memberID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery, Instructions: &instructions,
		Scope: &scope, Dependencies: &badConstraint,
	}); err == nil {
		t.Fatal("an unparseable version constraint was accepted — the resolver would then fail closed silently")
	}
}

func TestSlugIsUniquePerWorkspaceAndLockedAfterPublish(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Duplicate me", skillDeliveryRemote, "x", nil)

	name, description, delivery, instructions := "Duplicate me", "d", skillDeliveryRemote, "x"
	scope := skillScope{WorkspaceID: f.ws}
	if _, _, err := f.s.createSkill(f.userOf(t, f.memberID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery, Instructions: &instructions, Scope: &scope,
	}); err == nil {
		t.Fatal("two skills with the same slug exist in one workspace — an agent naming the slug would get either")
	}

	// The same slug in ANOTHER workspace is fine: the slug is workspace-scoped.
	if _, err := f.s.db.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'admin')`,
		f.otherWS, f.memberID); err != nil {
		t.Fatalf("join other workspace: %v", err)
	}
	otherScope := skillScope{WorkspaceID: f.otherWS}
	if _, _, err := f.s.createSkill(f.userOf(t, f.memberID), f.otherWS, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery, Instructions: &instructions, Scope: &otherScope,
	}); err != nil {
		t.Fatalf("the same slug in a different workspace was refused: %v", err)
	}

	sk, _ = f.publish(t, sk, v)
	renamed := "something-else"
	if _, err := f.s.updateSkillMeta(f.userOf(t, f.adminID), sk, nil, &renamed, nil); err == nil {
		t.Fatal("the slug changed after a version was published — audit rows and pinned resolutions name it")
	}
	newName := "Duplicate me, renamed"
	if _, err := f.s.updateSkillMeta(f.userOf(t, f.adminID), sk, &newName, nil, nil); err != nil {
		t.Errorf("the NAME should still be editable after publishing: %v", err)
	}
}

// The audit trail as a sequence: every act of the lifecycle left a row.
func TestTheAuditTrailFollowsTheWholeLifecycle(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Traceable", skillDeliveryRemote, "Do it once.", nil)
	changed := "Do it once, carefully."
	v, err := f.s.updateDraftVersion(f.userOf(t, f.memberID), sk, v, skillDraftInput{Instructions: &changed}, v.UpdatedAt)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	pending, err := f.s.submitForReview(f.userOf(t, f.memberID), sk, v)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	back, err := f.s.requestChanges(f.userOf(t, f.adminID), sk, pending, "spell out `carefully`")
	if err != nil {
		t.Fatalf("request changes: %v", err)
	}
	pending, err = f.s.submitForReview(f.userOf(t, f.memberID), sk, back)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	sk, approved, err := f.s.approveVersion(f.userOf(t, f.adminID), sk, pending)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, _, err := f.s.deprecateVersion(f.userOf(t, f.adminID), sk, approved, "done with it"); err != nil {
		t.Fatalf("deprecate: %v", err)
	}

	want := []string{
		skillActionCreated, skillActionUpdated, skillActionSubmitted,
		skillActionChangesRequested, skillActionSubmitted, skillActionApproved, skillActionDeprecated,
	}
	got := f.auditActions(t, sk.ID)
	if len(got) != len(want) {
		t.Fatalf("audit trail is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit trail is %v, want %v", got, want)
		}
	}
	// Every row names the exact version and hash — that is what makes it useful.
	entries, err := f.s.skillAudit(sk.ID, skillAuditFilter{})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	for _, e := range entries {
		if e.SkillVersionID == "" || e.VersionNumber == 0 || e.ContentHash == "" {
			t.Errorf("audit row %q names version %q/%d hash %q — an audit that cannot identify the exact version answers nothing",
				e.Action, e.SkillVersionID, e.VersionNumber, e.ContentHash)
		}
	}
}

// A version id from ANOTHER skill must not be reachable through this skill's
// path — otherwise the skill id in the URL is decoration.
func TestAVersionCannotBeFetchedThroughTheWrongSkill(t *testing.T) {
	f := newSkillFixture(t)
	skA, vA := f.makeSkill(t, f.memberID, "Alpha", skillDeliveryRemote, "a", nil)
	skB, _ := f.makeSkill(t, f.memberID, "Beta", skillDeliveryRemote, "b", nil)
	if _, err := f.s.skillVersionRef(skB.ID, vA.ID); err != sql.ErrNoRows {
		t.Fatalf("skill B returned skill A's version (err = %v)", err)
	}
	if _, err := f.s.skillVersionRef(skA.ID, vA.ID); err != nil {
		t.Fatalf("the version could not be fetched through its own skill: %v", err)
	}
}
