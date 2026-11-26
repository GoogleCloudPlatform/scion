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
import './admin-scheduler.js';
import type { ScionPageAdminScheduler } from './admin-scheduler.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-page-admin-scheduler durableQueues', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders durableQueues table when present in /api/v1/admin/scheduler response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/api/v1/admin/scheduler')) {
          return Promise.resolve(
            jsonResponse({
              scheduler: {
                tickCount: 10,
                tickInterval: '1m',
                recurringHandlers: [],
                eventHandlers: [],
                activeTimers: 0,
              },
              scheduledEvents: [],
              recurringSchedules: [],
              durableQueues: [
                {
                  kind: 'trigger.event',
                  pending: 4,
                  leased: 1,
                  oldestAgeSeconds: 12.5,
                },
              ],
            })
          );
        }
        return Promise.resolve(jsonResponse({ projects: [] }));
      })
    );

    const el = document.createElement('scion-page-admin-scheduler') as ScionPageAdminScheduler;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;

    const section = el.shadowRoot?.querySelector('[data-durable-queues-section]');
    expect(section).toBeTruthy();
    const row = section?.querySelector('[data-durable-queue-row="trigger.event"]');
    expect(row).toBeTruthy();
    expect(row?.textContent).toContain('4');
    expect(row?.textContent).toContain('1');
    expect(row?.textContent).toContain('12.5s');
  });

  it('omits durableQueues section when absent from response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/api/v1/admin/scheduler')) {
          return Promise.resolve(
            jsonResponse({
              scheduler: {
                tickCount: 10,
                tickInterval: '1m',
                recurringHandlers: [],
                eventHandlers: [],
                activeTimers: 0,
              },
              scheduledEvents: [],
              recurringSchedules: [],
            })
          );
        }
        return Promise.resolve(jsonResponse({ projects: [] }));
      })
    );

    const el = document.createElement('scion-page-admin-scheduler') as ScionPageAdminScheduler;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-durable-queues-section]')).toBeNull();
  });
});
