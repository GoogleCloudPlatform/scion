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
 * The standalone agent graph loads every agent with one unscoped request.
 * Only a whole response (an array, or an object with no `nextCursor`) marks
 * the store's agent set complete with full objects; a response that names
 * a next page, or a failed one, leaves the set unmarked.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LitElement } from 'lit';
import type { Agent, PageData } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

type GraphPage = LitElement & { pageData: PageData | null };

function agent(id: string, projectId = 'p1'): Agent {
  return {
    id,
    name: `Agent ${id}`,
    slug: id,
    projectId,
    template: 't',
    phase: 'running',
  } as Agent;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

let agentRequests: string[];
let page: GraphPage | null = null;

function stubAgentsResponse(response: () => Response): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (new URL(url, 'http://localhost').pathname === '/api/v1/agents') {
        agentRequests.push(url);
        return Promise.resolve(response());
      }
      return Promise.resolve(jsonResponse({}, 404));
    })
  );
}

async function mountGraph(): Promise<GraphPage> {
  const el = document.createElement('scion-page-agent-graph') as GraphPage;
  el.pageData = {
    path: '/agents/graph',
    title: 'Agent graph',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  page = el;
  await vi.waitFor(() => expect((el as unknown as { loading: boolean }).loading).toBe(false));
  await el.updateComplete;
  return el;
}

beforeAll(async () => {
  await import('./agent-graph.js');
}, 60_000);

beforeEach(() => {
  agentRequests = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  // Start from another scope so the page's own switch to the dashboard
  // scope clears the store and its completeness flag.
  stateManager.setScope({ type: 'project', projectId: 'elsewhere' });
  expect(stateManager.isAgentSetComplete('compact')).toBe(false);
});

afterEach(() => {
  page?.remove();
  page = null;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('/agents/graph completeness flag', () => {
  it('a complete unscoped response marks the set complete with full objects', async () => {
    stubAgentsResponse(() => jsonResponse({ agents: [agent('a1'), agent('a2', 'p2')] }));
    await mountGraph();
    expect(agentRequests).toHaveLength(1);
    const u = new URL(agentRequests[0], 'http://localhost');
    expect(u.search).toBe('');
    expect(stateManager.isAgentSetComplete('full')).toBe(true);
    expect(
      stateManager
        .getAgents()
        .map((a) => a.id)
        .sort()
    ).toEqual(['a1', 'a2']);
  });

  it('a bare array response marks the set complete with full objects', async () => {
    stubAgentsResponse(() => jsonResponse([agent('a1')]));
    await mountGraph();
    expect(agentRequests).toHaveLength(1);
    expect(stateManager.isAgentSetComplete('full')).toBe(true);
  });

  it('a response with a nextCursor does not mark the set complete', async () => {
    stubAgentsResponse(() => jsonResponse({ agents: [agent('a1')], nextCursor: 'next' }));
    await mountGraph();
    expect(agentRequests).toHaveLength(1);
    // The page still shows what it loaded.
    expect(stateManager.getAgents().map((a) => a.id)).toEqual(['a1']);
    expect(stateManager.isAgentSetComplete('full')).toBe(false);
    expect(stateManager.isAgentSetComplete('compact')).toBe(false);
  });

  it('a failed response does not mark the set complete', async () => {
    stubAgentsResponse(() => jsonResponse({ error: { message: 'boom' } }, 500));
    vi.spyOn(console, 'error').mockImplementation(() => {});
    await mountGraph();
    expect(agentRequests).toHaveLength(1);
    expect(stateManager.isAgentSetComplete('compact')).toBe(false);
  });
});

/**
 * Serves the agents list from `rows()` at the time each response is
 * released. Each request waits until `release()` lets it through.
 */
function stubHeldAgents(rows: () => Agent[]): { release(): void; held(): number } {
  const gates: Array<() => void> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string | URL | Request) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (new URL(url, 'http://localhost').pathname !== '/api/v1/agents') {
        return jsonResponse({}, 404);
      }
      agentRequests.push(url);
      await new Promise<void>((resolve) => gates.push(resolve));
      return jsonResponse({ agents: rows() });
    })
  );
  return {
    release: () => gates.shift()?.(),
    held: () => gates.length,
  };
}

function liveUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({
    subject,
    data,
  });
  (stateManager as unknown as { flush(): void }).flush();
}

interface GraphInternals {
  agents: Agent[];
  loading: boolean;
  fetchAgents(quiet: boolean): Promise<void>;
}

function graphInternals(el: GraphPage): GraphInternals {
  return el as unknown as GraphInternals;
}

/** Mounts the graph and returns once its first request is held. */
async function mountHeld(h: { held(): number }): Promise<GraphPage> {
  const el = document.createElement('scion-page-agent-graph') as GraphPage;
  el.pageData = {
    path: '/agents/graph',
    title: 'Agent graph',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  page = el;
  await vi.waitFor(() => expect(h.held()).toBe(1));
  return el;
}

describe('/agents/graph live changes while its request is in flight', () => {
  it('a status for an agent not yet in the store survives the seed', async () => {
    const h = stubHeldAgents(() => [agent('a1'), agent('a2')]);
    const el = await mountHeld(h);
    liveUpdate('agent.a1.status', { agentId: 'a1', phase: 'stopped' });
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    expect(stateManager.isAgentSetComplete('full')).toBe(true);
    expect(stateManager.getAgent('a1')?.phase).toBe('stopped');
    expect(graphInternals(el).agents.find((a) => a.id === 'a1')?.phase).toBe('stopped');
  });

  it('an activity for an agent not yet in the store survives the seed', async () => {
    const h = stubHeldAgents(() => [agent('a1')]);
    const el = await mountHeld(h);
    liveUpdate('agent.a1.status', { agentId: 'a1', activity: 'thinking' });
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    expect(stateManager.getAgent('a1')?.activity).toBe('thinking');
    expect(graphInternals(el).agents[0]?.activity).toBe('thinking');
  });

  it('a delete neither renders the agent nor keeps it in the store', async () => {
    const h = stubHeldAgents(() => [agent('a1'), agent('a2')]);
    const el = await mountHeld(h);
    liveUpdate('agent.a2.deleted', { agentId: 'a2' });
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    expect(graphInternals(el).agents.map((a) => a.id)).toEqual(['a1']);
    expect(stateManager.getAgent('a2')).toBeUndefined();
    expect(stateManager.isAgentSetComplete('full')).toBe(true);
  });

  it('a create the response predates is rendered and kept in the store', async () => {
    const h = stubHeldAgents(() => [agent('a1')]);
    const el = await mountHeld(h);
    liveUpdate('agent.n1.created', agent('n1', 'p2'));
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    expect(graphInternals(el).agents.map((a) => a.id)).toEqual(['a1', 'n1']);
    expect(stateManager.getAgent('n1')).toBeDefined();
  });

  it('a status for a known agent during the ancestry refetch survives the older row', async () => {
    const h = stubHeldAgents(() => [agent('a1'), agent('a2')]);
    const el = await mountHeld(h);
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    const refetch = graphInternals(el).fetchAgents(true);
    await vi.waitFor(() => expect(h.held()).toBe(1));
    liveUpdate('agent.a1.status', { agentId: 'a1', phase: 'stopped' });
    liveUpdate('agent.a2.deleted', { agentId: 'a2' });
    h.release();
    await refetch;
    expect(stateManager.getAgent('a1')?.phase).toBe('stopped');
    expect(graphInternals(el).agents.map((a) => [a.id, a.phase])).toEqual([['a1', 'stopped']]);
    expect(stateManager.getAgent('a2')).toBeUndefined();
  });
});

describe('/agents/graph seeding', () => {
  it('a later response replaces the stored fields: a field it omits is gone', async () => {
    let rows: Agent[] = [{ ...agent('a1'), taskSummary: 'x' } as Agent];
    const h = stubHeldAgents(() => rows);
    const el = await mountHeld(h);
    h.release();
    await vi.waitFor(() => expect(graphInternals(el).loading).toBe(false));
    expect(stateManager.getAgent('a1')?.taskSummary).toBe('x');

    rows = [agent('a1')];
    const refetch = graphInternals(el).fetchAgents(true);
    await vi.waitFor(() => expect(h.held()).toBe(1));
    h.release();
    await refetch;
    expect(stateManager.getAgent('a1')).toBeDefined();
    expect(stateManager.getAgent('a1')).not.toHaveProperty('taskSummary');
    expect(graphInternals(el).agents[0]).not.toHaveProperty('taskSummary');
  });
});
