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
 * Project Visitors editor (ptone/scion#4603): list, add by email and by
 * domain, change expiry, revoke, and the issuer field for hub admins and for
 * other owners. apiFetch, the admin status, the name lookup and the confirm
 * dialog are mocked; request bodies mirror pkg/hub.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { apiFetch } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import type { AdminStatus } from '../../lib/admin-permissions.js';
import {
  ISSUER_METADATA_PATH_ERROR,
  type ProjectVisitor,
  type ProjectVisitorRule,
} from '../../shared/project-visitors.js';
import { showConfirm } from './confirm-dialog.js';
import {
  ISSUER_TEXT_HINT,
  NO_VISITOR_ISSUERS_MESSAGE,
  ScionProjectVisitorsEditor,
  VISITORS_UNAVAILABLE_MESSAGE,
} from './project-visitors-editor.js';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

vi.mock('../../client/admin-status.js', () => ({
  loadAdminStatus: vi.fn(() => Promise.resolve(null)),
}));

vi.mock('../../client/principal-names.js', async (orig) => ({
  ...(await orig<typeof import('../../client/principal-names.js')>()),
  principalName: vi.fn((_kind: string, ref: string) =>
    Promise.resolve(ref === 'u-owner' ? 'Olive Owner' : '')
  ),
}));

vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const ISSUER = 'https://hub-a.example.com';

/** Instants relative to the test run, so window-based labels do not age. */
const DAY_MS = 24 * 60 * 60 * 1000;
const PAST = new Date(Date.now() - 30 * DAY_MS).toISOString();
const YESTERDAY = new Date(Date.now() - DAY_MS).toISOString();
const TOMORROW = new Date(Date.now() + DAY_MS).toISOString();
const FUTURE = new Date(Date.now() + 30 * DAY_MS).toISOString();

const ALICE: ProjectVisitor = {
  id: 'v-alice',
  bindingId: 'b-alice',
  email: 'alice@corp.example',
  issuer: ISSUER,
  status: 'active',
  issuerAccepted: true,
  grantedBy: 'u-owner',
  notBefore: PAST,
  expiresAt: FUTURE,
  createdAt: '2026-10-01T00:00:00Z',
  source: 'email',
};

const BOB_PENDING: ProjectVisitor = {
  id: 'v-bob',
  bindingId: 'b-bob',
  email: 'bob@corp.example',
  issuer: ISSUER,
  status: 'pending',
  issuerAccepted: true,
  grantedBy: 'u-owner',
  createdAt: '2026-10-01T00:00:00Z',
  source: 'email',
};

const CAROL_DISABLED: ProjectVisitor = {
  ...ALICE,
  id: 'v-carol',
  bindingId: 'b-carol',
  email: 'carol@other.example',
  issuer: 'https://off.example.com',
  issuerAccepted: false,
};

const RULE: ProjectVisitorRule = {
  id: 'r-corp',
  issuer: ISSUER,
  domain: 'corp.example',
  issuerAccepted: true,
  createdBy: 'u-owner',
  createdAt: '2026-10-01T00:00:00Z',
};

const DAVE_FROM_RULE: ProjectVisitor = {
  ...ALICE,
  id: 'v-dave',
  bindingId: 'b-dave',
  email: 'dave@corp.example',
  source: 'rule',
  sourceRuleId: 'r-corp',
};

const ERIN_EXPIRED: ProjectVisitor = {
  ...ALICE,
  id: 'v-erin',
  bindingId: 'b-erin',
  email: 'erin@corp.example',
  expiresAt: YESTERDAY,
};

const FRANK_NOT_STARTED: ProjectVisitor = {
  ...ALICE,
  id: 'v-frank',
  bindingId: 'b-frank',
  email: 'frank@corp.example',
  notBefore: TOMORROW,
};

const RULE_EXPIRED: ProjectVisitorRule = {
  ...RULE,
  id: 'r-old',
  domain: 'old.example',
  expiresAt: YESTERDAY,
};

const RULE_NOT_STARTED: ProjectVisitorRule = {
  ...RULE,
  id: 'r-later',
  domain: 'later.example',
  notBefore: TOMORROW,
  expiresAt: FUTURE,
};

const HUB_ADMIN: AdminStatus = {
  isAdmin: true,
  isSuperAdmin: false,
  permissions: ['hub.config.read'],
};

function jsonResponse(status: number, body: unknown): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Calls {
  method: string;
  url: string;
  body: unknown;
}

interface Routes {
  visitors?: ProjectVisitor[];
  rules?: ProjectVisitorRule[];
  listStatus?: number;
  /** trusted_issuers of GET /api/v1/admin/server-config. */
  issuers?: Array<Record<string, unknown>>;
  /** federation.enabled of GET /api/v1/admin/server-config (default true). */
  federationEnabled?: boolean;
  /** Response of a POST, PATCH or DELETE. */
  write?: { status: number; body: unknown };
}

function routeApi(routes: Routes): void {
  vi.mocked(apiFetch).mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    if (method !== 'GET') {
      const w = routes.write ?? { status: method === 'DELETE' ? 204 : 200, body: {} };
      return Promise.resolve(jsonResponse(w.status, w.body));
    }
    if (url === '/api/v1/admin/server-config') {
      return Promise.resolve(
        jsonResponse(200, {
          federation: {
            enabled: routes.federationEnabled ?? true,
            trusted_issuers: routes.issuers ?? [],
          },
        })
      );
    }
    if (routes.listStatus && routes.listStatus !== 200) {
      return Promise.resolve(jsonResponse(routes.listStatus, {}));
    }
    if (url.endsWith('/visitors/rules')) {
      return Promise.resolve(jsonResponse(200, { rules: routes.rules ?? [] }));
    }
    if (url.endsWith('/visitors')) {
      return Promise.resolve(jsonResponse(200, { visitors: routes.visitors ?? [] }));
    }
    return Promise.resolve(jsonResponse(404, {}));
  });
}

function calls(): Calls[] {
  return vi.mocked(apiFetch).mock.calls.map(([url, init]) => ({
    method: init?.method ?? 'GET',
    url,
    body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
  }));
}

interface EditorInternals extends HTMLElement {
  updateComplete: Promise<boolean>;
  loading: boolean;
  addOpen: boolean;
  addKind: 'email' | 'domain';
  addIssuer: string;
  addValue: string;
  addExpiry: string;
  addError: string | null;
  issuerChoices: { mode: 'picker' | 'text'; issuers: string[] } | null;
  extendTarget: unknown;
  extendExpiry: string;
  extendError: string | null;
  openAddDialog(): Promise<void>;
  submitAdd(): Promise<void>;
  submitExtend(): Promise<void>;
}

const mounted: HTMLElement[] = [];

async function mount(routes: Routes): Promise<EditorInternals> {
  routeApi(routes);
  const el = new ScionProjectVisitorsEditor();
  el.projectId = 'p-1';
  el.userId = 'u-me';
  document.body.appendChild(el);
  mounted.push(el);
  const i = el as unknown as EditorInternals;
  await vi.waitFor(() => expect(i.loading).toBe(false));
  await i.updateComplete;
  return i;
}

function q(el: EditorInternals, sel: string): HTMLElement | null {
  return el.shadowRoot!.querySelector<HTMLElement>(sel);
}

function text(el: Element | null): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

async function settle(el: EditorInternals): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await el.updateComplete;
}

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
  vi.mocked(loadAdminStatus).mockReset();
  vi.mocked(loadAdminStatus).mockResolvedValue(null);
  vi.mocked(showConfirm).mockClear();
});

afterEach(() => {
  while (mounted.length) mounted.pop()!.remove();
});

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

describe('scion-project-visitors-editor list', () => {
  it('loads grants and rules from the project visitor routes', async () => {
    await mount({ visitors: [ALICE], rules: [RULE] });
    const urls = calls().map((c) => c.url);
    expect(urls).toContain('/api/v1/projects/p-1/visitors');
    expect(urls).toContain('/api/v1/projects/p-1/visitors/rules');
    expect(urls).not.toContain('/api/v1/admin/server-config');
  });

  it('shows email or domain, issuer, status, granted by and expiry for each row', async () => {
    const el = await mount({
      visitors: [ALICE, BOB_PENDING, CAROL_DISABLED, DAVE_FROM_RULE],
      rules: [RULE],
    });
    await settle(el);

    const alice = q(el, '[data-visitor-row="v-alice"]');
    expect(text(alice)).toContain('alice@corp.example');
    expect(text(alice)).toContain(ISSUER);
    expect(text(alice?.querySelector('[data-visitor-status]') ?? null)).toBe('Active');
    expect(text(alice)).toContain('Olive Owner');
    expect(text(alice)).not.toContain('Never');

    const bob = q(el, '[data-visitor-row="v-bob"]');
    expect(text(bob?.querySelector('[data-visitor-status]') ?? null)).toBe('Pending');
    expect(text(bob)).toContain('Never');

    const carol = q(el, '[data-visitor-row="v-carol"]');
    expect(text(carol?.querySelector('[data-visitor-status]') ?? null)).toBe(
      'Issuer disabled by hub admin'
    );

    const rule = q(el, '[data-rule-row="r-corp"]');
    expect(text(rule)).toContain('@corp.example');
    expect(text(rule)).toContain('Olive Owner');
    expect(text(rule?.querySelector('[data-visitor-status]') ?? null)).toBe('Active');

    const dave = q(el, '[data-visitor-row="v-dave"]');
    expect(text(dave)).toContain('Through the rule @corp.example');
  });

  it('the status of grants and rules follows their window', async () => {
    const el = await mount({
      visitors: [ALICE, ERIN_EXPIRED, FRANK_NOT_STARTED],
      rules: [RULE, RULE_EXPIRED, RULE_NOT_STARTED],
    });
    const status = (sel: string): HTMLElement | null => q(el, `${sel} [data-visitor-status]`);

    expect(text(status('[data-visitor-row="v-alice"]'))).toBe('Active');
    expect(status('[data-visitor-row="v-alice"]')?.getAttribute('variant')).toBe('success');
    expect(text(status('[data-visitor-row="v-erin"]'))).toBe('Expired');
    expect(status('[data-visitor-row="v-erin"]')?.getAttribute('variant')).toBe('neutral');
    expect(text(status('[data-visitor-row="v-frank"]'))).toBe('Not yet active');

    expect(text(status('[data-rule-row="r-corp"]'))).toBe('Active');
    expect(text(status('[data-rule-row="r-old"]'))).toBe('Expired');
    expect(status('[data-rule-row="r-old"]')?.getAttribute('variant')).toBe('neutral');
    expect(text(status('[data-rule-row="r-later"]'))).toBe('Not yet active');
  });

  it('has no Last active column', async () => {
    const el = await mount({ visitors: [ALICE] });
    const headers = Array.from(el.shadowRoot!.querySelectorAll('th')).map((th) => text(th));
    expect(headers).toEqual(['Visitor', 'Issuer', 'Status', 'Granted by', 'Expires', 'Actions']);
  });

  it('offers no Change expiry on a visitor a rule added (it follows the rule)', async () => {
    const el = await mount({ visitors: [ALICE, DAVE_FROM_RULE], rules: [RULE] });
    expect(text(q(el, '[data-visitor-row="v-alice"] [data-action="extend"]'))).toBe(
      'Change expiry'
    );
    expect(q(el, '[data-visitor-row="v-dave"] [data-action="extend"]')).toBeNull();
    expect(q(el, '[data-visitor-row="v-dave"] [data-action="revoke"]')).not.toBeNull();
    expect(q(el, '[data-rule-row="r-corp"] [data-action="extend"]')).not.toBeNull();
  });

  it('shows the empty state with no grants and no rules', async () => {
    const el = await mount({});
    expect(q(el, '[data-visitors-empty]')).not.toBeNull();
  });

  it('a 404 from the hub says visitor access is not available', async () => {
    const el = await mount({ listStatus: 404 });
    expect(text(q(el, '[data-visitors-error]'))).toContain(VISITORS_UNAVAILABLE_MESSAGE);
  });
});

// ---------------------------------------------------------------------------
// Issuer field
// ---------------------------------------------------------------------------

describe('scion-project-visitors-editor issuer field', () => {
  it('an owner who is not a hub admin gets a text field and no admin request', async () => {
    const el = await mount({ visitors: [ALICE, CAROL_DISABLED] });
    await el.openAddDialog();
    await el.updateComplete;

    expect(q(el, '[data-issuer-picker]')).toBeNull();
    expect(q(el, '[data-issuer-input]')).not.toBeNull();
    expect(text(el.shadowRoot!.querySelector('.dialog-form'))).toContain(ISSUER_TEXT_HINT);
    // Suggestions come only from the project's own accepted issuers.
    const suggestions = text(q(el, '[data-issuer-suggestions]'));
    expect(suggestions).toContain(ISSUER);
    expect(suggestions).not.toContain('https://off.example.com');
    expect(calls().map((c) => c.url)).not.toContain('/api/v1/admin/server-config');
  });

  it('a hub admin picks from the issuers whose toggle is on', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(HUB_ADMIN);
    const el = await mount({
      issuers: [
        { issuer_url: ISSUER, issuer_type: 'hub', allow_visitor_grants: true },
        { issuer_url: 'https://off.example.com', issuer_type: 'hub' },
        {
          issuer_url: 'https://sa.example.com',
          issuer_type: 'service_account',
          allow_visitor_grants: true,
        },
      ],
    });
    await el.openAddDialog();
    await el.updateComplete;

    expect(q(el, '[data-issuer-input]')).toBeNull();
    const options = el.shadowRoot!.querySelectorAll('[data-issuer-picker] sl-option');
    expect(Array.from(options).map((o) => o.getAttribute('value'))).toEqual([ISSUER]);
    // A single choice is preselected.
    expect(el.addIssuer).toBe(ISSUER);
  });

  it('a hub admin with no issuer allowing visitors sees why and cannot submit', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(HUB_ADMIN);
    const el = await mount({ issuers: [{ issuer_url: ISSUER, issuer_type: 'hub' }] });
    await el.openAddDialog();
    await el.updateComplete;

    expect(text(q(el, '[data-no-visitor-issuers]'))).toBe(NO_VISITOR_ISSUERS_MESSAGE);
    expect(q(el, '[data-add-submit]')?.hasAttribute('disabled')).toBe(true);
  });

  it('the text field refuses an issuer that is not an https URL, without a request', async () => {
    const el = await mount({});
    await el.openAddDialog();
    el.addIssuer = 'http://hub-a.example.com';
    el.addValue = 'alice@corp.example';
    await el.submitAdd();
    await el.updateComplete;

    expect(el.addError).toBe('The issuer must be an https:// URL');
    expect(calls().filter((c) => c.method === 'POST')).toEqual([]);
  });

  it('the text field refuses a discovery document URL, without a request', async () => {
    const el = await mount({});
    await el.openAddDialog();
    el.addIssuer = `${ISSUER}/.well-known/openid-configuration`;
    el.addValue = 'alice@corp.example';
    await el.submitAdd();
    await el.updateComplete;

    expect(el.addError).toBe(ISSUER_METADATA_PATH_ERROR);
    expect(calls().filter((c) => c.method === 'POST')).toEqual([]);
  });

  it('a hub admin sees no issuer to pick while federation is off on the hub', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(HUB_ADMIN);
    const el = await mount({
      federationEnabled: false,
      issuers: [{ issuer_url: ISSUER, issuer_type: 'hub', allow_visitor_grants: true }],
    });
    await el.openAddDialog();
    await el.updateComplete;

    expect(text(q(el, '[data-no-visitor-issuers]'))).toBe(NO_VISITOR_ISSUERS_MESSAGE);
    expect(q(el, '[data-add-submit]')?.hasAttribute('disabled')).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Add, extend, revoke
// ---------------------------------------------------------------------------

describe('scion-project-visitors-editor actions', () => {
  it('adds a grant by email with POST /visitors, sending the issuer trimmed only', async () => {
    const el = await mount({});
    await el.openAddDialog();
    el.addIssuer = ` ${ISSUER}/realms/corp/ `;
    el.addValue = ' Alice@Corp.Example ';
    el.addExpiry = '';
    await el.submitAdd();

    const post = calls().find((c) => c.method === 'POST');
    expect(post).toEqual({
      method: 'POST',
      url: '/api/v1/projects/p-1/visitors',
      body: { issuer: `${ISSUER}/realms/corp/`, email: 'alice@corp.example' },
    });
    expect(el.addOpen).toBe(false);
  });

  it('adds a rule by domain with POST /visitors/rules and an expiry', async () => {
    const el = await mount({});
    await el.openAddDialog();
    el.addKind = 'domain';
    el.addIssuer = ISSUER;
    el.addValue = '@Corp.Example';
    el.addExpiry = '30';
    await el.submitAdd();

    const post = calls().find((c) => c.method === 'POST');
    expect(post?.url).toBe('/api/v1/projects/p-1/visitors/rules');
    const body = post?.body as Record<string, unknown>;
    expect(body['issuer']).toBe(ISSUER);
    expect(body['domain']).toBe('corp.example');
    expect(typeof body['expiresAt']).toBe('string');
  });

  it('shows the hub refusal text unchanged', async () => {
    const refusal = 'This hub does not accept visitors from that issuer';
    const el = await mount({
      write: {
        status: 400,
        body: {
          error: { code: 'validation_error', message: refusal, details: { field: 'issuer' } },
        },
      },
    });
    await el.openAddDialog();
    el.addIssuer = ISSUER;
    el.addValue = 'alice@corp.example';
    await el.submitAdd();
    await el.updateComplete;

    expect(el.addOpen).toBe(true);
    expect(text(q(el, '[data-add-error]'))).toBe(refusal);
  });

  it('extends a grant with PATCH, sending its notBefore back unchanged', async () => {
    const el = await mount({ visitors: [ALICE] });
    q(el, '[data-visitor-row="v-alice"] [data-action="extend"]')!.click();
    await el.updateComplete;
    el.extendExpiry = '';
    await el.submitExtend();

    const patch = calls().find((c) => c.method === 'PATCH');
    expect(patch).toEqual({
      method: 'PATCH',
      url: '/api/v1/projects/p-1/visitors/v-alice',
      body: { notBefore: PAST, expiresAt: null },
    });
    expect(el.extendTarget).toBeNull();
  });

  it('extends a rule with PATCH /visitors/rules/{id}', async () => {
    const el = await mount({ rules: [RULE] });
    q(el, '[data-rule-row="r-corp"] [data-action="extend"]')!.click();
    await el.updateComplete;
    el.extendExpiry = '90';
    await el.submitExtend();

    const patch = calls().find((c) => c.method === 'PATCH');
    expect(patch?.url).toBe('/api/v1/projects/p-1/visitors/rules/r-corp');
    const body = patch?.body as Record<string, unknown>;
    expect(body['notBefore']).toBeNull();
    expect(typeof body['expiresAt']).toBe('string');
  });

  it('shows the hub refusal of an extension unchanged', async () => {
    const refusal =
      'You can grant visitor access only when you hold every permission it gives on this project';
    const el = await mount({
      visitors: [ALICE],
      write: { status: 403, body: { error: { code: 'forbidden', message: refusal } } },
    });
    q(el, '[data-visitor-row="v-alice"] [data-action="extend"]')!.click();
    await el.updateComplete;
    await el.submitExtend();
    await el.updateComplete;

    expect(text(q(el, '[data-extend-error]'))).toBe(refusal);
  });

  it('revokes a visitor with DELETE after confirmation', async () => {
    const el = await mount({ visitors: [ALICE] });
    q(el, '[data-visitor-row="v-alice"] [data-action="revoke"]')!.click();
    await vi.waitFor(() => expect(calls().some((c) => c.method === 'DELETE')).toBe(true));

    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(calls().find((c) => c.method === 'DELETE')?.url).toBe(
      '/api/v1/projects/p-1/visitors/v-alice'
    );
  });

  it('removes a rule with DELETE /visitors/rules/{id}', async () => {
    const el = await mount({ rules: [RULE] });
    q(el, '[data-rule-row="r-corp"] [data-action="revoke"]')!.click();
    await vi.waitFor(() => expect(calls().some((c) => c.method === 'DELETE')).toBe(true));

    expect(calls().find((c) => c.method === 'DELETE')?.url).toBe(
      '/api/v1/projects/p-1/visitors/rules/r-corp'
    );
  });

  it('sends nothing when the revoke is not confirmed', async () => {
    vi.mocked(showConfirm).mockResolvedValueOnce(false);
    const el = await mount({ visitors: [ALICE] });
    q(el, '[data-visitor-row="v-alice"] [data-action="revoke"]')!.click();
    await settle(el);

    expect(calls().some((c) => c.method === 'DELETE')).toBe(false);
  });
});
