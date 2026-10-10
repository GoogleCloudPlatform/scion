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
 * The sidebar Admin section is gated by the permissions in the admin-status
 * response, not by its isAdmin flag: isAdmin is true only for hub admins and
 * super admins, while a member may hold permissions that open individual
 * admin pages.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { User } from '../../shared/types.js';
import { clearAdminStatus } from '../../client/admin-status.js';
import type { AdminStatus } from '../../lib/admin-permissions.js';
import type { ScionNav } from './nav.js';

let adminStatusBody: AdminStatus | null = null;

function adminStatusRequested(): boolean {
  return (
    vi
      .mocked(fetch)
      // eslint-disable-next-line @typescript-eslint/no-base-to-string -- test reads the recorded fetch URL; tracked in ptone/scion#4126
      .mock.calls.some(([input]) => String(input).includes('/api/v1/auth/admin-status'))
  );
}

async function mount(user: Pick<User, 'id' | 'role'>): Promise<ScionNav> {
  const el = document.createElement('scion-nav') as ScionNav;
  el.currentPath = '/';
  el.user = { email: `${user.id}@test.com`, name: user.id, ...user };
  document.body.appendChild(el);
  await el.updateComplete;
  if (user.role !== 'admin') {
    await vi.waitFor(() => expect(adminStatusRequested()).toBe(true));
    // Let the admin-status response resolve and the nav re-render.
    await new Promise((r) => setTimeout(r, 0));
  }
  await el.updateComplete;
  await el.updateComplete;
  return el;
}

function adminLinks(el: ScionNav): string[] {
  const section = el.shadowRoot!.querySelector('.admin-section');
  if (!section) return [];
  return Array.from(section.querySelectorAll('a.nav-link')).map((a) => a.getAttribute('href')!);
}

describe('sidebar Admin section permission gating', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        // eslint-disable-next-line @typescript-eslint/no-base-to-string -- test fetch stub reads the request URL; tracked in ptone/scion#4126
        if (String(input).includes('/api/v1/auth/admin-status') && adminStatusBody) {
          return Promise.resolve(new Response(JSON.stringify(adminStatusBody), { status: 200 }));
        }
        return Promise.resolve(new Response('{}', { status: 404 }));
      })
    );
    await import('./nav.js');
  }, 30_000);

  beforeEach(() => {
    clearAdminStatus();
    vi.mocked(fetch).mockClear();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    adminStatusBody = null;
    localStorage.clear();
  });

  it('shows the page a member holds a permission for, even though isAdmin is false', async () => {
    adminStatusBody = { isAdmin: false, isSuperAdmin: false, permissions: ['quota.read'] };
    const el = await mount({ id: 'u-member', role: 'member' });
    await vi.waitFor(() => expect(adminLinks(el)).toEqual(['/admin/quotas']));
  });

  it('hides the Admin section for a member without admin-page permissions', async () => {
    adminStatusBody = { isAdmin: false, isSuperAdmin: false, permissions: ['inbox.read'] };
    const el = await mount({ id: 'u-member', role: 'member' });
    expect(el.shadowRoot!.querySelector('.admin-section')).toBeNull();
  });

  it('shows a hub admin every page their permissions open', async () => {
    adminStatusBody = {
      isAdmin: true,
      isSuperAdmin: false,
      permissions: ['user.read', 'group.list', 'role.read', 'hub.health.read'],
    };
    const el = await mount({ id: 'u-hub-admin', role: 'member' });
    await vi.waitFor(() =>
      expect(adminLinks(el)).toEqual(['/admin/users', '/admin/groups', '/admin/roles', '/health'])
    );
  });

  it('shows a super admin every admin page, including Diagnostics and Maintenance', async () => {
    const el = await mount({ id: 'u-admin', role: 'admin' });
    const links = adminLinks(el);
    expect(links).toContain('/settings');
    expect(links).toContain('/admin/quotas');
    expect(links).toContain('/admin/diagnostics');
    expect(links).toContain('/admin/maintenance');
  });
});
