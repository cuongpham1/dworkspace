import { useEffect, useMemo, useState } from 'react';
import { Check, FileText, GitCompare, Save, X } from 'lucide-react';
import { api } from '../api';
import { formatMoment } from '../format';
import { t } from '../i18n';
import { useExclusiveModal } from '../modal';
import { toast } from '../toast';
import type { PageChangeProposal } from '../types';
import Portal from './Portal';

type ReviewTab = 'edit' | 'preview' | 'diff';
type UnknownRecord = Record<string, unknown>;

const isRecord = (value: unknown): value is UnknownRecord => typeof value === 'object' && value !== null;
const blockText = (value: unknown): string => {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(blockText).filter(Boolean).join('');
  if (!isRecord(value)) return '';
  return blockText(value.content) || blockText(value.props) || '';
};
const blockType = (value: unknown): string => isRecord(value) && typeof value.type === 'string' ? value.type : 'block';
const blocks = (value: unknown[]): Array<{ key: string; text: string; type: string }> => value.map((block, index) => {
  const id = isRecord(block) && typeof block.id === 'string' ? block.id : String(index);
  return { key: id, text: blockText(block), type: blockType(block) };
});
const markdownFromBlocks = (value: unknown[]): string => blocks(value).map((block) => {
  if (block.type.startsWith('heading')) return `# ${block.text}`;
  if (block.type === 'bulletListItem') return `- ${block.text}`;
  if (block.type === 'numberedListItem') return `1. ${block.text}`;
  return block.text;
}).join('\n');

function statusLabel(status: PageChangeProposal['status'] | 'added' | 'removed' | 'changed'): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
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
  const changes = useMemo(() => {
    const before = new Map(current.map((block) => [block.key, block]));
    const after = new Map(proposed.map((block) => [block.key, block]));
    const rows: Array<{ kind: 'added' | 'removed' | 'changed'; text: string; type: string; key: string }> = [];
    proposed.forEach((block) => {
      const old = before.get(block.key);
      if (!old) rows.push({ kind: 'added', text: block.text, type: block.type, key: `a-${block.key}` });
      else if (old.text !== block.text || old.type !== block.type) rows.push({ kind: 'changed', text: block.text, type: block.type, key: `c-${block.key}` });
    });
    current.forEach((block) => { if (!after.has(block.key)) rows.push({ kind: 'removed', text: block.text, type: block.type, key: `r-${block.key}` }); });
    return rows;
  }, [current, proposed]);
  const titleChanged = canonicalTitle !== proposal.proposedTitle;
  return <div className="proposal-diff" aria-label={t('Changes only')}>
    {titleChanged && <div className="proposal-diff-row changed"><span className="proposal-diff-kind">{t('Title')}</span><div><del>{canonicalTitle || t('Untitled')}</del><strong>{proposal.proposedTitle || t('Untitled')}</strong></div></div>}
    {changes.map((change) => <div className={`proposal-diff-row ${change.kind}`} key={change.key}><span className="proposal-diff-kind">{statusLabel(change.kind)}</span><div><span className="proposal-diff-type">{change.type}</span> {change.text || t('Empty block')}</div></div>)}
    {!titleChanged && changes.length === 0 && <p className="dialog-hint">{t('No block-level changes detected.')}</p>}
  </div>;
}

function ReviewPane({ proposal, onChange }: { proposal: PageChangeProposal; onChange: (next: PageChangeProposal) => void }) {
  const review = proposal.factReview;
  const updateFact = (gapId: string, value: string) => {
    const gap = review.gaps.find((item) => item.id === gapId);
    const existing = review.facts.find((fact) => fact.id === gapId);
    const facts = existing ? review.facts.map((fact) => fact === existing ? { ...fact, value, category: 'provided' } : fact) : [...review.facts, { id: gapId, constraintId: gap?.constraintId ?? gapId, label: gap?.label ?? gapId, value, category: 'provided' }];
    onChange({ ...proposal, factReview: { ...review, facts } });
  };
  const selected = new Set(proposal.selectedRelatedIds);
  return <aside className="proposal-knowledge" aria-label={t('Knowledge review')}>
    <div className="proposal-pane-heading"><span>{t('Knowledge review')}</span><span className={`proposal-validation proposal-validation-${review.validation}`}>{review.validation === 'unknown' ? t('UNKNOWN') : t('Validated')}</span></div>
    <p className="dialog-hint">{review.validation === 'unknown' ? t('No machine-readable constraint was supplied; completeness is not validated.') : t('Only deterministic constraints are counted. Candidates are never facts.')}</p>
    {review.constraints.length > 0 && <div className="proposal-fact-summary"><strong>{review.provided}/{review.required}</strong> {t('required facts provided')}</div>}
    {review.gaps.map((gap) => { const fact = review.facts.find((item) => item.id === gap.id || item.constraintId === gap.constraintId); return <label className="proposal-fact-gap" key={gap.id}><span>{gap.label}</span><input value={fact?.value ?? ''} placeholder={t('Missing fact')} onChange={(event) => updateFact(gap.id, event.target.value)} /><small>{gap.message}</small></label>; })}
    {review.gaps.length === 0 && review.constraints.length > 0 && <p className="dialog-hint">{t('No deterministic fact gaps.')}</p>}
    <div className="proposal-related"><h3>{t('Related document candidates')}</h3><p className="dialog-hint">{t('Suggestions only. Selecting one creates a link on Publish; it does not fill a fact gap.')}</p>{proposal.relatedCandidates.filter((candidate) => !candidate.dismissed).map((candidate) => <label className="proposal-candidate" key={candidate.pageId}><input type="checkbox" checked={selected.has(candidate.pageId)} onChange={(event) => onChange({ ...proposal, selectedRelatedIds: event.target.checked ? [...selected, candidate.pageId] : [...selected].filter((id) => id !== candidate.pageId), relatedCandidates: proposal.relatedCandidates.map((item) => item.pageId === candidate.pageId ? { ...item, selected: event.target.checked } : item) })} /><span><strong>{candidate.title}</strong><small>{candidate.rationale}{candidate.snippet ? ` · ${candidate.snippet}` : ''}</small></span></label>)}</div>
    <div className="proposal-metadata"><h3>{t('Metadata')}</h3><label>{t('Description')}<input value={proposal.proposedDescription} onChange={(event) => onChange({ ...proposal, proposedDescription: event.target.value })} /></label><label>{t('Tags')}<input value={proposal.proposedTags.join(', ')} onChange={(event) => onChange({ ...proposal, proposedTags: event.target.value.split(',').map((tag) => tag.trim()).filter(Boolean) })} /></label></div>
  </aside>;
}

export default function ProposalReviewModal({ pageId, initialProposalId, canonicalContent, canonicalTitle, canEdit, onClose, onPublished }: { pageId?: string; initialProposalId?: string; canonicalContent: unknown[]; canonicalTitle: string; canEdit: boolean; onClose: () => void; onPublished: () => void }) {
  const [proposals, setProposals] = useState<PageChangeProposal[]>([]);
  const [selectedId, setSelectedId] = useState(initialProposalId ?? '');
  const [tab, setTab] = useState<ReviewTab>('edit');
  const [busy, setBusy] = useState(false);
  useExclusiveModal(onClose);
  useEffect(() => {
    const request = pageId ? api.listProposals(pageId) : api.listAllProposals();
    void request.then((items) => { setProposals(items); setSelectedId((current) => current && items.some((item) => item.id === current) ? current : items[0]?.id ?? ''); }).catch(() => toast(t('Proposals could not be loaded')));
  }, [pageId]);
  const selected = proposals.find((proposal) => proposal.id === selectedId) ?? null;
  const canonicalForSelected = selected?.kind === 'create' ? [] : canonicalContent;
  const titleForSelected = selected?.kind === 'create' ? '' : canonicalTitle;
  const updateSelected = (next: PageChangeProposal) => setProposals((items) => items.map((item) => item.id === next.id ? next : item));
  const save = async () => {
    if (!selected) return;
    setBusy(true);
    try {
      const updated = await api.updateProposal(selected.id, { proposedTitle: selected.proposedTitle, markdown: markdownFromBlocks(selected.proposedContent), proposedDescription: selected.proposedDescription, proposedTags: selected.proposedTags, factReview: selected.factReview, relatedCandidates: selected.relatedCandidates, selectedRelatedIds: selected.selectedRelatedIds });
      updateSelected(updated);
      toast(t('Proposal changes saved'));
    } catch (error) { toast(error instanceof Error ? error.message : t('Proposal changes could not be saved')); } finally { setBusy(false); }
  };
  const act = async (action: 'publish' | 'reject') => {
    if (!selected || selected.status !== 'pending') return;
    setBusy(true);
    try {
      const updated = pageId ? (action === 'publish' ? await api.publishProposal(pageId, selected.id) : await api.rejectProposal(pageId, selected.id)) : (action === 'publish' ? await api.publishProposalById(selected.id) : await api.rejectProposalById(selected.id));
      updateSelected(updated);
      toast(action === 'publish' ? t(selected.kind === 'create' ? 'Document published' : 'Proposed revision published') : t('Proposed revision rejected'));
      if (action === 'publish') onPublished();
    } catch (error) { toast(error instanceof Error ? error.message : t('Proposal action failed')); } finally { setBusy(false); }
  };
  return <Portal><div className="modal-overlay" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><div className="dialog proposal-dialog" role="dialog" aria-modal="true" aria-label={t('Human review workspace')}><div className="proposal-dialog-head"><div><h2>{t('Human review workspace')}</h2><p className="dialog-hint">{t('Edit proposal state only. Canonical content changes only after human Publish.')}</p></div><button className="icon-btn" onClick={onClose} aria-label={t('Close')}><X size={18} /></button></div><div className="proposal-layout"><aside className="proposal-sidebar" aria-label={t('Proposal list')}>{proposals.map((proposal) => <button className={`proposal-list-item${proposal.id === selectedId ? ' selected' : ''}`} key={proposal.id} onClick={() => setSelectedId(proposal.id)}><span className="proposal-list-icon"><FileText size={15} /></span><span className="proposal-list-copy"><strong>{proposal.kind === 'create' ? proposal.proposedTitle : (proposal.summary || t('Untitled proposed revision'))}</strong><small>{proposal.kind === 'create' ? t('New document') : t('Edit proposal')} · {proposal.creatorName || t('Unknown creator')} · {formatMoment(proposal.createdAt, 'full')}</small></span><span className={`proposal-status proposal-status-${proposal.status}`}>{statusLabel(proposal.status)}</span></button>)}{proposals.length === 0 && <p className="dialog-hint">{t('No proposals yet.')}</p>}</aside><section className="proposal-main">{selected ? <><div className="proposal-meta"><div><span className={`proposal-status proposal-status-${selected.status}`}>{statusLabel(selected.status)}</span><span className="proposal-creator">{selected.kind === 'create' ? t('New document · Will be created on Publish') : t('Edit proposal · stale protection active')} · {selected.creatorType} · {selected.creatorName || t('Unknown creator')}</span></div>{selected.summary && <p>{selected.summary}</p>}</div><div className="proposal-workspace"><div className="proposal-editor-column">{canEdit && selected.status === 'pending' && <div className="proposal-editor-fields"><label>{t('Title')}<input value={selected.proposedTitle} onChange={(event) => updateSelected({ ...selected, proposedTitle: event.target.value })} /></label><label>{t('Document body')}<textarea value={markdownFromBlocks(selected.proposedContent)} onChange={(event) => updateSelected({ ...selected, proposedContent: [{ id: 'human-edit', type: 'paragraph', props: {}, content: [{ type: 'text', text: event.target.value, styles: {} }] }] })} /></label><button className="btn" disabled={busy} onClick={() => void save()}><Save size={15} /> {t('Save edits')}</button></div>}<div className="proposal-tabs" role="tablist"><button className={tab === 'edit' ? 'active' : ''} onClick={() => setTab('edit')} role="tab" aria-selected={tab === 'edit'}><FileText size={15} /> {t('Review')}</button><button className={tab === 'preview' ? 'active' : ''} onClick={() => setTab('preview')} role="tab" aria-selected={tab === 'preview'}><FileText size={15} /> {t('Preview')}</button><button className={tab === 'diff' ? 'active' : ''} onClick={() => setTab('diff')} role="tab" aria-selected={tab === 'diff'}><GitCompare size={15} /> {t('Changes only')}</button></div>{tab === 'diff' ? <Diff proposal={selected} canonicalContent={canonicalForSelected} canonicalTitle={titleForSelected} /> : <Preview proposal={selected} />}</div><ReviewPane proposal={selected} onChange={updateSelected} /></div>{canEdit && selected.status === 'pending' && <div className="dialog-actions proposal-actions"><button className="btn danger" disabled={busy} onClick={() => void act('reject')}><X size={15} /> {t('Reject')}</button><button className="btn primary" disabled={busy} onClick={() => void act('publish')}><Check size={15} /> {t(selected.kind === 'create' ? 'Publish document' : 'Publish revision')}</button></div>}</> : <p className="dialog-hint">{t('Select a proposal to review it.')}</p>}</section></div></div></div></Portal>;
}
