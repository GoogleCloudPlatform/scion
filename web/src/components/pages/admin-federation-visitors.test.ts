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
 * Admin > Federation (ptone/scion#4603): the per-issuer visitor access
 * switch (allow_visitor_grants), shown for hub and user issuers while the
 * federation.project_visitors experiment is on; the removed Default Role
 * field; and the issuer fields the dialog does not edit, which a save keeps
 * as stored while the hub accepts them for the saved issuer type. The page
 * saves through PUT /api/v1/admin/server-config.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { ALLOW_VISITOR_GRANTS_LABEL } from './admin-federation.js';

const FLAG = 'federation.project_visitors';

// Shapes the hub accepts (pkg/config/federation_config.go): allowed_domains
// on a hub issuer with accept_user_tokens, and allowed_gcp_projects and
// allowed_domains on the Google user issuer with an expected_audience.
const ISSUERS = [
  {
    issuer_url: 'https://hub-a.example.com',
    issuer_type: 'hub',
    accept_user_tokens: true,
    allowed_domains: ['corp.example'],
  },
  {
    issuer_url: 'https://idp.example.com',
    issuer_type: 'user',
    default_role: 'viewer',
    allow_visitor_grants: true,
  },
  {
    issuer_url: 'https://accounts.google.com',
    issuer_type: 'user',
    expected_audience: 'scion-hub',
    allowed_gcp_projects: ['my-project'],
    allowed_domains: ['corp.example'],
  },
  { issuer_url: 'https://sa.example.com', issuer_type: 'service_account' },
];

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

let puts: Array<{ federation: { trusted_issuers: Array<Record<string, unknown>> } }> = [];

function fetchHandler(url: string | URL | Request, init?: RequestInit): Promise<Response> {
  const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
  if (init?.method === 'PUT' && path.includes('/api/v1/admin/server-config')) {
    puts.push(JSON.parse(init.body as string));
    return json({});
  }
  if (path.includes('/api/v1/admin/server-config')) {
    return json({
      federation: {
        enabled: true,
        algorithms: ['RS256'],
        refresh_interval: '1h',
        debounce_interval: '5s',
        trusted_issuers: ISSUERS,
      },
    });
  }
  return json({});
}

function setFlag(value: boolean | undefined): void {
  const bag = { ...(window.__SCION_FEATURES__ ?? {}) };
  if (value === undefined) delete bag[FLAG];
  else bag[FLAG] = value;
  window.__SCION_FEATURES__ = bag;
}

interface PageInternals extends HTMLElement {
  updateComplete: Promise<boolean>;
  editingIssuer: Record<string, unknown> | null;
  openEditDialog(index: number): void;
  openAddDialog(): void;
  handleSaveIssuer(): void;
}

let element: PageInternals | null = null;

async function mount(): Promise<PageInternals> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-admin-federation') as unknown as PageInternals;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 100));
  await el.updateComplete;
  element = el;
  return el;
}

async function openEdit(el: PageInternals, index: number): Promise<void> {
  el.openEditDialog(index);
  await el.updateComplete;
}

async function saveDialog(el: PageInternals): Promise<Record<string, unknown>[]> {
  el.handleSaveIssuer();
  await vi.waitFor(() => expect(puts).toHaveLength(1));
  return puts[0]!.federation.trusted_issuers;
}

function q(el: PageInternals, sel: string): HTMLElement | null {
  return el.shadowRoot!.querySelector<HTMLElement>(sel);
}

function dialogText(el: PageInternals): string {
  return (q(el, '.dialog-form')?.textContent ?? '').replace(/\s+/g, ' ');
}

describe('scion-page-admin-federation: visitor access switch (ptone/scion#4603)', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler));
    await import('./admin-federation.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    puts = [];
    setFlag(undefined);
    vi.restoreAllMocks();
  });

  it('shows the switch for hub and user issuers while the experiment is on', async () => {
    setFlag(true);
    const el = await mount();

    await openEdit(el, 0);
    expect(q(el, '[data-allow-visitor-grants]')).not.toBeNull();
    expect(dialogText(el)).toContain(ALLOW_VISITOR_GRANTS_LABEL);
    expect(q(el, '[data-allow-visitor-grants]')?.hasAttribute('checked')).toBe(false);

    await openEdit(el, 1);
    expect(q(el, '[data-allow-visitor-grants]')?.hasAttribute('checked')).toBe(true);
  });

  it('does not show the switch for a service_account issuer', async () => {
    setFlag(true);
    const el = await mount();
    await openEdit(el, 3);
    expect(q(el, '[data-allow-visitor-grants]')).toBeNull();
  });

  it('does not show the switch while the experiment is off', async () => {
    setFlag(false);
    const el = await mount();
    await openEdit(el, 1);
    expect(q(el, '[data-allow-visitor-grants]')).toBeNull();
  });

  it('turning the switch on saves allow_visitor_grants: true on that issuer', async () => {
    setFlag(true);
    const el = await mount();
    await openEdit(el, 0);
    const sw = q(el, '[data-allow-visitor-grants]') as HTMLInputElement;
    sw.checked = true;
    sw.dispatchEvent(new Event('sl-change'));

    const issuers = await saveDialog(el);
    expect(issuers[0]).toEqual({
      issuer_url: 'https://hub-a.example.com',
      issuer_type: 'hub',
      accept_user_tokens: true,
      allowed_domains: ['corp.example'],
      allow_visitor_grants: true,
    });
  });

  it('turning the switch off drops allow_visitor_grants from the saved issuer', async () => {
    setFlag(true);
    const el = await mount();
    await openEdit(el, 1);
    const sw = q(el, '[data-allow-visitor-grants]') as HTMLInputElement;
    sw.checked = false;
    sw.dispatchEvent(new Event('sl-change'));

    const issuers = await saveDialog(el);
    expect(issuers[1]).not.toHaveProperty('allow_visitor_grants');
  });

  it('a save with the experiment off keeps a stored allow_visitor_grants', async () => {
    setFlag(false);
    const el = await mount();
    await openEdit(el, 1);

    const issuers = await saveDialog(el);
    expect(issuers[1]).toHaveProperty('allow_visitor_grants', true);
  });

  it('changing an issuer to service_account drops allow_visitor_grants', async () => {
    setFlag(true);
    const el = await mount();
    await openEdit(el, 1);
    el.editingIssuer = { ...el.editingIssuer!, issuer_type: 'service_account' };
    await el.updateComplete;

    const issuers = await saveDialog(el);
    expect(issuers[1]).not.toHaveProperty('allow_visitor_grants');
  });
});

describe('scion-page-admin-federation: Default Role removed (ptone/scion#4603)', () => {
  afterEach(() => {
    element?.remove();
    element = null;
    puts = [];
    setFlag(undefined);
    vi.restoreAllMocks();
  });

  it('the user issuer dialog has no Default Role field', async () => {
    setFlag(true);
    const el = await mount();
    await openEdit(el, 1);

    expect(dialogText(el)).not.toContain('Default Role');
    expect(dialogText(el)).not.toContain('Select a default role');
  });

  it('a new user issuer has no Default Role field and saves no default_role', async () => {
    const el = await mount();
    el.openAddDialog();
    el.editingIssuer = { issuer_url: 'https://new.example.com', issuer_type: 'user' };
    await el.updateComplete;
    expect(dialogText(el)).not.toContain('Default Role');

    const issuers = await saveDialog(el);
    expect(issuers[4]).toEqual({ issuer_url: 'https://new.example.com', issuer_type: 'user' });
  });

  it('editing keeps the fields the dialog does not edit as stored', async () => {
    const el = await mount();
    await openEdit(el, 1);
    const issuers = await saveDialog(el);

    expect(issuers[1]).toMatchObject({
      issuer_url: 'https://idp.example.com',
      issuer_type: 'user',
      default_role: 'viewer',
    });
    expect(issuers.length).toBe(4);
  });

  it('editing a hub issuer keeps accept_user_tokens and allowed_domains', async () => {
    const el = await mount();
    await openEdit(el, 0);
    const issuers = await saveDialog(el);

    expect(issuers[0]).toEqual({
      issuer_url: 'https://hub-a.example.com',
      issuer_type: 'hub',
      accept_user_tokens: true,
      allowed_domains: ['corp.example'],
    });
  });

  it('editing the Google user issuer keeps allowed_gcp_projects and allowed_domains', async () => {
    const el = await mount();
    await openEdit(el, 2);
    const issuers = await saveDialog(el);

    expect(issuers[2]).toEqual({
      issuer_url: 'https://accounts.google.com',
      issuer_type: 'user',
      expected_audience: 'scion-hub',
      allowed_gcp_projects: ['my-project'],
      allowed_domains: ['corp.example'],
    });
  });
});

describe('scion-page-admin-federation: issuer type change (ptone/scion#4603)', () => {
  afterEach(() => {
    element?.remove();
    element = null;
    puts = [];
    setFlag(undefined);
    vi.restoreAllMocks();
  });

  it('changing a hub issuer to user drops accept_user_tokens and allowed_domains', async () => {
    const el = await mount();
    await openEdit(el, 0);
    el.editingIssuer = { ...el.editingIssuer!, issuer_type: 'user' };
    await el.updateComplete;

    const issuers = await saveDialog(el);
    expect(issuers[0]).toEqual({ issuer_url: 'https://hub-a.example.com', issuer_type: 'user' });
  });

  it('changing a hub issuer to service_account drops accept_user_tokens', async () => {
    const el = await mount();
    await openEdit(el, 0);
    el.editingIssuer = { ...el.editingIssuer!, issuer_type: 'service_account' };
    await el.updateComplete;

    const issuers = await saveDialog(el);
    expect(issuers[0]).not.toHaveProperty('accept_user_tokens');
    expect(issuers[0]).not.toHaveProperty('allowed_domains');
  });

  it('changing the Google user issuer to service_account drops its Google fields', async () => {
    const el = await mount();
    await openEdit(el, 2);
    el.editingIssuer = { ...el.editingIssuer!, issuer_type: 'service_account' };
    await el.updateComplete;

    const issuers = await saveDialog(el);
    expect(issuers[2]).toEqual({
      issuer_url: 'https://accounts.google.com',
      issuer_type: 'service_account',
      expected_audience: 'scion-hub',
    });
  });

  it('changing a user issuer to hub drops its stored default_role', async () => {
    const el = await mount();
    await openEdit(el, 1);
    el.editingIssuer = { ...el.editingIssuer!, issuer_type: 'hub' };
    await el.updateComplete;

    const issuers = await saveDialog(el);
    expect(issuers[1]).not.toHaveProperty('default_role');
  });
});
