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
 * The pending secrets hint on the Create Agent page: shown once after a
 * create that started the agent, whose launch the hub is running, or that
 * was followed by a successful start; not after an incomplete create or a
 * failed start.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const { showToast, showPendingSecretsStartHint } = vi.hoisted(() => ({
  showToast: vi.fn(),
  showPendingSecretsStartHint: vi.fn(() => Promise.resolve()),
}));
vi.mock('../../utils/toast.js', () => ({ showToast }));
vi.mock('../../client/pending-secrets.js', () => ({ showPendingSecretsStartHint }));

let posts: string[] = [];
let navigations: string[] = [];

function recordNavigation(e: Event): void {
  navigations.push((e as CustomEvent<{ path: string }>).detail.path);
}

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as Response;
}

/** Accepts the create with `created` as the agent; the start answers `startStatus`. */
function stubFetch(created: Record<string, unknown>, startStatus = 200): void {
  posts = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if ((init?.method ?? 'GET') === 'POST') {
        posts.push(url);
        if (url.endsWith('/api/v1/agents')) {
          return Promise.resolve(jsonResponse({ agent: { id: 'agent-1', ...created } }, 201));
        }
        if (url.endsWith('/start')) {
          return Promise.resolve(
            startStatus === 200
              ? jsonResponse({ agent: { id: 'agent-1' } })
              : jsonResponse({ error: { code: 'start_failed', message: 'no broker' } }, startStatus)
          );
        }
        return Promise.resolve(jsonResponse({}));
      }
      return Promise.resolve(
        jsonResponse({ projects: [], brokers: [], templates: [], harnessConfigs: [] })
      );
    })
  );
}

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

interface AgentCreateInternals {
  name: string;
  projectId: string;
  submitting: boolean;
}

async function createAndStart(created: Record<string, unknown>, startStatus = 200): Promise<void> {
  stubFetch(created, startStatus);
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create') as MountedEl;
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  const page = el as unknown as AgentCreateInternals;
  page.name = 'test-agent';
  page.projectId = 'p1';
  const start = Array.from(
    el.shadowRoot?.querySelectorAll<HTMLElement>('.form-actions sl-button') ?? []
  ).find((b) => b.textContent?.trim() === 'Start');
  expect(start).toBeDefined();
  start!.click();
  await vi.waitFor(() => {
    expect(posts.length).toBeGreaterThan(0);
    expect(page.submitting).toBe(false);
  });
}

beforeEach(() => {
  showToast.mockClear();
  showPendingSecretsStartHint.mockClear();
  navigations = [];
  document.addEventListener('nav-click', recordNavigation);
});

afterEach(() => {
  document.removeEventListener('nav-click', recordNavigation);
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('Create Agent: pending secrets hint', () => {
  it('shows the hint once when the hub runs the launch asynchronously', async () => {
    await createAndStart({
      phase: 'created',
      launch: { id: 'l-1', state: 'active', active: true, kind: 'create', step: 'claim' },
    });
    expect(posts).toEqual(['/api/v1/agents']);
    expect(showPendingSecretsStartHint).toHaveBeenCalledTimes(1);
    expect(navigations).toEqual(['/agents/agent-1']);
  });

  it('shows the hint once when the create response shows the agent started', async () => {
    await createAndStart({ phase: 'provisioning' });
    expect(posts).toEqual(['/api/v1/agents']);
    expect(showPendingSecretsStartHint).toHaveBeenCalledTimes(1);
  });

  it('shows the hint once after a successful explicit start', async () => {
    await createAndStart({ phase: 'created' });
    expect(posts).toEqual(['/api/v1/agents', '/api/v1/agents/agent-1/start']);
    expect(showPendingSecretsStartHint).toHaveBeenCalledTimes(1);
  });

  it('shows no hint when the explicit start fails', async () => {
    await createAndStart({ phase: 'created' }, 500);
    expect(posts).toEqual(['/api/v1/agents', '/api/v1/agents/agent-1/start']);
    expect(showPendingSecretsStartHint).not.toHaveBeenCalled();
    expect(showToast).toHaveBeenCalledWith(
      expect.stringContaining('Agent was created but did not start'),
      'danger'
    );
  });

  it('shows no hint for an async create that did not complete', async () => {
    await createAndStart({
      phase: 'error',
      launch: {
        id: 'l-1',
        state: 'ended',
        active: false,
        kind: 'create',
        error: 'runtime_error',
        endReason: 'failed',
      },
    });
    expect(posts).toEqual(['/api/v1/agents']);
    expect(showPendingSecretsStartHint).not.toHaveBeenCalled();
  });
});
