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
 * A detail message the server clears must clear from the space view's
 * agent members, as it does in the hub view (ptone/scion#3690). The rows
 * the members code reads come from the state store, which merges each
 * status event onto the seeded row, so a row without a detail message
 * means the message is gone. Uses the real stateManager.
 */

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
          releaseMembers = (body: unknown): void =>
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
  v2Conversation: { projectId: string; isDM: boolean } | null;
  v2AgentMembers: Array<{ id: string; detailMessage?: string }>;
  _handleAgentsUpdated(): void;
}

/** A members row whose agent last reported a detail message. */
function membersBody(id: string): unknown {
  return {
    humans: [],
    agents: [
      {
        id,
        kind: 'agent',
        displayName: id,
        phase: 'running',
        projectId: 'p1',
        message: 'Cloning repository',
      },
    ],
  };
}

/**
 * A status event after the message cleared: the hub omits an empty
 * message from `detail`, and still sends the other detail fields.
 */
function clearedStatus(id: string): unknown {
  return { agentId: id, projectId: 'p1', phase: 'running', detail: { toolName: 'bash' } };
}

function detailOf(page: ChatInternals, id: string): string | undefined {
  return page.v2AgentMembers.find((m) => m.id === id)?.detailMessage;
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

describe('chat members detail message', () => {
  it('clears a message cleared while the members request is in flight', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    const load = page.loadV2Members('p1');
    expect(releaseMembers).not.toBeNull();

    handleUpdate('agent.d1.status', clearedStatus('d1'));
    releaseMembers?.(membersBody('d1'));
    await load;

    expect(detailOf(page, 'd1')).toBe('');
  });

  it('clears a message on the agents-updated rebuild', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = { projectId: 'p1', isDM: false };
    const load = page.loadV2Members('p1');
    releaseMembers?.(membersBody('d2'));
    await load;
    expect(detailOf(page, 'd2')).toBe('Cloning repository');

    handleUpdate('agent.d2.status', clearedStatus('d2'));
    page._handleAgentsUpdated();

    expect(detailOf(page, 'd2')).toBe('');
  });
});
