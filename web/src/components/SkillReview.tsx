import { useCallback, useEffect, useState } from 'react';
import { ArrowLeft, Check, RefreshCw, Undo2 } from 'lucide-react';
import { api } from '../api';
import { confirm } from '../dialog';
import { formatMoment } from '../format';
import { t } from '../i18n';
import { toast } from '../toast';
import type { SkillDependency, SkillReviewEntry, SkillScope, SkillVersion } from '../types';
import { DeliveryBadge, RiskFlags, highImpactRisks, riskLabel, shortHash } from './skillBits';

// Review and publish — the screen where trust is actually granted.
//
// Two decisions shape it.
//
// The default view is a DIFF, not the whole skill. A reviewer looking at v5 of
// a checklist does not need to re-read v4; they need to see what moved. Reading
// it all from the top is how a two-line scope change gets approved without
// anybody noticing it.
//
// And the confirmation is not a speed bump. It fires only for the handful of
// changes that alter what the skill IS (a first Remote publication, Client →
// Remote, workspace-wide scope, a required dependency dropped, the instructions
// rewritten) and it repeats the exact changes back. A dialog that asks "are you
// sure?" on every action trains people to click through it, including the once
// it mattered.

export function SkillReviewQueue({
  workspaceId,
  onOpen,
  onBack,
}: {
  workspaceId: string;
  onOpen: (skillId: string, version: number) => void;
  onBack: () => void;
}) {
  const [entries, setEntries] = useState<SkillReviewEntry[] | null>(null);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    setError('');
    try {
      setEntries(await api.skillReviewQueue(workspaceId));
    } catch (err) {
      setEntries(null);
      setError(err instanceof Error ? err.message : t('The review queue could not be loaded.'));
    }
  }, [workspaceId]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="skills-page">
      <header className="skills-head">
        <div className="skills-title">
          <button className="btn-link" onClick={onBack}>
            <ArrowLeft size={15} /> {t('Skills')}
          </button>
          <h1>{t('Review queue')}</h1>
          <p className="skills-sub">
            {t('Nothing here reaches an agent until somebody publishes it.')}
          </p>
        </div>
      </header>

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
          <div className="skills-skeleton-row" />
          <div className="skills-skeleton-row" />
        </div>
      )}
      {/* A clean empty state, never an endless spinner: "nothing is waiting" is
          the answer a reviewer wants most often. */}
      {!error && entries !== null && entries.length === 0 && (
        <div className="skills-state">
          <h2>{t('Nothing is waiting for review')}</h2>
          <p>{t('Submitted versions appear here with a summary of what changed.')}</p>
        </div>
      )}
      {!error && entries !== null && entries.length > 0 && (
        <table className="skills-table">
          <thead>
            <tr>
              <th>{t('Skill')}</th>
              <th>{t('Proposed')}</th>
              <th>{t('Delivery')}</th>
              <th>{t('Submitted by')}</th>
              <th>{t('Submitted')}</th>
              <th>{t('What changed')}</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr
                className="skills-row"
                key={entry.version.id}
                onClick={() => onOpen(entry.skill.id, entry.version.version)}
              >
                <td>
                  <button
                    className="skills-name"
                    onClick={() => onOpen(entry.skill.id, entry.version.version)}
                  >
                    {entry.skill.name}
                  </button>
                  <span className="skills-desc">{entry.version.description}</span>
                </td>
                <td>v{entry.version.version}</td>
                <td>
                  <DeliveryBadge delivery={entry.version.deliveryMode} />
                </td>
                <td>{entry.version.createdByName || '—'}</td>
                <td>{formatMoment(entry.version.submittedAt ?? entry.version.updatedAt, 'datetime')}</td>
                <td>
                  <RiskFlags risks={entry.risks} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

/** One line of a field-level diff. `before` is undefined for a first
 *  publication, which the screen says explicitly rather than rendering an empty
 *  left column somebody could read as "it used to be blank". */
function DiffRow({
  label,
  before,
  after,
  firstPublish,
}: {
  label: string;
  before: string;
  after: string;
  firstPublish: boolean;
}) {
  const changed = before !== after;
  return (
    <div className={'skill-diff-row' + (changed ? ' changed' : '')}>
      <span className="skill-diff-label">{label}</span>
      {firstPublish ? (
        <div className="skill-diff-values">
          <span className="skill-diff-new">{after || <em>{t('empty')}</em>}</span>
        </div>
      ) : (
        <div className="skill-diff-values">
          {changed ? (
            <>
              <del>{before || <em>{t('empty')}</em>}</del>
              <ins>{after || <em>{t('empty')}</em>}</ins>
            </>
          ) : (
            <span className="skill-diff-same">{after || <em>{t('empty')}</em>}</span>
          )}
        </div>
      )}
    </div>
  );
}

const scopeLine = (scope: SkillScope): string => {
  const parts: string[] = [];
  const add = (label: string, values?: string[]) => {
    if (values && values.length > 0) parts.push(`${label}: ${values.join(', ')}`);
  };
  add(t('projects'), scope.projectIds);
  add(t('repositories'), scope.repositoryPatterns);
  add(t('roles'), scope.roles);
  add(t('task types'), scope.taskTypes);
  add(t('agents'), scope.agentTypes);
  return parts.length > 0 ? parts.join(' · ') : t('the whole workspace');
};

const dependencyLine = (dependencies: SkillDependency[]): string =>
  dependencies.length === 0
    ? t('none')
    : dependencies
        .map(
          (dep) =>
            `${dep.type}:${dep.id}${dep.versionConstraint ? ' ' + dep.versionConstraint : ''}` +
            (dep.required ? ' *' : ''),
        )
        .join(', ');

/** A line-by-line diff of the instructions.
 *
 *  Deliberately simple — a per-line comparison rather than a word-level one.
 *  What a reviewer needs is "which paragraphs are new"; a clever character diff
 *  of a rewritten checklist produces a wall of highlights that is harder to
 *  read than the two texts side by side. */
function InstructionsDiff({ before, after }: { before: string; after: string }) {
  const beforeLines = before.split('\n');
  const afterLines = after.split('\n');
  const beforeSet = new Set(beforeLines.map((line) => line.trim()));
  const afterSet = new Set(afterLines.map((line) => line.trim()));
  if (before === after)
    return <p className="skill-muted">{t('The instructions are unchanged.')}</p>;
  return (
    <div className="skill-instructions-diff">
      <div className="skill-diff-col">
        <h4>{t('Removed')}</h4>
        <pre>
          {beforeLines
            .filter((line) => line.trim() !== '' && !afterSet.has(line.trim()))
            .join('\n') || t('nothing')}
        </pre>
      </div>
      <div className="skill-diff-col">
        <h4>{t('Added')}</h4>
        <pre>
          {afterLines
            .filter((line) => line.trim() !== '' && !beforeSet.has(line.trim()))
            .join('\n') || t('nothing')}
        </pre>
      </div>
    </div>
  );
}

export function SkillReviewScreen({
  entry,
  onBack,
  onDecided,
}: {
  entry: SkillReviewEntry;
  onBack: () => void;
  onDecided: () => void;
}) {
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [showWhole, setShowWhole] = useState(false);
  const { skill, version, previous, risks, canReview, firstPublish } = entry;
  const empty: SkillVersion | undefined = previous;

  const before = {
    description: empty?.description ?? '',
    delivery: empty?.deliveryMode ?? '',
    scope: empty ? scopeLine(empty.scope) : '',
    dependencies: empty ? dependencyLine(empty.dependencies) : '',
    triggers: (empty?.triggers ?? []).join(', '),
    instructions: empty?.instructions ?? '',
    hash: empty?.contentHash ?? '',
  };
  const after = {
    description: version.description,
    delivery: version.deliveryMode,
    scope: scopeLine(version.scope),
    dependencies: dependencyLine(version.dependencies),
    triggers: version.triggers.join(', '),
    instructions: version.instructions,
    hash: version.contentHash,
  };

  const approve = async () => {
    const serious = highImpactRisks(risks);
    if (serious.length > 0) {
      // The confirmation repeats the EXACT changes rather than asking a vague
      // question. That is what makes it worth reading.
      const ok = await confirm(
        t('Publish {name} v{n}?', { name: skill.name, n: version.version }) +
          '\n\n' +
          serious.map((flag) => '• ' + riskLabel(flag)).join('\n') +
          '\n\n' +
          t('Once published, this version is immutable — changing it means a new version.'),
      );
      if (!ok) return;
    }
    setBusy(true);
    setError('');
    try {
      await api.approveSkillVersion(skill.id, version.id);
      toast(t('Published v{n}', { n: version.version }));
      onDecided();
    } catch (err) {
      setError(err instanceof Error ? err.message : t('It could not be published.'));
    } finally {
      setBusy(false);
    }
  };

  const requestChanges = async () => {
    if (!note.trim()) {
      setError(t('Say what needs changing — the author sees this instead of guessing.'));
      return;
    }
    setBusy(true);
    setError('');
    try {
      await api.requestSkillChanges(skill.id, version.id, note);
      toast(t('Sent back to the author'));
      onDecided();
    } catch (err) {
      // The note stays in the box: a failed submit must not cost somebody the
      // paragraph of feedback they just wrote.
      setError(err instanceof Error ? err.message : t('It could not be sent back.'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="skills-page skill-review">
      <header className="skills-head">
        <div className="skills-title">
          <button className="btn-link" onClick={onBack}>
            <ArrowLeft size={15} /> {t('Review queue')}
          </button>
          <h1>
            {skill.name} <span className="skill-muted">v{version.version}</span>
          </h1>
          <p className="skills-sub">
            {firstPublish
              ? t('Nothing has been published for this skill yet — this would be the first version agents can use.')
              : t('Compared against v{n}, the last version the workspace approved.', {
                  n: previous?.version ?? '?',
                })}
          </p>
        </div>
        <div className="skills-head-actions">
          <button className="btn-link" onClick={() => setShowWhole((current) => !current)}>
            {showWhole ? t('Show only what changed') : t('Show the whole version')}
          </button>
        </div>
      </header>

      <div className="skill-risk-summary">
        <RiskFlags risks={risks} />
      </div>

      {error && (
        <div className="skill-problems" role="alert">
          {error}
        </div>
      )}

      <section className="skill-section">
        <h2>{t('Metadata, scope and dependencies')}</h2>
        <DiffRow label={t('Description')} before={before.description} after={after.description} firstPublish={firstPublish} />
        <DiffRow label={t('Delivery')} before={before.delivery} after={after.delivery} firstPublish={firstPublish} />
        <DiffRow label={t('Scope')} before={before.scope} after={after.scope} firstPublish={firstPublish} />
        <DiffRow label={t('Triggers')} before={before.triggers} after={after.triggers} firstPublish={firstPublish} />
        <DiffRow
          label={t('Dependencies')}
          before={before.dependencies}
          after={after.dependencies}
          firstPublish={firstPublish}
        />
        <DiffRow
          label={t('Content hash')}
          before={shortHash(before.hash)}
          after={shortHash(after.hash)}
          firstPublish={firstPublish}
        />
      </section>

      <section className="skill-section">
        <h2>{version.deliveryMode === 'remote' ? t('Instructions') : t('SKILL.md content')}</h2>
        {showWhole || firstPublish ? (
          <pre className="skill-preview">{version.instructions || t('Nothing written.')}</pre>
        ) : (
          <InstructionsDiff before={before.instructions} after={after.instructions} />
        )}
      </section>

      {canReview ? (
        <section className="skill-section skill-decide">
          <h2>{t('Decide')}</h2>
          <label>
            {t('Review feedback')}
            <textarea
              value={note}
              placeholder={t('What has to change before this can be published?')}
              onChange={(event) => setNote(event.target.value)}
            />
          </label>
          <div className="skill-decide-actions">
            <button className="btn" disabled={busy} onClick={() => void requestChanges()}>
              <Undo2 size={15} /> {t('Request changes')}
            </button>
            <button className="btn primary" disabled={busy} onClick={() => void approve()}>
              <Check size={15} /> {t('Approve & publish')}
            </button>
          </div>
        </section>
      ) : (
        <section className="skill-section">
          <p className="skill-muted">
            {t('Only a workspace admin can publish or send this back. You can read the change here.')}
          </p>
        </section>
      )}
    </div>
  );
}
