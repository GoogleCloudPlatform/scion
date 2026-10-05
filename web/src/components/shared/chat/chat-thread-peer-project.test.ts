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
 * An agent DM's peer project, which path links in the DM resolve against
 * when a message carries no project of its own: the global agent map (the
 * current view's full rows), then the agent store's hub list, then one read
 * of the peer per conversation. Never a hub walk, and nothing is written to
 * the global map.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { AgentListSnapshot } from '../../../client/agent-store.js';
import type { Agent } from '../../../shared/types.js';

/** The global agent map, as far as the thread uses it. */
const globalMap = vi.hoisted(() => {
  const agents = new Map<string, Agent>();
  return {
    agents,
    stateManager: Object.assign(new EventTarget(), {
      currentScope: null,
      isConnected: false,
      getAgent: (id: string): Agent | undefined => agents.get(id),
      seedAgents: vi.fn((list: Agent[]): void => {
        for (const a of list) agents.set(a.id, a);
      }),
    }),
  };
});

const store = vi.hoisted(() => ({
  hub: undefined as AgentListSnapshot | undefined,
  ensure: vi.fn(),
  retain: vi.fn(),
}));

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: unknown) => Promise<Response>>());

vi.mock('../../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: globalMap.stateManager,
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

vi.mock('../../../client/agent-store.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../client/agent-store.js')>();
  return {
    ...actual,
    agentStore: {
      peek: (): AgentListSnapshot | undefined => store.hub,
      ensure: store.ensure,
      retain: store.retain,
    },
  };
});

const { agentIndexOf } = await import('../../../client/agent-store.js');
await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

/** The thread's private surface these tests read. */
interface ThreadInternals {
  peerAgentProjectId(): string;
}

/** Peers the single-agent endpoint knows, by id. */
let peers: Record<string, string>;
let peerStatus: number;

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function singleAgentReads(id?: string): number {
  return apiFetch.mock.calls.filter(([path]) => {
    const match = /^\/api\/v1\/agents\/([^/?]+)$/.exec(path);
    return match !== null && (id === undefined || decodeURIComponent(match[1] ?? '') === id);
  }).length;
}

function agentListRequests(): number {
  return apiFetch.mock.calls.filter(([path]) =>
    /^\/api\/v1\/(agents|projects\/[^/]+\/agents)(\?|$)/.test(path)
  ).length;
}

function hubSnapshot(agents: Agent[], version = 1): AgentListSnapshot {
  return { key: 'hub', agents, status: 'ready', complete: true, version };
}

function row(id: string, projectId: string): Agent {
  return { id, name: id, projectId, template: '', phase: 'running' } as Agent;
}

/** An agent DM thread opened and loaded (history, then its follow-up reads). */
async function openDM(peerId: string, el?: ScionChatThread): Promise<ScionChatThread> {
  const thread = el ?? document.createElement('scion-chat-thread');
  thread.conversationKey = `dm:agent:${peerId}:user:u1`;
  thread.isDM = true;
  thread.projectId = 'proj-inherited';
  if (!thread.isConnected) document.body.appendChild(thread);
  await thread.updateComplete;
  await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
  return thread;
}

function peerProject(el: ScionChatThread): string {
  return (el as unknown as ThreadInternals).peerAgentProjectId();
}

beforeEach(() => {
  peers = { coder: 'proj-coder' };
  peerStatus = 200;
  store.hub = undefined;
  globalMap.agents.clear();
  globalMap.stateManager.seedAgents.mockClear();
  apiFetch.mockReset();
  apiFetch.mockImplementation((path) => {
    const single = /^\/api\/v1\/agents\/([^/?]+)$/.exec(path)?.[1];
    if (single !== undefined) {
      const projectId = peers[decodeURIComponent(single)];
      if (peerStatus !== 200) return Promise.resolve(json({ error: 'x' }, peerStatus));
      if (projectId === undefined) return Promise.resolve(json({ error: 'not found' }, 404));
      return Promise.resolve(json({ id: single, projectId, appliedConfig: { image: 'x' } }));
    }
    return Promise.resolve(json({ items: [] }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('agent DM peer project', () => {
  it('a direct-URL open with no hub list reads the peer once, with no hub walk', async () => {
    const el = await openDM('coder');

    expect(peerProject(el)).toBe('proj-coder');
    expect(singleAgentReads('coder')).toBe(1);
    expect(agentListRequests()).toBe(0);
    expect(store.ensure).not.toHaveBeenCalled();
    expect(store.retain).not.toHaveBeenCalled();
  });

  it('caches the read for the conversation: a reload of the same conversation reads nothing more', async () => {
    const el = await openDM('coder');
    expect(singleAgentReads()).toBe(1);

    (el as unknown as { loaded: boolean }).loaded = false;
    el.loadHistory();
    for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));

    expect(peerProject(el)).toBe('proj-coder');
    expect(singleAgentReads()).toBe(1);
  });

  it('a read for one conversation is not used for another', async () => {
    peers.reviewer = 'proj-reviewer';
    const el = await openDM('coder');
    expect(peerProject(el)).toBe('proj-coder');

    await openDM('reviewer', el);
    await vi.waitFor(() => expect(singleAgentReads('reviewer')).toBe(1));
    expect(peerProject(el)).toBe('proj-reviewer');
  });

  it('a switch to another DM does not answer with the previous peer read before its own lands', async () => {
    const el = await openDM('coder');
    expect(peerProject(el)).toBe('proj-coder');

    // The next conversation's history is held, so its own peer read has not started.
    apiFetch.mockImplementation(() => new Promise<Response>(() => {}));
    el.conversationKey = 'dm:agent:stranger:user:u1';
    await el.updateComplete;

    expect(peerProject(el)).toBe('');
  });

  for (const status of [403, 404]) {
    it(`a ${status} read leaves the project empty and raises no access-denied toast`, async () => {
      if (status === 404) delete peers.coder;
      else peerStatus = status;
      const el = await openDM('coder');

      expect(peerProject(el)).toBe('');
      const reads = apiFetch.mock.calls.filter(([path]) => path === '/api/v1/agents/coder');
      expect(reads).toHaveLength(1);
      expect(reads[0]?.[1]).toMatchObject({ suppressAccessDeniedToast: true });
    });
  }

  it("a late failed read for the previous DM does not clear the next DM's read", async () => {
    peers.reviewer = 'proj-reviewer';
    let failCoder = (): void => {};
    const base = apiFetch.getMockImplementation()!;
    apiFetch.mockImplementation((path, init) => {
      if (path === '/api/v1/agents/coder') {
        return new Promise<Response>((resolve) => {
          failCoder = () => resolve(json({ error: 'x' }, 500));
        });
      }
      return base(path, init);
    });
    const el = await openDM('coder');
    expect(singleAgentReads('coder')).toBe(1);

    await openDM('reviewer', el);
    await vi.waitFor(() => expect(peerProject(el)).toBe('proj-reviewer'));

    failCoder();
    for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
    expect(peerProject(el)).toBe('proj-reviewer');
  });

  it('a failed read is not cached: the next open of the conversation reads again', async () => {
    peerStatus = 500;
    const el = await openDM('coder');
    expect(peerProject(el)).toBe('');
    expect(singleAgentReads()).toBe(1);

    peerStatus = 200;
    (el as unknown as { loaded: boolean }).loaded = false;
    el.loadHistory();
    await vi.waitFor(() => expect(singleAgentReads()).toBe(2));
    await vi.waitFor(() => expect(peerProject(el)).toBe('proj-coder'));
  });

  it('uses the store hub list when it holds the peer, with no read', async () => {
    store.hub = hubSnapshot([row('other', 'p0'), row('coder', 'proj-hub')]);
    const el = await openDM('coder');

    expect(peerProject(el)).toBe('proj-hub');
    expect(singleAgentReads()).toBe(0);
    expect(agentListRequests()).toBe(0);
  });

  it('the agent detail embed uses the page seed of its agent, with no read', async () => {
    // The agent detail page seeds its agent (a full row) into the global map
    // and embeds the agent's DM thread.
    globalMap.agents.set('coder', row('coder', 'proj-detail'));
    store.hub = hubSnapshot([row('coder', 'proj-hub')]);
    const el = await openDM('coder');

    expect(peerProject(el)).toBe('proj-detail');
    expect(singleAgentReads()).toBe(0);
    expect(agentListRequests()).toBe(0);
  });

  it('writes nothing to the global agent map on any path', async () => {
    peers.reviewer = 'proj-reviewer';
    const el = await openDM('coder');
    expect(peerProject(el)).toBe('proj-coder');
    store.hub = hubSnapshot([row('hub-peer', 'proj-hub')]);
    await openDM('hub-peer', el);
    expect(peerProject(el)).toBe('proj-hub');

    expect(globalMap.stateManager.seedAgents).not.toHaveBeenCalled();
    expect(globalMap.agents.size).toBe(0);
  });
});

describe('agentIndexOf', () => {
  it('reuses the index for the same list and version, and builds a new one when either changes', () => {
    const agents = [row('a1', 'p1'), row('a2', 'p2')];
    const first = agentIndexOf(hubSnapshot(agents, 1));
    expect(first.get('a2')?.projectId).toBe('p2');
    expect(agentIndexOf(hubSnapshot(agents, 1))).toBe(first);
    expect(agentIndexOf(hubSnapshot(agents, 2))).not.toBe(first);
    const next = [...agents, row('a3', 'p3')];
    expect(agentIndexOf(hubSnapshot(next, 3)).get('a3')?.projectId).toBe('p3');
  });
});
