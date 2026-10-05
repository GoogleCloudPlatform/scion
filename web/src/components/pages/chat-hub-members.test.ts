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
 * The chat sidebar's hub view: every user (a `/api/v1/users` walk the page
 * owns, single-flight per view) and every agent (the agent store's hub
 * list, retained by the page). Users requests go through the mocked
 * `apiFetch`; agent-list requests go to the store's in-memory server.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import type { AgentStore } from '../../client/agent-store.js';
import { agent, createHarness, settle } from '../../client/__fixtures__/agent-store-harness.js';
import type { Harness } from '../../client/__fixtures__/agent-store-harness.js';
import type { Agent } from '../../shared/types.js';
import type { ChatAgentMember, ChatHumanMember } from '../shared/chat/chat-members.js';

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
    }),
  };
});

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
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

  it('the fallback poll still walks the users (the agents come from the store)', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(usersRequests()).toBe(usersBefore + 1);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('returning to the hub view after a conversation walks the users again', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore + 1);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it("a space's members load replaces the lists, so the next hub view walks the users again", async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      const usersBefore = usersRequests();
      await page.loadV2Members('p1');
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore + 1);
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
  it('walks the users again and issues no agent-list request once the agents are shown', async () => {
    let call = 0;
    serveUsers(() => usersPage([`u${++call}`]));
    const page = await mountPage();
    try {
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      const ensure = vi.spyOn(harness.store, 'ensure');
      const usersBefore = usersRequests();
      const agentRequestsBefore = harness.server.requests.filter(
        (p) => !p.includes('sort=')
      ).length;

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();

      expect(usersRequests()).toBe(usersBefore + 1);
      expect(ids(page.v2HumanMembers)).toEqual([`u${usersBefore + 1}`]);
      // The store's own probes (sort=updated) are not walks.
      expect(harness.server.requests.filter((p) => !p.includes('sort=')).length).toBe(
        agentRequestsBefore
      );
      expect(storeWalks()).toBe(1);
      expect(pageAgentRequests()).toBe(0);
      // The poll does not ask the store while the shown list is ready.
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

  it('the hub view claims the sidebar on its load, not on a publish: after a failed store read the poll walks the users', async () => {
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

      await vi.advanceTimersByTimeAsync(60_000);
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

  it('a users walk dropped for a space claim does not mark the users loaded: the returning hub view walks them', async () => {
    // The mount's users walk is held; it lands after the space's claim.
    const held = deferred<Response>();
    serveUsers(() => held.promise);
    const page = await mountPage();
    try {
      vi.mocked(apiFetch).mockImplementation((url) => {
        if (url.startsWith('/api/v1/users')) return Promise.resolve(usersPage(['hub-user']));
        return Promise.resolve(
          new Response(
            JSON.stringify({ humans: [{ id: 'h1', kind: 'user', displayName: 'h1' }], agents: [] }),
            { status: 200 }
          )
        );
      });
      await page.loadV2Members('p1');
      held.resolve(usersPage(['stale-user']));
      await settle();
      expect(ids(page.v2HumanMembers)).toEqual(['h1']);
      const usersBefore = usersRequests();

      page.loadHubMembers();
      await settle();

      expect(usersRequests()).toBe(usersBefore + 1);
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

  it('opening then closing a conversation mid-walk does not publish a truncated users list; a fresh walk publishes the full one', async () => {
    let userCall = 0;
    const usersPage1 = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? usersPage1.promise : usersPage(['u1', 'u2'])));
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      usersPage1.resolve(usersPage(['u1'], 'u-cursor'));
      await settle();

      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      expect(usersRequests()).toBe(2);
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('opening and closing a conversation within one Lit update batch does not publish a truncated users list', async () => {
    let userCall = 0;
    const usersPage1 = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? usersPage1.promise : usersPage(['u1', 'u2'])));
    const page = await mountPage();
    try {
      usersPage1.resolve(usersPage(['u1'], 'u-cursor'));
      // Up to the microtask where the walk re-checks shouldContinue before
      // its second page.
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

      // The users walk stopped for the moment the conversation was open
      // (page 1 only) and walked again for the hub view still on screen.
      expect(usersRequests()).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
    } finally {
      unmount(page);
    }
  });

  it('a superseded single-page users walk settling last does not overwrite the fresh walk', async () => {
    let userCall = 0;
    const staleUsers = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? staleUsers.promise : usersPage(['fresh-user'])));
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(userCall).toBe(2);

      staleUsers.resolve(usersPage(['stale-user']));
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['fresh-user']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: users pagination and errors', () => {
  it('walks every users page, and the store walks every agents page, with one request per page', async () => {
    harness = createHarness(Array.from({ length: 450 }, (_, i) => agent(`a${i}`)));
    const userIds = Array.from({ length: 120 }, (_, i) => `u${i}`);
    serveUsers((url) =>
      url.includes('cursor=')
        ? usersPage(userIds.slice(100))
        : usersPage(userIds.slice(0, 100), 'u-cursor')
    );
    const page = await mountPage();
    try {
      // Whole walks only: each walk is two users requests.
      expect(usersRequests() % 2).toBe(0);
      expect(storeWalks()).toBe(1);
      expect(harness.server.requests.filter((p) => p.includes('cursor=')).length).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(userIds);
      expect(page.v2AgentMembers.length).toBe(450);
    } finally {
      unmount(page);
    }
  });

  it('de-dupes a user returned on two pages (the offset-pagination boundary-shift case)', async () => {
    serveUsers((url) =>
      url.includes('cursor=') ? usersPage(['u2', 'u3']) : usersPage(['u1', 'u2'], 'u-cursor')
    );
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2', 'u3']);
    } finally {
      unmount(page);
    }
  });

  it('a failed second users page keeps the previous users; a failed agents revalidation keeps the agents', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);

      serveUsers((url) =>
        url.includes('cursor=') ? new Response('', { status: 500 }) : usersPage(['u2'], 'u-cursor')
      );
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
  it('a stalled users page times out, clears the in-flight marker, keeps the users, and the next poll reloads', async () => {
    serveUsers(() => usersPage(['u1']));
    const page = await mountPage();
    try {
      expect(page._hubUsersLoad).toBeNull();
      const usersBefore = usersRequests();

      serveUsers((url, init) => {
        if (!url.includes('cursor=')) return usersPage(['u2'], 'u-cursor');
        return new Promise<Response>((_, reject) => {
          init?.signal?.addEventListener('abort', () => {
            reject(new DOMException('The operation was aborted.', 'AbortError'));
          });
        });
      });
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(page._hubUsersLoad).not.toBeNull();
      expect(usersRequests()).toBe(usersBefore + 2);

      // A view re-parse while the page is stalled joins the walk.
      page.loadHubMembers();
      await settle();
      expect(usersRequests()).toBe(usersBefore + 2);

      await vi.advanceTimersByTimeAsync(60_000);
      await settle();
      expect(page._hubUsersLoad).toBeNull();
      expect(ids(page.v2HumanMembers)).toEqual(['u1']);

      serveUsers(() => usersPage(['u3']));
      page.loadHubMembers({ refresh: true });
      await settle();
      expect(usersRequests()).toBe(usersBefore + 3);
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
  it('a disconnect-then-reconnect while the users walk is in flight starts a fresh walk, and the stale walk does not overwrite it', async () => {
    let userCall = 0;
    const staleUsers = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? staleUsers.promise : usersPage(['u-fresh'])));
    const page = await mountPage();
    try {
      document.body.removeChild(page);
      document.body.appendChild(page);
      await settle();

      staleUsers.resolve(usersPage(['u-stale']));
      await settle();

      expect(ids(page.v2HumanMembers)).toEqual(['u-fresh']);
      expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      // The store answers the reconnected page from memory.
      expect(storeWalks()).toBe(1);
    } finally {
      unmount(page);
    }
  });

  it('a stale load settling after reconnect does not clear the in-flight markers of the still-running fresh loads', async () => {
    let userCall = 0;
    const staleUsers = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? staleUsers.promise : new Promise<Response>(() => {})));
    const release = harness.server.pause();
    const page = await mountPage();
    try {
      document.body.removeChild(page);
      document.body.appendChild(page);
      await settle();
      expect(userCall).toBe(2);
      const freshAgents = page._hubAgentsLoad;
      expect(freshAgents).not.toBeNull();

      staleUsers.resolve(usersPage(['u-stale']));
      await settle();

      expect(page._hubUsersLoad).not.toBeNull();
      expect(page._hubAgentsLoad).toBe(freshAgents);
      page.loadHubMembers();
      await settle();
      expect(userCall).toBe(2);
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

  it('stops requesting users pages once the element disconnects mid-walk', async () => {
    let userCall = 0;
    const page2 = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? usersPage(['u1'], 'u-cursor') : page2.promise));
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();
    page.loadHubMembers();
    await settle();
    expect(userCall).toBe(2);

    page.disconnectedCallback();
    page2.resolve(usersPage(['u2'], 'u2-cursor'));
    await settle();

    expect(userCall).toBe(2);
  });

  it('stops requesting users pages once a conversation opens mid-walk', async () => {
    let userCall = 0;
    const page2 = deferred<Response>();
    serveUsers(() => (++userCall === 1 ? usersPage(['u1'], 'u-cursor') : page2.promise));
    const page = createPage();
    harness.store.retain({ scope: 'hub' }, () => {});
    await harness.connect();
    page.loadHubMembers();
    await settle();
    expect(userCall).toBe(2);

    page.v2Conversation = { projectId: 'p1' };
    page2.resolve(usersPage(['u2'], 'u2-cursor'));
    await settle();

    expect(userCall).toBe(2);
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

describe('hub members: users stopped state is per attempt', () => {
  it('a superseded users walk stopping after a conversation opens and closes does not make the fresh walk re-run or discard its result', async () => {
    const staleUsers = deferred<Response>();
    const freshUsers = deferred<Response>();
    let userCall = 0;
    serveUsers(() => {
      userCall++;
      if (userCall === 1) return staleUsers.promise;
      if (userCall === 2) return freshUsers.promise;
      return usersPage(['u-rerun']);
    });
    const page = await mountPage();
    try {
      page.v2Conversation = { projectId: 'p1' };
      await settle();
      page.v2Conversation = null;
      page.loadHubMembers();
      await settle();
      expect(userCall).toBe(2);

      staleUsers.resolve(usersPage(['u-stale'], 'u-cursor'));
      await settle();
      freshUsers.resolve(usersPage(['u-fresh']));
      await settle();

      expect(usersRequests()).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(['u-fresh']);
    } finally {
      unmount(page);
    }
  });

  it('a superseded users walk stopping after a disconnect and reconnect does not make the fresh walk re-run or discard its result', async () => {
    const staleUsers = deferred<Response>();
    const freshUsers = deferred<Response>();
    let userCall = 0;
    serveUsers(() => {
      userCall++;
      if (userCall === 1) return staleUsers.promise;
      if (userCall === 2) return freshUsers.promise;
      return usersPage(['u-rerun']);
    });
    const page = await mountPage();
    try {
      document.body.removeChild(page);
      document.body.appendChild(page);
      await settle();
      expect(userCall).toBe(2);

      staleUsers.resolve(usersPage(['u-stale'], 'u-cursor'));
      await settle();
      freshUsers.resolve(usersPage(['u-fresh']));
      await settle();

      expect(usersRequests()).toBe(2);
      expect(ids(page.v2HumanMembers)).toEqual(['u-fresh']);
    } finally {
      unmount(page);
    }
  });
});

describe('hub members: request counts per trigger', () => {
  /** Two users pages; the first request can be held so a trigger lands mid-walk. */
  function twoUserPages(holdFirst: Deferred<Response>): void {
    let userCall = 0;
    serveUsers((url) => {
      userCall++;
      if (userCall === 1) return holdFirst.promise;
      return url.includes('cursor=') ? usersPage(['u2']) : usersPage(['u1'], 'u-cursor');
    });
  }

  const cases: Array<{
    trigger: string;
    run: (page: ChatPage, held: Deferred<Response>) => Promise<void>;
    users: number;
  }> = [
    {
      trigger: 'cold mount',
      run: async (_page, held): Promise<void> => {
        held.resolve(usersPage(['u1'], 'u-cursor'));
        await settle();
      },
      users: 2,
    },
    {
      trigger: 'open a conversation, then return to /chat mid-walk (separate update batches)',
      run: async (page, held): Promise<void> => {
        page.v2Conversation = { projectId: 'p1' };
        await settle();
        page.v2Conversation = null;
        page.loadHubMembers();
        await settle();
        held.resolve(usersPage(['u-stale'], 'u-cursor'));
        await settle();
      },
      // Superseded walk: page 1 only (stopped). Fresh walk: 2.
      users: 3,
    },
    {
      trigger: 'open and close a conversation within one update batch mid-walk',
      run: async (page, held): Promise<void> => {
        held.resolve(usersPage(['u-stale'], 'u-cursor'));
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
      // Stopped attempt: page 1 only. Re-run: 2.
      users: 3,
    },
    {
      trigger: 'disconnect and reconnect mid-walk',
      run: async (page, held): Promise<void> => {
        document.body.removeChild(page);
        document.body.appendChild(page);
        await settle();
        held.resolve(usersPage(['u-stale'], 'u-cursor'));
        await settle();
      },
      users: 3,
    },
    {
      trigger: 'fallback poll (three refresh calls) mid-walk',
      run: async (page, held): Promise<void> => {
        page.loadHubMembers({ refresh: true });
        page.loadHubMembers({ refresh: true });
        page.loadHubMembers({ refresh: true });
        held.resolve(usersPage(['u1'], 'u-cursor'));
        await settle();
      },
      // They join the walk: no trailing walk.
      users: 2,
    },
  ];

  for (const c of cases) {
    it(`${c.trigger}: ${c.users} users requests and one store walk, full lists published`, async () => {
      const held = deferred<Response>();
      twoUserPages(held);
      const page = await mountPage();
      try {
        await c.run(page, held);
        expect(usersRequests()).toBe(c.users);
        expect(storeWalks()).toBe(1);
        expect(pageAgentRequests()).toBe(0);
        expect(ids(page.v2HumanMembers)).toEqual(['u1', 'u2']);
        expect(ids(page.v2AgentMembers)).toEqual(['a1', 'a2']);
      } finally {
        unmount(page);
      }
    });
  }
});
