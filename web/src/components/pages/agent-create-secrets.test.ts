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
import type { ScionSecretValueField } from '../shared/secret-value-field.js';

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

function getValueField(rowEl: HTMLElement): ScionSecretValueField {
  return rowEl.querySelector<ScionSecretValueField>(
    'scion-secret-value-field[data-field="value"]'
  )!;
}

async function enterSecretValue(
  form: ScionAgentConfigForm,
  rowEl: HTMLElement,
  value: string
): Promise<void> {
  const field = getValueField(rowEl);
  await field.updateComplete;
  const input = field.shadowRoot!.querySelector<HTMLInputElement>('input.secret-input')!;
  if (value.includes('\n') || value.includes('\r')) {
    const pasteEv = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(pasteEv, 'clipboardData', {
      value: { getData: (t: string) => (t === 'text/plain' ? value : '') },
    });
    input.dispatchEvent(pasteEv);
  } else {
    input.value = value;
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  await field.updateComplete;
  await form.updateComplete;
}

async function addAndFillRow(
  form: ScionAgentConfigForm,
  opts: {
    key: string;
    value: string;
    type?: 'environment' | 'file' | 'on_demand';
    target?: string;
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

  await enterSecretValue(form, rowEl, opts.value);

  if (opts.target !== undefined) {
    const targetInput = rowEl.querySelector<Valued>('[data-field="target"]')!;
    targetInput.value = opts.target;
    targetInput.dispatchEvent(new Event('sl-input'));
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

  it('uses native password masking for every row type, adds Load from file on file rows, and reveals textarea only on Show', async () => {
    const { form } = await mountCreate(true);
    const rowEl = await addAndFillRow(form, { key: 'API_KEY', value: 's3cr3t' });

    const field = getValueField(rowEl);
    await field.updateComplete;
    const pwdInput = field.shadowRoot!.querySelector<HTMLInputElement>('input.secret-input')!;
    expect(pwdInput).not.toBeNull();
    expect(pwdInput.type).toBe('password');
    expect(pwdInput.autocomplete).toBe('off');
    expect(field.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(field.shadowRoot!.querySelector('input[type="file"]')).toBeNull();

    // Switch to file type while still masked -> stays native password input + shows Load from file
    const typeSelect = rowEl.querySelector<Valued>('[data-field="type"]')!;
    typeSelect.value = 'file';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
    await field.updateComplete;

    expect(field.shadowRoot!.querySelector('input.secret-input')?.getAttribute('type')).toBe(
      'password'
    );
    expect(field.shadowRoot!.querySelector('input[type="file"]')).not.toBeNull();
    expect(field.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(field.shadowRoot!.textContent).not.toContain('s3cr3t');

    // Toggle Show -> renders textarea with value and accurate aria-pressed
    const toggleBtn = field.shadowRoot!.querySelector<HTMLButtonElement>('.toggle-visibility')!;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await field.updateComplete;
    const textarea = field.shadowRoot!.querySelector<HTMLTextAreaElement>(
      'textarea.secret-textarea'
    )!;
    expect(textarea).not.toBeNull();
    expect(textarea.value).toBe('s3cr3t');
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    // Toggle Hide -> returns to native password input without losing value
    toggleBtn.click();
    await field.updateComplete;
    expect(field.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(field.shadowRoot!.querySelector('input.secret-input')?.getAttribute('type')).toBe(
      'password'
    );
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    expect(form.collectSecrets()[0].value).toBe('s3cr3t');
  });

  it('captures multi-line file values via paste and file picker without blur-swap or plaintext DOM leak', async () => {
    const { form } = await mountCreate(true);
    const rowEl = await addAndFillRow(form, {
      key: 'DEPLOY_KEY',
      value: '-----BEGIN KEY-----\npem-body\n-----END KEY-----',
      type: 'file',
      target: '~/.ssh/id_ed25519',
    });

    const field = getValueField(rowEl);
    await field.updateComplete;
    expect(field.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(field.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    expect(field.shadowRoot!.textContent).not.toContain('pem-body');
    expect(field.shadowRoot!.innerHTML).not.toContain('pem-body');
    expect(form.collectSecrets()[0].value).toBe('-----BEGIN KEY-----\npem-body\n-----END KEY-----');

    // Load from file replaces value verbatim while staying masked
    const fileInput = field.shadowRoot!.querySelector<HTMLInputElement>('input[type="file"]')!;
    const loadedPem = '-----BEGIN CERT-----\r\nfrom-file\r\n-----END CERT-----\r\n';
    const file = new File([loadedPem], 'cert.pem', { type: 'text/plain' });
    Object.defineProperty(fileInput, 'files', { value: [file], configurable: true });
    fileInput.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 10));
    await field.updateComplete;
    await form.updateComplete;

    expect(fileInput.value).toBe('');
    expect(field.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(field.shadowRoot!.textContent).not.toContain('from-file');
    expect(form.collectSecrets()[0].value).toBe(loadedPem);
  });

  it('keeps remaining row values isolated when an earlier row is removed', async () => {
    const { form } = await mountCreate(true);
    await addAndFillRow(form, { key: 'FIRST_KEY', value: 'first-secret' });
    await addAndFillRow(form, { key: 'SECOND_KEY', value: 'second-secret' });

    const firstRow = form.shadowRoot!.querySelectorAll<HTMLElement>(
      '[data-testid="secret-row"]'
    )[0];
    firstRow.querySelector<HTMLElement>('[data-action="remove"]')!.click();
    await form.updateComplete;

    expect(form.collectSecrets()).toEqual([
      {
        key: 'SECOND_KEY',
        value: 'second-secret',
        type: 'environment',
        injectionMode: 'always',
      },
    ]);
    const remainingRow = form.shadowRoot!.querySelector<HTMLElement>('[data-testid="secret-row"]')!;
    const remainingField = getValueField(remainingRow);
    await remainingField.updateComplete;
    expect(remainingField.value).toBe('second-secret');
  });

  it('omits secrets from POST body when none are entered', async () => {
    const { page } = await mountCreate(true);
    await page.handleSubmit(new Event('submit'));

    expect(postBodies).toHaveLength(1);
    expect(postBodies[0]).not.toHaveProperty('secrets');
  });

  it('omits secrets from POST body when hub.agent_secrets turns off and stays cleared if turned back on', async () => {
    const { page, form } = await mountCreate(true);
    await addAndFillRow(form, { key: 'LATE_FLAG_OFF', value: 'should-not-send' });
    expect(form.collectSecrets()).toHaveLength(1);

    setFeatureFlag(AGENT_SECRETS_FLAG, false);
    await form.updateComplete;
    await page.updateComplete;

    expect(form.collectSecrets()).toEqual([]);

    // Re-enabling the flag must not resurrect cleared secrets invisibly
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
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
    });
    await addAndFillRow(form, {
      key: 'DEPLOY_KEY',
      value: '-----BEGIN KEY-----\nsecret-pem-canary\n-----END KEY-----',
      type: 'file',
      target: '~/.ssh/id_ed25519',
    });
    await addAndFillRow(form, {
      key: 'CLI_TOKEN',
      value: 'cli-only-secret',
      type: 'on_demand',
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
      },
      {
        key: 'CLI_TOKEN',
        value: 'cli-only-secret',
        type: 'on_demand',
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

  it('clears secret values on navigation / disconnect and pagehide', async () => {
    const { page, form } = await mountCreate(true);
    await addAndFillRow(form, { key: 'TEMP_TOKEN', value: 'ephemeral-value' });
    expect(form.collectSecrets()).toHaveLength(1);

    window.dispatchEvent(new Event('pagehide'));
    await form.updateComplete;
    expect(form.collectSecrets()).toEqual([]);

    await addAndFillRow(form, { key: 'TEMP_TOKEN_2', value: 'ephemeral-value-2' });
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
        id: `bulk-row-${i}`,
        key: `K_${i}`,
        value: `v_${i}`,
        type: 'environment',
        target: '',
        injectionMode: 'as_needed',
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

  it('offers Environment/File/On demand, shows Type tooltip, hides delivery selector, and scopes Target to File only', async () => {
    const { form } = await mountCreate(true);
    const addBtn = form.shadowRoot!.querySelector<HTMLElement>('[data-testid="add-secret-row"]')!;
    addBtn.click();
    await form.updateComplete;

    const rowEl = form.shadowRoot!.querySelector<HTMLElement>('[data-testid="secret-row"]')!;
    const typeSelect = rowEl.querySelector<Valued>('[data-field="type"]')!;
    const optionValues = Array.from(typeSelect.querySelectorAll('sl-option')).map((o) =>
      o.getAttribute('value')
    );
    expect(optionValues).toEqual(['environment', 'file', 'on_demand']);

    // Type tooltip is present with descriptions of all three types.
    const tooltip = rowEl.querySelector('[data-testid="secret-type-tooltip"]');
    expect(tooltip).not.toBeNull();
    expect(tooltip!.textContent).toContain('Environment:');
    expect(tooltip!.textContent).toContain('File:');
    expect(tooltip!.textContent).toContain('On demand:');
    expect(tooltip!.textContent).toContain('scion secret get --value KEY');
    expect(tooltip!.querySelector('sl-icon[name="info-circle"]')).not.toBeNull();

    // No delivery (injectionMode) selector at agent scope.
    expect(rowEl.querySelector('[data-field="injectionMode"]')).toBeNull();

    // Target is hidden for environment.
    expect(rowEl.querySelector('[data-field="target"]')).toBeNull();

    // Target appears for file.
    typeSelect.value = 'file';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
    expect(rowEl.querySelector('[data-field="target"]')).not.toBeNull();

    // Target hides again for on_demand.
    typeSelect.value = 'on_demand';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await form.updateComplete;
    expect(rowEl.querySelector('[data-field="target"]')).toBeNull();
  });
});
