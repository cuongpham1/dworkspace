// Asserts the skill interface's behaviour (src/components/Skill*.tsx).
//
//   node scripts/check-skills.mjs
//
// Same shape as check-cardlayout.mjs and for the same reason: there is no test
// framework here, esbuild is present as part of vite, so the pure modules are
// bundled once and imported. What cannot be reached that way — a rule about how
// a component is WRITTEN rather than what a function returns — is checked
// against the source text, which is how check-i18n.mjs and check-treemode.mjs
// work.
//
// Three classes of thing are checked, in descending order of how badly it would
// hurt to get them wrong:
//
//  1. THE RESOLVER IS NOT REIMPLEMENTED HERE. The Resolver test screen exists
//     to answer "will an agent actually be given this skill". A second scoring
//     implementation in TypeScript would agree with the server on the day it
//     was written and drift afterwards, and a screen that is confidently wrong
//     about that is worse than no screen. So: no scoring weights, no ranking,
//     no lifecycle decisions in the frontend.
//  2. THE FORM DOES NOT EAT SOMEBODY'S WORK. A failed save, submit or approval
//     must leave every word the author typed exactly where it was.
//  3. Trust state is rendered from the SERVER's answer, and Edit is never
//     offered on something the server would refuse to edit.

import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const web = join(here, '..');
const src = join(web, 'src');
const tmp = mkdtempSync(join(tmpdir(), 'dworkspace-skills-'));

const out = [];
const check = (name, got, want) => out.push({ name, got: String(got), want: String(want) });
const read = (rel) => readFileSync(join(src, rel), 'utf8');

/** Strip comments so a rule cannot be satisfied — or broken — by prose. The
 *  scanner walks string state rather than pattern-matching, for the reason
 *  check-i18n.mjs documents: `accept="image/*"` opens a comment that runs on
 *  for hundreds of characters and swallows real code. */
function stripComments(text) {
  const buf = text.split('');
  let i = 0;
  let quote = null;
  while (i < text.length) {
    const c = text[i];
    const d = text[i + 1];
    if (quote) {
      if (c === '\\') i += 2;
      else {
        if (c === quote) quote = null;
        i++;
      }
      continue;
    }
    if (c === "'" || c === '"' || c === '`') {
      quote = c;
      i++;
      continue;
    }
    if (c === '/' && d === '/') {
      while (i < text.length && text[i] !== '\n') buf[i++] = ' ';
      continue;
    }
    if (c === '/' && d === '*') {
      const end = text.indexOf('*/', i + 2);
      const stop = end === -1 ? text.length : end + 2;
      for (; i < stop; i++) if (buf[i] !== '\n') buf[i] = ' ';
      continue;
    }
    i++;
  }
  return buf.join('');
}

function bundle(entry, name) {
  const file = join(tmp, name);
  try {
    execFileSync(
      join(web, 'node_modules/esbuild/bin/esbuild'),
      [
        join(src, entry),
        '--bundle',
        '--format=esm',
        '--jsx=automatic',
        '--loader:.json=json',
        // i18n.ts discovers the locale catalogs with Vite's import.meta.glob,
        // which esbuild does not implement and node does not have. The stub
        // returns no catalogs, which is exactly right here: t() then falls
        // through to its English source string, and these assertions are about
        // the logic rather than about the German.
        '--define:import.meta.glob=__localeGlobStub',
        '--banner:js=const __localeGlobStub = () => ({});',
        '--outfile=' + file,
      ],
      { stdio: ['ignore', 'ignore', 'pipe'] },
    );
  } catch (e) {
    console.error(String(e.stderr ?? e));
    process.exit(1);
  }
  return file;
}

try {
  // ---- 1. The pure display logic ----------------------------------------

  const bits = await import(bundle('components/skillBits.tsx', 'skillBits.mjs'));

  // Every lifecycle state has a human label, and they are all different. Two
  // states reading the same word would be worse than an untranslated one: the
  // reader would conclude the distinction does not matter.
  const labels = ['draft', 'pending', 'approved', 'deprecated'].map((s) => bits.statusLabel(s));
  check('every status has a label', labels.every((l) => l && l.length > 2), true);
  check('the four status labels are distinct', new Set(labels).size, 4);

  // The library row's status: what somebody scanning the list needs, which is
  // the skill's TRUST state rather than its newest activity. A skill with v4
  // published and v5 in draft is Approved — an agent can use it right now.
  const entry = (over) => ({
    lifecycleStatus: 'draft',
    currentVersionId: '',
    draftVersion: 0,
    pendingVersion: 0,
    ...over,
  });
  check('published + draft reads as approved',
    bits.describeSkillState(entry({ currentVersionId: 'v', draftVersion: 5, lifecycleStatus: 'approved' })),
    'approved');
  check('nothing published, one pending reads as pending',
    bits.describeSkillState(entry({ pendingVersion: 1, lifecycleStatus: 'pending' })), 'pending');
  check('deprecated with no replacement reads as deprecated',
    bits.describeSkillState(entry({ lifecycleStatus: 'deprecated' })), 'deprecated');
  check('a fresh skill reads as draft',
    bits.describeSkillState(entry({ draftVersion: 1 })), 'draft');

  // Risk flags. Each one is translated, the counted ones carry their number,
  // and an unknown flag falls through to its code rather than to "undefined" —
  // a new server-side flag must degrade to something readable, not to a gap.
  check('a counted risk flag carries its number',
    bits.riskLabel('dependencies_added:2').includes('2'), true);
  check('an unknown flag falls back to its code', bits.riskLabel('brand_new_flag'), 'brand_new_flag');
  check('a known flag is not returned raw', bits.riskLabel('scope_expanded') === 'scope_expanded', false);

  // The high-impact set drives the extra confirmation before publishing. These
  // five are the changes that alter what the skill IS; everything else must NOT
  // demand a confirmation, because a dialog on every action is a dialog people
  // click through.
  for (const flag of [
    'first_remote_publish',
    'client_to_remote',
    'workspace_wide_scope',
    'required_dependency_removed',
    'instructions_changed',
  ]) {
    check(`${flag} asks for confirmation`, bits.highImpactRisks([flag]).length, 1);
  }
  for (const flag of ['description_changed', 'triggers_changed', 'dependencies_added:1', 'first_publish']) {
    check(`${flag} does not ask for confirmation`, bits.highImpactRisks([flag]).length, 0);
  }
  check('a counted high-impact flag still matches on its code',
    bits.highImpactRisks(['dependencies_removed:3']).length, 0);

  // Hashes are shown abbreviated and never padded: a 12-character prefix is
  // comparable by eye, and a short hash must not be stretched into looking like
  // a full one.
  check('a long hash is abbreviated', bits.shortHash('a'.repeat(64)).length, 12);
  check('a short hash is left alone', bits.shortHash('abc'), 'abc');

  // ---- 2. The routes -----------------------------------------------------

  const routes = await import(bundle('components/SkillsApp.tsx', 'skillsApp.mjs'));
  const at = (pathname) => {
    globalThis.window.location = { pathname, search: '' };
    return routes.skillsRouteFromLocation();
  };
  globalThis.window = { location: { pathname: '/', search: '' } };

  check('/skills is the library', at('/skills')?.kind, 'library');
  check('/skills/ is the library too', at('/skills/')?.kind, 'library');
  check('/skills/review is the queue', at('/skills/review')?.kind, 'queue');
  check('a skill id opens the detail', at('/skills/abc123')?.kind, 'detail');
  check('the detail carries the id', at('/skills/abc123')?.skillId, 'abc123');
  check('the editor route parses', at('/skills/abc123/edit/def456')?.kind, 'edit');
  check('the editor carries the version id', at('/skills/abc123/edit/def456')?.versionId, 'def456');
  check('the review route parses', at('/skills/abc123/review/4')?.kind, 'review');
  check('the review route carries a NUMBER', at('/skills/abc123/review/4')?.version, 4);
  // Everything else must return null, or SkillsApp would swallow the document
  // application: App.tsx asks this one question and renders the rest of the app
  // when the answer is no.
  for (const other of ['/', '/p/abc123', '/skillsomething', '/skills/abc/nonsense', '/review/proposals/abc']) {
    check(`${other} is not a skills route`, at(other), 'null');
  }
  // A path segment that is not a hex id is not a skill: the ids are hex, and
  // treating any word as one would make /skills/review-queue load a skill.
  check('a non-hex segment is not a skill id', at('/skills/review-queue'), 'null');

  // ---- 3. The rules about how the components are written -----------------

  const files = {
    library: read('components/SkillsLibrary.tsx'),
    detail: read('components/SkillDetail.tsx'),
    editor: read('components/SkillEditor.tsx'),
    review: read('components/SkillReview.tsx'),
    resolver: read('components/SkillResolverTest.tsx'),
    bits: read('components/skillBits.tsx'),
    shell: read('components/SkillsApp.tsx'),
  };
  const code = Object.fromEntries(Object.entries(files).map(([k, v]) => [k, stripComments(v)]));
  const all = Object.values(code).join('\n');

  // (a) The resolver is not reimplemented. The weights are the tell: if any of
  // them appears in the frontend, somebody has started scoring here.
  for (const weight of ['100', '40', '20', '10']) {
    const scoring = new RegExp(`(score|points|weight|rank)\\s*[+*=]*=?\\s*${weight}\\b`, 'i');
    check(`no scoring weight ${weight} in the frontend`, scoring.test(all), false);
  }
  check('the resolver screen does no arithmetic on scores',
    /(score|points)\s*[+\-*/]\s*(score|points|\d)/.test(code.resolver), false);
  check('the resolver screen calls the backend', /api\.resolveSkills\(/.test(code.resolver), true);
  check('the resolver screen renders the reasons it was given',
    /hit\.reasons\.map/.test(code.resolver), true);

  // (b) No lifecycle decision is made locally. The frontend may READ a status
  // to decide what to render; it must not compute the next one, or the screen
  // could show "published" before the publish happened.
  check('the frontend never invents a status',
    /(setStatus|status)\s*=\s*['"](approved|pending|deprecated)['"]/.test(all), false);
  check('publishing goes through the API', /api\.approveSkillVersion\(/.test(code.review), true);

  // (c) No component talks to the network except through api.ts. Otherwise the
  // error translation, the 401 handling and the code-based branching are all
  // bypassed for exactly the screens that need them most.
  check('no raw fetch in the skill screens', /\bfetch\s*\(/.test(all), false);
  check('no raw XHR in the skill screens', /XMLHttpRequest/.test(all), false);

  // (d) A failed save keeps the form. Mechanically: nothing in the editor may
  // reset the draft state outside the initial useState, and every catch must
  // land in an error slot rather than in the form's state.
  const editorCatches = code.editor.split(/\bcatch\b/).slice(1);
  check('the editor has catch blocks at all', editorCatches.length > 0, true);
  for (const [i, block] of editorCatches.entries()) {
    const body = block.slice(0, 400);
    check(`editor catch ${i + 1} does not reset the form`, /setDraft\s*\(|draftFrom\s*\(/.test(body), false);
    check(`editor catch ${i + 1} reports the problem`, /setProblems\s*\(/.test(body), true);
  }
  check('the draft state is seeded once, from the server version',
    (code.editor.match(/useState<Draft>/g) ?? []).length, 1);
  // The review screen keeps the reviewer's feedback on a failure too: a
  // paragraph of review notes is exactly as expensive to retype.
  for (const block of code.review.split(/\bcatch\b/).slice(1)) {
    check('the review screen does not clear the note on failure',
      /setNote\s*\(\s*['"]{2}\s*\)/.test(block.slice(0, 300)), false);
  }

  // (e) Unsaved-change protection exists, and it is the browser's own — which
  // fires on a closed tab as well as on navigation, where a React-only guard
  // cannot.
  check('the editor guards unsaved changes', /beforeunload/.test(code.editor), true);

  // (f) Edit is offered only where the server would allow it. A button that
  // leads to a refusal is worse than no button: it teaches that the state
  // badges do not mean anything.
  check('the versions table gates Edit on draft',
    /status === 'draft' && canAuthor/.test(code.detail), true);
  check('the editor disables its fields off-draft',
    /const editable = version\.status === 'draft'/.test(code.editor), true);
  check('the editor actually applies that',
    (code.editor.match(/disabled=\{!editable\}/g) ?? []).length >= 4, true);
  check('Save and Submit are gated on it too',
    (code.editor.match(/disabled=\{busy \|\| !editable\}/g) ?? []).length, 2);

  // (g) The draft-safety banner is rendered where an author types. This is the
  // one piece of copy the PRD names as an acceptance criterion, because "I
  // wrote it down" and "the workspace agreed to it" are different things.
  check('the editor shows the draft-safety banner', /<DraftSafetyBanner/.test(code.editor), true);
  check('the banner says a draft is never injected',
    /never injected into agents/.test(files.bits), true);
  check('the approved banner points at a new version',
    /create a new version/i.test(files.bits), true);

  // (h) A Remote skill never gets Download as its primary action, and a Client
  // one does. Offering an install for something that installs nothing teaches
  // precisely the wrong model of what a Remote skill is.
  check('Download is gated on the client delivery mode',
    /isClient && version\.status === 'approved'/.test(code.detail), true);
  check('the download button lives inside that gate',
    code.detail.indexOf('downloadSkillPackage') > code.detail.indexOf("isClient && version.status === 'approved'"),
    true);

  // (i) The library list must not render instruction bodies. The endpoint does
  // not send them; a screen reaching for them would mean somebody widened the
  // endpoint to suit the screen.
  check('the library never reads instructions', /\.instructions/.test(code.library), false);

  // (j) Both empty states exist and are different, and the failure state is not
  // the same thing as "nothing here" — a permission error rendered as an empty
  // library is how somebody concludes their skills were deleted.
  check('the library has a first-use empty state', /No skills yet/.test(files.library), true);
  check('the library has a filtered empty state', /No skills match these filters/.test(files.library), true);
  check('the library has a failure state', /skills-state-error/.test(code.library), true);
  check('the library has a loading state', /skills-skeleton/.test(code.library), true);
  check('the queue has a clean empty state', /Nothing is waiting for review/.test(files.review), true);

  // (k) The audit screen must not offer to show the task text — there is none
  // to show, and a column promising it would be a lie about what is stored.
  check('the audit table shows the digest, not the prompt', /taskRef/.test(code.detail), false);
  check('and says so', /never stored/.test(files.detail), true);

  // ---- report ------------------------------------------------------------

  let failed = 0;
  for (const row of out) {
    if (row.got !== row.want) {
      failed++;
      console.log(`    FAIL ${row.name}: got ${row.got}, want ${row.want}`);
    }
  }
  console.log(`\n  skill interface: ${out.length - failed} passed, ${failed} failed`);
  if (failed > 0) process.exit(1);
} finally {
  rmSync(tmp, { recursive: true, force: true });
}
