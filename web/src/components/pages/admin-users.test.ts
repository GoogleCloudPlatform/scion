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
 * Tests for admin-users.ts: the per-user "Change role" submenu
 * (Admin / Member / Viewer), invited-user display, the role filter and the
 * invite dialog hint.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { AdminUser } from '../../shared/types.js';

const SELF_ID = 'u-self';

function makeUser(overrides: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 'u-target',
    email: 'target@example.com',
    displayName: 'Target User',
    role: 'member',
    status: 'active',
    created: '2026-01-01T00:00:00Z',
    _capabilities: { actions: ['read', 'update', 'promote', 'suspend', 'delete'] },
    ...overrides,
  } as AdminUser;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(users: AdminUser[]) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    const method = init?.method ?? 'GET';

    if (path.includes('/auth/me')) {
      return Promise.resolve(jsonResponse({ id: SELF_ID, role: 'admin' }));
    }
    if (method === 'PATCH' && path.includes('/api/v1/users/')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as { role?: string };
      const id = path.split('/api/v1/users/')[1];
      const u = users.find((x) => x.id === id);
      return Promise.resolve(jsonResponse({ ...u, ...body }));
    }
    if (path.includes('/api/v1/users')) {
      return Promise.resolve(
        jsonResponse({ users, totalCount: users.length, _capabilities: { actions: ['list'] } })
      );
    }
    return Promise.resolve(jsonResponse([]));
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let mod: any;

async function createComponent(users: AdminUser[]) {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(users)));
  const el = document.createElement('scion-page-admin-users') as HTMLElement & {
    updateComplete: Promise<boolean>;
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 100));
  await el.updateComplete;
  return el;
}

function queryAll(el: HTMLElement, selector: string): HTMLElement[] {
  return Array.from(el.shadowRoot?.querySelectorAll<HTMLElement>(selector) ?? []);
}

function roleItems(el: HTMLElement): HTMLElement[] {
  return queryAll(el, '.change-role-menu sl-menu-item');
}

/** Simulates Shoelace's sl-menu selection: toggle a checkbox item, then emit sl-select. */
function selectRole(item: HTMLElement): void {
  const it = item as HTMLElement & { checked: boolean; value: string };
  // Shoelace is not registered under happy-dom; mirror the properties a
  // registered sl-menu-item exposes from its attributes.
  it.value = item.getAttribute('value') ?? '';
  it.checked = !item.hasAttribute('checked');
  item.dispatchEvent(
    new CustomEvent('sl-select', { detail: { item }, bubbles: true, composed: true })
  );
}

function roleOf(item: HTMLElement): string | null {
  return item.getAttribute('value');
}

function patchCalls() {
  return vi
    .mocked(fetch)
    .mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PATCH');
}

describe('scion-page-admin-users — Change role submenu', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    mod = await import('./admin-users.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('offers Admin, Member and Viewer in order', async () => {
    element = await createComponent([makeUser({ role: 'member' })]);

    const items = roleItems(element);
    expect(items.map(roleOf)).toEqual(['admin', 'member', 'viewer']);
    expect(items.map((i) => i.textContent?.trim())).toEqual(['Admin', 'Member', 'Viewer']);
    expect(queryAll(element, '.change-role-item').length).toBe(1);
  });

  it('no longer renders the hard-coded Promote/Demote items', async () => {
    element = await createComponent([
      makeUser({ id: 'u-admin', email: 'a@example.com', role: 'admin' }),
      makeUser({ id: 'u-member', email: 'm@example.com', role: 'member' }),
    ]);
    const text = element.shadowRoot?.textContent ?? '';
    expect(text).not.toContain('Demote to Member');
    expect(text).not.toContain('Promote to Admin');
    expect(queryAll(element, '.change-role-item').length).toBe(2);
  });

  for (const current of ['admin', 'member', 'viewer'] as const) {
    it(`marks the current role (${current}) checked and disabled`, async () => {
      element = await createComponent([makeUser({ role: current })]);

      for (const item of roleItems(element)) {
        const isCurrent = roleOf(item) === current;
        expect(item.getAttribute('type'), `${roleOf(item)} type`).toBe('checkbox');
        expect(item.hasAttribute('disabled'), `${roleOf(item)} disabled`).toBe(isCurrent);
        expect(item.hasAttribute('checked'), `${roleOf(item)} checked`).toBe(isCurrent);
        expect(item.hasAttribute('aria-checked')).toBe(false);
      }
    });
  }

  it('hides Change role without the promote capability', async () => {
    element = await createComponent([
      makeUser({ _capabilities: { actions: ['read', 'suspend', 'delete'] } }),
    ]);
    expect(queryAll(element, '.change-role-item').length).toBe(0);
  });

  it('does not render actions for the signed-in user', async () => {
    element = await createComponent([makeUser({ id: SELF_ID, role: 'admin' })]);
    expect(queryAll(element, '.change-role-item').length).toBe(0);
  });

  it('choosing Viewer confirms with the role meaning and sends PATCH role viewer', async () => {
    const user = makeUser({ role: 'member' });
    element = await createComponent([user]);

    const viewer = roleItems(element).find((i) => roleOf(i) === 'viewer')!;
    selectRole(viewer);
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;

    // Nothing is sent until the change is confirmed, and the check mark
    // stays on the current role (the sl-menu toggle is undone).
    expect(patchCalls()).toHaveLength(0);
    expect((viewer as HTMLElement & { checked: boolean }).checked).toBe(false);

    const dialog = element.shadowRoot?.querySelector('sl-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog!.getAttribute('label')).toBe('Change role to Viewer');
    const dialogText = dialog!.textContent ?? '';
    expect(dialogText).toContain('from Member to Viewer');
    expect(dialogText).toContain(mod.HUB_ROLE_DESCRIPTIONS.viewer);
    expect(dialogText).toContain('cannot create projects');

    const confirm = Array.from(dialog!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Make Viewer'
    ) as HTMLElement;
    expect(confirm).toBeTruthy();
    confirm.click();
    await new Promise((resolve) => setTimeout(resolve, 50));

    const calls = patchCalls();
    expect(calls).toHaveLength(1);
    const [url, init] = calls[0];
    expect(String(url)).toContain(`/api/v1/users/${user.id}`);
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({ role: 'viewer' });
  });

  it('cancelling the confirmation sends nothing and keeps the current role checked', async () => {
    element = await createComponent([makeUser({ role: 'member' })]);
    const admin = roleItems(element).find((i) => roleOf(i) === 'admin')!;
    selectRole(admin);
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;

    const dialog = element.shadowRoot?.querySelector('sl-dialog');
    expect(dialog).not.toBeNull();
    const cancel = Array.from(dialog!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Cancel'
    ) as HTMLElement;
    cancel.click();
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;

    expect(element.shadowRoot?.querySelector('sl-dialog')).toBeNull();
    expect(patchCalls()).toHaveLength(0);
    expect((admin as HTMLElement & { checked: boolean }).checked).toBe(false);
  });

  it('selecting the current role does nothing', async () => {
    element = await createComponent([makeUser({ role: 'viewer' })]);
    const viewer = roleItems(element).find((i) => roleOf(i) === 'viewer')!;
    selectRole(viewer);
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(element.shadowRoot?.querySelector('sl-dialog')).toBeNull();
    expect(patchCalls()).toHaveLength(0);
    expect((viewer as HTMLElement & { checked: boolean }).checked).toBe(true);
  });

  it('describes every role', () => {
    for (const role of mod.HUB_ROLE_OPTIONS) {
      expect(mod.HUB_ROLE_DESCRIPTIONS[role]).toBeTruthy();
      expect(mod.HUB_ROLE_LABELS[role]).toBeTruthy();
    }
  });
});

function listCalls(): URL[] {
  return vi
    .mocked(fetch)
    .mock.calls.map(([url, init]) => ({
      url: String(url),
      method: (init as RequestInit | undefined)?.method ?? 'GET',
    }))
    .filter((c) => c.method === 'GET' && c.url.includes('/api/v1/users?'))
    .map((c) => new URL(c.url, 'http://localhost'));
}

/** Simulates choosing an option in an sl-select (Shoelace is not registered under happy-dom). */
async function chooseSelect(
  el: HTMLElement & { updateComplete: Promise<boolean> },
  select: HTMLElement,
  value: string
): Promise<void> {
  (select as HTMLElement & { value: string }).value = value;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
}

type PageEl = HTMLElement & { updateComplete: Promise<boolean> };

describe('scion-page-admin-users — invited users and role filter', () => {
  let element: PageEl | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    mod = await import('./admin-users.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  const invited = makeUser({
    id: 'u-invited',
    email: 'invited@example.com',
    displayName: '',
    role: 'member', // placeholder stored on invited rows
    status: 'invited',
  });

  it('shows "Assigned at sign-in" instead of a role badge for invited users', async () => {
    element = (await createComponent([invited])) as PageEl;
    const rows = queryAll(element, 'tbody tr');
    expect(rows).toHaveLength(1);
    const pending = rows[0].querySelector('.role-pending');
    expect(pending?.textContent?.trim()).toBe('Assigned at sign-in');
    expect(rows[0].querySelector('.role-badge')).toBeNull();
  });

  it('keeps the role badge for active users', async () => {
    element = (await createComponent([makeUser({ role: 'viewer' })])) as PageEl;
    const row = queryAll(element, 'tbody tr')[0];
    expect(row.querySelector('.role-badge.viewer')?.textContent?.trim()).toBe('viewer');
    expect(row.querySelector('.role-pending')).toBeNull();
  });

  it('offers no Change role for invited users, even with the promote capability', async () => {
    element = (await createComponent([invited])) as PageEl;
    expect(queryAll(element, '.change-role-item')).toHaveLength(0);
    expect(roleItems(element)).toHaveLength(0);
    const text = element.shadowRoot?.textContent ?? '';
    expect(text).toContain('Remove');
  });

  it('renders a role filter with All / Admin / Member / Viewer', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const select = element.shadowRoot?.querySelector('sl-select.role-filter');
    expect(select).not.toBeNull();
    const options = Array.from(select!.querySelectorAll('sl-option'));
    expect(options.map((o) => o.getAttribute('value'))).toEqual([
      'all',
      'admin',
      'member',
      'viewer',
    ]);
    expect(select!.getAttribute('value')).toBe('all');
    // Initial load sends no role parameter.
    const calls = listCalls();
    expect(calls.length).toBeGreaterThan(0);
    expect(calls[calls.length - 1].searchParams.has('role')).toBe(false);
  });

  it('choosing Viewer sends ?role=viewer', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const select = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, select, 'viewer');

    const calls = listCalls();
    const last = calls[calls.length - 1];
    expect(last.searchParams.get('role')).toBe('viewer');
  });

  it('excludes invited users from role buckets', async () => {
    const active = makeUser({ id: 'u-active', email: 'active@example.com', role: 'member' });
    element = (await createComponent([active, invited])) as PageEl;
    expect(queryAll(element, 'tbody tr')).toHaveLength(2);

    const select = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, select, 'member');

    const rows = queryAll(element, 'tbody tr');
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('active@example.com');
    expect(element.shadowRoot?.textContent).toContain('1 user');
  });

  it('disables the role filter and drops ?role= when the status filter is Invited', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const roleSelect = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, roleSelect, 'viewer');

    const statusSelect = Array.from(
      element.shadowRoot!.querySelectorAll<HTMLElement>('sl-select')
    ).find((s) => !s.classList.contains('role-filter'))!;
    await chooseSelect(element, statusSelect, 'invited');

    const last = listCalls().at(-1)!;
    expect(last.searchParams.get('status')).toBe('invited');
    expect(last.searchParams.has('role')).toBe(false);
    const roleAfter = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    expect(roleAfter.hasAttribute('disabled')).toBe(true);
    expect(roleAfter.getAttribute('value')).toBe('all');
  });

  it('invite dialog explains the role is assigned at first sign-in, with no role picker', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const inviteBtn = Array.from(element.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    expect(inviteBtn).toBeTruthy();
    inviteBtn.click();
    await element.updateComplete;

    const dialog = element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]');
    expect(dialog).not.toBeNull();
    expect(
      dialog!.querySelector('.invite-role-hint')?.textContent?.replace(/\s+/g, ' ').trim()
    ).toBe('Role is assigned at first sign-in using the hub default (Admin > Server Config).');
    expect(dialog!.querySelector('sl-select, sl-radio-group')).toBeNull();
    expect(dialog!.textContent).not.toContain('Viewer');
  });
});

describe('scion-page-admin-users — role filter across pages', () => {
  let element: PageEl | null = null;

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  function members(prefix: string, n: number): AdminUser[] {
    return Array.from({ length: n }, (_, i) =>
      makeUser({ id: `${prefix}-${i}`, email: `${prefix}-${i}@example.com`, role: 'member' })
    );
  }

  function invitedRows(prefix: string, n: number): AdminUser[] {
    return Array.from({ length: n }, (_, i) =>
      makeUser({
        id: `${prefix}-${i}`,
        email: `${prefix}-${i}@example.com`,
        displayName: '',
        role: 'member',
        status: 'invited',
      })
    );
  }

  /** Serves two cursor pages; the server total counts invited placeholder rows. */
  async function createPaged(
    page1: AdminUser[],
    page2: AdminUser[],
    total: number,
    page1Cursor: string | null = 'c2'
  ) {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
        if (path.includes('/auth/me')) {
          return Promise.resolve(jsonResponse({ id: SELF_ID, role: 'admin' }));
        }
        if (path.includes('/api/v1/users?')) {
          const cursor = new URL(path, 'http://localhost').searchParams.get('cursor');
          const body =
            cursor === 'c2'
              ? { users: page2, totalCount: total }
              : { users: page1, totalCount: total, nextCursor: page1Cursor ?? undefined };
          return Promise.resolve(jsonResponse(body));
        }
        return Promise.resolve(jsonResponse([]));
      })
    );
    const el = document.createElement('scion-page-admin-users') as PageEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 100));
    await el.updateComplete;
    return el;
  }

  function nextButton(el: HTMLElement): HTMLElement | undefined {
    return Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('.pagination sl-button') ?? []
    ).find((b) => b.textContent?.trim() === 'Next');
  }

  function text(el: HTMLElement, selector: string): string {
    return el.shadowRoot?.querySelector(selector)?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
  }

  function usersTab(el: HTMLElement): string {
    return text(el, '.tab-btn[aria-controls="panel-users"]');
  }

  it('keeps the pager and Next when invited rows are filtered out of page 1', async () => {
    element = await createPaged(
      [...members('m1', 40), ...invitedRows('i1', 10)],
      members('m2', 10),
      60
    );
    expect(nextButton(element)).toBeTruthy();
    expect(text(element, '.user-count')).toBe('60 users');
    expect(usersTab(element)).toBe('Users (60)');

    const select = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, select, 'member');

    expect(queryAll(element, 'tbody tr')).toHaveLength(40);
    const next = nextButton(element);
    expect(next).toBeTruthy();
    expect(next!.hasAttribute('disabled')).toBe(false);
    // The server total includes invited rows, so it is shown as an upper bound.
    expect(text(element, '.user-count')).toBe('up to 60 users');
    expect(usersTab(element)).toBe('Users (up to 60)');
    expect(text(element, '.page-indicator')).toBe('Page 1');
    expect(text(element, '.pagination-info')).toBe('Showing 40 on this page');

    next!.click();
    await new Promise((resolve) => setTimeout(resolve, 50));
    await element.updateComplete;

    const last = listCalls().at(-1)!;
    expect(last.searchParams.get('cursor')).toBe('c2');
    expect(last.searchParams.get('role')).toBe('member');
    expect(queryAll(element, 'tbody tr')).toHaveLength(10);
    expect(text(element, '.page-indicator')).toBe('Page 2');
  });

  it('still offers Next on a page made up only of invited rows', async () => {
    element = await createPaged(invitedRows('i1', 50), members('m2', 5), 55);
    const select = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, select, 'member');

    expect(queryAll(element, 'tbody tr')).toHaveLength(0);
    expect(text(element, '.empty-state p')).toBe('No matching users on this page.');
    const next = nextButton(element);
    expect(next).toBeTruthy();
    expect(next!.hasAttribute('disabled')).toBe(false);

    next!.click();
    await new Promise((resolve) => setTimeout(resolve, 50));
    await element.updateComplete;
    expect(queryAll(element, 'tbody tr')).toHaveLength(5);
  });

  it('shows the exact count and "Page a of b" when a status filter excludes invited rows on the server', async () => {
    element = await createPaged(members('m1', 50), members('m2', 10), 60);
    const statusSelect = Array.from(
      element.shadowRoot!.querySelectorAll<HTMLElement>('sl-select')
    ).find((s) => !s.classList.contains('role-filter'))!;
    await chooseSelect(element, statusSelect, 'active');
    const roleSelect = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, roleSelect, 'member');

    const last = listCalls().at(-1)!;
    expect(last.searchParams.get('status')).toBe('active');
    expect(last.searchParams.get('role')).toBe('member');
    expect(text(element, '.user-count')).toBe('60 users');
    expect(usersTab(element)).toBe('Users (60)');
    expect(text(element, '.page-indicator')).toBe('Page 1 of 2');
    expect(text(element, '.pagination-info')).toBe('Showing 1-50 of 60');
  });

  it('shows the client-side count in the toolbar and the Users tab on a single page', async () => {
    element = await createPaged([...members('m1', 3), ...invitedRows('i1', 2)], [], 5, null);
    expect(usersTab(element)).toBe('Users (5)');

    const select = element.shadowRoot!.querySelector<HTMLElement>('sl-select.role-filter')!;
    await chooseSelect(element, select, 'member');

    expect(queryAll(element, 'tbody tr')).toHaveLength(3);
    expect(text(element, '.user-count')).toBe('3 users');
    expect(usersTab(element)).toBe('Users (3)');
  });

  it('shows the exact server count and "Page a of b" without the role filter', async () => {
    element = await createPaged(members('m1', 50), members('m2', 10), 60);
    expect(text(element, '.user-count')).toBe('60 users');
    expect(text(element, '.page-indicator')).toBe('Page 1 of 2');
    expect(text(element, '.pagination-info')).toBe('Showing 1-50 of 60');
  });
});

describe('scion-page-admin-users — invite dialog display name and submit routing', () => {
  let element: PageEl | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler([])));
    mod = await import('./admin-users.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  /** Fetch handler that answers the two submit endpoints with the given responses. */
  function withSubmitResponses(inviteRes: () => Response, provisionRes: () => Response) {
    const base = createFetchHandler([makeUser()]);
    return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      if (init?.method === 'POST' && path.endsWith('/api/v1/admin/users/invite')) {
        return Promise.resolve(inviteRes());
      }
      if (init?.method === 'POST' && path.endsWith('/api/v1/users')) {
        return Promise.resolve(provisionRes());
      }
      return base(url, init);
    };
  }

  async function openDialogAndSubmit(
    el: PageEl,
    fields: { email: string; displayName?: string; note?: string }
  ): Promise<void> {
    const inviteBtn = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    inviteBtn.click();
    await el.updateComplete;
    const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')!;
    const setInput = (label: string, value: string) => {
      const input = dialog.querySelector(`sl-input[label="${label}"]`) as HTMLInputElement;
      expect(input, label).not.toBeNull();
      input.value = value;
      input.dispatchEvent(new Event('sl-input'));
    };
    setInput('Email address', fields.email);
    if (fields.displayName !== undefined) setInput('Display name (optional)', fields.displayName);
    if (fields.note !== undefined) setInput('Note (optional)', fields.note);
    await el.updateComplete;
    const submit = Array.from(dialog.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    submit.click();
    await new Promise((resolve) => setTimeout(resolve, 20));
    await el.updateComplete;
  }

  function postCalls(): Array<{ path: string; body: Record<string, unknown> }> {
    return vi
      .mocked(fetch)
      .mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
      .map(([url, init]) => ({
        path: String(url),
        body: JSON.parse(String((init as RequestInit).body)) as Record<string, unknown>,
      }));
  }

  function feedback(el: PageEl): { variant: string | null; text: string } {
    const alert = el.shadowRoot!.querySelector('.feedback-alert');
    return {
      variant: alert?.getAttribute('variant') ?? null,
      text: alert?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
    };
  }

  it('has an optional Display name field', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const inviteBtn = Array.from(element.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    inviteBtn.click();
    await element.updateComplete;
    const input = element.shadowRoot!.querySelector(
      'sl-dialog[label="Invite User"] sl-input[label="Display name (optional)"]'
    );
    expect(input).not.toBeNull();
    expect(input!.hasAttribute('required')).toBe(false);
  });

  it('without a display name, submits to the invite endpoint as before', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({ id: 'u1', email: 'a@example.com', status: 'invited' }, 201),
          () => jsonResponse({}, 500)
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'A@Example.com', displayName: '   ', note: 'n' });
    const posts = postCalls();
    expect(posts).toHaveLength(1);
    expect(posts[0].path).toContain('/api/v1/admin/users/invite');
    expect(posts[0].body).toEqual({ email: 'a@example.com', note: 'n' });
    expect(feedback(element)).toEqual({ variant: 'success', text: 'Invited a@example.com.' });
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).toBeNull();
  });

  it('with a display name, submits to POST /api/v1/users without a role', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              { user: { id: 'u1', email: 'a@example.com', status: 'invited' }, created: true },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: ' Alice ' });
    const posts = postCalls();
    expect(posts).toHaveLength(1);
    expect(posts[0].path).toMatch(/\/api\/v1\/users$/);
    expect(posts[0].body).toEqual({ email: 'a@example.com', displayName: 'Alice' });
    expect(feedback(element)).toEqual({ variant: 'success', text: 'Invited a@example.com.' });
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).toBeNull();
  });

  it('shows provisioning warnings as a non-blocking notice', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              {
                user: { id: 'u1', email: 'a@other.example', status: 'invited' },
                created: true,
                warnings: ['domain_not_authorized'],
              },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@other.example', displayName: 'A' });
    const fb = feedback(element);
    expect(fb.variant).toBe('warning');
    expect(fb.text).toContain('Invited a@other.example.');
    expect(fb.text).toContain("outside the hub's authorized domains");
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).toBeNull();
  });

  it('renders a 200 created:false replay as a notice, not an error', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse({ user: { email: 'a@example.com', status: 'invited' }, created: false })
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({
      variant: 'primary',
      text: 'a@example.com is already pre-registered with these details.',
    });
  });

  for (const [reason, message] of [
    ['pending_user_exists', 'A pending record for this email exists with different details.'],
    ['user_suspended_exists', 'This email belongs to a suspended user.'],
    ['user_exists', 'User already exists.'],
  ] as const) {
    it(`renders 409 ${reason} with its own message and keeps the dialog open`, async () => {
      element = (await createComponent([makeUser()])) as PageEl;
      vi.stubGlobal(
        'fetch',
        vi.fn(
          withSubmitResponses(
            () => jsonResponse({}, 500),
            () =>
              jsonResponse(
                { error: { code: 'conflict', message: 'x', details: { reason, userId: 'u9' } } },
                409
              )
          )
        )
      );
      await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
      expect(feedback(element)).toEqual({ variant: 'danger', text: message });
      expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).not.toBeNull();
    });
  }

  it('renders a 409 without a reason with the fallback copy', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () => jsonResponse({ error: { code: 'conflict', message: 'x' } }, 409)
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({ variant: 'danger', text: 'User already exists.' });
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).not.toBeNull();
  });

  it('sets the alert duration: warnings stay open, other notices close after 5 seconds', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              {
                user: { id: 'u1', email: 'a@other.example', status: 'invited' },
                created: true,
                warnings: ['domain_not_authorized'],
              },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@other.example', displayName: 'A' });
    const warning = element.shadowRoot!.querySelector('.feedback-alert') as HTMLElement & {
      duration: number;
    };
    expect(warning.getAttribute('variant')).toBe('warning');
    expect(warning.duration).toBe(Infinity);

    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              { user: { id: 'u2', email: 'b@example.com', status: 'invited' }, created: true },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'b@example.com', displayName: 'B' });
    const success = element.shadowRoot!.querySelector('.feedback-alert') as HTMLElement & {
      duration: number;
    };
    expect(success.getAttribute('variant')).toBe('success');
    expect(success.duration).toBe(5000);
    expect(success).not.toBe(warning);
  });

  it('a newer notice is not cleared by an earlier notice closing', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    let provisionCalls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () => {
            provisionCalls++;
            return provisionCalls === 1
              ? jsonResponse(
                  { error: { code: 'conflict', message: 'x', details: { reason: 'user_exists' } } },
                  409
                )
              : jsonResponse(
                  {
                    user: { id: 'u1', email: 'a@other.example', status: 'invited' },
                    created: true,
                    warnings: ['domain_not_authorized'],
                  },
                  201
                );
          }
        )
      )
    );
    // A 409 danger notice, then a corrected resubmit within 5 s that
    // returns 201 with warnings.
    await openDialogAndSubmit(element, { email: 'a@other.example', displayName: 'A' });
    const danger = element.shadowRoot!.querySelector('.feedback-alert') as HTMLElement;
    expect(danger.getAttribute('variant')).toBe('danger');

    vi.useFakeTimers();
    try {
      const dialog = element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')!;
      const submit = Array.from(dialog.querySelectorAll('sl-button')).find(
        (b) => b.textContent?.trim() === 'Invite User'
      ) as HTMLElement;
      submit.click();
      await vi.advanceTimersByTimeAsync(50);
      await element.updateComplete;
      const warning = element.shadowRoot!.querySelector('.feedback-alert') as HTMLElement;
      expect(warning.getAttribute('variant')).toBe('warning');
      expect(warning).not.toBe(danger);

      // The earlier notice's alert finishes closing after the newer one is
      // shown; it must not clear the newer notice.
      danger.dispatchEvent(new Event('sl-after-hide'));
      await vi.advanceTimersByTimeAsync(5000);
      await element.updateComplete;
      const still = element.shadowRoot!.querySelector('.feedback-alert') as HTMLElement;
      expect(still).not.toBeNull();
      expect(still.getAttribute('variant')).toBe('warning');
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows an unexpected 422 as a generic error', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              { error: { code: 'unprocessable', message: 'role cannot be set at provisioning' } },
              422
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({
      variant: 'danger',
      text: 'role cannot be set at provisioning',
    });
  });

  it('forwards a non-empty note on the provisioning path', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              { user: { id: 'u1', email: 'a@example.com', status: 'invited' }, created: true },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A', note: 'n' });
    const posts = postCalls();
    expect(posts).toHaveLength(1);
    expect(posts[0].body).toEqual({ email: 'a@example.com', displayName: 'A', note: 'n' });
  });

  it('keeps advisory warnings on a 200 created:false replay', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse({
              user: { email: 'a@other.example', status: 'invited' },
              created: false,
              warnings: ['domain_not_authorized'],
            })
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@other.example', displayName: 'A' });
    const fb = feedback(element);
    expect(fb.variant).toBe('warning');
    expect(fb.text).toContain('a@other.example is already pre-registered with these details.');
    expect(fb.text).toContain("outside the hub's authorized domains");
  });

  it('after success, reloads the user list and resets the form', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              { user: { id: 'u1', email: 'a@example.com', status: 'invited' }, created: true },
              201
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A', note: 'n' });
    const listCalls = vi
      .mocked(fetch)
      .mock.calls.filter(
        ([url, init]) =>
          String(url).includes('/api/v1/users') &&
          ((init as RequestInit | undefined)?.method ?? 'GET') === 'GET'
      );
    expect(listCalls.length).toBeGreaterThan(0);

    const inviteBtn = Array.from(element.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    inviteBtn.click();
    await element.updateComplete;
    const dialog = element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')!;
    for (const label of ['Email address', 'Display name (optional)', 'Note (optional)']) {
      const input = dialog.querySelector(`sl-input[label="${label}"]`) as HTMLInputElement;
      expect(input.value, label).toBe('');
    }
  });

  it('shows a 400 validation error and keeps the dialog open', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () =>
            jsonResponse(
              {
                error: {
                  code: 'validation_error',
                  message: 'displayName must not contain control characters',
                  details: { field: 'displayName' },
                },
              },
              400
            )
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({
      variant: 'danger',
      text: 'displayName must not contain control characters',
    });
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).not.toBeNull();
  });

  it('shows a network failure and keeps the dialog open', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const base = createFetchHandler([makeUser()]);
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        if (init?.method === 'POST') return Promise.reject(new Error('network down'));
        return base(url, init);
      })
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({ variant: 'danger', text: 'network down' });
    expect(element.shadowRoot!.querySelector('sl-dialog[label="Invite User"]')).not.toBeNull();
  });

  it('reports success when a 2xx body is not JSON', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        withSubmitResponses(
          () => jsonResponse({}, 500),
          () => new Response('not json', { status: 201 })
        )
      )
    );
    await openDialogAndSubmit(element, { email: 'a@example.com', displayName: 'A' });
    expect(feedback(element)).toEqual({ variant: 'success', text: 'Invited a@example.com.' });
  });

  it('explains display-name precedence in the field help text', async () => {
    element = (await createComponent([makeUser()])) as PageEl;
    const inviteBtn = Array.from(element.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Invite User'
    ) as HTMLElement;
    inviteBtn.click();
    await element.updateComplete;
    const input = element.shadowRoot!.querySelector(
      'sl-dialog[label="Invite User"] sl-input[label="Display name (optional)"]'
    );
    expect(input!.getAttribute('help-text')).toBe(
      'Replaced at first sign-in by the name from the sign-in provider, if it supplies one.'
    );
  });

  it('maps every documented warning to readable text', () => {
    expect(mod.provisionWarningText('reserved_identity')).toContain('reserved platform identity');
    expect(mod.provisionWarningText('domain_not_authorized')).toContain('authorized domains');
    expect(mod.provisionWarningText('sign_in_currently_blocked_by_access_mode')).toContain(
      'blocks all sign-ins'
    );
    expect(mod.provisionWarningText('something_new')).toBe('Warning: something_new.');
  });
});
