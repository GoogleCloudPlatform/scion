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
 * The agent detail page seeds the state store from its agent response.
 * A live status change that lands while that request (or the requests the
 * initial load waits on) is in flight must survive the older response
 * (ptone/scion#2982). Uses the real stateManager; only fetch is faked.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return { navigateTo: vi.fn(), stateManager: state.stateManager };
});

import './agent-detail.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

const AGENT_ID = 'agent-1';

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: AGENT_ID,
    name: 'agent-1',
    projectId: 'p1',
    template: 'default',
    phase: 'running',
    ...overrides,
  } as Agent;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

async function settle(): Promise<void> {
  // The store flushes on rAF or after 100ms without one.
  await new Promise((r) => setTimeout(r, 150));
}

type DetailEl = HTMLElement & {
  agentId: string;
  agent: Agent | null;
  updateComplete: Promise<unknown>;
  fetchAndMergeAgent(): Promise<void>;
};

/** Requests held until the test releases them, keyed by URL. */
let held: Map<string, () => void>;
/** Body returned for the agent request. */
let agentBody: Agent;
/** URLs to hold instead of answering at once. */
let holdUrls: Set<string>;

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  stateManager.setScope({ type: 'brokers-list' });
  held = new Map();
  holdUrls = new Set();
  agentBody = agent();
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      const path = url.replace(/^https?:\/\/[^/]+/, '');
      let body: unknown = {};
      let status = 404;
      if (path === `/api/v1/agents/${AGENT_ID}`) {
        body = agentBody;
        status = 200;
      } else if (path === '/api/v1/projects/p1') {
        body = { id: 'p1', name: 'P1' };
        status = 200;
      }
      const respond = (): Response => jsonResponse(body, status);
      if (holdUrls.has(path)) {
        return new Promise<Response>((resolve) => held.set(path, () => resolve(respond())));
      }
      return Promise.resolve(respond());
    })
  );
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
});

async function mount(): Promise<DetailEl> {
  const el = document.createElement('scion-page-agent-detail') as DetailEl;
  el.agentId = AGENT_ID;
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  return el;
}

async function waitForHeld(path: string): Promise<void> {
  for (let i = 0; i < 50 && !held.has(path); i++) {
    await new Promise((r) => setTimeout(r, 0));
  }
  expect(held.has(path)).toBe(true);
}

describe('agent detail seed epoch', () => {
  it('keeps a live change that lands during the initial load', async () => {
    holdUrls.add('/api/v1/projects/p1');
    const el = await mount();
    await waitForHeld('/api/v1/projects/p1');

    // The agent response (running) is read; the page is still waiting on
    // the project when the agent stops.
    handleUpdate(`agent.${AGENT_ID}.status`, { agentId: AGENT_ID, phase: 'stopped' });
    held.get('/api/v1/projects/p1')?.();
    await settle();
    await el.updateComplete;

    expect(stateManager.getAgent(AGENT_ID)?.phase).toBe('stopped');
    expect(el.agent?.phase).toBe('stopped');
  });

  it('keeps a live change that lands during a background refresh', async () => {
    const el = await mount();
    await settle();
    expect(stateManager.getAgent(AGENT_ID)?.phase).toBe('running');

    holdUrls.add(`/api/v1/agents/${AGENT_ID}`);
    const refresh = el.fetchAndMergeAgent();
    await waitForHeld(`/api/v1/agents/${AGENT_ID}`);

    // The refresh response was read before the agent stopped.
    handleUpdate(`agent.${AGENT_ID}.status`, { agentId: AGENT_ID, phase: 'stopped' });
    await settle();
    held.get(`/api/v1/agents/${AGENT_ID}`)?.();
    await refresh;
    await settle();
    await el.updateComplete;

    expect(stateManager.getAgent(AGENT_ID)?.phase).toBe('stopped');
    expect(el.agent?.phase).toBe('stopped');
  });

  it('keeps a live change that lands during the agent request of a reload', async () => {
    // A reload (Retry) in the agent's own scope: setScope is then a no-op,
    // so the epoch opened before the agent request stays open.
    stateManager.setScope({ type: 'agent-detail', projectId: 'p1', agentId: AGENT_ID });
    holdUrls.add(`/api/v1/agents/${AGENT_ID}`);
    const el = await mount();
    await waitForHeld(`/api/v1/agents/${AGENT_ID}`);

    // The agent response was read before the agent stopped.
    handleUpdate(`project.p1.agent.${AGENT_ID}.status`, { agentId: AGENT_ID, phase: 'stopped' });
    held.get(`/api/v1/agents/${AGENT_ID}`)?.();
    await settle();
    await el.updateComplete;

    expect(stateManager.getAgent(AGENT_ID)?.phase).toBe('stopped');
    expect(el.agent?.phase).toBe('stopped');
  });

  it('does not seed or show a refresh response after the user left the view', async () => {
    const el = await mount();
    await settle();
    expect(el.agent?.phase).toBe('running');

    holdUrls.add(`/api/v1/agents/${AGENT_ID}`);
    agentBody = agent({ phase: 'stopped' });
    const refresh = el.fetchAndMergeAgent();
    await waitForHeld(`/api/v1/agents/${AGENT_ID}`);

    // The user moves to another agent's page: the scope changes and the
    // page now shows a different agent.
    stateManager.setScope({ type: 'agent-detail', projectId: 'p1', agentId: 'agent-2' });
    el.agentId = 'agent-2';
    held.get(`/api/v1/agents/${AGENT_ID}`)?.();
    await refresh;
    await settle();

    expect(stateManager.getAgent(AGENT_ID)).toBeUndefined();
    expect(el.agent?.phase).toBe('running');
  });

  it('shows a refresh response after a scope change without seeding it', async () => {
    const el = await mount();
    await settle();

    holdUrls.add(`/api/v1/agents/${AGENT_ID}`);
    agentBody = agent({ phase: 'stopped' });
    const refresh = el.fetchAndMergeAgent();
    await waitForHeld(`/api/v1/agents/${AGENT_ID}`);

    stateManager.setScope({ type: 'brokers-list' });
    held.get(`/api/v1/agents/${AGENT_ID}`)?.();
    await refresh;
    await settle();

    expect(stateManager.getAgent(AGENT_ID)).toBeUndefined();
    expect(el.agent?.phase).toBe('stopped');
  });
});
