package server

import (
	"net/http"
	"strings"
)

// ---- legacy adoption ----

// handleAdoptLegacySkill imports an existing Skills Library page as a NORMALIZED
// DRAFT — and nothing more than a draft.
//
// This is the migration path §26 of the PRD asks for, and the shape of it is
// the whole point. The legacy library is a collection of pages: `Loại`,
// `Trạng thái`, `Tác giả`. A page whose status column reads "Đã duyệt" was
// approved by whatever that column means to the person who typed it — which is
// not the same act as a workspace admin reviewing text before agents follow it,
// and must not be treated as one.
//
// So this endpoint:
//
//   - reads the page's text as UNTRUSTED content, exactly as an agent would;
//   - creates a DRAFT version from it, never an approved one;
//   - ignores the legacy status entirely, including "Đã duyệt";
//   - defaults the delivery mode to CLIENT, because a legacy `Claude Skill` was
//     something somebody installed;
//   - and leaves the new draft to walk the same review path as anything else.
//
// The result: nothing about editing a collection field can produce trusted
// instructions. Adoption is a convenience that saves retyping, not a shortcut
// around review.
func (s *Server) handleAdoptLegacySkill(w http.ResponseWriter, r *http.Request) {
	u := requestUser(r)
	var body struct {
		PageID       string `json:"pageId"`
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		DeliveryMode string `json:"deliveryMode"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpErrorCode(w, http.StatusBadRequest, "bad_json", "invalid JSON")
		return
	}
	if body.PageID == "" {
		httpErrorCode(w, http.StatusBadRequest, "page_required", "name the page to adopt")
		return
	}
	if !s.canRead(u.ID, body.PageID) {
		httpErrorCode(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	ws := s.pageWorkspace(body.PageID)
	// Adoption is an admin act. Not because reading the page needs it — any
	// member may — but because it puts something into the review queue as a
	// candidate for trusted execution, and §26 asks for an admin-driven,
	// explicit path rather than a bulk import.
	if !s.isWorkspaceAdmin(u.ID, ws) {
		httpErrorCode(w, http.StatusForbidden, "skill_review_only",
			"only a workspace admin can adopt a legacy skill page")
		return
	}
	page, err := s.getPage(body.PageID)
	if err != nil {
		httpErrorCode(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	markdown := pageMarkdown(page)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = page.Title
	}
	delivery := strings.ToLower(strings.TrimSpace(body.DeliveryMode))
	if delivery != skillDeliveryRemote && delivery != skillDeliveryClient {
		delivery = skillDeliveryClient
	}
	description := strings.TrimSpace(page.Description)
	if description == "" {
		description = firstLine(markdown, maxSkillDescriptionLen)
	}
	slug := body.Slug
	scope := skillScope{WorkspaceID: ws}
	sk, v, err := s.createSkill(u, ws, skillDraftInput{
		Name: &name, Slug: &slug, Description: &description, DeliveryMode: &delivery,
		Instructions: &markdown, Scope: &scope,
		References: &[]skillReference{{Kind: "page", Target: page.ID, Label: page.Title}},
	})
	if err != nil {
		writeSkillError(w, err)
		return
	}
	s.audit("human", u.ID, u.Name, "skill_adopted_from_page", page.ID, ws, sk.Name)
	writeJSON(w, map[string]any{
		"skill": sk, "version": v,
		// Said out loud in the response, because this is exactly the place
		// somebody might assume otherwise.
		"note": "adopted as a draft — it must pass review before any agent can use it",
	})
}

func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#> -*"))
		if line == "" {
			continue
		}
		if len([]rune(line)) > max {
			return string([]rune(line)[:max])
		}
		return line
	}
	return ""
}
