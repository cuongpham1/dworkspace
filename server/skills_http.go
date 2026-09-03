package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The HTTP surface the interface uses.
//
// Every handler here does three things and nothing else: read the request, call
// the Skill Service, write the answer. No SQL, no lifecycle rule, no permission
// decision — those live in skills_service.go, so the browser and an agent over
// MCP are held to the same rules by the same code.
//
// The write half is sessionOnly (see routes in server.go). That is the same
// judgement workspace rules are held to and for the same reason: an approved
// Remote Skill is text agents follow, so an agent's own credential must not be
// able to author, submit or approve one. An API token may READ the library,
// resolve, fetch approved instructions and download packages — everything it
// needs to work, and nothing that would let it widen its own instructions.

// ---- wire shapes ----

// skillListEntry is one row of the library. It carries what the list has to
// SHOW — delivery, status, published version, scope summary, dependency health
// — so that answering "can this be used and by whom" never needs a second
// request per row.
//
// It deliberately does NOT carry `instructions`. That is progressive disclosure
// held at the API boundary rather than trusted to the client: a library of a
// hundred skills must not put a hundred instruction bodies on the wire, and the
// endpoint that cannot leak them is the one that never selects them.
type skillListEntry struct {
	skill
	Delivery         string   `json:"delivery"`
	DraftVersion     int      `json:"draftVersion,omitempty"`
	PendingVersion   int      `json:"pendingVersion,omitempty"`
	Triggers         []string `json:"triggers"`
	ScopeSummary     []string `json:"scopeSummary"`
	DependencyIssues []string `json:"dependencyIssues"`
	VersionCount     int      `json:"versionCount"`
	CanReview        bool     `json:"canReview"`
	CanAuthor        bool     `json:"canAuthor"`
}

// skillDetail is the detail screen's payload: the skill, every version, and
// what the caller is allowed to do.
type skillDetail struct {
	Skill     skill          `json:"skill"`
	Versions  []skillVersion `json:"versions"`
	Current   *skillVersion  `json:"current,omitempty"`
	Draft     *skillVersion  `json:"draft,omitempty"`
	Pending   *skillVersion  `json:"pending,omitempty"`
	CanReview bool           `json:"canReview"`
	CanAuthor bool           `json:"canAuthor"`
}

// reviewQueueEntry is one pending version plus the DETERMINISTIC risk summary.
//
// Deterministic, and computed on the server, because a risk flag is only useful
// if it means the same thing every time. "Delivery changed Client → Remote" is
// a fact about two rows; a score a model produced is a guess a reviewer would
// have to check anyway. §10.1 of the PRD says the same.
type reviewQueueEntry struct {
	Skill        skill         `json:"skill"`
	Version      skillVersion  `json:"version"`
	Previous     *skillVersion `json:"previous,omitempty"`
	Risks        []string      `json:"risks"`
	CanReview    bool          `json:"canReview"`
	FirstPublish bool          `json:"firstPublish"`
}

// ---- helpers ----

func (s *Server) skillOr404(w http.ResponseWriter, r *http.Request) (skill, *user, bool) {
	u := requestUser(r)
	sk, err := s.skillByID(r.PathValue("skillId"))
	if err == sql.ErrNoRows {
		httpErrorCode(w, http.StatusNotFound, "skill_not_found", "skill not found")
		return skill{}, u, false
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return skill{}, u, false
	}
	// Workspace isolation, enforced here for every route that names a skill:
	// a caller who is not a member gets the same answer as for an id that does
	// not exist.
	if !s.skillCanRead(u, sk) {
		httpErrorCode(w, http.StatusNotFound, "skill_not_found", "skill not found")
		return skill{}, u, false
	}
	return sk, u, true
}

func (s *Server) skillVersionOr404(w http.ResponseWriter, sk skill, ref string) (skillVersion, bool) {
	v, err := s.skillVersionRef(sk.ID, ref)
	if err == sql.ErrNoRows {
		httpErrorCode(w, http.StatusNotFound, "skill_version_not_found", "that skill version does not exist")
		return skillVersion{}, false
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return skillVersion{}, false
	}
	return v, true
}

// writeSkillError turns a service error into an HTTP status. The codes travel
// so the browser can say something actionable in the reader's language (see
// serverErrors.ts) instead of showing a raw sentence about hashes.
func writeSkillError(w http.ResponseWriter, err error) {
	var ce *codedError
	code := ""
	if errors.As(err, &ce) {
		code = ce.code
	}
	switch code {
	case "skill_not_found", "skill_version_not_found":
		httpErrorFrom(w, http.StatusNotFound, err)
	case "skill_forbidden", "skill_review_only":
		httpErrorFrom(w, http.StatusForbidden, err)
	case "skill_conflict", "skill_draft_exists", "skill_pending_exists", "skill_slug_taken":
		httpErrorFrom(w, http.StatusConflict, err)
	case "skill_not_draft", "skill_not_pending", "skill_not_approved":
		httpErrorFrom(w, http.StatusConflict, err)
	case "":
		httpError(w, http.StatusInternalServerError, err.Error())
	default:
		httpErrorFrom(w, http.StatusBadRequest, err)
	}
}

// scopeSummary is the one-line "where does this apply" the library column
// shows. Kept short on purpose: a cell that lists twelve repositories is a cell
// nobody reads.
func scopeSummary(sc skillScope) []string {
	out := []string{}
	add := func(label string, values []string) {
		if len(values) == 0 {
			return
		}
		if len(values) <= 2 {
			out = append(out, label+": "+strings.Join(values, ", "))
			return
		}
		out = append(out, fmt.Sprintf("%s: %s +%d", label, strings.Join(values[:2], ", "), len(values)-2))
	}
	add("project", sc.ProjectIDs)
	add("repo", sc.RepositoryPatterns)
	add("role", sc.Roles)
	add("task", sc.TaskTypes)
	add("agent", sc.AgentTypes)
	if len(out) == 0 {
		out = append(out, "workspace")
	}
	return out
}

// dependencyIssues lists the required dependencies that would need something
// from the runtime. It is not a claim that anything is broken — the server
// cannot see a client's capabilities — but it is what the library's warning
// icon should mean: this skill will only work where these exist.
func dependencyIssues(deps []skillDependency) []string {
	out := []string{}
	for _, d := range deps {
		if !d.Required {
			continue
		}
		if d.Fallback != "" {
			continue
		}
		label := d.Type + " " + d.ID
		if d.VersionConstraint != "" {
			label += " " + d.VersionConstraint
		}
		out = append(out, label)
	}
	return out
}

// ---- reads ----

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	u := requestUser(r)
	workspaces := s.skillWorkspacesFor(u)
	if ws := r.URL.Query().Get("workspace"); ws != "" {
		filtered := []string{}
		for _, candidate := range workspaces {
			if candidate == ws {
				filtered = append(filtered, candidate)
			}
		}
		workspaces = filtered
	}
	skills, err := s.skillsInWorkspaces(workspaces)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	delivery := strings.ToLower(r.URL.Query().Get("delivery"))
	status := strings.ToLower(r.URL.Query().Get("status"))
	owner := r.URL.Query().Get("owner")

	out := []skillListEntry{}
	for _, sk := range skills {
		if owner != "" && sk.OwnerID != owner {
			continue
		}
		versions, err := s.skillVersions(sk.ID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		entry := skillListEntry{
			skill: sk, Triggers: []string{}, ScopeSummary: []string{}, DependencyIssues: []string{},
			VersionCount: len(versions),
			CanReview:    s.skillCanReview(u, sk),
			CanAuthor:    s.skillCanAuthor(u, sk.WorkspaceID),
		}
		// The version the row DESCRIBES: the published one if there is one,
		// otherwise the newest, so a skill that has never been approved still
		// shows its delivery mode and scope instead of an empty row.
		describing := skillVersion{}
		for _, v := range versions {
			switch v.Status {
			case skillStatusDraft:
				if v.Version > entry.DraftVersion {
					entry.DraftVersion = v.Version
				}
			case skillStatusPending:
				if v.Version > entry.PendingVersion {
					entry.PendingVersion = v.Version
				}
			}
			if v.ID == sk.CurrentVersionID {
				describing = v
			}
		}
		if describing.ID == "" && len(versions) > 0 {
			describing = versions[0] // skillVersions sorts version DESC
		}
		entry.Delivery = describing.DeliveryMode
		entry.Triggers = orEmptyStrings(describing.Triggers)
		entry.ScopeSummary = scopeSummary(describing.Scope)
		entry.DependencyIssues = dependencyIssues(describing.Dependencies)

		if delivery != "" && entry.Delivery != delivery {
			continue
		}
		if status != "" && !skillMatchesStatusFilter(entry, status) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(sk.Name+" "+sk.Slug+" "+sk.Description+" "+
			strings.Join(entry.Triggers, " ")), query) {
			continue
		}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

// skillMatchesStatusFilter answers the library's status chips. `draft` and
// `pending` ask about a version in flight rather than the skill's own label,
// because that is what somebody filtering for "what needs my attention" means.
func skillMatchesStatusFilter(e skillListEntry, status string) bool {
	switch status {
	case skillStatusDraft:
		return e.DraftVersion > 0
	case skillStatusPending:
		return e.PendingVersion > 0
	case skillStatusApproved:
		return e.CurrentVersionID != ""
	case skillStatusDeprecated:
		return e.LifecycleStatus == skillStatusDeprecated && e.CurrentVersionID == ""
	}
	return true
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	versions, err := s.skillVersions(sk.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail := skillDetail{
		Skill: sk, Versions: versions,
		CanReview: s.skillCanReview(u, sk), CanAuthor: s.skillCanAuthor(u, sk.WorkspaceID),
	}
	for i := range versions {
		v := versions[i]
		switch {
		case v.ID == sk.CurrentVersionID:
			detail.Current = &versions[i]
		case v.Status == skillStatusDraft && detail.Draft == nil:
			detail.Draft = &versions[i]
		case v.Status == skillStatusPending && detail.Pending == nil:
			detail.Pending = &versions[i]
		}
	}
	writeJSON(w, detail)
}

func (s *Server) handleListSkillVersions(w http.ResponseWriter, r *http.Request) {
	sk, _, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	versions, err := s.skillVersions(sk.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, versions)
}

func (s *Server) handleGetSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, _, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
	if !ok {
		return
	}
	writeJSON(w, v)
}

// handleSkillReviewQueue lists what is waiting, with the risk summary.
func (s *Server) handleSkillAudit(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	// Reading the audit is a member's right in their own workspace — the same
	// bargain /api/audit strikes. It names skills, versions and agents, never
	// the task text (see taskFingerprint).
	_ = u
	q := r.URL.Query()
	f := skillAuditFilter{
		Action: q.Get("action"), Agent: q.Get("agent"),
		Since: q.Get("since"), Until: q.Get("until"),
	}
	if v, err := strconv.Atoi(q.Get("version")); err == nil {
		f.Version = v
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		f.Limit = v
	}
	entries, err := s.skillAudit(sk.ID, f)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, entries)
}

// ---- authoring ----

// skillVersionBody is the JSON an author's form sends. Pointers throughout, so
// a PATCH that mentions only the instructions leaves the scope alone instead of
// clearing it.
type skillVersionBody struct {
	Name         *string               `json:"name"`
	Slug         *string               `json:"slug"`
	Description  *string               `json:"description"`
	DeliveryMode *string               `json:"deliveryMode"`
	Instructions *string               `json:"instructions"`
	Triggers     *[]string             `json:"triggers"`
	References   *[]skillReference     `json:"references"`
	Dependencies *[]skillDependency    `json:"dependencies"`
	Scope        *skillScope           `json:"scope"`
	Package      *skillPackageMetadata `json:"packageMetadata"`
	WorkspaceID  string                `json:"workspaceId"`
	FromVersion  string                `json:"fromVersion"`
	UpdatedAt    string                `json:"updatedAt"`
	Note         string                `json:"note"`
}

func (b skillVersionBody) input() skillDraftInput {
	return skillDraftInput{
		Name: b.Name, Slug: b.Slug, Description: b.Description, DeliveryMode: b.DeliveryMode,
		Instructions: b.Instructions, Triggers: b.Triggers, References: b.References,
		Dependencies: b.Dependencies, Scope: b.Scope, Package: b.Package,
	}
}

func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var body skillVersionBody
	if err := decodeJSON(w, r, &body); err != nil {
		httpErrorCode(w, http.StatusBadRequest, "bad_json", "invalid JSON")
		return
	}
	sk, v, err := s.createSkill(requestUser(r), body.WorkspaceID, body.input())
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, skillDetail{
		Skill: sk, Versions: []skillVersion{v}, Draft: &v,
		CanReview: s.skillCanReview(requestUser(r), sk), CanAuthor: true,
	})
}

// handleCreateSkillVersion starts a new draft from an existing version — the
// only route to changing anything that has been approved.
func (s *Server) handleCreateSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	var body skillVersionBody
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			httpErrorCode(w, http.StatusBadRequest, "bad_json", "invalid JSON")
			return
		}
	}
	from := skillVersion{DeliveryMode: skillDeliveryRemote}
	if body.FromVersion != "" {
		loaded, ok := s.skillVersionOr404(w, sk, body.FromVersion)
		if !ok {
			return
		}
		from = loaded
	} else if sk.CurrentVersionID != "" {
		loaded, err := s.skillVersionByID(sk.CurrentVersionID)
		if err == nil {
			from = loaded
		}
	}
	v, err := s.newDraftVersion(u, sk, from)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleUpdateSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
	if !ok {
		return
	}
	var body skillVersionBody
	if err := decodeJSON(w, r, &body); err != nil {
		httpErrorCode(w, http.StatusBadRequest, "bad_json", "invalid JSON")
		return
	}
	// The logical skill's own fields travel on the same PATCH, because to the
	// author renaming the skill and rewriting its instructions are one edit.
	if body.Name != nil || body.Slug != nil {
		updated, err := s.updateSkillMeta(u, sk, body.Name, body.Slug, nil)
		if err != nil {
			writeSkillError(w, err)
			return
		}
		sk = updated
	}
	next, err := s.updateDraftVersion(u, sk, v, body.input(), body.UpdatedAt)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	// Description lives in both places: on the version (it is reviewed and
	// hashed with the rest) and on the skill (so the library can show it
	// without loading a version). The skill's copy follows the draft until
	// something is published, at which point approving sets it from the
	// approved version — never the other way round.
	if body.Description != nil && sk.CurrentVersionID == "" {
		if updated, err := s.updateSkillMeta(u, sk, nil, nil, body.Description); err == nil {
			sk = updated
		}
	}
	writeJSON(w, map[string]any{"skill": sk, "version": next})
}

func (s *Server) handleSubmitSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
	if !ok {
		return
	}
	next, err := s.submitForReview(u, sk, v)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, next)
}

// ---- tools ----

// resolveTestBody is the Resolver test form. The same shape MCP's skill_resolve
// takes, because it reaches the same function — the screen exists to show what
// an agent would get, and a different input shape would make that a lie.
type resolveTestBody struct {
	WorkspaceID           string                 `json:"workspaceId"`
	Task                  string                 `json:"task"`
	Project               string                 `json:"project"`
	Repository            string                 `json:"repository"`
	Role                  string                 `json:"role"`
	TaskType              string                 `json:"taskType"`
	Agent                 string                 `json:"agent"`
	Capabilities          []string               `json:"capabilities"`
	InstalledClientSkills []installedClientSkill `json:"installedClientSkills"`
	Limit                 int                    `json:"limit"`
}

func (s *Server) handleSkillResolveTest(w http.ResponseWriter, r *http.Request) {
	var body resolveTestBody
	if err := decodeJSON(w, r, &body); err != nil {
		httpErrorCode(w, http.StatusBadRequest, "bad_json", "invalid JSON")
		return
	}
	u := requestUser(r)
	req := resolveRequest{
		WorkspaceID: body.WorkspaceID, Task: body.Task, Project: body.Project,
		Repository: body.Repository, Role: body.Role, TaskType: body.TaskType, Agent: body.Agent,
		Capabilities: normalizeStrings(body.Capabilities), InstalledClientSkill: body.InstalledClientSkills,
		Limit: body.Limit,
	}
	res, err := s.resolveSkills(u, req)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A dry run is audited as a resolution like any other, with the actor
	// recorded as the person who ran it. Leaving it out would make the audit
	// trail disagree with itself about how often a skill was selected, and
	// hiding a human's dry run while recording an agent's is the wrong way
	// round.
	s.recordResolution(u, body.WorkspaceID, req, res, "human")
	writeJSON(w, res)
}

// handleSkillPackage serves the deterministic client package as a zip.
//
// Open to an API token as well as a session: an agent that resolved a Client
// Skill needs the package, and downloading an approved package is a read. What
// a token cannot do is create, submit or approve one.
func (s *Server) handleSkillPackage(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	_, v, err := s.approvedClientVersion(u, sk.ID, r.PathValue("versionId"))
	if err != nil {
		writeSkillError(w, err)
		return
	}
	archive, _, _, err := buildSkillPackage(sk, v)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	// The audit row comes FIRST and its failure stops the download. A package
	// is executable content leaving the workspace; if we cannot record that it
	// left, we do not hand it over. (The `resolved` action takes the opposite
	// trade — see recordSkillAudit.)
	if err := s.recordSkillAudit(skillAuditEntry{
		WorkspaceID: sk.WorkspaceID, SkillID: sk.ID, SkillVersionID: v.ID, VersionNumber: v.Version,
		DeliveryMode: v.DeliveryMode, Action: skillActionPackageDownloaded,
		ContentHash: v.ContentHash, ActorUserID: u.ID, ActorName: u.Name,
		ActorType: skillActorType(u),
	}); err != nil {
		httpErrorCode(w, http.StatusInternalServerError, "skill_audit_failed",
			"the download was not recorded in the audit trail, so it was not served — try again")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+skillPackageFileName(sk, v)+`"`)
	w.Header().Set("X-Skill-Content-Hash", v.ContentHash)
	w.Write(archive)
}

// skillActorType distinguishes a person in a browser from an agent's
// credential, so the audit trail can answer "did a human do this".
func skillActorType(u *user) string {
	if u != nil && u.TokenScope != "" {
		return "agent"
	}
	return "human"
}

// handleSkillPackageInfo is the same package described rather than delivered —
// what the detail screen shows before somebody clicks Download, and what MCP
// returns.
func (s *Server) handleSkillPackageInfo(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	_, v, err := s.approvedClientVersion(u, sk.ID, r.PathValue("versionId"))
	if err != nil {
		writeSkillError(w, err)
		return
	}
	info, err := s.skillPackageInfo(sk, v, s.publicShareBase(r))
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, info)
}

// skillPackageInfo builds the archive to describe it. Building is cheap (tens
// of kilobytes of text) and it means the hash and the size reported are the
// hash and size of the bytes somebody will actually receive, rather than a
// prediction of them.
func (s *Server) skillPackageInfo(sk skill, v skillVersion, base string) (skillPackageInfo, error) {
	archive, manifest, files, err := buildSkillPackage(sk, v)
	if err != nil {
		return skillPackageInfo{}, err
	}
	info := skillPackageInfo{
		Manifest: manifest, Files: files, ArchiveHash: archiveHash(archive),
		SizeBytes: len(archive), FileName: skillPackageFileName(sk, v),
	}
	if base != "" {
		info.DownloadURL = fmt.Sprintf("%s/api/skills/%s/versions/%d/package",
			strings.TrimRight(base, "/"), sk.ID, v.Version)
	}
	return info, nil
}
