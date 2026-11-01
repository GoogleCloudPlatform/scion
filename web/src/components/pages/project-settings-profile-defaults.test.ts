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
 * ptone/scion#3947: the Project Settings editor for the per-profile default
 * GCP service account map (defaultGCPIdentityServiceAccountIDByProfile).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionPageProjectSettings } from './project-settings.js';

const F = 'defaultGCPIdentityServiceAccountIDByProfile';

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
    id: 'broker-1',
    name: 'broker-1',
    slug: 'broker-1',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    profiles: [
      { name: 'k8s-a', type: 'kubernetes', available: true },
      { name: 'k8s-b', type: 'kubernetes', available: true },
    ],
  },
];

interface FetchOpts {
  settings?: Record<string, unknown>;
  puts?: Array<Record<string, unknown>>;
  /** Replaces the PUT response (status and JSON body). */
  putResponse?: { status: number; body: unknown };
  /** The caller's project capabilities (default: update and manage). */
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
  profileDefaultRows: Array<{ key: number; profile: string; serviceAccountId: string }>;
  addProfileDefaultRow: () => void;
  updateComplete: Promise<unknown>;
}

function section(el: HTMLElement): Element {
  const s = el.shadowRoot?.querySelector(`.profile-defaults[data-field="${F}"]`);
  if (!s) throw new Error('per-profile defaults section not rendered');
  return s;
}

function rowFor(el: HTMLElement, profile: string): Element | null {
  return section(el).querySelector(`.profile-default-row[data-profile="${profile}"]`);
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
  // Wait until brokers and accounts have loaded.
  for (let i = 0; i < 200; i++) {
    await el.updateComplete;
    const effective = el.shadowRoot?.querySelectorAll('.profile-effective-table tbody tr').length;
    if ((effective ?? 0) >= 2) break;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  await el.updateComplete;
  return el;
}

describe('project-settings: per-profile default service accounts', () => {
  let element: (HTMLElement & Internals) | null = null;

  beforeAll(async () => {
    stubFetch();
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('renders stored entries and the effective identity per profile', async () => {
    element = await mount({
      settings: {
        defaultGCPIdentityMode: 'assign',
        defaultGCPIdentityServiceAccountID: 'sa-p',
        [F]: { 'k8s-a': 'sa-a' },
      },
    });
    const row = rowFor(element, 'k8s-a');
    expect(row).not.toBeNull();
    // Shoelace is not registered in tests; the page binds value as an attribute.
    expect(row!.querySelector('.profile-default-sa')?.getAttribute('value')).toBe('sa-a');

    const cells = Array.from(
      element.shadowRoot!.querySelectorAll('.profile-effective-table tbody tr')
    ).map((tr) => text(tr));
    expect(cells).toEqual([
      "k8s-a Assign sa-a@example.com (this profile's default)",
      'k8s-b Assign sa-p@example.com (project default)',
    ]);
    // k8s-b has no default while its sibling has one.
    expect(text(section(element).querySelector('.profile-default-sibling-warning'))).toContain(
      'Kubernetes profile "k8s-b" has no per-profile default'
    );
  });

  it('leaves the map out of a save that does not change it', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { 'k8s-a': 'sa-a' } }, puts });
    await element.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect(F in puts[0]!).toBe(false);
  });

  it('adds, changes and removes entries, and sends {} when all are removed', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { 'k8s-a': 'sa-a' } }, puts });

    // Change k8s-a, add k8s-b.
    setSelect(rowFor(element, 'k8s-a')!.querySelector('.profile-default-sa'), 'sa-b');
    await element.updateComplete;
    element.addProfileDefaultRow();
    await element.updateComplete;
    // The only free profile is preselected.
    expect(element.profileDefaultRows.map((r) => r.profile)).toEqual(['k8s-a', 'k8s-b']);
    setSelect(rowFor(element, 'k8s-b')!.querySelector('.profile-default-sa'), 'sa-p');
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({ 'k8s-a': 'sa-b', 'k8s-b': 'sa-p' });
    expect(element.settingsError).toBeNull();
    await element.updateComplete;

    // Remove both rows: the save clears the map.
    for (const p of ['k8s-a', 'k8s-b']) {
      (rowFor(element, p)!.querySelector('.profile-default-remove') as HTMLElement).click();
      await element.updateComplete;
    }
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({});
  });

  it('blocks a save with an incomplete row', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ puts });
    element.addProfileDefaultRow();
    await element.updateComplete;
    await element.handleSaveConfig();
    await element.updateComplete;
    expect(puts).toHaveLength(0);
    expect(element.settingsError).toMatch(/incomplete per-profile default/);
    expect(section(element).querySelector('.profile-default-problem')).not.toBeNull();
  });

  it('shows a per-profile validation error inline', async () => {
    const message = `${F}[k8s-a]: a service account ID is required; remove the entry to clear it`;
    element = await mount({
      putResponse: { status: 400, body: { error: { code: 'invalid_request', message } } },
    });
    element.addProfileDefaultRow();
    await element.updateComplete;
    setSelect(
      section(element).querySelector('.profile-default-row .profile-default-profile'),
      'k8s-a'
    );
    await element.updateComplete;
    setSelect(rowFor(element, 'k8s-a')!.querySelector('.profile-default-sa'), 'sa-a');
    await element.updateComplete;

    vi.spyOn(console, 'error').mockImplementation(() => {});
    await element.handleSaveConfig();
    await element.updateComplete;
    expect(text(rowFor(element, 'k8s-a')!.querySelector('.profile-default-error'))).toBe(message);
    expect(element.settingsError).toBe(message);
  });

  it('shows the save response warnings next to their row', async () => {
    const warning = `${F}[k8s-a]: GCP service account sa-a@example.com is not mapped to a Kubernetes ServiceAccount on profile "k8s-a"`;
    element = await mount({
      putResponse: { status: 200, body: { [F]: { 'k8s-a': 'sa-a' }, warnings: [warning] } },
    });
    element.addProfileDefaultRow();
    await element.updateComplete;
    setSelect(
      section(element).querySelector('.profile-default-row .profile-default-profile'),
      'k8s-a'
    );
    await element.updateComplete;
    setSelect(rowFor(element, 'k8s-a')!.querySelector('.profile-default-sa'), 'sa-a');
    await element.updateComplete;

    await element.handleSaveConfig();
    await element.updateComplete;
    expect(text(rowFor(element, 'k8s-a')!.querySelector('.profile-default-warning'))).toBe(warning);
    expect(element.settingsSuccess).toMatch(/with warnings/);
  });

  it('disables every control for a caller who cannot manage the project', async () => {
    element = await mount({ settings: { [F]: { 'k8s-a': 'sa-a' } }, actions: ['read'] });
    const row = rowFor(element, 'k8s-a')!;
    expect(row.querySelector('.profile-default-profile')!.hasAttribute('disabled')).toBe(true);
    expect(row.querySelector('.profile-default-sa')!.hasAttribute('disabled')).toBe(true);
    expect(row.querySelector('.profile-default-remove')!.hasAttribute('disabled')).toBe(true);
    expect(section(element).querySelector('.profile-default-add')!.hasAttribute('disabled')).toBe(
      true
    );
  });

  it('marks a stored entry for an unreported profile and lets it be removed', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount({ settings: { [F]: { gone: 'sa-a' } }, puts });
    const row = rowFor(element, 'gone')!;
    expect(text(row.querySelector('sl-option[value="gone"]'))).toContain(
      '(not reported by any broker)'
    );
    expect(text(row)).toContain('No broker linked to this project reports profile "gone".');
    // The effective view lists it too, with its own default.
    expect(
      text(element.shadowRoot!.querySelector('.profile-effective-table tr[data-profile="gone"]'))
    ).toContain("this profile's default");

    (row.querySelector('.profile-default-remove') as HTMLElement).click();
    await element.updateComplete;
    await element.handleSaveConfig();
    expect(puts.at(-1)![F]).toEqual({});
  });

  it('places structured details.errors entries on their rows', async () => {
    element = await mount({
      settings: { [F]: { 'k8s-a': 'sa-a' } },
      putResponse: {
        status: 422,
        body: {
          error: {
            code: 'validation_failed',
            message: 'validation failed',
            details: {
              field: `${F}.k8s-a`,
              errors: [{ field: `${F}.k8s-a`, message: 'account not usable here' }],
            },
          },
        },
      },
    });
    setSelect(rowFor(element, 'k8s-a')!.querySelector('.profile-default-sa'), 'sa-b');
    await element.updateComplete;
    vi.spyOn(console, 'error').mockImplementation(() => {});
    await element.handleSaveConfig();
    await element.updateComplete;
    expect(text(rowFor(element, 'k8s-a')!.querySelector('.profile-default-error'))).toBe(
      'account not usable here'
    );
  });
});
