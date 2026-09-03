import { useCallback, useEffect, useState } from 'react';
import {
  ArrowLeft,
  Ban,
  ClipboardCheck,
  Download,
  FilePlus2,
  Pencil,
  RefreshCw,
  Send,
} from 'lucide-react';
import { api } from '../api';
import { confirm, promptText } from '../dialog';
import { formatMoment } from '../format';
import { t } from '../i18n';
import { toast } from '../toast';
import type { SkillAuditEntry, SkillDetail as Detail, SkillPackageInfo, SkillVersion } from '../types';
import SkillResolverTest from './SkillResolverTest';
import {
  DeliveryBadge,
  DependencyList,
  DraftSafetyBanner,
  PublishedVersion,
  ScopeTable,
  StatusBadge,
  shortHash,
  statusLabel,
} from './skillBits';

// The skill detail — five questions, answered above the fold (§7.2 of the BRD):
// what it does, who uses it, Remote or Client, which version is live and which
// is being worked on, and what it depends on.
//
// The tabs exist to keep metadata and instructions apart. A single Markdown
// page holding both is what the old library was, and it is why nobody could
// tell whether a skill was usable without reading it.

type Tab = 'overview' | 'instructions' | 'scope' | 'versions' | 'resolver' | 'audit';

const AUDIT_ACTIONS = [
  'resolved',
  'fetched',
  'package_downloaded',
  'approved',
  'deprecated',
  'created',
  'updated',
  'submitted',
  'changes_requested',
];

function auditActionLabel(action: string): string {
  switch (action) {
    case 'created':
      return t('created');
    case 'updated':
      return t('draft saved');
    case 'submitted':
      return t('submitted for review');
    case 'changes_requested':
      return t('changes requested');
    case 'approved':
      return t('published');
    case 'deprecated':
      return t('deprecated');
    case 'resolved':
      return t('resolved for a task');
    case 'fetched':
      return t('instructions fetched');
    case 'package_downloaded':
      return t('package downloaded');
    default:
      return action;
  }
}

export default function SkillDetailScreen({
  detail,
  onBack,
  onEdit,
  onReview,
  onReload,
}: {
  detail: Detail;
  onBack: () => void;
  onEdit: (version: SkillVersion) => void;
  onReview: (version: number) => void;
  onReload: () => void;
}) {
  const [tab, setTab] = useState<Tab>('overview');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const { skill, versions, current, draft, pending, canReview, canAuthor } = detail;

  // The version the read-only tabs describe: the published one if there is one,
  // otherwise whatever is newest. Never a blank screen for a skill that has
  // only ever been a draft.
  const shown = current ?? draft ?? pending ?? versions[0];

  const act = async (run: () => Promise<unknown>, done: string) => {
    setBusy(true);
    setError('');
    try {
      await run();
      toast(done);
      onReload();
    } catch (err) {
      setError(err instanceof Error ? err.message : t('That did not work.'));
    } finally {
      setBusy(false);
    }
  };

  const newVersion = async (fromVersion?: number) => {
    setBusy(true);
    setError('');
    try {
      const version = await api.createSkillVersion(skill.id, {
        fromVersion: fromVersion ? String(fromVersion) : undefined,
      });
      onEdit(version);
    } catch (err) {
      setError(err instanceof Error ? err.message : t('A new version could not be started.'));
    } finally {
      setBusy(false);
    }
  };

  const deprecate = async (version: SkillVersion) => {
    const lastLive = current?.id === version.id;
    const ok = await confirm(
      lastLive
        ? t(
            'Deprecate v{n}? It is the published version, and nothing replaces it — no agent will be able to use this skill for new work until another version is published.',
            { n: version.version },
          )
        : t('Deprecate v{n}? It stays in the history and the audit trail.', { n: version.version }),
      { danger: true },
    );
    if (!ok) return;
    const note = await promptText(t('Why is it being deprecated?'), {
      placeholder: t('It is superseded, wrong, or no longer how we work'),
    });
    if (note === null) return;
    await act(() => api.deprecateSkillVersion(skill.id, version.id, note), t('Deprecated'));
  };

  return (
    <div className="skills-page skill-detail">
      <header className="skills-head">
        <div className="skills-title">
          <button className="btn-link" onClick={onBack}>
            <ArrowLeft size={15} /> {t('Skills')}
          </button>
          <h1>
            {skill.name}
            <DeliveryBadge delivery={shown?.deliveryMode ?? 'remote'} />
            <StatusBadge status={skill.lifecycleStatus} />
          </h1>
          <p className="skills-sub">{skill.description || t('No description yet.')}</p>
          <p className="skill-detail-facts">
            <span>
              {t('Published')}: <PublishedVersion skill={skill} />
            </span>
            <span>
              {t('Owner')}: {skill.ownerName || '—'}
            </span>
            <span>
              {t('Slug')}: <code>{skill.slug}</code>
            </span>
          </p>
        </div>
        {/* Which actions exist depends on the state, and the primary action is
            never Download for a Remote Skill — there is nothing to install, and
            offering it would teach exactly the wrong model. */}
        <div className="skills-head-actions">
          {draft && canAuthor && (
            <>
              <button className="btn" disabled={busy} onClick={() => onEdit(draft)}>
                <Pencil size={15} /> {t('Edit draft v{n}', { n: draft.version })}
              </button>
              <button
                className="btn primary"
                disabled={busy}
                onClick={() =>
                  void act(() => api.submitSkillVersion(skill.id, draft.id), t('Sent for review'))
                }
              >
                <Send size={15} /> {t('Submit review')}
              </button>
            </>
          )}
          {pending && canReview && (
            <button className="btn primary" disabled={busy} onClick={() => onReview(pending.version)}>
              <ClipboardCheck size={15} /> {t('Review v{n}', { n: pending.version })}
            </button>
          )}
          {pending && !canReview && (
            <span className="skill-muted">
              {t('v{n} is waiting for a workspace admin to review it.', { n: pending.version })}
            </span>
          )}
          {!draft && !pending && canAuthor && (
            <button className="btn" disabled={busy} onClick={() => void newVersion(current?.version)}>
              <FilePlus2 size={15} /> {t('Create new version')}
            </button>
          )}
          {current && canReview && (
            <button className="btn danger" disabled={busy} onClick={() => void deprecate(current)}>
              <Ban size={15} /> {t('Deprecate')}
            </button>
          )}
        </div>
      </header>

      {error && (
        <div className="skill-problems" role="alert">
          {error}
        </div>
      )}

      <nav className="skill-tabs" role="tablist">
        {(
          [
            ['overview', t('Overview')],
            ['instructions', shown?.deliveryMode === 'client' ? t('Package') : t('Instructions')],
            ['scope', t('Scope & dependencies')],
            ['versions', t('Versions')],
            ['resolver', t('Resolver test')],
            ['audit', t('Audit')],
          ] as [Tab, string][]
        ).map(([key, label]) => (
          <button
            key={key}
            role="tab"
            aria-selected={tab === key}
            className={tab === key ? 'active' : ''}
            onClick={() => setTab(key)}
          >
            {label}
          </button>
        ))}
      </nav>

      {tab === 'overview' && shown && (
        <section className="skill-section">
          <DraftSafetyBanner version={shown} />
          <div className="skill-grid skill-readonly">
            <div>
              <h3>{t('What it is for')}</h3>
              <p>{shown.description || t('No description yet.')}</p>
            </div>
            <div>
              <h3>{t('Triggers')}</h3>
              {shown.triggers.length > 0 ? (
                <p>
                  {shown.triggers.map((trigger) => (
                    <span className="skill-scope-chip" key={trigger}>
                      {trigger}
                    </span>
                  ))}
                </p>
              ) : (
                <p className="skill-muted">
                  {t('None — it will only be selected on scope, role or task type.')}
                </p>
              )}
            </div>
            <div>
              <h3>{t('Delivery')}</h3>
              <p>
                {shown.deliveryMode === 'remote'
                  ? t('Remote — VUS sends the instructions to the agent working on the task.')
                  : t('Client — a package installed in the runtime.')}
              </p>
            </div>
            <div>
              <h3>{t('References')}</h3>
              {shown.references.length > 0 ? (
                <ul className="skill-ref-list">
                  {shown.references.map((ref) => (
                    <li key={ref.kind + ref.target}>
                      <span className="skill-ref-kind">{ref.kind}</span>{' '}
                      {ref.label || ref.target}
                      <code>{ref.target}</code>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="skill-muted">{t('None.')}</p>
              )}
            </div>
          </div>
        </section>
      )}

      {tab === 'instructions' && shown && (
        <InstructionsTab skill={skill} version={shown} canAuthor={canAuthor} onEdit={onEdit} />
      )}

      {tab === 'scope' && shown && (
        <section className="skill-section">
          <h2>{t('Where it applies')}</h2>
          <p className="skill-hint">
            {t('An empty dimension means "anywhere". A filled one is a requirement the agent has to match.')}
          </p>
          <ScopeTable scope={shown.scope} />
          <h2>{t('Dependencies')}</h2>
          <DependencyList dependencies={shown.dependencies} />
        </section>
      )}

      {tab === 'versions' && (
        <section className="skill-section">
          <table className="skills-table">
            <thead>
              <tr>
                <th>{t('Version')}</th>
                <th>{t('Status')}</th>
                <th>{t('Delivery')}</th>
                <th>{t('Author')}</th>
                <th>{t('Approver')}</th>
                <th>{t('Updated')}</th>
                <th>{t('Hash')}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {versions.map((version) => (
                <tr key={version.id}>
                  <td>
                    v{version.version}
                    {version.published && <span className="skill-live-dot">{t('live')}</span>}
                  </td>
                  <td>
                    <StatusBadge status={version.status} />
                  </td>
                  <td>
                    <DeliveryBadge delivery={version.deliveryMode} />
                  </td>
                  <td>{version.createdByName || '—'}</td>
                  <td>{version.approvedByName || '—'}</td>
                  <td>{formatMoment(version.updatedAt, 'datetime')}</td>
                  <td>
                    <code>{shortHash(version.contentHash)}</code>
                  </td>
                  <td className="skill-version-actions">
                    {/* Edit is offered on a DRAFT and nowhere else. An
                        approved or deprecated version is history, and offering
                        to edit it would promise something the server refuses. */}
                    {version.status === 'draft' && canAuthor && (
                      <button className="btn-link" onClick={() => onEdit(version)}>
                        {t('Edit')}
                      </button>
                    )}
                    {version.status === 'pending' && canReview && (
                      <button className="btn-link" onClick={() => onReview(version.version)}>
                        {t('Review')}
                      </button>
                    )}
                    {(version.status === 'approved' || version.status === 'deprecated') &&
                      canAuthor &&
                      !draft &&
                      !pending && (
                        <button className="btn-link" onClick={() => void newVersion(version.version)}>
                          {t('New version from this')}
                        </button>
                      )}
                    {version.status === 'approved' && canReview && (
                      <button className="btn-link danger" onClick={() => void deprecate(version)}>
                        {t('Deprecate')}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      {tab === 'resolver' && (
        <section className="skill-section">
          <h2>{t('Resolver test')}</h2>
          <SkillResolverTest workspaceId={skill.workspaceId} highlightSkillId={skill.id} />
        </section>
      )}

      {tab === 'audit' && <AuditTab skillId={skill.id} versions={versions} />}
    </div>
  );
}

/** The instructions tab. Read-only unless the version shown is a draft — and
 *  when it is not, the action offered is "create a new version", not a disabled
 *  edit button that leaves somebody wondering why. */
function InstructionsTab({
  skill,
  version,
  canAuthor,
  onEdit,
}: {
  skill: Detail['skill'];
  version: SkillVersion;
  canAuthor: boolean;
  onEdit: (version: SkillVersion) => void;
}) {
  const [info, setInfo] = useState<SkillPackageInfo | null>(null);
  const [error, setError] = useState('');
  const isClient = version.deliveryMode === 'client';

  useEffect(() => {
    if (!isClient || version.status !== 'approved') {
      setInfo(null);
      return;
    }
    let active = true;
    void api
      .skillPackageInfo(skill.id, version.version)
      .then((answer) => {
        if (active) setInfo(answer);
      })
      .catch((err: unknown) => {
        // A package that cannot be built is worth SAYING rather than showing an
        // empty panel — it means the stored version is broken, which is exactly
        // the thing somebody needs to know before relying on it.
        if (active) setError(err instanceof Error ? err.message : t('The package could not be built.'));
      });
    return () => {
      active = false;
    };
  }, [isClient, skill.id, version.status, version.version]);

  return (
    <section className="skill-section">
      <DraftSafetyBanner version={version} />
      <pre className="skill-preview">{version.instructions || t('Nothing written yet.')}</pre>
      {version.status === 'draft' && canAuthor && (
        <button className="btn" onClick={() => onEdit(version)}>
          <Pencil size={15} /> {t('Edit this draft')}
        </button>
      )}
      {error && (
        <div className="skill-problems" role="alert">
          {error}
        </div>
      )}
      {isClient && version.status === 'approved' && (
        <div className="skill-package">
          <h3>{t('Package')}</h3>
          {info ? (
            <>
              <p className="skill-muted">
                {t('{n} files, archive hash {hash}', {
                  n: info.files.length,
                  hash: shortHash(info.archiveHash),
                })}
                {' · '}
                {t('The same version always produces the same bytes.')}
              </p>
              <ul className="skill-package-files">
                {info.files.map((file) => (
                  <li key={file.name}>
                    <code>{file.name}</code>
                  </li>
                ))}
              </ul>
              <button
                className="btn primary"
                onClick={() => api.downloadSkillPackage(skill.id, version.version)}
              >
                <Download size={15} /> {t('Download package')}
              </button>
            </>
          ) : (
            !error && <p className="skill-muted">{t('Building the package…')}</p>
          )}
        </div>
      )}
    </section>
  );
}

/** The audit tab. It answers "which task used which version", and it does that
 *  WITHOUT the prompts: the task column is a digest, so two rows for the same
 *  task match each other and nobody can read the task back out. */
function AuditTab({ skillId, versions }: { skillId: string; versions: SkillVersion[] }) {
  const [entries, setEntries] = useState<SkillAuditEntry[] | null>(null);
  const [error, setError] = useState('');
  const [action, setAction] = useState('');
  const [version, setVersion] = useState('');
  const [agent, setAgent] = useState('');

  const load = useCallback(async () => {
    setError('');
    try {
      setEntries(await api.skillAudit(skillId, { action, version, agent }));
    } catch (err) {
      setEntries(null);
      setError(err instanceof Error ? err.message : t('The audit trail could not be loaded.'));
    }
  }, [skillId, action, version, agent]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <section className="skill-section">
      <div className="skills-filters">
        <select value={action} onChange={(event) => setAction(event.target.value)} aria-label={t('Action')}>
          <option value="">{t('Any action')}</option>
          {AUDIT_ACTIONS.map((value) => (
            <option value={value} key={value}>
              {auditActionLabel(value)}
            </option>
          ))}
        </select>
        <select value={version} onChange={(event) => setVersion(event.target.value)} aria-label={t('Version')}>
          <option value="">{t('Any version')}</option>
          {versions.map((entry) => (
            <option value={String(entry.version)} key={entry.id}>
              v{entry.version}
            </option>
          ))}
        </select>
        <input
          value={agent}
          placeholder={t('Agent')}
          onChange={(event) => setAgent(event.target.value)}
        />
        <button className="btn" onClick={() => void load()}>
          <RefreshCw size={15} /> {t('Refresh')}
        </button>
      </div>
      {error && (
        <div className="skills-state skills-state-error">
          <p>{error}</p>
        </div>
      )}
      {!error && entries === null && <div className="skills-skeleton-row" aria-hidden="true" />}
      {!error && entries !== null && entries.length === 0 && (
        <p className="skill-muted">{t('Nothing recorded for these filters yet.')}</p>
      )}
      {!error && entries !== null && entries.length > 0 && (
        <table className="skills-table">
          <thead>
            <tr>
              <th>{t('When')}</th>
              <th>{t('Action')}</th>
              <th>{t('Version')}</th>
              <th>{t('Who')}</th>
              <th>{t('Runtime')}</th>
              <th>{t('Why / status')}</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.id}>
                <td>{formatMoment(entry.createdAt, 'datetime')}</td>
                <td>{auditActionLabel(entry.action)}</td>
                <td>
                  v{entry.versionNumber}
                  {entry.contentHash && <code>{shortHash(entry.contentHash)}</code>}
                </td>
                <td>
                  {entry.actorName || '—'}
                  {entry.actorType === 'agent' && <span className="skill-actor-agent">{t('agent')}</span>}
                </td>
                <td>
                  {entry.agentType || '—'}
                  {entry.projectRef && <span className="skill-muted"> · {entry.projectRef}</span>}
                </td>
                <td>
                  {entry.resolutionReason || '—'}
                  {entry.missingDependencies && entry.missingDependencies.length > 0 && (
                    <span className="skill-dep-blocks">
                      {t('missing')}: {entry.missingDependencies.join(', ')}
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="skill-hint">
        {t('The task itself is never stored — only a digest, so two runs of the same task can be matched up.')}
      </p>
    </section>
  );
}

export { statusLabel };
