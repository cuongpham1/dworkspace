package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// The Resolver — one deterministic function, two callers.
//
// The UI's "Resolver test" and the MCP tool `skill_resolve` both end up in
// resolveSkills, and that is the point of the file existing at all. A resolver
// reimplemented in TypeScript for the test screen would be a screen that lies:
// it would agree with the backend on the day it was written and drift from then
// on, and the one thing the test screen is for is answering "will this skill
// actually be picked".
//
// Deterministic, not clever. No model, no embeddings, no ranking that changes
// between two identical calls. Every point in the score has a name, so the
// interface can print the arithmetic and a person can disagree with it. When
// semantic ranking is genuinely needed it can be added as an extra signal
// beside this, but it must not replace an explanation with a number.

// Scoring weights, from §20 of the PRD. Named constants rather than literals so
// the breakdown the UI prints and the numbers the resolver adds can never
// disagree.
const (
	scoreProjectMatch = 100
	scoreRoleMatch    = 40
	scoreTaskType     = 40
	scoreTrigger      = 20
	scoreAgentMatch   = 10
)

// resolveRequest is the runtime's description of the task at hand.
type resolveRequest struct {
	WorkspaceID string
	Task        string
	Project     string
	Repository  string
	Role        string
	TaskType    string
	Agent       string
	// What the runtime can do. `capabilities` answers dependencies of type
	// capability, tool and connector; `installedClientSkills` answers
	// client_skill dependencies, with versions so a constraint can be checked.
	Capabilities         []string
	InstalledClientSkill []installedClientSkill
	Limit                int
}

type installedClientSkill struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

// scoreReason is one line of the arithmetic. Structured rather than a sentence,
// because the Resolver test screen renders it as a breakdown and a sentence
// would have to be parsed back apart.
type scoreReason struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Points int    `json:"points"`
}

// missingDependency is a dependency the runtime does not satisfy. `Fallback`
// carries what the author said to do instead; when a REQUIRED dependency has no
// fallback the candidate is excluded outright and appears under Excluded.
type missingDependency struct {
	Type              string `json:"type"`
	ID                string `json:"id"`
	VersionConstraint string `json:"versionConstraint,omitempty"`
	Required          bool   `json:"required"`
	Fallback          string `json:"fallback,omitempty"`
	Detail            string `json:"detail"`
}

// resolvedSkill is one selected candidate: the exact skill, the exact version,
// the hash of the text that will be delivered, and why.
type resolvedSkill struct {
	SkillID      string              `json:"skillId"`
	Slug         string              `json:"slug"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Version      int                 `json:"version"`
	VersionID    string              `json:"versionId"`
	DeliveryMode string              `json:"deliveryMode"`
	ContentHash  string              `json:"contentHash"`
	Score        int                 `json:"score"`
	Reasons      []scoreReason       `json:"reasons"`
	Missing      []missingDependency `json:"missingDependencies,omitempty"`
	Dependencies []skillDependency   `json:"dependencies,omitempty"`
}

// excludedSkill is a candidate that did NOT make it, and why.
//
// Returning these is not padding. "No skill matched" with no explanation is the
// single most useless answer this system could give an author, and the failure
// modes the PRD names (FM-1, FM-2) are both about saying which filter bit.
type excludedSkill struct {
	SkillID      string              `json:"skillId"`
	Slug         string              `json:"slug"`
	Name         string              `json:"name"`
	Version      int                 `json:"version"`
	DeliveryMode string              `json:"deliveryMode"`
	Reason       string              `json:"reason"`
	ReasonCode   string              `json:"reasonCode"`
	Missing      []missingDependency `json:"missingDependencies,omitempty"`
}

type resolveResult struct {
	Selected []resolvedSkill `json:"selected"`
	Excluded []excludedSkill `json:"excluded"`
	// Candidates considered: the number of published, approved versions in the
	// caller's reach before any filter ran. `0` and "nothing matched" are
	// different problems and the interface says which.
	Considered int `json:"considered"`
}

// ---- the resolver ----

// resolveSkills runs the pipeline from §20 of the PRD:
//
//	permission → published+approved → scope → agent → dependencies → score
//
// Hard filters first, scoring last, so a skill can never be selected by
// out-scoring a filter it fails.
func (s *Server) resolveSkills(u *user, req resolveRequest) (resolveResult, error) {
	out := resolveResult{Selected: []resolvedSkill{}, Excluded: []excludedSkill{}}

	// 1. Permission. The workspaces the CALLER may reach, narrowed to the one
	// asked for. A workspace the caller is not a member of is not "empty
	// result" — it is absent from this list, so nothing in it can be scored.
	allowed := s.skillWorkspacesFor(u)
	workspaces := allowed
	if req.WorkspaceID != "" {
		workspaces = nil
		for _, w := range allowed {
			if w == req.WorkspaceID {
				workspaces = []string{w}
			}
		}
	}
	if len(workspaces) == 0 {
		return out, nil
	}

	// 2. Published + approved only. publishedSkillVersions follows each skill's
	// published pointer and re-checks that the version it names is approved, so
	// no draft, pending or deprecated version can ever reach the loop below.
	skills, versions, err := s.publishedSkillVersions(workspaces)
	if err != nil {
		return out, err
	}
	out.Considered = len(versions)

	task := strings.ToLower(req.Task)
	for i := range versions {
		sk, v := skills[i], versions[i]

		// 3. Scope. Every non-empty dimension must match; an empty one matches
		// anything. See skillScope for why an unanswered restriction is a miss.
		if reason, code, ok := scopeMatches(v.Scope, req); !ok {
			out.Excluded = append(out.Excluded, excludedSkill{
				SkillID: sk.ID, Slug: sk.Slug, Name: sk.Name, Version: v.Version,
				DeliveryMode: v.DeliveryMode, Reason: reason, ReasonCode: code,
			})
			continue
		}

		// 4. Dependencies. A required one that is missing and has no declared
		// fallback excludes the candidate — never silently, always with the
		// dependency named.
		missing, blocked := checkDependencies(v.Dependencies, req)
		if blocked != nil {
			out.Excluded = append(out.Excluded, excludedSkill{
				SkillID: sk.ID, Slug: sk.Slug, Name: sk.Name, Version: v.Version,
				DeliveryMode: v.DeliveryMode, ReasonCode: "missing_required_dependency",
				Reason:  fmt.Sprintf("missing required %s %q and no fallback is declared", blocked.Type, blocked.ID),
				Missing: missing,
			})
			continue
		}

		// 5. Score. Only now, and only over candidates that passed everything.
		score, reasons := scoreCandidate(v, req, task)
		out.Selected = append(out.Selected, resolvedSkill{
			SkillID: sk.ID, Slug: sk.Slug, Name: sk.Name, Description: v.Description,
			Version: v.Version, VersionID: v.ID, DeliveryMode: v.DeliveryMode,
			ContentHash: v.ContentHash, Score: score, Reasons: reasons, Missing: missing,
			Dependencies: v.Dependencies,
		})
	}

	// A candidate with nothing in its favour is not a match. Without this a
	// workspace-wide skill with no triggers would be handed to every task in
	// the workspace, which is how a control plane becomes noise.
	kept := out.Selected[:0]
	for _, c := range out.Selected {
		if c.Score > 0 {
			kept = append(kept, c)
			continue
		}
		out.Excluded = append(out.Excluded, excludedSkill{
			SkillID: c.SkillID, Slug: c.Slug, Name: c.Name, Version: c.Version,
			DeliveryMode: c.DeliveryMode, ReasonCode: "no_signal",
			Reason:  "nothing in this context matches the skill's triggers, scope, role or task type",
			Missing: c.Missing,
		})
	}
	out.Selected = kept

	// Deterministic order: score, then slug, then version. The tie-breakers are
	// what make two identical calls return the same list — sorting by score
	// alone leaves ties to the map iteration gods.
	sort.SliceStable(out.Selected, func(i, j int) bool {
		a, b := out.Selected[i], out.Selected[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Slug != b.Slug {
			return a.Slug < b.Slug
		}
		return a.Version > b.Version
	})
	sort.SliceStable(out.Excluded, func(i, j int) bool {
		a, b := out.Excluded[i], out.Excluded[j]
		if a.Slug != b.Slug {
			return a.Slug < b.Slug
		}
		return a.Version > b.Version
	})

	limit := req.Limit
	if limit <= 0 || limit > 3 {
		limit = 3
	}
	if len(out.Selected) > limit {
		out.Selected = out.Selected[:limit]
	}
	return out, nil
}

// scopeMatches applies the hard scope filters and, when one bites, says which.
func scopeMatches(sc skillScope, req resolveRequest) (reason, code string, ok bool) {
	if len(sc.ProjectIDs) > 0 {
		if req.Project == "" {
			return "the skill is scoped to specific projects and this context names none", "scope_project_unknown", false
		}
		if !containsFold(sc.ProjectIDs, req.Project) {
			return fmt.Sprintf("project %q is not in the skill's project scope", req.Project), "scope_project", false
		}
	}
	if len(sc.RepositoryPatterns) > 0 {
		if req.Repository == "" {
			return "the skill is scoped to specific repositories and this context names none", "scope_repository_unknown", false
		}
		matched := false
		for _, p := range sc.RepositoryPatterns {
			if matchesPattern(p, req.Repository) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("repository %q matches none of the skill's patterns", req.Repository), "scope_repository", false
		}
	}
	if len(sc.Roles) > 0 {
		if req.Role == "" {
			return "the skill is scoped to specific roles and this context names none", "scope_role_unknown", false
		}
		if !containsFold(sc.Roles, req.Role) {
			return fmt.Sprintf("role %q is not in the skill's role scope", req.Role), "scope_role", false
		}
	}
	if len(sc.TaskTypes) > 0 {
		if req.TaskType == "" {
			return "the skill is scoped to specific task types and this context names none", "scope_task_type_unknown", false
		}
		if !containsFold(sc.TaskTypes, req.TaskType) {
			return fmt.Sprintf("task type %q is not in the skill's task-type scope", req.TaskType), "scope_task_type", false
		}
	}
	if len(sc.AgentTypes) > 0 {
		if req.Agent == "" {
			return "the skill is scoped to specific agents and this context names none", "scope_agent_unknown", false
		}
		if !containsFold(sc.AgentTypes, req.Agent) {
			return fmt.Sprintf("agent %q is not in the skill's agent scope", req.Agent), "scope_agent", false
		}
	}
	return "", "", true
}

// checkDependencies returns every unsatisfied dependency, and the first
// REQUIRED one without a fallback — which is what excludes a candidate.
//
// Optional and fallback-covered dependencies are still reported. That is the
// difference between "the skill was selected and here is what your runtime is
// missing" and pretending everything is fine, which is the failure mode §21 of
// the PRD is about.
func checkDependencies(deps []skillDependency, req resolveRequest) (missing []missingDependency, blocked *skillDependency) {
	for i := range deps {
		d := deps[i]
		satisfied, detail := dependencySatisfied(d, req)
		if satisfied {
			continue
		}
		missing = append(missing, missingDependency{
			Type: d.Type, ID: d.ID, VersionConstraint: d.VersionConstraint,
			Required: d.Required, Fallback: d.Fallback, Detail: detail,
		})
		if d.Required && d.Fallback == "" && blocked == nil {
			blocked = &deps[i]
		}
	}
	return missing, blocked
}

func dependencySatisfied(d skillDependency, req resolveRequest) (bool, string) {
	switch d.Type {
	case skillDepClientSkill:
		for _, inst := range req.InstalledClientSkill {
			if !strings.EqualFold(strings.TrimSpace(inst.ID), d.ID) {
				continue
			}
			if d.VersionConstraint == "" {
				return true, ""
			}
			if versionSatisfies(inst.Version, d.VersionConstraint) {
				return true, ""
			}
			return false, fmt.Sprintf("client skill %s is installed at version %s, which does not satisfy %s",
				d.ID, orUnknownVersion(inst.Version), d.VersionConstraint)
		}
		return false, fmt.Sprintf("client skill %s is not installed", d.ID)
	default:
		// capability | tool | connector — the runtime either reports it or it
		// does not. There is no version handshake for these because nothing
		// reports one.
		if containsFold(req.Capabilities, d.ID) {
			return true, ""
		}
		return false, fmt.Sprintf("the runtime does not report the %s %q", d.Type, d.ID)
	}
}

func orUnknownVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unstated)"
	}
	return v
}

// scoreCandidate is the arithmetic, and it explains itself as it goes.
func scoreCandidate(v skillVersion, req resolveRequest, lowerTask string) (int, []scoreReason) {
	score := 0
	reasons := []scoreReason{}
	add := func(code, label string, points int) {
		score += points
		reasons = append(reasons, scoreReason{Code: code, Label: label, Points: points})
	}
	if req.Project != "" && containsFold(v.Scope.ProjectIDs, req.Project) {
		add("project", fmt.Sprintf("project scope %s", req.Project), scoreProjectMatch)
	} else if req.Repository != "" && len(v.Scope.RepositoryPatterns) > 0 {
		for _, p := range v.Scope.RepositoryPatterns {
			if matchesPattern(p, req.Repository) {
				add("repository", fmt.Sprintf("repository pattern %s", p), scoreProjectMatch)
				break
			}
		}
	}
	if req.Role != "" && containsFold(v.Scope.Roles, req.Role) {
		add("role", fmt.Sprintf("role %s", req.Role), scoreRoleMatch)
	}
	if req.TaskType != "" && containsFold(v.Scope.TaskTypes, req.TaskType) {
		add("task_type", fmt.Sprintf("task type %s", req.TaskType), scoreTaskType)
	}
	// Triggers are matched against the task text, in the author's order, once
	// each. Word-boundary matching rather than substring: "api" must not fire on
	// "rapid", which is the kind of match that makes a resolver look random.
	for _, trigger := range v.Triggers {
		if matchesTrigger(lowerTask, trigger) {
			add("trigger", fmt.Sprintf("trigger %s", trigger), scoreTrigger)
		}
	}
	if req.Agent != "" && containsFold(v.Scope.AgentTypes, req.Agent) {
		add("agent", fmt.Sprintf("agent %s", req.Agent), scoreAgentMatch)
	}
	return score, reasons
}

// matchesTrigger looks for the trigger as a whole word (or whole phrase) in the
// task text. Case-insensitive; the caller has already lowercased the task.
func matchesTrigger(lowerTask, trigger string) bool {
	needle := strings.ToLower(strings.TrimSpace(trigger))
	if needle == "" || lowerTask == "" {
		return false
	}
	from := 0
	for {
		idx := strings.Index(lowerTask[from:], needle)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(needle)
		if isWordBoundary(lowerTask, start-1) && isWordBoundary(lowerTask, end) {
			return true
		}
		from = start + 1
		if from >= len(lowerTask) {
			return false
		}
	}
}

func isWordBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	c := s[i]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

// ---- audit for a resolution ----

// taskFingerprint is what goes in the audit instead of the task text.
//
// The audit has to answer "which task used which skill", and the honest way to
// do that without keeping people's prompts is a digest: two audit rows for the
// same task match each other, and nobody can read the task back out of them.
// §5.5 of the PRD says not to log the full prompt by default; this is how.
func taskFingerprint(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(task)))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// recordResolution writes one audit row per SELECTED skill. Best-effort by the
// policy in recordSkillAudit: a discovery call is not refused because the log
// could not be written, but the failure is logged rather than swallowed.
func (s *Server) recordResolution(u *user, workspaceID string, req resolveRequest, res resolveResult, actorType string) {
	for _, sel := range res.Selected {
		missing := make([]string, 0, len(sel.Missing))
		for _, m := range sel.Missing {
			missing = append(missing, m.Type+":"+m.ID)
		}
		reasons := make([]string, 0, len(sel.Reasons))
		for _, r := range sel.Reasons {
			reasons = append(reasons, fmt.Sprintf("+%d %s", r.Points, r.Label))
		}
		err := s.recordSkillAudit(skillAuditEntry{
			WorkspaceID: workspaceID, SkillID: sel.SkillID, SkillVersionID: sel.VersionID,
			VersionNumber: sel.Version, DeliveryMode: sel.DeliveryMode, Action: skillActionResolved,
			AgentType: req.Agent, ProjectRef: req.Project, TaskRef: taskFingerprint(req.Task),
			ResolutionReason: strings.Join(reasons, ", "), MissingDependencies: missing,
			ContentHash: sel.ContentHash, ActorUserID: userIDOf(u), ActorName: userNameOf(u),
			ActorType: actorType,
		})
		logSkillAuditFailure(skillActionResolved, err)
	}
}

func userIDOf(u *user) string {
	if u == nil {
		return ""
	}
	return u.ID
}

func userNameOf(u *user) string {
	if u == nil {
		return ""
	}
	return u.Name
}
