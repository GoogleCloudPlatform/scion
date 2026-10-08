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
 * The chat members loader seeds the state store from the space members
 * response. A live status change that lands while that request is in
 * flight must survive the older response, in the store and in the members
 * sidebar (ptone/scion#2982). Uses the real stateManager.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return {
    navigateTo: vi.fn(),
    pushRoute: vi.fn(() => Promise.resolve()),
    replaceRoute: vi.fn(() => Promise.resolve()),
    stateManager: state.stateManager,
  };
});

/** The held members request; resolved by the test. */
let releaseMembers: ((body: unknown) => void) | null = null;

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(
      () =>
        new Promise<Response>((resolve) => {
          releaseMembers = (body: unknown) =>
            resolve(new Response(JSON.stringify(body), { status: 200 }));
        })
    ),
  };
});

import { stateManager } from '../../client/state.js';

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

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

interface ChatInternals extends HTMLElement {
  loadV2Members(projectId: string): Promise<void>;
  v2AgentMembers: Array<{ id: string; phase: string }>;
  _onScopeChanged: () => void;
}

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  stateManager.setScope({ type: 'brokers-list' });
  releaseMembers = null;
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('chat members seed epoch', () => {
  it('keeps a live change that lands while the members request is in flight', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    const load = page.loadV2Members('p1');
    expect(releaseMembers).not.toBeNull();

    // The members response was read before the agent stopped.
    handleUpdate('agent.a1.status', { agentId: 'a1', phase: 'stopped' });
    releaseMembers?.({
      humans: [],
      agents: [{ id: 'a1', kind: 'agent', displayName: 'a1', phase: 'running', projectId: 'p1' }],
    });
    await load;

    expect(stateManager.getAgent('a1')?.phase).toBe('stopped');
    expect(page.v2AgentMembers.find((m) => m.id === 'a1')?.phase).toBe('stopped');
  });

  it('keeps a live change from a scope set while the members request is in flight', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    // The page's own scope-changed listener (connectedCallback adds it).
    stateManager.addEventListener('scope-changed', page._onScopeChanged);
    try {
      const load = page.loadV2Members('p1');
      expect(releaseMembers).not.toBeNull();

      // The chat scope is set while the members load, then a status delta
      // from the new scope lands before the response.
      stateManager.setScope({ type: 'chat', spaceIds: ['p1'], userId: 'u1' });
      handleUpdate('project.p1.agent.a1.status', { agentId: 'a1', phase: 'stopped' });
      releaseMembers?.({
        humans: [],
        agents: [{ id: 'a1', kind: 'agent', displayName: 'a1', phase: 'running', projectId: 'p1' }],
      });
      await load;

      expect(stateManager.getAgent('a1')?.phase).toBe('stopped');
      expect(page.v2AgentMembers.find((m) => m.id === 'a1')?.phase).toBe('stopped');
    } finally {
      stateManager.removeEventListener('scope-changed', page._onScopeChanged);
    }
  });
});
