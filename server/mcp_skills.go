package server

import (
	"fmt"
	"strings"
)

// The MCP surface of the skill control plane.
//
// Four tools, and the shape of them is the security design:
//
//	skill_catalog        metadata only — no instruction body can come out of it
//	skill_resolve        which exact version applies here, and why
//	skill_get            ONE exact approved Remote version, trusted-framed
//	skill_client_package ONE exact approved Client version, as a package
//
// What is deliberately absent is the tool somebody would otherwise ask for: a
// way to hand in a page id and get its contents back as instructions. That
// tool is the vulnerability. `search` and `get_page` continue to return page
// bodies fenced as UNTRUSTED, and there is no bridge from there to here — the
// only way text reaches skill_get is as a normalized skill_versions row that a
// workspace admin approved in a browser session.
//
// Nothing in this file re-implements a rule. Permission, lifecycle, scope and
// dependency logic all live in the service and the resolver, which the HTTP
// handlers call too. An agent and a person are therefore held to the same rules
// by the same code, which is the only way to keep them from drifting apart.

// mcpSkillCatalog answers "what is there", metadata only.
//
// Progressive disclosure starts here: an agent gets names, descriptions,
// triggers, scope and dependency summaries for the whole approved library —
// enough to decide what to ask for next — and not one instruction body. For a
// library of a hundred skills that is the difference between a usable context
// and a full one.
func (s *Server) mcpSkillCatalog(u *user, workspaceID, project, repository, role, taskType, agent string, capabilities []string) (string, error) {
	workspaces := s.skillWorkspacesFor(u)
	if workspaceID != "" {
		filtered := []string{}
		for _, w := range workspaces {
			if w == workspaceID {
				filtered = append(filtered, w)
			}
		}
		workspaces = filtered
		if len(workspaces) == 0 {
			return "", fmt.Errorf("workspace %q not found", workspaceID)
		}
	}
	skills, versions, err := s.publishedSkillVersions(workspaces)
	if err != nil {
		return "", err
	}
	req := resolveRequest{
		WorkspaceID: workspaceID, Project: project, Repository: repository, Role: role,
		TaskType: taskType, Agent: agent, Capabilities: normalizeStrings(capabilities),
	}
	entries := make([]catalogEntry, 0, len(versions))
	for i := range versions {
		// `scope_match` is advisory here, not a filter. A catalogue that hid
		// everything out of scope would leave an agent unable to see that a
		// skill exists for a context it could switch into — and the resolver is
		// where scope actually decides anything.
		_, _, match := scopeMatches(versions[i].Scope, req)
		entries = append(entries, catalogEntryFor(skills[i], versions[i], match))
	}
	if len(entries) == 0 {
		return "No approved skills in reach. Nothing has been published, or your credential " +
			"cannot see the workspace that holds them.", nil
	}
	return skillJSON(map[string]any{
		"skills": entries,
		"note": "Metadata only. Call skill_resolve with the task to find out which of these applies, " +
			"then skill_get for the exact approved instructions of the one that was selected.",
	}), nil
}

// mcpSkillResolve is the same resolver the Resolver test screen calls.
func (s *Server) mcpSkillResolve(u *user, req resolveRequest) (string, error) {
	res, err := s.resolveSkills(u, req)
	if err != nil {
		return "", err
	}
	workspace := req.WorkspaceID
	if workspace == "" {
		workspace = s.defaultWorkspaceFor(u)
	}
	s.recordResolution(u, workspace, req, res, skillActorType(u))

	// Missing dependencies are lifted to the top level as well as staying on
	// each selection: an agent that reads only the summary must not be able to
	// miss the reason a skill it expected is absent (§21, FM-2).
	missing := []missingDependency{}
	for _, sel := range res.Selected {
		missing = append(missing, sel.Missing...)
	}
	for _, ex := range res.Excluded {
		missing = append(missing, ex.Missing...)
	}
	out := map[string]any{
		"selected":             res.Selected,
		"excluded":             res.Excluded,
		"considered":           res.Considered,
		"missing_dependencies": missing,
	}
	switch {
	case len(res.Selected) == 0 && res.Considered == 0:
		out["note"] = "No approved skill is published in reach of this credential."
	case len(res.Selected) == 0:
		out["note"] = "No approved skill matched this context. `excluded` says which filter each " +
			"candidate failed."
	default:
		out["note"] = "Selection pins an EXACT version. Call skill_get with the skill_id and that " +
			"version number; do not assume the newest. Remote skills return instructions, client " +
			"skills return a package via skill_client_package."
	}
	return skillJSON(out), nil
}

// mcpSkillGet delivers one exact approved Remote version inside the trusted
// envelope.
//
// The version argument is REQUIRED and there is no "latest" spelling. That is
// deliberate: an agent resolved a specific version, the audit trail records
// that version, and a call meaning "whatever is current now" would break the
// one guarantee the immutable-version model exists to provide — that what ran
// can be reconstructed exactly.
func (s *Server) mcpSkillGet(u *user, skillID, slug, version string) (string, error) {
	if strings.TrimSpace(skillID) == "" && strings.TrimSpace(slug) == "" {
		return "", fmt.Errorf("skill_id (or slug) is required")
	}
	if strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("version is required — resolve first, then fetch the exact version it selected")
	}
	if skillID == "" {
		// A slug is workspace-scoped, so it is only usable once the workspace
		// is known. Resolved through the caller's own workspaces, which keeps
		// the lookup inside the permission boundary.
		found := false
		for _, w := range s.skillWorkspacesFor(u) {
			sk, err := s.skillBySlug(w, slugify(slug))
			if err == nil {
				skillID, found = sk.ID, true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("no skill with slug %q in reach", slug)
		}
	}
	sk, v, err := s.approvedRemoteVersion(u, skillID, version)
	if err != nil {
		return "", err
	}
	// Recorded BEFORE the text is handed over, and a failure refuses the call.
	// This is the moment content crosses onto the trusted plane; an untraceable
	// crossing is not allowed to succeed. (Contrast `resolved`, which is
	// best-effort — see recordSkillAudit.)
	if err := s.recordSkillAudit(skillAuditEntry{
		WorkspaceID: sk.WorkspaceID, SkillID: sk.ID, SkillVersionID: v.ID, VersionNumber: v.Version,
		DeliveryMode: v.DeliveryMode, Action: skillActionFetched, ContentHash: v.ContentHash,
		ActorUserID: u.ID, ActorName: u.Name, ActorType: skillActorType(u),
	}); err != nil {
		return "", fmt.Errorf("the fetch could not be recorded in the audit trail, so the skill was not delivered — try again")
	}
	return trustedRemoteEnvelope(sk, v), nil
}

// mcpSkillClientPackage describes the package for an exact approved Client
// version: manifest, file list, archive hash, and the URL to download it from.
//
// The bytes travel over HTTP rather than through MCP. Two reasons: a base64
// archive in a tool result is megabytes of an agent's context spent on a file
// it will write to disk unread, and the download is the natural place for the
// same credential check plus the audit row that says a package left the
// workspace.
func (s *Server) mcpSkillClientPackage(u *user, skillID, slug, version, publicBase string) (string, error) {
	if strings.TrimSpace(skillID) == "" && strings.TrimSpace(slug) == "" {
		return "", fmt.Errorf("skill_id (or slug) is required")
	}
	if strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("version is required — resolve first, then ask for the exact version it selected")
	}
	if skillID == "" {
		found := false
		for _, w := range s.skillWorkspacesFor(u) {
			sk, err := s.skillBySlug(w, slugify(slug))
			if err == nil {
				skillID, found = sk.ID, true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("no skill with slug %q in reach", slug)
		}
	}
	sk, v, err := s.approvedClientVersion(u, skillID, version)
	if err != nil {
		return "", err
	}
	info, err := s.skillPackageInfo(sk, v, publicBase)
	if err != nil {
		return "", err
	}
	return skillJSON(map[string]any{
		"package": info,
		"note": "Deterministic for this immutable version: the same version always produces the same " +
			"bytes, so archiveHash can be compared against what you installed. Download it with your " +
			"own credential. Nothing is installed for you — ask the person you are working with " +
			"before writing files into their environment.",
	}), nil
}
