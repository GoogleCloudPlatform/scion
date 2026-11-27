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
 * Never-suspend Web UI tests (ptone/scion#9184, lifecycle.never_suspend):
 * - Create form checkbox -> suspendPolicy: "never"
 * - Edit form toggle -> PATCH suspendPolicy, suspensionInFlight hint,
 *   non-resumable harness warnings, and graceful 403 handling
 * - Agent detail badge, Suspend policy row, force-suspend confirm dialog,
 *   and hiding Suspend for callers who cannot force
 * - Hub-wide agents view Policy filter, Suspend Policy column, badge, and
 *   force-suspend confirm dialog
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, PageData } from '../../shared/types.js';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';
import type { ScionPageAgentCreate } from './agent-create.js';
import type { ScionPageAgentEdit } from './agent-edit.js';
import type { ScionPageAgentDetail } from './agent-detail.js';
import type { ScionPageAgents } from './agents.js';
import type { ScionPageProjectDetail } from './project-detail.js';
import {
  NEVER_SUSPEND_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

const toasts: string[] = [];
vi.mock('../../utils/toast.js', () => ({
  showToast: (message: string): void => {
    toasts.push(message);
  },
}));

const confirmMock = vi.fn<() => Promise<boolean>>();
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: (...args: unknown[]): Promise<boolean> => confirmMock(...(args as [])),
}));

vi.mock('../../client/navigation.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/navigation.js')>();
  return {
    ...actual,
    navigateTo: vi.fn(),
    replaceSearch: vi.fn(),
  };
});

class FakeEventSource {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  readyState = 1;
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {
    this.readyState = 2;
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'lead-agent',
    slug: 'lead-agent',
    projectId: 'proj-1',
    project: 'Alpha',
    template: 'default',
    phase: 'running',
    activity: 'working',
    suspendPolicy: 'auto',
    stateVersion: 3,
    _capabilities: { actions: ['read', 'update', 'lifecycle', 'delete', 'attach'] },
    editability: {
      phase: 'running',
      fields: {
        name: { tier: 'T0', disposition: 'immediate' },
        suspendPolicy: { tier: 'T0', disposition: 'immediate' },
      },
    },
    ...overrides,
  };
}

function requestUrl(input: RequestInfo | URL): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.pathname + input.search;
  return input.url;
}

function bodyString(init?: RequestInit): string {
  return typeof init?.body === 'string' ? init.body : '{}';
}

describe('Never-suspend create form (scion-page-agent-create)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let postedBodies: Array<{ url: string; body: Record<string, unknown> }>;

  beforeAll(async () => {
    await import('./agent-create.js');
  });

  beforeEach(() => {
    toasts.length = 0;
    postedBodies = [];
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/agents/new');

    fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = requestUrl(input);
      const method = (init?.method ?? 'GET').toUpperCase();
      if (url.includes('/api/v1/projects') && method === 'GET') {
        if (url.includes('/providers')) return Promise.resolve(jsonResponse({ providers: [] }));
        if (url.includes('/settings')) return Promise.resolve(jsonResponse({}));
        if (url.includes('/gcp-service-accounts'))
          return Promise.resolve(jsonResponse({ accounts: [] }));
        return Promise.resolve(
          jsonResponse({
            projects: [{ id: 'proj-1', name: 'Alpha', slug: 'alpha', status: 'active' }],
          })
        );
      }
      if (url.includes('/api/v1/templates'))
        return Promise.resolve(jsonResponse({ templates: [] }));
      if (url.includes('/api/v1/harness-configs'))
        return Promise.resolve(jsonResponse({ harnessConfigs: [] }));
      if (url.includes('/api/v1/runtime-brokers'))
        return Promise.resolve(jsonResponse({ brokers: [] }));
      if (url.includes('/api/v1/settings/public')) return Promise.resolve(jsonResponse({}));
      if (url.includes('/api/v1/agents') && method === 'POST') {
        const body = JSON.parse(bodyString(init)) as Record<string, unknown>;
        postedBodies.push({ url, body });
        return Promise.resolve(jsonResponse({ agent: { id: 'agent-new', phase: 'running' } }, 201));
      }
      return Promise.resolve(jsonResponse({}));
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountCreate(): Promise<ScionPageAgentCreate> {
    const el = document.createElement('scion-page-agent-create') as ScionPageAgentCreate;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('#name')).not.toBeNull();
    });
    return el;
  }

  it('hides the Never suspend checkbox when experiment is off', async () => {
    const el = await mountCreate();
    expect(el.shadowRoot?.querySelector('[data-testid="never-suspend-checkbox"]')).toBeNull();
  });

  it('shows the checkbox when experiment is on and sends suspendPolicy="never" when checked', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    const el = await mountCreate();
    const checkbox = el.shadowRoot?.querySelector('[data-testid="never-suspend-checkbox"]') as
      | (HTMLElement & { checked: boolean })
      | null;
    expect(checkbox).not.toBeNull();

    // Fill name and check Never suspend
    const nameInput = el.shadowRoot!.querySelector('#name') as HTMLElement & { value: string };
    nameInput.value = 'standing-lead';
    nameInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true, composed: true }));

    checkbox!.checked = true;
    checkbox!.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await el.updateComplete;

    const startBtn = Array.from(el.shadowRoot!.querySelectorAll('.form-actions sl-button')).find(
      (b) => b.textContent?.includes('Start')
    );
    expect(startBtn).toBeDefined();
    startBtn!.click();

    await vi.waitFor(() => {
      expect(postedBodies.some((p) => p.url.endsWith('/api/v1/agents'))).toBe(true);
    });
    const createPost = postedBodies.find((p) => p.url.endsWith('/api/v1/agents'))!;
    expect(createPost.body.suspendPolicy).toBe('never');
  });
});

describe('Never-suspend edit form (scion-page-agent-edit)', () => {
  let agentData: Agent;
  let patchResponse: { status: number; body: unknown };
  let patchBodies: Array<Record<string, unknown>>;

  beforeAll(async () => {
    await import('./agent-edit.js');
  });

  beforeEach(() => {
    toasts.length = 0;
    patchBodies = [];
    agentData = makeAgent({ suspendPolicy: 'auto' });
    patchResponse = {
      status: 200,
      body: {
        ...agentData,
        suspendPolicy: 'never',
        disposition: { applied: ['suspendPolicy'] },
      },
    };
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/agents/agent-1/edit');

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        if (url.includes('/api/v1/agents/agent-1') && method === 'GET') {
          return Promise.resolve(jsonResponse(agentData));
        }
        if (url.includes('/api/v1/agents/agent-1') && method === 'PATCH') {
          patchBodies.push(JSON.parse(bodyString(init)) as Record<string, unknown>);
          return Promise.resolve(jsonResponse(patchResponse.body, patchResponse.status));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountEdit(): Promise<{ el: ScionPageAgentEdit; form: ScionAgentConfigForm }> {
    const el = document.createElement('scion-page-agent-edit') as ScionPageAgentEdit;
    document.body.appendChild(el);
    let form: ScionAgentConfigForm | null = null;
    await vi.waitFor(() => {
      form = el.shadowRoot?.querySelector('scion-agent-config-form') as ScionAgentConfigForm | null;
      expect(form).not.toBeNull();
    });
    return { el, form: form! };
  }

  it('hides the suspendPolicy field when experiment is off', async () => {
    const { form } = await mountEdit();
    expect(form.shadowRoot?.querySelector('[data-testid="suspend-policy-toggle"]')).toBeNull();
  });

  it('toggles suspendPolicy via PATCH and shows suspensionInFlight hint and non-resumable harness warning', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    patchResponse = {
      status: 200,
      body: {
        ...agentData,
        suspendPolicy: 'never',
        suspensionInFlight: true,
        warnings: [
          'harness "generic" does not support resume; this agent is never auto-suspended so suspendPolicy="never" has no effect',
        ],
        disposition: { applied: ['suspendPolicy'] },
      },
    };
    const { el, form } = await mountEdit();
    const toggle = form.shadowRoot?.querySelector('[data-testid="suspend-policy-toggle"]') as
      | (HTMLElement & { checked: boolean })
      | null;
    expect(toggle).not.toBeNull();
    expect(toggle!.hasAttribute('checked')).toBe(false);

    toggle!.checked = true;
    toggle!.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await form.updateComplete;
    await el.updateComplete;

    const saveBtn = el.shadowRoot!.querySelector('[data-testid="save"]') as HTMLElement;
    saveBtn.click();

    await vi.waitFor(() => {
      expect(patchBodies.length).toBe(1);
    });
    expect(patchBodies[0].suspendPolicy).toBe('never');

    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-testid="suspension-in-flight"]')).not.toBeNull();
      const warningsEl = el.shadowRoot?.querySelector('[data-testid="warnings"]');
      expect(warningsEl?.textContent).toContain('does not support resume');
    });
  });

  it('handles 403 on PATCH gracefully', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    patchResponse = {
      status: 403,
      body: { error: { code: 'forbidden', message: 'agents cannot PATCH suspendPolicy' } },
    };
    const { el, form } = await mountEdit();
    const toggle = form.shadowRoot!.querySelector(
      '[data-testid="suspend-policy-toggle"]'
    ) as HTMLElement & { checked: boolean };
    toggle.checked = true;
    toggle.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await form.updateComplete;
    await el.updateComplete;

    (el.shadowRoot!.querySelector('[data-testid="save"]') as HTMLElement).click();

    await vi.waitFor(() => {
      const errEl = el.shadowRoot?.querySelector('[data-testid="error"]');
      expect(errEl?.textContent).toContain('agents cannot PATCH suspendPolicy');
    });
  });
});

describe('Never-suspend agent detail (scion-page-agent-detail)', () => {
  let agentData: Agent;
  let suspendCalls: Array<{ url: string; body: string | null }>;

  beforeAll(async () => {
    await import('./agent-detail.js');
  });

  beforeEach(() => {
    toasts.length = 0;
    suspendCalls = [];
    confirmMock.mockReset();
    confirmMock.mockResolvedValue(true);
    agentData = makeAgent({ suspendPolicy: 'never', phase: 'running' });
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/agents/agent-1');
    vi.stubGlobal('EventSource', FakeEventSource);

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        if (url.endsWith('/api/v1/agents/agent-1') && method === 'GET') {
          return Promise.resolve(jsonResponse(agentData));
        }
        if (url.endsWith('/api/v1/agents/agent-1/suspend') && method === 'POST') {
          suspendCalls.push({
            url,
            body: typeof init?.body === 'string' ? init.body : null,
          });
          return Promise.resolve(jsonResponse({ ...agentData, phase: 'suspended' }));
        }
        if (url.includes('/metrics')) {
          return Promise.resolve(jsonResponse({ error: { message: 'not found' } }, 404));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountDetail(pageData?: PageData): Promise<ScionPageAgentDetail> {
    const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
    if (pageData) el.pageData = pageData;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('scion-detail-header')).not.toBeNull();
    });
    return el;
  }

  it('hides badge and policy row when experiment is off', async () => {
    const el = await mountDetail();
    expect(el.shadowRoot?.querySelector('[data-testid="never-suspend-badge"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-testid="agent-suspend-policy"]')).toBeNull();
  });

  it('shows badge and policy row, confirms force-suspend, and sends force=true when confirmed', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    const el = await mountDetail({
      path: '/agents/agent-1',
      title: 'Agent',
      user: { id: 'u1', email: 'alice@example.com', name: 'Alice', role: 'admin' },
    });

    expect(el.shadowRoot?.querySelector('[data-testid="never-suspend-badge"]')).not.toBeNull();
    const policyRow = el.shadowRoot?.querySelector('[data-testid="agent-suspend-policy"]');
    expect(policyRow?.textContent).toContain('never');

    const suspendBtn = el.shadowRoot?.querySelector(
      '[data-testid="header-suspend"]'
    ) as HTMLElement;
    expect(suspendBtn).not.toBeNull();
    suspendBtn.click();

    await vi.waitFor(() => {
      expect(confirmMock).toHaveBeenCalledTimes(1);
      expect(suspendCalls.length).toBe(1);
    });
    expect(JSON.parse(suspendCalls[0].body!)).toEqual({ force: true });
  });

  it('does not suspend when force-suspend confirm dialog is cancelled', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    confirmMock.mockResolvedValue(false);
    const el = await mountDetail();
    (el.shadowRoot!.querySelector('[data-testid="header-suspend"]') as HTMLElement).click();

    await vi.waitFor(() => {
      expect(confirmMock).toHaveBeenCalledTimes(1);
    });
    expect(suspendCalls.length).toBe(0);
  });

  it('hides Suspend button on a flagged agent when _capabilities lacks lifecycle', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    agentData = makeAgent({
      suspendPolicy: 'never',
      phase: 'running',
      _capabilities: { actions: ['read', 'update'] },
    });
    const el = await mountDetail({
      path: '/agents/agent-1',
      title: 'Agent',
      user: { id: 'u2', email: 'viewer@example.com', name: 'Viewer', role: 'viewer' },
    });
    expect(el.shadowRoot?.querySelector('[data-testid="header-suspend"]')).toBeNull();
  });
});

describe('Never-suspend hub-wide agents view (scion-page-agents)', () => {
  let agentsList: Agent[];

  beforeAll(async () => {
    await import('./agents.js');
  });

  beforeEach(() => {
    toasts.length = 0;
    confirmMock.mockReset();
    confirmMock.mockResolvedValue(true);
    localStorage.setItem('scion-view-agents', 'list');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/agents');
    vi.stubGlobal('EventSource', FakeEventSource);

    agentsList = [
      makeAgent({ id: 'a1', name: 'pinned-lead', suspendPolicy: 'never' }),
      makeAgent({ id: 'a2', name: 'regular-dev', suspendPolicy: 'auto' }),
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const url = requestUrl(input);
        if (url.includes('/api/v1/agents')) {
          return Promise.resolve(
            jsonResponse({
              agents: agentsList,
              totalCount: agentsList.length,
              stats: { total: agentsList.length, running: 2 },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    localStorage.removeItem('scion-view-agents');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountAgents(): Promise<ScionPageAgents> {
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'admin@example.com', name: 'Admin', role: 'admin' },
    };
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('table')).not.toBeNull();
    });
    return el;
  }

  it('hides Policy filter and Never suspend pill in Traits when experiment is off, while keeping Traits column', async () => {
    const el = await mountAgents();
    expect(el.shadowRoot?.querySelector('[data-testid="suspend-policy-filter"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-testid="traits-col-header"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('[data-testid="never-suspend-badge"]')).toBeNull();
    const cells = Array.from(el.shadowRoot!.querySelectorAll('[data-testid="traits-cell"]'));
    expect(cells.length).toBe(2);
    expect(cells[0].textContent?.trim()).toBe('');
    expect(cells[1].textContent?.trim()).toBe('');
  });

  it('shows Policy filter and Never suspend pill in Traits when experiment is on, leaving default agent empty', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    const el = await mountAgents();

    expect(el.shadowRoot?.querySelector('[data-testid="suspend-policy-filter"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('[data-testid="traits-col-header"]')).not.toBeNull();

    const cells = Array.from(el.shadowRoot!.querySelectorAll('[data-testid="traits-cell"]'));
    expect(cells.length).toBe(2);
    const badge = cells[0].querySelector('[data-testid="never-suspend-badge"]');
    expect(badge).not.toBeNull();
    expect(badge?.getAttribute('aria-label')).toContain('Never suspend');
    expect(cells[1].textContent?.trim()).toBe('');
    expect(cells[1].children.length).toBe(0);
  });

  it('renders only non-default traits (mode pill, never-suspend pill, reachability exception arrow) with aria-labels', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    agentsList = [
      makeAgent({
        id: 'a-default',
        name: 'default-worker',
        messageMode: 'project',
        suspendPolicy: 'auto',
        _messageability: { canMessage: true, canReachViewer: true },
      }),
      makeAgent({
        id: 'a-sealed',
        name: 'sealed-worker',
        messageMode: 'none',
        suspendPolicy: 'auto',
        _messageability: {
          canMessage: false,
          canReachViewer: false,
          reason: 'mode_none',
        },
      }),
      makeAgent({
        id: 'a-oneway',
        name: 'oneway-lead',
        messageMode: 'owner',
        suspendPolicy: 'never',
        _messageability: {
          canMessage: true,
          canReachViewer: false,
          replyReason: 'mode_owner',
        },
      }),
    ];

    const el = await mountAgents();
    const cells = Array.from(el.shadowRoot!.querySelectorAll('[data-testid="traits-cell"]'));
    expect(cells.length).toBe(3);

    // Default agent (project mode, auto policy, two-way reachability) -> empty cell
    expect(cells[0].textContent?.trim()).toBe('');
    expect(cells[0].querySelector('[data-testid="trait-message-mode"]')).toBeNull();
    expect(cells[0].querySelector('[data-testid="never-suspend-badge"]')).toBeNull();
    expect(cells[0].querySelector('scion-messageability-indicator')).toBeNull();

    // Sealed agent (none mode, unreachable) -> Sealed pill + reachability indicator, no never-suspend
    const sealedPill = cells[1].querySelector('[data-testid="trait-message-mode"]');
    expect(sealedPill?.textContent?.trim()).toBe('Sealed');
    expect(sealedPill?.getAttribute('aria-label')).toContain('Sealed');
    expect(sealedPill?.getAttribute('title')).toContain('Cannot send or receive messages');
    expect(cells[1].querySelector('[data-testid="never-suspend-badge"]')).toBeNull();
    expect(cells[1].querySelector('scion-messageability-indicator')).not.toBeNull();

    // One-way never-suspend agent (owner mode, never suspend, one-way) -> all three
    const ownerPill = cells[2].querySelector('[data-testid="trait-message-mode"]');
    expect(ownerPill?.textContent?.trim()).toBe('Owner');
    expect(ownerPill?.getAttribute('aria-label')).toContain('Owner');
    const nsPill = cells[2].querySelector('[data-testid="never-suspend-badge"]');
    expect(nsPill).not.toBeNull();
    expect(nsPill?.getAttribute('aria-label')).toContain('Never suspend');
    expect(cells[2].querySelector('scion-messageability-indicator')).not.toBeNull();
  });
});

describe('Never-suspend project detail (scion-page-project-detail)', () => {
  let suspendCalls: Array<{ url: string; body: string | null }>;

  beforeAll(async () => {
    await import('./project-detail.js');
  });

  beforeEach(() => {
    toasts.length = 0;
    suspendCalls = [];
    confirmMock.mockReset();
    confirmMock.mockResolvedValue(true);
    localStorage.setItem('scion-view-project-agents', 'list');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/projects/proj-1');
    vi.stubGlobal('EventSource', FakeEventSource);

    const flagged = makeAgent({ id: 'agent-1', name: 'pinned-lead', suspendPolicy: 'never' });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        if (url.endsWith('/api/v1/agents/agent-1/suspend') && method === 'POST') {
          suspendCalls.push({
            url,
            body: typeof init?.body === 'string' ? init.body : null,
          });
          return Promise.resolve(jsonResponse({ ...flagged, phase: 'suspended' }));
        }
        if (url.includes('/api/v1/projects/proj-1/agents')) {
          return Promise.resolve(
            jsonResponse({
              agents: [flagged],
              totalCount: 1,
              stats: { total: 1, running: 1 },
            })
          );
        }
        if (url.endsWith('/api/v1/projects/proj-1')) {
          return Promise.resolve(
            jsonResponse({
              id: 'proj-1',
              name: 'Alpha',
              slug: 'alpha',
              status: 'active',
              _capabilities: { actions: ['read', 'update'] },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    localStorage.removeItem('scion-view-project-agents');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('clicks Suspend on a flagged agent row and sends force=true after confirmation', async () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.pageData = {
      path: '/projects/proj-1',
      title: 'Alpha',
      user: { id: 'u1', email: 'alice@example.com', name: 'Alice', role: 'admin' },
    };
    document.body.appendChild(el);

    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-testid="agent-row-suspend"]')).not.toBeNull();
    });

    (el.shadowRoot!.querySelector('[data-testid="agent-row-suspend"]') as HTMLElement).click();

    await vi.waitFor(() => {
      expect(confirmMock).toHaveBeenCalledTimes(1);
      expect(suspendCalls.length).toBe(1);
    });
    expect(JSON.parse(suspendCalls[0].body!)).toEqual({ force: true });
  });
});
