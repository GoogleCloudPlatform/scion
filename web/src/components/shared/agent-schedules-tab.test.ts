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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionAgentSchedulesTab } from './agent-schedules-tab.js';

let scheduleDetailHref: (projectId: string, scheduleId: string) => string;

function makeSchedule(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'sched-1',
    projectId: 'proj-1',
    name: 'daily-check',
    cronExpr: '0 9 * * *',
    timeZone: 'UTC',
    eventType: 'message',
    payload: '{"agentName":"alpha","message":"ping"}',
    status: 'active',
    nextRunAt: '2026-12-01T09:00:00Z',
    lastRunAt: '2026-11-24T09:00:00Z',
    lastRunStatus: 'success',
    runCount: 5,
    errorCount: 0,
    createdAt: '2026-11-01T00:00:00Z',
    ...overrides,
  };
}

async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 30));
  await el.updateComplete;
}

describe('scion-agent-schedules-tab', () => {
  beforeAll(async () => {
    const mod = await import('./agent-schedules-tab.js');
    scheduleDetailHref = mod.scheduleDetailHref;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('builds encoded detail card hrefs', () => {
    expect(scheduleDetailHref('proj 1', 'sched/2')).toBe(
      '/projects/proj%201/schedules?schedule=sched%2F2'
    );
  });

  it('fetches createdBy and targetAgentId lists and renders both groups with links', async () => {
    const requestedUrls: string[] = [];
    const created = [
      makeSchedule({
        id: 'sched-created-1',
        name: 'outbound-ping',
        eventType: 'message',
        payload: '{"agentName":"beta","message":"hi"}',
        cronExpr: '0 * * * *',
        status: 'active',
        lastRunStatus: 'success',
      }),
    ];
    const targeting = [
      makeSchedule({
        id: 'sched-target-wake',
        name: 'wake-alpha',
        eventType: 'message',
        payload: '{"agentName":"alpha","message":"wake","wake":true}',
        cronExpr: '*/15 * * * *',
        status: 'paused',
        statusReason: 'target_deleted',
        lastRunStatus: 'error',
        lastRunError: 'agent offline',
      }),
      makeSchedule({
        id: 'sched-target-exec',
        name: 'health-exec',
        eventType: 'exec',
        payload: '{"agentId":"agent-1","commandRedacted":true,"argc":2}',
        cronExpr: '0 12 * * *',
        status: 'active',
        lastRunAt: undefined,
        lastRunStatus: undefined,
      }),
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        requestedUrls.push(url);
        const items = url.includes('createdBy=agent-1')
          ? created
          : url.includes('targetAgentId=agent-1')
            ? targeting
            : [];
        return Promise.resolve(
          new Response(JSON.stringify({ schedules: items, totalCount: items.length }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const el = document.createElement('scion-agent-schedules-tab') as ScionAgentSchedulesTab;
    el.projectId = 'proj-1';
    el.agentId = 'agent-1';
    document.body.appendChild(el);
    await el.loadSchedules();
    await settle(el);

    expect(
      requestedUrls.some((u) => u.includes('/api/v1/projects/proj-1/schedules?createdBy=agent-1'))
    ).toBe(true);
    expect(
      requestedUrls.some((u) =>
        u.includes('/api/v1/projects/proj-1/schedules?targetAgentId=agent-1')
      )
    ).toBe(true);

    const root = el.shadowRoot as ShadowRoot;
    const createdSection = root.querySelector('[data-testid="agent-schedules-created"]')!;
    const targetSection = root.querySelector('[data-testid="agent-schedules-targeting"]')!;

    const createdLink = createdSection.querySelector('a.schedule-link') as HTMLAnchorElement;
    expect(createdLink.textContent?.trim()).toBe('outbound-ping');
    expect(createdLink.getAttribute('href')).toBe(
      '/projects/proj-1/schedules?schedule=sched-created-1'
    );
    expect(createdSection.textContent).toContain('0 * * * *');
    expect(createdSection.textContent).toContain('active');
    expect(createdSection.textContent).toContain('success');

    const targetLinks = Array.from(
      targetSection.querySelectorAll('a.schedule-link')
    ) as HTMLAnchorElement[];
    expect(targetLinks.map((a) => a.textContent?.trim())).toEqual(['wake-alpha', 'health-exec']);
    expect(targetLinks.map((a) => a.getAttribute('href'))).toEqual([
      '/projects/proj-1/schedules?schedule=sched-target-wake',
      '/projects/proj-1/schedules?schedule=sched-target-exec',
    ]);
    expect(targetSection.textContent).toContain('wake');
    expect(targetSection.textContent).toContain('exec');
    expect(targetSection.textContent).toContain('agent offline');
    expect(targetSection.textContent).toContain('Never run');

    // Second loadSchedules() call without force is a no-op.
    const callCount = requestedUrls.length;
    await el.loadSchedules();
    expect(requestedUrls.length).toBe(callCount);
  });

  it('renders empty states when no schedules relate to the agent', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ schedules: [], totalCount: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );

    const el = document.createElement('scion-agent-schedules-tab') as ScionAgentSchedulesTab;
    el.projectId = 'proj-1';
    el.agentId = 'agent-empty';
    document.body.appendChild(el);
    await el.loadSchedules();
    await settle(el);

    const root = el.shadowRoot as ShadowRoot;
    expect(root.querySelector('[data-testid="agent-schedules-created"]')?.textContent).toContain(
      'No schedules created by this agent.'
    );
    expect(root.querySelector('[data-testid="agent-schedules-targeting"]')?.textContent).toContain(
      'No schedules targeting this agent.'
    );
  });

  it('shows hub error message when read authorization fails (403)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ error: { message: 'Access denied to schedules' } }), {
            status: 403,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );

    const el = document.createElement('scion-agent-schedules-tab') as ScionAgentSchedulesTab;
    el.projectId = 'proj-1';
    el.agentId = 'agent-1';
    document.body.appendChild(el);
    await el.loadSchedules();
    await settle(el);

    const root = el.shadowRoot as ShadowRoot;
    expect(root.querySelector('.error-details')?.textContent).toContain(
      'Access denied to schedules'
    );
  });
});
