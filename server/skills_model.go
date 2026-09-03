package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The VUS Skill Control Plane — the domain model.
//
// A workspace already has two planes of text, and the whole point of this file
// is that they must never be confused:
//
//   - the KNOWLEDGE plane: pages, notes, rows, attachments, imported documents.
//     An agent reads them through search/get_page and they arrive fenced as
//     UNTRUSTED (see wrapUntrusted in mcp.go). A page saying "ignore your
//     instructions" is data about somebody writing that sentence, nothing more.
//   - the TRUSTED INSTRUCTION plane: workspace rules, and now approved Remote
//     Skill versions. Text here is meant to guide an agent's work.
//
// A Skill is therefore NOT a page with a status field on it. It is its own
// normalized entity with its own lifecycle, and the ONLY way text enters the
// trusted plane is an explicitly reviewed, approved skillVersion. There is no
// endpoint anywhere that takes a page id and hands its body back as
// instructions — that endpoint is the vulnerability, so it does not exist.
//
// The second thing the model has to carry is honesty about WHAT a skill is:
//
//   - a REMOTE skill is behaviour. Metadata, matching, instructions,
//     references. It needs no filesystem and no install; VUS hands the exact
//     approved instruction to the session that needs it.
//   - a CLIENT skill is capability. Scripts, assets, host-native skill
//     semantics, offline use. VUS manages and versions it, but delivery is a
//     package somebody installs.
//
// A remote skill may DEPEND on a client skill or a capability. That is the
// reason the two are one library with one review path rather than two systems:
// `ui-review` is behaviour that happens to need a browser at the client.

// ---- lifecycle ----

const (
	skillStatusDraft      = "draft"
	skillStatusPending    = "pending"
	skillStatusApproved   = "approved"
	skillStatusDeprecated = "deprecated"
)

const (
	skillDeliveryRemote = "remote"
	skillDeliveryClient = "client"
)

// The audit actions. Deliberately a closed set: an audit trail whose verbs are
// free text cannot be filtered, and "what did agents actually execute" is the
// question this table exists to answer.
const (
	skillActionCreated           = "created"
	skillActionUpdated           = "updated"
	skillActionSubmitted         = "submitted"
	skillActionChangesRequested  = "changes_requested"
	skillActionApproved          = "approved"
	skillActionDeprecated        = "deprecated"
	skillActionResolved          = "resolved"
	skillActionFetched           = "fetched"
	skillActionPackageDownloaded = "package_downloaded"
)

// Dependency types. `client_skill` is the one with a version constraint; the
// other three name something the runtime either has or has not.
const (
	skillDepClientSkill = "client_skill"
	skillDepCapability  = "capability"
	skillDepTool        = "tool"
	skillDepConnector   = "connector"
)

// ---- value objects ----

// skillDependency is what a version needs from the runtime it is used in.
//
// `Required` plus `Fallback` is the whole safety story: a missing REQUIRED
// dependency with no declared fallback excludes the skill, because handing an
// agent instructions it cannot carry out is worse than handing it none. A
// fallback is the author saying out loud what to do instead.
type skillDependency struct {
	Type              string `json:"type"`
	ID                string `json:"id"`
	VersionConstraint string `json:"versionConstraint,omitempty"`
	Required          bool   `json:"required"`
	Fallback          string `json:"fallback,omitempty"`
}

// skillReference points AT something; it never inlines it.
//
// That is a deliberate limit rather than a shortcut. A reference to a VUS page
// is fetched through get_page and therefore arrives UNTRUSTED, exactly like any
// other page — so a reference cannot be used to smuggle instructions into the
// trusted envelope. Whatever must be followed belongs in `Instructions`, where
// a human reviewed it.
type skillReference struct {
	Kind   string `json:"kind"` // page | url | note
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
}

// skillScope decides WHERE a version may be selected.
//
// Every list is "empty means anywhere". A non-empty list is a restriction, and
// a restriction the caller cannot answer is a MISS, not a pass: if a version is
// scoped to two projects and the runtime names no project at all, there is no
// evidence the skill applies, so it is excluded with that as the reason. The
// opposite reading (unknown counts as match) would make a narrow scope
// meaningless the moment a client forgets a field.
type skillScope struct {
	WorkspaceID        string   `json:"workspaceId"`
	ProjectIDs         []string `json:"projectIds,omitempty"`
	RepositoryPatterns []string `json:"repositoryPatterns,omitempty"`
	Roles              []string `json:"roles,omitempty"`
	TaskTypes          []string `json:"taskTypes,omitempty"`
	AgentTypes         []string `json:"agentTypes,omitempty"`
}

// skillPackageMetadata describes a CLIENT skill's package beyond SKILL.md.
//
// Scripts and assets are declared here as name+content, so a package is
// reproducible from the immutable version alone. Nothing here is ever executed
// by the server, and nothing is ever written into a client's filesystem by
// VUS — the package is a download somebody installs.
type skillPackageMetadata struct {
	Entrypoint string            `json:"entrypoint,omitempty"`
	Scripts    []skillPackedFile `json:"scripts,omitempty"`
	Assets     []skillPackedFile `json:"assets,omitempty"`
}

type skillPackedFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ---- entities ----

type skill struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspaceId"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	OwnerID         string `json:"ownerId"`
	OwnerName       string `json:"ownerName,omitempty"`
	LifecycleStatus string `json:"lifecycleStatus"`
	// The published pointer. Empty when nothing has ever been approved, or when
	// the published version was deprecated without a replacement — in which case
	// the skill is no longer usable for execution, and the interface says so.
	CurrentVersionID     string `json:"currentVersionId,omitempty"`
	CurrentVersionNumber int    `json:"currentVersion,omitempty"`
	CreatedAt            string `json:"createdAt"`
	UpdatedAt            string `json:"updatedAt"`
}

type skillVersion struct {
	ID             string               `json:"id"`
	SkillID        string               `json:"skillId"`
	Version        int                  `json:"version"`
	DeliveryMode   string               `json:"deliveryMode"`
	Status         string               `json:"status"`
	Description    string               `json:"description"`
	Triggers       []string             `json:"triggers"`
	Instructions   string               `json:"instructions"`
	References     []skillReference     `json:"references"`
	Dependencies   []skillDependency    `json:"dependencies"`
	Scope          skillScope           `json:"scope"`
	Package        skillPackageMetadata `json:"packageMetadata"`
	ReviewNote     string               `json:"reviewNote,omitempty"`
	CreatedBy      string               `json:"createdBy"`
	CreatedByName  string               `json:"createdByName,omitempty"`
	ApprovedBy     string               `json:"approvedBy,omitempty"`
	ApprovedByName string               `json:"approvedByName,omitempty"`
	CreatedAt      string               `json:"createdAt"`
	UpdatedAt      string               `json:"updatedAt"`
	SubmittedAt    string               `json:"submittedAt,omitempty"`
	ApprovedAt     string               `json:"approvedAt,omitempty"`
	DeprecatedAt   string               `json:"deprecatedAt,omitempty"`
	ContentHash    string               `json:"contentHash"`
	// Whether this version is the skill's published pointer. Derived, not
	// stored — the pointer lives on the skill row so publishing it is one
	// transactional write.
	Published bool `json:"published"`
}

type skillAuditEntry struct {
	ID                  int64    `json:"id"`
	CreatedAt           string   `json:"createdAt"`
	WorkspaceID         string   `json:"workspaceId"`
	SkillID             string   `json:"skillId"`
	SkillVersionID      string   `json:"skillVersionId,omitempty"`
	VersionNumber       int      `json:"versionNumber,omitempty"`
	DeliveryMode        string   `json:"deliveryMode,omitempty"`
	Action              string   `json:"action"`
	AgentType           string   `json:"agentType,omitempty"`
	ProjectRef          string   `json:"projectRef,omitempty"`
	TaskRef             string   `json:"taskRef,omitempty"`
	ResolutionReason    string   `json:"resolutionReason,omitempty"`
	MissingDependencies []string `json:"missingDependencies,omitempty"`
	ContentHash         string   `json:"contentHash,omitempty"`
	ActorUserID         string   `json:"actorUserId,omitempty"`
	ActorName           string   `json:"actorName,omitempty"`
	ActorType           string   `json:"actorType,omitempty"`
}

// ---- normalization ----

// slugify turns a name into a stable, URL-safe identifier. Lossy on purpose:
// the slug is a handle, the name is what people read.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r), r == '-', r == '_', r == '.', r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// normalizeStrings trims, drops blanks and de-duplicates while KEEPING the
// author's order. Order is kept because the trigger list is read by humans in
// the interface; de-duplication happens because a repeated trigger would score
// twice and quietly outrank a better match.
func normalizeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	return out
}

func normalizeScope(sc skillScope, workspaceID string) skillScope {
	sc.WorkspaceID = workspaceID
	sc.ProjectIDs = normalizeStrings(sc.ProjectIDs)
	sc.RepositoryPatterns = normalizeStrings(sc.RepositoryPatterns)
	sc.Roles = normalizeStrings(sc.Roles)
	sc.TaskTypes = normalizeStrings(sc.TaskTypes)
	sc.AgentTypes = normalizeStrings(sc.AgentTypes)
	return sc
}

func normalizeDependencies(in []skillDependency) []skillDependency {
	out := make([]skillDependency, 0, len(in))
	seen := map[string]bool{}
	for _, d := range in {
		d.Type = strings.TrimSpace(strings.ToLower(d.Type))
		d.ID = strings.TrimSpace(d.ID)
		d.VersionConstraint = strings.TrimSpace(d.VersionConstraint)
		d.Fallback = strings.TrimSpace(d.Fallback)
		if d.Type == "" || d.ID == "" {
			continue
		}
		key := d.Type + "\x00" + strings.ToLower(d.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	// Sorted, because the dependency list is part of the content hash and part
	// of the reviewer's diff: reordering rows in a form must not read as a
	// change, and must not produce a different hash for identical requirements.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func normalizeReferences(in []skillReference) []skillReference {
	out := make([]skillReference, 0, len(in))
	for _, r := range in {
		r.Kind = strings.TrimSpace(strings.ToLower(r.Kind))
		r.Target = strings.TrimSpace(r.Target)
		r.Label = strings.TrimSpace(r.Label)
		if r.Target == "" {
			continue
		}
		if r.Kind == "" {
			r.Kind = "url"
		}
		out = append(out, r)
	}
	return out
}

func normalizePackage(p skillPackageMetadata) skillPackageMetadata {
	p.Entrypoint = strings.TrimSpace(p.Entrypoint)
	p.Scripts = normalizePackedFiles(p.Scripts)
	p.Assets = normalizePackedFiles(p.Assets)
	return p
}

func normalizePackedFiles(in []skillPackedFile) []skillPackedFile {
	out := make([]skillPackedFile, 0, len(in))
	seen := map[string]bool{}
	for _, f := range in {
		f.Name = strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "/"))
		if f.Name == "" || seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		out = append(out, f)
	}
	// Sorted by name: a package must be byte-identical for the same immutable
	// version, and zip entry order is part of those bytes.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- validation ----

const (
	maxSkillNameLen         = 120
	maxSkillSlugLen         = 80
	maxSkillDescriptionLen  = 500
	maxSkillInstructionsLen = 200_000 // ~50k tokens: past this a "skill" is a document
	maxSkillTriggers        = 50
	maxSkillDependencies    = 40
	maxSkillReferences      = 40
	maxSkillPackedFiles     = 40
	maxSkillPackedFileBytes = 256 << 10
)

// validateVersionDraft checks what a DRAFT must satisfy to be storable. It is
// deliberately weaker than validateForReview: a draft is somebody's work in
// progress, and refusing to save half-finished work is how people lose it.
func validateVersionDraft(v skillVersion) error {
	if v.DeliveryMode != skillDeliveryRemote && v.DeliveryMode != skillDeliveryClient {
		return coded("skill_bad_delivery", "delivery mode must be remote or client")
	}
	if len([]rune(v.Description)) > maxSkillDescriptionLen {
		return coded("skill_description_long", "the description is too long")
	}
	if len(v.Instructions) > maxSkillInstructionsLen {
		return coded("skill_instructions_long", "the instructions are too long for one skill — split it")
	}
	if len(v.Triggers) > maxSkillTriggers {
		return coded("skill_too_many_triggers", "too many triggers")
	}
	if len(v.Dependencies) > maxSkillDependencies {
		return coded("skill_too_many_dependencies", "too many dependencies")
	}
	if len(v.References) > maxSkillReferences {
		return coded("skill_too_many_references", "too many references")
	}
	for _, d := range v.Dependencies {
		switch d.Type {
		case skillDepClientSkill, skillDepCapability, skillDepTool, skillDepConnector:
		default:
			return coded("skill_bad_dependency_type",
				fmt.Sprintf("dependency type %q is not one of client_skill, capability, tool, connector", d.Type))
		}
		if d.VersionConstraint != "" {
			if _, _, err := parseVersionConstraint(d.VersionConstraint); err != nil {
				return coded("skill_bad_version_constraint",
					fmt.Sprintf("version constraint %q is not understood — use >=1.2.0, >1.2.0 or 1.2.0", d.VersionConstraint))
			}
		}
	}
	for _, r := range v.References {
		switch r.Kind {
		case "page", "url", "note":
		default:
			return coded("skill_bad_reference_kind", fmt.Sprintf("reference kind %q is not one of page, url, note", r.Kind))
		}
	}
	if n := len(v.Package.Scripts) + len(v.Package.Assets); n > maxSkillPackedFiles {
		return coded("skill_too_many_files", "too many package files")
	}
	for _, f := range append(append([]skillPackedFile{}, v.Package.Scripts...), v.Package.Assets...) {
		if strings.Contains(f.Name, "..") {
			return coded("skill_bad_file_name", fmt.Sprintf("package file name %q may not contain \"..\"", f.Name))
		}
		if len(f.Content) > maxSkillPackedFileBytes {
			return coded("skill_file_too_large", fmt.Sprintf("package file %q is too large", f.Name))
		}
	}
	return nil
}

// validateForReview is the gate in front of Submit review. Everything here is
// something a reviewer would otherwise have to reject by hand, so it is caught
// while the author still has the form open.
func validateForReview(sk skill, v skillVersion) error {
	if err := validateVersionDraft(v); err != nil {
		return err
	}
	if strings.TrimSpace(sk.Name) == "" {
		return coded("skill_name_required", "a skill needs a name")
	}
	if strings.TrimSpace(sk.Slug) == "" {
		return coded("skill_slug_required", "a skill needs a slug")
	}
	if strings.TrimSpace(v.Description) == "" {
		return coded("skill_description_required", "a description is required — it is what the catalogue shows an agent")
	}
	if v.DeliveryMode == skillDeliveryRemote && strings.TrimSpace(v.Instructions) == "" {
		return coded("skill_instructions_required",
			"a Remote Skill is its instructions — there is nothing to deliver without them")
	}
	if v.DeliveryMode == skillDeliveryClient && strings.TrimSpace(v.Instructions) == "" {
		return coded("skill_package_required",
			"a Client Skill needs its SKILL.md content — that is what the package installs")
	}
	if v.Scope.WorkspaceID == "" {
		return coded("skill_scope_workspace_required", "a skill must be scoped to a workspace")
	}
	return nil
}

// ---- content hash ----

// skillVersionContent is the part of a version that IS the skill: what an agent
// would act on. Status, timestamps, authorship and review notes are about the
// version, not in it, so they stay out — otherwise approving a version would
// change its own hash, and the hash could never certify "this is the text I
// reviewed".
type skillVersionContent struct {
	Slug         string               `json:"slug"`
	Version      int                  `json:"version"`
	DeliveryMode string               `json:"deliveryMode"`
	Description  string               `json:"description"`
	Triggers     []string             `json:"triggers"`
	Instructions string               `json:"instructions"`
	References   []skillReference     `json:"references"`
	Dependencies []skillDependency    `json:"dependencies"`
	Scope        skillScope           `json:"scope"`
	Package      skillPackageMetadata `json:"packageMetadata"`
}

// skillContentHash is deterministic for identical content: struct field order
// fixes key order, the normalizers fix list order, and nil and empty lists are
// folded together so "cleared the field" and "never filled it in" cannot hash
// differently.
func skillContentHash(slug string, v skillVersion) string {
	c := skillVersionContent{
		Slug:         slug,
		Version:      v.Version,
		DeliveryMode: v.DeliveryMode,
		Description:  v.Description,
		Triggers:     orEmptyStrings(v.Triggers),
		Instructions: v.Instructions,
		References:   orEmptyRefs(v.References),
		Dependencies: orEmptyDeps(v.Dependencies),
		Scope:        v.Scope,
		Package:      v.Package,
	}
	c.Scope.ProjectIDs = orEmptyStrings(c.Scope.ProjectIDs)
	c.Scope.RepositoryPatterns = orEmptyStrings(c.Scope.RepositoryPatterns)
	c.Scope.Roles = orEmptyStrings(c.Scope.Roles)
	c.Scope.TaskTypes = orEmptyStrings(c.Scope.TaskTypes)
	c.Scope.AgentTypes = orEmptyStrings(c.Scope.AgentTypes)
	c.Package.Scripts = orEmptyFiles(c.Package.Scripts)
	c.Package.Assets = orEmptyFiles(c.Package.Assets)
	b, err := json.Marshal(c)
	if err != nil {
		// Marshalling plain strings and slices cannot fail. If it somehow does,
		// a hash of the error is still stable and still unequal to any real
		// content, which beats returning "" and letting two versions look
		// identical.
		b = []byte("hash-error:" + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func orEmptyStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyRefs(v []skillReference) []skillReference {
	if v == nil {
		return []skillReference{}
	}
	return v
}

func orEmptyDeps(v []skillDependency) []skillDependency {
	if v == nil {
		return []skillDependency{}
	}
	return v
}

func orEmptyFiles(v []skillPackedFile) []skillPackedFile {
	if v == nil {
		return []skillPackedFile{}
	}
	return v
}

// shortHash is what the interface shows. Long enough to compare by eye, short
// enough to fit in a table row.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// ---- version constraints ----

// parseVersionConstraint understands the three forms an author actually writes.
// Ranges, carets and tildes are deliberately absent: nobody has asked for them,
// and a constraint language nobody can predict is worse than one that refuses
// what it does not understand.
func parseVersionConstraint(c string) (op string, version []int, err error) {
	c = strings.TrimSpace(c)
	switch {
	case strings.HasPrefix(c, ">="):
		op, c = ">=", strings.TrimSpace(c[2:])
	case strings.HasPrefix(c, ">"):
		op, c = ">", strings.TrimSpace(c[1:])
	case strings.HasPrefix(c, "="):
		op, c = "=", strings.TrimSpace(c[1:])
	default:
		op = "="
	}
	v, err := parseVersionNumber(c)
	if err != nil {
		return "", nil, err
	}
	return op, v, nil
}

func parseVersionNumber(v string) ([]int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, fmt.Errorf("empty version")
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%q is not a dotted number", v)
		}
		out = append(out, n)
	}
	return out, nil
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionSatisfies reports whether `have` meets `constraint`. An unparseable
// HAVE fails closed: the runtime told us a version it could not name, and
// pretending that satisfies a requirement is exactly the silent success the
// dependency handshake exists to prevent.
func versionSatisfies(have, constraint string) bool {
	if strings.TrimSpace(constraint) == "" {
		return true
	}
	op, want, err := parseVersionConstraint(constraint)
	if err != nil {
		return false
	}
	got, err := parseVersionNumber(have)
	if err != nil {
		return false
	}
	switch op {
	case ">=":
		return compareVersions(got, want) >= 0
	case ">":
		return compareVersions(got, want) > 0
	default:
		return compareVersions(got, want) == 0
	}
}

// matchesPattern is a one-star glob, which is all a repository pattern needs:
// `acme/*`, `*-service`, `infra/*/charts`.
func matchesPattern(pattern, value string) bool {
	pattern, value = strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(strings.TrimSpace(value))
	if pattern == "" || value == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false // a pattern not starting with * is anchored at the front
		}
		pos += idx + len(part)
	}
	// A pattern not ending in * is anchored at the end.
	if last := parts[len(parts)-1]; last != "" && pos != len(value) {
		return false
	}
	return true
}

func containsFold(list []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), needle) {
			return true
		}
	}
	return false
}
