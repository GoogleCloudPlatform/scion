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
 * Suspended-agent "Last screen" card on the agent detail page (ptone/scion#8530).
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

/** Minimal stand-in for stateManager matching agent-detail.ts usage. */
class FakeStateManager extends EventTarget {
  private agentsById = new Map<string, Record<string, unknown>>();
  private deletedIds = new Set<string>();
  getAgent(id: string): Record<string, unknown> | undefined {
    return this.agentsById.get(id);
  }
  getDeletedAgentIds(): Set<string> {
    return this.deletedIds;
  }
  getProject(): undefined {
    return undefined;
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
  reset(): void {
    this.agentsById.clear();
    this.deletedIds.clear();
  }
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();

const xtermMock = vi.hoisted(() => ({
  instances: [] as Array<{
    options: Record<string, unknown>;
    open: ReturnType<typeof vi.fn>;
    write: ReturnType<typeof vi.fn>;
    dispose: ReturnType<typeof vi.fn>;
  }>,
}));

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    options: Record<string, unknown>;
    open = vi.fn();
    write = vi.fn();
    dispose = vi.fn();
    constructor(options: Record<string, unknown> = {}) {
      this.options = options;
      xtermMock.instances.push(this);
    }
  },
}));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '.xterm {}' }));

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

vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

await import('./agent-detail.js');
import {
  agentScreenUrl,
  computeScreenDimensions,
  stripAnsi,
  MAX_SCREEN_ROWS,
  MIN_SCREEN_COLS,
  MAX_SCREEN_COLS,
  NO_SAVED_SCREEN_MESSAGE,
  type ScionAgentSuspendScreen,
} from '../shared/agent-suspend-screen.js';
import type { ScionPageAgentDetail } from './agent-detail.js';
import type { Agent, AgentSuspendScreen } from '../../shared/types.js';

const AGENT_ID = 'agent-1';
const PROJECT_ID = 'proj-1';
const SCREEN_URL = `/api/v1/projects/${PROJECT_ID}/agents/${AGENT_ID}/screen`;

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

async function flushAsync(turns = 6): Promise<void> {
  for (let i = 0; i < turns; i++) {
    await Promise.resolve();
  }
}

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: AGENT_ID,
    name: 'Test Agent',
    projectId: PROJECT_ID,
    template: 'default',
    phase: 'suspended',
    suspendReason: 'idle',
    _capabilities: { actions: ['read', 'attach'] },
    ...overrides,
  };
}

function makeScreenPayload(overrides: Partial<AgentSuspendScreen> = {}): AgentSuspendScreen {
  return {
    agentId: AGENT_ID,
    content: '\x1b[32mPASS\x1b[0m all tests\nline 2\n',
    capturedAt: '2026-11-21T10:04:05Z',
    truncated: false,
    phase: 'suspended',
    ...overrides,
  };
}

function screenCalls(): unknown[][] {
  return apiFetch.mock.calls.filter((c) => typeof c[0] === 'string' && c[0].endsWith('/screen'));
}

describe('agent suspend screen helpers', () => {
  it('builds project-scoped URL when projectId is set and top-level fallback otherwise', () => {
    expect(agentScreenUrl('a/1', 'p/1')).toBe('/api/v1/projects/p%2F1/agents/a%2F1/screen');
    expect(agentScreenUrl('a-1')).toBe('/api/v1/agents/a-1/screen');
  });

  it('strips ANSI CSI and OSC sequences', () => {
    expect(stripAnsi('\x1b[1;32mhello\x1b[0m \x1b]0;title\x07world')).toBe('hello world');
  });

  it('computes rows capped at MAX_SCREEN_ROWS and cols clamped to [MIN, MAX]', () => {
    const short = computeScreenDimensions('\x1b[31mshort\x1b[0m\nline 2\n');
    expect(short.lineCount).toBe(2);
    expect(short.rows).toBe(2);
    expect(short.cols).toBe(MIN_SCREEN_COLS);

    const tallContent = Array.from({ length: 120 }, (_, i) => `row ${i}`).join('\n') + '\n';
    const tall = computeScreenDimensions(tallContent);
    expect(tall.lineCount).toBe(120);
    expect(tall.rows).toBe(MAX_SCREEN_ROWS);

    const wide = computeScreenDimensions('x'.repeat(250) + '\n');
    expect(wide.cols).toBe(MAX_SCREEN_COLS);
  });
});

describe('scion-agent-suspend-screen component', () => {
  const mounted: HTMLElement[] = [];

  beforeEach(() => {
    apiFetch.mockReset();
    xtermMock.instances.length = 0;
  });

  afterEach(() => {
    for (const el of mounted.splice(0)) {
      el.remove();
    }
    vi.restoreAllMocks();
  });

  function mountScreen(props: {
    agentId?: string;
    projectId?: string;
    phase?: string;
  }): ScionAgentSuspendScreen {
    const el = document.createElement('scion-agent-suspend-screen') as ScionAgentSuspendScreen;
    el.agentId = props.agentId ?? AGENT_ID;
    el.projectId = props.projectId ?? PROJECT_ID;
    el.phase = props.phase ?? 'suspended';
    document.body.appendChild(el);
    mounted.push(el);
    return el;
  }

  it('fetches and renders ANSI screen in a read-only xterm instance on 200 OK', async () => {
    const payload = makeScreenPayload();
    apiFetch.mockResolvedValue(jsonResponse(200, payload));

    const el = mountScreen({});
    await el.updateComplete;
    await flushAsync();
    await el.updateComplete;

    expect(screenCalls()).toHaveLength(1);
    expect(screenCalls()[0][0]).toBe(SCREEN_URL);

    await vi.waitFor(() => {
      expect(xtermMock.instances).toHaveLength(1);
    });
    const term = xtermMock.instances[0];
    expect(term.options.disableStdin).toBe(true);
    expect(term.options.cursorBlink).toBe(false);
    expect(term.options.convertEol).toBe(true);
    expect(term.options.rows).toBe(2);
    expect(term.open).toHaveBeenCalledTimes(1);
    expect(term.write).toHaveBeenCalledWith(payload.content);

    const savedEl = el.shadowRoot?.querySelector('[data-testid="suspend-screen-saved-at"]');
    expect(savedEl?.textContent?.trim()).toMatch(/^Saved .+ before suspend$/);
    const tooltip = savedEl?.closest('sl-tooltip');
    expect(tooltip?.getAttribute('content')).toBeTruthy();

    expect(el.shadowRoot?.querySelector('[data-testid="suspend-screen-truncated"]')).toBeNull();

    // Disposes xterm on disconnect.
    el.remove();
    expect(term.dispose).toHaveBeenCalledTimes(1);
  });

  it('shows truncated badge when truncated is true', async () => {
    apiFetch.mockResolvedValue(jsonResponse(200, makeScreenPayload({ truncated: true })));

    const el = mountScreen({});
    await el.updateComplete;
    await flushAsync();
    await el.updateComplete;

    const badge = el.shadowRoot?.querySelector('[data-testid="suspend-screen-truncated"]');
    expect(badge?.textContent?.trim()).toBe('truncated');
  });

  it('shows muted empty message on 404 (no saved screen or experiment off)', async () => {
    apiFetch.mockResolvedValue(
      jsonResponse(404, { error: { code: 'not_found', message: 'No saved screen for this agent' } })
    );

    const el = mountScreen({});
    await el.updateComplete;
    await flushAsync();
    await el.updateComplete;

    expect(xtermMock.instances).toHaveLength(0);
    const empty = el.shadowRoot?.querySelector('[data-testid="suspend-screen-empty"]');
    expect(empty?.textContent?.trim()).toBe(NO_SAVED_SCREEN_MESSAGE);
  });

  it('shows muted empty message and logs warning on 403 or server error without throwing', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    apiFetch.mockResolvedValue(jsonResponse(403, { error: { code: 'forbidden' } }));

    const el = mountScreen({});
    await el.updateComplete;
    await flushAsync();
    await el.updateComplete;

    expect(xtermMock.instances).toHaveLength(0);
    expect(warnSpy).toHaveBeenCalled();
    const empty = el.shadowRoot?.querySelector('[data-testid="suspend-screen-empty"]');
    expect(empty?.textContent?.trim()).toBe(NO_SAVED_SCREEN_MESSAGE);
  });

  it('discards an in-flight screen fetch if the phase leaves suspended', async () => {
    let resolveFetch!: (res: Response) => void;
    let capturedSignal: AbortSignal | undefined;
    apiFetch.mockImplementation((_url: string, init?: RequestInit) => {
      capturedSignal = init?.signal ?? undefined;
      return new Promise<Response>((resolve) => {
        resolveFetch = resolve;
      });
    });

    const el = mountScreen({ phase: 'suspended' });
    await el.updateComplete;
    expect(screenCalls()).toHaveLength(1);

    // Phase changes to running before the fetch settles.
    el.phase = 'running';
    await el.updateComplete;
    expect(capturedSignal?.aborted).toBe(true);

    resolveFetch(jsonResponse(200, makeScreenPayload()));
    await flushAsync();
    await el.updateComplete;

    expect(xtermMock.instances).toHaveLength(0);
    expect(el.shadowRoot?.querySelector('[data-testid="agent-last-screen-card"]')).toBeNull();
  });
});

describe('agent-detail Last screen card visibility and SSE transitions', () => {
  const mounted: HTMLElement[] = [];

  beforeEach(() => {
    apiFetch.mockReset();
    xtermMock.instances.length = 0;
    fakeStateManager.reset();
  });

  afterEach(() => {
    for (const el of mounted.splice(0)) {
      el.remove();
    }
  });

  function mountDetail(agent: Agent): ScionPageAgentDetail {
    apiFetch.mockImplementation((url: string) => {
      if (url === SCREEN_URL) {
        return Promise.resolve(jsonResponse(200, makeScreenPayload()));
      }
      if (url === `/api/v1/agents/${agent.id}`) {
        return Promise.resolve(jsonResponse(200, agent));
      }
      return Promise.resolve(jsonResponse(404, {}));
    });

    const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
    el.agentId = agent.id;
    document.body.appendChild(el);
    mounted.push(el);
    return el;
  }

  it('renders the Last screen card and fetches /screen for a suspended agent with attach', async () => {
    const el = mountDetail(
      makeAgent({ phase: 'suspended', _capabilities: { actions: ['read', 'attach'] } })
    );
    await flushAsync(10);
    await el.updateComplete;
    await flushAsync(6);

    const card = el.shadowRoot?.querySelector('scion-agent-suspend-screen');
    expect(card).not.toBeNull();
    expect(screenCalls()).toHaveLength(1);
    expect(screenCalls()[0][0]).toBe(SCREEN_URL);
  });

  it('hides the card and makes no /screen request when the user lacks attach', async () => {
    const el = mountDetail(makeAgent({ phase: 'suspended', _capabilities: { actions: ['read'] } }));
    await flushAsync(10);
    await el.updateComplete;
    await flushAsync(6);

    expect(el.shadowRoot?.querySelector('scion-agent-suspend-screen')).toBeNull();
    expect(screenCalls()).toHaveLength(0);
  });

  it('hides the card and makes no /screen request when the agent is running', async () => {
    const el = mountDetail(
      makeAgent({ phase: 'running', _capabilities: { actions: ['read', 'attach'] } })
    );
    await flushAsync(10);
    await el.updateComplete;
    await flushAsync(6);

    expect(el.shadowRoot?.querySelector('scion-agent-suspend-screen')).toBeNull();
    expect(screenCalls()).toHaveLength(0);
  });

  it('mounts the card and fetches /screen when an SSE update suspends a running agent', async () => {
    const running = makeAgent({ phase: 'running', _capabilities: { actions: ['read', 'attach'] } });
    const el = mountDetail(running);
    await flushAsync(10);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-agent-suspend-screen')).toBeNull();
    expect(screenCalls()).toHaveLength(0);

    // Simulate SSE agents-changed transitioning the agent to suspended.
    fakeStateManager.seedAgents([{ ...running, phase: 'suspended', suspendReason: 'idle' }]);
    fakeStateManager.dispatchEvent(
      new CustomEvent('agents-changed', {
        detail: { data: { upserted: [AGENT_ID] } },
      })
    );

    await el.updateComplete;
    await flushAsync(10);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-agent-suspend-screen')).not.toBeNull();
    expect(screenCalls()).toHaveLength(1);
    expect(screenCalls()[0][0]).toBe(SCREEN_URL);
  });
});
