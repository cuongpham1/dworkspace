import { useEffect, useMemo, useRef, useState } from 'react';
import { Check, FileText, Plus, Save, Search, X } from 'lucide-react';
import { api } from '../api';
import { formatMoment } from '../format';
import { t } from '../i18n';
import { useExclusiveModal } from '../modal';
import { toast } from '../toast';
import type { PageChangeProposal } from '../types';
import Portal from './Portal';

type UnknownRecord = Record<string, unknown>;

const isRecord = (value: unknown): value is UnknownRecord => typeof value === 'object' && value !== null;
const blockText = (value: unknown): string => {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(blockText).filter(Boolean).join('');
  if (!isRecord(value)) return '';
  if (typeof value.text === 'string') return value.text;
  return blockText(value.content) || blockText(value.props) || '';
};
const blockType = (value: unknown): string => isRecord(value) && typeof value.type === 'string' ? value.type : 'block';
const blocks = (value: unknown[]): Array<{ key: string; text: string; type: string }> => value.map((block, index) => {
  const id = isRecord(block) && typeof block.id === 'string' ? block.id : String(index);
  return { key: id, text: blockText(block), type: blockType(block) };
});
const textLeaves = (value: unknown): string[] => {
  if (Array.isArray(value)) return value.flatMap(textLeaves);
  if (!isRecord(value)) return [];
  if (value.type === 'text' && typeof value.text === 'string') return [value.text];
  return textLeaves(value.content);
};
function replaceTextLeaf(value: unknown, target: number, nextText: string, state = { index: 0 }): unknown {
  if (Array.isArray(value)) return value.map((item) => replaceTextLeaf(item, target, nextText, state));
  if (!isRecord(value)) return value;
  if (value.type === 'text' && typeof value.text === 'string') {
    const match = state.index === target;
    state.index += 1;
    return match ? { ...value, text: nextText } : value;
  }
  return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, replaceTextLeaf(child, target, nextText, state)]));
}
const updateBlockText = (value: unknown[], index: number, leafIndex: number, nextText: string): unknown[] => value.map((block, blockIndex) => blockIndex === index ? replaceTextLeaf(block, leafIndex, nextText) : block);
const hasTextLeaf = (value: unknown): boolean => {
  if (Array.isArray(value)) return value.some(hasTextLeaf);
  return isRecord(value) && ((value.type === 'text' && typeof value.text === 'string') || hasTextLeaf(value.content));
};

function statusLabel(status: PageChangeProposal['status'] | 'added' | 'removed' | 'changed'): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

// Lets the page header's pending-changes count refresh itself without a full
// reload — the same trick CommentsPanel's COMMENTS_CHANGED plays for the
// comment badge: the header owns the count, this component only nudges it.
export const PROPOSALS_CHANGED = 'dworkspace:proposals-changed';

const SIDEBAR_WIDTH_KEY = 'dworkspace-proposal-sidebar-width';
const SIDEBAR_WIDTH_MIN = 200;
const SIDEBAR_WIDTH_MAX = 440;
const SIDEBAR_WIDTH_DEFAULT = 264;

function loadSidebarWidth(): number {
  try {
    const stored = Number(localStorage.getItem(SIDEBAR_WIDTH_KEY));
    if (Number.isFinite(stored) && stored >= SIDEBAR_WIDTH_MIN && stored <= SIDEBAR_WIDTH_MAX) return stored;
  } catch {
    /* private mode, quota — the default is fine */
  }
  return SIDEBAR_WIDTH_DEFAULT;
}

// Generic LCS-based diff: works on any array (blocks, or word tokens) given an
// equality test. Reused for both the block-level alignment and the word-level
// diff inside one modified block — same algorithm, two granularities.
type DiffOp<T> = { op: 'same' | 'del' | 'add'; item: T };
function lcsDiff<T>(a: T[], b: T[], eq: (x: T, y: T) => boolean): DiffOp<T>[] {
  const n = a.length;
  const m = b.length;
  const dp: number[][] = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = eq(a[i], b[j]) ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const ops: DiffOp<T>[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (eq(a[i], b[j])) { ops.push({ op: 'same', item: b[j] }); i += 1; j += 1; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { ops.push({ op: 'del', item: a[i] }); i += 1; }
    else { ops.push({ op: 'add', item: b[j] }); j += 1; }
  }
  while (i < n) { ops.push({ op: 'del', item: a[i] }); i += 1; }
  while (j < m) { ops.push({ op: 'add', item: b[j] }); j += 1; }
  return ops;
}

// Splits on whitespace while keeping the whitespace itself as tokens, so the
// diffed pieces rejoin into exactly the original text with no lost spacing.
const wordTokens = (text: string): string[] => text.split(/(\s+)/).filter((token) => token.length > 0);

function renderWordDiff(oldText: string, newText: string) {
  const ops = lcsDiff(wordTokens(oldText), wordTokens(newText), (a, b) => a === b);
  return ops.map((op, index) => {
    if (op.op === 'same') return <span key={index}>{op.item}</span>;
    if (op.op === 'del') return <del key={index} className="diff-del">{op.item}</del>;
    return <ins key={index} className="diff-ins">{op.item}</ins>;
  });
}

type BlockRef = { key: string; text: string; type: string };
type DiffSegment =
  | { kind: 'same'; block: BlockRef }
  | { kind: 'modified'; before: BlockRef; after: BlockRef }
  | { kind: 'added'; block: BlockRef }
  | { kind: 'removed'; block: BlockRef };

// Aligns two block lists by CONTENT (type+text), not by block id — a full
// `write_content(mode=replace)` regenerates every block id from scratch, so id
// matching would show the entire document as removed+added even when only one
// sentence changed. Within one contiguous run of removals+insertions, blocks of
// the same type are paired as "modified" so their text gets a word-level diff
// instead of two opaque red/green blobs.
function alignBlocks(before: BlockRef[], after: BlockRef[]): DiffSegment[] {
  const ops = lcsDiff(before, after, (a, b) => a.type === b.type && a.text === b.text);
  const segments: DiffSegment[] = [];
  let i = 0;
  while (i < ops.length) {
    const op = ops[i];
    if (op.op === 'same') { segments.push({ kind: 'same', block: op.item }); i += 1; continue; }
    const dels: BlockRef[] = [];
    const adds: BlockRef[] = [];
    while (i < ops.length && ops[i].op === 'del') { dels.push(ops[i].item); i += 1; }
    while (i < ops.length && ops[i].op === 'add') { adds.push(ops[i].item); i += 1; }
    const usedAdds = new Set<number>();
    for (const del of dels) {
      const matchIndex = adds.findIndex((candidate, ai) => !usedAdds.has(ai) && candidate.type === del.type);
      if (matchIndex >= 0) {
        usedAdds.add(matchIndex);
        segments.push({ kind: 'modified', before: del, after: adds[matchIndex] });
      } else {
        segments.push({ kind: 'removed', block: del });
      }
    }
    adds.forEach((add, ai) => { if (!usedAdds.has(ai)) segments.push({ kind: 'added', block: add }); });
  }
  return segments;
}

function Preview({ proposal }: { proposal: PageChangeProposal }) {
  return <div className="proposal-preview" aria-label={t('Proposed document preview')}>
    {proposal.proposedTitle && <h3 className="proposal-preview-title">{proposal.proposedTitle}</h3>}
    {blocks(proposal.proposedContent).map((block) => <div className={`proposal-block proposal-block-${block.type}`} key={block.key}>{block.text || <span className="proposal-empty-block">{t('Empty block')}</span>}</div>)}
    {proposal.proposedContent.length === 0 && <p className="dialog-hint">{t('The proposed document is empty.')}</p>}
  </div>;
}

function Diff({ proposal, canonicalContent, canonicalTitle }: { proposal: PageChangeProposal; canonicalContent: unknown[]; canonicalTitle: string }) {
  const current = useMemo(() => blocks(canonicalContent), [canonicalContent]);
  const proposed = useMemo(() => blocks(proposal.proposedContent), [proposal.proposedContent]);
  const segments = useMemo(() => alignBlocks(current, proposed), [current, proposed]);
  const titleChanged = canonicalTitle !== proposal.proposedTitle;
  const noChanges = !titleChanged && segments.every((segment) => segment.kind === 'same');
  return <div className="proposal-diff proposal-diff-flow" aria-label={t('Changes only')}>
    {titleChanged && <h3 className="proposal-diff-title">{renderWordDiff(canonicalTitle || t('Untitled'), proposal.proposedTitle || t('Untitled'))}</h3>}
    {segments.map((segment, index) => {
      if (segment.kind === 'same') return <div className={`proposal-block proposal-block-${segment.block.type}`} key={`s-${index}`}>{segment.block.text || <span className="proposal-empty-block">{t('Empty block')}</span>}</div>;
      if (segment.kind === 'modified') return <div className={`proposal-block proposal-block-${segment.after.type} proposal-block-modified`} key={`m-${index}`}>{renderWordDiff(segment.before.text, segment.after.text)}</div>;
      if (segment.kind === 'added') return <div className={`proposal-block proposal-block-${segment.block.type} proposal-block-added`} key={`a-${index}`}><ins className="diff-ins">{segment.block.text || t('Empty block')}</ins></div>;
      return <div className={`proposal-block proposal-block-${segment.block.type} proposal-block-removed`} key={`r-${index}`}><del className="diff-del">{segment.block.text || t('Empty block')}</del></div>;
    })}
    {noChanges && <p className="dialog-hint">{t('No block-level changes detected.')}</p>}
  </div>;
}

function ReviewPane({ proposal, onChange, canEdit }: { proposal: PageChangeProposal; onChange: (next: PageChangeProposal) => void; canEdit: boolean }) {
  const review = proposal.factReview;
  const [query, setQuery] = useState('');
  const [matches, setMatches] = useState<PageChangeProposal['relatedCandidates']>([]);
  const [searching, setSearching] = useState(false);
  const updateFact = (factId: string, value: string) => {
    const gap = review.gaps.find((item) => item.id === factId);
    const existing = review.facts.some((fact) => fact.id === factId);
    const facts = existing
      ? review.facts.map((fact) => fact.id === factId ? { ...fact, value, category: 'provided' } : fact)
      : [...review.facts, { id: factId, constraintId: gap?.constraintId ?? factId, label: gap?.label ?? factId, value, category: 'provided' }];
    onChange({ ...proposal, factReview: { ...review, facts } });
  };
  const confirmFacts = () => onChange({ ...proposal, factReview: { ...review, trusted: true, facts: review.facts.map((fact) => ({ ...fact, category: fact.value.trim() ? 'provided' : fact.category, confirmed: fact.value.trim() ? true : fact.confirmed })) } });
  const selected = new Set(proposal.selectedRelatedIds);
  const search = async () => {
    if (!query.trim()) return;
    setSearching(true);
    try { setMatches(await api.searchProposalRelated(proposal.id, query)); } catch (error) { toast(error instanceof Error ? error.message : t('Related documents could not be searched')); } finally { setSearching(false); }
  };
  const addCandidate = (candidate: PageChangeProposal['relatedCandidates'][number]) => {
    if (!proposal.relatedCandidates.some((item) => item.pageId === candidate.pageId)) onChange({ ...proposal, relatedCandidates: [...proposal.relatedCandidates, candidate] });
  };
  const dismiss = (pageId: string) => onChange({ ...proposal, selectedRelatedIds: [...selected].filter((id) => id !== pageId), relatedCandidates: proposal.relatedCandidates.map((item) => item.pageId === pageId ? { ...item, selected: false, dismissed: true } : item) });
  return <aside className="proposal-knowledge" aria-label={t('Knowledge review')}>
    <div className="proposal-pane-heading"><span>{t('Knowledge review')}</span><span className={`proposal-validation proposal-validation-${review.validation}`}>{review.validation === 'unknown' ? t('UNKNOWN') : t('Validated')}</span></div>
    <p className="dialog-hint">{review.validation === 'unknown' ? t('No machine-readable constraint was supplied; completeness is not validated.') : t('Only deterministic constraints are counted. Candidates are never facts.')}</p>
    {review.constraints.length > 0 && <div className="proposal-fact-summary">{review.validation === 'deterministic' ? <><strong>{review.provided}/{review.required}</strong> {t('required facts provided')}</> : <>{t('Agent constraint claim')}: {review.constraints.map((constraint) => `${constraint.label} ${constraint.required}`).join(', ')}</>}</div>}
    {review.facts.length > 0 && <div className="proposal-facts"><h3>{t('Fact provenance')}</h3>{review.facts.map((fact) => <label className="proposal-fact" key={fact.id}><span><strong>{fact.label || fact.id}</strong><small>{fact.category}{fact.claimedCategory ? ` · ${t('Agent claimed')} ${fact.claimedCategory}` : ''}{fact.source ? ` · ${fact.source}` : ''}{fact.evidence ? ` · ${fact.evidence}` : ''}</small></span>{canEdit && review.validation === 'unknown' ? <input value={fact.value} onChange={(event) => updateFact(fact.id ?? '', event.target.value)} /> : <em>{fact.value || t('Missing fact')}</em>}</label>)}</div>}
    {review.validation === 'unknown' && canEdit && (review.facts.length > 0 || review.constraints.length > 0) && <button className="btn" onClick={confirmFacts}>{t('Confirm facts and validate')}</button>}
    {review.gaps.map((gap) => { const fact = review.facts.find((item) => item.id === gap.id); return <label className="proposal-fact-gap" key={gap.id}><span>{gap.label}</span><input value={fact?.value ?? ''} placeholder={t('Missing fact')} disabled={!canEdit} onChange={(event) => updateFact(gap.id, event.target.value)} /><small>{gap.message}</small></label>; })}
    {review.gaps.length === 0 && review.validation === 'deterministic' && <p className="dialog-hint">{t('No deterministic fact gaps.')}</p>}
    <div className="proposal-related"><h3>{t('Related document candidates')}</h3><p className="dialog-hint">{t('Suggestions only. Selecting one creates a link on Publish; it does not fill a fact gap.')}</p>{proposal.relatedCandidates.filter((candidate) => !candidate.dismissed).map((candidate) => <label className="proposal-candidate" key={candidate.pageId}><input type="checkbox" disabled={!canEdit} checked={selected.has(candidate.pageId)} onChange={(event) => onChange({ ...proposal, selectedRelatedIds: event.target.checked ? [...selected, candidate.pageId] : [...selected].filter((id) => id !== candidate.pageId), relatedCandidates: proposal.relatedCandidates.map((item) => item.pageId === candidate.pageId ? { ...item, selected: event.target.checked } : item) })} /><span><strong><span className="proposal-rank">#{candidate.rank}</span> {candidate.title}</strong><small>{candidate.rationale}{candidate.snippet ? ` · ${candidate.snippet}` : ''}</small></span>{canEdit && <button className="icon-btn" type="button" aria-label={t('Dismiss candidate')} onClick={(event) => { event.preventDefault(); dismiss(candidate.pageId); }}><X size={14} /></button>}</label>)}{canEdit && <div className="proposal-candidate-search"><input value={query} placeholder={t('Search existing documents')} onChange={(event) => setQuery(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') void search(); }} /><button className="btn" disabled={searching} onClick={() => void search()}><Search size={14} /> {t('Search')}</button>{matches.map((candidate) => <button className="proposal-candidate-result" key={candidate.pageId} onClick={() => addCandidate(candidate)}><Plus size={13} /> #{candidate.rank} {candidate.title}</button>)}</div>}</div>
    {proposal.kind === 'create' && <div className="proposal-destination"><h3>{t('Destination')}</h3><p>{t('Workspace')}: <code>{proposal.targetWorkspaceId || t('Unknown')}</code></p><p>{t('Parent')}: <code>{proposal.targetParentId || t('Workspace root')}</code></p></div>}<div className="proposal-metadata"><h3>{t('Metadata')}</h3><label>{t('Icon')}<input value={proposal.proposedIcon} disabled={!canEdit} onChange={(event) => onChange({ ...proposal, proposedIcon: event.target.value })} /></label><label>{t('Cover')}<input value={proposal.proposedCover} disabled={!canEdit} onChange={(event) => onChange({ ...proposal, proposedCover: event.target.value })} /></label><label>{t('Description')}<input value={proposal.proposedDescription} disabled={!canEdit} onChange={(event) => onChange({ ...proposal, proposedDescription: event.target.value })} /></label><label>{t('Tags')}<input value={proposal.proposedTags.join(', ')} disabled={!canEdit} onChange={(event) => onChange({ ...proposal, proposedTags: event.target.value.split(',').map((tag) => tag.trim()).filter(Boolean) })} /></label></div>
    <div className="proposal-provenance"><h3>{t('Provenance')}</h3><p>{t('Original proposal snapshot retained')}</p>{proposal.lastHumanEditor && <p>{t('Last human editor')}: {proposal.lastHumanEditor}</p>}{proposal.publishedBy && <p>{t('Publisher')}: {proposal.publishedBy}</p>}</div>
  </aside>;
}

function StructuredEditor({ proposal, onChange }: { proposal: PageChangeProposal; onChange: (next: PageChangeProposal) => void }) {
  return <div className="proposal-structured-editor" aria-label={t('Structured document editor')}>
    {proposal.proposedContent.map((block, index) => { const detail = blocks([block])[0]; const leaves = textLeaves(block); return <div className="proposal-edit-block" data-block-type={detail.type} key={detail.key}><span className="proposal-block-type">{detail.type}</span>{hasTextLeaf(block) ? leaves.map((text, leafIndex) => <textarea key={`${detail.key}-${leafIndex}`} value={text} onChange={(event) => onChange({ ...proposal, proposedContent: updateBlockText(proposal.proposedContent, index, leafIndex, event.target.value) })} />) : <span className="proposal-preserved-block">{t('Non-text block preserved')}</span>}</div>; })}
    {proposal.proposedContent.length === 0 && <p className="dialog-hint">{t('The proposed document is empty.')}</p>}
  </div>;
}

export default function ProposalReviewModal({ pageId, initialProposalId, canonicalContent, canonicalTitle, canEdit, onClose, onPublished }: { pageId?: string; initialProposalId?: string; canonicalContent: unknown[]; canonicalTitle: string; canEdit: boolean; onClose: () => void; onPublished: () => void }) {
  const [proposals, setProposals] = useState<PageChangeProposal[]>([]);
  const [selectedId, setSelectedId] = useState(initialProposalId ?? '');
  const [showEditor, setShowEditor] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loadedCanonical, setLoadedCanonical] = useState({ content: canonicalContent, title: canonicalTitle });
  const [sidebarWidth, setSidebarWidth] = useState(loadSidebarWidth);
  const resizeStart = useRef<{ pointerX: number; width: number } | null>(null);
  const onSidebarResizeStart = (event: React.PointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture(event.pointerId);
    resizeStart.current = { pointerX: event.clientX, width: sidebarWidth };
  };
  const onSidebarResizeMove = (event: React.PointerEvent<HTMLDivElement>) => {
    if (!resizeStart.current) return;
    const next = resizeStart.current.width + (event.clientX - resizeStart.current.pointerX);
    setSidebarWidth(Math.min(SIDEBAR_WIDTH_MAX, Math.max(SIDEBAR_WIDTH_MIN, next)));
  };
  const onSidebarResizeEnd = () => { resizeStart.current = null; };
  useEffect(() => {
    try { localStorage.setItem(SIDEBAR_WIDTH_KEY, String(sidebarWidth)); } catch { /* private mode, quota — nothing to persist to */ }
  }, [sidebarWidth]);
  useExclusiveModal(onClose);
  useEffect(() => {
    const request = pageId ? api.listProposals(pageId) : api.listAllProposals();
    void request.then((items) => { setProposals(items); setSelectedId((current) => current && items.some((item) => item.id === current) ? current : items[0]?.id ?? ''); }).catch(() => toast(t('Proposals could not be loaded')));
  }, [pageId]);
  const selected = proposals.find((proposal) => proposal.id === selectedId) ?? null;
  useEffect(() => {
    let active = true;
    if (!selected || selected.kind === 'create' || !selected.pageId || pageId) { setLoadedCanonical({ content: canonicalContent, title: canonicalTitle }); return () => { active = false; }; }
    void api.getPage(selected.pageId).then((page) => { if (active) setLoadedCanonical({ content: page.content, title: page.title }); }).catch(() => { if (active) setLoadedCanonical({ content: [], title: '' }); });
    return () => { active = false; };
  }, [canonicalContent, canonicalTitle, pageId, selected?.id, selected?.kind, selected?.pageId]);
  const canonicalForSelected = selected?.kind === 'create' ? [] : loadedCanonical.content;
  const titleForSelected = selected?.kind === 'create' ? '' : loadedCanonical.title;
  const mayEdit = canEdit && selected?.canEdit !== false;
  const showDiff = selected?.kind !== 'create';
  const updateSelected = (next: PageChangeProposal) => setProposals((items) => items.map((item) => item.id === next.id ? next : item));
  const persist = async (proposal: PageChangeProposal, notify: boolean) => {
    const updated = await api.updateProposal(proposal.id, { proposedTitle: proposal.proposedTitle, content: proposal.proposedContent, proposedIcon: proposal.proposedIcon, proposedCover: proposal.proposedCover, proposedDescription: proposal.proposedDescription, proposedTags: proposal.proposedTags, factReview: proposal.factReview, relatedCandidates: proposal.relatedCandidates, selectedRelatedIds: proposal.selectedRelatedIds, updatedAt: proposal.updatedAt });
    updateSelected(updated);
    if (notify) toast(t('Proposal changes saved'));
    return updated;
  };
  const save = async () => {
    if (!selected) return;
    setBusy(true);
    try { await persist(selected, true); } catch (error) { toast(error instanceof Error ? error.message : t('Proposal changes could not be saved')); } finally { setBusy(false); }
  };
  const act = async (action: 'publish' | 'reject') => {
    if (!selected || selected.status !== 'pending') return;
    setBusy(true);
    try {
      const current = action === 'publish' ? await persist(selected, false) : selected;
      const updated = pageId ? (action === 'publish' ? await api.publishProposal(pageId, current.id) : await api.rejectProposal(pageId, current.id)) : (action === 'publish' ? await api.publishProposalById(current.id) : await api.rejectProposalById(current.id));
      updateSelected(updated);
      window.dispatchEvent(new Event(PROPOSALS_CHANGED));
      toast(action === 'publish' ? t(selected.kind === 'create' ? 'Document published' : 'Proposed revision published') : t('Proposed revision rejected'));
      if (action === 'publish') onPublished();
    } catch (error) { toast(error instanceof Error ? error.message : t('Proposal action failed')); } finally { setBusy(false); }
  };
  return <Portal><div className="modal-overlay" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><div className="dialog proposal-dialog" role="dialog" aria-modal="true" aria-label={t('Human review workspace')}><div className="proposal-dialog-head"><div><h2>{t('Human review workspace')}</h2><p className="dialog-hint">{t('Edit proposal state only. Canonical content changes only after human Publish.')}</p></div><button className="icon-btn" onClick={onClose} aria-label={t('Close')}><X size={18} /></button></div><div className="proposal-layout" style={{ ['--proposal-sidebar-width' as string]: `${sidebarWidth}px` }}><aside className="proposal-sidebar" aria-label={t('Proposal list')}>{proposals.map((proposal) => <button className={`proposal-list-item${proposal.id === selectedId ? ' selected' : ''}`} key={proposal.id} onClick={() => setSelectedId(proposal.id)}><span className="proposal-list-icon"><FileText size={15} /></span><span className="proposal-list-copy"><strong>{proposal.kind === 'create' ? proposal.proposedTitle : (proposal.summary || t('Untitled proposed revision'))}</strong><small>{proposal.kind === 'create' ? t('New document') : t('Edit proposal')} · {proposal.creatorName || t('Unknown creator')} · {formatMoment(proposal.createdAt, 'full')}</small></span><span className={`proposal-status proposal-status-${proposal.status}`}>{statusLabel(proposal.status)}</span></button>)}{proposals.length === 0 && <p className="dialog-hint">{t('No proposals yet.')}</p>}</aside><div className="proposal-resize-handle" role="separator" aria-orientation="vertical" aria-label={t('Resize proposal list')} onPointerDown={onSidebarResizeStart} onPointerMove={onSidebarResizeMove} onPointerUp={onSidebarResizeEnd} onPointerCancel={onSidebarResizeEnd} /><section className="proposal-main">{selected ? <><div className="proposal-meta"><div><span className={`proposal-status proposal-status-${selected.status}`}>{statusLabel(selected.status)}</span><span className="proposal-creator">{selected.kind === 'create' ? t('New document · Will be created on Publish') : t('Edit proposal · stale protection active')} · {selected.creatorType} · {selected.creatorName || t('Unknown creator')}</span></div>{selected.summary && <p>{selected.summary}</p>}</div><div className={`proposal-workspace${showDiff ? ' proposal-workspace-full' : ''}`}><div className="proposal-editor-column">{mayEdit && selected.status === 'pending' && !showEditor && <button className="btn proposal-editor-toggle" onClick={() => setShowEditor(true)}>{t('Edit before publishing')}</button>}{mayEdit && selected.status === 'pending' && showEditor && <div className="proposal-editor-fields"><div className="proposal-editor-fields-head"><label>{t('Title')}<input value={selected.proposedTitle} onChange={(event) => updateSelected({ ...selected, proposedTitle: event.target.value })} /></label><button className="icon-btn" onClick={() => setShowEditor(false)} aria-label={t('Close')}><X size={15} /></button></div><label>{t('Document body')}</label><StructuredEditor proposal={selected} onChange={updateSelected} /><button className="btn" disabled={busy} onClick={() => void save()}><Save size={15} /> {t('Save edits')}</button></div>}{showDiff ? <Diff proposal={selected} canonicalContent={canonicalForSelected} canonicalTitle={titleForSelected} /> : <Preview proposal={selected} />}</div>{!showDiff && <ReviewPane proposal={selected} onChange={updateSelected} canEdit={mayEdit} />}</div>{mayEdit && selected.status === 'pending' && <div className="dialog-actions proposal-actions"><button className="btn danger" disabled={busy} onClick={() => void act('reject')}><X size={15} /> {t('Reject')}</button><button className="btn primary" disabled={busy} onClick={() => void act('publish')}><Check size={15} /> {t(selected.kind === 'create' ? 'Publish document' : 'Publish revision')}</button></div>}</> : <p className="dialog-hint">{t('Select a proposal to review it.')}</p>}</section></div></div></div></Portal>;
}
