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
 * `loadHubMembers` coalescing and full-pagination coverage.
 *
 * The chat sidebar's hub-members load has several call sites (route parse,
 * `initV2`'s no-conversation branch, a rail-data re-parse, `handleResetView`,
 * and a fallback poll) and used to fetch only the first page of users/agents
 * with no in-flight coalescing — a cold `/chat` open could issue several
 * overlapping full-list requests.
 *
 * Most of these tests exercise the private `loadHubMembers` method directly
 * (the element is never appended, so `connectedCallback`/`initV2` never
 * runs) to isolate the gate and pagination behaviour from the rest of the
 * page's lifecycle — that only proves same-turn calls batch correctly, not
 * that the real call sites actually land in the same turn on a real mount.
 * The "real cold mount" test below instead appends the element and drives
 * `connectedCallback` for real, to check that.
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

// The real cold-mount test drives `connectedCallback`, which awaits these
// lazy imports before its own route parse / no-conversation `loadHubMembers`
// call. Their actual implementations are irrelevant to the coalescing gate
// under test, and importing them for real would pull in unrelated component
// trees — stub them so the import resolves immediately.
vi.mock('../shared/chat/chat-space-rail.js', () => ({}));
vi.mock('../shared/chat/chat-members.js', () => ({}));

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
  it('same-turn loadHubMembers calls, all arriving before the walk starts, batch into a single walk', async () => {
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => usersPage(['u1']),
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    // Four calls back-to-back in the same synchronous turn, before any of
    // them has actually issued a network request. This only proves the
    // batching window collapses same-turn calls — see the "real cold mount"
    // test below for whether the actual call sites land in this window on a
    // real mount.
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

  it('a single refresh trigger during an in-flight load causes exactly one trailing reload', async () => {
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

    // The periodic poll is the one caller that passes `{ refresh: true }`,
    // since it exists specifically because the member list might have
    // changed since the last load. It arrives while the first walk's users
    // request is still unresolved.
    page.loadHubMembers({ refresh: true });

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

  it('three refresh triggers during one in-flight load still cause only one trailing reload', async () => {
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

    // Three independent later refresh triggers while the first walk is in
    // flight.
    page.loadHubMembers({ refresh: true });
    page.loadHubMembers({ refresh: true });
    page.loadHubMembers({ refresh: true });

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

  it('join triggers (the default — route/view re-parses) during an in-flight load do not queue a trailing reload', async () => {
    let resolveFirstUsers!: (r: Response) => void;
    const firstUsers = new Promise<Response>((resolve) => {
      resolveFirstUsers = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => firstUsers,
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    // Three later join triggers — e.g. a route re-parse and initV2's own
    // no-conversation check — arrive while the walk is in flight. None of
    // them knows of anything that could have changed, so none should queue a
    // trailing walk.
    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();

    resolveFirstUsers(usersPage(['u1']));
    await flush();

    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    expect(usersCalls.length).toBe(1);
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1']);
  });

  it('a real cold mount: connectedCallback, its lazy-import-gated route re-parse, and a rail-loaded re-parse join the same walk', async () => {
    window.history.pushState({}, '', '/chat');

    let resolveUsers!: (r: Response) => void;
    const pendingUsers = new Promise<Response>((resolve) => {
      resolveUsers = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => pendingUsers,
        () => agentsPage(['a1'])
      )
    );

    const page = document.createElement('scion-page-chat') as any;
    // The router sets pageData before inserting the page into the shell (see
    // main.ts's route rendering) — match that order here, before
    // `initV2`'s lazy rail/members imports have resolved.
    page.pageData = { user: { id: 'user-me' } };
    document.body.appendChild(page);

    // Let everything that can fire in this window actually fire: updated()'s
    // pageData branch, initV2's lazy imports and no-conversation branch, all
    // while the first walk's users request is still unresolved.
    await flush();

    // A later rail-loaded re-parse (handleRailLoaded's parseV2Route call)
    // also lands in this window on a real mount.
    page.dispatchEvent(new CustomEvent('rail-loaded', { detail: { spaceIds: [], spaces: [] } }));
    await flush();

    resolveUsers(usersPage(['u1']));
    await flush();

    try {
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
    } finally {
      document.body.removeChild(page);
    }
  });
});

describe('loadHubMembers view-change race', () => {
  it('a walk in flight when the user opens a project does not overwrite that project members with hub members', async () => {
    let resolveUsers!: (r: Response) => void;
    const pendingUsers = new Promise<Response>((resolve) => {
      resolveUsers = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => pendingUsers,
        () => agentsPage(['hub-agent'])
      )
    );
    const page = createPage();

    // A hub-wide walk starts while the global /chat view is showing.
    page.loadHubMembers();
    await flush();

    // The user opens a project before the hub walk's users request resolves.
    // loadV2Members sets v2Conversation synchronously, same as every real
    // call site (handleThreadSelect, etc.), before its own fetch resolves.
    page.v2Conversation = { projectId: 'p1' };
    page.v2HumanMembers = [{ id: 'proj-user', kind: 'user', displayName: 'Proj User' }];
    page.v2AgentMembers = [{ id: 'proj-agent', kind: 'agent', displayName: 'Proj Agent' }];
    page.v2Members = [{ id: 'proj-user', name: 'Proj User', email: '', kind: 'user' }];

    // The hub-wide walk, still in flight, now lands.
    resolveUsers(usersPage(['hub-user']));
    await flush();

    // The project's member list must survive — the hub walk was for a view
    // that's no longer on screen.
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['proj-user']);
    expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['proj-agent']);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['proj-user']);
  });

  it('a trailing walk queued before a project opens is skipped rather than overwriting that project on completion', async () => {
    let resolveFirstUsers!: (r: Response) => void;
    const firstUsers = new Promise<Response>((resolve) => {
      resolveFirstUsers = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => firstUsers,
        () => agentsPage(['hub-agent'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    // The fallback poll fires while the first walk is in flight, queuing a
    // trailing walk.
    page.loadHubMembers({ refresh: true });

    // Before that first walk settles (and before the trailing walk would
    // run), the user opens a project.
    page.v2Conversation = { projectId: 'p1' };
    page.v2HumanMembers = [{ id: 'proj-user', kind: 'user', displayName: 'Proj User' }];

    resolveFirstUsers(usersPage(['hub-user']));
    await flush();

    // Neither the settling first walk nor a trailing walk (which should not
    // even have started once a conversation is open) may publish hub members
    // over the project's.
    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['proj-user']);
    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    // Only the first walk's request — no trailing walk was started once a
    // conversation was open.
    expect(usersCalls.length).toBe(1);
  });

  it('opening then closing a conversation mid-walk does not publish a truncated list — a fresh walk publishes the full one', async () => {
    // Regression test for: a walk in flight when a project opens stops its
    // users leg at the next page boundary (a real, intentional truncation —
    // see the "walk cancellation" describe block below); if the user then
    // returns to /chat before the slower agents leg settles, the stale walk
    // must not publish that truncated users list as the full hub roster.
    // This uses a real mount (`document.body.appendChild`) rather than
    // `createPage()`, since the fix depends on `updated()` actually running
    // to bump the hub-members generation when `v2Conversation` is assigned.
    window.history.pushState({}, '', '/chat');

    let userCall = 0;
    let resolveUsersPage1!: (r: Response) => void;
    const usersPage1Promise = new Promise<Response>((resolve) => {
      resolveUsersPage1 = resolve;
    });
    let agentCall = 0;
    let resolveStaleAgents!: (r: Response) => void;
    const staleAgentsPromise = new Promise<Response>((resolve) => {
      resolveStaleAgents = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          // Call 1: the stale walk's only page, held until resolved below.
          // Call 2+: a fresh walk's own full (single-page) result.
          return userCall === 1 ? usersPage1Promise : usersPage(['u1', 'u2']);
        },
        () => {
          agentCall++;
          // Call 1: the stale walk's pending leg, held until resolved below.
          // Call 2+: a fresh walk's own result.
          return agentCall === 1 ? staleAgentsPromise : agentsPage(['a1']);
        }
      )
    );

    const page = document.createElement('scion-page-chat') as any;
    page.pageData = { user: { id: 'user-me' } };
    document.body.appendChild(page);
    // Cold mount starts the initial walk (generation 0): both legs' first
    // requests are now in flight.
    await flush();

    try {
      // The user opens a project. Setting v2Conversation is a plain field
      // write, so shouldContinue sees it immediately; the generation bump
      // that invalidates this walk for good runs on the next microtask via
      // updated() — flush() below gives it room to do so.
      page.v2Conversation = { projectId: 'p1' };
      // The users leg's only page lands with a cursor; shouldContinue is now
      // false, so it stops there instead of fetching page 2 — a genuine,
      // intentional truncation to ['u1'].
      resolveUsersPage1(usersPage(['u1'], 'u-cursor'));
      await flush();

      // Before the agents leg settles, the user returns to the global view —
      // exactly what handleResetView and the bare-/chat branch of
      // parseV2Route do.
      page.v2Conversation = null;
      page.loadHubMembers();
      await flush();

      // The stale walk's agents leg finally lands late.
      resolveStaleAgents(agentsPage(['a-stale']));
      await flush();

      // The fresh walk (started when loadHubMembers saw the stale walk
      // belonged to a superseded generation) published the full lists; the
      // stale walk's truncated ['u1']/['a-stale'] must never have been
      // published, including transiently.
      expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1', 'u2']);
      expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['a1']);

      const usersCalls = vi
        .mocked(apiFetch)
        .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
      const agentsCalls = vi
        .mocked(apiFetch)
        .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/agents'));
      // The stale walk's one users request (page 1 only — it stopped before
      // page 2) plus the fresh walk's one full-page request.
      expect(usersCalls.length).toBe(2);
      // The stale walk's one agents request plus the fresh walk's one.
      expect(agentsCalls.length).toBe(2);
    } finally {
      document.body.removeChild(page);
    }
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

  it('de-dupes a user returned on two pages (the offset-pagination boundary-shift case)', async () => {
    // /api/v1/users paginates by offset, not a keyset cursor, so a signup
    // between page 1 and page 2 can shift the boundary and return the same
    // user on both pages.
    let userCall = 0;
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          return userCall === 1 ? usersPage(['u1', 'u2'], 'u-cursor') : usersPage(['u2', 'u3']);
        },
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();

    expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u1', 'u2', 'u3']);
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

describe('loadHubMembers reconnect handling', () => {
  it('a disconnect-then-reconnect while a walk is in flight starts a fresh walk, and the stale walk publishing later does not overwrite it', async () => {
    window.history.pushState({}, '', '/chat');

    let resolveStaleUsers!: (r: Response) => void;
    const staleUsers = new Promise<Response>((resolve) => {
      resolveStaleUsers = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => staleUsers,
        () => agentsPage(['a-stale'])
      )
    );

    const page = document.createElement('scion-page-chat') as any;
    page.pageData = { user: { id: 'user-me' } };
    document.body.appendChild(page);
    // Let the cold-mount walk start; its users request is left unresolved.
    await flush();

    // The element disconnects and reconnects (e.g. moved within the DOM)
    // while that walk's users request is still pending.
    document.body.removeChild(page);
    document.body.appendChild(page);

    // The reconnected view's own walk gets fresh responses.
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => usersPage(['u-fresh']),
        () => agentsPage(['a-fresh'])
      )
    );
    await flush();

    // The stale walk, started before the reconnect, finally resolves.
    resolveStaleUsers(usersPage(['u-stale']));
    await flush();

    try {
      expect(page.v2HumanMembers.map((m: any) => m.id)).toEqual(['u-fresh']);
      expect(page.v2AgentMembers.map((m: any) => m.id)).toEqual(['a-fresh']);
    } finally {
      document.body.removeChild(page);
    }
  });

  it('a stale walk settling after reconnect does not clear the in-flight flag out from under the still-running fresh walk', async () => {
    // Regression test for _runHubMembersLoad's generation-ownership check in
    // its `finally` block: it must only clear `_hubMembersInFlight` if it is
    // still that flag's owner. If that check were made unconditional, the
    // stale walk's completion below would wrongly clear the flag while the
    // fresh walk (the true owner) is still running, and the loadHubMembers
    // call below would then start a redundant third walk instead of joining
    // the fresh one.
    window.history.pushState({}, '', '/chat');

    let userCall = 0;
    let resolveStaleUsers!: (r: Response) => void;
    const staleUsers = new Promise<Response>((resolve) => {
      resolveStaleUsers = resolve;
    });
    const freshUsers = new Promise<Response>(() => {
      // Deliberately never resolved — the fresh walk must still be in
      // flight for the rest of this test.
    });
    let agentCall = 0;
    const freshAgents = new Promise<Response>(() => {
      // Also deliberately never resolved, same reason as freshUsers.
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          return userCall === 1 ? staleUsers : freshUsers;
        },
        () => {
          agentCall++;
          return agentCall === 1 ? agentsPage(['a-stale']) : freshAgents;
        }
      )
    );

    const page = document.createElement('scion-page-chat') as any;
    page.pageData = { user: { id: 'user-me' } };
    document.body.appendChild(page);
    // Cold-mount walk (generation 0): users pending on staleUsers; agents
    // resolves immediately to ['a-stale'], so only the users leg is
    // outstanding by the time this settles.
    await flush();

    // Disconnect and reconnect bumps the generation and starts a fresh walk
    // (generation 1); both its legs are left pending throughout this test.
    document.body.removeChild(page);
    document.body.appendChild(page);
    await flush();

    try {
      // The stale (generation 0) walk's users leg finally resolves. Its own
      // publish guard discards the result (superseded generation) — already
      // covered by the test above — but its `finally` block also runs here.
      resolveStaleUsers(usersPage(['u-stale']));
      await flush();

      // A later call (e.g. a route re-parse) arrives while the fresh walk is
      // still the only thing in flight. It must join that walk rather than
      // start a third one.
      page.loadHubMembers();
      await flush();

      expect(userCall).toBe(2);
      expect(agentCall).toBe(2);
    } finally {
      document.body.removeChild(page);
    }
  });
});

describe('loadHubMembers walk cancellation', () => {
  it('stops requesting further pages once the element disconnects mid-walk', async () => {
    let userCall = 0;
    let resolvePage2!: (r: Response) => void;
    const page2 = new Promise<Response>((resolve) => {
      resolvePage2 = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          if (userCall === 1) return usersPage(['u1'], 'u-cursor');
          return page2;
        },
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();
    // First page landed with a cursor; the second page's request has gone
    // out and is unresolved.
    expect(userCall).toBe(2);

    // Disconnect while that second page's request is still in flight.
    page.disconnectedCallback();

    // The pending request resolves with yet another cursor — if the walk
    // kept going, it would fetch a third page.
    resolvePage2(usersPage(['u2'], 'u2-cursor'));
    await flush();

    expect(userCall).toBe(2);
  });

  it('stops requesting further pages once a conversation opens mid-walk', async () => {
    let userCall = 0;
    let resolvePage2!: (r: Response) => void;
    const page2 = new Promise<Response>((resolve) => {
      resolvePage2 = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(
      routeByPath(
        () => {
          userCall++;
          if (userCall === 1) return usersPage(['u1'], 'u-cursor');
          return page2;
        },
        () => agentsPage(['a1'])
      )
    );
    const page = createPage();

    page.loadHubMembers();
    await flush();
    expect(userCall).toBe(2);

    // The user opens a project before the second page resolves.
    page.v2Conversation = { projectId: 'p1' };

    resolvePage2(usersPage(['u2'], 'u2-cursor'));
    await flush();

    expect(userCall).toBe(2);
  });
});
