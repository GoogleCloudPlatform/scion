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

import { describe, it, expect, afterEach, vi } from 'vitest';
import './trigger-fire-list.js';
import type { ScionTriggerFireList } from './trigger-fire-list.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-trigger-fire-list', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders waiting, expired, coalescing, deferred, and fired statuses generically with timestamps', async () => {
    const fires = [
      {
        id: 'f-wait',
        triggerId: 'tr-1',
        eventId: 'ev-1',
        projectId: 'proj-1',
        actionType: 'message',
        status: 'waiting',
        waitUntil: '2026-11-21T10:00:00Z',
        startedAt: '2026-11-20T10:00:00Z',
      },
      {
        id: 'f-exp',
        triggerId: 'tr-1',
        eventId: 'ev-2',
        projectId: 'proj-1',
        actionType: 'message',
        status: 'expired',
        error: 'offline queue ttl expired',
        startedAt: '2026-11-20T09:00:00Z',
      },
      {
        id: 'f-coal',
        triggerId: 'tr-2',
        eventId: 'ev-3',
        projectId: 'proj-1',
        actionType: 'dispatch_agent',
        status: 'coalescing',
        notBefore: '2026-11-20T10:05:00Z',
        startedAt: '2026-11-20T10:04:00Z',
      },
      {
        id: 'f-def',
        triggerId: 'tr-2',
        eventId: 'ev-4',
        projectId: 'proj-1',
        actionType: 'dispatch_agent',
        status: 'deferred',
        notBefore: '2026-11-20T10:10:00Z',
        deferredAt: '2026-11-20T10:02:00Z',
        startedAt: '2026-11-20T10:01:00Z',
      },
      {
        id: 'f-fired',
        triggerId: 'tr-2',
        eventId: 'ev-5',
        projectId: 'proj-1',
        actionType: 'dispatch_agent',
        status: 'fired',
        resultRef: 'agent-uuid-1234',
        startedAt: '2026-11-20T08:00:00Z',
      },
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ fires, totalCount: fires.length }))
    );

    const el = document.createElement('scion-trigger-fire-list') as ScionTriggerFireList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-fire-status="waiting"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-wait-until="f-wait"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-status="expired"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-status="coalescing"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-not-before="f-coal"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-status="deferred"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-deferred-at="f-def"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-not-before="f-def"]')).toBeTruthy();

    const agentLink = el.shadowRoot?.querySelector(
      '[data-fire-agent-link="agent-uuid-1234"]'
    ) as HTMLAnchorElement;
    expect(agentLink).toBeTruthy();
    expect(agentLink.getAttribute('href')).toBe('/agents/agent-uuid-1234');
  });

  it('ignores stale out-of-order responses when filters change rapidly (O3)', async () => {
    let resolveFirst!: (res: Response) => void;
    let callCount = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(() => {
        callCount++;
        if (callCount === 1) {
          return new Promise<Response>((resolve) => {
            resolveFirst = resolve;
          });
        }
        return Promise.resolve(
          jsonResponse({
            fires: [
              {
                id: 'f-fresh',
                triggerId: 'tr-2',
                eventId: 'ev-2',
                projectId: 'proj-1',
                status: 'fired',
                startedAt: '2026-11-20T10:00:00Z',
              },
            ],
            totalCount: 1,
          })
        );
      })
    );

    const el = document.createElement('scion-trigger-fire-list') as ScionTriggerFireList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await el.updateComplete;

    // Trigger a second load before the first resolves.
    const secondLoad = el.loadFires(false);
    await secondLoad;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-fire-row="f-fresh"]')).toBeTruthy();

    // Now resolve the first (stale) request — it must NOT overwrite f-fresh.
    resolveFirst(
      jsonResponse({
        fires: [
          {
            id: 'f-stale',
            triggerId: 'tr-1',
            eventId: 'ev-1',
            projectId: 'proj-1',
            status: 'failed',
            startedAt: '2026-11-20T09:00:00Z',
          },
        ],
        totalCount: 1,
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-fire-row="f-fresh"]')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('[data-fire-row="f-stale"]')).toBeNull();
  });
});
