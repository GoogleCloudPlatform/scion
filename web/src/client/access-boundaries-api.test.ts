/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

import { listAudit, StaleResponseError } from './access-boundaries-api.js';
import type {
  AccessBoundaryAuditEvent,
  AccessBoundaryAuditPage,
} from '../shared/access-boundaries.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const retainedPage = {
  items: [
    {
      id: 'event-2',
      constraintId: 'constraint/a',
      operation: 'create',
      actorKind: 'user',
      actorId: 'user-1',
      correlationId: 'request-1',
      beforeRevision: null,
      afterRevision: '1',
      classification: 'tighten',
      previewId: 'preview-1',
      impactCounts: { agents: 2, users: 1, projects: 1 },
      changedFields: ['maximum_permissions'],
      timestamp: '2026-10-02T01:02:03Z',
    },
  ],
  nextPageToken: 'older-token',
  totalCount: 1,
  totalCountExact: true,
  retention: { maxRows: 1000, note: 'Current retained rows.' },
};

beforeEach(() => apiFetch.mockReset());

describe('listAudit', () => {
  it('returns the retained-history shape and encodes pagination parameters', async () => {
    apiFetch.mockResolvedValueOnce(jsonResponse(retainedPage));

    const page = await listAudit('constraint/a', { pageSize: 20, pageToken: 'tuple cursor' });

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/admin/access-constraints/constraint%2Fa/audit?pageSize=20&pageToken=tuple+cursor',
      { signal: null }
    );
    expect(page).toEqual(retainedPage);
    expect(page.items[0].correlationId).toBe('request-1');
    expect(page.items[0]).not.toHaveProperty('actor.credentialId');
  });

  it('accepts a retained event whose correlation ID is omitted', async () => {
    const { correlationId: _correlationId, ...withoutCorrelation } = retainedPage.items[0];
    const event: AccessBoundaryAuditEvent = withoutCorrelation;
    const page: AccessBoundaryAuditPage = { ...retainedPage, items: [event] };
    apiFetch.mockResolvedValueOnce(jsonResponse(page));

    const result = await listAudit('constraint/a');

    expect(result.items[0]).not.toHaveProperty('correlationId');
  });

  it('rejects an older response superseded for the same constraint', async () => {
    let resolveOlder!: (response: Response) => void;
    apiFetch
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (resolveOlder = resolve)))
      .mockResolvedValueOnce(jsonResponse(retainedPage));

    const older = listAudit('constraint-a');
    await expect(listAudit('constraint-a')).resolves.toEqual(retainedPage);
    resolveOlder(jsonResponse(retainedPage));
    await expect(older).rejects.toBeInstanceOf(StaleResponseError);
  });
});
