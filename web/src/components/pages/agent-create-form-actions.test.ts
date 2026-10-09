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
 * The Create Agent form's actions (ptone/scion#3973).
 *
 * The form used to offer a bare "Create" button next to "Start" that created
 * the agent without starting it (`provisionOnly`). Users mistook it for the
 * form's primary action. The form now has a single submit action, Start, plus
 * Cancel, and the web never sends `provisionOnly`. (The CLI's `scion create`
 * keeps that behaviour; it is not part of this page.)
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }));
vi.mock('../../utils/toast.js', () => ({ showToast }));

interface RecordedRequest {
  url: string;
  method: string;
  body?: Record<string, unknown>;
}

let requests: RecordedRequest[] = [];
let navigations: string[] = [];

function recordNavigation(e: Event): void {
  navigations.push((e as CustomEvent<{ path: string }>).detail.path);
}

/** A start response; defaults to success. */
interface StartResponse {
  ok: boolean;
  status: number;
  body?: unknown;
  /** When set, the start fetch rejects with this error. */
  reject?: Error;
}

/** Accepts the create request and records every POST for inspection. */
function stubFetch(createdPhase?: string, start: StartResponse = { ok: true, status: 200 }): void {
  requests = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString();
      const method = init?.method ?? 'GET';
      if (method === 'POST') {
        requests.push({
          url,
          method,
          body:
            typeof init?.body === 'string'
              ? (JSON.parse(init.body) as Record<string, unknown>)
              : undefined,
        });
        if (url.endsWith('/api/v1/agents')) {
          return Promise.resolve({
            ok: true,
            status: 201,
            json: async () => ({ agent: { id: 'agent-1', phase: createdPhase } }),
          } as Response);
        }
        if (url.endsWith('/start')) {
          if (start.reject) return Promise.reject(start.reject);
          return Promise.resolve({
            ok: start.ok,
            status: start.status,
            json: async () => {
              if (start.body === undefined) throw new SyntaxError('no JSON body');
              return start.body;
            },
          } as Response);
        }
        return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
}

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

interface AgentCreateInternals {
  name: string;
  projectId: string;
  submitting: boolean;
}

async function mountAgentCreate(): Promise<MountedEl> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  const mounted = el as MountedEl;
  await mounted.updateComplete;
  return mounted;
}

function actionButtons(el: MountedEl): HTMLElement[] {
  return Array.from(el.shadowRoot?.querySelectorAll<HTMLElement>('.form-actions sl-button') ?? []);
}

function label(button: HTMLElement): string {
  return button.textContent?.trim() ?? '';
}

/** Click Start and wait until the submit has finished. */
async function clickStart(el: MountedEl): Promise<void> {
  const start = actionButtons(el).find((b) => label(b) === 'Start');
  expect(start, 'the form should have a Start button').toBeDefined();
  start!.click();
  const page = el as unknown as AgentCreateInternals;
  await vi.waitFor(() => {
    expect(requests.length).toBeGreaterThan(0);
    expect(page.submitting).toBe(false);
  });
}

beforeEach(() => {
  stubFetch();
  showToast.mockClear();
  navigations = [];
  document.addEventListener('nav-click', recordNavigation);
});

afterEach(() => {
  document.removeEventListener('nav-click', recordNavigation);
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('Create Agent form actions', () => {
  it('offers exactly Start and Cancel', async () => {
    const el = await mountAgentCreate();

    expect(actionButtons(el).map(label)).toEqual(['Start', 'Cancel']);
  });

  it('marks Start as the only primary action', async () => {
    const el = await mountAgentCreate();

    const primary = actionButtons(el).filter((b) => b.getAttribute('variant') === 'primary');
    expect(primary.map(label)).toEqual(['Start']);
  });

  it('creates the agent without provisionOnly and then starts it', async () => {
    const el = await mountAgentCreate();
    const page = el as unknown as AgentCreateInternals;
    page.name = 'test-agent';
    page.projectId = 'p1';

    await clickStart(el);

    expect(requests.map((r) => r.url)).toEqual(['/api/v1/agents', '/api/v1/agents/agent-1/start']);
    expect(requests[0].body).toBeDefined();
    expect(requests[0].body).not.toHaveProperty('provisionOnly');
    expect(showToast).not.toHaveBeenCalled();
    expect(navigations).toEqual(['/agents/agent-1']);
  });

  it('shows the server error when the start call fails and still opens the agent', async () => {
    stubFetch(undefined, {
      ok: false,
      status: 500,
      body: { error: { code: 'runtime_error', message: 'broker unavailable' } },
    });
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const el = await mountAgentCreate();
    const page = el as unknown as AgentCreateInternals & { error: string | null };
    page.name = 'test-agent';
    page.projectId = 'p1';

    await clickStart(el);

    expect(requests.map((r) => r.url)).toEqual(['/api/v1/agents', '/api/v1/agents/agent-1/start']);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith(
      'Agent was created but did not start: broker unavailable',
      'danger'
    );
    expect(navigations).toEqual(['/agents/agent-1']);
    expect(page.error).toBeFalsy();
  });

  it('falls back to the HTTP status when the failed start response has no error text', async () => {
    stubFetch(undefined, { ok: false, status: 502 });
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const el = await mountAgentCreate();
    const page = el as unknown as AgentCreateInternals;
    page.name = 'test-agent';
    page.projectId = 'p1';

    await clickStart(el);

    expect(showToast).toHaveBeenCalledWith(
      'Agent was created but did not start: HTTP 502',
      'danger'
    );
    expect(navigations).toEqual(['/agents/agent-1']);
  });

  it('shows a toast and still opens the agent when the start fetch rejects', async () => {
    stubFetch(undefined, { ok: false, status: 0, reject: new TypeError('Failed to fetch') });
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const el = await mountAgentCreate();
    const page = el as unknown as AgentCreateInternals & { error: string | null };
    page.name = 'test-agent';
    page.projectId = 'p1';

    await clickStart(el);
    await vi.waitFor(() => expect(navigations).toEqual(['/agents/agent-1']));

    expect(requests.map((r) => r.url)).toEqual(['/api/v1/agents', '/api/v1/agents/agent-1/start']);
    expect(requests.filter((r) => r.url === '/api/v1/agents')).toHaveLength(1);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith(
      'Agent was created but did not start: Failed to fetch',
      'danger'
    );
    expect(page.error).toBeFalsy();
  });

  it('skips the separate start call when the create response shows the agent already starting', async () => {
    stubFetch('provisioning');
    const el = await mountAgentCreate();
    const page = el as unknown as AgentCreateInternals;
    page.name = 'test-agent';
    page.projectId = 'p1';

    await clickStart(el);

    expect(requests.map((r) => r.url)).toEqual(['/api/v1/agents']);
    expect(requests[0].body).not.toHaveProperty('provisionOnly');
    expect(showToast).not.toHaveBeenCalled();
    expect(navigations).toEqual(['/agents/agent-1']);
  });
});
