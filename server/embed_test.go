package server

import (
	"strings"
	"testing"
)

// An embed is a reference, and every place that reasons about references has to
// know about it. These pin the three that do.

// Backlinks. An embed is a stronger reference than a link — the page is not
// pointed at, it is reproduced — so a page shown inside five others must be
// able to say so. It could not: extractLinks only knew the inline pageLink, and
// the links table stayed empty however many embeds were placed.
func TestEmbedCountsAsALink(t *testing.T) {
	content := []byte(`[
		{"type":"paragraph","content":[{"type":"text","text":"before"}]},
		{"type":"embed","props":{"pageId":"target-1"}},
		{"type":"paragraph","content":[
			{"type":"pageLink","props":{"pageId":"target-2","label":"A link"}}
		]}
	]`)
	got := extractLinks(content)
	want := map[string]bool{"target-1": true, "target-2": true}
	if len(got) != len(want) {
		t.Fatalf("extractLinks returned %v, want both the embed and the link", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected link %q", id)
		}
	}
}

// The same page embedded twice is one reference: the links table is keyed on
// (source, target), so a duplicate would be an insert that fails rather than a
// second row, and nothing downstream wants to count appearances anyway.
func TestEmbedOfTheSamePageIsCountedOnce(t *testing.T) {
	content := []byte(`[
		{"type":"embed","props":{"pageId":"same"}},
		{"type":"embed","props":{"pageId":"same"}},
		{"type":"paragraph","content":[{"type":"pageLink","props":{"pageId":"same"}}]}
	]`)
	if got := extractLinks(content); len(got) != 1 || got[0] != "same" {
		t.Errorf("extractLinks = %v, want exactly one \"same\"", got)
	}
}

// An embed with no page chosen yet — the block exists from the moment it is
// inserted and is only given a target when one is picked from its list.
func TestEmptyEmbedLinksToNothing(t *testing.T) {
	if got := extractLinks([]byte(`[{"type":"embed","props":{"pageId":""}}]`)); len(got) != 0 {
		t.Errorf("extractLinks = %v, want none", got)
	}
}

// Both exports carry the reference and NOT the text. Inlining the body would
// turn one page into two copies that drift apart at the next edit, which is the
// thing embedding exists to avoid — the same call the database block makes.
func TestExportsCarryTheReferenceNotTheBody(t *testing.T) {
	content := []byte(`[
		{"type":"paragraph","content":[{"type":"text","text":"host text"}]},
		{"type":"embed","props":{"pageId":"abc123"}}
	]`)

	md := blocksToMarkdown(content)
	if !strings.Contains(md, "/p/abc123") {
		t.Errorf("Markdown export lost the embed reference:\n%s", md)
	}
	if !strings.Contains(md, "host text") {
		t.Errorf("Markdown export lost the host's own text:\n%s", md)
	}

	htm := blocksToHTML(content)
	if !strings.Contains(htm, "/p/abc123") {
		t.Errorf("HTML export lost the embed reference:\n%s", htm)
	}
}
