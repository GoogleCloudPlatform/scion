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
 * ptone/scion#8185 (sub-issue of ptone/scion#4895): the Project Settings
 * editor for the per-broker default GCP service account map
 * (defaultGCPIdentityServiceAccountIDByBroker), gated behind
 * hub.flat_runtime_brokers (FLAT_BROKERS_FLAG).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { FEATURE_FLAGS_CHANGED_EVENT, FLAT_BROKERS_FLAG } from '../../utils/feature-flags.js';
import type { ScionPageProjectSettings } from './project-settings.js';

const F = 'defaultGCPIdentityServiceAccountIDByBroker';

function setFlatBrokersFlag(on: boolean): void {
  window.__SCION_FEATURES__ = {
    ...(window.__SCION_FEATURES__ ?? {}),
    [FLAT_BROKERS_FLAG]: on,
  };
  window.dispatchEvent(new Event(FEATURE_FLAGS_CHANGED_EVENT));
}

const PROJECT_RESPONSE = {
  id: 'proj-1',
  name: 'Test Project',
  slug: 'test-project',
  _capabilities: { actions: ['update', 'manage'] },
};

function account(id: string): Record<string, unknown> {
  return {
    id,
    scope: 'project',
    scopeId: 'proj-1',
    email: `${id}@example.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    createdBy: 'user-1',
    createdAt: '2026-01-01T00:00:00Z',
  };
}

const BROKERS = [
  {
    id: 'legacy-1',
    name: 'Legacy Profile Broker',
    slug: 'legacy-1',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    profiles: [{ name: 'k8s-a', type: 'kubernetes', available: true }],
  },
  {
    id: 'flat-cr',
    name: 'Cloud Run US',
    slug: 'cloud-run-us',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    runtimeTarget: { type: 'cloudrun', region: 'us-central1' },
  },
  {
    id: 'flat-k8s',
    name: 'GKE Agents',
    slug: 'gke-agents',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    runtimeTarget: { type: 'kubernetes', namespace: 'agents' },
  },
];

interface FetchOpts {
  settings?: Record<string, unknown>;
  puts?: Array<Record<string, unknown>>;
  putResponse?: { status: number; body: unknown };
  actions?: string[];
}

function stubFetch(opts: FetchOpts = {}): void {
  const json = (body: unknown, status = 200): Promise<Response> =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(requestUrl(input), 'http://hub.example.com');
      const path = url.pathname;
      if (init?.method === 'PUT' && path.endsWith('/settings') && typeof init.body === 'string') {
        const body = JSON.parse(init.body) as Record<string, unknown>;
        opts.puts?.push(body);
        if (opts.putResponse) return json(opts.putResponse.body, opts.putResponse.status);
        return json(body);
      }
      if (path.includes('/runtime-brokers')) return json({ brokers: BROKERS });
      if (path.endsWith('/gcp-service-accounts')) {
        return json({ items: [account('sa-a'), account('sa-b'), account('sa-p')] });
      }
      if (/\/api\/v1\/projects\/[^/]+$/.test(path)) {
        return json(
          opts.actions
            ? { ...PROJECT_RESPONSE, _capabilities: { actions: opts.actions } }
            : PROJECT_RESPONSE
        );
      }
      if (path.endsWith('/settings/resolved')) {
        return json({ projectId: 'proj-1', project: opts.settings ?? {}, settings: {} });
      }
      if (path.endsWith('/settings')) return json(opts.settings ?? {});
      return json({});
    })
  );
}

interface Internals {
  handleSaveConfig: () => Promise<void>;
  settingsError: string | null;
  settingsSuccess: string | null;
  settings: Record<string, unknown> | null;
  configDefaultMaxDuration: string;
  brokerDefaultRows: Array<{ key: number; brokerId: string; serviceAccountId: string }>;
  addBrokerDefaultRow: () => void;
  brokers: unknown[];
  gcpServiceAccounts: unknown[];
  settingsLoading: boolean;
  updateComplete: Promise<unknown>;
}

function maybeSection(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector(`.broker-defaults[data-field="${F}"]`) ?? null;
}

function section(el: HTMLElement): Element {
  const s = maybeSection(el);
  if (!s) throw new Error('per-broker defaults section not rendered');
  return s;
}

function rowFor(el: HTMLElement, brokerId: string): Element | null {
  return section(el).querySelector(`.broker-default-row[data-broker="${brokerId}"]`);
}

function text(node: Element | null | undefined): string {
  return (node?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function setSelect(select: Element | null, value: string): void {
  const s = select as HTMLElement & { value: string };
  s.value = value;
  s.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
}

async function mount(opts: FetchOpts = {}): Promise<HTMLElement & Internals> {
  stubFetch(opts);
  const el = document.createElement('scion-page-project-settings') as InstanceType<
    typeof ScionPageProjectSettings
  > &
    Internals;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  for (let i = 0; i < 200; i++) {
    await el.updateComplete;
    if (
      el.settings !== null &&
      !el.settingsLoading &&
      el.brokers.length >= 3 &&
      el.gcpServiceAccounts.length >= 3 &&
      el.shadowRoot?.querySelector('sl-tab-panel[name="auth-security"]')
    ) {
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  await el.updateComplete;
  return el;
}

describe('project-settings: Per Runtime Broker default service accounts', () => {
  let element: (HTMLElement & Internals) | null = null;

  beforeAll(async () => {
    stubFetch();
    await import('./project-settings.js');
  }, 30_000);

  beforeEach(() => {
    setFlatBrokersFlag(true);
  });

  afterEach(() => {
    element?.remove();
    element = null;
    delete window.__SCION_FEATURES__;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('hides Per Runtime Broker and omits defaultGCPIdentityServiceAccountIDByBroker on save when hub.flat_runtime_brokers is off', async () => {
    setFlatBrokersFlag(false);
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({
      settings: {
        defaultMaxDuration: '1h',
        [F]: { 'flat-cr': 'sa-a' },
      },
      puts,
    });
    expect(maybeSection(element)).toBeNull();

    // Saving an unrelated project setting MUST omit defaultGCPIdentityServiceAccountIDByBroker
    // (never send {}) so stored per-broker entries are preserved and Hub 412 is not triggered.
    element.configDefaultMaxDuration = '2h';
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect(puts[0]!.defaultMaxDuration).toBe('2h');
    expect(F in puts[0]!).toBe(false);
  });

  it('dynamically shows and hides Per Runtime Broker on FEATURE_FLAGS_CHANGED_EVENT', async () => {
    setFlatBrokersFlag(false);
    element = await mount({ settings: { [F]: { 'flat-cr': 'sa-a' } } });
    expect(maybeSection(element)).toBeNull();

    setFlatBrokersFlag(true);
    await element.updateComplete;
    expect(maybeSection(element)).not.toBeNull();
    expect(rowFor(element, 'flat-cr')).not.toBeNull();
  });

  it('renders only flat Runtime Brokers (excluding legacy profile brokers) and shows effective identity per flat broker', async () => {
    element = await mount({
      settings: {
        defaultGCPIdentityMode: 'assign',
        defaultGCPIdentityServiceAccountID: 'sa-p',
        [F]: { 'flat-cr': 'sa-a' },
      },
    });
    expect(text(section(element).querySelector('#broker-defaults-label'))).toContain(
      'Per Runtime Broker'
    );
    const row = rowFor(element, 'flat-cr');
    expect(row).not.toBeNull();
    expect(row!.querySelector('.broker-default-sa')?.getAttribute('value')).toBe('sa-a');

    // Legacy profile broker must NOT appear in the broker options or effective table.
    const optionValues = Array.from(row!.querySelectorAll('.broker-default-broker sl-option')).map(
      (o) => o.getAttribute('value')
    );
    expect(optionValues).toEqual(['flat-cr', 'flat-k8s']);
    expect(optionValues).not.toContain('legacy-1');

    const cells = Array.from(
      section(element).querySelectorAll('.broker-effective-table tbody tr')
    ).map((tr) => text(tr));
    expect(cells).toEqual([
      "Cloud Run US Assign sa-a@example.com (this broker's default)",
      'GKE Agents Assign sa-p@example.com (project default)',
    ]);
  });

  it('omits the map from a save that does not touch per-broker rows', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { 'flat-cr': 'sa-a' } }, puts });
    await element.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect(F in puts[0]!).toBe(false);
  });

  it('adds, edits and removes per-broker entries, and sends {} when all are removed', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { 'flat-cr': 'sa-a' } }, puts });

    // Edit flat-cr, add flat-k8s (only remaining flat broker is preselected).
    setSelect(rowFor(element, 'flat-cr')!.querySelector('.broker-default-sa'), 'sa-b');
    await element.updateComplete;
    element.addBrokerDefaultRow();
    await element.updateComplete;
    expect(element.brokerDefaultRows.map((r) => r.brokerId)).toEqual(['flat-cr', 'flat-k8s']);
    setSelect(rowFor(element, 'flat-k8s')!.querySelector('.broker-default-sa'), 'sa-p');
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({ 'flat-cr': 'sa-b', 'flat-k8s': 'sa-p' });
    expect(element.settingsError).toBeNull();
    await element.updateComplete;

    // Remove both rows: save sends {} to clear all per-broker defaults.
    for (const id of ['flat-cr', 'flat-k8s']) {
      (rowFor(element, id)!.querySelector('.broker-default-remove') as HTMLElement).click();
      await element.updateComplete;
    }
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({});
  });

  it('renders a stored entry for a deleted/unlinked broker ID and lets an admin remove it', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { 'gone-broker-id': 'sa-a' } }, puts });
    const row = rowFor(element, 'gone-broker-id')!;
    expect(row).not.toBeNull();
    expect(text(row.querySelector('sl-option[value="gone-broker-id"]'))).toContain(
      '(not linked to this project)'
    );
    expect(text(row)).toContain(
      'Runtime Broker "gone-broker-id" is no longer linked to this project.'
    );

    (row.querySelector('.broker-default-remove') as HTMLElement).click();
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({});
  });

  it('blocks save with an incomplete row or more than 64 entries', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ puts });
    element.addBrokerDefaultRow();
    await element.updateComplete;
    await element.handleSaveConfig();
    await element.updateComplete;
    expect(puts).toHaveLength(0);
    expect(element.settingsError).toMatch(/incomplete per-broker default/);
    expect(section(element).querySelector('.broker-default-problem')).not.toBeNull();

    // 65 complete rows exceed the 64-entry cap.
    element.brokerDefaultRows = Array.from({ length: 65 }, (_, i) => ({
      key: i + 1,
      brokerId: `flat-${i}`,
      serviceAccountId: 'sa-a',
    }));
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts).toHaveLength(0);
    expect(element.settingsError).toContain('64');
  });

  it('shows inline per-broker validation errors and save warnings on their row', async () => {
    const errMsg = `${F}[flat-cr]: Runtime Broker "flat-cr" not found`;
    element = await mount({
      putResponse: { status: 400, body: { error: { code: 'invalid_request', message: errMsg } } },
    });
    element.addBrokerDefaultRow();
    await element.updateComplete;
    setSelect(
      section(element).querySelector('.broker-default-row .broker-default-broker'),
      'flat-cr'
    );
    await element.updateComplete;
    setSelect(rowFor(element, 'flat-cr')!.querySelector('.broker-default-sa'), 'sa-a');
    await element.updateComplete;

    vi.spyOn(console, 'error').mockImplementation(() => {});
    await element.handleSaveConfig();
    await element.updateComplete;
    expect(text(rowFor(element, 'flat-cr')!.querySelector('.broker-default-error'))).toBe(errMsg);
    expect(element.settingsError).toBe(errMsg);
  });

  it('shows save response warnings next to their row without duplicating into profile warnings', async () => {
    const warning = `${F}[flat-cr]: service account warning for flat-cr`;
    element = await mount({
      putResponse: { status: 200, body: { [F]: { 'flat-cr': 'sa-a' }, warnings: [warning] } },
    });
    element.addBrokerDefaultRow();
    await element.updateComplete;
    setSelect(
      section(element).querySelector('.broker-default-row .broker-default-broker'),
      'flat-cr'
    );
    await element.updateComplete;
    setSelect(rowFor(element, 'flat-cr')!.querySelector('.broker-default-sa'), 'sa-a');
    await element.updateComplete;

    await element.handleSaveConfig();
    await element.updateComplete;
    expect(text(rowFor(element, 'flat-cr')!.querySelector('.broker-default-warning'))).toBe(
      warning
    );
    expect(element.shadowRoot!.querySelectorAll('.profile-default-warning')).toHaveLength(0);
    expect(element.settingsSuccess).toMatch(/with warnings/);
  });

  it('disables controls for a caller who cannot manage the project', async () => {
    element = await mount({ settings: { [F]: { 'flat-cr': 'sa-a' } }, actions: ['read'] });
    const row = rowFor(element, 'flat-cr')!;
    expect(row.querySelector('.broker-default-broker')!.hasAttribute('disabled')).toBe(true);
    expect(row.querySelector('.broker-default-sa')!.hasAttribute('disabled')).toBe(true);
    expect(row.querySelector('.broker-default-remove')!.hasAttribute('disabled')).toBe(true);
    expect(section(element).querySelector('.broker-default-add')!.hasAttribute('disabled')).toBe(
      true
    );
  });
});
