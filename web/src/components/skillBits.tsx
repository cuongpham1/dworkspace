import { AlertTriangle, Cloud, HardDrive } from 'lucide-react';
import { t } from '../i18n';
import type {
  Skill,
  SkillDelivery,
  SkillDependency,
  SkillListEntry,
  SkillScope,
  SkillStatus,
  SkillVersion,
} from '../types';

// The small pieces every skill screen shares.
//
// They live in one file because the alternative is four screens that each
// spell a status badge slightly differently — and the first UX principle of
// this feature is that trust state has to be VISIBLE and unambiguous. A skill
// that reads "Approved" in the library and "Published" on its detail page has
// already taught the reader that the words do not mean anything exact.

/** Delivery mode, said in the words the product uses.
 *
 *  The two are not decoration: `Remote` means VUS hands the instructions to an
 *  agent for the current task with nothing installed anywhere, `Client` means a
 *  package that has to exist in the runtime. Somebody scanning the library
 *  needs to know which without opening anything. */
export function DeliveryBadge({ delivery }: { delivery: SkillDelivery }) {
  const remote = delivery === 'remote';
  return (
    <span className={'skill-badge skill-delivery-' + delivery}>
      {remote ? <Cloud size={12} /> : <HardDrive size={12} />}
      {remote ? t('Remote') : t('Client')}
    </span>
  );
}

export function statusLabel(status: SkillStatus): string {
  switch (status) {
    case 'draft':
      return t('Draft');
    case 'pending':
      return t('Pending review');
    case 'approved':
      return t('Approved');
    default:
      return t('Deprecated');
  }
}

export function StatusBadge({ status }: { status: SkillStatus }) {
  return <span className={'skill-badge skill-status-' + status}>{statusLabel(status)}</span>;
}

/** The published version, or an em dash. `v4` and "nothing is live" are
 *  different answers and the second one matters more: a skill with no published
 *  version cannot be used by an agent at all. */
export function PublishedVersion({ skill }: { skill: Skill }) {
  if (!skill.currentVersionId || !skill.currentVersion) {
    return <span className="skill-version-none" title={t('Nothing is published, so no agent can use this skill yet')}>—</span>;
  }
  return <span className="skill-version-live">v{skill.currentVersion}</span>;
}

/** Required dependencies with no declared fallback: the skill only works where
 *  the runtime has these. Not an error — the server cannot see a client's
 *  capabilities — but the thing the warning icon in the library means. */
export function DependencyWarning({ issues }: { issues: string[] }) {
  if (issues.length === 0) return null;
  return (
    <span
      className="skill-dep-warning"
      title={t('Only works where the runtime provides: {list}', { list: issues.join(', ') })}
    >
      <AlertTriangle size={13} /> {issues.length}
    </span>
  );
}

export function ScopeSummary({ summary }: { summary: string[] }) {
  if (summary.length === 0) return <span className="skill-muted">{t('workspace')}</span>;
  return (
    <span className="skill-scope-summary">
      {summary.map((part) => (
        <span className="skill-scope-chip" key={part}>
          {part}
        </span>
      ))}
    </span>
  );
}

export function shortHash(hash: string): string {
  return hash.length > 12 ? hash.slice(0, 12) : hash;
}

/** Translate a deterministic risk flag from the review queue.
 *
 *  Every one of these is a comparison of two stored versions, so the wording
 *  can be specific — and specific is the point. "This change is risky" tells a
 *  reviewer nothing they can check; "Delivery changed from Client to Remote"
 *  tells them exactly what to look at. */
export function riskLabel(flag: string): string {
  const [code, count] = flag.split(':');
  switch (code) {
    case 'first_publish':
      return t('First publication');
    case 'first_remote_publish':
      return t('First Remote Skill — instructions will be sent to agents');
    case 'client_to_remote':
      return t('Delivery changed: Client → Remote');
    case 'delivery_changed':
      return t('Delivery mode changed');
    case 'scope_expanded':
      return t('Scope expanded');
    case 'workspace_wide_scope':
      return t('Applies to the whole workspace');
    case 'dependencies_added':
      return t('{n} dependencies added', { n: count ?? '?' });
    case 'dependencies_removed':
      return t('{n} dependencies removed', { n: count ?? '?' });
    case 'required_dependency_removed':
      return t('A required dependency was removed');
    case 'instructions_changed':
      return t('Instructions changed');
    case 'description_changed':
      return t('Description changed');
    case 'triggers_changed':
      return t('Triggers changed');
    default:
      return code;
  }
}

/** Which risk flags are serious enough that approving asks for an explicit
 *  confirmation. The confirmation then summarises the exact change — a dialog
 *  that only says "are you sure?" is theatre and gets clicked through. */
const HIGH_IMPACT = new Set([
  'first_remote_publish',
  'client_to_remote',
  'workspace_wide_scope',
  'required_dependency_removed',
  'instructions_changed',
]);

export function highImpactRisks(risks: string[]): string[] {
  return risks.filter((flag) => HIGH_IMPACT.has(flag.split(':')[0]));
}

export function RiskFlags({ risks }: { risks: string[] }) {
  if (risks.length === 0) return <span className="skill-muted">{t('No detected change')}</span>;
  return (
    <span className="skill-risks">
      {risks.map((flag) => (
        <span
          className={
            'skill-risk' + (highImpactRisks([flag]).length > 0 ? ' skill-risk-high' : '')
          }
          key={flag}
        >
          {riskLabel(flag)}
        </span>
      ))}
    </span>
  );
}

export function dependencyLabel(dep: SkillDependency): string {
  const parts = [dep.type.replace('_', ' '), dep.id];
  if (dep.versionConstraint) parts.push(dep.versionConstraint);
  return parts.join(' ');
}

export function DependencyList({ dependencies }: { dependencies: SkillDependency[] }) {
  if (dependencies.length === 0)
    return <p className="skill-muted">{t('Runs anywhere — nothing is required of the runtime.')}</p>;
  return (
    <ul className="skill-dep-list">
      {dependencies.map((dep) => (
        <li key={dep.type + dep.id}>
          <code>{dependencyLabel(dep)}</code>
          <span className={dep.required ? 'skill-dep-required' : 'skill-dep-optional'}>
            {dep.required ? t('required') : t('optional')}
          </span>
          {dep.fallback && (
            <span className="skill-dep-fallback">
              {t('if missing')}: {dep.fallback}
            </span>
          )}
          {dep.required && !dep.fallback && (
            <span className="skill-dep-blocks">{t('excludes the skill when missing')}</span>
          )}
        </li>
      ))}
    </ul>
  );
}

/** The scope, spelled out. Each dimension says what "empty" means, because
 *  "Roles: —" and "Roles: reviewer" have opposite consequences and a blank cell
 *  reads like missing data rather than "any role". */
export function ScopeTable({ scope }: { scope: SkillScope }) {
  const rows: { label: string; values?: string[] }[] = [
    { label: t('Projects'), values: scope.projectIds },
    { label: t('Repositories'), values: scope.repositoryPatterns },
    { label: t('Roles'), values: scope.roles },
    { label: t('Task types'), values: scope.taskTypes },
    { label: t('Agents'), values: scope.agentTypes },
  ];
  return (
    <dl className="skill-scope-table">
      {rows.map((row) => (
        <div key={row.label}>
          <dt>{row.label}</dt>
          <dd>
            {row.values && row.values.length > 0 ? (
              row.values.map((value) => (
                <span className="skill-scope-chip" key={value}>
                  {value}
                </span>
              ))
            ) : (
              <span className="skill-muted">{t('any')}</span>
            )}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** The banner over a draft editor.
 *
 *  It is not reassurance, it is the answer to the question an author actually
 *  has while typing: is anything reading this yet? Nothing is, until a reviewer
 *  publishes it — and saying so is what stops somebody from treating the
 *  editor as a live control. */
export function DraftSafetyBanner({ version }: { version: SkillVersion }) {
  if (version.status === 'draft')
    return (
      <div className="skill-banner skill-banner-draft">
        {t('Draft content is never injected into agents. Nothing reads this until a reviewer publishes it.')}
      </div>
    );
  if (version.status === 'pending')
    return (
      <div className="skill-banner skill-banner-pending">
        {t('Waiting for review. The content is frozen so a reviewer decides on exactly what they read.')}
      </div>
    );
  if (version.status === 'approved')
    return (
      <div className="skill-banner skill-banner-approved">
        {t('Published and read-only. To change anything, create a new version.')}
      </div>
    );
  return (
    <div className="skill-banner skill-banner-deprecated">
      {t('Deprecated. It is no longer selected for new work; the history stays readable.')}
    </div>
  );
}

/** What an agent can be shown from a list entry, at a glance. */
export function describeSkillState(entry: SkillListEntry): SkillStatus {
  if (entry.currentVersionId) return 'approved';
  if (entry.pendingVersion) return 'pending';
  if (entry.lifecycleStatus === 'deprecated') return 'deprecated';
  return 'draft';
}
