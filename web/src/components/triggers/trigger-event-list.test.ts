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
import './trigger-event-list.js';
import type { ScionTriggerEventList } from './trigger-event-list.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-trigger-event-list', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('paginates events and opens event detail with payload and fires', async () => {
    const ev1 = {
      id: 'ev-1',
      projectId: 'proj-1',
      source: { kind: 'webhook', id: 'ep-1' },
      type: 'webhook.received',
      subject: 'webhook:ep-1',
      dedupKey: 'deliv-1',
      occurredAt: '2026-11-20T10:00:00Z',
      receivedAt: '2026-11-20T10:00:00Z',
      attributes: { endpoint_name: 'ci-deploys' },
      payloadBytes: 42,
      status: 'processed',
      matchCount: 1,
      fireCount: 1,
      createdAt: '2026-11-20T10:00:00Z',
    };
    const ev2 = { ...ev1, id: 'ev-2', dedupKey: 'deliv-2' };

    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/trigger-events/ev-1')) {
        return Promise.resolve(
          jsonResponse({
            ...ev1,
            headers: { 'X-Scion-Delivery': 'deliv-1' },
            payload: { service: 'payments' },
          })
        );
      }
      if (url.includes('/trigger-fires?eventId=ev-1')) {
        return Promise.resolve(
          jsonResponse({
            fires: [
              {
                id: 'fire-1',
                triggerId: 'tr-1',
                eventId: 'ev-1',
                projectId: 'proj-1',
                status: 'fired',
                resultRef: 'msg-1',
                startedAt: '2026-11-20T10:00:01Z',
              },
            ],
            totalCount: 1,
          })
        );
      }
      if (url.includes('cursor=cur-1')) {
        return Promise.resolve(jsonResponse({ events: [ev2], totalCount: 2 }));
      }
      return Promise.resolve(jsonResponse({ events: [ev1], nextCursor: 'cur-1', totalCount: 2 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-trigger-event-list') as ScionTriggerEventList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('[data-event-row]')).toHaveLength(1);

    // Load more
    (el.shadowRoot?.querySelector('[data-load-more-events]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('[data-event-row]')).toHaveLength(2);

    // Open detail
    await el.openEventDetail('ev-1');
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-event-payload]')?.textContent).toContain(
      '"service": "payments"'
    );
    expect(el.shadowRoot?.textContent).toContain('Fires Produced (1)');
  });
});
