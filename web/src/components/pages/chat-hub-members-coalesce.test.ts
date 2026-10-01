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
 * `loadHubMembers` coalescing and full-pagination coverage (ptone/scion#2367).
 *
 * The chat sidebar's hub-members load has at least four call sites (route
 * parse, `initV2`'s no-conversation branch, a rail-data re-parse, and a
 * fallback poll) and used to fetch only the first page of users/agents with
 * no in-flight coalescing — a cold `/chat` open could issue several
 * overlapping full-list requests. These tests exercise the private
 * `loadHubMembers` method directly (the element is never appended, so
 * `connectedCallback`/`initV2` never runs) to isolate the gate and
 * pagination behaviour from the rest of the page's lifecycle.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: Object.assign(new EventTarget(), { seedAgents: vi.fn() }),
}));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

let ScionPageChat: any;

beforeAll(async () => {
  const mod = await import('./chat.js');
  ScionPageChat = mod.ScionPageChat;
  expect(ScionPageChat).toBeDefined();
});

function createPage(): any {
  const el = document.createElement('scion-page-chat') as any;
  el.v2HumanMembers = [];
  el.v2AgentMembers = [];
  el.v2Members = [];
  return el;
}

function usersPage(ids: string[], nextCursor?: string): Response {
  const users = ids.map((id) => ({ id, displayName: id }));
  return new Response(JSON.stringify({ users, ...(nextCursor ? { nextCursor } : {}) }), {
    status: 200,
  });
}

function agentsPage(ids: string[], nextCursor?: string): Response {
  const agents = ids.map((id) => ({ id, name: id }));
  return new Response(JSON.stringify({ agents, ...(nextCursor ? { nextCursor } : {}) }), {
    status: 200,
  });
}

/** Routes a mocked `apiFetch` call to a users or agents responder by path. */
function routeByPath(
  usersFn: (url: string) => Response | Promise<Response>,
  agentsFn: (url: string) => Response | Promise<Response>
): (url: string) => Promise<Response> {
  return async (url: string) => {
    if (url.startsWith('/api/v1/users')) return usersFn(url);
    if (url.startsWith('/api/v1/agents')) return agentsFn(url);
    return new Response('{}', { status: 200 });
  };
}

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
});

/** Flushes pending microtasks (promise chains, `Response.json()`, etc.) enough times to settle `loadHubMembers`'s internal awaits. */
async function flush(ticks = 30): Promise<void> {
  for (let i = 0; i < ticks; i++) {
    await Promise.resolve();
  }
}

afterEach(() => {
  vi.useRealTimers();
});

describe('loadHubMembers coalescing gate', () => {
  it('a cold mount that triggers all the call sites issues exactly one users walk and one agents walk', async () => {
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => usersPage(['u1']),
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    // Simulate the four real call sites firing back-to-back in the same
    // synchronous turn, exactly as they do during a cold `/chat` mount
    // (route parse, initV2's no-conversation branch, a rail-data re-parse,
    // and the fallback poll's first tick all reach this method before any
    // of them has actually issued a network request).
    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();

    // Let the gate's queued microtask (and the resulting fetch promises)
    // settle.
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    const agentsCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/agents'));
    expect(usersCalls.length).toBe(1);
    expect(agentsCalls.length).toBe(1);
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1']);
    expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['a1']);
  });

  it('a single trigger during an in-flight load causes exactly one trailing reload', async () => {
    let resolveFirstUsers!: (r: Response) => void;
    const firstUsers = new Promise<Response>((resolve) => {
      resolveFirstUsers = resolve;
    });
    let usersCallCount = 0;
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          usersCallCount++;
          return usersCallCount === 1 ? firstUsers : Promise.resolve(usersPage(['u2']));
        },
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    // Let the gate's queued microtask run and the first walk's requests
    // actually go out — this is the "in flight" window.
    await flush();

    // A genuinely later trigger (e.g. the periodic poll) arrives while the
    // first walk's users request is still unresolved.
    page.loadHubMembers();

    resolveFirstUsers(usersPage(['u1']));
    // Flush the first walk's completion and the trailing reload it queues.
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    const agentsCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/agents'));
    // One walk from the first trigger, one trailing walk from the second.
    expect(usersCalls.length).toBe(2);
    expect(agentsCalls.length).toBe(2);
    // The trailing reload's result is what's actually shown.
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u2']);
  });

  it('three triggers during one in-flight load still cause only one trailing reload', async () => {
    let resolveFirstUsers!: (r: Response) => void;
    const firstUsers = new Promise<Response>((resolve) => {
      resolveFirstUsers = resolve;
    });
    let usersCallCount = 0;
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          usersCallCount++;
          return usersCallCount === 1 ? firstUsers : Promise.resolve(usersPage(['u2']));
        },
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    // Three independent later triggers while the first walk is in flight.
    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();

    resolveFirstUsers(usersPage(['u1']));
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    expect(usersCalls.length).toBe(2); // one walk + exactly one trailing reload
  });

  it('issues no extra request when nothing new is triggered', async () => {
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => usersPage(['u1']),
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    expect(usersCalls.length).toBe(1);
  });
});

describe('loadHubMembers full pagination', () => {
  it('walks every page for over-100 agents and over-100 users, showing all of them, with one request per page', async () => {
    const userIds = Array.from({ length: 120 }, (_, i) => `u${i}`);
    const agentIds = Array.from({ length: 150 }, (_, i) => `a${i}`);
    const userPage1 = userIds.slice(0, 100);
    const userPage2 = userIds.slice(100);
    const agentPage1 = agentIds.slice(0, 100);
    const agentPage2 = agentIds.slice(100);

    let userCall = 0;
    let agentCall = 0;
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          return userCall === 1 ? usersPage(userPage1, 'u-cursor') : usersPage(userPage2);
        },
        () => {
          agentCall++;
          return agentCall === 1 ? agentsPage(agentPage1, 'a-cursor') : agentsPage(agentPage2);
        }
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    const agentsCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/agents'));
    expect(usersCalls.length).toBe(2);
    expect(agentsCalls.length).toBe(2);
    expect(page.v2HumanMembers.length).toBe(120);
    expect(page.v2AgentMembers.length).toBe(150);
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(userIds);
    expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(agentIds);
  });
});

describe('loadHubMembers error handling', () => {
  it('a failed second page keeps the previous members', async () => {
    const page = createPage();

    // First, successful load.
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => usersPage(['u1']),
        () => agentsPage(['a1'])
      )
    );
    page.loadHubMembers();
    await flush();
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1']);
    expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['a1']);

    // Second load: the users walk's second page fails; agents succeed.
    let userCall = 0;
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          return userCall === 1 ? usersPage(['u2'], 'u-cursor') : new Response('', { status: 500 });
        },
        () => agentsPage(['a2'])
      )
    );
    page.loadHubMembers();
    await flush();

    // Users list is unchanged from before the failed walk; agents updated.
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1']);
    expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['a2']);
  });
});
