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
 * The chat sidebar's hub view: the most recently seen users (one bounded
 * `/api/v1/users` page, shared page-wide and reused for a few minutes; see
 * client/user-directory.ts) and every agent (the agent store's hub
 * list, retained by the page). Users requests go through the mocked
 * `apiFetch`; agent-list requests go to the store's in-memory server.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import type { AgentStore } from '../../client/agent-store.js';
import { agent, createHarness, settle } from '../../client/__fixtures__/agent-store-harness.js';
import type { Harness } from '../../client/__fixtures__/agent-store-harness.js';
import type { Agent } from '../../shared/types.js';
import type { ChatAgentMember, ChatHumanMember } from '../shared/chat/chat-members.js';
import { resetUserDirectory } from '../../client/user-directory.js';

/** The global agent map, as far as the page uses it: rows seeded into it are kept. */
const globalMap = vi.hoisted(() => {
  const agents = new Map<string, Agent>();
  const target = new EventTarget();
  return {
    agents,
    stateManager: Object.assign(target, {
      seedAgents: (list: Agent[]): void => {
        for (const a of list) agents.set(a.id, a);
      },
      getAgent: (id: string): Agent | undefined => agents.get(id),
      getAgents: (): Agent[] => Array.from(agents.values()),
      getDeletedAgentIds: (): Set<string> => new Set<string>(),
      removeAgent: (id: string): void => {
        agents.delete(id);
      },
      setScope: (): void => {},
      // Seed epochs record nothing here.
      scopeGeneration: 0,
      beginSeedEpoch: (): symbol => Symbol('seed-epoch'),
      endSeedEpoch: (): void => {},
    }),
  };
});

vi.mock('../../client/main.js', async () => ({
  ...(await import('../../client/__fixtures__/main-stub.js')),
  replaceRoute: vi.fn((path: string) => {
    window.history.replaceState({}, '', path);
    return Promise.resolve();
  }),
  stateManager: globalMap.stateManager,
}));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

let harness: Harness;

/** The store singleton, as the chat page sees it: this test's store. */
vi.mock('../../client/agent-store.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/agent-store.js')>();
  const agentStore = new Proxy({} as AgentStore, {
    get: (_target, key): unknown => {
      const store = harness.store;
      const value: unknown = Reflect.get(store, key);
      return typeof value === 'function' ? (value as () => unknown).bind(store) : value;
    },
  });
  return { ...actual, agentStore };
});

// Mounting awaits these lazy imports before the no-conversation load; their
// components are irrelevant here.
vi.mock('../shared/chat/chat-space-rail.js', () => ({}));
vi.mock('../shared/chat/chat-members.js', () => ({}));

/** The page's private surface these tests drive and read. */
interface ChatPage extends HTMLElement {
  pageData: unknown;
  v2Conversation: Record<string, unknown> | null;
  v2HumanMembers: ChatHumanMember[];
  v2AgentMembers: ChatAgentMember[];
  v2Members: Array<{ id: string }>;
  loadHubMembers(options?: { refresh?: boolean }): void;
  loadV2Members(projectId: string): Promise<void>;
  disconnectedCallback(): void;
  _hubUsersLoad: unknown;
  _hubAgentsLoad: unknown;
  _hubAgentsLive: boolean;
  _hubMembersGeneration: number;
  _loadHubAgents(generation: number): Promise<void>;
  _handleAgentsUpdated(): void;
  _sidebarOwner: string;
  handleRailLoaded(e: Event): void;
}

beforeAll(async () => {
  await import('./chat.js');
});

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function usersPage(ids: string[], nextCursor?: string): Response {
  const users = ids.map((id) => ({ id, displayName: id }));
  return new Response(JSON.stringify({ users, ...(nextCursor ? { nextCursor } : {}) }), {
    status: 200,
  });
}

type UsersResponder = (
  url: string,
  init?: { signal?: AbortSignal | null | undefined }
) => Response | Promise<Response>;

/** Serve `/api/v1/users` with `users`; every other page request gets `{}`. */
function serveUsers(users: UsersResponder): void {
  vi.mocked(apiFetch).mockImplementation(async (url, init) => {
    if (url.startsWith('/api/v1/users')) return users(url, init);
    return new Response('{}', { status: 200 });
  });
}

function usersRequests(): number {
  return vi.mocked(apiFetch).mock.calls.filter((c) => c[0].startsWith('/api/v1/users')).length;
}

/** Every agent-list request the page made itself, outside the store: none expected. */
function pageAgentRequests(): number {
  return vi.mocked(apiFetch).mock.calls.filter((c) => c[0].startsWith('/api/v1/agents')).length;
}

/** Hub-list walks the store started. */
function storeWalks(): number {
  return harness.server.walks('/api/v1/agents');
}

function ids(list: Array<{ id: string }>): string[] {
  return list.map((m) => m.id);
}

function createPage(): ChatPage {
  const page = document.createElement('scion-page-chat') as unknown as ChatPage;
  page.v2HumanMembers = [];
  page.v2AgentMembers = [];
  page.v2Members = [];
  return page;
}

/** A connected page with its feed open: its no-conversation load has started. */
async function mountPage(): Promise<ChatPage> {
  window.history.pushState({}, '', '/chat');
  const page = document.createElement('scion-page-chat') as unknown as ChatPage;
  page.pageData = { user: { id: 'user-me' } };
  document.body.appendChild(page);
  await settle();
  harness.stream().open();
  await settle();
  return page;
}

function unmount(page: ChatPage): void {
  if (page.isConnected) document.body.removeChild(page);
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.mocked(apiFetch).mockReset();
  resetUserDirectory();
  globalMap.agents.clear();
  harness = createHarness([agent('a1'), agent('a2')]);
});

afterEach(() => {
  harness.store.destroy();
  document.body.innerHTML = '';
  vi.useRealTimers();
});

describe('hub members: one load per view', () => {
  it('same-turn calls batch into one users walk and one store walk', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();

    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();
    page.loadHubMembers();
    await settle();

    expect(usersRequests()).toBe(1);
    expect(storeWalks()).toBe(1);
    expect(pageAgentRequests()).toBe(0);
    expect(ids(page.v2HumanMembers)).toEqual(['u1']);
    expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
  });

  it('issues no extra request when nothing new is triggered', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      // A mount's route parses join the one users walk, or arrive after it
      // finished and walk nothing.
      const usersBefore = usersRequests();
      expect(usersBefore).toBe(1);
      await vi.advanceTimersByTimeAsync(1_000);
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('join and refresh calls during an in-flight users walk join it: no trailing walk', async () => {
    const firstUsers = deferred<Response>();
    serveUsers(() => firstUsers.promise);
    const page = await mountPage();
    try {
      expect(usersRequests()).toBe(1);

      page.loadHubMembers();
      page.loadHubMembers({ refresh: true });
      page.loadHubMembers({ refresh: true });
      page.loadHubMembers({ refresh: true });
      await settle();

      firstUsers.resolve(usersPage(['u1']));
      await settle();

      expect(usersRequests()).toBe(1);
      expect(storeWalks()).toBe(1);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
    } finally {
      unmount(page);
    }
  });

  it('a real cold mount: the route parse, the no-conversation branch and a rail-loaded re-parse share one users walk and one store walk', async () => {
    const pendingUsers = deferred<Response>();
    serveUsers(() => pendingUsers.promise);
    const page = await mountPage();
    try {
      page.dispatchEvent(
        new CustomEvent('rail-loaded', {
          detail: { spaceIds: [], spaces: [] },
        })
      );
      await settle();

      pendingUsers.resolve(usersPage(['u1']));
      await settle();

      expect(usersRequests()).toBe(1);
      expect(storeWalks()).toBe(1);
      expect(pageAgentRequests()).toBe(0);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      expect(ids(page.v2Members)).toEqual(['u1', 'a1', 'a2']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: after a finished walk', () => {
  it('a join call (the re-parse after rail-loaded) walks neither list again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      expect(usersBefore).toBeGreaterThan(0);

      page.loadHubMembers();
      page.loadHubMembers();
      await settle();

      expect(usersRequests()).toBe(usersBefore);
      expect(storeWalks()).toBe(1);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('the fallback poll reuses the fresh recent-users page: no users request (the agents come from the store)', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('returning to the hub view after a conversation shows the shared page again with no users request', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it("a space's members load replaces the lists; the next hub view republishes the shared page with no users request", async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      await page.loadV2Members('p1');
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
    } finally {
      unmount(page);
    }
  });

  it('a failed users walk is not treated as finished', async () => {
    let call = 0;
    serveUsers(() => (++call === 1 ? new Response('', { status: 500 }) : usersPage(['u1'])));
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();
    page.loadHubMembers();
    await settle();
    expect(page.v2HumanMembers).toEqual([]);

    page.loadHubMembers();
    await settle();

    expect(usersRequests()).toBe(2);
    expect(ids(page.v2HumanMembers)).toEqual(['u1']);
  });
});

describe('hub members: hub presence fetch', () => {
  function membersRequests(): number {
    return vi
      .mocked(apiFetch)
      .mock.calls.filter((c) => /\/chat\/spaces\/[^/]+\/members$/.test(c[0])).length;
  }

  function railLoaded(page: ChatPage): void {
    page.handleRailLoaded(
      new CustomEvent('rail-loaded', {
        detail: {
          spaceIds: ['p1'],
          spaces: [{ projectId: 'p1', projectSlug: 'p1', projectName: 'P1' }],
        },
      })
    );
  }

  /** A click on the rail's empty area: the rail's own `reset-view` event. */
  function resetView(page: ChatPage): void {
    const rail = page.shadowRoot?.querySelector('scion-chat-space-rail');
    expect(rail).toBeTruthy();
    rail?.dispatchEvent(new CustomEvent('reset-view', { bubbles: true, composed: true }));
  }

  function presenceReads(): number {
    return vi.mocked(apiFetch).mock.calls.filter((c) => c[0] === '/api/v1/chat/spaces/p1/members')
      .length;
  }

  it('returning to the hub view from a space fetches presence again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(1);

      // On mobile: a space expanded with no conversation, then back to /chat.
      await page.loadV2Members('p2');
      page.loadHubMembers();
      await settle();

      expect(presenceReads()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it("an older presence response dropped after a mobile round trip does not release the newer request's claim", async () => {
    const held: Array<Deferred<Response>> = [];
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p1/members') {
        const d = deferred<Response>();
        held.push(d);
        return d.promise;
      }
      return Promise.resolve(
        new Response(JSON.stringify({ humans: [], agents: [] }), { status: 200 })
      );
    });
    const page = await mountPage();
    try {
      // Rail reloads here stand for later messages; the route re-parse a real
      // rail-loaded triggers would claim the sidebar again and is not under
      // test.
      (page as unknown as { parseV2Route: () => void }).parseV2Route = vi.fn();
      railLoaded(page);
      await settle();
      // Request A in flight; a space expands, then the hub view returns.
      expect(presenceReads()).toBe(1);
      await page.loadV2Members('p2');
      page.loadHubMembers();
      await settle();
      // Request B in flight for the same generation.
      expect(presenceReads()).toBe(2);

      // A lands for a view that moved on: dropped, but B keeps its claim.
      held[0]?.resolve(new Response(JSON.stringify({ humans: [] }), { status: 200 }));
      await settle();
      railLoaded(page);
      await settle();

      expect(presenceReads()).toBe(2);

      // B merges, and keeps the view's presence claimed.
      held[1]?.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      );
      await settle();
      expect(page.v2HumanMembers.find((h) => h.id === 'u1')?.presenceState).toBe('active');
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it("a poll resync overlapping the first presence request takes the claim: the first one's failure does not release it", async () => {
    const held: Array<Deferred<Response>> = [];
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p1/members') {
        const d = deferred<Response>();
        held.push(d);
        return d.promise;
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      // Request M, held.
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(1);

      // The poll's resync P, held too.
      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(presenceReads()).toBe(2);

      held[1]?.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      );
      await settle();
      expect(page.v2HumanMembers.find((h) => h.id === 'u1')?.presenceState).toBe('active');

      held[0]?.resolve(new Response('', { status: 500 }));
      await settle();
      railLoaded(page);
      await settle();

      expect(presenceReads()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it('a presence response dropped by a view change releases its claim: the next rail load asks again', async () => {
    const presence = deferred<Response>();
    let held = true;
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p1/members' && held) {
        held = false;
        return presence.promise;
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(1);

      // A join call claims the sidebar again (its view token moves on).
      page.loadHubMembers();
      presence.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      );
      await settle();

      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it('a rail reload while presence is in flight does not drop it: it merges with no further rail load or request', async () => {
    const presence = deferred<Response>();
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p1/members') return presence.promise;
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(1);

      // A burst of rail reloads (one per chat message), each re-parsing
      // /chat, while the presence request is in flight.
      railLoaded(page);
      railLoaded(page);
      await settle();
      presence.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      );
      await settle();

      expect(page.v2HumanMembers.find((h) => h.id === 'u1')?.presenceState).toBe('active');
      expect(presenceReads()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it("a rail reload while a space's members are in flight still drops them: they do not overwrite the hub list", async () => {
    const body = deferred<unknown>();
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p2/members') {
        // The body is held, not the response, so the abort cannot mask a
        // missed view-token check.
        return Promise.resolve({ ok: true, json: () => body.promise } as unknown as Response);
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();

      const spaceLoad = page.loadV2Members('p2');
      await settle();
      // The /chat re-parse claims the sidebar back from the space.
      railLoaded(page);
      await settle();
      body.resolve({ humans: [{ id: 's1', kind: 'user', displayName: 's1' }], agents: [] });
      await spaceLoad;
      await settle();

      expect(ids(page.v2HumanMembers)).not.toContain('s1');
      expect(ids(page.v2HumanMembers)).toContain('u1');
    } finally {
      unmount(page);
    }
  });

  it('a reset to the hub view already shown does not drop presence in flight: it merges with no extra request', async () => {
    const presence = deferred<Response>();
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p1/members') return presence.promise;
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      expect(presenceReads()).toBe(1);

      // A click on the rail's empty area while /chat is shown.
      resetView(page);
      await settle();
      presence.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      );
      await settle();

      expect(page.v2HumanMembers.find((h) => h.id === 'u1')?.presenceState).toBe('active');
      expect(presenceReads()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it("a reset to the hub view from a space still drops the space's members in flight", async () => {
    const body = deferred<unknown>();
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (url === '/api/v1/chat/spaces/p2/members') {
        // The body is held, not the response, so the abort cannot mask a
        // missed view-token check.
        return Promise.resolve({ ok: true, json: () => body.promise } as unknown as Response);
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();

      const spaceLoad = page.loadV2Members('p2');
      await settle();
      expect(page._sidebarOwner).toBe('space');
      resetView(page);
      await settle();
      body.resolve({ humans: [{ id: 's1', kind: 'user', displayName: 's1' }], agents: [] });
      await spaceLoad;
      await settle();

      expect(ids(page.v2HumanMembers)).not.toContain('s1');
      expect(ids(page.v2HumanMembers)).toContain('u1');
    } finally {
      unmount(page);
    }
  });

  it('is fetched once per hub view, not again on every rail reload', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      railLoaded(page);
      railLoaded(page);
      await settle();
      expect(membersRequests()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('waits for the hub users list when the rail loads first', async () => {
    const users = deferred<Response>();
    serveUsers(() => users.promise);
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      expect(membersRequests()).toBe(0);

      users.resolve(usersPage(['u1']));
      await settle();
      expect(membersRequests()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('a failed presence fetch is retried by the next rail load', async () => {
    let members = 0;
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['u1']));
      if (/\/members$/.test(url)) {
        return Promise.resolve(
          ++members === 1 ? new Response('', { status: 500 }) : new Response('{}', { status: 200 })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      railLoaded(page);
      await settle();
      expect(membersRequests()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it('the fallback poll resyncs it', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      const before = membersRequests();
      expect(before).toBe(1);

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();

      expect(membersRequests()).toBe(before + 1);
    } finally {
      unmount(page);
    }
  });

  it('a new hub view (after a conversation) fetches it again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      railLoaded(page);
      await settle();
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(membersRequests()).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it('is not fetched while a space holds the sidebar', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      await page.loadV2Members('p2');
      const before = membersRequests();
      // The route re-parse rail-loaded triggers is not under test here (on
      // mobile the route names the expanded space).
      (page as unknown as { parseV2Route: () => void }).parseV2Route = vi.fn();
      railLoaded(page);
      await settle();
      expect(membersRequests()).toBe(before);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: the fallback poll', () => {
  it('issues no users request while the recent-users page is fresh, one per TTL after, and no agent-list request', async () => {
    let call = 0;
    serveUsers(() => usersPage([`u${++call}`]));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      const ensure = vi.spyOn(harness.store, 'ensure');
      const usersBefore = usersRequests();
      expect(usersBefore).toBe(1);
      const agentRequestsBefore = harness.server.requests.filter(
        (p) => !p.includes('sort=')
      ).length;

      // The first poll: no users request, and no agent-list request (the
      // store's own probes, sort=updated, are not walks).
      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(harness.server.requests.filter((p) => !p.includes('sort=')).length).toBe(
        agentRequestsBefore
      );

      // Three more polls inside the TTL: still no users request.
      await vi.advanceTimersByTimeAsync(3 * 60_000);
      await settle();
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);

      // The poll at the TTL loads the one bounded page again.
      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(usersRequests()).toBe(usersBefore + 1);
      expect(ids(page.v2HumanMembers)).toEqual(['u2']);

      expect(pageAgentRequests()).toBe(0);
      // The poll does not ask the store while the shown list is ready (the
      // store's own periodic check is its business, not the page's).
      expect(ensure).not.toHaveBeenCalled();
    } finally {
      unmount(page);
    }
  });

  it('asks the store again after a failed revalidation, and the next walk refreshes the sidebar', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(storeWalks()).toBe(1);
      harness.server.status = 500;
      harness.store.invalidate('resync');
      await settle();
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      expect(storeWalks()).toBe(2);
      harness.server.status = 200;

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();

      expect(storeWalks()).toBe(3);
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('ready');
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('asks the store again while the agents are not shown yet: a failed first load is retried', async () => {
    serveUsers(() => usersPage(['u1']));
    harness.server.status = 500;
    const page = await mountPage();
    try {
      expect(storeWalks()).toBe(1);
      expect(page.v2AgentMembers).toEqual([]);

      harness.server.status = 200;
      page.loadHubMembers({ refresh: true });
      await settle();

      expect(storeWalks()).toBe(2);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: live updates from the store', () => {
  it('an SSE create, status change and delete reach the sidebar with no request', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const requestsBefore = harness.server.requests.length;

      harness.server.agents.push(agent('a3'));
      await harness.emitAgent('created', {
        agentId: 'a3',
        name: 'a3',
        slug: 'a3',
        projectId: 'p1',
      });
      expect(ids(page.v2AgentMembers)).toContain('a3');
      expect(ids(page.v2Members)).toContain('a3');

      await harness.emitAgent('status', {
        agentId: 'a1',
        projectId: 'p1',
        activity: 'working',
        detail: { message: '<b>Running</b> tests' },
      });
      expect(page.v2AgentMembers.find((m) => m.id === 'a1')?.activity).toBe('working');
      // The agent's free-text detail stays plain text (rendered as text by the sidebar).
      expect(page.v2AgentMembers.find((m) => m.id === 'a1')?.detailMessage).toBe(
        '<b>Running</b> tests'
      );

      await harness.emitAgent('deleted', { agentId: 'a2', projectId: 'p1' });
      expect(ids(page.v2AgentMembers)).not.toContain('a2');

      // No list request: probes (sort=updated) and the store's single read of
      // the created row are not walks.
      expect(
        harness.server.requests
          .slice(requestsBefore)
          .filter((p) => !p.includes('sort=') && !p.startsWith('/api/v1/agents/'))
      ).toEqual([]);
    } finally {
      unmount(page);
    }
  });

  it("a walked row's detail message reaches the sidebar, and a row that omits it clears it", async () => {
    harness.server.agents = [agent('a1', { message: 'Cloning repository' }), agent('a2')];
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const member = (): ChatAgentMember | undefined =>
        page.v2AgentMembers.find((m) => m.id === 'a1');
      expect(member()?.detailMessage).toBe('Cloning repository');

      await harness.emitAgent('status', {
        agentId: 'a1',
        projectId: 'p1',
        detail: { message: 'Installing tools' },
      });
      expect(member()?.detailMessage).toBe('Installing tools');

      // The agent cleared its message while the feed missed it: the next
      // walk's compact row omits the empty value.
      harness.server.agents = [agent('a1'), agent('a2')];
      harness.store.invalidate('manual');
      await settle();

      expect(storeWalks()).toBe(2);
      expect(member()?.detailMessage).toBe('');
    } finally {
      unmount(page);
    }
  });

  it('after a failed revalidation the kept rows stay live: an SSE change reaches the sidebar', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      harness.server.status = 500;
      harness.store.invalidate('resync');
      await settle();
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      harness.server.status = 200;

      await harness.emitAgent('status', {
        agentId: 'a1',
        projectId: 'p1',
        activity: 'working',
      });
      expect(page.v2AgentMembers.find((m) => m.id === 'a1')?.activity).toBe('working');

      // agents-updated in the hub view reads the same kept rows (after
      // something else rewrote the sidebar's agents).
      page.v2AgentMembers = [];
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('an unchanged hub list leaves the sidebar members as they are (no rebuild)', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const shown = page.v2AgentMembers;
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      page.loadHubMembers();
      await settle();
      expect(page.v2AgentMembers).toBe(shown);
    } finally {
      unmount(page);
    }
  });

  it('a failed first load shows nothing from the error snapshot', async () => {
    serveUsers(() => usersPage(['u1']));
    harness.server.status = 500;
    const page = await mountPage();
    try {
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      page.v2AgentMembers = [{ id: 'sentinel', kind: 'agent', displayName: 'sentinel' }];
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      expect(ids(page.v2AgentMembers)).toEqual(['sentinel']);
      // Not shown, so the poll keeps asking the store.
      expect(page._hubAgentsLive).toBe(false);
    } finally {
      unmount(page);
    }
  });

  it('a DM opened from the hub view keeps the hub list live, read from the store', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      page.v2Conversation = {
        conversationKey: 'dm:agent:a1:user:user-me',
        projectId: '',
        isDM: true,
      };
      await settle();

      await harness.emitAgent('status', {
        agentId: 'a2',
        projectId: 'p1',
        activity: 'thinking',
      });
      expect(page.v2AgentMembers.find((m) => m.id === 'a2')?.activity).toBe('thinking');

      // The global map's agents-updated, in this view, also reads the store's
      // list: a row only the global map holds (the chat scope's) is not shown.
      globalMap.agents.set('scope-only', agent('scope-only', { projectId: 'p7' }));
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      expect(page.v2AgentMembers.find((m) => m.id === 'a2')?.activity).toBe('thinking');
      // The DM thread finds its peer in the hub list: no single-agent read.
      expect(
        vi.mocked(apiFetch).mock.calls.filter((c) => c[0].startsWith('/api/v1/agents/a1'))
      ).toEqual([]);
      expect(harness.server.agentFetches('a1')).toBe(0);
    } finally {
      unmount(page);
    }
  });

  it('a space view claiming the sidebar stops the hub list updating it, whatever the conversation', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      page.v2Conversation = {
        conversationKey: 'dm:agent:a1:user:user-me',
        projectId: '',
        isDM: true,
      };
      vi.mocked(apiFetch).mockImplementation(() => new Promise<Response>(() => {}));
      void page.loadV2Members('p1');
      page.v2AgentMembers = [{ id: 'proj-agent', kind: 'agent', displayName: 'Proj Agent' }];
      await settle();

      harness.server.agents.push(agent('a3'));
      await harness.emitAgent('created', {
        agentId: 'a3',
        name: 'a3',
        slug: 'a3',
        projectId: 'p1',
      });
      expect(ids(page.v2AgentMembers)).toEqual(['proj-agent']);
    } finally {
      unmount(page);
    }
  });

  it('a project view stops hearing the hub list', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      page.v2Conversation = {
        conversationKey: 'p1',
        projectId: 'p1',
        isDM: false,
      };
      page.v2AgentMembers = [{ id: 'proj-agent', kind: 'agent', displayName: 'Proj Agent' }];
      vi.mocked(apiFetch).mockImplementation(async () => new Promise<Response>(() => {}));
      void page.loadV2Members('p1');
      await settle();

      await harness.emitAgent('created', {
        agentId: 'a3',
        name: 'a3',
        slug: 'a3',
        projectId: 'p9',
      });
      expect(ids(page.v2AgentMembers)).toEqual(['proj-agent']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: a space claiming the sidebar with no conversation', () => {
  it('a mobile expanded space keeps its members on hub SSE changes and agents-updated', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      // On mobile, selecting a space expands it in the rail and loads its
      // members without opening a conversation.
      vi.mocked(apiFetch).mockImplementation(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
              agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
            }),
            { status: 200 }
          )
        )
      );
      await page.loadV2Members('p1');
      await settle();
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);

      await harness.emitAgent('status', { agentId: 'a2', projectId: 'p9', activity: 'thinking' });
      // The chat scope's map holds another rail project's agent: it is not
      // this space's.
      globalMap.agents.set('other', agent('other', { projectId: 'p2' }));
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      await settle();

      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);
      expect(ids(page.v2Members)).toEqual(['h1', 'sp1']);
    } finally {
      unmount(page);
    }
  });

  it('the 60s poll reloads the expanded space instead of claiming the sidebar for the hub', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const spaceReads = (): number =>
        vi.mocked(apiFetch).mock.calls.filter((c) => c[0] === '/api/v1/chat/spaces/p1/members')
          .length;
      vi.mocked(apiFetch).mockImplementation((url) =>
        Promise.resolve(
          url === '/api/v1/chat/spaces/p1/members'
            ? new Response(
                JSON.stringify({
                  humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
                  agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
                }),
                { status: 200 }
              )
            : new Response('{}', { status: 200 })
        )
      );
      await page.loadV2Members('p1');
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);
      const usersBefore = usersRequests();

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();

      expect(spaceReads()).toBe(2);
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);
      expect(ids(page.v2HumanMembers)).toEqual(['h1']);
    } finally {
      unmount(page);
    }
  });

  it('the poll reloads the space expanded last, not the first one', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const reads = (projectId: string): number =>
        vi
          .mocked(apiFetch)
          .mock.calls.filter((c) => c[0] === `/api/v1/chat/spaces/${projectId}/members`).length;
      vi.mocked(apiFetch).mockImplementation(() =>
        Promise.resolve(new Response(JSON.stringify({ humans: [], agents: [] }), { status: 200 }))
      );
      await page.loadV2Members('pa');
      await page.loadV2Members('pb');

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();

      expect(reads('pa')).toBe(1);
      expect(reads('pb')).toBe(2);
    } finally {
      unmount(page);
    }
  });

  it('a space reached from the hub view re-seeds its rows when the scope changes', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      vi.mocked(apiFetch).mockImplementation(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [],
              agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1' }],
            }),
            { status: 200 }
          )
        )
      );
      page.v2Conversation = { conversationKey: 'p1', projectId: 'p1', isDM: false };
      await page.loadV2Members('p1');
      // setScope wipes the map once the rail reports its spaces.
      globalMap.agents.clear();
      globalMap.stateManager.dispatchEvent(new Event('scope-changed'));
      expect(Array.from(globalMap.agents.keys())).toEqual(['sp1']);
    } finally {
      unmount(page);
    }
  });

  it('a hub store walk landing after a space claimed the sidebar leaves the space agents', async () => {
    serveUsers(() => usersPage(['u1']));
    const release = harness.server.pause();
    const page = await mountPage();
    try {
      expect(page._hubAgentsLoad).not.toBeNull();
      vi.mocked(apiFetch).mockImplementation((url) =>
        Promise.resolve(
          url === '/api/v1/chat/spaces/p1/members'
            ? new Response(
                JSON.stringify({
                  humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
                  agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
                }),
                { status: 200 }
              )
            : new Response('{}', { status: 200 })
        )
      );
      await page.loadV2Members('p1');
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);

      release();
      await settle();

      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('ready');
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);
      expect(ids(page.v2Members)).toEqual(['h1', 'sp1']);
    } finally {
      unmount(page);
    }
  });

  it('a hub users walk landing after a space claimed the sidebar leaves the space humans', async () => {
    const pendingUsers = deferred<Response>();
    serveUsers(() => pendingUsers.promise);
    const page = await mountPage();
    try {
      vi.mocked(apiFetch).mockImplementation((url) =>
        Promise.resolve(
          url === '/api/v1/chat/spaces/p1/members'
            ? new Response(
                JSON.stringify({
                  humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
                  agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
                }),
                { status: 200 }
              )
            : new Response('{}', { status: 200 })
        )
      );
      await page.loadV2Members('p1');
      expect(ids(page.v2HumanMembers)).toEqual(['h1']);

      pendingUsers.resolve(usersPage(['hub-user']));
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['h1']);
      expect(ids(page.v2Members)).toEqual(['h1', 'sp1']);
    } finally {
      unmount(page);
    }
  });

  it('the hub view claims the sidebar on its load, not on a publish: after a failed store read the poll loads the users', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      harness.server.status = 500;
      harness.store.invalidate('resync');
      await settle();
      const spaceReads = (): number =>
        vi.mocked(apiFetch).mock.calls.filter((c) => c[0] === '/api/v1/chat/spaces/p1/members')
          .length;
      vi.mocked(apiFetch).mockImplementation((url) =>
        Promise.resolve(
          url === '/api/v1/chat/spaces/p1/members'
            ? new Response(
                JSON.stringify({
                  humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
                  agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
                }),
                { status: 200 }
              )
            : new Response('{}', { status: 200 })
        )
      );
      await page.loadV2Members('p1');
      page.loadHubMembers();
      await settle();
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      const usersBefore = usersRequests();

      // Polls up to the recent-users TTL: the one at the TTL reloads them.
      await vi.advanceTimersByTimeAsync(5 * 60_000);
      await settle();

      expect(spaceReads()).toBe(1);
      expect(usersRequests()).toBeGreaterThan(usersBefore);
    } finally {
      unmount(page);
    }
  });

  it('the hub view claiming it back shows the hub list again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      vi.mocked(apiFetch).mockImplementation(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }],
              agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1', projectId: 'p1' }],
            }),
            { status: 200 }
          )
        )
      );
      await page.loadV2Members('p1');
      expect(ids(page.v2AgentMembers)).toEqual(['sp1']);

      serveUsers(() => usersPage(['u1']));
      page.loadHubMembers();
      await settle();
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: guards', () => {
  it('a scope change after leaving the hub for a project, before its members land, seeds no hub row', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      const members = deferred<Response>();
      vi.mocked(apiFetch).mockImplementation(() => members.promise);
      page.v2Conversation = { conversationKey: 'p1', projectId: 'p1', isDM: false };
      void page.loadV2Members('p1');
      await settle();

      globalMap.stateManager.dispatchEvent(new Event('scope-changed'));
      expect(globalMap.agents.size).toBe(0);

      members.resolve(
        new Response(
          JSON.stringify({
            humans: [],
            agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1' }],
          }),
          { status: 200 }
        )
      );
      await settle();
      expect(Array.from(globalMap.agents.keys())).toEqual(['sp1']);
    } finally {
      unmount(page);
    }
  });

  it('a failed first store read is retried by the next join call, with no users re-walk', async () => {
    serveUsers(() => usersPage(['u1']));
    harness.server.status = 500;
    const page = await mountPage();
    try {
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      const usersBefore = usersRequests();
      const walksBefore = storeWalks();
      harness.server.status = 200;

      page.loadHubMembers();
      await settle();

      expect(usersRequests()).toBe(usersBefore);
      expect(storeWalks()).toBe(walksBefore + 1);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a users load dropped for a space claim does not mark the users loaded: the returning hub view publishes the shared page', async () => {
    // The mount's users load is held; it lands after the space's claim.
    const held = deferred<Response>();
    serveUsers(() => held.promise);
    const page = await mountPage();
    try {
      vi.mocked(apiFetch).mockImplementation((url) => {
        if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['other-user']));
        return Promise.resolve(
          new Response(
            JSON.stringify({ humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }], agents: [] }),
            { status: 200 }
          )
        );
      });
      await page.loadV2Members('p1');
      held.resolve(usersPage(['hub-user']));
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['h1']);
      const usersBefore = usersRequests();

      page.loadHubMembers();
      await settle();

      // The dropped load's page is still fresh and shared: no new request.
      expect(usersRequests()).toBe(usersBefore);
      expect(ids(page.v2HumanMembers)).toEqual(['hub-user']);
    } finally {
      unmount(page);
    }
  });

  it('agents-updated in a DM without a live hub view is not scoped to a space visited earlier: it adopts agents from any space', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    vi.mocked(apiFetch).mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify({ humans: [], agents: [] }), { status: 200 }))
    );
    await page.loadV2Members('p1');
    serveUsers(() => usersPage(['u1']));
    page.loadHubMembers();
    await settle();
    // The real precondition is a hub list that never published (a failed
    // first store read); a disconnect clears the live flag the same way.
    document.body.removeChild(page);

    page.v2Conversation = { conversationKey: 'dm:user:u9', projectId: '', isDM: true };
    page.v2AgentMembers = [];
    globalMap.agents.set('elsewhere', agent('elsewhere', { projectId: 'p2' }));
    page._handleAgentsUpdated();

    expect(ids(page.v2AgentMembers)).toEqual(['elsewhere']);
  });

  it("a disconnect aborts a space's members request in flight", async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    let signal: AbortSignal | null | undefined;
    vi.mocked(apiFetch).mockImplementation((_url, init) => {
      signal = init?.signal;
      return new Promise<Response>(() => {});
    });
    void page.loadV2Members('p1');
    expect(signal?.aborted).toBe(false);

    document.body.removeChild(page);

    expect(signal?.aborted).toBe(true);
  });

  it('agents-updated during a first walk does not show its partial rows', async () => {
    harness = createHarness(
      Array.from({ length: 5 }, (_, i) => agent(`a${i}`)),
      { pageSize: 2 }
    );
    const realFetch = harness.server.fetch.getMockImplementation()!;
    const page2 = deferred<void>();
    harness.server.fetch.mockImplementation(async (path, options) => {
      if (path.includes('cursor=')) await page2.promise;
      return realFetch(path, options);
    });
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('loading');
      expect(harness.store.peek({ scope: 'hub' })?.agents.length).toBe(2);
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      expect(page.v2AgentMembers).toEqual([]);

      page2.resolve();
      await settle();
      expect(page.v2AgentMembers.length).toBe(5);
    } finally {
      unmount(page);
    }
  });

  it('the poll starts no store walk while the feed is down and the shown list is ready', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(storeWalks()).toBe(1);
      harness.stream().drop();
      await settle();

      // The poll, then past the store's wait for the feed to reconnect.
      await vi.advanceTimersByTimeAsync(60_000);
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();

      expect(storeWalks()).toBe(1);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a disconnect ends the hub list being live: a DM afterwards takes the global-map path', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    document.body.removeChild(page);

    page.v2Conversation = { conversationKey: 'dm:user:u9', projectId: '', isDM: true };
    page.v2AgentMembers = [{ id: 'kept', kind: 'agent', displayName: 'kept' }];
    page._handleAgentsUpdated();

    expect(ids(page.v2AgentMembers)).toEqual(['kept']);
  });
});

describe('hub members: compact rows never reach the global agent map', () => {
  it('no hub-list row is seeded on mount, live updates, the poll, a scope change or agents-updated', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      harness.server.agents.push(agent('a3'));
      await harness.emitAgent('created', {
        agentId: 'a3',
        name: 'a3',
        slug: 'a3',
        projectId: 'p1',
      });
      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      globalMap.stateManager.dispatchEvent(new Event('scope-changed'));
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      await settle();

      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2', 'a3']);
      expect(globalMap.agents.size).toBe(0);
    } finally {
      unmount(page);
    }
  });

  it('a later space view seeds its own rows, and a scope change back on the hub view does not re-seed hub rows', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      vi.mocked(apiFetch).mockImplementation(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [],
              agents: [{ id: 'sp1', kind: 'agent', displayName: 'sp1' }],
            }),
            { status: 200 }
          )
        )
      );
      page.v2Conversation = {
        conversationKey: 'p1',
        projectId: 'p1',
        isDM: false,
      };
      await page.loadV2Members('p1');
      expect(Array.from(globalMap.agents.keys())).toEqual(['sp1']);

      globalMap.agents.clear();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      globalMap.stateManager.dispatchEvent(new Event('scope-changed'));
      expect(globalMap.agents.size).toBe(0);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: view-change race', () => {
  it('a load in flight when the user opens a project does not overwrite that project members', async () => {
    const pendingUsers = deferred<Response>();
    serveUsers(() => pendingUsers.promise);
    const release = harness.server.pause();
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      page.v2HumanMembers = [{ id: 'proj-user', kind: 'user', displayName: 'Proj User' }];
      page.v2AgentMembers = [{ id: 'proj-agent', kind: 'agent', displayName: 'Proj Agent' }];
      page.v2Members = [{ id: 'proj-user' }];
      await settle();

      release();
      pendingUsers.resolve(usersPage(['hub-user']));
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['proj-user']);
      expect(ids(page.v2AgentMembers)).toEqual(['proj-agent']);
      expect(ids(page.v2Members)).toEqual(['proj-user']);
    } finally {
      unmount(page);
    }
  });

  it('a DM opened mid-load drops the detached store result, and the hub list does not reach that DM', async () => {
    serveUsers(() => usersPage(['u1']));
    const release = harness.server.pause();
    const page = await mountPage();
    try {
      expect(page._hubAgentsLoad).not.toBeNull();
      page.v2Conversation = {
        conversationKey: 'dm:user:u9',
        projectId: '',
        isDM: true,
      };
      await settle();
      expect(page._hubAgentsLoad).toBeNull();

      release();
      await settle();

      // The walk itself went on: the page retains the hub list for its palette.
      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('ready');
      expect(page.v2AgentMembers).toEqual([]);
      await harness.emitAgent('created', {
        agentId: 'a3',
        name: 'a3',
        slug: 'a3',
        projectId: 'p1',
      });
      expect(page.v2AgentMembers).toEqual([]);
    } finally {
      unmount(page);
    }
  });

  it('opening then closing a conversation mid-load: the returning hub view joins the one request and publishes it', async () => {
    const held = deferred<Response>();
    serveUsers(() => held.promise);
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      held.resolve(usersPage(['u1', 'u2']));
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      expect(usersRequests()).toBe(1);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('opening and closing a conversation within one Lit update batch publishes the one page for the hub view', async () => {
    const held = deferred<Response>();
    serveUsers(() => held.promise);
    const page = await mountPage();
    try {
      held.resolve(usersPage(['u1', 'u2']));
      for (let i = 0; i < 3; i++) await Promise.resolve();
      queueMicrotask(() => {
        page.v2Conversation = null;
        page.loadHubMembers();
      });
      page.v2Conversation = {
        projectId: 'p1',
        conversationKey: 'p1',
        isDM: false,
      };
      await settle();

      expect(usersRequests()).toBe(1);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a newer hub view joins the users request in flight rather than issuing another', async () => {
    let userCall = 0;
    const held = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? held.promise : usersPage(['second-user'])));
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(userCall).toBe(1);

      held.resolve(usersPage(['first-user']));
      await settle();

      expect(userCall).toBe(1);
      expect(ids(page.v2HumanMembers)).toEqual(['first-user']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: users pagination and errors', () => {
  it('loads one bounded users page (most recently seen first), while the store walks every agents page', async () => {
    harness = createHarness(Array.from({ length: 450 }, (_, i) => agent(`a${i}`)));
    const userIds = Array.from({ length: 100 }, (_, i) => `u${i}`);
    serveUsers(() => usersPage(userIds, 'u-cursor'));
    const page = await mountPage();
    try {
      // One users request, never following the cursor.
      const userUrls = vi
        .mocked(apiFetch)
        .mock.calls.map((c) => c[0])
        .filter((u) => u.startsWith('/api/v1/users'));
      expect(userUrls).toEqual(['/api/v1/users?sort=lastSeen&dir=desc&limit=100']);
      expect(storeWalks()).toBe(1);
      expect(harness.server.requests.filter((p) => p.includes('cursor=')).length).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(userIds);
      expect((page as unknown as { v2HumansTruncated: boolean }).v2HumansTruncated).toBe(true);
      expect(page.v2AgentMembers.length).toBe(450);
    } finally {
      unmount(page);
    }
  });

  it('a hub whose users fit on the page is not truncated', async () => {
    serveUsers(() => usersPage(['u1', 'u2']));
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
      expect((page as unknown as { v2HumansTruncated: boolean }).v2HumansTruncated).toBe(false);
    } finally {
      unmount(page);
    }
  });

  it('de-dupes a user returned twice in the page', async () => {
    serveUsers(() => usersPage(['u1', 'u2', 'u2']));
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
    } finally {
      unmount(page);
    }
  });

  it('a failed users reload keeps the previous users; a failed agents revalidation keeps the agents', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);

      resetUserDirectory();
      serveUsers(() => new Response('', { status: 500 }));
      page.loadHubMembers({ refresh: true });
      harness.server.status = 500;
      harness.store.invalidate('manual');
      await settle();

      expect(harness.store.peek({ scope: 'hub' })?.status).toBe('error');
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: hung page', () => {
  it('a stalled users request times out, clears the in-flight marker, keeps the users, and the next poll reloads', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(page._hubUsersLoad).toBeNull();
      const usersBefore = usersRequests();

      // Expire the shared page so the next refresh asks again, and stall it.
      resetUserDirectory();
      serveUsers(
        (_url, init) =>
          new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () => {
              reject(new DOMException('The operation was aborted.', 'AbortError'));
            });
          })
      );
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(page._hubUsersLoad).not.toBeNull();
      expect(usersRequests()).toBe(usersBefore + 1);

      // A view re-parse while the request is stalled joins it.
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore + 1);

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(page._hubUsersLoad).toBeNull();
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);

      serveUsers(() => usersPage(['u3']));
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(usersRequests()).toBe(usersBefore + 2);
      expect(ids(page.v2HumanMembers)).toEqual(['u3']);
    } finally {
      unmount(page);
    }
  });

  it('a stalled agents page times out in the store, clears the sidebar caller, and the next trigger walks again', async () => {
    serveUsers(() => usersPage(['u1']));
    harness = createHarness(Array.from({ length: 250 }, (_, i) => agent(`a${i}`)));
    const realFetch = harness.server.fetch.getMockImplementation()!;
    harness.server.fetch.mockImplementation((path, options) => {
      if (!path.includes('cursor=')) return realFetch(path, options);
      return new Promise<Response>((_, reject) => {
        options.signal?.addEventListener('abort', () => {
          reject(new DOMException('The operation was aborted.', 'AbortError'));
        });
      });
    });
    const page = await mountPage();
    try {
      expect(storeWalks()).toBe(1);
      expect(page._hubAgentsLoad).not.toBeNull();

      // A view re-parse while the page is stalled joins the walk.
      page.loadHubMembers();
      await settle();
      expect(storeWalks()).toBe(1);

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(page._hubAgentsLoad).toBeNull();
      expect(page.v2AgentMembers).toEqual([]);

      harness.server.fetch.mockImplementation(realFetch);
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(storeWalks()).toBe(2);
      expect(page.v2AgentMembers.length).toBe(250);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: reconnect and disconnect', () => {
  it('a DM opened after the store answered, before updated() bumps the generation, gets no hub list', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      page.v2AgentMembers = [];
      const generation = page._hubMembersGeneration;
      const answer = harness.store.peek({ scope: 'hub' });
      vi.spyOn(harness.store, 'ensure').mockImplementation(() => {
        // Runs before the load resumes; updated()'s bump runs after it.
        queueMicrotask(() => {
          page.v2Conversation = { conversationKey: 'dm:user:u9', projectId: '', isDM: true };
        });
        return Promise.resolve(answer!);
      });
      await page._loadHubAgents(generation);
      // The load resumed in the lag window: same generation, the hub's claim.
      expect(page._sidebarOwner).toBe('hub');
      await settle();
      expect(page.v2AgentMembers).toEqual([]);
    } finally {
      unmount(page);
    }
  });

  it('a disconnect landing after the store answered, before the load resumes, is caught by the generation check', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    page.v2AgentMembers = [];
    const answer = harness.store.peek({ scope: 'hub' });
    vi.spyOn(harness.store, 'ensure').mockImplementation(() => {
      // The store has already answered, so the detach's abort cannot reject it.
      queueMicrotask(() => document.body.removeChild(page));
      return Promise.resolve(answer!);
    });
    await page._loadHubAgents(page._hubMembersGeneration);
    await settle();
    expect(page.isConnected).toBe(false);
    expect(page.v2AgentMembers).toEqual([]);
  });

  it('a disconnect-then-reconnect while the users request is in flight joins it and publishes it for the new view', async () => {
    let userCall = 0;
    const held = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? held.promise : usersPage(['u-second'])));
    const page = await mountPage();
    try {
      document.body.removeChild(page);
      document.body.appendChild(page);
      await settle();

      held.resolve(usersPage(['u1']));
      await settle();

      expect(userCall).toBe(1);
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      // The store answers the reconnected page from memory.
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('a superseded load settling after reconnect does not clear the in-flight markers of the fresh loads', async () => {
    const held = deferred<Response>();
    serveUsers(() => held.promise);
    const release = harness.server.pause();
    const page = await mountPage();
    try {
      const staleUsersLoad = page._hubUsersLoad;
      expect(staleUsersLoad).not.toBeNull();
      document.body.removeChild(page);
      document.body.appendChild(page);
      await settle();
      const freshUsersLoad = page._hubUsersLoad;
      expect(freshUsersLoad).not.toBeNull();
      expect(freshUsersLoad).not.toBe(staleUsersLoad);
      const freshAgents = page._hubAgentsLoad;
      expect(freshAgents).not.toBeNull();

      page.loadHubMembers();
      await settle();
      expect(page._hubUsersLoad).toBe(freshUsersLoad);
      expect(page._hubAgentsLoad).toBe(freshAgents);
      expect(usersRequests()).toBe(1);
      held.resolve(usersPage(['u1']));
      await settle();
      expect(page._hubUsersLoad).toBeNull();
      release();
    } finally {
      unmount(page);
    }
  });

  it('a load requested just before a disconnect in the same turn issues no requests', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    expect(ids(page.v2HumanMembers)).toEqual(['u1']);
    vi.mocked(apiFetch).mockClear();
    const walksBefore = storeWalks();

    page.loadHubMembers();
    document.body.removeChild(page);
    // With the feed down, a store read would walk rather than answer from
    // memory: any read the load made would show as a walk.
    harness.stream().drop();
    await settle();
    // Past the store's wait for the feed to reconnect.
    await vi.advanceTimersByTimeAsync(30_000);
    await settle();

    expect(apiFetch).not.toHaveBeenCalled();
    expect(storeWalks()).toBe(walksBefore);
    expect(ids(page.v2HumanMembers)).toEqual(['u1']);
  });

  it('a superseded store read settling after a newer one started does not clear the newer one', async () => {
    serveUsers(() => usersPage(['u1']));
    const release = harness.server.pause();
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();

    page._sidebarOwner = 'hub';
    void page._loadHubAgents(0);
    page._hubMembersGeneration = 1;
    void page._loadHubAgents(1);
    const fresh = page._hubAgentsLoad;
    // The superseded read was detached; its rejection settles now.
    await settle();

    expect(page._hubAgentsLoad).toBe(fresh);
    release();
    await settle();
    expect(page._hubAgentsLoad).toBeNull();
    expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
  });

  it('a store result for a generation that moved on is dropped even without a detach', async () => {
    serveUsers(() => usersPage(['u1']));
    const release = harness.server.pause();
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();

    page._sidebarOwner = 'hub';
    void page._loadHubAgents(0);
    page._hubMembersGeneration = 1;
    release();
    await settle();

    expect(page.v2AgentMembers).toEqual([]);
  });

  it('a reconnect into a DM whose agents-updated merges global rows, then /chat, shows the hub list again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    document.body.removeChild(page);

    // Back on a DM route (not opened from a live hub view): agents-updated
    // takes the global-map path and merges a row only that map holds.
    page.v2Conversation = {
      conversationKey: 'dm:user:u9',
      projectId: '',
      isDM: true,
    };
    globalMap.agents.set('scope-only', agent('scope-only', { projectId: 'p7' }));
    page._handleAgentsUpdated();
    expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2', 'scope-only']);

    page.v2Conversation = null;
    document.body.appendChild(page);
    await settle();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a project opened and left before its members load: the hub list replaces the merged rows', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      page.v2Conversation = {
        conversationKey: 'p1',
        projectId: 'p1',
        isDM: false,
      };
      await settle();
      globalMap.agents.set('scope-only', agent('scope-only', { projectId: 'p1' }));
      globalMap.stateManager.dispatchEvent(new Event('agents-updated'));
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2', 'scope-only']);

      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();

      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a disconnect mid-walk releases the hub list: the store aborts a walk no one else wants', async () => {
    serveUsers(() => usersPage(['u1']));
    harness = createHarness(Array.from({ length: 450 }, (_, i) => agent(`a${i}`)));
    const realFetch = harness.server.fetch.getMockImplementation()!;
    const page2 = deferred<void>();
    const asked: string[] = [];
    harness.server.fetch.mockImplementation(async (path, options) => {
      asked.push(path);
      if (path.includes('cursor=')) await page2.promise;
      return realFetch(path, options);
    });
    const page = await mountPage();
    // Page 1 served, page 2 held.
    expect(asked.length).toBe(2);

    document.body.removeChild(page);
    page2.resolve();
    await settle();

    // No third page: the walk stopped with its last waiter and retainer gone.
    expect(asked.length).toBe(2);
    expect(page.v2AgentMembers).toEqual([]);
  });
});

describe('hub members: people beyond the recent page', () => {
  type SearchPage = ChatPage & {
    v2HumansTruncated: boolean;
    v2HumanSearchPending: boolean;
    _handleMemberSearch(e: CustomEvent<{ query: string }>): void;
    parseV2Route(): void;
  };

  function search(page: ChatPage, query: string): void {
    (page as SearchPage)._handleMemberSearch(
      new CustomEvent('member-search', { detail: { query } })
    );
  }

  function searchRequests(): string[] {
    return vi
      .mocked(apiFetch)
      .mock.calls.map((c) => c[0])
      .filter((u) => u.startsWith('/api/v1/users?search='));
  }

  it('a truncated list searches the hub (debounced) and adds what it finds', async () => {
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=')
        ? usersPage(['far-user'])
        : usersPage(['u1'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      expect((page as SearchPage).v2HumansTruncated).toBe(true);
      search(page, 'f');
      search(page, 'fa');
      search(page, 'far');
      await vi.advanceTimersByTimeAsync(250);
      await settle();

      expect(searchRequests()).toEqual(['/api/v1/users?search=far&limit=20']);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);

      // A found user stays listed when the page is published again.
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);
    } finally {
      unmount(page);
    }
  });

  for (const outcome of ['finds nobody', 'fails'] as const) {
    it(`a query that ${outcome} is searched once, not again on every poll`, async () => {
      serveUsers((url) =>
        url.startsWith('/api/v1/users?search=')
          ? outcome === 'fails'
            ? new Response('', { status: 500 })
            : usersPage([])
          : usersPage(['u1'], 'u-cursor')
      );
      const page = await mountPage();
      try {
        search(page, 'zzz');
        await vi.advanceTimersByTimeAsync(250);
        await settle();
        expect(searchRequests().length).toBe(1);

        // Five polls, including the one that reloads the recent page.
        await vi.advanceTimersByTimeAsync(5 * 60_000);
        await settle();
        expect(searchRequests().length).toBe(1);
        expect((page as SearchPage).v2HumanSearchPending).toBe(false);
      } finally {
        unmount(page);
      }
    });
  }

  it('a list that is not truncated never searches the hub', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      search(page, 'far');
      await vi.advanceTimersByTimeAsync(1_000);
      await settle();
      expect(searchRequests()).toEqual([]);
    } finally {
      unmount(page);
    }
  });

  it('a search result landing after a conversation opened is dropped; the returning hub view searches again', async () => {
    const held = deferred<Response>();
    let searchCall = 0;
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=')
        ? ++searchCall === 1
          ? held.promise
          : usersPage(['far-user'])
        : usersPage(['u1'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      search(page, 'far');
      await vi.advanceTimersByTimeAsync(250);
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      held.resolve(usersPage(['stale-far-user']));
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect((page as SearchPage).v2HumanSearchPending).toBe(false);

      // Back on the hub view, the query still in the sidebar searches again.
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(searchRequests().length).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);
    } finally {
      unmount(page);
    }
  });

  it('a search that settled before a conversation opened runs again for the returning hub view', async () => {
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=')
        ? usersPage(['far-user'])
        : usersPage(['u1'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      search(page, 'far');
      await vi.advanceTimersByTimeAsync(250);
      await settle();
      expect(searchRequests().length).toBe(1);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);

      // A DM round trip retires the view (and its found users); the query
      // stays in the sidebar, so the returning view must search again.
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();

      expect(searchRequests().length).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);
    } finally {
      unmount(page);
    }
  });

  it('a superseded search landing after a newer one cannot publish', async () => {
    const slow = deferred<Response>();
    serveUsers((url) => {
      if (url.startsWith('/api/v1/users?search=first')) return slow.promise;
      if (url.startsWith('/api/v1/users?search=second')) return usersPage(['second-user']);
      return usersPage(['u1'], 'u-cursor');
    });
    const page = await mountPage();
    try {
      search(page, 'first');
      await vi.advanceTimersByTimeAsync(250);
      search(page, 'second');
      await vi.advanceTimersByTimeAsync(250);
      await settle();
      slow.resolve(usersPage(['first-user']));
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'second-user']);
    } finally {
      unmount(page);
    }
  });

  it('is pending from the keystroke until the search settles; clearing the query cancels it', async () => {
    const held = deferred<Response>();
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=') ? held.promise : usersPage(['u1'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      search(page, 'far');
      expect((page as SearchPage).v2HumanSearchPending).toBe(true);
      await vi.advanceTimersByTimeAsync(250);
      expect((page as SearchPage).v2HumanSearchPending).toBe(true);
      held.resolve(usersPage([]));
      await settle();
      expect((page as SearchPage).v2HumanSearchPending).toBe(false);

      search(page, 'again');
      expect((page as SearchPage).v2HumanSearchPending).toBe(true);
      search(page, '');
      expect((page as SearchPage).v2HumanSearchPending).toBe(false);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(searchRequests().length).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('a space claiming the sidebar clears the found users and the query', async () => {
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=')
        ? usersPage(['far-user'])
        : url.startsWith('/api/v1/users')
          ? usersPage(['u1'], 'u-cursor')
          : new Response('{}', { status: 200 })
    );
    const page = await mountPage();
    try {
      search(page, 'far');
      await vi.advanceTimersByTimeAsync(250);
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-user']);

      await page.loadV2Members('p1');
      page.loadHubMembers();
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(searchRequests().length).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('a DM with a user outside the recent page is named by one cached by-id lookup', async () => {
    serveUsers((url) =>
      url === '/api/v1/users/far-user'
        ? new Response(JSON.stringify({ id: 'far-user', displayName: 'Far User' }), {
            status: 200,
          })
        : usersPage(['u1'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      window.history.pushState({}, '', '/chat/dm/far-user');
      (page as SearchPage).parseV2Route();
      await settle();
      expect(page.v2Conversation?.peerName).toBe('Far User');

      // Reopened later: named from the cache, with no second lookup.
      window.history.pushState({}, '', '/chat');
      (page as SearchPage).parseV2Route();
      await settle();
      window.history.pushState({}, '', '/chat/dm/far-user');
      (page as SearchPage).parseV2Route();
      await settle();
      expect(page.v2Conversation?.peerName).toBe('Far User');
      expect(
        vi.mocked(apiFetch).mock.calls.filter((c) => c[0] === '/api/v1/users/far-user').length
      ).toBe(1);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: request counts per trigger', () => {
  const cases: Array<{
    trigger: string;
    run: (page: ChatPage, held: Deferred<Response>) => Promise<void>;
  }> = [
    {
      trigger: 'cold mount',
      run: async (_page, held): Promise<void> => {
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        await settle();
      },
    },
    {
      trigger: 'open a conversation, then return to /chat mid-load (separate update batches)',
      run: async (page, held): Promise<void> => {
        page.v2Conversation = { projectId: 'p1' };
        await settle();
        page.v2Conversation = null;
        page.loadHubMembers();
        await settle();
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        await settle();
      },
    },
    {
      trigger: 'open and close a conversation within one update batch mid-load',
      run: async (page, held): Promise<void> => {
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        for (let i = 0; i < 3; i++) await Promise.resolve();
        queueMicrotask(() => {
          page.v2Conversation = null;
          page.loadHubMembers();
        });
        page.v2Conversation = {
          projectId: 'p1',
          conversationKey: 'p1',
          isDM: false,
        };
        await settle();
      },
    },
    {
      trigger: 'disconnect and reconnect mid-load',
      run: async (page, held): Promise<void> => {
        document.body.removeChild(page);
        document.body.appendChild(page);
        await settle();
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        await settle();
      },
    },
    {
      trigger: 'fallback poll (three refresh calls) mid-load',
      run: async (page, held): Promise<void> => {
        page.loadHubMembers({ refresh: true });
        page.loadHubMembers({ refresh: true });
        page.loadHubMembers({ refresh: true });
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        await settle();
      },
    },
    {
      trigger: 'ten minutes idle on /chat after the load',
      run: async (_page, held): Promise<void> => {
        held.resolve(usersPage(['u1', 'u2'], 'u-cursor'));
        await settle();
        // Four polls inside the TTL: none reloads the page.
        await vi.advanceTimersByTimeAsync(4 * 60_000);
        await settle();
      },
    },
  ];

  for (const c of cases) {
    it(`${c.trigger}: one users request and one store walk, lists published`, async () => {
      const held = deferred<Response>();
      let userCall = 0;
      serveUsers(() => (++userCall === 1 ? held.promise : usersPage(['u1', 'u2'], 'u-cursor')));
      const page = await mountPage();
      try {
        await c.run(page, held);
        expect(usersRequests()).toBe(1);
        expect(storeWalks()).toBe(1);
        expect(pageAgentRequests()).toBe(0);
        expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
        expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      } finally {
        unmount(page);
      }
    });
  }

  it('an hour idle on /chat: one users request per TTL, not one walk per poll', async () => {
    serveUsers(() => usersPage(['u1', 'u2'], 'u-cursor'));
    const page = await mountPage();
    try {
      expect(usersRequests()).toBe(1);
      await vi.advanceTimersByTimeAsync(60 * 60_000);
      await settle();
      // 60 polls; the page is reloaded only once it is RECENT_USERS_TTL_MS old.
      expect(usersRequests()).toBe(1 + 60 / 5);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: deactivated users', () => {
  function statusPage(
    users: Array<{ id: string; status?: string }>,
    nextCursor?: string
  ): Response {
    const rows = users.map((u) => ({ ...u, displayName: u.id }));
    return new Response(JSON.stringify({ users: rows, ...(nextCursor ? { nextCursor } : {}) }), {
      status: 200,
    });
  }

  it('the recent page hides a suspended user and shows active, invited and status-less users', async () => {
    serveUsers(() =>
      statusPage([
        { id: 'u-active', status: 'active' },
        { id: 'u-suspended', status: 'suspended' },
        { id: 'u-invited', status: 'invited' },
        { id: 'u-none' },
      ])
    );
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u-active', 'u-invited', 'u-none']);
      expect(ids(page.v2Members)).not.toContain('u-suspended');
    } finally {
      unmount(page);
    }
  });

  it('a suspended user found by search is hidden too', async () => {
    serveUsers((url) =>
      url.startsWith('/api/v1/users?search=')
        ? statusPage([
            { id: 'far-active', status: 'active' },
            { id: 'far-suspended', status: 'suspended' },
          ])
        : statusPage([{ id: 'u1', status: 'active' }], 'u-cursor')
    );
    const page = await mountPage();
    try {
      (
        page as ChatPage & { _handleMemberSearch(e: CustomEvent<{ query: string }>): void }
      )._handleMemberSearch(new CustomEvent('member-search', { detail: { query: 'far' } }));
      await vi.advanceTimersByTimeAsync(250);
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'far-active']);
    } finally {
      unmount(page);
    }
  });
});
