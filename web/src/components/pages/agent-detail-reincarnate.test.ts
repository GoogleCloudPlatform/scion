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
 * Reincarnate action in the agent detail Configuration tab
 * (ptone/scion#3707). The hub authorizes a user's reincarnate with the
 * `agent.lifecycle` permission (authorizeAgentReincarnate), so the button
 * follows the `lifecycle` capability like the header's lifecycle actions.
 */

// @vitest-environment happy-dom

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
  setScope(): void {}
  getCurrentUserId(): string {
    return '';
  }
  seedAgents(agents: Array<{ id: string }>): void {
    for (const a of agents) this.agentsById.set(a.id, a);
  }
  seedProjects(): void {}
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
import { showConfirm } from '../shared/confirm-dialog.js';
import { REINCARNATE_CONFIRM_MESSAGE } from './agent-detail.js';
type ScionPageAgentDetail = import('./agent-detail.js').ScionPageAgentDetail;
type Agent = import('../../shared/types.js').Agent;

const AGENT_ID = 'agent-1';
const PROJECT_ID = 'proj-1';
const REINCARNATE_URL = `/api/v1/agents/${AGENT_ID}/reincarnate`;

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: AGENT_ID,
    name: 'Test Agent',
    projectId: PROJECT_ID,
    template: 'default',
    phase: 'running',
    _capabilities: { actions: ['read', 'lifecycle'] },
    ...overrides,
  } as Agent;
}

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

/** Answers for POST .../reincarnate; every other call degrades with a 404. */
let reincarnateResponder: () => Promise<Response>;

async function mount(agent: Agent): Promise<ScionPageAgentDetail> {
  apiFetch.mockReset();
  apiFetch.mockImplementation((url: string) => {
    if (url.endsWith('/reincarnate')) return reincarnateResponder();
    return Promise.resolve(jsonResponse(404, {}));
  });

  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail & {
    pageData: unknown;
    agentId: string;
  };
  el.agentId = AGENT_ID;
  el.pageData = {
    path: `/agents/${AGENT_ID}`,
    title: 'Agent',
    data: agent as unknown as Record<string, unknown>,
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => {
    expect((el as unknown as { loading: boolean }).loading).toBe(false);
  });
  await el.updateComplete;
  return el;
}

function reincarnateCard(el: ScionPageAgentDetail): HTMLElement | null {
  return el.shadowRoot!.querySelector<HTMLElement>(
    'sl-tab-panel[name="configuration"] .reincarnate-card'
  );
}

function reincarnateButton(el: ScionPageAgentDetail): HTMLElement {
  const button = reincarnateCard(el)?.querySelector<HTMLElement>('sl-button');
  expect(button).toBeTruthy();
  return button!;
}

function reincarnateCalls(): unknown[][] {
  return apiFetch.mock.calls.filter(([url]) => String(url).endsWith('/reincarnate'));
}

/**
 * Lets the ended attempt settle, then clicks again: a second confirm proves
 * the in-flight guard was released and the action can be started again.
 */
async function expectCanStartAgain(el: ScionPageAgentDetail): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  expect(showConfirm).toHaveBeenCalledTimes(1);
  reincarnateButton(el).click();
  await vi.waitFor(() => expect(showConfirm).toHaveBeenCalledTimes(2));
}

describe('agent detail Reincarnate action (ptone/scion#3707)', () => {
  beforeEach(() => {
    fakeStateManager.reset();
    vi.mocked(showConfirm).mockReset();
    vi.mocked(showConfirm).mockResolvedValue(true);
    reincarnateResponder = (): Promise<Response> => Promise.resolve(jsonResponse(202, {}));
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('hides the button without the lifecycle capability', async () => {
    const el = await mount(makeAgent({ _capabilities: { actions: ['read', 'attach'] } }));
    expect(reincarnateCard(el)).toBeNull();
  });

  it('hides the card while the agent is being deleted, like the header actions', async () => {
    const now = Date.now();
    const el = await mount(
      makeAgent({
        deletion: {
          state: 'deleting',
          soft: false,
          claim: 1,
          startedAt: new Date(now).toISOString(),
          leaseExpiresAt: new Date(now + 60_000).toISOString(),
        },
      })
    );
    expect(reincarnateCard(el)).toBeNull();
    // The header hides its lifecycle actions on the same condition.
    const headerActions = el.shadowRoot!.querySelector('.header-actions');
    expect(headerActions?.textContent ?? '').not.toContain('Stop');
  });

  it('hides the button when capabilities are missing (fail closed)', async () => {
    const agent: Partial<Agent> = makeAgent();
    delete agent._capabilities;
    const el = await mount(agent as Agent);
    expect(reincarnateCard(el)).toBeNull();
  });

  it('shows an enabled button in the Configuration tab with the lifecycle capability', async () => {
    const el = await mount(makeAgent());
    const button = reincarnateButton(el);
    expect(button.textContent).toContain('Reincarnate');
    expect(button.hasAttribute('disabled')).toBe(false);
    // Control for the delete test: the header shows Stop when not deleting.
    expect(el.shadowRoot!.querySelector('.header-actions')?.textContent).toContain('Stop');
  });

  it('confirms, then POSTs to the agent-scoped reincarnate route and refreshes', async () => {
    const el = await mount(makeAgent());
    reincarnateButton(el).click();

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showConfirm).mock.calls[0][0]).toBe(REINCARNATE_CONFIRM_MESSAGE);
    expect(REINCARNATE_CONFIRM_MESSAGE).toMatch(
      /stopped and re-provisioned with its current settings and template/
    );

    const [url, init] = reincarnateCalls()[0] as [string, RequestInit];
    // The page addresses every lifecycle action by agent ID; the
    // project-scoped form is not used here.
    expect(url).toBe(REINCARNATE_URL);
    expect(url).not.toContain(`/projects/${PROJECT_ID}/`);
    expect(init.method).toBe('POST');
    // The hub decodes a JSON body; an empty body would be a 400.
    expect(init.body).toBe('{}');

    // Success refreshes the agent the same way the other actions do.
    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some(([u]) => u === `/api/v1/agents/${AGENT_ID}`)).toBe(true)
    );
    await el.updateComplete;
    expect(reincarnateCard(el)!.querySelector('sl-alert')).toBeNull();
  });

  it('makes no call when the confirm is cancelled', async () => {
    vi.mocked(showConfirm).mockResolvedValue(false);
    const el = await mount(makeAgent());
    reincarnateButton(el).click();

    await vi.waitFor(() => expect(showConfirm).toHaveBeenCalledTimes(1));
    await el.updateComplete;
    expect(reincarnateCalls()).toHaveLength(0);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);

    await expectCanStartAgain(el);
    expect(reincarnateCalls()).toHaveLength(0);
  });

  it('can be started again after the confirm dialog rejects', async () => {
    vi.mocked(showConfirm).mockRejectedValueOnce(new Error('dialog failed'));
    const el = await mount(makeAgent());
    reincarnateButton(el).click();

    await vi.waitFor(() => {
      const alert = reincarnateCard(el)?.querySelector('sl-alert');
      expect(alert?.textContent).toContain('dialog failed');
    });
    expect(reincarnateCalls()).toHaveLength(0);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);

    await expectCanStartAgain(el);
    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
  });

  it.each([
    [403, 'forbidden', 'You do not have permission to reincarnate this agent'],
    [409, 'agent_launching', 'Agent is still launching; try again once it has started'],
  ])('shows the hub error message on %i', async (status, code, message) => {
    reincarnateResponder = (): Promise<Response> =>
      Promise.resolve(jsonResponse(status, { error: { code, message } }));
    const el = await mount(makeAgent());
    reincarnateButton(el).click();

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    await vi.waitFor(() => {
      const alert = reincarnateCard(el)?.querySelector('sl-alert');
      expect(alert?.textContent).toContain(message);
    });
    // The button is usable again after the failure.
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);

    await expectCanStartAgain(el);
    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(2));
  });

  it('shows progress and ignores further clicks while the request runs', async () => {
    let resolve!: (r: Response) => void;
    reincarnateResponder = (): Promise<Response> =>
      new Promise<Response>((r) => {
        resolve = r;
      });
    const el = await mount(makeAgent());
    const button = reincarnateButton(el);
    button.click();

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    await el.updateComplete;
    expect(reincarnateButton(el).hasAttribute('loading')).toBe(true);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(true);

    // A disabled sl-button blocks clicks natively; dispatching on the element
    // bypasses that, so this also proves the handler's own guard.
    reincarnateButton(el).click();
    reincarnateButton(el).click();
    await el.updateComplete;
    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(reincarnateCalls()).toHaveLength(1);

    resolve(jsonResponse(202, {}));
    await vi.waitFor(() => expect(reincarnateButton(el).hasAttribute('loading')).toBe(false));

    // After the 202 the action can be started again.
    await expectCanStartAgain(el);
    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(2));
  });

  it('ignores a second click while the confirm dialog is open', async () => {
    let answer!: (v: boolean) => void;
    vi.mocked(showConfirm).mockImplementation(
      () =>
        new Promise<boolean>((r) => {
          answer = r;
        })
    );
    const el = await mount(makeAgent());
    reincarnateButton(el).click();
    reincarnateButton(el).click();
    await el.updateComplete;
    expect(showConfirm).toHaveBeenCalledTimes(1);
    // No spinner behind the open dialog: progress shows only after confirm.
    expect(reincarnateButton(el).hasAttribute('loading')).toBe(false);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);

    answer(true);
    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
  });
});
