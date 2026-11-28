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
 * Tests for Status -> Current State timestamps and idle auto-suspend countdown
 * on the Agent Detail page (Container started, Activity badge activitySince
 * alignment + fallback, Idle since + Suspends in / Suspends soon / Never
 * suspends / absent, and SSE phase/activity transition refresh).
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

class FakeStateManager extends EventTarget {
  private agentsById = new Map<string, Record<string, unknown>>();
  private deletedIds = new Set<string>();
  setAgent(agent: Record<string, unknown> & { id: string }): void {
    this.agentsById.set(agent.id, agent);
  }
  getAgent(id: string): Record<string, unknown> | undefined {
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
  seedAgents(agents: Array<Record<string, unknown> & { id: string }>): void {
    for (const a of agents) this.agentsById.set(a.id, a);
  }
  seedProjects(): void {}
  readonly scopeGeneration = 0;
  beginSeedEpoch(): symbol {
    return Symbol('seed-epoch');
  }
  endSeedEpoch(): void {}
  notifyAgentsChanged(upserted: string[]): void {
    this.dispatchEvent(
      new CustomEvent('agents-changed', {
        detail: {
          data: {
            upserted,
            deleted: [],
            unknown: new Map(),
            generation: 0,
          },
        },
      })
    );
  }
  reset(): void {
    this.agentsById.clear();
    this.deletedIds.clear();
  }
}

const fakeStateManager = new FakeStateManager();
const apiFetch = vi.fn();
let mockNeverSuspendEnabled = false;

vi.mock('../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

vi.mock('../../client/state.js', () => ({
  get stateManager(): FakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../../client/main.js', async () => ({
  ...(await import('../../client/__fixtures__/main-stub.js')),
  get stateManager(): FakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../../utils/feature-flags.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../utils/feature-flags.js')>();
  return {
    ...actual,
    neverSuspendEnabled: () => mockNeverSuspendEnabled,
  };
});

vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

await import('./agent-detail.js');
type ScionPageAgentDetail = import('./agent-detail.js').ScionPageAgentDetail;
type Agent = import('../../shared/types.js').Agent;

const AGENT_ID = 'agent-ts-1';
const NOW = new Date('2026-11-28T12:00:00Z');

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

type DetailHost = ScionPageAgentDetail & {
  agentId: string;
  agent: Agent | null;
  loading: boolean;
  loadData(): Promise<void>;
};

async function mountWithAgent(agent: Agent): Promise<DetailHost> {
  const el = document.createElement('scion-page-agent-detail') as DetailHost;
  el.agentId = agent.id;
  el.agent = agent;
  el.loading = false;
  // Stub loadData so connectedCallback doesn't overwrite our test agent.
  vi.spyOn(el, 'loadData').mockResolvedValue();
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function baseAgent(overrides: Record<string, unknown> = {}): Agent {
  const agent: Record<string, unknown> = {
    id: AGENT_ID,
    name: 'Timestamp Agent',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    activity: 'idle',
    created: '2026-11-28T09:00:00Z',
    updated: '2026-11-28T11:58:00Z',
    lastSeen: '2026-11-28T11:59:45Z',
    startedAt: '2026-11-28T10:00:00Z',
    activitySince: '2026-11-28T11:55:00Z',
    lastActivityEvent: '2026-11-28T11:59:00Z',
    idleSuspendAt: '2026-11-28T12:10:00Z',
  };
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) {
      delete agent[k];
    } else {
      agent[k] = v;
    }
  }
  return agent as unknown as Agent;
}

describe('agent detail Status -> Current State timestamps and auto-suspend', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
    mockNeverSuspendEnabled = false;
    apiFetch.mockReset();
    fakeStateManager.reset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
    fakeStateManager.reset();
  });

  it('renders Container started with absolute and relative time, or dash when unset/zero', async () => {
    const el = await mountWithAgent(baseAgent());
    const row = el.shadowRoot?.querySelector('[data-testid="agent-container-started"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Container started');
    expect(row?.textContent).toContain('2 hours ago');

    el.agent = baseAgent({ startedAt: '0001-01-01T00:00:00Z' });
    await el.updateComplete;
    const zeroRow = el.shadowRoot?.querySelector('[data-testid="agent-container-started"]');
    expect(zeroRow?.textContent).toContain('—');

    el.agent = baseAgent({ startedAt: undefined });
    await el.updateComplete;
    const unsetRow = el.shadowRoot?.querySelector('[data-testid="agent-container-started"]');
    expect(unsetRow?.textContent).toContain('—');
  });

  it('renders Last reincarnated with absolute and relative time, or Never when unset/zero', async () => {
    const el = await mountWithAgent(
      baseAgent({
        _capabilities: { actions: ['read', 'lifecycle'] },
        lastReincarnatedAt: '2026-11-28T11:30:00Z',
      })
    );
    const row = el.shadowRoot?.querySelector('[data-testid="agent-last-reincarnated"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Last reincarnated');
    expect(row?.textContent).toContain('30 minutes ago');

    el.agent = baseAgent({
      _capabilities: { actions: ['read', 'lifecycle'] },
      lastReincarnatedAt: undefined,
    });
    await el.updateComplete;
    const neverRow = el.shadowRoot?.querySelector('[data-testid="agent-last-reincarnated"]');
    expect(neverRow?.textContent).toContain('Never');
    expect(neverRow?.textContent).not.toContain('ago');

    el.agent = baseAgent({
      _capabilities: { actions: ['read', 'lifecycle'] },
      lastReincarnatedAt: '0001-01-01T00:00:00Z',
    });
    await el.updateComplete;
    const zeroRow = el.shadowRoot?.querySelector('[data-testid="agent-last-reincarnated"]');
    expect(zeroRow?.textContent).toContain('Never');
  });

  it('aligns Activity badge relative time to activitySince when present and falls back when absent', async () => {
    // activitySince = 11:55 (5m ago), lastActivityEvent = 11:59 (1m ago)
    const el = await mountWithAgent(baseAgent());
    const badgeTime = el.shadowRoot?.querySelector('[data-testid="agent-activity-since"]');
    expect(badgeTime?.textContent?.trim()).toBe('5 minutes ago');

    // Suspended agent has no activitySince -> falls back to lastActivityEvent (1m ago)
    el.agent = baseAgent({
      phase: 'suspended',
      activity: 'idle',
      activitySince: undefined,
      idleSuspendAt: undefined,
      lastActivityEvent: '2026-11-28T11:59:00Z',
    });
    await el.updateComplete;
    const fallbackBadgeTime = el.shadowRoot?.querySelector('[data-testid="agent-activity-since"]');
    expect(fallbackBadgeTime?.textContent?.trim()).toBe('1 minute ago');
  });

  it('renders Idle since and Suspends in countdown when idleSuspendAt is in the future', async () => {
    const el = await mountWithAgent(baseAgent());
    const row = el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Idle since 5 minutes ago');
    expect(row?.textContent).toContain('Suspends in 10 minutes');
  });

  it('clamps past-due idleSuspendAt to "Suspends soon"', async () => {
    const el = await mountWithAgent(
      baseAgent({
        activitySince: '2026-11-28T11:40:00Z',
        idleSuspendAt: '2026-11-28T11:55:00Z', // 5 minutes in the past
      })
    );
    const row = el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Idle since 20 minutes ago');
    expect(row?.textContent).toContain('Suspends soon');
    expect(row?.textContent).not.toContain('Suspends 5 minutes ago');
  });

  it('renders "Never suspends" when never-suspend policy is active', async () => {
    mockNeverSuspendEnabled = true;
    const el = await mountWithAgent(
      baseAgent({
        suspendPolicy: 'never',
        idleSuspendAt: undefined,
      })
    );
    const row = el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Idle since 5 minutes ago');
    expect(row?.textContent).toContain('Never suspends');

    // Even when working (not idle), Never suspends still renders on running agent
    el.agent = baseAgent({
      activity: 'working',
      suspendPolicy: 'never',
      idleSuspendAt: undefined,
    });
    await el.updateComplete;
    const workingRow = el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]');
    expect(workingRow).not.toBeNull();
    expect(workingRow?.textContent).toContain('Never suspends');
    expect(workingRow?.textContent).not.toContain('Idle since');
  });

  it('renders Idle since without countdown when idleSuspendAt is absent (blocked/disabled)', async () => {
    const el = await mountWithAgent(
      baseAgent({
        idleSuspendAt: undefined,
      })
    );
    const row = el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]');
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain('Idle since 5 minutes ago');
    expect(row?.textContent).not.toContain('Suspends');
    expect(row?.textContent).not.toContain('Never suspends');
  });

  it('omits Auto-suspend row for non-running agents and working agents without never-suspend', async () => {
    const el = await mountWithAgent(
      baseAgent({
        phase: 'running',
        activity: 'working',
        idleSuspendAt: undefined,
      })
    );
    expect(el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]')).toBeNull();

    el.agent = baseAgent({
      phase: 'suspended',
      activity: 'idle',
      activitySince: undefined,
      idleSuspendAt: undefined,
    });
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="agent-idle-suspend"]')).toBeNull();
  });

  it('clears stale idleSuspendAt on non-qualifying SSE transition and triggers backgroundRefresh on phase/activity change', async () => {
    apiFetch.mockResolvedValue(
      jsonResponse(
        200,
        baseAgent({
          phase: 'running',
          activity: 'working',
          activitySince: '2026-11-28T12:00:00Z',
          idleSuspendAt: undefined,
        })
      )
    );

    const el = await mountWithAgent(baseAgent());
    expect(el.agent?.idleSuspendAt).toBe('2026-11-28T12:10:00Z');

    // Same phase & activity SSE update should NOT trigger backgroundRefresh
    fakeStateManager.setAgent({
      id: AGENT_ID,
      phase: 'running',
      activity: 'idle',
      taskSummary: 'x',
    });
    fakeStateManager.notifyAgentsChanged([AGENT_ID]);
    expect(apiFetch).not.toHaveBeenCalled();

    // SSE transition idle -> working clears stale idleSuspendAt immediately and refetches
    fakeStateManager.setAgent({ id: AGENT_ID, phase: 'running', activity: 'working' });
    fakeStateManager.notifyAgentsChanged([AGENT_ID]);
    expect(el.agent?.idleSuspendAt).toBeUndefined();
    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/agents/${AGENT_ID}`);
  });

  it('clears stale activitySince and idleSuspendAt when SSE transitions away from running without refetching', async () => {
    const el = await mountWithAgent(baseAgent());
    fakeStateManager.setAgent({ id: AGENT_ID, phase: 'suspended', activity: 'idle' });
    fakeStateManager.notifyAgentsChanged([AGENT_ID]);

    expect(el.agent?.idleSuspendAt).toBeUndefined();
    expect(el.agent?.activitySince).toBeUndefined();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('refetches activitySince and idleSuspendAt when SSE transitions working -> idle while running', async () => {
    apiFetch.mockResolvedValue(
      jsonResponse(
        200,
        baseAgent({
          phase: 'running',
          activity: 'idle',
          activitySince: '2026-11-28T12:00:00Z',
          idleSuspendAt: '2026-11-28T12:15:00Z',
        })
      )
    );

    await mountWithAgent(
      baseAgent({
        phase: 'running',
        activity: 'working',
        idleSuspendAt: undefined,
      })
    );
    fakeStateManager.setAgent({ id: AGENT_ID, phase: 'running', activity: 'idle' });
    fakeStateManager.notifyAgentsChanged([AGENT_ID]);

    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/agents/${AGENT_ID}`);
  });
});
