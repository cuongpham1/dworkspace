import { useEffect, useMemo, useRef, useState } from 'react';
import { Download } from 'lucide-react';
import type { GraphEdge, PageMeta } from '../types';
import { t, plural } from '../i18n';

// The graph: every page as a dot, every connection as a line, settling into
// shape by itself.
//
// Three kinds of edge, and keeping them apart is what makes it readable rather
// than decorative:
//
//   PARENT    where a page is filed. Structure, drawn thin and quiet — it is
//             the thing the sidebar already tells you.
//   LINK      a mention of one page inside another. Drawn bright, because THIS
//             is what a graph is for: the connection nobody filed anywhere,
//             the one you cannot see in a tree.
//   RELATION  a database relation property — a real, human-named connection
//             a team already built (Feature "depends on" Feature, Feature
//             "impacts" Metric). Drawn in its own colour per distinct label,
//             since "depends on" and "impacts" mean different things and a
//             reader filtering the graph needs to tell them apart.
//
// Canvas, not SVG, and no library. A thousand nodes as DOM elements is a
// slideshow; the same thousand on a canvas is a smooth 60fps, and the force
// simulation is about forty lines. Nothing here reaches the network.

const REPULSION = 5200; // how hard two dots push apart
const SPRING = 0.012; // how hard an edge pulls together
const PARENT_LEN = 70; // resting length of a "filed under" edge
const LINK_LEN = 130; // a mention may sit further away
const RELATION_LEN = 150;
const DAMPING = 0.86;
const CENTER_PULL = 0.004;

// The dworkspace palette, turned up. A graph is the one screen where punchy is
// correct: here the colour is doing work rather than decorating text somebody
// has to read.
//
// It groups by ROOT — the top-level page a dot ultimately hangs off — not by
// workspace. Colouring by workspace was the obvious choice and it is useless
// in the common case: most people keep everything in one, and the whole picture
// came out a single shade of green. By root, each customer, project or area
// gets its own colour inside one workspace, which is the grouping somebody
// actually looks for.
const HUES = [
  '#2f9e5f', // dworkspace green, the house colour, brightened
  '#3f86e0',
  '#e0a53b',
  '#9a5fd6',
  '#e2645a',
  '#2fb6bd',
  '#e070ab',
  '#8bc020',
  '#f08a3c',
  '#5f6fe0',
];

// Relation edges get their own small palette, distinct from node colours (root
// families) so a "depends on" line is never confused for "this page belongs to
// that cluster".
const RELATION_HUES = ['#e2645a', '#9a5fd6', '#e0a53b', '#2fb6bd', '#e070ab', '#5f6fe0'];

interface Node {
  id: string;
  title: string;
  isDb: boolean;
  ws: string;
  x: number;
  y: number;
  vx: number;
  vy: number;
  r: number;
  color: string;
  deg: number;
}

interface Edge {
  a: number;
  b: number;
  kind: 'parent' | 'link' | 'relation';
  // Which toggle this edge belongs to: 'parent', 'link', or "relation:<label>"
  // for a relation, so two differently-named relation fields can be shown or
  // hidden independently.
  filterKey: string;
  color: string;
}

// The set of nodes within `hops` steps of `focusId`, walking every edge kind —
// what "See related graph" actually shows: not the whole workspace, just what
// is reachable from here.
function neighborhood(focusId: string, pages: PageMeta[], linkEdges: GraphEdge[], hops: number): Set<string> {
  const adj = new Map<string, Set<string>>();
  const link = (a: string, b: string) => {
    if (!adj.has(a)) adj.set(a, new Set());
    adj.get(a)!.add(b);
  };
  for (const p of pages) {
    if (p.parentId) {
      link(p.id, p.parentId);
      link(p.parentId, p.id);
    }
  }
  for (const e of linkEdges) {
    link(e.source, e.target);
    link(e.target, e.source);
  }
  let frontier = new Set([focusId]);
  const seen = new Set([focusId]);
  for (let h = 0; h < hops; h++) {
    const next = new Set<string>();
    for (const id of frontier) {
      for (const nb of adj.get(id) ?? []) {
        if (!seen.has(nb)) {
          seen.add(nb);
          next.add(nb);
        }
      }
    }
    frontier = next;
  }
  return seen;
}

export default function GraphView({
  pages,
  edges: linkEdges,
  onNavigate,
  focusId,
}: {
  pages: PageMeta[];
  edges: GraphEdge[];
  onNavigate: (id: string) => void;
  // When set, the graph shows only this page's neighborhood (2 hops) instead
  // of the whole workspace — what "See related graph" on a page opens into.
  focusId?: string;
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [hoverTitle, setHoverTitle] = useState<string | null>(null);
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [showWhole, setShowWhole] = useState(!focusId);
  const [counts, setCounts] = useState({ nodes: 0, links: 0 });

  // Everything the simulation touches lives in refs: React state at 60fps would
  // re-render the whole tree sixty times a second to move some dots.
  const state = useRef<{
    nodes: Node[];
    edges: Edge[];
    byId: Map<string, number>;
    hover: number | null;
    drag: number | null;
    pan: { x: number; y: number };
    zoom: number;
    alpha: number;
  }>({ nodes: [], edges: [], byId: new Map(), hover: null, drag: null, pan: { x: 0, y: 0 }, zoom: 1, alpha: 1 });

  const hiddenRef = useRef(hidden);
  hiddenRef.current = hidden;

  // Every distinct filter a reader can toggle: "Where pages are filed" (parent),
  // "Mentions" (link) if any exist, and one entry per distinct relation label.
  // Built from the DATA rather than hard-coded, because relation labels are
  // whatever a team named their own database column.
  const filters = useMemo(() => {
    const relationLabels = new Set<string>();
    let hasLink = false;
    for (const e of linkEdges) {
      if (e.kind === 'relation') relationLabels.add(e.label || t('Relation'));
      else hasLink = true;
    }
    const list: { key: string; label: string; color: string }[] = [
      { key: 'parent', label: t('Where pages are filed'), color: '' },
    ];
    if (hasLink) list.push({ key: 'link', label: t('Mentions'), color: '' });
    [...relationLabels].sort().forEach((label, i) => {
      list.push({ key: 'relation:' + label, label, color: RELATION_HUES[i % RELATION_HUES.length] });
    });
    return list;
  }, [linkEdges]);

  const focusSet = useMemo(
    // Every edge kind counts here — a page reachable only through a relation
    // field (Feature "impacts" Metric) is exactly the kind of connection
    // "See related graph" exists to surface, not one to leave out.
    () => (focusId && !showWhole ? neighborhood(focusId, pages, linkEdges, 2) : null),
    [focusId, showWhole, pages, linkEdges],
  );

  // ---- build the graph ----
  useEffect(() => {
    const live = pages.filter((p) => !p.trashed && !p.isTemplate && (!focusSet || focusSet.has(p.id)));
    const byId = new Map<string, number>();
    const meta = new Map(live.map((p) => [p.id, p]));
    // The top-level page a dot ultimately hangs off. Guarded, because a cycle
    // in the parent chain would otherwise spin here forever — and a chain that
    // walks off the visible set (a private parent) stops at what is visible.
    const rootOf = (p: PageMeta): string => {
      let cur = p;
      for (let g = 0; g < 100; g++) {
        const parent = cur.parentId ? meta.get(cur.parentId) : undefined;
        if (!parent) return cur.id;
        cur = parent;
      }
      return cur.id;
    };
    // Colours go to the BIGGEST family first, so the house green lands on
    // whatever this instance is mostly about rather than on whichever page
    // happened to be created first — which was the welcome page.
    const size = new Map<string, number>();
    for (const p of live) {
      const r = rootOf(p);
      size.set(r, (size.get(r) ?? 0) + 1);
    }
    const rootColor = new Map<string, string>(
      [...size.entries()]
        .sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1))
        .map(([id], i): [string, string] => [id, HUES[i % HUES.length]]),
    );
    const nodes: Node[] = live.map((p, i) => {
      byId.set(p.id, i);
      const ws = rootOf(p);
      return {
        id: p.id,
        title: p.title || t('Untitled'),
        isDb: p.type === 'collection',
        ws,
        // Start on a small ring rather than dead centre: identical positions
        // give identical forces, and the whole graph would sit in one dot
        // forever. The spread is what lets it unfold.
        x: Math.cos((i / live.length) * Math.PI * 2) * 220,
        y: Math.sin((i / live.length) * Math.PI * 2) * 220,
        vx: 0,
        vy: 0,
        r: 4,
        color: rootColor.get(ws)!,
        deg: 0,
      };
    });

    const relationColor = new Map<string, string>();
    filters.forEach((f) => {
      if (f.color) relationColor.set(f.key, f.color);
    });

    const edges: Edge[] = [];
    for (const p of live) {
      if (p.parentId && byId.has(p.parentId) && byId.has(p.id)) {
        edges.push({ a: byId.get(p.id)!, b: byId.get(p.parentId)!, kind: 'parent', filterKey: 'parent', color: '' });
      }
    }
    let links = 0;
    for (const e of linkEdges) {
      const a = byId.get(e.source);
      const b = byId.get(e.target);
      if (a === undefined || b === undefined || a === b) continue;
      if (e.kind === 'relation') {
        const key = 'relation:' + (e.label || t('Relation'));
        edges.push({ a, b, kind: 'relation', filterKey: key, color: relationColor.get(key) ?? RELATION_HUES[0] });
      } else {
        edges.push({ a, b, kind: 'link', filterKey: 'link', color: '' });
        links++;
      }
    }
    for (const e of edges) {
      nodes[e.a].deg++;
      nodes[e.b].deg++;
    }
    // Size by how connected a page is — the hub of a workspace should look like
    // one. Square root, or one page with fifty children swamps the picture.
    //
    // A database gets a floor. Its rows are deliberately not in this graph (a
    // database can hold tens of thousands and the canvas would die), so it has
    // no edges to be sized by and came out as the smallest mark on screen: a
    // container holding a hundred pages looked exactly like an orphan nobody
    // linked. The floor makes it read as a container, and it clears the label
    // threshold below, so it says its own name.
    for (const n of nodes) {
      n.r = 4 + Math.sqrt(n.deg) * 2.6;
      if (n.isDb) n.r = Math.max(n.r, 8);
    }
    // The page the reader asked about should stand out and start centred —
    // otherwise a focused graph looks exactly like a random slice.
    if (focusId && byId.has(focusId)) {
      const i = byId.get(focusId)!;
      nodes[i].r = Math.max(nodes[i].r, 10);
      nodes[i].x = 0;
      nodes[i].y = 0;
    }

    state.current = { ...state.current, nodes, edges, byId, alpha: 1, pan: { x: 0, y: 0 }, zoom: 1 };
    setCounts({ nodes: nodes.length, links });
  }, [pages, linkEdges, focusSet, focusId, filters]);

  // ---- simulate and draw ----
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    let raf = 0;

    const css = getComputedStyle(document.documentElement);
    const fg = css.getPropertyValue('--fg').trim() || '#37352f';
    const muted = css.getPropertyValue('--muted').trim() || '#787774';
    const bg = css.getPropertyValue('--bg').trim() || '#ffffff';

    const resize = () => {
      const dpr = window.devicePixelRatio || 1;
      const rect = canvas.getBoundingClientRect();
      canvas.width = rect.width * dpr;
      canvas.height = rect.height * dpr;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    resize();
    window.addEventListener('resize', resize);

    const step = () => {
      const s = state.current;
      const { nodes, edges } = s;
      const rect = canvas.getBoundingClientRect();
      const cx = rect.width / 2;
      const cy = rect.height / 2;
      const hide = hiddenRef.current;

      // --- forces ---
      // Repulsion is O(n²). Fine to a few hundred nodes, which is what a
      // workspace is; beyond that it is capped rather than approximated,
      // because a wrong-looking graph is worse than a slightly lazy one.
      const n = nodes.length;
      const cap = n > 700 ? 700 : n;
      for (let i = 0; i < cap; i++) {
        const a = nodes[i];
        for (let j = i + 1; j < cap; j++) {
          const b = nodes[j];
          let dx = b.x - a.x;
          let dy = b.y - a.y;
          let d2 = dx * dx + dy * dy;
          if (d2 < 1) {
            dx = (i % 7) - 3;
            dy = (j % 7) - 3;
            d2 = 25;
          }
          if (d2 > 90000) continue; // far apart: no measurable push, skip it
          const f = REPULSION / d2;
          const d = Math.sqrt(d2);
          const fx = (dx / d) * f;
          const fy = (dy / d) * f;
          a.vx -= fx;
          a.vy -= fy;
          b.vx += fx;
          b.vy += fy;
        }
      }
      for (const e of edges) {
        if (hide.has(e.filterKey)) continue;
        const a = nodes[e.a];
        const b = nodes[e.b];
        const dx = b.x - a.x;
        const dy = b.y - a.y;
        const d = Math.hypot(dx, dy) || 1;
        const rest = e.kind === 'parent' ? PARENT_LEN : e.kind === 'relation' ? RELATION_LEN : LINK_LEN;
        const f = (d - rest) * SPRING;
        const fx = (dx / d) * f;
        const fy = (dy / d) * f;
        a.vx += fx;
        a.vy += fy;
        b.vx -= fx;
        b.vy -= fy;
      }
      for (let i = 0; i < n; i++) {
        const a = nodes[i];
        if (i === s.drag) continue;
        a.vx -= a.x * CENTER_PULL;
        a.vy -= a.y * CENTER_PULL;
        a.vx *= DAMPING;
        a.vy *= DAMPING;
        a.x += a.vx * s.alpha;
        a.y += a.vy * s.alpha;
      }
      // Cools down to a stop instead of jittering forever, and never quite to
      // zero — a graph that breathes very slightly looks alive; one frozen
      // solid looks like a screenshot.
      s.alpha = Math.max(0.12, s.alpha * 0.994);

      // --- draw ---
      ctx.clearRect(0, 0, rect.width, rect.height);
      ctx.save();
      ctx.translate(cx + s.pan.x, cy + s.pan.y);
      ctx.scale(s.zoom, s.zoom);

      const hov = s.hover;
      const near = new Set<number>();
      if (hov !== null) {
        near.add(hov);
        for (const e of edges) {
          if (e.a === hov) near.add(e.b);
          if (e.b === hov) near.add(e.a);
        }
      }

      for (const e of edges) {
        if (hide.has(e.filterKey)) continue;
        const a = nodes[e.a];
        const b = nodes[e.b];
        const lit = hov === null || near.has(e.a) || near.has(e.b);
        if (e.kind === 'link') {
          // A mention takes the colour of the page it comes FROM, so a line
          // leaving one cluster for another is visibly a crossing — which is
          // the single most interesting thing a graph can show.
          ctx.strokeStyle = a.color;
          ctx.globalAlpha = lit ? 0.8 : 0.07;
          ctx.lineWidth = lit ? 1.8 : 1;
        } else if (e.kind === 'relation') {
          ctx.strokeStyle = e.color;
          ctx.globalAlpha = lit ? 0.85 : 0.08;
          ctx.lineWidth = lit ? 2 : 1;
          ctx.setLineDash([5, 3]);
        } else {
          ctx.strokeStyle = muted;
          ctx.globalAlpha = lit ? 0.4 : 0.08;
          ctx.lineWidth = 1;
        }
        ctx.beginPath();
        ctx.moveTo(a.x, a.y);
        ctx.lineTo(b.x, b.y);
        ctx.stroke();
        ctx.setLineDash([]);
        ctx.globalAlpha = 1;
      }

      for (let i = 0; i < n; i++) {
        const a = nodes[i];
        const lit = hov === null || near.has(i);
        ctx.globalAlpha = lit ? 1 : 0.18;
        // A database gets a ring, a document a filled dot — the same
        // distinction the sidebar makes, without needing a legend.
        ctx.beginPath();
        ctx.arc(a.x, a.y, a.r, 0, Math.PI * 2);
        if (a.isDb) {
          ctx.fillStyle = bg;
          ctx.fill();
          ctx.strokeStyle = a.color;
          ctx.lineWidth = 2.4;
          ctx.stroke();
        } else {
          ctx.fillStyle = a.color;
          ctx.fill();
        }
        // The page a focused graph is ABOUT gets a ring of its own, the same
        // treatment hover gets, but permanent — otherwise a scoped graph looks
        // exactly like an arbitrary slice with no obvious center.
        if (a.id === focusId || i === hov) {
          ctx.save();
          ctx.shadowColor = a.color;
          ctx.shadowBlur = 18;
          ctx.beginPath();
          ctx.arc(a.x, a.y, a.r + 5, 0, Math.PI * 2);
          ctx.strokeStyle = a.color;
          ctx.lineWidth = 2;
          ctx.globalAlpha = 0.85;
          ctx.stroke();
          ctx.restore();
          ctx.globalAlpha = 1;
        }
        // Labels only for the big ones and for whatever is under the pointer:
        // every label at once is a wall of text with a graph behind it.
        if ((a.r > 7 || a.isDb || i === hov || a.id === focusId) && lit && s.zoom > 0.55) {
          ctx.globalAlpha = i === hov ? 1 : 0.75;
          ctx.fillStyle = fg;
          ctx.font = `${i === hov || a.id === focusId ? 600 : 400} 11px system-ui, sans-serif`;
          ctx.textAlign = 'center';
          const label = a.title.length > 26 ? a.title.slice(0, 25) + '…' : a.title;
          ctx.fillText(label, a.x, a.y + a.r + 12);
        }
        ctx.globalAlpha = 1;
      }
      ctx.restore();
      raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener('resize', resize);
    };
  }, [focusId]);

  // ---- pointer ----
  const toWorld = (ev: React.MouseEvent) => {
    const canvas = canvasRef.current!;
    const rect = canvas.getBoundingClientRect();
    const s = state.current;
    return {
      x: (ev.clientX - rect.left - rect.width / 2 - s.pan.x) / s.zoom,
      y: (ev.clientY - rect.top - rect.height / 2 - s.pan.y) / s.zoom,
    };
  };

  const pick = (wx: number, wy: number): number | null => {
    const { nodes } = state.current;
    let best: number | null = null;
    let bestD = Infinity;
    for (let i = 0; i < nodes.length; i++) {
      const d = Math.hypot(nodes[i].x - wx, nodes[i].y - wy);
      if (d < nodes[i].r + 7 && d < bestD) {
        best = i;
        bestD = d;
      }
    }
    return best;
  };

  const panFrom = useRef<{ x: number; y: number } | null>(null);
  // A click arrives after every mouse-up, whether or not the pointer moved, so
  // dragging a dot to see what comes with it ended by opening the page you were
  // only trying to move — the one interaction this view exists for, punished
  // with a navigation. These remember whether the pointer travelled far enough
  // to count as a drag.
  const downAt = useRef<{ x: number; y: number } | null>(null);
  const moved = useRef(false);

  const toggleFilter = (key: string) => {
    setHidden((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
    state.current.alpha = 1;
  };

  const exportImage = () => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const url = canvas.toDataURL('image/png');
    const a = document.createElement('a');
    a.href = url;
    a.download = 'graph.png';
    a.click();
  };

  return (
    <div className="graph-wrap">
      <canvas
        ref={canvasRef}
        className="graph-canvas"
        onMouseDown={(e) => {
          moved.current = false;
          downAt.current = { x: e.clientX, y: e.clientY };
          const { x, y } = toWorld(e);
          const hit = pick(x, y);
          if (hit !== null) state.current.drag = hit;
          else panFrom.current = { x: e.clientX - state.current.pan.x, y: e.clientY - state.current.pan.y };
        }}
        onMouseMove={(e) => {
          const s = state.current;
          if (downAt.current && !moved.current) {
            // Four pixels: a shaky hand on a trackpad still counts as a click.
            const dx = e.clientX - downAt.current.x;
            const dy = e.clientY - downAt.current.y;
            if (dx * dx + dy * dy > 16) moved.current = true;
          }
          const { x, y } = toWorld(e);
          if (s.drag !== null) {
            // Dragging a node stirs the whole graph back to life, which is the
            // point: you pull one page out and watch what comes with it.
            s.nodes[s.drag].x = x;
            s.nodes[s.drag].y = y;
            s.nodes[s.drag].vx = 0;
            s.nodes[s.drag].vy = 0;
            s.alpha = Math.max(s.alpha, 0.7);
            return;
          }
          if (panFrom.current) {
            s.pan = { x: e.clientX - panFrom.current.x, y: e.clientY - panFrom.current.y };
            return;
          }
          const hit = pick(x, y);
          if (hit !== s.hover) {
            s.hover = hit;
            setHoverTitle(hit === null ? null : s.nodes[hit].title);
          }
        }}
        onMouseUp={() => {
          state.current.drag = null;
          panFrom.current = null;
        }}
        onMouseLeave={() => {
          state.current.drag = null;
          state.current.hover = null;
          panFrom.current = null;
          setHoverTitle(null);
        }}
        onClick={(e) => {
          const wasDrag = moved.current;
          downAt.current = null;
          moved.current = false;
          if (wasDrag) return; // that was a drag, not a click
          const { x, y } = toWorld(e);
          const hit = pick(x, y);
          if (hit !== null) onNavigate(state.current.nodes[hit].id);
        }}
        onWheel={(e) => {
          const s = state.current;
          s.zoom = Math.min(3, Math.max(0.25, s.zoom * (e.deltaY > 0 ? 0.92 : 1.08)));
        }}
      />
      <div className="graph-bar">
        <span className="graph-count">
          {plural(counts.nodes, '{n} page', '{n} pages')} ·{' '}
          {plural(counts.links, '{n} link', '{n} links')}
        </span>
        {focusId && (
          <label className="graph-toggle">
            <input type="checkbox" checked={showWhole} onChange={(e) => setShowWhole(e.target.checked)} />
            {t('Show the whole workspace')}
          </label>
        )}
        {filters.map((f) => (
          <label className="graph-toggle" key={f.key}>
            <input
              type="checkbox"
              checked={!hidden.has(f.key)}
              onChange={() => toggleFilter(f.key)}
            />
            {f.color && <span className="graph-filter-swatch" style={{ background: f.color }} />}
            {f.label}
          </label>
        ))}
        <button className="btn-sm graph-export" onClick={exportImage} title={t('Save as image')}>
          <Download size={13} /> {t('Export')}
        </button>
        <span className="graph-hint">{t('Drag a dot, scroll to zoom, click to open')}</span>
      </div>
      {hoverTitle && <div className="graph-tip">{hoverTitle}</div>}
    </div>
  );
}
