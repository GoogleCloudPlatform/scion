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
 * Keep suspended on the agent page (ptone/scion#7826): POST suspend on an
 * agent the hub suspended, which the hub turns into a user suspension.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

/** Stand-in for the global stateManager: only the surface agent-detail.ts uses. */
class FakeStateManager extends EventTarget {
  private agentsById = new Map<string, { id: string }>();
  private deletedIds = new Set<string>();
  getAgent(id: string): { id: string } | undefined {
    return this.agentsById.get(id);
  }
  getDeletedAgentIds(): Set<string> {
    return this.deletedIds;
  }
  getProject(): undefined {
    return undefined;
  }
  getProjects(): Array<{ id: string }> {
    return [];
  }
  setScope(): void {}
  getCurrentUserId(): string {
    return '';
  }
  seedAgents(agents: Array<{ id: string }>): void {
    for (const a of agents) this.agentsById.set(a.id, a);
  }
  seedProjects(): void {}
  /** Seed-epoch surface used through AgentSeedEpoch; epochs record nothing here. */
  readonly scopeGeneration = 0;
  beginSeedEpoch(): symbol {
    return Symbol('seed-epoch');
  }
  endSeedEpoch(): void {}
  reset(): void {
    this.agentsById.clear();
    this.deletedIds.clear();
  }
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();

vi.mock('../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

vi.mock('../../client/state.js', () => ({
  get stateManager(): FakeStateManager {
    return fakeStateManager;
  },
}));

// chat-thread (imported by agent-detail) pulls in the app entry point; stub
// it as the other agent-detail tests do.
// Remove once chat-thread stops importing client/main (chat lane, ptone/scion#3118).
vi.mock('../../client/main.js', async () => ({
  ...(await import('../../client/__fixtures__/main-stub.js')),
  get stateManager(): FakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

await import('./agent-detail.js');
import { showToast } from '../../utils/toast.js';
type ScionPageAgentDetail = import('./agent-detail.js').ScionPageAgentDetail;
type Agent = import('../../shared/types.js').Agent;

const AGENT_ID = 'agent-1';
const SUSPEND_URL = `/api/v1/agents/${AGENT_ID}/suspend`;

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

type KeepSuspendedHost = ScionPageAgentDetail & {
  agentId: string;
  agent: Agent;
  keepSuspended(): Promise<void>;
  actionLoading: Record<string, boolean>;
};

function makeElement(): KeepSuspendedHost {
  const el = document.createElement('scion-page-agent-detail') as KeepSuspendedHost;
  el.agentId = AGENT_ID;
  el.agent = {
    id: AGENT_ID,
    name: 'Test Agent',
    projectId: 'proj-1',
    template: 'default',
    phase: 'suspended',
    suspendReason: 'idle',
    _capabilities: { actions: ['read', 'lifecycle'] },
  };
  return el;
}

function suspendCalls(): unknown[][] {
  return apiFetch.mock.calls.filter((c) => c[0] === SUSPEND_URL);
}

describe('agent detail Keep suspended', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    vi.mocked(showToast).mockReset();
  });
  afterEach(() => {
    fakeStateManager.reset();
  });

  it('posts suspend and shows no error on success', async () => {
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve(
        url === SUSPEND_URL ? jsonResponse(200, { agent: {} }) : jsonResponse(404, {})
      )
    );
    const el = makeElement();
    await el.keepSuspended();

    expect(suspendCalls()).toHaveLength(1);
    expect((suspendCalls()[0][1] as RequestInit).method).toBe('POST');
    expect(showToast).not.toHaveBeenCalled();
    expect(el.actionLoading['keep-suspended']).toBe(false);
    // The optimistic phase is untouched: the agent stays suspended.
    expect(el.agent.phase).toBe('suspended');
  });

  it("shows the hub's error when the suspend is refused", async () => {
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve(
        url === SUSPEND_URL
          ? jsonResponse(409, {
              error: { code: 'conflict', message: 'Agent has a lifecycle operation in progress' },
            })
          : jsonResponse(404, {})
      )
    );
    const el = makeElement();
    await el.keepSuspended();

    expect(suspendCalls()).toHaveLength(1);
    expect(showToast).toHaveBeenCalledWith('Agent has a lifecycle operation in progress');
    expect(el.actionLoading['keep-suspended']).toBe(false);
  });
});
