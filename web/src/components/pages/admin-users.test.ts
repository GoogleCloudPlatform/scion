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
 * (Admin / Member / Viewer).
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
  return queryAll(el, '.change-role-menu sl-menu-item[data-role]');
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
    expect(items.map((i) => i.dataset.role)).toEqual(['admin', 'member', 'viewer']);
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
        const isCurrent = item.dataset.role === current;
        expect(item.hasAttribute('disabled'), `${item.dataset.role} disabled`).toBe(isCurrent);
        expect(item.getAttribute('aria-checked')).toBe(isCurrent ? 'true' : 'false');
        const check = item.querySelector('sl-icon[name="check2"]') as HTMLElement | null;
        expect(check).not.toBeNull();
        expect(check!.getAttribute('style') ?? '').toBe(isCurrent ? '' : 'visibility: hidden');
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

    const viewer = roleItems(element).find((i) => i.dataset.role === 'viewer')!;
    viewer.click();
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;

    // Nothing is sent until the change is confirmed.
    expect(patchCalls()).toHaveLength(0);

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

  it('clicking the current (disabled) role does nothing', async () => {
    element = await createComponent([makeUser({ role: 'viewer' })]);
    const viewer = roleItems(element).find((i) => i.dataset.role === 'viewer')!;
    viewer.click();
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(element.shadowRoot?.querySelector('sl-dialog')).toBeNull();
    expect(patchCalls()).toHaveLength(0);
  });

  it('describes every role', () => {
    for (const role of mod.HUB_ROLE_OPTIONS) {
      expect(mod.HUB_ROLE_DESCRIPTIONS[role]).toBeTruthy();
      expect(mod.HUB_ROLE_LABELS[role]).toBeTruthy();
    }
  });
});
