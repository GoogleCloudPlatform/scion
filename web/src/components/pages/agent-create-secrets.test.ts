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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals } from './__fixtures__/agent-create-internals.js';
import { invalidatePublicSettings } from '../../client/server-feature-flags.js';
import { AGENT_SECRETS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';

interface CreatePrivate extends HTMLElement {
  loading: boolean;
  name: string;
  projectId: string;
  error: string | null;
  errorLinks: Array<{ label: string; href: string }>;
  updateComplete: Promise<unknown>;
  handleSubmit(e: Event): Promise<void>;
}

let postBodies: Array<Record<string, unknown>> = [];
let postResponse: () => Response = () =>
  new Response(JSON.stringify({ agent: { id: 'new-agent-1', phase: 'running' } }), {
    status: 201,
    headers: { 'Content-Type': 'application/json' },
  });

function stubFetch(): void {
  postBodies = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          postBodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve(postResponse());
      }
      let body: unknown = { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      if (url.includes('/api/v1/projects?')) {
        body = { projects: [{ id: 'p1', name: 'P1' }] };
      } else if (url.includes('/settings/public')) {
        body = {};
      }
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
}

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

async function mountCreate(experimentOn: boolean): Promise<{
  page: CreatePrivate;
  form: ScionAgentConfigForm;
}> {
  if (experimentOn) {
    window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
  } else {
    delete window.__SCION_FEATURES__;
  }
  stubFetch();
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  const page = createInternals<CreatePrivate>(el);
  await settle(page);
  page.name = 'secret-agent';
  page.projectId = 'p1';
  await page.updateComplete;
  const form = el.shadowRoot!.querySelector('scion-agent-config-form') as ScionAgentConfigForm;
  return { page, form };
}

type Valued = HTMLElement & { value: string; type?: string };

async function addAndFillRow(
  form: ScionAgentConfigForm,
  opts: {
    key: string;
    value: string;
    type?: 'environment' | 'file' | 'variable';
    target?: string;
    injectionMode?: 'as_needed' | 'always';
  }
): Promise<HTMLElement> {
  const addBtn = form.shadowRoot!.querySelector<HTMLElement>('[data-testid="add-secret-row"]')!;
  addBtn.click();
  await form.updateComplete;

  const rows = form.shadowRoot!.querySelectorAll<HTMLElement>('[data-testid="secret-row"]');
  const rowEl = rows[rows.length - 1];

  const keyInput = rowEl.querySelector<Valued>('[data-field="key"]')!;
  keyInput.value = opts.key;
  keyInput.dispatchEvent(new Event('sl-input'));
  await form.updateComplete;

  if (opts.type) {
    const typeSelect = rowEl.querySelector<Valued>('[data-field="type"]')!;
    typeSelect.value = opts.type;
    typeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
  }

  const valInput = rowEl.querySelector<Valued>('[data-field="value"]')!;
  valInput.value = opts.value;
  valInput.dispatchEvent(new Event('sl-input'));
  await form.updateComplete;

  if (opts.target !== undefined) {
    const targetInput = rowEl.querySelector<Valued>('[data-field="target"]')!;
    targetInput.value = opts.target;
    targetInput.dispatchEvent(new Event('sl-input'));
    await form.updateComplete;
  }

  if (opts.injectionMode) {
    const modeSelect = rowEl.querySelector<Valued>('[data-field="injectionMode"]')!;
    modeSelect.value = opts.injectionMode;
    modeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
  }

  return rowEl;
}

describe('create agent page — Secrets tab (hub.agent_secrets)', () => {
  beforeAll(async () => {
    await Promise.all([import('./agent-create.js'), import('../shared/agent-config-form.js')]);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    invalidatePublicSettings();
    document.body.innerHTML = '';
    localStorage.clear();
    sessionStorage.clear();
    delete window.__SCION_FEATURES__;
    postResponse = () =>
      new Response(JSON.stringify({ agent: { id: 'new-agent-1', phase: 'running' } }), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      });
  });

  it('shows the Secrets tab only in create mode and only when hub.agent_secrets is on', async () => {
    const off = await mountCreate(false);
    expect(off.form.shadowRoot!.querySelector('[data-testid="tab-secrets"]')).toBeNull();
    off.page.remove();

    const on = await mountCreate(true);
    expect(on.form.shadowRoot!.querySelector('[data-testid="tab-secrets"]')).not.toBeNull();
    expect(
      on.form
        .shadowRoot!.querySelector('[data-testid="profile-secrets-link"]')
        ?.getAttribute('href')
    ).toBe('/profile/secrets');
    on.page.remove();

    // Edit mode never renders the create Secrets tab even with the experiment on.
    window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
    const editForm = document.createElement('scion-agent-config-form') as ScionAgentConfigForm;
    editForm.mode = 'edit';
    document.body.appendChild(editForm);
    await editForm.updateComplete;
    expect(editForm.shadowRoot!.querySelector('[data-testid="create-secrets-editor"]')).toBeNull();
  });

  it('masks values with show/hide toggle and uses textarea for file type', async () => {
    const { form } = await mountCreate(true);
    const rowEl = await addAndFillRow(form, { key: 'API_KEY', value: 's3cr3t' });

    const valInput = rowEl.querySelector<Valued>('[data-field="value"]')!;
    expect(valInput.tagName.toLowerCase()).toBe('sl-input');
    expect(valInput.getAttribute('type')).toBe('password');

    // Toggle show/hide
    rowEl.querySelector<HTMLElement>('[data-action="toggle-reveal"]')!.click();
    await form.updateComplete;
    expect(rowEl.querySelector<Valued>('[data-field="value"]')!.getAttribute('type')).toBe('text');

    // Switch to file type -> renders sl-textarea
    const typeSelect = rowEl.querySelector<Valued>('[data-field="type"]')!;
    typeSelect.value = 'file';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
    expect(rowEl.querySelector('[data-field="value"]')!.tagName.toLowerCase()).toBe('sl-textarea');
  });

  it('omits secrets from POST body when none are entered', async () => {
    const { page } = await mountCreate(true);
    await page.handleSubmit(new Event('submit'));

    expect(postBodies).toHaveLength(1);
    expect(postBodies[0]).not.toHaveProperty('secrets');
  });

  it('omits secrets from POST body when hub.agent_secrets turns off after rows were entered', async () => {
    const { page, form } = await mountCreate(true);
    await addAndFillRow(form, { key: 'LATE_FLAG_OFF', value: 'should-not-send' });
    expect(form.collectSecrets()).toHaveLength(1);

    setFeatureFlag(AGENT_SECRETS_FLAG, false);
    await form.updateComplete;
    await page.updateComplete;

    expect(form.collectSecrets()).toEqual([]);
    await page.handleSubmit(new Event('submit'));

    expect(postBodies).toHaveLength(1);
    expect(postBodies[0]).not.toHaveProperty('secrets');
  });

  it('sends secrets in POST body, never in config.env or storage, and clears values after create', async () => {
    const { page, form } = await mountCreate(true);

    await addAndFillRow(form, {
      key: 'GITHUB_TOKEN',
      value: 'ghp_secret_canary_value',
      type: 'environment',
      injectionMode: 'always',
    });
    await addAndFillRow(form, {
      key: 'DEPLOY_KEY',
      value: '-----BEGIN KEY-----\nsecret-pem-canary\n-----END KEY-----',
      type: 'file',
      target: '~/.ssh/id_ed25519',
      injectionMode: 'as_needed',
    });

    await page.handleSubmit(new Event('submit'));

    expect(postBodies).toHaveLength(1);
    expect(postBodies[0].secrets).toEqual([
      {
        key: 'GITHUB_TOKEN',
        value: 'ghp_secret_canary_value',
        type: 'environment',
        injectionMode: 'always',
      },
      {
        key: 'DEPLOY_KEY',
        value: '-----BEGIN KEY-----\nsecret-pem-canary\n-----END KEY-----',
        type: 'file',
        target: '~/.ssh/id_ed25519',
        injectionMode: 'as_needed',
      },
    ]);

    // Never in config.env, localStorage, sessionStorage, or URL.
    const cfg = postBodies[0].config as Record<string, unknown> | undefined;
    expect(cfg?.env).toBeUndefined();
    expect(JSON.stringify(localStorage)).not.toContain('ghp_secret_canary_value');
    expect(JSON.stringify(sessionStorage)).not.toContain('ghp_secret_canary_value');
    expect(window.location.href).not.toContain('ghp_secret_canary_value');

    // Cleared after successful create.
    expect(form.collectSecrets()).toEqual([]);
  });

  it('clears secret values on navigation / disconnect', async () => {
    const { page, form } = await mountCreate(true);
    await addAndFillRow(form, { key: 'TEMP_TOKEN', value: 'ephemeral-value' });
    expect(form.collectSecrets()).toHaveLength(1);

    page.remove();
    expect(form.collectSecrets()).toEqual([]);
  });

  it('blocks submit on validation errors (reserved key, duplicate, file without target, >50)', async () => {
    // 1. Reserved key
    let mounted = await mountCreate(true);
    await addAndFillRow(mounted.form, { key: 'SCION_HUB_TOKEN', value: 'x' });
    await mounted.page.handleSubmit(new Event('submit'));
    expect(postBodies).toHaveLength(0);
    expect(mounted.page.error).toContain('reserved');
    mounted.page.remove();

    // 2. Duplicate key
    mounted = await mountCreate(true);
    await addAndFillRow(mounted.form, { key: 'DUP', value: 'a' });
    await addAndFillRow(mounted.form, { key: 'DUP', value: 'b' });
    await mounted.page.handleSubmit(new Event('submit'));
    expect(postBodies).toHaveLength(0);
    expect(mounted.page.error).toContain('Duplicate');
    mounted.page.remove();

    // 3. File without target
    mounted = await mountCreate(true);
    await addAndFillRow(mounted.form, { key: 'PEM', value: 'data', type: 'file', target: '' });
    await mounted.page.handleSubmit(new Event('submit'));
    expect(postBodies).toHaveLength(0);
    expect(mounted.page.error).toContain('target');
    mounted.page.remove();

    // 4. >50 secrets
    mounted = await mountCreate(true);
    (mounted.form as unknown as { secretRows: unknown[] }).secretRows = Array.from(
      { length: 51 },
      (_, i) => ({
        key: `K_${i}`,
        value: `v_${i}`,
        type: 'environment',
        target: '',
        injectionMode: 'as_needed',
        revealed: false,
      })
    );
    await mounted.page.handleSubmit(new Event('submit'));
    expect(postBodies).toHaveLength(0);
    expect(mounted.page.error).toContain('50');
    mounted.page.remove();
  });

  it('surfaces server 400 and 503 errors cleanly', async () => {
    // 400
    postResponse = () =>
      new Response(JSON.stringify({ error: { message: 'secret key is reserved' } }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      });
    let mounted = await mountCreate(true);
    await addAndFillRow(mounted.form, { key: 'VALID_KEY', value: 'val' });
    await mounted.page.handleSubmit(new Event('submit'));
    expect(mounted.page.error).toContain('secret key is reserved');
    mounted.page.remove();

    // 503 fail-closed
    postResponse = () =>
      new Response(
        JSON.stringify({
          error: { message: 'agent secrets could not be stored; the agent was not created' },
        }),
        { status: 503, headers: { 'Content-Type': 'application/json' } }
      );
    mounted = await mountCreate(true);
    await addAndFillRow(mounted.form, { key: 'VALID_KEY', value: 'val' });
    await mounted.page.handleSubmit(new Event('submit'));
    expect(mounted.page.error).toContain(
      'agent secrets could not be stored; the agent was not created'
    );
    mounted.page.remove();
  });
});
