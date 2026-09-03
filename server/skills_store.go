package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Persistence for the skill control plane.
//
// Everything here is SQL and scanning, nothing here decides anything. The rules
// — who may act, what a lifecycle transition means, what the resolver picks —
// live in skills_service.go and skills_resolver.go, so that the HTTP handlers
// and the MCP handlers cannot end up enforcing two different versions of them.

const skillSelect = `SELECT s.id, s.workspace_id, s.slug, s.name, s.description, s.owner_id,
	COALESCE(u.name, ''), s.lifecycle_status, COALESCE(s.current_version_id, ''),
	s.current_version_number, s.created_at, s.updated_at
	FROM skills s LEFT JOIN users u ON u.id = s.owner_id`

func scanSkill(sc interface{ Scan(...any) error }) (skill, error) {
	var sk skill
	err := sc.Scan(&sk.ID, &sk.WorkspaceID, &sk.Slug, &sk.Name, &sk.Description, &sk.OwnerID,
		&sk.OwnerName, &sk.LifecycleStatus, &sk.CurrentVersionID, &sk.CurrentVersionNumber,
		&sk.CreatedAt, &sk.UpdatedAt)
	return sk, err
}

const skillVersionSelect = `SELECT v.id, v.skill_id, v.version, v.delivery_mode, v.status, v.description,
	v.triggers, v.instructions, v.refs, v.dependencies, v.scope, v.package_metadata, v.review_note,
	v.created_by, COALESCE(cu.name, ''), COALESCE(v.approved_by, ''), COALESCE(au.name, ''),
	v.created_at, v.updated_at, COALESCE(v.submitted_at, ''), COALESCE(v.approved_at, ''),
	COALESCE(v.deprecated_at, ''), v.content_hash,
	COALESCE((SELECT 1 FROM skills s WHERE s.id = v.skill_id AND s.current_version_id = v.id), 0)
	FROM skill_versions v
	LEFT JOIN users cu ON cu.id = v.created_by
	LEFT JOIN users au ON au.id = v.approved_by`

func scanSkillVersion(sc interface{ Scan(...any) error }) (skillVersion, error) {
	var v skillVersion
	var triggers, refs, deps, scope, pkg string
	var published int
	err := sc.Scan(&v.ID, &v.SkillID, &v.Version, &v.DeliveryMode, &v.Status, &v.Description,
		&triggers, &v.Instructions, &refs, &deps, &scope, &pkg, &v.ReviewNote,
		&v.CreatedBy, &v.CreatedByName, &v.ApprovedBy, &v.ApprovedByName,
		&v.CreatedAt, &v.UpdatedAt, &v.SubmittedAt, &v.ApprovedAt, &v.DeprecatedAt, &v.ContentHash,
		&published)
	if err != nil {
		return skillVersion{}, err
	}
	// Malformed stored JSON degrades to an EMPTY value, never to an error.
	//
	// That choice is about what the failure would cost. A version row whose
	// scope column got corrupted must still be listable and still be
	// deprecatable; an unmarshal error here would take the whole library down
	// with it. An empty scope is also the safe direction for the resolver: it
	// cannot grant a match that the stored data does not support, because the
	// dependencies it would have to satisfy are gone too and a required one
	// missing excludes the candidate.
	if json.Unmarshal([]byte(triggers), &v.Triggers) != nil || v.Triggers == nil {
		v.Triggers = []string{}
	}
	if json.Unmarshal([]byte(refs), &v.References) != nil || v.References == nil {
		v.References = []skillReference{}
	}
	if json.Unmarshal([]byte(deps), &v.Dependencies) != nil || v.Dependencies == nil {
		v.Dependencies = []skillDependency{}
	}
	if json.Unmarshal([]byte(scope), &v.Scope) != nil {
		v.Scope = skillScope{}
	}
	if json.Unmarshal([]byte(pkg), &v.Package) != nil {
		v.Package = skillPackageMetadata{}
	}
	v.Published = published != 0
	return v, nil
}

// ---- reads ----

func (s *Server) skillByID(id string) (skill, error) {
	return scanSkill(s.db.QueryRow(skillSelect+` WHERE s.id = ?`, id))
}

func (s *Server) skillBySlug(workspaceID, slug string) (skill, error) {
	return scanSkill(s.db.QueryRow(skillSelect+` WHERE s.workspace_id = ? AND s.slug = ?`, workspaceID, slug))
}

// skillsInWorkspaces lists the skills of the given workspaces, newest activity
// first. Rows are fully drained before anything else touches the database —
// there is a single connection (see openDB).
func (s *Server) skillsInWorkspaces(workspaces []string) ([]skill, error) {
	if len(workspaces) == 0 {
		return []skill{}, nil
	}
	args := make([]any, len(workspaces))
	for i, w := range workspaces {
		args[i] = w
	}
	rows, err := s.db.Query(skillSelect+` WHERE s.workspace_id IN (`+placeholders(len(workspaces))+`)
		ORDER BY s.updated_at DESC, s.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []skill{}
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *Server) skillVersionByID(id string) (skillVersion, error) {
	return scanSkillVersion(s.db.QueryRow(skillVersionSelect+` WHERE v.id = ?`, id))
}

func (s *Server) skillVersionByNumber(skillID string, version int) (skillVersion, error) {
	return scanSkillVersion(s.db.QueryRow(skillVersionSelect+` WHERE v.skill_id = ? AND v.version = ?`, skillID, version))
}

// skillVersionRef resolves the {versionId} path segment, which may be either the
// version's opaque id or its human number.
//
// Both, because both are what a caller has in hand: the interface navigates by
// id, while an agent that resolved `architecture-review@4` has the number and
// nothing else. Refusing one of them would mean an extra lookup on every call
// for no gain in safety — the skill id still has to match either way.
func (s *Server) skillVersionRef(skillID, ref string) (skillVersion, error) {
	if n, err := parseVersionInt(ref); err == nil {
		return s.skillVersionByNumber(skillID, n)
	}
	v, err := s.skillVersionByID(ref)
	if err != nil {
		return skillVersion{}, err
	}
	if v.SkillID != skillID {
		return skillVersion{}, sql.ErrNoRows
	}
	return v, nil
}

func parseVersionInt(ref string) (int, error) {
	ref = strings.TrimSpace(strings.TrimPrefix(ref, "v"))
	if ref == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range ref {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func (s *Server) skillVersions(skillID string) ([]skillVersion, error) {
	rows, err := s.db.Query(skillVersionSelect+` WHERE v.skill_id = ? ORDER BY v.version DESC`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []skillVersion{}
	for rows.Next() {
		v, err := scanSkillVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// skillVersionsByStatus is the review queue's query: every version in a given
// lifecycle state across the caller's workspaces.
func (s *Server) skillVersionsByStatus(workspaces []string, status string) ([]skillVersion, error) {
	if len(workspaces) == 0 {
		return []skillVersion{}, nil
	}
	args := make([]any, 0, len(workspaces)+1)
	args = append(args, status)
	for _, w := range workspaces {
		args = append(args, w)
	}
	rows, err := s.db.Query(skillVersionSelect+` JOIN skills s ON s.id = v.skill_id
		WHERE v.status = ? AND s.workspace_id IN (`+placeholders(len(workspaces))+`)
		ORDER BY COALESCE(v.submitted_at, v.updated_at) ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []skillVersion{}
	for rows.Next() {
		v, err := scanSkillVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// publishedSkillVersions returns each skill's CURRENT published version for the
// given workspaces — the resolver's and the catalogue's candidate set.
//
// The join through skills.current_version_id is what makes "approved" and
// "runtime-visible" different things. An older approved version stays approved
// forever (so a resolution that pinned it can still be fetched) but is not a
// candidate for a NEW resolution, because it is not what the pointer says.
func (s *Server) publishedSkillVersions(workspaces []string) ([]skill, []skillVersion, error) {
	if len(workspaces) == 0 {
		return nil, nil, nil
	}
	// Two passes rather than one join: the skill and the version have different
	// row shapes, and with a single database connection the first query has to
	// be fully drained before the second can run anyway. A library is tens of
	// rows, not thousands — the join would buy nothing and cost a scan function
	// that exists only here.
	skills, err := s.skillsInWorkspaces(workspaces)
	if err != nil {
		return nil, nil, err
	}
	published := make([]skill, 0, len(skills))
	for _, sk := range skills {
		if sk.CurrentVersionID != "" {
			published = append(published, sk)
		}
	}
	// The two slices are returned index-aligned: outSkills[i] owns
	// outVersions[i]. The resolver needs both halves of every candidate and
	// this keeps it from having to build a map for a list of tens.
	outSkills := make([]skill, 0, len(published))
	outVersions := make([]skillVersion, 0, len(published))
	for _, sk := range published {
		v, err := s.skillVersionByID(sk.CurrentVersionID)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		// The pointer is only trustworthy if the version it names is still
		// approved. It cannot normally be anything else — deprecating clears
		// the pointer in the same transaction — but the resolver is the one
		// place where believing a stale pointer would put unreviewed text into
		// an agent's context, so it is checked here rather than assumed.
		if v.Status != skillStatusApproved {
			continue
		}
		outSkills = append(outSkills, sk)
		outVersions = append(outVersions, v)
	}
	return outSkills, outVersions, nil
}

// ---- writes ----

func insertSkillVersionTx(tx *sql.Tx, v skillVersion) error {
	triggers, _ := json.Marshal(orEmptyStrings(v.Triggers))
	refs, _ := json.Marshal(orEmptyRefs(v.References))
	deps, _ := json.Marshal(orEmptyDeps(v.Dependencies))
	scope, _ := json.Marshal(v.Scope)
	pkg, _ := json.Marshal(v.Package)
	_, err := tx.Exec(`INSERT INTO skill_versions
		(id, skill_id, version, delivery_mode, status, description, triggers, instructions, refs,
		 dependencies, scope, package_metadata, review_note, created_by, approved_by, created_at,
		 updated_at, submitted_at, approved_at, deprecated_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.SkillID, v.Version, v.DeliveryMode, v.Status, v.Description, string(triggers),
		v.Instructions, string(refs), string(deps), string(scope), string(pkg), v.ReviewNote,
		v.CreatedBy, nullIfEmpty(v.ApprovedBy), v.CreatedAt, v.UpdatedAt,
		nullIfEmpty(v.SubmittedAt), nullIfEmpty(v.ApprovedAt), nullIfEmpty(v.DeprecatedAt), v.ContentHash)
	return err
}

// updateDraftVersionTx writes a draft's content back, but ONLY while the row is
// still the draft the caller loaded.
//
// The two extra conditions are the whole point. `status = 'draft'` means a
// submit or an approval that landed in between cannot be overwritten by a stale
// form — the version a reviewer is looking at can never change under them.
// `updated_at = ?` is the optimistic lock: two people editing one draft, the
// second save is refused with a conflict rather than silently discarding the
// first.
func updateDraftVersionTx(tx *sql.Tx, v skillVersion, expectedUpdatedAt, ts string) (int64, error) {
	triggers, _ := json.Marshal(orEmptyStrings(v.Triggers))
	refs, _ := json.Marshal(orEmptyRefs(v.References))
	deps, _ := json.Marshal(orEmptyDeps(v.Dependencies))
	scope, _ := json.Marshal(v.Scope)
	pkg, _ := json.Marshal(v.Package)
	res, err := tx.Exec(`UPDATE skill_versions SET delivery_mode = ?, description = ?, triggers = ?,
		instructions = ?, refs = ?, dependencies = ?, scope = ?, package_metadata = ?,
		content_hash = ?, updated_at = ?
		WHERE id = ? AND status = ? AND updated_at = ?`,
		v.DeliveryMode, v.Description, string(triggers), v.Instructions, string(refs), string(deps),
		string(scope), string(pkg), v.ContentHash, ts,
		v.ID, skillStatusDraft, expectedUpdatedAt)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- audit ----

// recordSkillAuditTx writes the audit row INSIDE a transaction.
//
// Lifecycle events use this form because the decision was made explicitly (see
// §28 of the PRD, which demands the policy be stated): an approval whose audit
// row could not be written must not be an approval. Publishing is the moment
// text enters the trusted plane, and a trusted change nobody can trace is worse
// than a failed publish somebody retries.
func recordSkillAuditTx(tx *sql.Tx, e skillAuditEntry) error {
	missing, _ := json.Marshal(orEmptyStrings(e.MissingDependencies))
	_, err := tx.Exec(`INSERT INTO skill_audit
		(created_at, workspace_id, skill_id, skill_version_id, version_number, delivery_mode, action,
		 agent_type, project_ref, task_ref, resolution_reason, missing_dependencies, content_hash,
		 actor_user_id, actor_name, actor_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), e.WorkspaceID, e.SkillID, e.SkillVersionID, e.VersionNumber, e.DeliveryMode, e.Action,
		e.AgentType, e.ProjectRef, e.TaskRef, e.ResolutionReason, string(missing), e.ContentHash,
		e.ActorUserID, e.ActorName, e.ActorType)
	return err
}

// recordSkillAudit is the runtime form, outside a transaction.
//
// It returns the error rather than swallowing it, and each caller decides:
// `fetched` and `package_downloaded` fail the call if the row cannot be written
// (those ARE the delivery of trusted content, so an untraceable one is not
// allowed to succeed), while `resolved` logs and carries on — resolution pins
// nothing and delivers no instructions, and refusing to answer a discovery call
// because of a full disk would take the whole runtime down for no security
// gain.
func (s *Server) recordSkillAudit(e skillAuditEntry) error {
	missing, _ := json.Marshal(orEmptyStrings(e.MissingDependencies))
	_, err := s.db.Exec(`INSERT INTO skill_audit
		(created_at, workspace_id, skill_id, skill_version_id, version_number, delivery_mode, action,
		 agent_type, project_ref, task_ref, resolution_reason, missing_dependencies, content_hash,
		 actor_user_id, actor_name, actor_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), e.WorkspaceID, e.SkillID, e.SkillVersionID, e.VersionNumber, e.DeliveryMode, e.Action,
		e.AgentType, e.ProjectRef, e.TaskRef, e.ResolutionReason, string(missing), e.ContentHash,
		e.ActorUserID, e.ActorName, e.ActorType)
	return err
}

const skillAuditSelect = `SELECT id, created_at, workspace_id, skill_id, skill_version_id, version_number,
	delivery_mode, action, agent_type, project_ref, task_ref, resolution_reason, missing_dependencies,
	content_hash, actor_user_id, actor_name, actor_type FROM skill_audit`

func scanSkillAudit(sc interface{ Scan(...any) error }) (skillAuditEntry, error) {
	var e skillAuditEntry
	var missing string
	err := sc.Scan(&e.ID, &e.CreatedAt, &e.WorkspaceID, &e.SkillID, &e.SkillVersionID, &e.VersionNumber,
		&e.DeliveryMode, &e.Action, &e.AgentType, &e.ProjectRef, &e.TaskRef, &e.ResolutionReason,
		&missing, &e.ContentHash, &e.ActorUserID, &e.ActorName, &e.ActorType)
	if err != nil {
		return skillAuditEntry{}, err
	}
	if json.Unmarshal([]byte(missing), &e.MissingDependencies) != nil {
		e.MissingDependencies = nil
	}
	return e, nil
}

// skillAuditFilter is what the audit table's filters narrow by. Empty fields
// mean "everything", which is what an unfiltered view asks for.
type skillAuditFilter struct {
	Action  string
	Agent   string
	Version int
	Since   string
	Until   string
	Limit   int
}

func (s *Server) skillAudit(skillID string, f skillAuditFilter) ([]skillAuditEntry, error) {
	where := []string{"skill_id = ?"}
	args := []any{skillID}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.Agent != "" {
		where = append(where, "agent_type = ?")
		args = append(args, f.Agent)
	}
	if f.Version > 0 {
		where = append(where, "version_number = ?")
		args = append(args, f.Version)
	}
	if f.Since != "" {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since)
	}
	if f.Until != "" {
		where = append(where, "created_at <= ?")
		args = append(args, f.Until)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(skillAuditSelect+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []skillAuditEntry{}
	for rows.Next() {
		e, err := scanSkillAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
