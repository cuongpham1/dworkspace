import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { t } from '../i18n';
import { toast } from '../toast';
import type { SkillDetail, SkillReviewEntry, SkillVersion } from '../types';
import Logo from '../Logo';
import SkillDetailScreen from './SkillDetail';
import SkillEditor from './SkillEditor';
import { SkillReviewQueue, SkillReviewScreen } from './SkillReview';
import SkillsLibrary from './SkillsLibrary';

// The Skills section, and the only place that knows its URLs.
//
// It keeps the routing out of App.tsx, which already carries the document
// application: one entry point in there (`/skills…` → this), and everything
// below it decided here. The screens themselves take callbacks and know nothing
// about the address bar, so any of them can be rendered somewhere else later
// without unpicking a router.
//
// Routes:
//   /skills                                the library
//   /skills/review                         the review queue
//   /skills/<id>                           the detail, tabbed
//   /skills/<id>/edit/<versionId>          the authoring page
//   /skills/<id>/review/<version>          the diff a reviewer decides on

export type SkillsRoute =
  | { kind: 'library' }
  | { kind: 'queue' }
  | { kind: 'detail'; skillId: string }
  | { kind: 'edit'; skillId: string; versionId: string }
  | { kind: 'review'; skillId: string; version: number };

/** Parse the current location. Returns null when this is not a skills URL, so
 *  App.tsx can ask one question and render the rest of the application if the
 *  answer is no. */
export function skillsRouteFromLocation(): SkillsRoute | null {
  const path = window.location.pathname;
  if (path === '/skills' || path === '/skills/') return { kind: 'library' };
  if (path === '/skills/review') return { kind: 'queue' };
  const edit = path.match(/^\/skills\/([0-9a-f]+)\/edit\/([0-9a-f]+)$/);
  if (edit) return { kind: 'edit', skillId: edit[1], versionId: edit[2] };
  const review = path.match(/^\/skills\/([0-9a-f]+)\/review\/(\d+)$/);
  if (review) return { kind: 'review', skillId: review[1], version: Number(review[2]) };
  const detail = path.match(/^\/skills\/([0-9a-f]+)$/);
  if (detail) return { kind: 'detail', skillId: detail[1] };
  return null;
}

const go = (path: string) => {
  window.history.pushState(null, '', path);
  window.dispatchEvent(new PopStateEvent('popstate'));
};

export default function SkillsApp({
  route,
  workspaceId,
  onLeave,
}: {
  route: SkillsRoute;
  workspaceId: string;
  onLeave: () => void;
}) {
  const [detail, setDetail] = useState<SkillDetail | null>(null);
  const [queue, setQueue] = useState<SkillReviewEntry[]>([]);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const skillId = 'skillId' in route ? route.skillId : '';

  const loadDetail = useCallback(async () => {
    if (!skillId) {
      setDetail(null);
      return;
    }
    setError('');
    try {
      setDetail(await api.getSkill(skillId));
    } catch (err) {
      setDetail(null);
      setError(err instanceof Error ? err.message : t('This skill could not be loaded.'));
    }
  }, [skillId]);

  useEffect(() => {
    void loadDetail();
  }, [loadDetail]);

  // The pending count feeds the library's Review-queue badge. Loaded here so
  // both the library and the queue screen agree on it.
  const loadQueue = useCallback(async () => {
    try {
      setQueue(await api.skillReviewQueue(workspaceId));
    } catch {
      // A failure here costs a badge, not a screen. The queue screen itself
      // reports its own errors properly; suppressing this one keeps a
      // permission quirk from covering the library in a toast.
      setQueue([]);
    }
  }, [workspaceId]);

  useEffect(() => {
    void loadQueue();
  }, [loadQueue]);

  /** Create a skill and go straight to its editor. A "New skill" that opens an
   *  empty form and only creates the row on save leaves somebody with nowhere
   *  to save to when the first request fails. */
  const createSkill = async () => {
    setCreating(true);
    try {
      const created = await api.createSkill({
        workspaceId,
        name: t('Untitled skill'),
        deliveryMode: 'remote',
      });
      if (created.draft) go(`/skills/${created.skill.id}/edit/${created.draft.id}`);
      else go(`/skills/${created.skill.id}`);
    } catch (err) {
      toast(err instanceof Error ? err.message : t('The skill could not be created.'));
    } finally {
      setCreating(false);
    }
  };

  if (route.kind === 'library') {
    return (
      <SkillsLibrary
        workspaceId={workspaceId}
        pendingCount={queue.length}
        onOpen={(id) => go(`/skills/${id}`)}
        onNew={() => void createSkill()}
        onOpenQueue={() => go('/skills/review')}
      />
    );
  }

  if (route.kind === 'queue') {
    return (
      <SkillReviewQueue
        workspaceId={workspaceId}
        onOpen={(id, version) => go(`/skills/${id}/review/${version}`)}
        onBack={() => go('/skills')}
      />
    );
  }

  if (error) {
    return (
      <div className="skills-page">
        <div className="skills-state skills-state-error">
          <p>{error}</p>
          <button className="btn" onClick={() => go('/skills')}>
            {t('Back to Skills')}
          </button>
        </div>
      </div>
    );
  }

  if (!detail) {
    return (
      <div className="app-loading">
        <Logo size={40} />
      </div>
    );
  }

  if (route.kind === 'edit') {
    const version =
      detail.versions.find((candidate) => candidate.id === route.versionId) ?? detail.draft;
    if (!version) {
      return (
        <div className="skills-page">
          <div className="skills-state">
            <h2>{t('That version is gone')}</h2>
            <p>{t('It may have been submitted or published in the meantime.')}</p>
            <button className="btn" onClick={() => go(`/skills/${detail.skill.id}`)}>
              {t('Back to the skill')}
            </button>
          </div>
        </div>
      );
    }
    return (
      <SkillEditor
        detail={detail}
        version={version}
        onBack={() => go(`/skills/${detail.skill.id}`)}
        onSaved={setDetail}
        onSubmitted={() => {
          void loadQueue();
          go(`/skills/${detail.skill.id}`);
        }}
      />
    );
  }

  if (route.kind === 'review') {
    const version = detail.versions.find((candidate) => candidate.version === route.version);
    const entry = queue.find(
      (candidate) => candidate.skill.id === detail.skill.id && candidate.version.version === route.version,
    );
    if (!version) {
      return (
        <div className="skills-page">
          <div className="skills-state">
            <h2>{t('That version is gone')}</h2>
            <button className="btn" onClick={() => go(`/skills/${detail.skill.id}`)}>
              {t('Back to the skill')}
            </button>
          </div>
        </div>
      );
    }
    // The queue is the source of the risk flags and the previous version to
    // diff against — both computed on the server. When the version is no longer
    // pending it is not in the queue, so a minimal entry is built and the
    // screen renders read-only: somebody following a stale link sees the
    // version rather than an error.
    const resolved: SkillReviewEntry =
      entry ??
      {
        skill: detail.skill,
        version,
        previous: detail.versions.find(
          (candidate) =>
            candidate.version < version.version &&
            (candidate.status === 'approved' || candidate.status === 'deprecated'),
        ),
        risks: [],
        canReview: detail.canReview && version.status === 'pending',
        firstPublish: !detail.skill.currentVersionId,
      };
    return (
      <SkillReviewScreen
        entry={resolved}
        onBack={() => go('/skills/review')}
        onDecided={() => {
          void loadQueue();
          void loadDetail();
          go(`/skills/${detail.skill.id}`);
        }}
      />
    );
  }

  return (
    <SkillDetailScreen
      detail={detail}
      onBack={() => {
        onLeave();
        go('/skills');
      }}
      onEdit={(version) => go(`/skills/${detail.skill.id}/edit/${version.id}`)}
      onReview={(version) => go(`/skills/${detail.skill.id}/review/${version}`)}
      onReload={() => {
        void loadDetail();
        void loadQueue();
      }}
    />
  );
}

/** Exported so the sidebar can start a skill without duplicating the route
 *  strings. */
export function openSkills() {
  go('/skills');
}

export type { SkillVersion };
