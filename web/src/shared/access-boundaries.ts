/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Access Boundary — shared TypeScript contract (WP C1)
 *
 * Canonical types for the Access Boundary domain. Every frontend component
 * (F1–F7) imports from this module — no page file may define its own
 * response types.
 *
 * Authority:
 *   - constraint-ui-ux-design.md §9.2–§9.8
 *   - constraint-ui-ux-implementation-plan.md §6 (file map)
 *   - Frozen type shapes: fixtures/type-shapes.ts
 *
 * PRODUCT NAMING
 * The engineering object is `AccessConstraint`; the user-facing noun is
 * "access boundary". Types that model the persisted/draft record keep the
 * `AccessConstraint*` name to match the Go API (`pkg/hub`, `pkg/store`);
 * types that model UI-facing projections use `AccessBoundary*`.
 */

/* -------------------------------------------------------------------------- */
/* Shared primitives                                                          */
/* -------------------------------------------------------------------------- */

/** RFC 3339 UTC timestamp. Aliased for self-documentation. */
export type Iso8601 = string;

/**
 * Opaque monotonic version token for a boundary record. Echoed into `If-Match`
 * on every mutation (design §9.5). Never parse, compare ordinally, or
 * increment client-side.
 */
export type BoundaryRevision = string;

/** Opaque cursor. Absent (not `null`, not `''`) means the last page. */
export type PageToken = string;

/** Permission ID as registered in `pkg/hub/permissions.Registry`. */
export type PermissionId = string;

/**
 * Principal kinds usable as a boundary subject.
 *
 * Narrower than the evaluator's principal-kind domain — the boundary subject
 * vocabulary stays at three values.
 */
export type PrincipalType = 'user' | 'agent' | 'group';

/** A resolved principal reference. `displayName` may be redacted to `null`. */
export interface PrincipalRef {
  type: PrincipalType;
  id: string;
  displayName?: string | null;
}

/** Lifecycle status of a principal, used to explain no-effect impact rows. */
export type PrincipalStatus = 'active' | 'suspended' | 'deleted' | 'unknown';

/**
 * Capability envelope. Reuses the existing wire shape verbatim.
 * The UI derives EVERY affordance from `actions`. Never from a role name,
 * never from a permission ID, never from a status value (design §9.8, §3.3).
 */
export interface AccessBoundaryCapabilities {
  actions: AccessBoundaryCapabilityAction[];
}

/** The seven capability actions defined by design §9.8. */
export type AccessBoundaryCapabilityAction =
  | 'read'
  | 'readAudit'
  | 'previewCreate'
  | 'previewTighten'
  | 'previewRelax'
  | 'commit'
  | 'delete';

/**
 * Field-level redaction marker (design §6.5). Redaction must preserve the
 * causal shape of the answer.
 */
export interface RedactionNotice {
  /** Dotted paths, relative to the object carrying the notice. */
  fields: string[];
  reason:
    | 'insufficient_audit_permission'
    | 'insufficient_scope_permission'
    | 'principal_not_visible';
  requiredPermissionIds: PermissionId[];
  message: string;
}

/* -------------------------------------------------------------------------- */
/* 1. ConstraintSubject — tagged union (design §9.2)                          */
/* -------------------------------------------------------------------------- */

/**
 * Who a boundary applies to. Three variants matching the persisted
 * `subject_kind` enum.
 */
export type ConstraintSubject =
  | { kind: 'principal'; principal: { type: PrincipalType; id: string } }
  | { kind: 'group_closure'; groupId: string }
  | { kind: 'all_principals' };

/**
 * Flattened five-way subject vocabulary for pickers and filters. Maps onto
 * `ConstraintSubject` losslessly.
 */
export type SubjectSelection =
  | 'exact_user'
  | 'exact_agent'
  | 'exact_group'
  | 'group_closure'
  | 'all_principals';

export function subjectSelectionOf(subject: ConstraintSubject): SubjectSelection {
  switch (subject.kind) {
    case 'principal':
      return `exact_${subject.principal.type}` as SubjectSelection;
    case 'group_closure':
      return 'group_closure';
    case 'all_principals':
      return 'all_principals';
  }
}

/** Server-resolved display projection of a subject. Never derive labels client-side. */
export type ConstraintSubjectDisplay =
  | {
      kind: 'principal';
      label: string;
      principalType: PrincipalType;
      principalName: string | null;
      resolved: boolean;
    }
  | {
      kind: 'group_closure';
      label: string;
      groupName: string | null;
      resolved: boolean;
      memberCount: number | null;
      directMemberCount?: number | null;
      nestedGroupCount?: number | null;
    }
  | {
      kind: 'all_principals';
      label: string;
      resolved: true;
      memberCount: number | null;
    };

/* -------------------------------------------------------------------------- */
/* 2. ConstraintScope — tagged union (design §9.2)                            */
/* -------------------------------------------------------------------------- */

/**
 * Where a boundary applies. A system boundary applies everywhere; a project
 * boundary applies only within its project.
 */
export type ConstraintScope = { type: 'system' } | { type: 'project'; projectId: string };

export interface ConstraintScopeDisplay {
  type: 'system' | 'project';
  label: string;
  projectName: string | null;
  resolved: boolean;
}

/* -------------------------------------------------------------------------- */
/* 3. AccessConstraintDraft (design §9.2)                                     */
/* -------------------------------------------------------------------------- */

/**
 * The full desired state of a boundary, as authored. Always complete — never
 * a patch. Design §9.5 uses PUT semantics for updates.
 */
export interface AccessConstraintDraft {
  name: string;
  purpose: string;
  subject: ConstraintSubject;
  scope: ConstraintScope;
  /**
   * The ceiling. A permission NOT in this list is denied to every affected
   * principal, including permissions registered after this boundary was last
   * edited (design §3.4).
   */
  maximumPermissions: PermissionId[];
  appliesWhen?: {
    notBefore?: Iso8601;
    expiresAt?: Iso8601;
  };
}

/* -------------------------------------------------------------------------- */
/* 4. Status and health (design §6.1)                                         */
/* -------------------------------------------------------------------------- */

/**
 * Derived server-side; never computed in the browser from timestamps and the
 * `disabled` flag, because the browser clock is not the evaluation clock.
 */
export type AccessBoundaryStatus =
  | 'active'
  | 'scheduled'
  | 'expired'
  | 'recovery_disabled'
  | 'invalid_degraded';

/**
 * Whether the boundary's typed references still resolve.
 */
export interface ResolutionHealth {
  state: 'healthy' | 'degraded' | 'unresolvable';
  unresolvedReferences: UnresolvedReference[];
}

export interface UnresolvedReference {
  /** Dotted path into the record, e.g. `subject.groupId`. */
  field: string;
  referenceType: 'user' | 'agent' | 'group' | 'project' | 'permission';
  referenceId: string;
  reason:
    | 'group_not_found'
    | 'user_not_found'
    | 'agent_not_found'
    | 'project_not_found'
    | 'permission_not_registered'
    | 'resolution_failed';
}

/** Advisory risk tags for list filtering and row emphasis (design §6.1). */
export type AccessBoundaryRisk =
  | 'tightening'
  | 'relaxation_scheduled'
  | 'mixed'
  | 'lockout_sensitive'
  | 'degraded';

/* -------------------------------------------------------------------------- */
/* 5. Classification and completeness (design §7.4, §7.9)                     */
/* -------------------------------------------------------------------------- */

/**
 * The direction of a mutation's effect on authority.
 *
 * `no_effect` is a DISPLAY-ONLY subtype of `relax` — the wire classification
 * is always one of the first three.
 */
export type MutationClassification = 'tighten' | 'relax' | 'mixed';

/** Display-level refinement. Not a wire value. */
export type MutationClassificationDisplay = MutationClassification | 'no_effect';

/**
 * Whether an impact analysis can be trusted as final.
 *
 * `complete: false` blocks commit server-side (design §9.1).
 */
export interface PreviewCompleteness {
  complete: boolean;
  truncated: boolean;
  degraded: boolean;
  reasons: PreviewIncompletenessReason[];
}

export interface PreviewIncompletenessReason {
  code:
    | 'MEMBERSHIP_RESOLUTION_FAILED'
    | 'SUBJECT_SET_TOO_LARGE'
    | 'PERMISSION_RESOLUTION_FAILED'
    | 'TIME_BUDGET_EXCEEDED';
  message: string;
  details?: Record<string, unknown>;
}

/* -------------------------------------------------------------------------- */
/* 6. Impact (design §9.4, §6.4)                                              */
/* -------------------------------------------------------------------------- */

/** Aggregate blast radius. All counts are server-computed and server-paged. */
export interface BoundaryImpact {
  affectedPrincipalCount: number;
  affectedPrincipalCountExact?: boolean;
  losingPrincipalCount: number;
  regainingPrincipalCount: number;
  noEffectPrincipalCount: number;
  permissionDiffs: PermissionImpact[];
  permissionDiffsTruncated: boolean;
  current: { effectivePermissionCount: number };
  proposed: { effectivePermissionCount: number };
  futureMostRestrictive: FutureMostRestrictiveImpact | null;
}

export interface FutureMostRestrictiveImpact {
  at: Iso8601;
  affectedPrincipalCount: number;
  removedPermissionCount: number;
  note?: string;
}

/** Per-permission diff row. */
export interface PermissionImpact {
  permissionId: PermissionId;
  losingCount: number;
  regainingCount: number;
}

/**
 * One segment of the change's timeline. A scheduled or expiring boundary
 * produces several, each with its own classification.
 */
export interface TemporalImpact {
  from: Iso8601;
  until: Iso8601 | null;
  label: string;
  classification: MutationClassification;
  affectedPrincipalCount: number;
  removedPermissionCount: number;
  note?: string;
}

/* -------------------------------------------------------------------------- */
/* 7. Affected principals and provenance (design §6.4, §6.5)                  */
/* -------------------------------------------------------------------------- */

/** One principal's impact row. */
export interface AffectedPrincipal {
  /** `null` when redacted; `redacted` then explains why. */
  principal: PrincipalRef | null;
  status: PrincipalStatus;
  changeKind: 'loses' | 'regains' | 'mixed' | 'no_effect';
  /** Why a principal in the subject set is nonetheless unaffected. */
  noEffectReason?:
    | 'principal_suspended'
    | 'no_overlapping_authority'
    | 'already_constrained_by_other_boundary';
  membershipPaths: MembershipPath[];
  currentPermissionCount: number;
  proposedPermissionCount: number;
  removedPermissions: PermissionId[];
  regainedPermissions: PermissionId[];
  removedPermissionsTruncated: boolean;
  removedPermissionCount: number;
  grantSources: GrantSource[];
  redacted?: RedactionNotice;
  _capabilities: AccessBoundaryCapabilities;
}

/** A chain from the principal to the boundary's subject. */
export interface MembershipPath {
  /** Ordered, principal first, subject last. Redacted hops have `id: null`. */
  hops: PrincipalRef[];
  /** True when the principal is a direct member of the subject group. */
  direct: boolean;
  truncated: boolean;
}

/** Where the principal's authority comes from, before the boundary applies. */
export interface GrantSource {
  kind: 'role_binding' | 'relationship_grant';
  id: string;
  roleName?: string;
  scope: ConstraintScope;
}

/**
 * Another boundary whose subject or permission set overlaps this one.
 *
 * Boundaries only ever intersect — there is no priority, no override, and no
 * winner (design §3.1). `relationship` describes the observed interaction.
 */
export interface IntersectingBoundary {
  id: string;
  /** `null` when the reader may not see boundaries in that scope. */
  name: string | null;
  scope: ConstraintScope;
  relationship: 'narrows' | 'overlaps' | 'limits_relaxation' | 'blocks_relaxation';
  overlappingPermissionCount: number;
  netEffectNote: string;
  redacted?: RedactionNotice;
  _capabilities: AccessBoundaryCapabilities;
}

/* -------------------------------------------------------------------------- */
/* 8. Boundary records (design §9.3)                                          */
/* -------------------------------------------------------------------------- */

/** Row shape for the inventory list. */
export interface AccessBoundarySummary {
  id: string;
  name: string;
  purpose: string;
  revision: BoundaryRevision;
  subject: ConstraintSubject;
  subjectDisplay: ConstraintSubjectDisplay;
  scope: ConstraintScope;
  scopeDisplay: ConstraintScopeDisplay;
  status: AccessBoundaryStatus;
  risk: AccessBoundaryRisk[];
  health: ResolutionHealth;
  maximumPermissionCount: number;
  affectedPrincipalCount: number;
  affectedPrincipalCountExact: boolean;
  appliesWhen: { notBefore: Iso8601 | null; expiresAt: Iso8601 | null };
  createdBy: PrincipalRef | null;
  createdAt: Iso8601;
  updatedBy: PrincipalRef | null;
  updatedAt: Iso8601;
  redacted?: RedactionNotice;
  _capabilities: AccessBoundaryCapabilities;
}

/** Full record for the detail view. */
export interface AccessBoundaryDetail extends Omit<
  AccessBoundarySummary,
  'maximumPermissionCount' | 'affectedPrincipalCount' | 'affectedPrincipalCountExact'
> {
  maximumPermissions: PermissionId[];
  permissionRegistry: PermissionRegistryContext;
  effect: BoundaryEffectSummary;
  intersectingBoundaries: IntersectingBoundary[];
  recovery: RecoveryState;
  links: {
    affectedPrincipals: string | null;
    audit: string | null;
    preview: string | null;
  };
  lastAuditEventId: string | null;
}

/** Registry context for the "what is excluded" view. */
export interface PermissionRegistryContext {
  revision: string;
  totalPermissionCount: number;
  excludedPermissionCount: number;
  /** Permissions registered after this boundary was last edited. */
  newSincePermissionIds: PermissionId[];
  newSinceRevision: string | null;
}

export interface BoundaryEffectSummary {
  affectedPrincipalCount: number;
  affectedPrincipalCountExact: boolean;
  principalsLosingAuthorityCount: number;
  intersectingBoundaryCount: number;
  computedAt: Iso8601;
  completeness: PreviewCompleteness;
  note?: string;
}

/**
 * Offline-recovery state. There is deliberately no in-product re-enable.
 */
export interface RecoveryState {
  disabled: boolean;
  disabledAt: Iso8601 | null;
  disabledBy: (PrincipalRef & { credentialType?: string }) | null;
  disabledReason: string | null;
  auditEventId?: string | null;
  reenableSupported?: false;
  reenableGuidance?: string;
}

/* -------------------------------------------------------------------------- */
/* 9. AccessBoundaryPreview (design §9.4)                                     */
/* -------------------------------------------------------------------------- */

export type PreviewOperation = 'create' | 'update' | 'delete';

/** Request body for `POST /api/v1/admin/access-constraint-previews`. */
export type AccessBoundaryPreviewRequest =
  | { operation: 'create'; draft: AccessConstraintDraft }
  | {
      operation: 'update';
      constraintId: string;
      baseRevision: BoundaryRevision;
      draft: AccessConstraintDraft;
    }
  | { operation: 'delete'; constraintId: string; baseRevision: BoundaryRevision };

/**
 * The impact analysis, and the authorization to act on it.
 *
 * `previewToken` is OPAQUE and SINGLE-USE. The client must never parse,
 * recompute, cache across drafts, or reuse it.
 */
export interface AccessBoundaryPreview {
  previewId: string;
  previewToken: string;
  generatedAt: Iso8601;
  expiresAt: Iso8601;
  operation: PreviewOperation;
  constraintId: string | null;
  baseRevision: BoundaryRevision | null;
  draftHash: string;
  classification: MutationClassification;
  completeness: PreviewCompleteness;
  lockout: LockoutAssessment;
  impact: BoundaryImpact;
  temporalStates: TemporalImpact[];
  principalsPage: {
    items: AffectedPrincipal[];
    nextPageToken?: PageToken;
    totalCount: number;
    totalCountExact: boolean;
  };
  intersectingBoundaries: IntersectingBoundary[];
  warnings: PreviewWarning[];
  /**
   * Present when the server would reject a commit. When set,
   * `_capabilities.actions` omits 'commit' and the UI shows the control
   * DISABLED WITH THIS REASON rather than hidden (design §3.3).
   */
  commitBlocked?: {
    code: AccessBoundaryErrorCode;
    message: string;
    missingPermissionIds: PermissionId[];
  };
  _capabilities: AccessBoundaryCapabilities;
}

/**
 * Post-state lockout assessment. Fields are `null` when the analysis was
 * degraded and the answer is genuinely unknown.
 */
export interface LockoutAssessment {
  safe: boolean | null;
  remainingActiveDirectAdmins: number | null;
  actorRetainsMutationAuthority: boolean | null;
  checkedPermissionIds: PermissionId[];
  undeterminedReason?: string;
}

export interface PreviewWarning {
  code:
    | 'LARGE_BLAST_RADIUS'
    | 'RELAXATION_INCLUDED'
    | 'RELAXATION_MASKED_BY_INTERSECTION'
    | 'DESTRUCTIVE_PERMISSION_RESTORED'
    | 'PREVIEW_DEGRADED'
    | 'SCHEDULED_ACTIVATION';
  severity: 'info' | 'warning' | 'error';
  message: string;
  details?: Record<string, unknown>;
}

/** `202 Accepted` handle for previews too large to compute synchronously. */
export interface AccessBoundaryPreviewJob {
  jobId: string;
  status: 'accepted' | 'running' | 'succeeded' | 'failed' | 'cancelled';
  operation: PreviewOperation;
  constraintId: string | null;
  baseRevision: BoundaryRevision | null;
  draftHash: string;
  acceptedAt: Iso8601;
  startedAt?: Iso8601 | null;
  completedAt?: Iso8601 | null;
  /** Omitted entirely when the server cannot estimate. */
  progress?: {
    phase: string;
    processedCount: number;
    totalCount: number | null;
    determinate: boolean;
  };
  pollUrl?: string;
  retryAfterSeconds: number | null;
  /** Populated only when `status === 'succeeded'`. */
  preview: AccessBoundaryPreview | null;
  error: StructuredAPIError | null;
  _capabilities: AccessBoundaryCapabilities;
}

/** Commit bodies (design §9.5). All carry `If-Match` in the request headers. */
export interface AccessBoundaryCommitRequest {
  previewToken: string;
  /** Omitted for delete. */
  draft?: AccessConstraintDraft;
  acknowledgements?: {
    acknowledgedClassification: MutationClassification;
    acknowledgedLosingPrincipalCount?: number;
    acknowledgedRegainingPrincipalCount?: number;
  };
}

export interface AccessBoundaryCommitResponse {
  constraint: AccessBoundaryDetail | AccessBoundarySummary;
  revision: BoundaryRevision;
  auditEventId: string;
  correlationId: string;
  requestId?: string;
  committed: {
    previewId: string;
    classification: MutationClassification;
    affectedPrincipalCount: number;
    losingPrincipalCount: number;
    regainingPrincipalCount: number;
    permissionDiffSummary: {
      removedPermissionCount: number;
      addedPermissionCount: number;
    };
    /** False when the committed effect differed from the previewed effect. */
    matchedPreview: boolean;
    committedAt: Iso8601;
  };
}

/* -------------------------------------------------------------------------- */
/* 10. AccessBoundaryAuditEvent (design §9.3, §11)                            */
/* -------------------------------------------------------------------------- */

/** One entry in a boundary's audit trail. */
export interface AccessBoundaryAuditEvent {
  id: string;
  occurredAt: Iso8601;
  eventType:
    | 'boundary.created'
    | 'boundary.updated'
    | 'boundary.deleted'
    | 'boundary.commit_rejected'
    | 'boundary.recovery_disabled';
  classification: MutationClassification | null;
  actor: {
    principal: PrincipalRef | null;
    credentialType: string | null;
    credentialId: string | null;
  };
  target: { type: 'access_constraint'; id: string; name: string | null };
  revisionBefore: BoundaryRevision | null;
  revisionAfter: BoundaryRevision | null;
  previewId: string | null;
  correlationId: string;
  changeSummary: AuditChangeSummary | null;
  beforeSummary: string | null;
  afterSummary: string | null;
  outcome: 'committed' | 'rejected';
  /** Present when `outcome === 'rejected'`. */
  rejectionCode?: AccessBoundaryErrorCode;
  rejectionDetails?: Record<string, unknown>;
  reason?: string;
  redacted?: RedactionNotice;
  _capabilities: AccessBoundaryCapabilities;
}

export interface AuditChangeSummary {
  /** `['*']` for creation. */
  fieldsChanged: string[];
  permissionsAdded: PermissionId[];
  permissionsRemoved: PermissionId[];
  affectedPrincipalCount: number;
  losingPrincipalCount: number;
  regainingPrincipalCount: number;
}

/* -------------------------------------------------------------------------- */
/* 11. StructuredAPIError (design §9.7)                                       */
/* -------------------------------------------------------------------------- */

/**
 * The thirteen canonical boundary error codes.
 *
 * CASING CONFLICT: existing `ErrCode*` constants are lower_snake_case. These
 * are SCREAMING_SNAKE_CASE per the frozen design (gap G2).
 */
export type AccessBoundaryErrorCode =
  | 'CONSTRAINT_ADMIN_LOCKOUT'
  | 'STALE_AUTHORIZATION_PREVIEW'
  | 'PREVIEW_INCOMPLETE'
  | 'RESOLUTION_FAILED'
  | 'SUBJECT_NOT_FOUND'
  | 'SCOPE_NOT_FOUND'
  | 'SCOPE_MISMATCH'
  | 'PERMISSION_REGISTRY_CHANGED'
  | 'INSUFFICIENT_CONSTRAINT_RELAXATION_AUTHORITY'
  | 'MUTATION_PERMISSION_LOST'
  | 'REVISION_CONFLICT'
  | 'RECOVERY_DISABLED_IMMUTABLE'
  | 'SECURITY_REVIEW_REQUIRED';

/**
 * Error payload. `code`, `message`, `details`, `requestId` match the existing
 * `APIError` shape. `correlationId` and `retryable` are additive per design §9.7.
 *
 * The `code` field is typed as `string` rather than the narrower
 * `AccessBoundaryErrorCode` union because the server may introduce new codes
 * before the client is updated. Known codes are enumerated by
 * {@link AccessBoundaryErrorCode} and pinned in {@link ACCESS_BOUNDARY_ERROR_CODES}.
 */
export interface StructuredAPIError {
  code: string;
  message: string;
  retryable: boolean;
  correlationId: string;
  requestId?: string;
  details?: Record<string, unknown>;
}

export interface StructuredAPIErrorResponse {
  error: StructuredAPIError;
}

/* -------------------------------------------------------------------------- */
/* 12. Collection envelopes (design §9.3)                                     */
/* -------------------------------------------------------------------------- */

export interface AccessBoundaryListFilters {
  q?: string;
  scopeType?: 'system' | 'project';
  scopeId?: string;
  subjectKind?: SubjectSelection;
  status?: AccessBoundaryStatus;
  risk?: AccessBoundaryRisk;
  pageSize?: number;
  pageToken?: PageToken;
  sort?: string;
}

export interface AccessBoundaryListResponse {
  items: AccessBoundarySummary[];
  nextPageToken?: PageToken;
  totalCount: number;
  totalCountExact: boolean;
  /** Consistency point for every count in this page. */
  snapshotAt: Iso8601;
  appliedFilters: AccessBoundaryListFilters;
  facets?: {
    status?: Partial<Record<AccessBoundaryStatus, number>>;
    subjectKind?: Partial<Record<ConstraintSubject['kind'], number>>;
  };
  /** Collection-level; gates the "New boundary" control. */
  _capabilities: AccessBoundaryCapabilities;
}

export interface AffectedPrincipalsPage {
  constraintId: string;
  constraintRevision: BoundaryRevision;
  snapshotAt: Iso8601;
  items: AffectedPrincipal[];
  nextPageToken?: PageToken;
  totalCount: number;
  totalCountExact: boolean;
  completeness: PreviewCompleteness;
  _capabilities: AccessBoundaryCapabilities;
}

export interface AccessBoundaryAuditPage {
  constraintId: string;
  items: AccessBoundaryAuditEvent[];
  nextPageToken?: PageToken;
  totalCount: number;
  totalCountExact: boolean;
  retention: { windowDays: number | null; note?: string };
  _capabilities: AccessBoundaryCapabilities;
}

/* -------------------------------------------------------------------------- */
/* Narrowing helpers                                                          */
/* -------------------------------------------------------------------------- */

export function isProjectScope(
  scope: ConstraintScope
): scope is { type: 'project'; projectId: string } {
  return scope.type === 'project';
}

export function isPrincipalSubject(
  subject: ConstraintSubject
): subject is { kind: 'principal'; principal: { type: PrincipalType; id: string } } {
  return subject.kind === 'principal';
}

/** True when every count in the payload is authoritative rather than a lower bound. */
export function countsAreAuthoritative(c: PreviewCompleteness): boolean {
  return c.complete && !c.truncated && !c.degraded;
}

/** The only sanctioned way to decide whether a control is available. */
export function canAccessBoundary(
  capabilities: AccessBoundaryCapabilities | undefined,
  action: AccessBoundaryCapabilityAction
): boolean {
  return capabilities?.actions?.includes(action) ?? false;
}

/* -------------------------------------------------------------------------- */
/* Runtime shape guards                                                       */
/* -------------------------------------------------------------------------- */

/** All known subject kind discriminators. Used by fixture tests. */
export const SUBJECT_KINDS = ['principal', 'group_closure', 'all_principals'] as const;

/** All known status values. Used by fixture tests. */
export const BOUNDARY_STATUSES: readonly AccessBoundaryStatus[] = [
  'active',
  'scheduled',
  'expired',
  'recovery_disabled',
  'invalid_degraded',
] as const;

/** All known mutation classification values. Used by fixture tests. */
export const MUTATION_CLASSIFICATIONS: readonly MutationClassification[] = [
  'tighten',
  'relax',
  'mixed',
] as const;

/** All known preview incompleteness reason codes. Used by fixture tests. */
export const INCOMPLETENESS_REASON_CODES: readonly PreviewIncompletenessReason['code'][] = [
  'MEMBERSHIP_RESOLUTION_FAILED',
  'SUBJECT_SET_TOO_LARGE',
  'PERMISSION_RESOLUTION_FAILED',
  'TIME_BUDGET_EXCEEDED',
] as const;

/** All known access boundary error codes. Used by fixture tests. */
export const ACCESS_BOUNDARY_ERROR_CODES: readonly AccessBoundaryErrorCode[] = [
  'CONSTRAINT_ADMIN_LOCKOUT',
  'STALE_AUTHORIZATION_PREVIEW',
  'PREVIEW_INCOMPLETE',
  'RESOLUTION_FAILED',
  'SUBJECT_NOT_FOUND',
  'SCOPE_NOT_FOUND',
  'SCOPE_MISMATCH',
  'PERMISSION_REGISTRY_CHANGED',
  'INSUFFICIENT_CONSTRAINT_RELAXATION_AUTHORITY',
  'MUTATION_PERMISSION_LOST',
  'REVISION_CONFLICT',
  'RECOVERY_DISABLED_IMMUTABLE',
  'SECURITY_REVIEW_REQUIRED',
] as const;

/**
 * Runtime shape guard for ConstraintSubject discriminated union.
 * Validates that the `kind` field matches a known variant and that required
 * properties are present.
 */
export function isConstraintSubject(value: unknown): value is ConstraintSubject {
  if (typeof value !== 'object' || value === null) return false;
  const obj = value as Record<string, unknown>;
  switch (obj.kind) {
    case 'principal':
      return (
        typeof obj.principal === 'object' &&
        obj.principal !== null &&
        typeof (obj.principal as Record<string, unknown>).type === 'string' &&
        typeof (obj.principal as Record<string, unknown>).id === 'string'
      );
    case 'group_closure':
      return typeof obj.groupId === 'string';
    case 'all_principals':
      return true;
    default:
      return false;
  }
}

/** Runtime shape guard for ConstraintScope discriminated union. */
export function isConstraintScope(value: unknown): value is ConstraintScope {
  if (typeof value !== 'object' || value === null) return false;
  const obj = value as Record<string, unknown>;
  switch (obj.type) {
    case 'system':
      return true;
    case 'project':
      return typeof obj.projectId === 'string';
    default:
      return false;
  }
}

/**
 * Runtime shape guard for AccessBoundarySummary.
 *
 * Depth note: `risk` (array) and `health.state` (enum) are not deeply
 * validated — only `_capabilities` presence is checked. This is acceptable
 * for the current use case (list-item validation with a warning-only policy).
 * Fixture tests provide deeper structural coverage.
 */
export function isAccessBoundarySummary(value: unknown): value is AccessBoundarySummary {
  if (typeof value !== 'object' || value === null) return false;
  const obj = value as Record<string, unknown>;
  return (
    typeof obj.id === 'string' &&
    typeof obj.name === 'string' &&
    typeof obj.revision === 'string' &&
    isConstraintSubject(obj.subject) &&
    isConstraintScope(obj.scope) &&
    typeof obj.status === 'string' &&
    (BOUNDARY_STATUSES as readonly string[]).includes(obj.status) &&
    typeof obj._capabilities === 'object' &&
    obj._capabilities !== null
  );
}

/** Runtime shape guard for StructuredAPIError. */
export function isStructuredAPIError(value: unknown): value is StructuredAPIError {
  if (typeof value !== 'object' || value === null) return false;
  const obj = value as Record<string, unknown>;
  return (
    typeof obj.code === 'string' &&
    typeof obj.message === 'string' &&
    typeof obj.retryable === 'boolean' &&
    typeof obj.correlationId === 'string'
  );
}

/** Runtime shape guard for StructuredAPIErrorResponse envelope. */
export function isStructuredAPIErrorResponse(value: unknown): value is StructuredAPIErrorResponse {
  if (typeof value !== 'object' || value === null) return false;
  const obj = value as Record<string, unknown>;
  return isStructuredAPIError(obj.error);
}
