import { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api';
import type { Backlink, GraphEdge, PageMeta, DworkspaceFile } from '../types';
import { PageIcon } from '../pageIcon';
import { FilePreview, isPreviewable } from './FilePreview';
import { formatBytes } from '../format';
import { t } from '../i18n';
import GraphView from './GraphView';
import {
  ChevronDown,
  ChevronRight,
  CornerDownRight,
  FileText,
  Link2,
  PanelRightClose,
  Table2,
} from 'lucide-react';

// What a document carries but never showed: the pages below it, the files
// hanging off it, and who points at it. All three existed — parent_id has
// always built the tree, the file index (W125) has always known which page an
// upload belongs to, and backlinks were already collected for the strip under
// the body — but a reader had to hunt through the sidebar or scroll to the end
// to find any of it. The panel is a view onto data that was already there.

const PANEL_KEY = 'dworkspace-structure-open';
const GRAPH_KEY = 'dworkspace-structure-graph';

export function structurePanelOpen(): boolean {
  return localStorage.getItem(PANEL_KEY) === '1';
}

export function setStructurePanelOpen(open: boolean): void {
  if (open) localStorage.setItem(PANEL_KEY, '1');
  else localStorage.removeItem(PANEL_KEY);
}

// ---- the local graph ----
//
// The panel already answers "what is under this page", "what hangs off it" and
// "who points at it" as three lists. The graph answers the question none of
// them can: what does this page sit in the MIDDLE of. One hop out, every kind
// of edge, the page itself lit in the centre — Obsidian's local graph, from the
// data /api/graph has served the library view all along.
//
// Edges are cached across navigations. The editor is keyed by page id, so this
// panel is torn down and rebuilt every time somebody opens a document, and a
// fetch per navigation would be a request for data that had not changed. It is
// small (250 edges for 2,482 pages on the instance this was built against) and
// changes only when somebody edits a link, which arrives as a pages event
// anyway.
const GRAPH_TTL = 60_000;
let graphCache: { at: number; edges: GraphEdge[] } | null = null;
let graphInFlight: Promise<GraphEdge[]> | null = null;

function loadGraphEdges(): Promise<GraphEdge[]> {
  if (graphCache && Date.now() - graphCache.at < GRAPH_TTL) {
    return Promise.resolve(graphCache.edges);
  }
  // Coalesced, not just cached: opening two documents quickly would otherwise
  // fire two identical requests before either had answered.
  if (!graphInFlight) {
    graphInFlight = api
      .graph()
      .then((g) => {
        graphCache = { at: Date.now(), edges: g.edges };
        return g.edges;
      })
      .catch(() => [] as GraphEdge[])
      .finally(() => {
        graphInFlight = null;
      });
  }
  return graphInFlight;
}

// The ids within `hops` steps of `pageId`, over links and filing alike.
// Deliberately duplicated from GraphView's own neighborhood(): that one runs
// over the pages it was HANDED, and the whole problem here is working out which
// pages to hand it before they have been loaded.
//
// Two hops, where the panel draws one. The extra ring is fetched but not drawn
// on purpose: a neighbour's own neighbours are what make the next click
// instant, and the metadata for one more ring is a few kilobytes.
function nearbyIds(
  pageId: string,
  pagesById: Map<string, PageMeta>,
  edges: GraphEdge[],
  hops = 2,
): Set<string> {
  const adj = new Map<string, Set<string>>();
  const join = (a: string, b: string) => {
    if (!adj.has(a)) adj.set(a, new Set());
    adj.get(a)!.add(b);
    if (!adj.has(b)) adj.set(b, new Set());
    adj.get(b)!.add(a);
  };
  for (const p of pagesById.values()) {
    if (p.parentId) join(p.id, p.parentId);
  }
  for (const e of edges) join(e.source, e.target);

  const seen = new Set([pageId]);
  let frontier: string[] = [pageId];
  for (let hop = 0; hop < hops; hop++) {
    const next: string[] = [];
    for (const id of frontier) {
      for (const nb of adj.get(id) ?? []) {
        if (!seen.has(nb)) {
          seen.add(nb);
          next.push(nb);
        }
      }
    }
    frontier = next;
  }
  return seen;
}

type TreeItem = { page: PageMeta; depth: number };

// The chain from the top down to (but not including) this page. The panel used
// to look only downwards, so standing on a sub-page you could see everything
// below you and nothing about where you were — the breadcrumb in the topbar
// knows, but that is above the reading line and easy to miss. Nearest parent
// last, so the list reads top-down like the tree does.
function ancestors(pageId: string, pagesById: Map<string, PageMeta>): PageMeta[] {
	const out: PageMeta[] = [];
	const seen = new Set<string>([pageId]);
	let cur = pagesById.get(pageId)?.parentId ?? null;
	while (cur && !seen.has(cur)) {
		seen.add(cur); // a cycle cannot happen, but a bad import must not hang the panel
		const p = pagesById.get(cur);
		if (!p) break; // a database row's parent is not in the tree map — stop there
		out.unshift(p);
		cur = p.parentId;
	}
	return out;
}

// Children of `rootId`, depth-first, so the panel shows the shape of the
// subtree and not just its first level. Database rows are absent by design:
// the page tree endpoint leaves them out (they belong in the collection view,
// and there can be tens of thousands), so a collection simply shows no
// sub-pages here.
function subtree(rootId: string, pagesById: Map<string, PageMeta>): TreeItem[] {
  const byParent = new Map<string, PageMeta[]>();
  for (const p of pagesById.values()) {
    if (p.trashed || !p.parentId) continue;
    const list = byParent.get(p.parentId);
    if (list) list.push(p);
    else byParent.set(p.parentId, [p]);
  }
  const out: TreeItem[] = [];
  const walk = (id: string, depth: number) => {
    const kids = byParent.get(id);
    if (!kids) return;
    for (const p of [...kids].sort((a, b) => a.position - b.position)) {
      out.push({ page: p, depth });
      walk(p.id, depth + 1); // cycles are impossible: moves reject them server-side
    }
  };
  walk(rootId, 0);
  return out;
}

// A file's kind carries colour because the list is scanned, not read: the eye
// finds "the spreadsheet" faster than it reads three names. Kinds outside this
// table stay neutral rather than getting an arbitrary hue.
const EXT_CLASS: Record<string, string> = {
  pdf: 'ext-pdf',
  xls: 'ext-sheet',
  xlsx: 'ext-sheet',
  csv: 'ext-sheet',
  doc: 'ext-doc',
  docx: 'ext-doc',
  ppt: 'ext-slide',
  pptx: 'ext-slide',
};

function Section({ label, count, children }: { label: string; count: number; children: React.ReactNode }) {
  return (
    <div className="structure-section">
      <div className="structure-label">
        {label} <span className="structure-count">· {count}</span>
      </div>
      {children}
    </div>
  );
}

export default function StructurePanel({
  pageId,
  isCollection,
  pagesById,
  onNavigate,
  onClose,
}: {
  pageId: string;
  isCollection: boolean;
  pagesById: Map<string, PageMeta>;
  onNavigate: (id: string | null) => void;
  onClose: () => void;
}) {
  const [files, setFiles] = useState<DworkspaceFile[]>([]);
  const [links, setLinks] = useState<Backlink[]>([]);
  const [preview, setPreview] = useState<{ name: string; url: string } | null>(null);
  const [edges, setEdges] = useState<GraphEdge[]>(() => graphCache?.edges ?? []);
  // Neighbours the page tree has not loaded. Since the sidebar started loading
  // a level at a time, pagesById holds what somebody has unfolded — and a page
  // two link-hops away is exactly the kind of page nobody unfolded.
  const [nearby, setNearby] = useState<Map<string, PageMeta>>(new Map());
  const [graphOpen, setGraphOpen] = useState(() => localStorage.getItem(GRAPH_KEY) !== '0');
  const askedFor = useRef<Set<string>>(new Set());

  const pages = useMemo(() => subtree(pageId, pagesById), [pageId, pagesById]);
  const above = useMemo(() => ancestors(pageId, pagesById), [pageId, pagesById]);

  // The pages the graph may draw: everything already loaded, plus the
  // neighbours fetched for it. GraphView narrows this to two hops itself.
  const graphPages = useMemo(() => {
    const byId = new Map(pagesById);
    for (const [id, p] of nearby) if (!byId.has(id)) byId.set(id, p);
    return [...byId.values()];
  }, [pagesById, nearby]);

  // How much there is to draw — ONE hop, matching what the panel actually
  // renders. One dot is not a graph: it is the same "nothing links here" the
  // section below already says, drawn as a circle.
  const graphSize = useMemo(
    () => (graphOpen ? nearbyIds(pageId, pagesById, edges, 1).size : 0),
    [graphOpen, pageId, pagesById, edges],
  );

  useEffect(() => {
    if (!graphOpen) return;
    let alive = true;
    void loadGraphEdges().then((e) => alive && setEdges(e));
    return () => {
      alive = false;
    };
  }, [graphOpen, pageId]);

  // Fetch the metadata for neighbours the tree never loaded. One round, not a
  // loop: the ids come from edges that are already known, and chasing the
  // parents of the pages this brings back would walk the whole tree one
  // request at a time. GraphView draws a page whose parent is missing as a root
  // of its own, which is the honest picture of what is loaded.
  useEffect(() => {
    if (!graphOpen) return;
    const want = [...nearbyIds(pageId, pagesById, edges)].filter(
      (id) => !pagesById.has(id) && !nearby.has(id) && !askedFor.current.has(id),
    );
    if (want.length === 0) return;
    for (const id of want) askedFor.current.add(id); // never ask twice, even if the answer is "gone"
    let alive = true;
    void api
      .listPages({ ids: want })
      .then((list) => {
        if (!alive) return;
        setNearby((prev) => {
          const next = new Map(prev);
          for (const p of list) next.set(p.id, p);
          return next;
        });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [graphOpen, pageId, pagesById, edges, nearby]);

  useEffect(() => {
    let alive = true;
    api
      .listFiles({ under: pageId })
      .then((f) => alive && setFiles(f))
      .catch(() => alive && setFiles([]));
    api
      .backlinks(pageId)
      .then((l) => alive && setLinks(l))
      .catch(() => alive && setLinks([]));
    return () => {
      alive = false;
    };
  }, [pageId, pagesById]);

  const openFile = (f: DworkspaceFile) => {
    const url = '/files/' + f.name;
    if (isPreviewable(url)) setPreview({ name: f.displayName || f.name, url });
    else window.open(url, '_blank', 'noopener');
  };

  return (
    <aside className="structure-panel" aria-label={t('Structure')}>
      <div className="structure-head">
        <span>{t('Structure')}</span>
        <button className="icon-btn" title={t('Hide structure')} onClick={onClose}>
          <PanelRightClose size={17} />
        </button>
      </div>

      {/* The head holds still and only this scrolls — the same shape as the
          comments panel, which is the other thing that can stand here. */}
      <div className="structure-scroll">

        {/* What this page sits in the middle of. Foldable and remembered,
            because it is the one section here that costs a canvas and a force
            simulation rather than a few rows of text. */}
        <div className="structure-section structure-graph-section">
          <button
            className="structure-label structure-fold"
            onClick={() => {
              const next = !graphOpen;
              setGraphOpen(next);
              localStorage.setItem(GRAPH_KEY, next ? '1' : '0');
            }}
            aria-expanded={graphOpen}
          >
            {graphOpen ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
            {t('Graph')}
            {graphOpen && graphSize > 1 && <span className="structure-count">· {graphSize}</span>}
          </button>
          {graphOpen &&
            (graphSize > 1 ? (
              <div className="structure-graph">
                <GraphView
                  compact
                  pages={graphPages}
                  edges={edges}
                  focusId={pageId}
                  onNavigate={(id) => onNavigate(id)}
                />
              </div>
            ) : (
              <div className="structure-empty">{t('Nothing connects to this page yet')}</div>
            ))}
        </div>

        {/* Where this page sits, before what sits under it. Shown only when
            there is somewhere to go: on a top-level page the section would be an
            empty box saying nothing. */}
        {above.length > 0 && (
          <div className="structure-section structure-above">
            {above.map((p, i) => (
              <button
                key={p.id}
                className="structure-item"
                style={{ paddingLeft: 8 + i * 10 }}
                onClick={() => onNavigate(p.id)}
                title={p.title || t('Untitled')}
              >
                <span className="structure-icon">
                  <PageIcon
                    icon={p.icon}
                    size={14}
                    fallback={p.type === 'collection' ? <Table2 size={14} /> : <FileText size={14} />}
                  />
                </span>
                <span className="structure-text">{p.title || t('Untitled')}</span>
              </button>
            ))}
            {/* The current page closes the chain, so the panel shows a position
                and not just a list of strangers. Not a button: you are here. */}
            <div className="structure-item structure-here" style={{ paddingLeft: 8 + above.length * 10 }}>
              <span className="structure-icon">
                <CornerDownRight size={13} />
              </span>
              <span className="structure-text">{t('This page')}</span>
            </div>
          </div>
        )}

        {/* A database's rows ARE its sub-pages, but they live in the table and
            are deliberately kept out of the page tree. Showing an empty
            "0 sub-pages" next to a table full of rows would read as a bug, so
            the section stays out entirely — the files below still count. */}
        {!isCollection && (
          <Section label={t('Sub-pages')} count={pages.length}>
            {pages.length === 0 && <div className="structure-empty">{t('No sub-pages')}</div>}
            {pages.map(({ page, depth }) => (
              <button
                key={page.id}
                className="structure-item"
                style={{ paddingLeft: 8 + Math.min(depth, 4) * 14 }}
                onClick={() => onNavigate(page.id)}
                title={page.title || t('Untitled')}
              >
                {/* Depth reads as a turn, not as blank space: indentation alone
                    is ambiguous once a title wraps or a list gets long. */}
                <span className="structure-icon">
                  {depth > 0 ? (
                    <CornerDownRight size={13} />
                  ) : (
                    <PageIcon
                      icon={page.icon}
                      size={14}
                      fallback={page.type === 'collection' ? <Table2 size={14} /> : <FileText size={14} />}
                    />
                  )}
                </span>
                <span className="structure-text">{page.title || t('Untitled')}</span>
              </button>
            ))}
          </Section>
        )}

        <Section label={t('Files')} count={files.length}>
          {files.length === 0 && <div className="structure-empty">{t('No files')}</div>}
          {files.map((f) => {
            const ext = f.ext.replace('.', '').toLowerCase();
            return (
              <button
                key={f.name}
                className="structure-item structure-file"
                onClick={() => openFile(f)}
                title={f.pageId === pageId ? f.displayName : `${f.displayName} — ${f.pageTitle}`}
              >
                <span className={'structure-icon structure-ext ' + (EXT_CLASS[ext] ?? '')}>
                  {ext.slice(0, 4) || '?'}
                </span>
                <span className="structure-text">
                  {f.displayName || f.name}
                  {/* Two files can carry the same name and differ only in which
                      sub-page they hang off — "real.pdf" twice over tells the
                      reader nothing. The source page is named whenever it is not
                      the document already on screen. */}
                  {f.pageId !== pageId && f.pageTitle && (
                    <span className="structure-from">{f.pageTitle}</span>
                  )}
                </span>
                <span className="structure-size">{formatBytes(f.size)}</span>
              </button>
            );
          })}
        </Section>

        <Section label={t('Linked from')} count={links.length}>
          {links.length === 0 && <div className="structure-empty">{t('Nothing links here')}</div>}
          {links.map((l) => (
            <button
              key={l.id}
              className="structure-item"
              onClick={() => onNavigate(l.id)}
              title={l.title || t('Untitled')}
            >
              <span className="structure-icon">
                <Link2 size={14} />
              </span>
              <span className="structure-text">{l.title || t('Untitled')}</span>
            </button>
          ))}
        </Section>

      </div>

      {preview && (
        <FilePreview name={preview.name} url={preview.url} onClose={() => setPreview(null)} />
      )}
    </aside>
  );
}
