/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { afterEach, describe, expect, it } from 'vitest';
import type { AccessBoundaryAuditEvent } from '../../shared/access-boundaries.js';
import './access-boundary-audit-timeline.js';
import type { ScionAccessBoundaryAuditTimeline } from './access-boundary-audit-timeline.js';

const event = {
  id: 'event-safe',
  constraintId: 'constraint-1',
  operation: 'create',
  actorKind: 'user',
  actorId: 'user-safe',
  correlationId: 'request-safe',
  batchOperationId: '',
  beforeRevision: null,
  afterRevision: '1',
  classification: 'tighten',
  previewId: 'preview-safe',
  draftHash: 'must-not-render',
  impactCounts: { agents: 2, users: 3, projects: 1 },
  changedFields: ['maximum_permissions'],
  timestamp: '2026-10-02T01:02:03Z',
} as AccessBoundaryAuditEvent;

async function mount(): Promise<ScionAccessBoundaryAuditTimeline> {
  const element = document.createElement(
    'scion-access-boundary-audit-timeline'
  ) as ScionAccessBoundaryAuditTimeline;
  document.body.appendChild(element);
  await element.updateComplete;
  return element;
}

afterEach(() => document.body.replaceChildren());

describe('access boundary audit timeline', () => {
  it('renders only safe retained fields in newest-first input order', async () => {
    const element = await mount();
    element.events = [event, { ...event, id: 'event-older', timestamp: '2026-10-01T00:00:00Z' }];
    await element.updateComplete;

    const text = element.shadowRoot?.textContent ?? '';
    expect(text).toContain('Created');
    expect(text).toContain('User: user-safe');
    expect(text).toContain('Tightening');
    expect(text).toContain('— → 1');
    expect(text).toContain('2 agents');
    expect(text).toContain('Correlation:');
    expect(text).toContain('request-safe');
    expect(text).not.toContain('must-not-render');
    const ids = [...(element.shadowRoot?.querySelectorAll('.timeline-event') ?? [])].map((node) =>
      node.getAttribute('data-event-id')
    );
    expect(ids).toEqual(['event-safe', 'event-older']);
  });

  it('does not invent or render a correlation value when the property is absent', async () => {
    const element = await mount();
    const { correlationId: _correlationId, ...withoutCorrelation } = event;
    element.events = [withoutCorrelation];
    await element.updateComplete;

    const text = element.shadowRoot?.textContent ?? '';
    expect(text).not.toContain('Correlation:');
    expect(text).not.toContain('request-safe');
  });

  it('renders loading, empty, and explicit error states', async () => {
    const element = await mount();
    element.loading = true;
    await element.updateComplete;
    expect(element.shadowRoot?.querySelector('[role="status"]')).not.toBeNull();

    element.loading = false;
    await element.updateComplete;
    expect(element.shadowRoot?.textContent).toContain('No retained audit history');

    element.errorMessage = 'Audit history is unavailable.';
    await element.updateComplete;
    expect(element.shadowRoot?.querySelector('[role="alert"]')?.textContent).toContain(
      'Audit history is unavailable.'
    );
  });

  it('requests the next tuple cursor once clicked', async () => {
    const element = await mount();
    element.events = [event];
    element.nextPageToken = 'older-token';
    await element.updateComplete;

    const detail = new Promise<string>((resolve) => {
      element.addEventListener('audit-page-request', (raw) => {
        resolve((raw as CustomEvent<{ pageToken: string }>).detail.pageToken);
      });
    });
    (element.shadowRoot?.querySelector('sl-button') as HTMLElement).click();
    await expect(detail).resolves.toBe('older-token');
  });
});
