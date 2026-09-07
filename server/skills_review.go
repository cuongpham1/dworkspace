package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// The review side of the skill control plane: the queue, the deterministic risk
// summary a reviewer sees before publishing, and the three decisions they can
// make.
//
// The risk flags are the part worth being careful about. Every one of them is a
// comparison of two stored rows, so a reviewer can verify any flag by looking at
// the diff — no model, no score, nothing that could be confidently wrong. That
// is also why "Approve" is allowed to demand a confirmation for some of them
// (§10.2 of the PRD): a confirmation that summarises an exact change is worth
// clicking through, and one that says "are you sure?" is theatre.

func (s *Server) handleSkillReviewQueue(w http.ResponseWriter, r *http.Request) {
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
	pending, err := s.skillVersionsByStatus(workspaces, skillStatusPending)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []reviewQueueEntry{}
	for _, v := range pending {
		sk, err := s.skillByID(v.SkillID)
		if err != nil {
			continue
		}
		entry := reviewQueueEntry{
			Skill: sk, Version: v, CanReview: s.skillCanReview(u, sk),
			FirstPublish: sk.CurrentVersionID == "",
		}
		if prev, err := s.previousApprovedVersion(sk, v); err == nil {
			entry.Previous = &prev
		}
		entry.Risks = skillRiskFlags(entry.Previous, v)
		out = append(out, entry)
	}
	writeJSON(w, out)
}

// previousApprovedVersion is the version a reviewer's diff compares against:
// the newest approved (or since-deprecated) version below the one under
// review. Deprecated ones count — it is still what the workspace last decided,
// and diffing against nothing would hide the change.
func (s *Server) previousApprovedVersion(sk skill, v skillVersion) (skillVersion, error) {
	versions, err := s.skillVersions(sk.ID)
	if err != nil {
		return skillVersion{}, err
	}
	for _, candidate := range versions {
		if candidate.Version >= v.Version {
			continue
		}
		if candidate.Status == skillStatusApproved || candidate.Status == skillStatusDeprecated {
			return candidate, nil
		}
	}
	return skillVersion{}, sql.ErrNoRows
}

// skillRiskFlags is the deterministic risk summary — and the list of cases the
// review screen asks for an explicit confirmation on.
//
// Every flag is a comparison of two stored rows. That is what makes it worth
// showing: a reviewer can check any of them by looking, and none of them can
// be wrong in a way nobody notices.
func skillRiskFlags(prev *skillVersion, next skillVersion) []string {
	flags := []string{}
	if prev == nil {
		flags = append(flags, "first_publish")
		if next.DeliveryMode == skillDeliveryRemote {
			flags = append(flags, "first_remote_publish")
		}
		if scopeIsWorkspaceWide(next.Scope) {
			flags = append(flags, "workspace_wide_scope")
		}
		return flags
	}
	if prev.DeliveryMode != next.DeliveryMode {
		flags = append(flags, "delivery_changed")
		if next.DeliveryMode == skillDeliveryRemote {
			// Client → Remote is the one that changes what the text IS: a
			// package somebody installs becomes an instruction VUS injects.
			flags = append(flags, "client_to_remote")
		}
	}
	if scopeExpanded(prev.Scope, next.Scope) {
		flags = append(flags, "scope_expanded")
		if scopeIsWorkspaceWide(next.Scope) && !scopeIsWorkspaceWide(prev.Scope) {
			flags = append(flags, "workspace_wide_scope")
		}
	}
	added, removed := dependencyDelta(prev.Dependencies, next.Dependencies)
	if added > 0 {
		flags = append(flags, fmt.Sprintf("dependencies_added:%d", added))
	}
	if removed > 0 {
		flags = append(flags, fmt.Sprintf("dependencies_removed:%d", removed))
	}
	if requiredDependencyRemoved(prev.Dependencies, next.Dependencies) {
		flags = append(flags, "required_dependency_removed")
	}
	if prev.Instructions != next.Instructions {
		flags = append(flags, "instructions_changed")
	}
	if prev.Description != next.Description {
		flags = append(flags, "description_changed")
	}
	if len(prev.Triggers) != len(next.Triggers) || !sameStrings(prev.Triggers, next.Triggers) {
		flags = append(flags, "triggers_changed")
	}
	return flags
}

// scopeIsWorkspaceWide: no dimension restricts anything, so the skill is a
// candidate anywhere in the workspace.
func scopeIsWorkspaceWide(sc skillScope) bool {
	return len(sc.ProjectIDs) == 0 && len(sc.RepositoryPatterns) == 0 && len(sc.Roles) == 0 &&
		len(sc.TaskTypes) == 0 && len(sc.AgentTypes) == 0
}

// scopeExpanded is true when the new scope reaches somewhere the old one did
// not: a restriction dropped entirely, or a value added to one.
func scopeExpanded(prev, next skillScope) bool {
	pairs := [][2][]string{
		{prev.ProjectIDs, next.ProjectIDs},
		{prev.RepositoryPatterns, next.RepositoryPatterns},
		{prev.Roles, next.Roles},
		{prev.TaskTypes, next.TaskTypes},
		{prev.AgentTypes, next.AgentTypes},
	}
	for _, p := range pairs {
		before, after := p[0], p[1]
		if len(before) > 0 && len(after) == 0 {
			return true // a restriction was lifted
		}
		for _, v := range after {
			if !containsFold(before, v) && len(before) > 0 {
				return true
			}
		}
	}
	return false
}

func dependencyDelta(prev, next []skillDependency) (added, removed int) {
	key := func(d skillDependency) string { return d.Type + ":" + strings.ToLower(d.ID) }
	before := map[string]bool{}
	for _, d := range prev {
		before[key(d)] = true
	}
	after := map[string]bool{}
	for _, d := range next {
		after[key(d)] = true
		if !before[key(d)] {
			added++
		}
	}
	for k := range before {
		if !after[k] {
			removed++
		}
	}
	return added, removed
}

func requiredDependencyRemoved(prev, next []skillDependency) bool {
	key := func(d skillDependency) string { return d.Type + ":" + strings.ToLower(d.ID) }
	after := map[string]bool{}
	for _, d := range next {
		after[key(d)] = true
	}
	for _, d := range prev {
		if d.Required && !after[key(d)] {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if !strings.EqualFold(x[i], y[i]) {
			return false
		}
	}
	return true
}

// ---- review ----

func (s *Server) handleRequestSkillChanges(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
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
	next, err := s.requestChanges(u, sk, v, body.Note)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, next)
}

func (s *Server) handleApproveSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
	if !ok {
		return
	}
	nextSkill, nextVersion, err := s.approveVersion(u, sk, v)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, map[string]any{"skill": nextSkill, "version": nextVersion})
}

func (s *Server) handleDeprecateSkillVersion(w http.ResponseWriter, r *http.Request) {
	sk, u, ok := s.skillOr404(w, r)
	if !ok {
		return
	}
	v, ok := s.skillVersionOr404(w, sk, r.PathValue("versionId"))
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
	nextSkill, nextVersion, err := s.deprecateVersion(u, sk, v, body.Note)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, map[string]any{"skill": nextSkill, "version": nextVersion})
}
