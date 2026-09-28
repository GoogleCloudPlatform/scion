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
 * Regression test for the members sidebar flicker (investigation
 * nc-member-sort-inv): a stale `stateManager` agent entry that the server no
 * longer returns (e.g. deleted while the client was disconnected, so the SSE
 * `deleted` event was missed) used to toggle in and out of the sidebar
 * forever — every SSE agent tick re-added it from stateManager, which
 * scheduled a debounced authoritative refetch, which dropped it again, which
 * the next SSE tick undid. See `chat-member-flicker.repro.test.ts` on branch
 * `scion/nc-member-sort-inv` for the pre-fix repro.
 *
 * The fix has two parts, both exercised here:
 *  1. `loadV2Members` reconciles `stateManager` against the authoritative
 *     response, removing any agent the server no longer lists for the
 *     project (`stateManager.removeAgent`).
 *  2. `_handleAgentsUpdated` won't resurrect an ID a refetch just omitted
 *     until a genuine SSE `created` event confirms it (`agent-created`),
 *     which guards against the entry being reintroduced by some other
 *     consumer of the shared `stateManager` (e.g. another page re-seeding).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const fakeState = vi.hoisted(() => {
  const t = new EventTarget() as any;
  t.agents = new Map<string, any>();
  t.getAgents = () => Array.from(t.agents.values());
  t.getDeletedAgentIds = () => new Set<string>();
  t.seedAgents = (list: any[]) => list.forEach((a) => t.agents.set(a.id, a)); // additive, like the real one
  t.removeAgent = (id: string) => t.agents.delete(id); // matches real stateManager.removeAgent: no notify
  return t;
});

vi.mock('../../client/main.js', () => ({ navigateTo: vi.fn(), stateManager: fakeState }));
vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  fakeState.agents.clear();
  vi.mocked(apiFetch).mockReset();
});

afterEach(() => vi.useRealTimers());

// Stale stateManager entry: never removed by an SSE `deleted` event because
// the client missed it (backgrounded tab, dropped connection).
const ghost = {
  id: 'ghost',
  name: 'ci-compat-literals-dev',
  slug: 'ci-compat-literals-dev',
  projectId: 'p1',
  phase: 'running',
  activity: 'blocked',
};
const live = {
  id: 'live',
  name: 'custom-auth-proxy-lead',
  slug: 'custom-auth-proxy-lead',
  projectId: 'p1',
  phase: 'running',
  activity: 'executing',
};

/** The `/members` REST shape for the `live` agent — the server never lists `ghost`. */
const liveMemberResponse = {
  id: 'live',
  kind: 'agent',
  displayName: 'custom-auth-proxy-lead',
  slug: 'custom-auth-proxy-lead',
  projectId: 'p1',
};

function mockMembersResponse(agents: unknown[]): void {
  vi.mocked(apiFetch).mockImplementation(
    async () => new Response(JSON.stringify({ humans: [], agents }), { status: 200 })
  );
}

function createPage(): any {
  const page = document.createElement('scion-page-chat') as any;
  page.v2Conversation = { projectId: 'p1', isDM: false };
  page.v2AgentMembers = [];
  return page;
}

describe('member list flicker fix', () => {
  it('keeps the ghost absent after one refetch, across further SSE ticks, with no extra /members fetches', async () => {
    vi.useFakeTimers();
    fakeState.agents.set('ghost', ghost);
    fakeState.agents.set('live', live);
    mockMembersResponse([liveMemberResponse]);

    const page = createPage();

    // The authoritative refetch (what the debounced canAttach refresh
    // eventually calls) reconciles stateManager against the response.
    await page.loadV2Members('p1');
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(1);
    expect(page.v2AgentMembers.some((a: any) => a.id === 'ghost')).toBe(false);
    expect(fakeState.agents.has('ghost')).toBe(false);

    // Simulate the ghost resurfacing in the shared stateManager (e.g. another
    // page reseeding it) to prove the loop guard, not just the removal,
    // holds it back.
    fakeState.agents.set('ghost', ghost);

    const ghostVisible: boolean[] = [];
    for (let i = 0; i < 5; i++) {
      page._handleAgentsUpdated(); // SSE tick, as any agent status update triggers
      ghostVisible.push(page.v2AgentMembers.some((a: any) => a.id === 'ghost'));
      await vi.advanceTimersByTimeAsync(1100); // would-be canAttach refetch window
    }

    expect(ghostVisible).toEqual([false, false, false, false, false]);
    // No further /members fetch fired from the flicker loop.
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(1);
  });

  it('lets a legitimately new agent appear immediately via SSE created', async () => {
    vi.useFakeTimers();
    fakeState.agents.set('live', live);
    mockMembersResponse([liveMemberResponse]);

    const page = createPage();
    await page.loadV2Members('p1');
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['live']);

    // A brand-new agent (never touched by the loop guard) arrives via SSE.
    const created = { id: 'newagent', name: 'fresh-agent', projectId: 'p1' };
    fakeState.agents.set('newagent', created);
    page._handleAgentsUpdated();

    expect(page.v2AgentMembers.some((a: any) => a.id === 'newagent')).toBe(true);
  });

  it('lifts the loop guard once a genuine SSE created event confirms the ID', async () => {
    vi.useFakeTimers();
    fakeState.agents.set('ghost', ghost);
    fakeState.agents.set('live', live);
    mockMembersResponse([liveMemberResponse]);

    const page = createPage();
    await page.loadV2Members('p1');
    expect(page.v2AgentMembers.some((a: any) => a.id === 'ghost')).toBe(false);

    // Without a created confirmation, resurfacing in stateManager is ignored.
    fakeState.agents.set('ghost', ghost);
    page._handleAgentsUpdated();
    expect(page.v2AgentMembers.some((a: any) => a.id === 'ghost')).toBe(false);

    // A genuine SSE `created` event for the ID lifts the guard.
    page._handleAgentCreated(
      new CustomEvent('agent-created', { detail: { data: { agentId: 'ghost' } } })
    );
    page._handleAgentsUpdated();
    expect(page.v2AgentMembers.some((a: any) => a.id === 'ghost')).toBe(true);
  });
});
