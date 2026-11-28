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
 * (ptone/scion#3707, ptone/scion#9845). The hub authorizes a user's
 * reincarnate with the `agent.lifecycle` permission
 * (authorizeAgentReincarnate), so the card follows the `lifecycle` capability
 * like the header's lifecycle actions. Clicking Reincarnate opens
 * `<scion-reincarnate-dialog>` offering "Reincarnate now" or
 * "Ask the agent to prepare".
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
  extractApiError: async (res: Response, fallback: string): Promise<string> => {
    try {
      const body = (await res.json()) as { error?: { message?: string } };
      return body?.error?.message || fallback;
    } catch {
      return fallback;
    }
  },
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

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

await import('./agent-detail.js');
import {
  REINCARNATE_CONFIRM_MESSAGE,
  REINCARNATE_PREPARE_MESSAGE,
} from '../../shared/agent-reincarnate.js';
import type { ScionReincarnateDialog } from '../shared/reincarnate-dialog.js';
type ScionPageAgentDetail = import('./agent-detail.js').ScionPageAgentDetail;
type Agent = import('../../shared/types.js').Agent;

const AGENT_ID = 'agent-1';
const PROJECT_ID = 'proj-1';
const REINCARNATE_URL = `/api/v1/agents/${AGENT_ID}/reincarnate`;
const MESSAGE_URL = `/api/v1/agents/${AGENT_ID}/message`;

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: AGENT_ID,
    name: 'Test Agent',
    projectId: PROJECT_ID,
    template: 'default',
    phase: 'running',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle', 'message'] },
    ...overrides,
  };
}

function jsonResponse(status: number, body: unknown): Response {
  const res = {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
    clone: () => res,
  };
  return res as unknown as Response;
}

let reincarnateResponder: () => Promise<Response>;
let messageResponder: () => Promise<Response>;

async function mount(agent: Agent): Promise<ScionPageAgentDetail> {
  apiFetch.mockReset();
  apiFetch.mockImplementation((url: string) => {
    if (url.endsWith('/reincarnate')) return reincarnateResponder();
    if (url.endsWith('/message')) return messageResponder();
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

function reincarnateDialog(el: ScionPageAgentDetail): ScionReincarnateDialog {
  const dialog = el.shadowRoot!.querySelector<ScionReincarnateDialog>('scion-reincarnate-dialog');
  expect(dialog).toBeTruthy();
  return dialog!;
}

function reincarnateCalls(): unknown[][] {
  return apiFetch.mock.calls.filter(([url]) => String(url).endsWith('/reincarnate'));
}

function messageCalls(): unknown[][] {
  return apiFetch.mock.calls.filter(([url]) => String(url).endsWith('/message'));
}

async function openAndConfirm(
  el: ScionPageAgentDetail,
  mode: 'now' | 'prepare' = 'now'
): Promise<void> {
  reincarnateButton(el).click();
  await el.updateComplete;
  const dialog = reincarnateDialog(el);
  await dialog.updateComplete;
  expect(dialog.open).toBe(true);
  if (mode === 'prepare') {
    dialog.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-prepare"]')!.click();
    await dialog.updateComplete;
  }
  dialog.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-confirm"]')!.click();
  await el.updateComplete;
}

describe('agent detail Reincarnate action (ptone/scion#3707, ptone/scion#9845)', () => {
  beforeEach(() => {
    fakeStateManager.reset();
    reincarnateResponder = (): Promise<Response> => Promise.resolve(jsonResponse(202, {}));
    messageResponder = (): Promise<Response> => Promise.resolve(jsonResponse(200, {}));
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
    expect(el.shadowRoot!.querySelector('.header-actions')?.textContent).toContain('Stop');
  });

  it('opens the dialog, and Reincarnate now POSTs to /reincarnate and refreshes', async () => {
    const el = await mount(makeAgent());
    await openAndConfirm(el, 'now');

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    expect(messageCalls()).toHaveLength(0);
    expect(REINCARNATE_CONFIRM_MESSAGE).toMatch(
      /stopped and re-provisioned with its current settings and template/
    );

    const [url, init] = reincarnateCalls()[0] as [string, RequestInit];
    expect(url).toBe(REINCARNATE_URL);
    expect(url).not.toContain(`/projects/${PROJECT_ID}/`);
    expect(init.method).toBe('POST');
    expect(init.body).toBe('{}');

    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some(([u]) => u === `/api/v1/agents/${AGENT_ID}`)).toBe(true)
    );
    await el.updateComplete;
    expect(reincarnateCard(el)!.querySelector('sl-alert')).toBeNull();
  });

  it('makes no call when Cancel is clicked in the dialog', async () => {
    const el = await mount(makeAgent());
    reincarnateButton(el).click();
    await el.updateComplete;
    const dialog = reincarnateDialog(el);
    await dialog.updateComplete;
    expect(dialog.open).toBe(true);

    dialog.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-cancel"]')!.click();
    await el.updateComplete;
    expect(dialog.open).toBe(false);
    expect(reincarnateCalls()).toHaveLength(0);
    expect(messageCalls()).toHaveLength(0);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);
  });

  it.each([
    [403, 'forbidden', 'You do not have permission to reincarnate this agent'],
    [409, 'agent_launching', 'Agent is still launching; try again once it has started'],
  ])('shows the hub error message on %i for Reincarnate now', async (status, code, message) => {
    reincarnateResponder = (): Promise<Response> =>
      Promise.resolve(jsonResponse(status, { error: { code, message } }));
    const el = await mount(makeAgent());
    await openAndConfirm(el, 'now');

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    await vi.waitFor(() => {
      const alert = reincarnateCard(el)?.querySelector('sl-alert');
      expect(alert?.textContent).toContain(message);
    });
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(false);
  });

  it('shows progress and ignores further clicks while Reincarnate now is in flight', async () => {
    let resolve!: (r: Response) => void;
    reincarnateResponder = (): Promise<Response> =>
      new Promise<Response>((r) => {
        resolve = r;
      });
    const el = await mount(makeAgent());
    await openAndConfirm(el, 'now');

    await vi.waitFor(() => expect(reincarnateCalls()).toHaveLength(1));
    await el.updateComplete;
    expect(reincarnateButton(el).hasAttribute('loading')).toBe(true);
    expect(reincarnateButton(el).hasAttribute('disabled')).toBe(true);

    reincarnateButton(el).click();
    await el.updateComplete;
    expect(reincarnateDialog(el).open).toBe(false);
    expect(reincarnateCalls()).toHaveLength(1);

    resolve(jsonResponse(202, {}));
    await vi.waitFor(() => expect(reincarnateButton(el).hasAttribute('loading')).toBe(false));
  });

  it('Ask the agent to prepare sends the prepare message with wake: true and shows confirmation', async () => {
    const el = await mount(makeAgent({ phase: 'suspended', suspendReason: 'user' }));
    await openAndConfirm(el, 'prepare');

    await vi.waitFor(() => expect(messageCalls()).toHaveLength(1));
    expect(reincarnateCalls()).toHaveLength(0);

    const [url, init] = messageCalls()[0] as [string, RequestInit];
    expect(url).toBe(MESSAGE_URL);
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body as string)).toEqual({
      structured_message: { msg: REINCARNATE_PREPARE_MESSAGE, plain: false },
      interrupt: false,
      wake: true,
    });

    await vi.waitFor(() => {
      const sent = reincarnateCard(el)?.querySelector('[data-testid="reincarnate-prepare-sent"]');
      expect(sent).not.toBeNull();
      expect(sent?.textContent).toContain('Asked the agent to prepare');
    });
  });

  it('surfaces message_denied errors from Ask the agent to prepare in the card', async () => {
    messageResponder = (): Promise<Response> =>
      Promise.resolve(
        jsonResponse(403, {
          error: { code: 'message_denied', details: { reason: 'missing_permission' } },
        })
      );
    const el = await mount(makeAgent());
    await openAndConfirm(el, 'prepare');

    await vi.waitFor(() => expect(messageCalls()).toHaveLength(1));
    await vi.waitFor(() => {
      const alert = reincarnateCard(el)?.querySelector('.reincarnate-error');
      expect(alert?.textContent).toContain('permission');
    });
    expect(
      reincarnateCard(el)?.querySelector('[data-testid="reincarnate-prepare-sent"]')
    ).toBeNull();
  });

  it('disables Ask the agent to prepare in the dialog for stopped or shell agents', async () => {
    const el = await mount(makeAgent({ phase: 'stopped' }));
    reincarnateButton(el).click();
    await el.updateComplete;
    const dialog = reincarnateDialog(el);
    await dialog.updateComplete;
    expect(dialog.prepareAvailable).toBe(false);
    expect(dialog.prepareUnavailableReason).toContain('must be running');
  });
});
