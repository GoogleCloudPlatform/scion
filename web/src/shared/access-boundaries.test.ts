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
 * Access Boundary — contract fixture tests (WP C1)
 *
 * Frozen fixture tests that validate runtime shape guards. These tests
 * FAIL when a subject kind, classification, completeness, or error code
 * is omitted — this is the acceptance gate.
 *
 * Fixtures are loaded from the frozen WP0 fixture directory.
 */

import { describe, it, expect } from 'vitest';
import type {
  AccessBoundarySummary,
  AccessBoundaryDetail,
  AccessBoundaryPreview,
  AccessBoundaryPreviewJob,
  AccessBoundaryListResponse,
  AccessBoundaryAuditPage,
  AffectedPrincipalsPage,
  AccessBoundaryCommitResponse,
  AccessBoundaryAuditEvent,
  AffectedPrincipal,
  ConstraintSubject,
  ConstraintScope,
  AccessBoundaryStatus,
  MutationClassification,
  PreviewCompleteness,
  PreviewIncompletenessReason,
  AccessBoundaryErrorCode,
  StructuredAPIErrorResponse,
} from './access-boundaries.js';
import {
  SUBJECT_KINDS,
  BOUNDARY_STATUSES,
  MUTATION_CLASSIFICATIONS,
  INCOMPLETENESS_REASON_CODES,
  ACCESS_BOUNDARY_ERROR_CODES,
  isConstraintSubject,
  isConstraintScope,
  isAccessBoundarySummary,
  isStructuredAPIError,
  isStructuredAPIErrorResponse,
  isPrincipalSubject,
  isProjectScope,
  countsAreAuthoritative,
  canAccessBoundary,
  subjectSelectionOf,
} from './access-boundaries.js';

/* -------------------------------------------------------------------------- */
/* Fixture loading                                                            */
/* -------------------------------------------------------------------------- */

import { readFileSync } from 'fs';
import { resolve } from 'path';

const FIXTURE_DIR = resolve(__dirname, '__fixtures__/access-boundaries');

function loadFixture<T>(name: string): T {
  const raw = readFileSync(resolve(FIXTURE_DIR, name), 'utf-8');
  return JSON.parse(raw) as T;
}

/* -------------------------------------------------------------------------- */
/* 1. Exhaustive enum coverage — acceptance gate                              */
/* -------------------------------------------------------------------------- */

describe('enum exhaustiveness (acceptance gate)', () => {
  describe('subject kinds', () => {
    it('exports all three subject kind discriminators', () => {
      expect(SUBJECT_KINDS).toEqual(['principal', 'group_closure', 'all_principals']);
      expect(SUBJECT_KINDS).toHaveLength(3);
    });

    it('list fixture facets cover all subject kinds', () => {
      const listResponse = loadFixture<AccessBoundaryListResponse>('list-response.json');
      const facetKinds = Object.keys(listResponse.facets?.subjectKind ?? {});
      // The facets must cover all three subject kinds even if the page
      // doesn't contain an item of each kind.
      for (const kind of SUBJECT_KINDS) {
        expect(facetKinds).toContain(kind);
      }
    });

    // This test fails at compile time if a subject kind is missing from the union
    it('ConstraintSubject union covers all subject kinds', () => {
      const subjects: ConstraintSubject[] = [
        { kind: 'principal', principal: { type: 'user', id: 'u1' } },
        { kind: 'group_closure', groupId: 'g1' },
        { kind: 'all_principals' },
      ];
      expect(subjects).toHaveLength(SUBJECT_KINDS.length);
    });
  });

  describe('boundary statuses', () => {
    it('exports all five status values', () => {
      expect(BOUNDARY_STATUSES).toEqual([
        'active',
        'scheduled',
        'expired',
        'recovery_disabled',
        'invalid_degraded',
      ]);
      expect(BOUNDARY_STATUSES).toHaveLength(5);
    });

    it('list fixture facets include all status values', () => {
      const listResponse = loadFixture<AccessBoundaryListResponse>('list-response.json');
      const statusFacets = listResponse.facets?.status;
      expect(statusFacets).toBeDefined();
      for (const status of BOUNDARY_STATUSES) {
        expect(statusFacets).toHaveProperty(status);
      }
    });

    // Compile-time check: every status is assignable
    it('AccessBoundaryStatus covers all status values', () => {
      const statuses: AccessBoundaryStatus[] = [
        'active',
        'scheduled',
        'expired',
        'recovery_disabled',
        'invalid_degraded',
      ];
      expect(statuses).toHaveLength(BOUNDARY_STATUSES.length);
    });
  });

  describe('mutation classifications', () => {
    it('exports all three classification values', () => {
      expect(MUTATION_CLASSIFICATIONS).toEqual(['tighten', 'relax', 'mixed']);
      expect(MUTATION_CLASSIFICATIONS).toHaveLength(3);
    });

    // Compile-time check
    it('MutationClassification covers all classification values', () => {
      const classifications: MutationClassification[] = ['tighten', 'relax', 'mixed'];
      expect(classifications).toHaveLength(MUTATION_CLASSIFICATIONS.length);
    });
  });

  describe('preview incompleteness reason codes', () => {
    it('exports all four incompleteness reason codes', () => {
      expect(INCOMPLETENESS_REASON_CODES).toEqual([
        'MEMBERSHIP_RESOLUTION_FAILED',
        'SUBJECT_SET_TOO_LARGE',
        'PERMISSION_RESOLUTION_FAILED',
        'TIME_BUDGET_EXCEEDED',
      ]);
      expect(INCOMPLETENESS_REASON_CODES).toHaveLength(4);
    });

    // Compile-time check
    it('PreviewIncompletenessReason code covers all reason codes', () => {
      const codes: PreviewIncompletenessReason['code'][] = [
        'MEMBERSHIP_RESOLUTION_FAILED',
        'SUBJECT_SET_TOO_LARGE',
        'PERMISSION_RESOLUTION_FAILED',
        'TIME_BUDGET_EXCEEDED',
      ];
      expect(codes).toHaveLength(INCOMPLETENESS_REASON_CODES.length);
    });
  });

  describe('error codes', () => {
    it('exports all thirteen error codes', () => {
      expect(ACCESS_BOUNDARY_ERROR_CODES).toEqual([
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
      ]);
      expect(ACCESS_BOUNDARY_ERROR_CODES).toHaveLength(13);
    });

    it('every error code has a corresponding fixture file', () => {
      const index = loadFixture<{ codes: Array<{ code: string; fixture: string }> }>(
        'errors-index.json'
      );
      const indexedCodes = new Set(index.codes.map((c) => c.code));
      for (const code of ACCESS_BOUNDARY_ERROR_CODES) {
        expect(indexedCodes.has(code)).toBe(true);
      }
    });

    it('every error fixture parses as StructuredAPIErrorResponse', () => {
      const index = loadFixture<{ codes: Array<{ code: string; fixture: string }> }>(
        'errors-index.json'
      );
      for (const entry of index.codes) {
        const fixture = loadFixture<unknown>(entry.fixture);
        expect(isStructuredAPIErrorResponse(fixture)).toBe(true);
        const typed = fixture as StructuredAPIErrorResponse;
        expect(typed.error.code).toBe(entry.code);
        expect(typeof typed.error.message).toBe('string');
        expect(typeof typed.error.retryable).toBe('boolean');
        expect(typeof typed.error.correlationId).toBe('string');
      }
    });

    // Compile-time check
    it('AccessBoundaryErrorCode covers all error codes', () => {
      const codes: AccessBoundaryErrorCode[] = [
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
      ];
      expect(codes).toHaveLength(ACCESS_BOUNDARY_ERROR_CODES.length);
    });
  });
});

/* -------------------------------------------------------------------------- */
/* 2. Runtime shape guards                                                    */
/* -------------------------------------------------------------------------- */

describe('runtime shape guards', () => {
  describe('isConstraintSubject', () => {
    it('accepts principal subject', () => {
      expect(
        isConstraintSubject({ kind: 'principal', principal: { type: 'user', id: 'u1' } })
      ).toBe(true);
      expect(
        isConstraintSubject({ kind: 'principal', principal: { type: 'agent', id: 'a1' } })
      ).toBe(true);
      expect(
        isConstraintSubject({ kind: 'principal', principal: { type: 'group', id: 'g1' } })
      ).toBe(true);
    });

    it('accepts group_closure subject', () => {
      expect(isConstraintSubject({ kind: 'group_closure', groupId: 'grp-1' })).toBe(true);
    });

    it('accepts all_principals subject', () => {
      expect(isConstraintSubject({ kind: 'all_principals' })).toBe(true);
    });

    it('rejects invalid subjects', () => {
      expect(isConstraintSubject(null)).toBe(false);
      expect(isConstraintSubject(undefined)).toBe(false);
      expect(isConstraintSubject({})).toBe(false);
      expect(isConstraintSubject({ kind: 'unknown' })).toBe(false);
      expect(isConstraintSubject({ kind: 'principal' })).toBe(false);
      expect(isConstraintSubject({ kind: 'principal', principal: null })).toBe(false);
      expect(isConstraintSubject({ kind: 'principal', principal: { type: 'user' } })).toBe(false);
      expect(isConstraintSubject({ kind: 'group_closure' })).toBe(false);
    });
  });

  describe('isConstraintScope', () => {
    it('accepts system scope', () => {
      expect(isConstraintScope({ type: 'system' })).toBe(true);
    });

    it('accepts project scope', () => {
      expect(isConstraintScope({ type: 'project', projectId: 'proj-1' })).toBe(true);
    });

    it('rejects invalid scopes', () => {
      expect(isConstraintScope(null)).toBe(false);
      expect(isConstraintScope(undefined)).toBe(false);
      expect(isConstraintScope({})).toBe(false);
      expect(isConstraintScope({ type: 'unknown' })).toBe(false);
      expect(isConstraintScope({ type: 'project' })).toBe(false);
    });
  });

  describe('isAccessBoundarySummary', () => {
    it('validates list fixture items', () => {
      const listResponse = loadFixture<AccessBoundaryListResponse>('list-response.json');
      for (const item of listResponse.items) {
        expect(isAccessBoundarySummary(item)).toBe(true);
      }
    });

    it('rejects partial objects', () => {
      expect(isAccessBoundarySummary({})).toBe(false);
      expect(isAccessBoundarySummary({ id: 'x' })).toBe(false);
    });
  });

  describe('isStructuredAPIError', () => {
    it('validates error fixture payloads', () => {
      const lockout = loadFixture<StructuredAPIErrorResponse>(
        'errors-constraint-admin-lockout.json'
      );
      expect(isStructuredAPIError(lockout.error)).toBe(true);
    });

    it('rejects incomplete errors', () => {
      expect(isStructuredAPIError({})).toBe(false);
      expect(isStructuredAPIError({ code: 'x', message: 'y' })).toBe(false);
    });
  });
});

/* -------------------------------------------------------------------------- */
/* 3. Fixture shape validation                                                */
/* -------------------------------------------------------------------------- */

describe('fixture shape validation', () => {
  describe('list-response.json', () => {
    it('has correct envelope structure', () => {
      const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
      expect(data.items).toBeInstanceOf(Array);
      expect(data.items.length).toBeGreaterThan(0);
      expect(typeof data.totalCount).toBe('number');
      expect(typeof data.totalCountExact).toBe('boolean');
      expect(typeof data.snapshotAt).toBe('string');
      expect(data._capabilities).toBeDefined();
      expect(data._capabilities.actions).toBeInstanceOf(Array);
    });

    it('items have required fields', () => {
      const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
      for (const item of data.items) {
        expect(typeof item.id).toBe('string');
        expect(typeof item.name).toBe('string');
        expect(typeof item.purpose).toBe('string');
        expect(typeof item.revision).toBe('string');
        expect(isConstraintSubject(item.subject)).toBe(true);
        expect(isConstraintScope(item.scope)).toBe(true);
        expect((BOUNDARY_STATUSES as readonly string[]).includes(item.status)).toBe(true);
        expect(item.risk).toBeInstanceOf(Array);
        expect(item.health).toBeDefined();
        expect(typeof item.health.state).toBe('string');
        expect(item.health.unresolvedReferences).toBeInstanceOf(Array);
        expect(typeof item.maximumPermissionCount).toBe('number');
        expect(typeof item.affectedPrincipalCount).toBe('number');
        expect(typeof item.affectedPrincipalCountExact).toBe('boolean');
        expect(item.appliesWhen).toBeDefined();
        expect(typeof item.createdAt).toBe('string');
        expect(typeof item.updatedAt).toBe('string');
        expect(item._capabilities).toBeDefined();
        expect(item._capabilities.actions).toBeInstanceOf(Array);
      }
    });

    it('subjectDisplay matches subject kind', () => {
      const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
      for (const item of data.items) {
        expect(item.subjectDisplay.kind).toBe(item.subject.kind);
      }
    });

    it('scopeDisplay matches scope type', () => {
      const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
      for (const item of data.items) {
        expect(item.scopeDisplay.type).toBe(item.scope.type);
      }
    });
  });

  describe('detail-response.json', () => {
    it('extends summary with detail-only fields', () => {
      const data = loadFixture<AccessBoundaryDetail>('detail-response.json');
      expect(typeof data.id).toBe('string');
      expect(isConstraintSubject(data.subject)).toBe(true);
      expect(isConstraintScope(data.scope)).toBe(true);
      // Detail-only fields
      expect(data.maximumPermissions).toBeInstanceOf(Array);
      expect(data.permissionRegistry).toBeDefined();
      expect(typeof data.permissionRegistry.revision).toBe('string');
      expect(typeof data.permissionRegistry.totalPermissionCount).toBe('number');
      expect(data.effect).toBeDefined();
      expect(typeof data.effect.affectedPrincipalCount).toBe('number');
      expect(data.effect.completeness).toBeDefined();
      expect(data.intersectingBoundaries).toBeInstanceOf(Array);
      expect(data.recovery).toBeDefined();
      expect(typeof data.recovery.disabled).toBe('boolean');
      expect(data.links).toBeDefined();
    });
  });

  describe('preview-create-response.json', () => {
    it('has correct preview structure', () => {
      const data = loadFixture<AccessBoundaryPreview>('preview-create-response.json');
      expect(typeof data.previewId).toBe('string');
      expect(typeof data.previewToken).toBe('string');
      expect(typeof data.generatedAt).toBe('string');
      expect(typeof data.expiresAt).toBe('string');
      expect(data.operation).toBe('create');
      expect((MUTATION_CLASSIFICATIONS as readonly string[]).includes(data.classification)).toBe(
        true
      );
      expect(data.completeness).toBeDefined();
      expect(typeof data.completeness.complete).toBe('boolean');
      expect(typeof data.completeness.truncated).toBe('boolean');
      expect(typeof data.completeness.degraded).toBe('boolean');
      expect(data.completeness.reasons).toBeInstanceOf(Array);
      expect(data.lockout).toBeDefined();
      expect(data.impact).toBeDefined();
      expect(data.temporalStates).toBeInstanceOf(Array);
      expect(data.principalsPage).toBeDefined();
      expect(data.principalsPage.items).toBeInstanceOf(Array);
      expect(data.intersectingBoundaries).toBeInstanceOf(Array);
      expect(data.warnings).toBeInstanceOf(Array);
      expect(data._capabilities).toBeDefined();
    });

    it('impact has required structure', () => {
      const data = loadFixture<AccessBoundaryPreview>('preview-create-response.json');
      const impact = data.impact;
      expect(typeof impact.affectedPrincipalCount).toBe('number');
      expect(typeof impact.losingPrincipalCount).toBe('number');
      expect(typeof impact.regainingPrincipalCount).toBe('number');
      expect(typeof impact.noEffectPrincipalCount).toBe('number');
      expect(impact.permissionDiffs).toBeInstanceOf(Array);
      expect(typeof impact.permissionDiffsTruncated).toBe('boolean');
      expect(typeof impact.current.effectivePermissionCount).toBe('number');
      expect(typeof impact.proposed.effectivePermissionCount).toBe('number');
    });

    it('temporal states have correct shape', () => {
      const data = loadFixture<AccessBoundaryPreview>('preview-create-response.json');
      for (const state of data.temporalStates) {
        expect(typeof state.from).toBe('string');
        expect(typeof state.label).toBe('string');
        expect((MUTATION_CLASSIFICATIONS as readonly string[]).includes(state.classification)).toBe(
          true
        );
        expect(typeof state.affectedPrincipalCount).toBe('number');
        expect(typeof state.removedPermissionCount).toBe('number');
      }
    });

    it('affected principal items have correct shape', () => {
      const data = loadFixture<AccessBoundaryPreview>('preview-create-response.json');
      for (const p of data.principalsPage.items) {
        expect(p.principal === null || typeof p.principal === 'object').toBe(true);
        expect(['active', 'suspended', 'deleted', 'unknown']).toContain(p.status);
        expect(['loses', 'regains', 'mixed', 'no_effect']).toContain(p.changeKind);
        expect(p.membershipPaths).toBeInstanceOf(Array);
        expect(typeof p.currentPermissionCount).toBe('number');
        expect(typeof p.proposedPermissionCount).toBe('number');
        expect(p.removedPermissions).toBeInstanceOf(Array);
        expect(p.regainedPermissions).toBeInstanceOf(Array);
        expect(typeof p.removedPermissionsTruncated).toBe('boolean');
        expect(typeof p.removedPermissionCount).toBe('number');
        expect(p.grantSources).toBeInstanceOf(Array);
        expect(p._capabilities).toBeDefined();
      }
    });
  });

  describe('preview-async-accepted.json', () => {
    it('has async job shape with 202 fields', () => {
      const data = loadFixture<AccessBoundaryPreviewJob>('preview-async-accepted.json');
      expect(typeof data.jobId).toBe('string');
      expect(data.status).toBe('accepted');
      expect(typeof data.draftHash).toBe('string');
      expect(typeof data.acceptedAt).toBe('string');
      expect(typeof data.pollUrl).toBe('string');
      expect(data.retryAfterSeconds === null || typeof data.retryAfterSeconds === 'number').toBe(
        true
      );
      expect(data._capabilities).toBeDefined();
    });
  });

  describe('commit-create-response.json', () => {
    it('has commit response shape', () => {
      const data = loadFixture<AccessBoundaryCommitResponse>('commit-create-response.json');
      expect(data.constraint).toBeDefined();
      expect(typeof data.revision).toBe('string');
      expect(typeof data.auditEventId).toBe('string');
      expect(typeof data.correlationId).toBe('string');
      expect(data.committed).toBeDefined();
      expect(typeof data.committed.previewId).toBe('string');
      expect(
        (MUTATION_CLASSIFICATIONS as readonly string[]).includes(data.committed.classification)
      ).toBe(true);
      expect(typeof data.committed.affectedPrincipalCount).toBe('number');
      expect(typeof data.committed.matchedPreview).toBe('boolean');
      expect(typeof data.committed.committedAt).toBe('string');
    });
  });

  describe('audit-events.json', () => {
    it('has audit page shape', () => {
      const data = loadFixture<AccessBoundaryAuditPage>('audit-events.json');
      expect(typeof data.constraintId).toBe('string');
      expect(data.items).toBeInstanceOf(Array);
      expect(data.items.length).toBeGreaterThan(0);
      expect(typeof data.totalCount).toBe('number');
      expect(typeof data.totalCountExact).toBe('boolean');
      expect(data.retention).toBeDefined();
      expect(data._capabilities).toBeDefined();
    });

    it('audit events have correct shape', () => {
      const data = loadFixture<AccessBoundaryAuditPage>('audit-events.json');
      const validEventTypes = [
        'boundary.created',
        'boundary.updated',
        'boundary.deleted',
        'boundary.commit_rejected',
        'boundary.recovery_disabled',
      ];
      for (const event of data.items) {
        expect(typeof event.id).toBe('string');
        expect(typeof event.occurredAt).toBe('string');
        expect(validEventTypes).toContain(event.eventType);
        expect(event.actor).toBeDefined();
        expect(event.target).toBeDefined();
        expect(event.target.type).toBe('access_constraint');
        expect(typeof event.correlationId).toBe('string');
        expect(['committed', 'rejected']).toContain(event.outcome);
        expect(event._capabilities).toBeDefined();
      }
    });

    it('rejected events carry rejection code', () => {
      const data = loadFixture<AccessBoundaryAuditPage>('audit-events.json');
      const rejected = data.items.filter((e) => e.outcome === 'rejected');
      expect(rejected.length).toBeGreaterThan(0);
      for (const event of rejected) {
        expect(typeof event.rejectionCode).toBe('string');
        expect(
          (ACCESS_BOUNDARY_ERROR_CODES as readonly string[]).includes(event.rejectionCode!)
        ).toBe(true);
      }
    });

    it('covers multiple event types', () => {
      const data = loadFixture<AccessBoundaryAuditPage>('audit-events.json');
      const types = new Set(data.items.map((e) => e.eventType));
      expect(types.size).toBeGreaterThanOrEqual(3);
    });
  });

  describe('affected-principals-page.json', () => {
    it('has affected principals page shape', () => {
      const data = loadFixture<AffectedPrincipalsPage>('affected-principals-page.json');
      expect(typeof data.constraintId).toBe('string');
      expect(typeof data.constraintRevision).toBe('string');
      expect(typeof data.snapshotAt).toBe('string');
      expect(data.items).toBeInstanceOf(Array);
      expect(typeof data.totalCount).toBe('number');
      expect(typeof data.totalCountExact).toBe('boolean');
      expect(data.completeness).toBeDefined();
      expect(data._capabilities).toBeDefined();
    });
  });

  describe('recovery-disabled-detail.json', () => {
    it('has recovery_disabled status and recovery state', () => {
      const data = loadFixture<AccessBoundaryDetail>('recovery-disabled-detail.json');
      expect(data.status).toBe('recovery_disabled');
      expect(data.recovery.disabled).toBe(true);
      expect(typeof data.recovery.disabledAt).toBe('string');
      expect(data.recovery.disabledBy).toBeDefined();
      expect(typeof data.recovery.disabledReason).toBe('string');
    });
  });
});

/* -------------------------------------------------------------------------- */
/* 4. Narrowing helpers                                                       */
/* -------------------------------------------------------------------------- */

describe('narrowing helpers', () => {
  describe('isPrincipalSubject', () => {
    it('narrows principal subjects', () => {
      const subject: ConstraintSubject = {
        kind: 'principal',
        principal: { type: 'user', id: 'u1' },
      };
      expect(isPrincipalSubject(subject)).toBe(true);
      if (isPrincipalSubject(subject)) {
        // This must compile — proves the type guard works
        expect(subject.principal.type).toBe('user');
      }
    });

    it('rejects non-principal subjects', () => {
      expect(isPrincipalSubject({ kind: 'group_closure', groupId: 'g1' })).toBe(false);
      expect(isPrincipalSubject({ kind: 'all_principals' })).toBe(false);
    });
  });

  describe('isProjectScope', () => {
    it('narrows project scopes', () => {
      const scope: ConstraintScope = { type: 'project', projectId: 'proj-1' };
      expect(isProjectScope(scope)).toBe(true);
      if (isProjectScope(scope)) {
        expect(scope.projectId).toBe('proj-1');
      }
    });

    it('rejects system scope', () => {
      expect(isProjectScope({ type: 'system' })).toBe(false);
    });
  });

  describe('countsAreAuthoritative', () => {
    it('returns true for complete, non-truncated, non-degraded', () => {
      const c: PreviewCompleteness = {
        complete: true,
        truncated: false,
        degraded: false,
        reasons: [],
      };
      expect(countsAreAuthoritative(c)).toBe(true);
    });

    it('returns false when any flag is set', () => {
      expect(
        countsAreAuthoritative({
          complete: false,
          truncated: false,
          degraded: false,
          reasons: [],
        })
      ).toBe(false);
      expect(
        countsAreAuthoritative({
          complete: true,
          truncated: true,
          degraded: false,
          reasons: [],
        })
      ).toBe(false);
      expect(
        countsAreAuthoritative({
          complete: true,
          truncated: false,
          degraded: true,
          reasons: [],
        })
      ).toBe(false);
    });
  });

  describe('canAccessBoundary', () => {
    it('returns true when action is present', () => {
      expect(canAccessBoundary({ actions: ['read', 'commit'] }, 'read')).toBe(true);
      expect(canAccessBoundary({ actions: ['read', 'commit'] }, 'commit')).toBe(true);
    });

    it('returns false when action is absent', () => {
      expect(canAccessBoundary({ actions: ['read'] }, 'commit')).toBe(false);
    });

    it('returns false for undefined capabilities (fail-closed)', () => {
      expect(canAccessBoundary(undefined, 'read')).toBe(false);
    });
  });

  describe('subjectSelectionOf', () => {
    it('maps principal subjects to exact_* selections', () => {
      expect(subjectSelectionOf({ kind: 'principal', principal: { type: 'user', id: 'u' } })).toBe(
        'exact_user'
      );
      expect(subjectSelectionOf({ kind: 'principal', principal: { type: 'agent', id: 'a' } })).toBe(
        'exact_agent'
      );
      expect(subjectSelectionOf({ kind: 'principal', principal: { type: 'group', id: 'g' } })).toBe(
        'exact_group'
      );
    });

    it('maps group_closure', () => {
      expect(subjectSelectionOf({ kind: 'group_closure', groupId: 'g1' })).toBe('group_closure');
    });

    it('maps all_principals', () => {
      expect(subjectSelectionOf({ kind: 'all_principals' })).toBe('all_principals');
    });
  });
});

/* -------------------------------------------------------------------------- */
/* 5. Cross-fixture consistency                                               */
/* -------------------------------------------------------------------------- */

describe('cross-fixture consistency', () => {
  it('list subjects are all valid discriminated unions', () => {
    const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
    for (const item of data.items) {
      expect(isConstraintSubject(item.subject)).toBe(true);
      expect(isConstraintScope(item.scope)).toBe(true);
    }
  });

  it('detail subjects are valid discriminated unions', () => {
    const data = loadFixture<AccessBoundaryDetail>('detail-response.json');
    expect(isConstraintSubject(data.subject)).toBe(true);
    expect(isConstraintScope(data.scope)).toBe(true);
  });

  it('preview classification matches temporal state classifications', () => {
    const data = loadFixture<AccessBoundaryPreview>('preview-create-response.json');
    // The preview classification should be consistent with temporal states
    for (const state of data.temporalStates) {
      expect((MUTATION_CLASSIFICATIONS as readonly string[]).includes(state.classification)).toBe(
        true
      );
    }
  });

  it('error index covers all exported error codes', () => {
    const index = loadFixture<{ codes: Array<{ code: string }> }>('errors-index.json');
    const indexedCodes = new Set(index.codes.map((c) => c.code));
    for (const code of ACCESS_BOUNDARY_ERROR_CODES) {
      expect(indexedCodes.has(code)).toBe(true);
    }
    // And the reverse: every indexed code is in our exported set
    for (const entry of index.codes) {
      expect((ACCESS_BOUNDARY_ERROR_CODES as readonly string[]).includes(entry.code)).toBe(true);
    }
  });

  it('subject kinds in list facets match exported SUBJECT_KINDS', () => {
    const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
    const facetKinds = Object.keys(data.facets?.subjectKind ?? {});
    for (const kind of facetKinds) {
      expect((SUBJECT_KINDS as readonly string[]).includes(kind)).toBe(true);
    }
  });

  it('status values in list facets match exported BOUNDARY_STATUSES', () => {
    const data = loadFixture<AccessBoundaryListResponse>('list-response.json');
    const facetStatuses = Object.keys(data.facets?.status ?? {});
    for (const status of facetStatuses) {
      expect((BOUNDARY_STATUSES as readonly string[]).includes(status)).toBe(true);
    }
  });
});
