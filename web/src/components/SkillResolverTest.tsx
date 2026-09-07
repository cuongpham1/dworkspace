import { useState } from 'react';
import { FlaskConical } from 'lucide-react';
import { api } from '../api';
import { t } from '../i18n';
import type { SkillResolveResult } from '../types';
import { shortHash } from './skillBits';

// The resolver test.
//
// This screen exists so nobody has to call MCP by hand to answer the only
// question that matters about a skill: will an agent actually be given it?
//
// It posts the context and RENDERS WHAT THE BACKEND SAYS. There is no scoring
// in this file, and there must never be — a second implementation in TypeScript
// would agree with the server on the day it was written and drift afterwards,
// and a screen that is confidently wrong about which skill an agent gets is
// worse than no screen at all. The numbers below are the same numbers
// skill_resolve returns to ChatGPT.

export default function SkillResolverTest({
  workspaceId,
  highlightSkillId,
}: {
  workspaceId: string;
  highlightSkillId?: string;
}) {
  const [task, setTask] = useState('');
  const [project, setProject] = useState('');
  const [repository, setRepository] = useState('');
  const [role, setRole] = useState('');
  const [taskType, setTaskType] = useState('');
  const [agent, setAgent] = useState('');
  const [capabilities, setCapabilities] = useState('mcp');
  const [installed, setInstalled] = useState('');
  const [result, setResult] = useState<SkillResolveResult | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setBusy(true);
    setError('');
    try {
      setResult(
        await api.resolveSkills({
          workspaceId,
          task,
          project,
          repository,
          role,
          taskType,
          agent,
          capabilities: capabilities
            .split(/[,\n]/)
            .map((part) => part.trim())
            .filter(Boolean),
          // "agent-browser@2.1.0, figma" — the version is optional, because a
          // runtime that will not name one is a real case and the resolver
          // treats an unstated version as failing a constraint rather than
          // satisfying it.
          installedClientSkills: installed
            .split(/[,\n]/)
            .map((part) => part.trim())
            .filter(Boolean)
            .map((part) => {
              const [id, version] = part.split('@');
              return { id: id.trim(), version: (version ?? '').trim() };
            }),
        }),
      );
    } catch (err) {
      // The form is untouched, so a failed run costs nothing but the click.
      setError(err instanceof Error ? err.message : t('The resolver could not be run.'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="skill-resolver">
      <p className="skill-hint">
        {t(
          'Describe a task the way a person would give it to an agent. This runs the same resolver an agent gets over MCP — the scores below are the ones it would see.',
        )}
      </p>
      <div className="skill-grid">
        <label className="skill-wide">
          {t('Sample task')}
          <textarea
            value={task}
            rows={2}
            placeholder={t('Review the architecture of the billing service before we merge')}
            onChange={(event) => setTask(event.target.value)}
          />
        </label>
        <label>
          {t('Project')}
          <input value={project} onChange={(event) => setProject(event.target.value)} />
        </label>
        <label>
          {t('Repository')}
          <input
            value={repository}
            placeholder="acme/web"
            onChange={(event) => setRepository(event.target.value)}
          />
        </label>
        <label>
          {t('Role')}
          <input value={role} onChange={(event) => setRole(event.target.value)} />
        </label>
        <label>
          {t('Task type')}
          <input value={taskType} onChange={(event) => setTaskType(event.target.value)} />
        </label>
        <label>
          {t('Agent')}
          <input
            value={agent}
            placeholder="chatgpt" // i18n-ok: an agent's own name, matched literally by the resolver
            onChange={(event) => setAgent(event.target.value)}
          />
        </label>
        <label>
          {t('Capabilities')}
          <input
            value={capabilities}
            placeholder="mcp, browser, shell" // i18n-ok: capability ids, matched literally by the resolver
            onChange={(event) => setCapabilities(event.target.value)}
          />
        </label>
        <label className="skill-wide">
          {t('Installed client skills')}
          <input
            value={installed}
            placeholder="agent-browser@2.1.0"
            onChange={(event) => setInstalled(event.target.value)}
          />
          <small>{t('id@version, comma separated. Leave a version out to test a runtime that does not report one.')}</small>
        </label>
      </div>
      <button className="btn primary" disabled={busy || !task.trim()} onClick={() => void run()}>
        <FlaskConical size={15} /> {t('Run the resolver')}
      </button>

      {error && (
        <div className="skill-problems" role="alert">
          {error}
        </div>
      )}

      {result && (
        <div className="skill-resolver-result">
          {result.selected.length === 0 ? (
            <div className="skill-resolver-none">
              <strong>{t('Not selected')}</strong>
              <p>
                {result.considered === 0
                  ? t('No approved skill is published in this workspace yet.')
                  : t('{n} approved skills were considered and none matched.', {
                      n: result.considered,
                    })}
              </p>
            </div>
          ) : (
            result.selected.map((hit) => (
              <div
                className={
                  'skill-resolver-hit' + (hit.skillId === highlightSkillId ? ' highlight' : '')
                }
                key={hit.versionId}
              >
                <h3>
                  {t('Selected')}: {hit.slug}@{hit.version}
                  <span className="skill-resolver-score">{hit.score}</span>
                </h3>
                <ul className="skill-resolver-reasons">
                  {hit.reasons.map((reason) => (
                    <li key={reason.code + reason.label}>
                      <span className="skill-resolver-points">+{reason.points}</span> {reason.label}
                    </li>
                  ))}
                </ul>
                <p className="skill-muted">
                  {t('Content hash')}: <code>{shortHash(hit.contentHash)}</code>
                </p>
                {hit.missingDependencies && hit.missingDependencies.length > 0 ? (
                  <div className="skill-resolver-gap">
                    <strong>{t('Dependencies not satisfied')}</strong>
                    <ul>
                      {hit.missingDependencies.map((dep) => (
                        <li key={dep.type + dep.id}>
                          {dep.detail}
                          {dep.fallback && (
                            <span className="skill-dep-fallback">
                              {t('fallback')}: {dep.fallback}
                            </span>
                          )}
                        </li>
                      ))}
                    </ul>
                  </div>
                ) : (
                  <p className="skill-resolver-ok">{t('Dependencies: satisfied')}</p>
                )}
              </div>
            ))
          )}

          {/* The exclusions are the useful half when nothing was selected.
              "No skill matched" with no reason is the least actionable answer
              this system could give an author. */}
          {result.excluded.length > 0 && (
            <div className="skill-resolver-excluded">
              <h3>{t('Considered and not selected')}</h3>
              <ul>
                {result.excluded.map((miss) => (
                  <li key={miss.skillId + miss.version}>
                    <strong>
                      {miss.slug}@{miss.version}
                    </strong>{' '}
                    — {miss.reason}
                    {miss.missingDependencies && miss.missingDependencies.length > 0 && (
                      <ul>
                        {miss.missingDependencies.map((dep) => (
                          <li key={dep.type + dep.id}>{dep.detail}</li>
                        ))}
                      </ul>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
