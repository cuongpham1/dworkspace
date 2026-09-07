package server

import "encoding/json"

// ---- MCP-facing JSON helpers ----

// catalogEntry is the metadata-only shape `skill_catalog` returns.
//
// There is no `instructions` field on this struct, and that is the mechanism
// rather than a convention: the catalogue physically cannot leak an instruction
// body, because there is nowhere in the type to put one. Progressive disclosure
// enforced by the type system is the version of it that survives a refactor.
type catalogEntry struct {
	SkillID      string     `json:"skill_id"`
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Version      int        `json:"version"`
	DeliveryMode string     `json:"delivery_mode"`
	ContentHash  string     `json:"content_hash"`
	Triggers     []string   `json:"triggers"`
	Scope        skillScope `json:"scope"`
	ScopeMatch   bool       `json:"scope_match"`
	Dependencies []string   `json:"dependencies"`
	Workspace    string     `json:"workspace_id"`
}

func catalogEntryFor(sk skill, v skillVersion, scopeMatch bool) catalogEntry {
	deps := make([]string, 0, len(v.Dependencies))
	for _, d := range v.Dependencies {
		label := d.Type + ":" + d.ID
		if d.VersionConstraint != "" {
			label += " " + d.VersionConstraint
		}
		if d.Required {
			label += " (required)"
		}
		deps = append(deps, label)
	}
	return catalogEntry{
		SkillID: sk.ID, Slug: sk.Slug, Name: sk.Name, Description: v.Description,
		Version: v.Version, DeliveryMode: v.DeliveryMode, ContentHash: v.ContentHash,
		Triggers: orEmptyStrings(v.Triggers), Scope: v.Scope, ScopeMatch: scopeMatch,
		Dependencies: deps, Workspace: sk.WorkspaceID,
	}
}

func skillJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return `{"error":"could not be rendered"}`
	}
	return string(b)
}
