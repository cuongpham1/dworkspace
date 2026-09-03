import type { Prefs } from './i18n';

export interface PageMeta {
  id: string;
  parentId: string | null;
  title: string;
  icon: string;
  cover: string;
  position: number;
  updatedAt: string;
  trashed: boolean;
  type: 'doc' | 'collection';
  props: Record<string, unknown>;
  workspaceId: string;
  ownerId: string;
  visibility: 'workspace' | 'private';
  isTemplate: boolean;
  tags: string[];
  description: string;
  snippet: string; // plain-text preview for the notes list (derived server-side)
  thumb: string; // first image URL, '' if none
}

export interface Workspace {
  id: string;
  name: string;
  role: 'admin' | 'member' | 'viewer';
  icon: string;
  image: string;
  /** This account's own area — it belongs to the account, not the instance. */
  personal?: boolean;
  /** Every newly created account joins automatically (only the owner sets this). */
  autoJoin?: boolean;
  /** Working conventions the admin wrote down — agents get them over MCP, members read them here. */
  rules?: string;
  /** A pending rules draft (usually from an agent) — inert until an admin applies it. */
  // Empty means the default in both cases — 'open' and 'split'. There is no
  // third state, so nothing has to handle one.
  agentAccess?: string;
  treeMode?: string;
  rulesProposal?: string;
  rulesProposalBy?: string;
  rulesProposalAt?: string;
}

/** One entry of the file index (W125): a file plus the page carrying it. */
export interface DworkspaceFile {
  /** Stored name — the segment behind /files/. */
  name: string;
  displayName: string;
  ext: string;
  size: number;
  createdAt: string;
  pageId: string;
  pageTitle: string;
  workspaceId: string;
}

export interface AuditEntry {
  id: number;
  createdAt: string;
  actorType: 'human' | 'agent';
  actorName: string;
  action: string;
  pageId: string;
  detail: string;
  /** The server offers a take-back only where it recorded a before/after. */
  revertible?: boolean;
  pageTitle?: string;
}

export interface Page extends PageMeta {
  content: unknown[];
  createdAt: string;
}

export interface SearchResult {
  id: string;
  title: string;
  icon: string;
  snippet: string;
}

export interface Backlink {
  id: string;
  title: string;
  icon: string;
}

export interface User {
  id: string;
  email: string;
  name: string;
  color: string;
  avatar: string;
  isAdmin: boolean;
  /** Instanzrolle: owner betreibt die Instanz, admin verwaltet Menschen. */
  orgRole?: 'owner' | 'admin' | 'member';
  /** Deactivated: no sign-in, but everything stays attributable. */
  disabled?: boolean;
}

export interface Me {
  setupRequired: boolean;
  authenticated: boolean;
  user: User | null;
  version: string;
  // May non-admins create workspaces of their own? (instance setting, W97)
  allowUserWorkspaces?: boolean;
  /** Language and time preferences of this account (W112). Empty fields mean
   *  automatic — see server/prefs.go. */
  prefs?: Prefs;
}

/** An outbound webhook: dworkspace calls this URL when something happens, so
 *  other tools do not have to keep asking. The secret is returned once, when
 *  it is created, and never again. */
export interface Webhook {
  id: string;
  url: string;
  events: string;
  active: boolean;
  createdAt: string;
  lastStatus: string;
  lastAt: string;
  secret?: string;
}

export interface Revision {
  id: string;
  createdAt: string;
  authorName: string;
  title: string;
}

export interface PageChangeProposal {
  id: string;
  pageId?: string;
  kind: 'create' | 'edit';
  targetParentId?: string;
  targetWorkspaceId?: string;
  baseHash: string;
  proposedContent: unknown[];
  proposedTitle: string;
  proposedType: 'doc' | 'collection';
  proposedIcon: string;
  proposedCover: string;
  proposedDescription: string;
  proposedTags: string[];
  proposedProps: Record<string, unknown>;
  creatorId: string;
  creatorType: 'human' | 'agent';
  creatorName: string;
  createdAt: string;
  updatedAt: string;
  status: 'pending' | 'published' | 'rejected' | 'superseded';
  summary: string;
  factReview: FactReview;
  relatedCandidates: RelatedCandidate[];
  selectedRelatedIds: string[];
  originalSnapshot?: unknown;
  lastHumanEditor?: string;
  lastHumanEditedAt?: string;
  publishedAt?: string;
  publishedBy?: string;
  rejectedAt?: string;
  rejectedBy?: string;
  canEdit?: boolean;
}

export interface FactItem {
  id?: string;
  constraintId?: string;
  label: string;
  value: string;
  category: 'provided' | 'derived' | 'assumption' | 'missing' | 'unsupported' | string;
  source?: string;
  evidence?: string;
  confirmed?: boolean;
  verified?: boolean;
  claimedCategory?: string;
}

export interface FactConstraint {
  id: string;
  label: string;
  required: number;
}

export interface FactGap {
  id: string;
  constraintId: string;
  label: string;
  message: string;
  category: 'missing' | string;
}

export interface FactReview {
  trusted: boolean;
  validation: 'deterministic' | 'unknown' | string;
  facts: FactItem[];
  constraints: FactConstraint[];
  provided: number;
  required: number;
  gaps: FactGap[];
}

export interface RelatedCandidate {
  pageId: string;
  title: string;
  snippet?: string;
  rationale: string;
  rank: number;
  selected: boolean;
  dismissed?: boolean;
}

export interface Comment {
  id: string;
  blockId: string;
  authorId: string;
  authorName: string;
  body: string;
  createdAt: string;
  authorColor?: string;
  authorAvatar?: string;
  resolvedAt: string | null;
}

// One entry of a page's raw trail. No resolvedAt, no editedAt and no author id
// to check against: nothing about it can change after it is written, which is
// the entire point (server/notelog.go).
export interface PageNote {
  id: string;
  body: string;
  author: string; // the account — verified
  agent?: string; // what an agent called itself — a claim
  label?: string;
  createdAt: string;
}

export interface ApiToken {
  id: string;
  name: string;
  scope: 'read' | 'write';
  workspaces: string[]; // empty = all the user's workspaces
  createdAt: string;
  lastUsedAt: string | null;
  lastUsedIp: string;
}

export type PropType =
  | 'text'
  | 'number'
  | 'select'
  | 'multiselect'
  | 'date'
  | 'checkbox'
  | 'checklist'
  | 'url'
  | 'person'
  | 'relation'
  | 'backrelation'
  | 'rollup'
  | 'formula'
  | 'lastActivity';

/** One sub-task of a checklist property. Progress is derived from these — a
    stored percentage would be a second truth to keep in sync. */
export interface ChecklistItem {
  id: string;
  text: string;
  done: boolean;
}

export interface PropOption {
  id: string;
  name: string;
  color: string;
}

export interface PropDef {
  id: string;
  name: string;
  type: PropType;
  options?: PropOption[];
  // relation: which collection this links to
  relationCollection?: string;
  // rollup: aggregate a target property over a relation
  rollupRelation?: string;
  rollupTarget?: string;
  rollupAgg?: 'sum' | 'count' | 'avg' | 'min' | 'max' | 'percent';
  // rollup: count/sum only the related rows meeting this condition — the
  // difference between "how many tasks" and "how many are done".
  rollupWhereProp?: string;
  rollupWhereOp?: 'is' | 'is_not' | 'is_empty' | 'is_not_empty' | 'contains';
  rollupWhereValue?: string;
  // Several values, for is / is_not. "Open" means neither done NOR discarded,
  // and one comparison cannot say that. Empty falls back to rollupWhereValue,
  // so conditions written before this keep their meaning exactly.
  rollupWhereValues?: string[];
  // backrelation: the other side of a relation someone else declared. Computed
  // at read time, never stored — see backrelationIDs in derived.go.
  backrelationCollection?: string;
  backrelationProp?: string;
  // formula: expression over other props, {propId} references
  formula?: string;
  // number/rollup/formula: render a numeric value as a plain number (default),
  // a progress bar, or a ring. numberMax is the value that = 100% (default 100).
  numberDisplay?: 'plain' | 'bar' | 'ring';
  numberMax?: number;
}

export type FilterOp =
  | 'is'
  | 'is_not'
  | 'contains'
  | 'gt'
  | 'lt'
  | 'between'
  | 'is_empty'
  | 'is_not_empty';

export interface Filter {
  property: string;
  op?: FilterOp; // default 'is'; legacy empty value = is_not_empty
  value: string;
  /** Several values for is/is_not — "class is none of A, H" as ONE condition
   *  rather than two rows that happen to sit next to each other. When present it
   *  replaces `value`; a filter never carries both. */
  values?: string[];
  /** Upper bound of `between`, inclusive. A range is value…value2. */
  value2?: string;
}

export interface Sort {
  property: string;
  dir: 'asc' | 'desc';
}

export interface ViewDef {
  id: string;
  name: string;
  type: 'table' | 'board' | 'list' | 'gallery' | 'calendar' | 'form' | 'timeline';
  groupBy?: string;
  dateProp?: string; // calendar/timeline view: date property (timeline: start)
  endDateProp?: string; // timeline view: optional end-date property (else 1-day bar)
  hidden?: string[]; // property ids hidden in this view
  filters?: Filter[];
  sort?: Sort | null;
  formTitle?: string; // form view: heading above the form
  formDesc?: string; // form view: description under the heading
  formSubmit?: string; // form view: submit-button label
  subItemProp?: string; // table view: a self-relation prop whose value = child rows (renders a tree)
  // table view: pixel widths the reader dragged, by property id ('__title' for
  // the name column). A column that is not in here sizes itself to its content;
  // only the ones somebody deliberately set are pinned. Stored on the VIEW, so
  // two views of the same database can be laid out for different jobs.
  colWidths?: Record<string, number>;
}

export interface CollectionConfig {
  schema: PropDef[];
  views: ViewDef[];
}

// Public form config served (unauthenticated) at /api/public/form/{token} —
// only the fillable field defs, never rows or the rest of the workspace.
export interface PublicFormConfig {
  title: string;
  icon: string;
  formTitle?: string;
  formDesc?: string;
  formSubmit?: string;
  schema: PropDef[];
}

// One item on the blueprint shelf (see server/library.go). Everything from
// `databases` down is READ OUT OF THE BLUEPRINT by the server, never written
// beside it — the preview built from this cannot promise something the
// blueprint does not contain.
export interface BlueprintEntry {
  id: string;
  title: string;
  tagline: string;
  icon: string;
  accent: string;
  tags: string[];
  price: string; // empty = free
  source: string; // 'built-in'
  databases: BlueprintDatabase[];
  rules: string;
}

export interface BlueprintDatabase {
  title: string;
  icon: string;
  description: string;
  props: BlueprintProp[];
  views: { name: string; type: string }[];
}

export interface BlueprintProp {
  name: string;
  type: string;
  options?: { name: string; color?: string }[];
}

// A connection an agent signed in for (server/oauth_provider.go). The counterpart
// to an API token, except it expires on its own and can be ended from here.
export interface OAuthGrant {
  id: string;
  clientName: string;
  scope: string;
  workspaces: string;
  createdAt: string;
  lastUsedAt: string | null;
  lastUsedIp: string;
}

/** What GET /api/update answers. Admins only — a member's browser gets a 403
 *  and the banner renders nothing. `available` is already the comparison's
 *  verdict, so the client never has to know how versions are ordered. */
export type UpdateInfo = {
  available: boolean;
  current: string;
  latest?: string;
  url?: string;
  checkedAt?: string;
  enabled: boolean;
};

// ---- the skill control plane (server/skills_model.go) ----
//
// Two things are worth knowing before reading these.
//
// A skill's DELIVERY MODE says what it is: `remote` is behaviour — instructions
// VUS hands an agent for the current task, no install anywhere. `client` is
// capability — a package with scripts or assets that has to exist in the
// runtime. One library, one review path, two ways out.
//
// A skill's STATUS is trust. Only an `approved` version is ever given to an
// agent, and only through the trusted path. A draft is inert, and the editor
// says so out loud, because "I wrote it down" and "the workspace agreed to it"
// are different things and confusing them is how a control plane becomes a
// liability.

export type SkillDelivery = 'remote' | 'client';
export type SkillStatus = 'draft' | 'pending' | 'approved' | 'deprecated';

export interface SkillDependency {
  type: 'client_skill' | 'capability' | 'tool' | 'connector';
  id: string;
  versionConstraint?: string;
  required: boolean;
  fallback?: string;
}

/** A pointer, never inlined content. Whatever a reference points at is fetched
 *  through the ordinary read path and stays untrusted. */
export interface SkillReference {
  kind: 'page' | 'url' | 'note';
  target: string;
  label?: string;
}

/** Where a version may be selected. Every list is "empty means anywhere"; a
 *  non-empty list is a restriction, and a restriction the runtime cannot answer
 *  counts as a miss. */
export interface SkillScope {
  workspaceId: string;
  projectIds?: string[];
  repositoryPatterns?: string[];
  roles?: string[];
  taskTypes?: string[];
  agentTypes?: string[];
}

export interface SkillPackedFile {
  name: string;
  content: string;
}

export interface SkillPackageMetadata {
  entrypoint?: string;
  scripts?: SkillPackedFile[];
  assets?: SkillPackedFile[];
}

export interface Skill {
  id: string;
  workspaceId: string;
  slug: string;
  name: string;
  description: string;
  ownerId: string;
  ownerName?: string;
  lifecycleStatus: SkillStatus;
  /** Empty when nothing is live: never approved, or the published version was
   *  deprecated with no replacement. */
  currentVersionId?: string;
  currentVersion?: number;
  createdAt: string;
  updatedAt: string;
}

export interface SkillVersion {
  id: string;
  skillId: string;
  version: number;
  deliveryMode: SkillDelivery;
  status: SkillStatus;
  description: string;
  triggers: string[];
  instructions: string;
  references: SkillReference[];
  dependencies: SkillDependency[];
  scope: SkillScope;
  packageMetadata: SkillPackageMetadata;
  /** What a reviewer said when they sent it back. Shown in the editor. */
  reviewNote?: string;
  createdBy: string;
  createdByName?: string;
  approvedBy?: string;
  approvedByName?: string;
  createdAt: string;
  updatedAt: string;
  submittedAt?: string;
  approvedAt?: string;
  deprecatedAt?: string;
  contentHash: string;
  published: boolean;
}

export interface SkillListEntry extends Skill {
  delivery: SkillDelivery;
  draftVersion?: number;
  pendingVersion?: number;
  triggers: string[];
  scopeSummary: string[];
  dependencyIssues: string[];
  versionCount: number;
  canReview: boolean;
  canAuthor: boolean;
}

export interface SkillDetail {
  skill: Skill;
  versions: SkillVersion[];
  current?: SkillVersion;
  draft?: SkillVersion;
  pending?: SkillVersion;
  canReview: boolean;
  canAuthor: boolean;
}

export interface SkillReviewEntry {
  skill: Skill;
  version: SkillVersion;
  previous?: SkillVersion;
  /** Deterministic flags, computed by comparing two stored versions — never a
   *  model's guess. serverErrors-style codes so the interface can translate. */
  risks: string[];
  canReview: boolean;
  firstPublish: boolean;
}

export interface SkillScoreReason {
  code: string;
  label: string;
  points: number;
}

export interface SkillMissingDependency {
  type: string;
  id: string;
  versionConstraint?: string;
  required: boolean;
  fallback?: string;
  detail: string;
}

export interface SkillResolvedEntry {
  skillId: string;
  slug: string;
  name: string;
  description: string;
  version: number;
  versionId: string;
  deliveryMode: SkillDelivery;
  contentHash: string;
  score: number;
  reasons: SkillScoreReason[];
  missingDependencies?: SkillMissingDependency[];
  dependencies?: SkillDependency[];
}

export interface SkillExcludedEntry {
  skillId: string;
  slug: string;
  name: string;
  version: number;
  deliveryMode: SkillDelivery;
  reason: string;
  reasonCode: string;
  missingDependencies?: SkillMissingDependency[];
}

/** What POST /api/skills/resolve-test answers — the SAME shape MCP's
 *  skill_resolve returns, because it is the same function. The scoring is never
 *  reimplemented here. */
export interface SkillResolveResult {
  selected: SkillResolvedEntry[];
  excluded: SkillExcludedEntry[];
  considered: number;
}

export interface SkillAuditEntry {
  id: number;
  createdAt: string;
  workspaceId: string;
  skillId: string;
  skillVersionId?: string;
  versionNumber?: number;
  deliveryMode?: SkillDelivery;
  action: string;
  agentType?: string;
  projectRef?: string;
  /** A digest of the task, never the task text. */
  taskRef?: string;
  resolutionReason?: string;
  missingDependencies?: string[];
  contentHash?: string;
  actorUserId?: string;
  actorName?: string;
  actorType?: string;
}

export interface SkillPackageInfo {
  manifest: {
    skill_id: string;
    slug: string;
    name: string;
    version: number;
    content_hash: string;
    delivery_mode: SkillDelivery;
    description: string;
    dependencies: SkillDependency[];
    entrypoint?: string;
  };
  files: { name: string; size: number }[];
  archiveHash: string;
  sizeBytes: number;
  downloadUrl?: string;
  fileName: string;
}

/** The authoring form's payload. Every field optional: a PATCH that mentions
 *  only the instructions must leave the scope alone rather than clearing it. */
export interface SkillDraftPayload {
  workspaceId?: string;
  name?: string;
  slug?: string;
  description?: string;
  deliveryMode?: SkillDelivery;
  instructions?: string;
  triggers?: string[];
  references?: SkillReference[];
  dependencies?: SkillDependency[];
  scope?: SkillScope;
  packageMetadata?: SkillPackageMetadata;
  updatedAt?: string;
  fromVersion?: string;
  note?: string;
}

export interface SkillResolveInput {
  workspaceId?: string;
  task: string;
  project?: string;
  repository?: string;
  role?: string;
  taskType?: string;
  agent?: string;
  capabilities?: string[];
  installedClientSkills?: { id: string; version?: string }[];
  limit?: number;
}
