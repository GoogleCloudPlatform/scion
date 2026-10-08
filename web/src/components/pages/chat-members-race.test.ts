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
 * Regression test: loadV2Members and refreshHubMemberPresence used to
 * assign/merge into the sidebar member arrays unconditionally once their
 * fetch resolved, with no check that the view which started the request was
 * still the active one. A later-started, earlier-resolving
 * request would then be clobbered by an earlier-started, later-resolving
 * one.
 *
 * loadV2Members and refreshHubMemberPresence are guarded by
 * `_membersViewSeq`, which loadV2Members and loadHubMembers both bump. The
 * hub loads (the users walk, and the agent store's hub list) are guarded by
 * their own `v2Conversation`/generation checks instead; the hub cases below
 * pin that a stale hub load still cannot overwrite a project view, whichever
 * guard does it. Cases, in `it` order:
 *
 *  1. Switching from project A to project B before A's response arrives —
 *     A's stale response must not overwrite B's members.
 *  2. Leaving a project for the hub view while a late project response
 *     arrives — it must not overwrite the hub members.
 *  3. A stale project response whose headers resolve — via an explicit
 *     macrotask flush that lets the first load's headers settle before the
 *     second load starts — but whose body (`res.json()`) only resolves after
 *     the newer load has already finished and written the sidebar. This pins
 *     the guard's *position*: it must run after the body is read, not
 *     merely after the headers, or this case cannot fail.
 *  4. The same as case 2, but the hub call only joins a walk that is already
 *     in flight — loadHubMembers must still claim the sidebar for the hub,
 *     so the late project response is discarded.
 *  5. Leaving the hub for a project while a hub walk is in flight — the walk
 *     landing late must not overwrite the project members.
 *  6. The same race as case 5, but with the hub users response's body
 *     deferred until after the project has written the sidebar.
 *  7. A late hub-presence merge landing after the user has moved to a
 *     project — it must not merge onto the project's members.
 *  8. The same race as case 7, but with the presence response's body
 *     deferred — pins that refreshHubMemberPresence's guard must run after
 *     its body is read, not merely after its headers.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';
import { apiFetch } from '../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', async () => ({
  ...(await import('../../client/__fixtures__/main-stub.js')),
  stateManager: {
    getAgents: () => [],
    getDeletedAgentIds: () => new Set<string>(),
    seedAgents: () => {},
    removeAgent: () => {},
  },
}));

/** Pending reads of the agent store's hub list, oldest first. */
const hubAgentReads = vi.hoisted(() => [] as Array<(snapshot: unknown) => void>);

// The hub view's agents come from the agent store: each read stays pending
// until the test resolves it.
vi.mock('../../client/agent-store.js', () => ({
  agentStore: {
    ensure: () =>
      new Promise((resolve) => {
        hubAgentReads.push(resolve);
      }),
    peek: () => undefined,
    retain: () => () => {},
  },
}));

/** Resolve every pending hub-list read with `agents`. */
function resolveHubAgents(agents: Array<{ id: string; name: string }>): void {
  const snapshot = { key: 'hub', agents, status: 'ready', complete: true, version: 1 };
  for (const resolve of hubAgentReads.splice(0)) resolve(snapshot);
}

/** One pending fetch the test can resolve on demand, keyed by call order. */
interface PendingFetch {
  url: string;
  resolve: (body: unknown) => void;
}

let pending: PendingFetch[];

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(
      (url: string) =>
        new Promise<Response>((resolve) => {
          pending.push({
            url,
            resolve: (body: unknown) =>
              resolve(new Response(JSON.stringify(body), { status: 200 })),
          });
        })
    ),
  };
});

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  pending = [];
  hubAgentReads.length = 0;
  vi.mocked(apiFetch).mockClear();
});

function createPage(): any {
  const page = document.createElement('scion-page-chat') as any;
  return page;
}

/** Resolve the oldest not-yet-resolved fetch matching `urlSubstring`. */
function resolveFetch(urlSubstring: string, body: unknown): void {
  const idx = pending.findIndex((p) => p.url.includes(urlSubstring));
  if (idx === -1) throw new Error(`no pending fetch matching ${urlSubstring}`);
  const [p] = pending.splice(idx, 1);
  p.resolve(body);
}

/** Drain pending microtasks (and the hub walk's queued start) via one macrotask. */
function flushMacrotask(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function membersBody(tag: string) {
  return {
    humans: [{ id: `human-${tag}`, kind: 'user', displayName: `Human ${tag}` }],
    agents: [{ id: `agent-${tag}`, kind: 'agent', displayName: `Agent ${tag}` }],
  };
}

describe('members sidebar stale-response guard', () => {
  it("aborts the previous view's members request when another view claims the sidebar", async () => {
    const page = createPage();
    const signalFor = (projectId: string): AbortSignal | undefined => {
      const call = vi.mocked(apiFetch).mock.calls.find((c) => String(c[0]).includes(projectId)) as
        | unknown[]
        | undefined;
      return (call?.[1] as RequestInit | undefined)?.signal ?? undefined;
    };

    void page.loadV2Members('project-a');
    void page.loadV2Members('project-b');
    expect(signalFor('project-a')?.aborted).toBe(true);
    expect(signalFor('project-b')?.aborted).toBe(false);

    // Leaving for the hub view stops the project request as well.
    page.loadHubMembers();
    expect(signalFor('project-b')?.aborted).toBe(true);
  });

  it('keeps project B members when project A resolves after B (out of order)', async () => {
    const page = createPage();

    const loadA = page.loadV2Members('project-a');
    const loadB = page.loadV2Members('project-b');

    // B's response arrives first (e.g. a faster/closer backend), then A's
    // stale response for the view the user already left arrives late.
    resolveFetch('project-b', membersBody('b'));
    await loadB;
    resolveFetch('project-a', membersBody('a'));
    await loadA;

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['human-b']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['agent-b']);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['human-b', 'agent-b']);
  });

  it('keeps hub members when a stale project response arrives after leaving for the hub', async () => {
    const page = createPage();

    const loadProject = page.loadV2Members('project-a');
    page.loadHubMembers();
    // Let the hub walk's queued start run so its requests go out.
    await flushMacrotask();

    // The hub's two parallel requests resolve first — the user has already
    // left the project view.
    resolveFetch('/api/v1/users', { users: [{ id: 'hub-user', displayName: 'Hub User' }] });
    resolveHubAgents([{ id: 'hub-agent', name: 'Hub Agent' }]);
    await flushMacrotask();
    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['hub-user']);

    // The late project response for the view the user already left arrives after.
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['hub-user']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['hub-agent']);
  });

  it("keeps project B members when project A's body resolves after B has already written the sidebar", async () => {
    const page = createPage();

    // Project A's headers resolve right away, but its body (res.json())
    // stays pending until explicitly released below — the staleness check
    // must still catch it once the body finally arrives.
    let resolveABody!: (value: unknown) => void;
    const aBody = new Promise((resolve) => {
      resolveABody = resolve;
    });
    vi.mocked(apiFetch).mockImplementationOnce(
      () => Promise.resolve({ ok: true, json: () => aBody }) as unknown as Promise<Response>
    );

    const loadA = page.loadV2Members('project-a');

    // Flush a macrotask so A's headers resolve — and it parks on its
    // deferred body — before B starts. Without this, B's synchronous seq
    // bump (inside loadV2Members, before its own await) always happens
    // before A's continuation gets a turn, so A already sees a stale seq
    // as soon as it resumes at all — this case would then pass regardless
    // of whether the guard sits before or after the body await, and could
    // never catch a guard moved back to the headers position.
    await new Promise((resolve) => setTimeout(resolve, 0));

    const loadB = page.loadV2Members('project-b');

    // B's headers and body both resolve while A's body is still pending.
    resolveFetch('project-b', membersBody('b'));
    await loadB;

    // A's body arrives only now, after B has already written the sidebar.
    resolveABody(membersBody('a'));
    await loadA;

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['human-b']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['agent-b']);
  });

  it('keeps hub members when a stale project response arrives after a hub call that joined an in-flight walk', async () => {
    const page = createPage();

    // A hub walk is already in flight.
    page.loadHubMembers();
    await flushMacrotask();

    // The user opens project A, then returns to the hub before A's response
    // arrives. The element is not mounted, so updated() never runs and the
    // hub walk's generation is unchanged: the return-to-hub call joins the
    // walk already in flight rather than starting a new one.
    page.v2Conversation = { projectId: 'project-a' };
    const loadProject = page.loadV2Members('project-a');
    page.v2Conversation = null;
    page.loadHubMembers();
    await flushMacrotask();
    const usersCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => (c[0] as string).startsWith('/api/v1/users'));
    expect(usersCalls.length).toBe(1);

    resolveFetch('/api/v1/users', { users: [{ id: 'hub-user', displayName: 'Hub User' }] });
    resolveHubAgents([{ id: 'hub-agent', name: 'Hub Agent' }]);
    await flushMacrotask();
    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['hub-user']);

    // A's response, for the view the user already left, arrives late.
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['hub-user']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['hub-agent']);
  });

  it('keeps project members when a stale hub walk lands after leaving the hub for a project', async () => {
    const page = createPage();

    page.loadHubMembers();
    // The walk's requests go out — both legs' only page is now in flight, so
    // the walk's per-page check has already passed.
    await flushMacrotask();

    // The user opens a project. Every real call site assigns v2Conversation
    // before calling loadV2Members.
    page.v2Conversation = { projectId: 'project-a' };
    const loadProject = page.loadV2Members('project-a');
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    // The hub walk, for the view the user already left, lands late.
    resolveFetch('/api/v1/users', { users: [{ id: 'hub-user', displayName: 'Hub User' }] });
    resolveHubAgents([{ id: 'hub-agent', name: 'Hub Agent' }]);
    await flushMacrotask();

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['human-a']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['agent-a']);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['human-a', 'agent-a']);
  });

  it('keeps project members when the hub users body resolves after the project has already written the sidebar', async () => {
    const page = createPage();

    // The hub users response's headers resolve right away, but its body
    // (res.json()) stays pending until explicitly released below. The users
    // leg issues the walk's first request.
    let resolveUsersBody!: (value: unknown) => void;
    const usersBody = new Promise((resolve) => {
      resolveUsersBody = resolve;
    });
    vi.mocked(apiFetch).mockImplementationOnce(
      () => Promise.resolve({ ok: true, json: () => usersBody }) as unknown as Promise<Response>
    );

    page.loadHubMembers();
    await flushMacrotask();

    // The agents leg completes, and the users leg parks on its body.
    resolveHubAgents([{ id: 'hub-agent', name: 'Hub Agent' }]);
    await flushMacrotask();

    page.v2Conversation = { projectId: 'project-a' };
    const loadProject = page.loadV2Members('project-a');
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    // The hub users body arrives only now, after the project has already
    // written the sidebar.
    resolveUsersBody({ users: [{ id: 'hub-user', displayName: 'Hub User' }] });
    await flushMacrotask();

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['human-a']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['agent-a']);
  });

  it("leaves a project member's presence unchanged when a stale hub presence response arrives late", async () => {
    const page = createPage();

    const loadPresence = page.refreshHubMemberPresence('hub-project');
    const loadProject = page.loadV2Members('project-a');

    // The project response resolves first — the user has already left for
    // a project before the hub presence fetch (started while still on the
    // hub view) completes.
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    expect(page.v2HumanMembers[0].presenceState).toBe('');

    // The late presence response names the project's own human member with
    // a presence state — it must not be merged in, since it was fetched
    // for a view the user already left.
    resolveFetch('/api/v1/chat/spaces/hub-project/members', {
      humans: [{ id: 'human-a', presenceState: 'active' }],
    });
    await loadPresence;

    expect(page.v2HumanMembers[0].presenceState).toBe('');
  });

  it("leaves a project member's presence unchanged when the presence body resolves after project has already written the sidebar", async () => {
    const page = createPage();

    // The presence response's headers resolve right away, but its body
    // (res.json()) stays pending until explicitly released below.
    let resolvePresenceBody!: (value: unknown) => void;
    const presenceBody = new Promise((resolve) => {
      resolvePresenceBody = resolve;
    });
    vi.mocked(apiFetch).mockImplementationOnce(
      () => Promise.resolve({ ok: true, json: () => presenceBody }) as unknown as Promise<Response>
    );

    const loadPresence = page.refreshHubMemberPresence('hub-project');

    // Flush a macrotask so the presence request's headers resolve — and it
    // parks on its deferred body — before the project load starts and
    // bumps the seq.
    await new Promise((resolve) => setTimeout(resolve, 0));

    const loadProject = page.loadV2Members('project-a');
    resolveFetch('project-a', membersBody('a'));
    await loadProject;

    expect(page.v2HumanMembers[0].presenceState).toBe('');

    // The presence body arrives only now, after the project has already
    // written the sidebar.
    resolvePresenceBody({ humans: [{ id: 'human-a', presenceState: 'active' }] });
    await loadPresence;

    expect(page.v2HumanMembers[0].presenceState).toBe('');
  });
});
