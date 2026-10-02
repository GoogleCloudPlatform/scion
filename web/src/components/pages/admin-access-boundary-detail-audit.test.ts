/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { afterEach, describe, expect, it, vi } from 'vitest';
import type {
  AccessBoundaryAuditEvent,
  AccessBoundaryAuditPage,
} from '../../shared/access-boundaries.js';

const { listAudit } = vi.hoisted(() => ({ listAudit: vi.fn() }));
vi.mock('../../client/access-boundaries-api.js', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../client/access-boundaries-api.js')>()),
  listAudit,
}));

import { AccessBoundaryAPIError } from '../../client/access-boundaries-api.js';
import './admin-access-boundary-detail.js';
import type { ScionPageAdminAccessBoundaryDetail } from './admin-access-boundary-detail.js';

type AuditHarness = ScionPageAdminAccessBoundaryDetail & {
  boundaryId: string;
  auditEvents: AccessBoundaryAuditEvent[];
  auditNextToken?: string;
  auditError: string;
  loadAuditEvents(pageToken?: string): Promise<void>;
};

function retainedEvent(id: string, constraintId: string): AccessBoundaryAuditEvent {
  return {
    id,
    constraintId,
    operation: 'create',
    actorKind: 'user',
    actorId: 'user-1',
    correlationId: 'request-1',
    batchOperationId: '',
    beforeRevision: null,
    afterRevision: '1',
    classification: 'tighten',
    previewId: 'preview-1',
    draftHash: '',
    impactCounts: { agents: 1, users: 1, projects: 0 },
    changedFields: ['maximum_permissions'],
    timestamp: '2026-10-02T01:02:03Z',
  };
}

function page(items: AccessBoundaryAuditEvent[], nextPageToken?: string): AccessBoundaryAuditPage {
  return {
    items,
    nextPageToken,
    totalCount: items.length,
    totalCountExact: true,
    retention: { maxRows: 1000, note: 'retained' },
  };
}

function harness(id = 'constraint-a'): AuditHarness {
  const element = document.createElement('scion-page-admin-access-boundary-detail') as AuditHarness;
  element.boundaryId = id;
  return element;
}

afterEach(() => {
  listAudit.mockReset();
  document.body.replaceChildren();
});

describe('admin access boundary detail audit integration', () => {
  it('deduplicates appended rows and stops a repeated cursor', async () => {
    const element = harness();
    listAudit
      .mockResolvedValueOnce(page([retainedEvent('new', 'constraint-a')], 'older'))
      .mockResolvedValueOnce(
        page([retainedEvent('new', 'constraint-a'), retainedEvent('old', 'constraint-a')], 'older')
      );

    await element.loadAuditEvents();
    await element.loadAuditEvents('older');
    await element.loadAuditEvents('older');

    expect(element.auditEvents.map((item) => item.id)).toEqual(['new', 'old']);
    expect(element.auditNextToken).toBeUndefined();
    expect(listAudit).toHaveBeenCalledTimes(2);
  });

  it('ignores a stale response after the resource changes', async () => {
    const element = harness('constraint-a');
    let resolveA!: (value: AccessBoundaryAuditPage) => void;
    listAudit.mockImplementation((id: string) => {
      if (id === 'constraint-a') {
        return new Promise<AccessBoundaryAuditPage>((resolve) => (resolveA = resolve));
      }
      return Promise.resolve(page([retainedEvent('event-b', 'constraint-b')]));
    });

    const stale = element.loadAuditEvents();
    element.boundaryId = 'constraint-b';
    await element.loadAuditEvents();
    resolveA(page([retainedEvent('event-a', 'constraint-a')]));
    await stale;

    expect(element.auditEvents.map((item) => item.id)).toEqual(['event-b']);
  });

  it('clears stale rows and distinguishes private 404 from service failure safely', async () => {
    const element = harness();
    element.auditEvents = [retainedEvent('stale', 'constraint-a')];
    listAudit.mockRejectedValueOnce(
      new AccessBoundaryAPIError(404, {
        code: 'not_found',
        message: 'do not expose this',
        retryable: false,
        correlationId: '',
      })
    );
    await element.loadAuditEvents();
    expect(element.auditEvents).toEqual([]);
    expect(element.auditError).toBe('Audit history is unavailable.');

    listAudit.mockRejectedValueOnce(new Error('database password must not render'));
    await element.loadAuditEvents();
    expect(element.auditError).toBe('Unable to load audit history. Try again.');
  });
});
