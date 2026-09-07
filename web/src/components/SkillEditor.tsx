import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ArrowLeft, Eye, Pencil, Plus, Save, Send, Trash2, X } from 'lucide-react';
import { api, ApiError } from '../api';
import { t } from '../i18n';
import { toast } from '../toast';
import type {
  SkillDelivery,
  SkillDependency,
  SkillDetail,
  SkillDraftPayload,
  SkillReference,
  SkillVersion,
} from '../types';
import { DraftSafetyBanner, shortHash } from './skillBits';

// The authoring screen.
//
// A dedicated page rather than a modal, for a reason that is not aesthetic: the
// instructions field is the product. Somebody writing a review checklist needs
// room and needs it to survive a mis-click on a backdrop, and a small dialog
// with a scrollbar teaches people to write short, useless skills.
//
// Three properties are load-bearing here, and each of them is a thing that goes
// wrong in forms like this:
//
//  1. A FAILED SAVE KEEPS THE FORM. Everything lives in local state and is only
//     replaced by a server answer on SUCCESS. An author who hits a conflict or
//     a validation error still has every word they typed.
//  2. NO SILENT RESET. Reloading the skill does not overwrite fields the author
//     has touched — otherwise a background refresh eats an edit in progress.
//  3. THE STATUS IS NEVER GUESSED LOCALLY. The banner and the read-only state
//     come from the version the server returned, so the screen cannot show
//     "published" before the publish actually happened.

/** The editable shape. Kept flat and all-strings where the input is text, so
 *  that a half-typed trigger list is a valid state rather than a parse error. */
type Draft = {
  name: string;
  slug: string;
  description: string;
  delivery: SkillDelivery;
  instructions: string;
  triggers: string;
  projects: string;
  repositories: string;
  roles: string;
  taskTypes: string;
  agentTypes: string;
  dependencies: SkillDependency[];
  references: SkillReference[];
  entrypoint: string;
};

const splitList = (value: string): string[] =>
  value
    .split(/[,\n]/)
    .map((part) => part.trim())
    .filter(Boolean);

const joinList = (values?: string[]): string => (values ?? []).join(', ');

function draftFrom(detail: SkillDetail, version: SkillVersion): Draft {
  return {
    name: detail.skill.name,
    slug: detail.skill.slug,
    description: version.description,
    delivery: version.deliveryMode,
    instructions: version.instructions,
    triggers: joinList(version.triggers),
    projects: joinList(version.scope.projectIds),
    repositories: joinList(version.scope.repositoryPatterns),
    roles: joinList(version.scope.roles),
    taskTypes: joinList(version.scope.taskTypes),
    agentTypes: joinList(version.scope.agentTypes),
    dependencies: version.dependencies.map((dep) => ({ ...dep })),
    references: version.references.map((ref) => ({ ...ref })),
    entrypoint: version.packageMetadata.entrypoint ?? '',
  };
}

function payloadFrom(draft: Draft, workspaceId: string, updatedAt?: string): SkillDraftPayload {
  return {
    name: draft.name,
    slug: draft.slug,
    description: draft.description,
    deliveryMode: draft.delivery,
    instructions: draft.instructions,
    triggers: splitList(draft.triggers),
    dependencies: draft.dependencies.filter((dep) => dep.id.trim() !== ''),
    references: draft.references.filter((ref) => ref.target.trim() !== ''),
    scope: {
      workspaceId,
      projectIds: splitList(draft.projects),
      repositoryPatterns: splitList(draft.repositories),
      roles: splitList(draft.roles),
      taskTypes: splitList(draft.taskTypes),
      agentTypes: splitList(draft.agentTypes),
    },
    packageMetadata: { entrypoint: draft.entrypoint.trim() },
    updatedAt,
  };
}

/** What a person must fill in before Submit review is worth attempting.
 *
 *  Checked here as well as on the server, and the server's copy is the one that
 *  decides — this exists only so an author is told before a round trip, not as
 *  the rule itself. A form-only rule is not a rule. */
function localValidation(draft: Draft): string[] {
  const problems: string[] = [];
  if (!draft.name.trim()) problems.push(t('A name is needed.'));
  if (!draft.description.trim())
    problems.push(t('A description is needed — it is what the catalogue shows an agent.'));
  if (!draft.instructions.trim()) {
    problems.push(
      draft.delivery === 'remote'
        ? t('A Remote Skill is its instructions — there is nothing to deliver without them.')
        : t('A Client Skill needs its SKILL.md content — that is what the package installs.'),
    );
  }
  for (const dep of draft.dependencies) {
    if (dep.id.trim() === '' && (dep.versionConstraint || dep.fallback))
      problems.push(t('A dependency row has no id.'));
  }
  return problems;
}

export default function SkillEditor({
  detail,
  version,
  onBack,
  onSaved,
  onSubmitted,
}: {
  detail: SkillDetail;
  version: SkillVersion;
  onBack: () => void;
  onSaved: (next: SkillDetail) => void;
  onSubmitted: () => void;
}) {
  const [draft, setDraft] = useState<Draft>(() => draftFrom(detail, version));
  const [savedAt, setSavedAt] = useState(version.updatedAt);
  const [busy, setBusy] = useState(false);
  const [problems, setProblems] = useState<string[]>([]);
  const [tab, setTab] = useState<'write' | 'preview'>('write');
  const dirtyRef = useRef(false);
  const editable = version.status === 'draft';

  const update = useCallback(<K extends keyof Draft>(key: K, value: Draft[K]) => {
    dirtyRef.current = true;
    setDraft((current) => ({ ...current, [key]: value }));
  }, []);

  // Unsaved-change protection. The browser's own prompt rather than a custom
  // dialog: it is the one people recognise, and it fires on a closed tab as
  // well as on navigation, which a React-only guard cannot do.
  useEffect(() => {
    const warn = (event: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return;
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, []);

  const save = async (): Promise<SkillVersion | null> => {
    setBusy(true);
    setProblems([]);
    try {
      const answer = await api.saveSkillVersion(
        detail.skill.id,
        version.id,
        payloadFrom(draft, detail.skill.workspaceId, savedAt),
      );
      // Only NOW is the local state considered clean, and only the server's
      // timestamps and hash are taken over — the text stays exactly as typed,
      // so a save cannot reformat somebody's work under their cursor.
      dirtyRef.current = false;
      setSavedAt(answer.version.updatedAt);
      onSaved({ ...detail, skill: answer.skill, draft: answer.version });
      toast(t('Draft saved'));
      return answer.version;
    } catch (err) {
      // The form is untouched. That is the point: the author reads the message,
      // fixes one field and saves again with everything else still there.
      const conflict = err instanceof ApiError && err.code === 'skill_conflict';
      setProblems([
        conflict
          ? t('Somebody else saved this draft while you were editing. Copy your changes, reload, and apply them again.')
          : err instanceof Error
            ? err.message
            : t('The draft could not be saved.'),
      ]);
      return null;
    } finally {
      setBusy(false);
    }
  };

  const submit = async () => {
    const found = localValidation(draft);
    if (found.length > 0) {
      setProblems(found);
      return;
    }
    // Saved first, so a reviewer never sees a version older than what the
    // author was looking at when they pressed the button.
    const saved = await save();
    if (!saved) return;
    setBusy(true);
    try {
      await api.submitSkillVersion(detail.skill.id, saved.id);
      dirtyRef.current = false;
      toast(t('Sent for review'));
      onSubmitted();
    } catch (err) {
      setProblems([err instanceof Error ? err.message : t('It could not be sent for review.')]);
    } finally {
      setBusy(false);
    }
  };

  const characters = draft.instructions.length;
  const lines = useMemo(() => draft.instructions.split('\n').length, [draft.instructions]);

  return (
    <div className="skills-page skill-editor">
      <header className="skills-head">
        <div className="skills-title">
          <button className="btn-link" onClick={onBack}>
            <ArrowLeft size={15} /> {t('Back to the skill')}
          </button>
          <h1>
            {detail.skill.name || t('New skill')} <span className="skill-muted">v{version.version}</span>
          </h1>
        </div>
        <div className="skills-head-actions">
          <button className="btn" disabled={busy || !editable} onClick={() => void save()}>
            <Save size={15} /> {t('Save draft')}
          </button>
          <button className="btn primary" disabled={busy || !editable} onClick={() => void submit()}>
            <Send size={15} /> {t('Submit review')}
          </button>
        </div>
      </header>

      <DraftSafetyBanner version={version} />

      {problems.length > 0 && (
        <div className="skill-problems" role="alert">
          <strong>{t('Not saved yet')}</strong>
          <ul>
            {problems.map((problem) => (
              <li key={problem}>{problem}</li>
            ))}
          </ul>
        </div>
      )}

      {version.reviewNote && (
        <div className="skill-review-note">
          <strong>{t('The reviewer asked for changes')}</strong>
          <p>{version.reviewNote}</p>
        </div>
      )}

      <fieldset className="skill-section" disabled={!editable}>
        <legend>{t('Basics')}</legend>
        <div className="skill-grid">
          <label>
            {t('Name')}
            <input value={draft.name} onChange={(event) => update('name', event.target.value)} />
          </label>
          <label>
            {t('Slug')}
            <input value={draft.slug} onChange={(event) => update('slug', event.target.value)} />
            <small>
              {detail.skill.currentVersionId
                ? t('Locked — agents and the audit trail refer to it.')
                : t('How agents and the audit trail name this skill.')}
            </small>
          </label>
          <label className="skill-wide">
            {t('Description')}
            <input
              value={draft.description}
              onChange={(event) => update('description', event.target.value)}
            />
            <small>{t('One line. This is what an agent sees in the catalogue before anything else.')}</small>
          </label>
        </div>
        <div className="skill-delivery-choice">
          <span className="skill-field-label">{t('Delivery')}</span>
          <div className="skill-segmented">
            <button
              type="button"
              className={draft.delivery === 'remote' ? 'active' : ''}
              onClick={() => update('delivery', 'remote')}
            >
              {t('Remote')}
            </button>
            <button
              type="button"
              className={draft.delivery === 'client' ? 'active' : ''}
              onClick={() => update('delivery', 'client')}
            >
              {t('Client')}
            </button>
          </div>
          <p className="skill-hint">
            {draft.delivery === 'remote'
              ? t('VUS sends these instructions straight to the agent working on the task. Nothing is installed anywhere.')
              : t('A package the runtime has to install or sync — for scripts, assets or host-native skill semantics.')}
          </p>
        </div>
      </fieldset>

      <fieldset className="skill-section" disabled={!editable}>
        <legend>{t('When this applies')}</legend>
        <p className="skill-hint">
          {t(
            'Leave a field empty to mean "anywhere". A field that IS filled in becomes a requirement: if the agent cannot name a matching value, the skill is not selected.',
          )}
        </p>
        <div className="skill-grid">
          <label>
            {t('Triggers')}
            <input value={draft.triggers} onChange={(event) => update('triggers', event.target.value)} />
            <small>{t('Words matched against the task, comma separated. Each match adds to the score.')}</small>
          </label>
          <label>
            {t('Task types')}
            <input value={draft.taskTypes} onChange={(event) => update('taskTypes', event.target.value)} />
          </label>
          <label>
            {t('Projects')}
            <input value={draft.projects} onChange={(event) => update('projects', event.target.value)} />
          </label>
          <label>
            {t('Repositories')}
            <input
              value={draft.repositories}
              onChange={(event) => update('repositories', event.target.value)}
            />
            <small>{t('One star allowed, e.g. acme/* or *-service.')}</small>
          </label>
          <label>
            {t('Roles')}
            <input value={draft.roles} onChange={(event) => update('roles', event.target.value)} />
          </label>
          <label>
            {t('Agents')}
            <input value={draft.agentTypes} onChange={(event) => update('agentTypes', event.target.value)} />
            <small>{t('Which runtimes this is meant for, e.g. chatgpt, claude.')}</small>
          </label>
        </div>
      </fieldset>

      <fieldset className="skill-section" disabled={!editable}>
        <legend>{draft.delivery === 'remote' ? t('Instructions') : t('SKILL.md content')}</legend>
        <div className="skill-tabs" role="tablist">
          <button
            className={tab === 'write' ? 'active' : ''}
            role="tab"
            aria-selected={tab === 'write'}
            onClick={() => setTab('write')}
          >
            <Pencil size={14} /> {t('Write')}
          </button>
          <button
            className={tab === 'preview' ? 'active' : ''}
            role="tab"
            aria-selected={tab === 'preview'}
            onClick={() => setTab('preview')}
          >
            <Eye size={14} /> {t('Preview')}
          </button>
          <span className="skill-counter">
            {t('{chars} characters, {lines} lines', { chars: characters, lines })}
          </span>
        </div>
        {tab === 'write' ? (
          <textarea
            className="skill-instructions"
            value={draft.instructions}
            spellCheck={false}
            onChange={(event) => update('instructions', event.target.value)}
            placeholder={
              draft.delivery === 'remote'
                ? t('Write it as you would tell a colleague. Say what to do, in what order, and what to leave alone.')
                : t('The SKILL.md body the package installs.')
            }
          />
        ) : (
          <pre className="skill-preview">{draft.instructions || t('Nothing written yet.')}</pre>
        )}
        {draft.delivery === 'client' && (
          <label className="skill-entrypoint">
            {t('Entrypoint')}
            <input
              value={draft.entrypoint}
              onChange={(event) => update('entrypoint', event.target.value)}
              placeholder="scripts/run.sh"
            />
            <small>{t('Optional. Recorded in the package manifest; VUS never runs it.')}</small>
          </label>
        )}
      </fieldset>

      <fieldset className="skill-section" disabled={!editable}>
        <legend>{t('Dependencies')}</legend>
        <p className="skill-hint">
          {t(
            'What the runtime must provide. A required dependency with no fallback excludes the skill where it is missing — which is better than handing an agent instructions it cannot carry out.',
          )}
        </p>
        {draft.dependencies.map((dep, index) => (
          <div className="skill-dep-row" key={index}>
            <select
              value={dep.type}
              aria-label={t('Dependency type')}
              onChange={(event) =>
                update(
                  'dependencies',
                  draft.dependencies.map((row, i) =>
                    i === index ? { ...row, type: event.target.value as SkillDependency['type'] } : row,
                  ),
                )
              }
            >
              <option value="capability">{t('capability')}</option>
              <option value="client_skill">{t('client skill')}</option>
              <option value="tool">{t('tool')}</option>
              <option value="connector">{t('connector')}</option>
            </select>
            <input
              value={dep.id}
              placeholder={t('id, e.g. browser')}
              onChange={(event) =>
                update(
                  'dependencies',
                  draft.dependencies.map((row, i) =>
                    i === index ? { ...row, id: event.target.value } : row,
                  ),
                )
              }
            />
            <input
              value={dep.versionConstraint ?? ''}
              placeholder=">=2.0.0"
              onChange={(event) =>
                update(
                  'dependencies',
                  draft.dependencies.map((row, i) =>
                    i === index ? { ...row, versionConstraint: event.target.value } : row,
                  ),
                )
              }
            />
            <label className="skill-dep-required-toggle">
              <input
                type="checkbox"
                checked={dep.required}
                onChange={(event) =>
                  update(
                    'dependencies',
                    draft.dependencies.map((row, i) =>
                      i === index ? { ...row, required: event.target.checked } : row,
                    ),
                  )
                }
              />
              {t('required')}
            </label>
            <input
              value={dep.fallback ?? ''}
              placeholder={t('if it is missing, do this instead')}
              onChange={(event) =>
                update(
                  'dependencies',
                  draft.dependencies.map((row, i) =>
                    i === index ? { ...row, fallback: event.target.value } : row,
                  ),
                )
              }
            />
            <button
              className="icon-btn"
              type="button"
              aria-label={t('Remove dependency')}
              onClick={() =>
                update(
                  'dependencies',
                  draft.dependencies.filter((_, i) => i !== index),
                )
              }
            >
              <Trash2 size={14} />
            </button>
          </div>
        ))}
        <button
          className="btn"
          type="button"
          onClick={() =>
            update('dependencies', [
              ...draft.dependencies,
              { type: 'capability', id: '', required: true },
            ])
          }
        >
          <Plus size={14} /> {t('Add dependency')}
        </button>
      </fieldset>

      <fieldset className="skill-section" disabled={!editable}>
        <legend>{t('References')}</legend>
        <p className="skill-hint">
          {t(
            'Pointers only. Anything an agent fetches through a reference is ordinary workspace content and stays untrusted — put whatever must be followed in the instructions above, where a reviewer reads it.',
          )}
        </p>
        {draft.references.map((ref, index) => (
          <div className="skill-ref-row" key={index}>
            <select
              value={ref.kind}
              aria-label={t('Reference kind')}
              onChange={(event) =>
                update(
                  'references',
                  draft.references.map((row, i) =>
                    i === index ? { ...row, kind: event.target.value as SkillReference['kind'] } : row,
                  ),
                )
              }
            >
              <option value="page">{t('page')}</option>
              <option value="url">{t('link')}</option>
              <option value="note">{t('note')}</option>
            </select>
            <input
              value={ref.target}
              placeholder={t('page id or link')}
              onChange={(event) =>
                update(
                  'references',
                  draft.references.map((row, i) =>
                    i === index ? { ...row, target: event.target.value } : row,
                  ),
                )
              }
            />
            <input
              value={ref.label ?? ''}
              placeholder={t('what it is')}
              onChange={(event) =>
                update(
                  'references',
                  draft.references.map((row, i) =>
                    i === index ? { ...row, label: event.target.value } : row,
                  ),
                )
              }
            />
            <button
              className="icon-btn"
              type="button"
              aria-label={t('Remove reference')}
              onClick={() =>
                update(
                  'references',
                  draft.references.filter((_, i) => i !== index),
                )
              }
            >
              <X size={14} />
            </button>
          </div>
        ))}
        <button
          className="btn"
          type="button"
          onClick={() => update('references', [...draft.references, { kind: 'url', target: '' }])}
        >
          <Plus size={14} /> {t('Add reference')}
        </button>
      </fieldset>

      <footer className="skill-editor-foot">
        <span className="skill-muted">
          {t('Content hash')}: <code>{shortHash(version.contentHash)}</code>
        </span>
      </footer>
    </div>
  );
}
