# dworkspace Design System

## 1. Atmosphere & Identity

The app is a quiet, document-first workspace: familiar editorial reading space
with just enough operational density for teams. Its signature is green action
colour carried through soft tonal surfaces, not ornamental chrome. Review states
should feel like a calm safety rail around the document, making the pending
decision visible without turning the editor into an admin console.

## 2. Color

| Role | Token | Light | Dark | Usage |
|------|------|------|------|------|
| Surface / primary | `--bg` | `#ffffff` | `#191919` | App and document background |
| Surface / secondary | `--sidebar-bg` | `#f7f6f3` | `#202020` | Sidebars and diff context |
| Surface / elevated | `--card` | `#ffffff` | `#242424` | Dialogs and proposal panels |
| Text / primary | `--fg` | `#37352f` | `#d4d4d4` | Document and headings |
| Text / secondary | `--muted` | `#787774` | `#8f8f8f` | Metadata and hints |
| Border / default | `--border` | `#e9e7e4` | `#2f2f2f` | Dividers and controls |
| Accent / primary | `--accent` | `#2f7d4f` | `#4fa872` | Primary action and pending emphasis |
| Accent / soft | `--accent-soft` | `rgba(47,125,79,.12)` | `rgba(79,168,114,.16)` | Active and pending surfaces |
| Status / success | `--accent` | `#2f7d4f` | `#4fa872` | Published state |
| Status / warning | `--glow-orange` | `#f97316` | `#f97316` | Pending state and stale warning |
| Status / error | `--danger` | `#c4554d` | `#e06c62` | Rejection and destructive action |

Accent is reserved for actions and state. The review UI uses the existing
border-and-shadow dialog treatment and soft status surfaces rather than adding
new decorative colours.

## 3. Typography

- Primary: Inter, `-apple-system`, BlinkMacSystemFont, `Segoe UI`, sans-serif.
- Mono: JetBrains Mono for hashes and machine-readable identifiers.
- Page and dialog headings use the existing 22/28px scale; body text is 14px;
  captions and metadata are 12px.

## 4. Spacing & Layout

All spacing uses the existing 4px base unit: 4, 8, 12, 16, 20, 24, and 32px.
The editor is a bounded app shell. `.page-body` owns document scrolling;
proposal review is a modal with its own bounded scroll body. At 375px the
review switches from a two-column comparison to a single readable column.

## 5. Components

### Proposal review modal

- **Structure:** modal overlay → dialog header/status → proposal list → wide
  review workspace. The workspace has an editable document column and a
  Knowledge Review pane with deterministic fact gaps and conservative related
  document candidates.
- **Variants:** pending, published, rejected; preview and changes-only views.
- **Spacing:** 8px clusters, 16px panel padding, 24px dialog padding.
- **States:** loading, empty, pending, published, rejected, conflict/error.
- **Create state:** `New document · Will be created on Publish` is explicit;
  before publish there is no page, tree entry, search index row, graph edge, or
  `page.created` event. Human edits update proposal state only.
- **Knowledge review:** facts are labelled `provided`, `derived`, `assumption`,
  `missing`, or `unsupported`. Agent-supplied categories and constraints are
  untrusted claims and stay `UNKNOWN`; only server-trusted evidence or an
  explicit human browser confirmation can produce deterministic counts and
  gaps. Free-form rules are never heuristically parsed. A confirmed 3-of-5
  deterministic constraint is shown as `3/5` with two editable gaps.
- **Related documents:** candidates show title, rationale, snippet, and rank.
  Agents cannot preselect them. Humans can search, add, dismiss, check, and
  uncheck candidates. Only selected, re-authorized candidates become links at
  publish; candidates never fill fact gaps.
- **Provenance:** the original proposal snapshot, agent creator, human editor,
  and human publisher remain auditable. Human edits use a structure-preserving
  block editor and save exact proposed block JSON. Edit proposals retain their
  original revision hash over title, body, and relevant metadata for stale
  protection; canonical metadata changes also block publication.
- **Accessibility:** labelled dialog, native buttons, keyboard tab order,
  visible focus, status text independent of colour.
- **Motion:** existing 100–150ms control transitions; no decorative motion.
- **Layout:** desktop uses a wide `min(1240px, 94vw)` dialog with a compact
  `264px` proposal navigation column and a flexing review pane. The dialog is
  roughly `86dvh` tall; preview and diff content own their internal scroll.
  On narrow widths, the comparison `switcher` collapses to one column and
  returns scroll ownership to the bounded modal body.

### Status badge

- **Structure:** inline label with text and state colour.
- **Variants:** pending, published, rejected.
- **Spacing:** 4px horizontal padding, 8px gap from adjacent metadata.
- **States:** default and focus when it is part of a control.
- **Accessibility:** state is written as text, never colour-only.

## 6. Motion & Interaction

Controls use the existing ease-out 100–150ms hover/press transition. Modal
entry and tab changes remain static unless the existing shell supplies motion.
`prefers-reduced-motion` disables non-essential transitions.

## 7. Depth & Surface

Strategy: mixed. Existing dialogs use a 1px border plus the shared `--shadow`;
review panes use tonal surfaces and subtle borders to distinguish canonical and
proposed content. No new shadow recipes are introduced.

## 8. Accessibility Constraints & Accepted Debt

- WCAG 2.2 AA target, 4.5:1 body contrast, visible keyboard focus, full
  keyboard reachability, and reduced-motion support.
- The MVP diff is block-aware and text-normalised rather than a rich-text
  character diff. This is accepted because the requirement prioritises seeing
  changed sections before publish; a future inline rich-text diff can replace
  the projection without changing the proposal contract.

## 9. MCP Apps Proposal Review

VUS exposes `DocumentProposalView` as a bundled, self-contained
`text/html;profile=mcp-app` resource at
`ui://dworkspace/proposals/document-review.html`. The proposal tool links it
with `_meta.ui.resourceUri` and retains both `structuredContent` and a
meaningful text fallback. The view follows the MCP Apps `ui/initialize`,
`ui/notifications/initialized`, `ui/notifications/tool-result`, and
`ui/notifications/size-changed` messages, and uses host theme variables when
provided. It renders the full proposed document, canonical context, and a
block-aware changes-only view at responsive sizes.

The view has no external assets, API tokens, or network dependencies. Its
resource metadata declares empty CSP domains and requests a visible host
border. This is intentional for sandboxed hosts and keeps the review content
inside the tool result. Clients without MCP Apps support receive the same
structured proposal data and text status message as a normal MCP tool.

Create results identify `kind=create`, have no `pageId` before publish, and
include a small fact-review and related-candidate summary. Fact constraints are
validated only when they are machine-readable; otherwise the UI shows
`UNKNOWN`. Candidates are suggestions, never facts or automatic links.

Approval is not an MCP App action. VUS does not expose publish/reject tools,
because this stateless bearer transport cannot establish app-call provenance
strong enough to prevent a model from replaying one. The widget hands a human
to the signed-in VUS browser review surface, where page permission and the
  proposal revision hash are checked before the existing atomic publish path runs.
