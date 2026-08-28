import { useEffect, useMemo, useState } from 'react';
import { Check, FileText, GitCompare, X } from 'lucide-react';
import { api } from '../api';
import { formatMoment } from '../format';
import { t } from '../i18n';
import { useExclusiveModal } from '../modal';
import { toast } from '../toast';
import type { PageChangeProposal } from '../types';
import Portal from './Portal';

type ReviewTab = 'preview' | 'diff';

type UnknownRecord = Record<string, unknown>;

const isRecord = (value: unknown): value is UnknownRecord =>
  typeof value === 'object' && value !== null;

const blockText = (value: unknown): string => {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(blockText).filter(Boolean).join('');
  if (!isRecord(value)) return '';
  return blockText(value.content) || blockText(value.props) || '';
};

const blockType = (value: unknown): string => {
  if (!isRecord(value)) return 'block';
  const type = value.type;
  return typeof type === 'string' ? type : 'block';
};

const blocks = (value: unknown[]): Array<{ key: string; text: string; type: string }> =>
  value.map((block, index) => {
    const id = isRecord(block) && typeof block.id === 'string' ? block.id : String(index);
    return { key: id, text: blockText(block), type: blockType(block) };
  });

function statusLabel(status: PageChangeProposal['status'] | 'added' | 'removed' | 'changed'): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

function Preview({ proposal }: { proposal: PageChangeProposal }) {
  return (
    <div className="proposal-preview" aria-label={t('Proposed document preview')}>
      {proposal.proposedTitle && <h3 className="proposal-preview-title">{proposal.proposedTitle}</h3>}
      {blocks(proposal.proposedContent).map((block) => (
        <div className={`proposal-block proposal-block-${block.type}`} key={block.key}>
          {block.text || <span className="proposal-empty-block">{t('Empty block')}</span>}
        </div>
      ))}
      {proposal.proposedContent.length === 0 && <p className="dialog-hint">{t('The proposed document is empty.')}</p>}
    </div>
  );
}

function Diff({ proposal, canonicalContent, canonicalTitle }: {
  proposal: PageChangeProposal;
  canonicalContent: unknown[];
  canonicalTitle: string;
}) {
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
    current.forEach((block) => {
      if (!after.has(block.key)) rows.push({ kind: 'removed', text: block.text, type: block.type, key: `r-${block.key}` });
    });
    return rows;
  }, [current, proposed]);
  const titleChanged = canonicalTitle !== proposal.proposedTitle;

  return (
    <div className="proposal-diff" aria-label={t('Changes only')}>
      {titleChanged && (
        <div className="proposal-diff-row changed">
          <span className="proposal-diff-kind">{t('Title')}</span>
          <div><del>{canonicalTitle || t('Untitled')}</del><strong>{proposal.proposedTitle || t('Untitled')}</strong></div>
        </div>
      )}
      {changes.map((change) => (
        <div className={`proposal-diff-row ${change.kind}`} key={change.key}>
          <span className="proposal-diff-kind">{statusLabel(change.kind)}</span>
          <div><span className="proposal-diff-type">{change.type}</span> {change.text || t('Empty block')}</div>
        </div>
      ))}
      {!titleChanged && changes.length === 0 && <p className="dialog-hint">{t('No block-level changes detected.')}</p>}
    </div>
  );
}

export default function ProposalReviewModal({
  pageId,
  canonicalContent,
  canonicalTitle,
  canEdit,
  onClose,
  onPublished,
}: {
  pageId: string;
  canonicalContent: unknown[];
  canonicalTitle: string;
  canEdit: boolean;
  onClose: () => void;
  onPublished: () => void;
}) {
  const [proposals, setProposals] = useState<PageChangeProposal[]>([]);
  const [selectedId, setSelectedId] = useState('');
  const [tab, setTab] = useState<ReviewTab>('preview');
  const [busy, setBusy] = useState(false);
  useExclusiveModal(onClose);

  const load = () => void api.listProposals(pageId).then((items) => {
    setProposals(items);
    setSelectedId((current) => current && items.some((item) => item.id === current) ? current : items[0]?.id ?? '');
  }).catch(() => toast(t('Proposals could not be loaded')));
  useEffect(load, [pageId]);

  const selected = proposals.find((proposal) => proposal.id === selectedId) ?? null;
  const act = async (action: 'publish' | 'reject') => {
    if (!selected || selected.status !== 'pending') return;
    setBusy(true);
    try {
      const updated = action === 'publish'
        ? await api.publishProposal(pageId, selected.id)
        : await api.rejectProposal(pageId, selected.id);
      setProposals((items) => items.map((item) => item.id === updated.id ? updated : item));
      toast(action === 'publish' ? t('Proposed revision published') : t('Proposed revision rejected'));
      if (action === 'publish') onPublished();
    } catch (error) {
      toast(error instanceof Error ? error.message : t('Proposal action failed'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Portal>
      <div className="modal-overlay" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
        <div className="dialog wide proposal-dialog" role="dialog" aria-modal="true" aria-label={t('Proposed revisions')}>
          <div className="proposal-dialog-head">
            <div>
              <h2>{t('Proposed revisions')}</h2>
              <p className="dialog-hint">{t('Canonical content stays unchanged until a human publishes a pending revision.')}</p>
            </div>
            <button className="icon-btn" onClick={onClose} aria-label={t('Close')}><X size={18} /></button>
          </div>
          <div className="proposal-layout">
            <aside className="proposal-sidebar" aria-label={t('Proposal list')}>
              {proposals.map((proposal) => (
                <button className={`proposal-list-item${proposal.id === selectedId ? ' selected' : ''}`} key={proposal.id} onClick={() => setSelectedId(proposal.id)}>
                  <span className="proposal-list-icon"><FileText size={15} /></span>
                  <span className="proposal-list-copy">
                    <strong>{proposal.summary || t('Untitled proposed revision')}</strong>
                    <small>{proposal.creatorName || t('Unknown creator')} · {formatMoment(proposal.createdAt, 'full')}</small>
                  </span>
                  <span className={`proposal-status proposal-status-${proposal.status}`}>{statusLabel(proposal.status)}</span>
                </button>
              ))}
              {proposals.length === 0 && <p className="dialog-hint">{t('No proposed revisions yet.')}</p>}
            </aside>
            <section className="proposal-main">
              {selected ? (
                <>
                  <div className="proposal-meta">
                    <div><span className={`proposal-status proposal-status-${selected.status}`}>{statusLabel(selected.status)}</span><span className="proposal-creator">{selected.creatorType} · {selected.creatorName || t('Unknown creator')} · {formatMoment(selected.createdAt, 'full')}</span></div>
                    {selected.summary && <p>{selected.summary}</p>}
                  </div>
                  <div className="proposal-tabs" role="tablist">
                    <button className={tab === 'preview' ? 'active' : ''} onClick={() => setTab('preview')} role="tab" aria-selected={tab === 'preview'}><FileText size={15} /> {t('Preview')}</button>
                    <button className={tab === 'diff' ? 'active' : ''} onClick={() => setTab('diff')} role="tab" aria-selected={tab === 'diff'}><GitCompare size={15} /> {t('Changes only')}</button>
                  </div>
                  {tab === 'preview' ? <Preview proposal={selected} /> : <Diff proposal={selected} canonicalContent={canonicalContent} canonicalTitle={canonicalTitle} />}
                  {canEdit && selected.status === 'pending' && (
                    <div className="dialog-actions proposal-actions">
                      <button className="btn danger" disabled={busy} onClick={() => void act('reject')}><X size={15} /> {t('Reject')}</button>
                      <button className="btn primary" disabled={busy} onClick={() => void act('publish')}><Check size={15} /> {t('Publish revision')}</button>
                    </div>
                  )}
                </>
              ) : <p className="dialog-hint">{t('Select a proposed revision to review it.')}</p>}
            </section>
          </div>
          <button className="btn dialog-close" onClick={onClose}>{t('Close')}</button>
        </div>
      </div>
    </Portal>
  );
}
