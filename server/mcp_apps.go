package server

import (
	_ "embed"
	"strings"
)

const (
	mcpAppResourceURI = "ui://dworkspace/proposals/document-review.html"
	mcpAppMIME        = "text/html;profile=mcp-app"
)

//go:embed document_proposal_view.html
var documentProposalViewHTML string

// mcpReviewURL only uses administrator-controlled external bases. Falling back
// to the request Host header would let an untrusted MCP caller turn the human
// handoff into an attacker-controlled link.
func (s *Server) mcpReviewURL(path string) string {
	base := s.setting("public_base_url", "")
	if base == "" {
		if domain, enabled := s.PublicHTTPSConfig(); enabled && domain != "" {
			base = "https://" + strings.TrimRight(domain, "/")
		}
	}
	if base == "" {
		s.tunnel.mu.Lock()
		base = s.tunnel.url
		s.tunnel.mu.Unlock()
	}
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + path
}

func mcpProposalViewMeta() map[string]any {
	return map[string]any{
		"ui": map[string]any{
			"csp": map[string]any{
				"connectDomains":  []string{},
				"resourceDomains": []string{},
				"frameDomains":    []string{},
				"baseUriDomains":  []string{},
			},
			"prefersBorder": true,
		},
	}
}

func mcpProposalViewResource() map[string]any {
	return map[string]any{
		"uri": mcpAppResourceURI, "name": "DocumentProposalView",
		"description": "Inline preview and block-aware diff for a VUS proposed document revision. Approval remains in the authenticated VUS browser.",
		"mimeType":    mcpAppMIME, "_meta": mcpProposalViewMeta(),
	}
}

func mcpProposalViewContents() map[string]any {
	return map[string]any{
		"contents": []map[string]any{{
			"uri": mcpAppResourceURI, "mimeType": mcpAppMIME,
			"text": documentProposalViewHTML, "_meta": mcpProposalViewMeta(),
		}},
	}
}
