import { useCallback, useEffect, useState } from 'react';
import { ClipboardCheck, Plus, RefreshCw, Search, Sparkles } from 'lucide-react';
import { api } from '../api';
import { formatMoment } from '../format';
import { t } from '../i18n';
import type { SkillDelivery, SkillListEntry, SkillStatus } from '../types';
import {
  DeliveryBadge,
  DependencyWarning,
  PublishedVersion,
  ScopeSummary,
  StatusBadge,
  describeSkillState,
} from './skillBits';

// The Skills library — the human entry point.
//
// The whole design goal of this screen is in §7.2 of the PRD: somebody should
// be able to answer "can this be used, by whom, and which version" WITHOUT
// opening anything. So every row carries delivery, status, published version,
// scope and dependency health, and the list endpoint deliberately does not send
// instruction bodies — a hundred skills would otherwise be a hundred bodies on
// the wire for a screen that shows none of them.

type StatusFilter = '' | SkillStatus;
type DeliveryFilter = '' | SkillDelivery;

export default function SkillsLibrary({
  workspaceId,
  pendingCount,
  onOpen,
  onNew,
  onOpenQueue,
}: {
  workspaceId: string;
  pendingCount: number;
  onOpen: (skillId: string) => void;
  onNew: () => void;
  onOpenQueue: () => void;
}) {
  const [entries, setEntries] = useState<SkillListEntry[] | null>(null);
  const [error, setError] = useState('');
  const [query, setQuery] = useState('');
  const [delivery, setDelivery] = useState<DeliveryFilter>('');
  const [status, setStatus] = useState<StatusFilter>('');
  const [owner, setOwner] = useState('');

  const load = useCallback(async () => {
    setError('');
    try {
      setEntries(await api.listSkills({ workspace: workspaceId, q: query, delivery, status, owner }));
    } catch (err) {
      // The list is REPLACED by the failure state rather than left showing
      // stale rows with a toast over them: a library that silently shows
      // yesterday's answer is how somebody concludes a skill was never
      // published.
      setEntries(null);
      setError(err instanceof Error ? err.message : t('The skills could not be loaded.'));
    }
  }, [workspaceId, query, delivery, status, owner]);

  useEffect(() => {
    void load();
  }, [load]);

  const owners = new Map<string, string>();
  for (const entry of entries ?? []) {
    if (entry.ownerId) owners.set(entry.ownerId, entry.ownerName || entry.ownerId);
  }
  const canReview = (entries ?? []).some((entry) => entry.canReview);
  const filtersActive = Boolean(query || delivery || status || owner);

  return (
    <div className="skills-page">
      <header className="skills-head">
        <div className="skills-title">
          <h1>
            <Sparkles size={20} /> {t('Skills')}
          </h1>
          <p className="skills-sub">
            {t(
              'What this team has agreed on, and what agents are allowed to follow. Remote skills are sent to an agent for the task at hand; client skills are packages installed in a runtime.',
            )}
          </p>
        </div>
        <div className="skills-head-actions">
          {(canReview || pendingCount > 0) && (
            <button className="btn" onClick={onOpenQueue}>
              <ClipboardCheck size={15} /> {t('Review queue')}
              {pendingCount > 0 && <span className="skills-count">{pendingCount}</span>}
            </button>
          )}
          <button className="btn primary" onClick={onNew}>
            <Plus size={15} /> {t('New skill')}
          </button>
        </div>
      </header>

      <div className="skills-filters">
        <label className="skills-search">
          <Search size={15} />
          <input
            value={query}
            placeholder={t('Search skills')}
            onChange={(event) => setQuery(event.target.value)}
          />
        </label>
        <select value={delivery} onChange={(event) => setDelivery(event.target.value as DeliveryFilter)}
          aria-label={t('Delivery')}>
          <option value="">{t('Any delivery')}</option>
          <option value="remote">{t('Remote')}</option>
          <option value="client">{t('Client')}</option>
        </select>
        <select value={status} onChange={(event) => setStatus(event.target.value as StatusFilter)}
          aria-label={t('Status')}>
          <option value="">{t('Any status')}</option>
          <option value="approved">{t('Approved')}</option>
          <option value="pending">{t('Pending review')}</option>
          <option value="draft">{t('Draft')}</option>
          <option value="deprecated">{t('Deprecated')}</option>
        </select>
        <select value={owner} onChange={(event) => setOwner(event.target.value)} aria-label={t('Owner')}>
          <option value="">{t('Any owner')}</option>
          {[...owners.entries()].map(([id, name]) => (
            <option value={id} key={id}>
              {name}
            </option>
          ))}
        </select>
        {filtersActive && (
          <button
            className="btn-link"
            onClick={() => {
              setQuery('');
              setDelivery('');
              setStatus('');
              setOwner('');
            }}
          >
            {t('Clear filters')}
          </button>
        )}
      </div>

      {error && (
        <div className="skills-state skills-state-error">
          <p>{error}</p>
          <button className="btn" onClick={() => void load()}>
            <RefreshCw size={15} /> {t('Try again')}
          </button>
        </div>
      )}

      {!error && entries === null && (
        <div className="skills-skeleton" aria-hidden="true">
          {[0, 1, 2, 3].map((row) => (
            <div className="skills-skeleton-row" key={row} />
          ))}
        </div>
      )}

      {/* The two empty states say different things on purpose. "Nothing here
          yet" is an invitation and has to explain what the two delivery modes
          are; "nothing matches" must keep the filters visible, or somebody
          concludes their library is empty when they had a filter on. */}
      {!error && entries !== null && entries.length === 0 && !filtersActive && (
        <div className="skills-state">
          <h2>{t('No skills yet')}</h2>
          <p>
            {t(
              'A Remote Skill is instructions VUS sends to an agent for a task — nothing is installed anywhere. A Client Skill is a package with scripts or assets that has to exist in the runtime. Both are reviewed before any agent sees them.',
            )}
          </p>
          <button className="btn primary" onClick={onNew}>
            <Plus size={15} /> {t('Create the first skill')}
          </button>
        </div>
      )}
      {!error && entries !== null && entries.length === 0 && filtersActive && (
        <div className="skills-state">
          <h2>{t('No skills match these filters')}</h2>
          <p>{t('The filters above are still applied.')}</p>
        </div>
      )}

      {!error && entries !== null && entries.length > 0 && (
        <table className="skills-table">
          <thead>
            <tr>
              <th>{t('Name')}</th>
              <th>{t('Delivery')}</th>
              <th>{t('Status')}</th>
              <th>{t('Published')}</th>
              <th>{t('Scope')}</th>
              <th>{t('Owner')}</th>
              <th>{t('Updated')}</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.id} onClick={() => onOpen(entry.id)} className="skills-row">
                <td>
                  <button className="skills-name" onClick={() => onOpen(entry.id)}>
                    {entry.name}
                  </button>
                  <span className="skills-desc">{entry.description}</span>
                  {entry.draftVersion && entry.currentVersionId && (
                    <span className="skills-inflight">
                      {t('v{n} in draft', { n: entry.draftVersion })}
                    </span>
                  )}
                  {entry.pendingVersion && (
                    <span className="skills-inflight">
                      {t('v{n} waiting for review', { n: entry.pendingVersion })}
                    </span>
                  )}
                </td>
                <td>
                  <DeliveryBadge delivery={entry.delivery} />
                </td>
                <td>
                  <StatusBadge status={describeSkillState(entry)} />
                </td>
                <td>
                  <PublishedVersion skill={entry} />
                </td>
                <td>
                  <ScopeSummary summary={entry.scopeSummary} />
                  <DependencyWarning issues={entry.dependencyIssues} />
                </td>
                <td>{entry.ownerName || '—'}</td>
                <td>{formatMoment(entry.updatedAt, 'datetime')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
