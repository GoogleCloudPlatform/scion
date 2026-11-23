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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { SYSTEM_ONE_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());

vi.mock('../../client/api.js', async () => {
  const actual = await vi.importActual<typeof import('../../client/api.js')>('../../client/api.js');
  return {
    extractApiError: actual.extractApiError,
    apiFetch,
  };
});

const mod = await import('./admin-system-one.js');
const {
  formatElapsedNs,
  formatCredentialStatus,
  hasInvalidSecretBinding,
  instanceNeedsProjectSelectorForTest,
} = mod;
type Page = import('./admin-system-one.js').ScionPageAdminSystemOne;
type AdapterCap = import('./admin-system-one.js').SystemOneAdapterCapability;
type Instance = import('./admin-system-one.js').SystemOneInstance;

const SAMPLE_ADAPTERS: AdapterCap[] = [
  {
    id: 'gemini_decisions',
    version: '0.1.0',
    displayName: 'Gemini Decisions',
    providerRetention: 'Free tier vs paid tier',
    defaultEndpoint: 'https://generativelanguage.googleapis.com',
    modelRequired: true,
    supportsModelListing: false,
    authModes: [
      {
        mode: 'owned_api_key',
        authKind: 'api_key',
        transportScheme: 'header:x-goog-api-key',
        customEndpointAllowed: true,
      },
      {
        mode: 'secret_ref',
        authKind: 'api_key',
        transportScheme: 'header:x-goog-api-key',
        customEndpointAllowed: false,
      },
      {
        mode: 'google_access_token',
        authKind: 'google_access_token',
        transportScheme: 'google_access_token',
        customEndpointAllowed: false,
        scopesRequired: true,
      },
    ],
    allowedSecretNames: ['GEMINI_API_KEY', 'GEMINI_TEAM_KEY'],
    defaultSecretName: 'GEMINI_API_KEY',
    allowedSecretScopes: ['hub', 'project', 'project_or_hub'],
    defaultSecretScope: 'project_or_hub',
  },
  {
    id: 'typesafe',
    version: '0.1.0',
    displayName: 'TypeSafe',
    defaultEndpoint: 'https://api.typesafe.ai',
    modelRequired: true,
    supportsModelListing: true,
    authModes: [
      {
        mode: 'owned_api_key',
        authKind: 'api_key',
        transportScheme: 'bearer',
        customEndpointAllowed: true,
      },
    ],
  },
];

const SAMPLE_INSTANCES: Instance[] = [
  {
    id: 'inst-1',
    name: 'gemini-named',
    adapter: 'gemini_decisions',
    model: 'gemini-decision-1',
    endpoint: 'https://generativelanguage.googleapis.com',
    auth: {
      kind: 'api_key',
      secretRef: { name: 'GEMINI_API_KEY', scope: 'project_or_hub' },
    },
    enabled: true,
    revision: 1,
    createdAt: '2026-11-20T10:00:00Z',
    updatedAt: '2026-11-20T10:00:00Z',
  },
  {
    id: 'inst-2',
    name: 'gemini-adc',
    adapter: 'gemini_decisions',
    model: 'gemini-decision-1',
    endpoint: 'https://generativelanguage.googleapis.com',
    auth: {
      kind: 'google_access_token',
      scopes: ['https://www.googleapis.com/auth/generative-language'],
      quotaProject: 'implicit (service account association)',
      credentialType: 'unresolved',
    },
    enabled: true,
    revision: 1,
    createdAt: '2026-11-20T11:00:00Z',
    updatedAt: '2026-11-20T11:00:00Z',
  },
  {
    id: 'inst-3',
    name: 'typesafe-owned',
    adapter: 'typesafe',
    model: 'jev-1.14.0',
    endpoint: 'https://custom.typesafe.example',
    auth: {
      kind: 'api_key',
      secretName: 'systemone.instance.inst-3',
    },
    enabled: true,
    revision: 1,
    createdAt: '2026-11-20T12:00:00Z',
    updatedAt: '2026-11-20T12:00:00Z',
  },
];

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

async function mount(): Promise<Page> {
  const el = document.createElement('scion-page-admin-system-one') as Page;
  document.body.appendChild(el);
  await settle();
  return el;
}

function shadowText(el: Page): string {
  return (el.shadowRoot?.textContent ?? '').replace(/\s+/g, ' ');
}

describe('admin-system-one helpers', () => {
  it('formats elapsed nanoseconds cleanly', () => {
    expect(formatElapsedNs(undefined)).toBe('Unavailable');
    expect(formatElapsedNs(0)).toBe('Unavailable');
    expect(formatElapsedNs(-10)).toBe('Unavailable');
    expect(formatElapsedNs(500_000)).toBe('<1 ms');
    expect(formatElapsedNs(42_000_000)).toBe('42 ms');
    expect(formatElapsedNs(1_500_000_000)).toBe('1.50 s');
  });

  it('formats credential status without revealing values', () => {
    expect(formatCredentialStatus(SAMPLE_INSTANCES[0]!)).toEqual({
      modeLabel: 'Named secret',
      statusText: 'Named: GEMINI_API_KEY (project_or_hub)',
    });
    expect(formatCredentialStatus(SAMPLE_INSTANCES[1]!)).toEqual({
      modeLabel: 'ADC (Hub process identity)',
      statusText: 'ADC (unresolved)',
    });
    expect(formatCredentialStatus(SAMPLE_INSTANCES[2]!)).toEqual({
      modeLabel: 'Owned API key',
      statusText: 'Instance key set',
    });
  });

  it('detects invalid named secret bindings when operator removes an extra name', () => {
    const byId = new Map(SAMPLE_ADAPTERS.map((a) => [a.id, a]));
    expect(hasInvalidSecretBinding(SAMPLE_INSTANCES[0]!, byId)).toBe(false);
    const removed: Instance = {
      ...SAMPLE_INSTANCES[0]!,
      auth: {
        kind: 'api_key',
        secretRef: { name: 'REMOVED_EXTRA_KEY', scope: 'hub' },
      },
    };
    expect(hasInvalidSecretBinding(removed, byId)).toBe(true);
  });

  it('identifies instances requiring project selector for Test', () => {
    expect(instanceNeedsProjectSelectorForTest(SAMPLE_INSTANCES[0]!)).toBe(true);
    expect(instanceNeedsProjectSelectorForTest(SAMPLE_INSTANCES[1]!)).toBe(false);
    expect(instanceNeedsProjectSelectorForTest(SAMPLE_INSTANCES[2]!)).toBe(false);
  });
});

describe('scion-page-admin-system-one', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    setFeatureFlag(SYSTEM_ONE_FLAG, false);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    setFeatureFlag(SYSTEM_ONE_FLAG, false);
  });

  it('renders 404 and makes zero API calls when hub.system_one is off', async () => {
    const el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('renders 404 when the Hub returns 404 even if browser flag is on', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockResolvedValue(json({ error: { message: 'not found' } }, 404));
    const el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
  });

  it('renders Access Denied when the Hub returns 403', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockResolvedValue(json({ error: { message: 'forbidden' } }, 403));
    const el = await mount();
    expect(shadowText(el)).toContain('Access Denied');
  });

  it('lists instances with credential status and never calls /probe on load', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(
          json({
            instances: [
              ...SAMPLE_INSTANCES,
              {
                ...SAMPLE_INSTANCES[0]!,
                id: 'inst-4',
                name: 'gemini-stale-ref',
                auth: {
                  kind: 'api_key',
                  secretRef: { name: 'REMOVED_KEY', scope: 'hub' },
                },
              },
            ],
            totalCount: 4,
          })
        );
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    const text = shadowText(el);
    expect(text).toContain('gemini-named');
    expect(text).toContain('Named: GEMINI_API_KEY (project_or_hub)');
    expect(text).toContain('gemini-adc');
    expect(text).toContain('ADC (unresolved)');
    expect(text).toContain('typesafe-owned');
    expect(text).toContain('Instance key set');
    expect(text).toContain('gemini-stale-ref');
    expect(text).toContain('REMOVED_KEY');
    expect(text).toContain('Invalid binding');

    // Never calls /probe on load
    const probeCalls = apiFetch.mock.calls.filter(([p]) => p.includes('/probe'));
    expect(probeCalls).toHaveLength(0);
  });

  it('renders Test dialog with cost warning, project selector, and unavailable model/usage', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockImplementation((path: string, init?: RequestInit) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      if (path === '/api/v1/projects') {
        return Promise.resolve(
          json({ projects: [{ id: 'proj-1', name: 'Alpha', slug: 'alpha' }] })
        );
      }
      if (
        path === '/api/v1/admin/system-one/instances/gemini-named/probe' &&
        init?.method === 'POST'
      ) {
        return Promise.resolve(
          json({
            class: 'success',
            decision: { kind: 'judgment_bool', bool: true },
            evaluation: {
              eval_id: 'ev-1',
              configured_model: 'gemini-decision-1',
              resolved_model: '',
              class: 'success',
              notes: ['model_unreported', 'usage_unreported'],
              elapsed: 125_000_000,
              auth: {
                auth_kind: 'api_key',
                secret_ref: {
                  name: 'GEMINI_API_KEY',
                  scope: 'project_or_hub',
                  scope_used: 'project',
                },
                secret_version: 2,
              },
            },
          })
        );
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    // Click Test on gemini-named row
    const row = el.shadowRoot?.querySelector('tr[data-instance-name="gemini-named"]');
    const testBtn = Array.from(row?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Test')
    ) as HTMLElement | undefined;
    expect(testBtn).toBeDefined();
    testBtn?.click();
    await settle();

    const dialogText = shadowText(el);
    expect(dialogText).toContain('This sends one request to the provider and may incur charges.');
    expect(dialogText).toContain('project.read');
    expect(dialogText).toContain('project.secret_read');

    const projectSelect = el.shadowRoot?.querySelector('sl-select[name="testProject"]') as
      | (HTMLElement & { value: string })
      | null;
    expect(projectSelect).not.toBeNull();
    expect(projectSelect?.getAttribute('label')).toBe('Project (required)');

    // Run Test must be disabled until a project is chosen for project_or_hub
    const runBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Run Test')
    ) as HTMLElement | undefined;
    expect(runBtn).toBeDefined();
    expect(runBtn?.hasAttribute('disabled')).toBe(true);

    // Choose a project -> Run Test becomes enabled
    if (projectSelect) {
      projectSelect.value = 'proj-1';
      projectSelect.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    }
    await settle();
    expect(runBtn?.hasAttribute('disabled')).toBe(false);

    // Click Run Test
    runBtn?.click();
    await settle();

    const resultEl = el.shadowRoot?.querySelector('[data-testid="probe-result"]');
    expect(resultEl).not.toBeNull();
    const resText = resultEl?.textContent ?? '';
    expect(resText).toContain('success');
    expect(resText).toContain('125 ms');
    expect(resText).toContain('gemini-decision-1');
    expect(resText).toContain('Model unreported');
    expect(resText).toContain('Usage unavailable');
    expect(resText).toContain('resolved: project v2');
  });

  it('hides project selector and enables Run Test immediately for ADC and owned-key instances', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    const adcRow = el.shadowRoot?.querySelector('tr[data-instance-name="gemini-adc"]');
    const testBtn = Array.from(adcRow?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Test')
    ) as HTMLElement | undefined;
    testBtn?.click();
    await settle();

    expect(el.shadowRoot?.querySelector('sl-select[name="testProject"]')).toBeNull();
    const runBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Run Test')
    ) as HTMLElement | undefined;
    expect(runBtn?.hasAttribute('disabled')).toBe(false);
  });

  it('C1 regression: nested sl-select sl-after-hide does not close Create or Test dialog or wipe API key', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      if (path === '/api/v1/projects') {
        return Promise.resolve(
          json({ projects: [{ id: 'proj-1', name: 'Alpha', slug: 'alpha' }] })
        );
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();

    // Open Create dialog and enter an API key
    const addBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Add Provider Instance')
    ) as HTMLElement | undefined;
    addBtn?.click();
    await settle();

    const createDialog = el.shadowRoot?.querySelector(
      'sl-dialog[label="Add System One Provider Instance"]'
    );
    expect(createDialog?.hasAttribute('open')).toBe(true);

    const apiKeyInput = createDialog?.querySelector('sl-input[name="apiKey"]') as
      | (HTMLElement & { value: string })
      | null;
    expect(apiKeyInput).not.toBeNull();
    if (apiKeyInput) {
      apiKeyInput.value = 'synthetic-secret-key';
      apiKeyInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true, composed: true }));
    }
    await settle();

    // Simulate nested sl-select firing bubbling sl-after-hide when its menu closes
    const adapterSelect = createDialog?.querySelector('sl-select[name="adapter"]');
    adapterSelect?.dispatchEvent(
      new CustomEvent('sl-after-hide', { bubbles: true, composed: true })
    );
    await settle();

    // Dialog must remain open and API key must remain intact
    expect(createDialog?.hasAttribute('open')).toBe(true);
    expect(
      (
        createDialog?.querySelector('sl-input[name="apiKey"]') as
          | (HTMLElement & { value: string })
          | null
      )?.value
    ).toBe('synthetic-secret-key');

    // Close Create dialog legitimately (target === currentTarget)
    createDialog?.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: false }));
    await settle();
    expect(createDialog?.hasAttribute('open')).toBe(false);

    // Now open Test dialog for gemini-named and dispatch bubbling sl-after-hide from testProject select
    const row = el.shadowRoot?.querySelector('tr[data-instance-name="gemini-named"]');
    const testBtn = Array.from(row?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Test')
    ) as HTMLElement | undefined;
    testBtn?.click();
    await settle();

    const testDialog = el.shadowRoot?.querySelector(
      'sl-dialog[label="Test Provider Instance: gemini-named"]'
    );
    expect(testDialog?.hasAttribute('open')).toBe(true);
    const projectSelect = testDialog?.querySelector('sl-select[name="testProject"]');
    projectSelect?.dispatchEvent(
      new CustomEvent('sl-after-hide', { bubbles: true, composed: true })
    );
    await settle();
    expect(testDialog?.hasAttribute('open')).toBe(true);
  });

  it('creates instances for owned_api_key, secret_ref, and ADC, clearing API key on submit and error', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    const postedBodies: Array<Record<string, unknown>> = [];
    let failNextPost = false;

    apiFetch.mockImplementation((path: string, init?: RequestInit) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances' && !init?.method) {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      if (path === '/api/v1/admin/system-one/instances' && init?.method === 'POST') {
        const raw = typeof init.body === 'string' ? init.body : '{}';
        postedBodies.push(JSON.parse(raw) as Record<string, unknown>);
        if (failNextPost) {
          failNextPost = false;
          return Promise.resolve(json({ error: { message: 'validation rejected' } }, 400));
        }
        return Promise.resolve(json(SAMPLE_INSTANCES[0], 201));
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    const openCreate = async (): Promise<Element> => {
      const addBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
        b.textContent?.includes('Add Provider Instance')
      ) as HTMLElement;
      addBtn.click();
      await settle();
      return el.shadowRoot!.querySelector('sl-dialog[label="Add System One Provider Instance"]')!;
    };
    const setVal = (parent: Element, selector: string, val: string, event = 'sl-input'): void => {
      const input = parent.querySelector(selector) as HTMLElement & { value: string };
      input.value = val;
      input.dispatchEvent(new CustomEvent(event, { bubbles: true, composed: true }));
    };

    // 1. Owned API key — error path still clears key immediately
    failNextPost = true;
    let dlg = await openCreate();
    setVal(dlg, 'sl-input[name="name"]', 'owned-err');
    setVal(dlg, 'sl-input[name="model"]', 'gemini-decision-1');
    setVal(dlg, 'sl-input[name="apiKey"]', 'synthetic-key-error');
    await settle();
    dlg.querySelector('form')?.dispatchEvent(new Event('submit', { cancelable: true }));
    await settle();
    expect(
      (dlg.querySelector('sl-input[name="apiKey"]') as HTMLElement & { value: string }).value
    ).toBe('');
    expect(shadowText(el)).toContain('validation rejected');

    // 2. Owned API key — success path clears key and submits expected payload
    setVal(dlg, 'sl-input[name="apiKey"]', 'synthetic-key-ok');
    await settle();
    dlg.querySelector('form')?.dispatchEvent(new Event('submit', { cancelable: true }));
    await settle();
    expect(postedBodies[1]).toMatchObject({
      name: 'owned-err',
      adapter: 'gemini_decisions',
      model: 'gemini-decision-1',
      auth: { kind: 'api_key', apiKey: 'synthetic-key-ok' },
    });
    expect(dlg.hasAttribute('open')).toBe(false);

    // 3. Named secret_ref
    dlg = await openCreate();
    setVal(dlg, 'sl-radio-group[name="authMode"]', 'secret_ref', 'sl-change');
    await settle();
    setVal(dlg, 'sl-input[name="name"]', 'named-one');
    setVal(dlg, 'sl-input[name="model"]', 'gemini-decision-1');
    await settle();
    dlg.querySelector('form')?.dispatchEvent(new Event('submit', { cancelable: true }));
    await settle();
    expect(postedBodies[2]).toMatchObject({
      name: 'named-one',
      adapter: 'gemini_decisions',
      endpoint: 'https://generativelanguage.googleapis.com',
      auth: { kind: 'api_key', secretRef: { name: 'GEMINI_API_KEY', scope: 'project_or_hub' } },
    });

    // 4. ADC (google_access_token)
    dlg = await openCreate();
    setVal(dlg, 'sl-radio-group[name="authMode"]', 'google_access_token', 'sl-change');
    await settle();
    setVal(dlg, 'sl-input[name="name"]', 'adc-one');
    setVal(dlg, 'sl-input[name="model"]', 'gemini-decision-1');
    setVal(
      dlg,
      'sl-textarea[name="scopes"]',
      'https://www.googleapis.com/auth/generative-language https://www.googleapis.com/auth/cloud-platform'
    );
    setVal(dlg, 'sl-input[name="quotaProject"]', 'my-gcp-proj');
    await settle();
    dlg.querySelector('form')?.dispatchEvent(new Event('submit', { cancelable: true }));
    await settle();
    expect(postedBodies[3]).toMatchObject({
      name: 'adc-one',
      adapter: 'gemini_decisions',
      endpoint: 'https://generativelanguage.googleapis.com',
      auth: {
        kind: 'google_access_token',
        scopes: [
          'https://www.googleapis.com/auth/generative-language',
          'https://www.googleapis.com/auth/cloud-platform',
        ],
      },
      options: { quota_project: 'my-gcp-proj' },
    });
  });

  it('requires confirmation before sending DELETE and reloads list', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    const deleteCalls: string[] = [];
    apiFetch.mockImplementation((path: string, init?: RequestInit) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      if (path.startsWith('/api/v1/admin/system-one/instances/') && init?.method === 'DELETE') {
        deleteCalls.push(path);
        return Promise.resolve(json({}, 204));
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    const row = el.shadowRoot?.querySelector('tr[data-instance-name="typesafe-owned"]');
    const delBtn = Array.from(row?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Delete')
    ) as HTMLElement;
    delBtn.click();
    await settle();

    // Must not have called DELETE before confirmation
    expect(deleteCalls).toHaveLength(0);

    const confirmBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Delete Instance')
    ) as HTMLElement;
    confirmBtn.click();
    await settle();

    expect(deleteCalls).toEqual(['/api/v1/admin/system-one/instances/typesafe-owned']);
  });

  it('sends exact probe payload, deduplicates double-clicks, and ignores stale results on close/reopen (R1)', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    const probeRequests: Array<{ path: string; body: Record<string, unknown> }> = [];
    let resolveFirstProbe!: (res: Response) => void;

    apiFetch.mockImplementation((path: string, init?: RequestInit) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(json({ instances: SAMPLE_INSTANCES, totalCount: 3 }));
      }
      if (path === '/api/v1/projects') {
        return Promise.resolve(
          json({ projects: [{ id: 'proj-1', name: 'Alpha', slug: 'alpha' }] })
        );
      }
      if (path.endsWith('/probe') && init?.method === 'POST') {
        const raw = typeof init.body === 'string' ? init.body : '{}';
        probeRequests.push({
          path,
          body: JSON.parse(raw) as Record<string, unknown>,
        });
        return new Promise<Response>((resolve) => {
          resolveFirstProbe = resolve;
        });
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();

    // Open Test on gemini-named and select project
    const rowA = el.shadowRoot?.querySelector('tr[data-instance-name="gemini-named"]');
    Array.from(rowA?.querySelectorAll('sl-button') ?? [])
      .find((b) => b.textContent?.includes('Test'))
      ?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await settle();

    const projectSelect = el.shadowRoot?.querySelector(
      'sl-select[name="testProject"]'
    ) as HTMLElement & { value: string };
    projectSelect.value = 'proj-1';
    projectSelect.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await settle();

    // Click Run Test twice rapidly -> must send ONLY ONE probe request
    const runBtn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find((b) =>
      b.textContent?.includes('Run Test')
    ) as HTMLElement;
    runBtn.click();
    runBtn.click();
    await settle();

    expect(probeRequests).toHaveLength(1);
    expect(probeRequests[0]?.path).toBe('/api/v1/admin/system-one/instances/gemini-named/probe');
    expect(probeRequests[0]?.body).toMatchObject({
      project: 'proj-1',
      probe: { text: expect.any(String) as unknown },
      spec: { version: 1 },
    });

    // Close dialog and reopen for gemini-adc BEFORE first probe resolves (R1)
    const closeBtn = Array.from(
      el.shadowRoot
        ?.querySelector('sl-dialog[label="Test Provider Instance: gemini-named"]')
        ?.querySelectorAll('sl-button') ?? []
    ).find((b) => b.textContent?.includes('Close')) as HTMLElement;
    closeBtn.click();
    await settle();

    const rowB = el.shadowRoot?.querySelector('tr[data-instance-name="gemini-adc"]');
    Array.from(rowB?.querySelectorAll('sl-button') ?? [])
      .find((b) => b.textContent?.includes('Test'))
      ?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await settle();

    // Now resolve the stale probe for gemini-named
    resolveFirstProbe(
      json({
        class: 'success',
        evaluation: {
          eval_id: 'stale-ev',
          configured_model: 'gemini-decision-1',
          resolved_model: 'stale-model-should-not-appear',
          class: 'success',
          elapsed: 50_000_000,
        },
      })
    );
    await settle();

    // Stale result must NOT appear under gemini-adc
    expect(el.shadowRoot?.querySelector('[data-testid="probe-result"]')).toBeNull();
    expect(shadowText(el)).not.toContain('stale-model-should-not-appear');
  });

  it('follows nextCursor pagination when listing instances', async () => {
    setFeatureFlag(SYSTEM_ONE_FLAG, true);
    apiFetch.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system-one/adapters') {
        return Promise.resolve(json({ adapters: SAMPLE_ADAPTERS }));
      }
      if (path === '/api/v1/admin/system-one/instances') {
        return Promise.resolve(
          json({ instances: [SAMPLE_INSTANCES[0]], nextCursor: 'page-2', totalCount: 2 })
        );
      }
      if (path === '/api/v1/admin/system-one/instances?cursor=page-2&limit=200') {
        return Promise.resolve(json({ instances: [SAMPLE_INSTANCES[1]], totalCount: 2 }));
      }
      return Promise.reject(new Error(`Unexpected request: ${path}`));
    });

    const el = await mount();
    const text = shadowText(el);
    expect(text).toContain('gemini-named');
    expect(text).toContain('gemini-adc');
  });
});
