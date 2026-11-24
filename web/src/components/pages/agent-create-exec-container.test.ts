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
 * Create Agent — Execution container (shell harness) UI (ptone/scion#8819):
 * - Offers the "Execution container" (`shell`) harness option only when
 *   `hub.exec_containers` is enabled.
 * - Prefills `shell` when navigated with `?harness=shell` while the flag is on.
 * - Hides the Task and Template fields for `shell` and omits both from the
 *   create request (the hub refuses initial tasks and templates on shell agents).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals } from './__fixtures__/agent-create-internals.js';
import { invalidatePublicSettings } from '../../client/server-feature-flags.js';
import { EXEC_CONTAINERS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';

interface CreatePrivate extends HTMLElement {
  loading: boolean;
  name: string;
  projectId: string;
  templateId: string;
  harness: string;
  task: string;
  updateComplete: Promise<unknown>;
  handleSubmit(e: Event): Promise<void>;
}

let bodies: Array<Record<string, unknown>> = [];

function stubFetch(): void {
  bodies = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not created' } }),
        } as Response);
      }
      let body: unknown = { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      if (url.includes('/settings/public')) {
        body = {};
      } else if (url.includes('/api/v1/projects?')) {
        body = {
          projects: [
            { id: 'p1', name: 'P1' },
            { id: 'p2', name: 'P2' },
          ],
        };
      } else if (url.includes('/api/v1/projects/') && url.includes('/settings')) {
        body = {};
      } else if (url.includes('/api/v1/templates')) {
        body = {
          templates: [
            {
              id: 't1',
              name: 'default',
              slug: 'default',
              harness: 'claude',
              status: 'active',
              scope: 'global',
            },
          ],
        };
      } else if (url.includes('/api/v1/harness-configs')) {
        body = { harnessConfigs: [{ name: 'claude' }, { name: 'gemini-cli' }] };
      } else if (url.includes('/gcp-service-accounts')) {
        body = { items: [] };
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
      } as Response);
    })
  );
}

beforeAll(async () => {
  await import('./agent-create.js');
});

afterEach(() => {
  vi.unstubAllGlobals();
  invalidatePublicSettings();
  document.body.innerHTML = '';
  const features = { ...window.__SCION_FEATURES__ };
  delete features[EXEC_CONTAINERS_FLAG];
  window.__SCION_FEATURES__ = features;
  window.history.replaceState({}, '', '/agents/new');
});

async function settle(c: CreatePrivate): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 20));
  await c.updateComplete;
}

async function mount(search = ''): Promise<CreatePrivate> {
  window.history.replaceState({}, '', `/agents/new${search}`);
  stubFetch();
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  const c = createInternals<CreatePrivate>(el);
  await settle(c);
  c.name = 'ops-shell';
  await c.updateComplete;
  return c;
}

describe('Create Agent — execution container (shell harness)', () => {
  it('hides the shell option and ignores ?harness=shell when hub.exec_containers is off', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, false);
    const c = await mount('?harness=shell');

    const shellOpt = c.shadowRoot?.querySelector('#harness sl-option[value="shell"]');
    expect(shellOpt).toBeNull();
    expect(c.harness).toBe('');
    expect(c.shadowRoot?.querySelector('#task')).not.toBeNull();
    expect(c.shadowRoot?.querySelector('#template')).not.toBeNull();
  });

  it('offers the Execution container option when hub.exec_containers is on', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const c = await mount();

    const shellOpt = c.shadowRoot?.querySelector('#harness sl-option[value="shell"]');
    expect(shellOpt).not.toBeNull();
    expect(shellOpt?.textContent).toContain('Execution container');
  });

  it('prefills shell from ?harness=shell, hides Task and Template, and omits both on submit', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const c = await mount('?harness=shell');

    expect(c.harness).toBe('shell');
    expect(c.shadowRoot?.querySelector('#task')).toBeNull();
    expect(c.shadowRoot?.querySelector('#template')).toBeNull();

    // Even if a task string were set in state, submit must omit task and template.
    c.task = 'should not be sent';
    await c.handleSubmit(new Event('submit'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].harnessConfig).toBe('shell');
    expect(bodies[0]).not.toHaveProperty('task');
    expect(bodies[0]).not.toHaveProperty('template');
  });

  it('keeps shell selected when switching projects and restores Task when switching back', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const c = await mount('?harness=shell');

    const projSelect = c.shadowRoot?.querySelector('#project') as HTMLElement & { value: string };
    projSelect.value = 'p2';
    projSelect.dispatchEvent(new Event('sl-change'));
    await settle(c);

    expect(c.projectId).toBe('p2');
    expect(c.harness).toBe('shell');
    expect(c.shadowRoot?.querySelector('#task')).toBeNull();

    const harnessSelect = c.shadowRoot?.querySelector('#harness') as HTMLElement & {
      value: string;
    };
    harnessSelect.value = 'claude';
    harnessSelect.dispatchEvent(new Event('sl-change'));
    await c.updateComplete;

    expect(c.harness).toBe('claude');
    expect(c.shadowRoot?.querySelector('#task')).not.toBeNull();
    expect(c.shadowRoot?.querySelector('#template')).not.toBeNull();
  });
});
