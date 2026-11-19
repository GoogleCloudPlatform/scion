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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';
import { clearAdminStatus } from '../../client/admin-status.js';
import { stateManager } from '../../client/state.js';
import {
  renderPermissionList,
  type EffectivePermissionSet,
  type ScionEffectivePermissionsView,
} from './effective-permissions-view.js';
import './effective-permissions-view.js';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

function samplePermissionSet(
  overrides: Partial<EffectivePermissionSet> = {}
): EffectivePermissionSet {
  const futureIso = new Date(Date.now() + 3 * 60 * 60 * 1000).toISOString();
  return {
    principal: { type: 'user', id: 'user-target-1', status: 'active' },
    scope: { type: 'system' },
    statusGate: null,
    evaluatedAt: '2026-11-19T18:00:00Z',
    permissions: [
      {
        id: 'agent.create',
        resource: 'agent',
        action: 'create',
        description: 'Create agents in a project',
        grants: [
          {
            bindingId: 'bind-direct-1',
            role: { id: 'role-hub-admin', name: 'hub-admin', system: true },
            scope: { type: 'system' },
            via: null,
            expiresAt: futureIso,
          },
        ],
      },
      {
        id: 'project.read',
        resource: 'project',
        action: 'read',
        description: 'Read project metadata',
        grants: [
          {
            bindingId: 'bind-direct-1',
            role: { id: 'role-hub-admin', name: 'hub-admin', system: true },
            scope: { type: 'system' },
            via: null,
            expiresAt: null,
          },
          {
            bindingId: 'bind-group-2',
            role: { id: 'role-hub-member', name: 'hub-member', system: true },
            scope: { type: 'system' },
            via: { groupId: 'grp-1', groupName: 'hub-members', groupSlug: 'hub-members' },
            expiresAt: null,
          },
        ],
      },
      {
        id: 'user.read',
        resource: 'user',
        action: 'read',
        description: 'Read user directory records',
        grants: [
          {
            bindingId: 'bind-group-2',
            role: { id: 'role-hub-member', name: 'hub-member', system: true },
            scope: { type: 'system' },
            via: { groupId: 'grp-1', groupName: 'hub-members', groupSlug: 'hub-members' },
            expiresAt: null,
          },
        ],
      },
    ],
    removed: [],
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function requestPath(input: string | URL | Request): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.pathname + input.search;
  const u = new URL(input.url, 'http://localhost');
  return u.pathname + u.search;
}

describe('renderPermissionList pure helper', () => {
  let host: HTMLDivElement;

  beforeEach(() => {
    host = document.createElement('div');
    document.body.appendChild(host);
  });

  afterEach(() => {
    host.remove();
  });

  it('groups permissions by resource and renders origin chips, tooltips, and expiry badges', () => {
    const data = samplePermissionSet();
    render(renderPermissionList(data), host);

    const groups = Array.from(host.querySelectorAll('.resource-group'));
    expect(groups.map((g) => g.getAttribute('data-resource'))).toEqual([
      'agent',
      'project',
      'user',
    ]);

    const permIds = Array.from(host.querySelectorAll('.perm-id')).map((el) =>
      el.textContent?.trim()
    );
    expect(permIds).toEqual(['agent.create', 'project.read', 'user.read']);

    const chips = Array.from(host.querySelectorAll('.origin-chip'));
    const chipTexts = chips.map((c) => c.textContent?.replace(/\s+/g, ' ').trim());
    expect(chipTexts).toContain('hub-admin · direct');
    expect(chipTexts).toContain('hub-member · via group: hub-members');

    const tooltips = Array.from(host.querySelectorAll('sl-tooltip')).map((t) =>
      t.getAttribute('content')
    );
    expect(tooltips.some((c) => c?.includes('bind-direct-1'))).toBe(true);
    expect(tooltips.some((c) => c?.includes('bind-group-2'))).toBe(true);

    const expiresBadge = host.querySelector('.expires-badge');
    expect(expiresBadge?.textContent?.toLowerCase()).toContain('expires');
  });

  it('filters rows by permission ID substring case-insensitively', () => {
    const data = samplePermissionSet();
    render(renderPermissionList(data, { filter: 'PROJECT' }), host);

    const permIds = Array.from(host.querySelectorAll('.perm-id')).map((el) =>
      el.textContent?.trim()
    );
    expect(permIds).toEqual(['project.read']);
  });

  it('copies the binding ID when an origin chip is clicked', () => {
    const onCopy = vi.fn();
    const data = samplePermissionSet();
    render(renderPermissionList(data, { onCopyBindingId: onCopy }), host);

    const firstChip = host.querySelector<HTMLButtonElement>('.origin-chip');
    expect(firstChip).not.toBeNull();
    firstChip!.click();
    expect(onCopy).toHaveBeenCalledWith('bind-direct-1');
  });
});

describe('scion-effective-permissions-view lazy loading', () => {
  let el: ScionEffectivePermissionsView | null = null;
  let calls: string[] = [];

  beforeEach(() => {
    clearAdminStatus();
    stateManager.setCurrentUserId('viewer-admin-1');
    calls = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const path = requestPath(input);
        calls.push(path);
        if (path.includes('/api/v1/auth/admin-status')) {
          return Promise.resolve(
            jsonResponse({ isAdmin: true, isSuperAdmin: false, permissions: ['role_binding.read'] })
          );
        }
        if (path.includes('/api/v1/admin/effective-permissions')) {
          return Promise.resolve(jsonResponse(samplePermissionSet()));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    el?.remove();
    el = null;
    clearAdminStatus();
    vi.unstubAllGlobals();
  });

  it('fetches only when active becomes true and caches across tab toggles', async () => {
    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalType = 'user';
    el.principalId = 'user-target-1';
    el.active = false;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(0);

    // Activate tab -> fetches once.
    el.active = true;
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(1);
    expect(el.shadowRoot?.querySelectorAll('.perm-id').length).toBe(3);

    // Toggle inactive -> active for the same principal -> no duplicate fetch.
    el.active = false;
    await el.updateComplete;
    el.active = true;
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(1);
  });

  it('does not fetch effective-permissions when caller lacks role_binding.read', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const path = requestPath(input);
        calls.push(path);
        if (path.includes('/api/v1/auth/admin-status')) {
          return Promise.resolve(
            jsonResponse({ isAdmin: false, isSuperAdmin: false, permissions: ['user.read'] })
          );
        }
        return Promise.resolve(jsonResponse(samplePermissionSet()));
      })
    );

    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalId = 'user-target-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(0);
  });
});

describe('scion-page-admin-users two-tab Roles dialog', () => {
  let pageEl: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;
  let calls: string[] = [];

  beforeEach(async () => {
    clearAdminStatus();
    stateManager.setCurrentUserId('admin-self');
    calls = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const path = requestPath(input);
        calls.push(path);
        if (path.includes('/auth/me')) {
          return Promise.resolve(jsonResponse({ id: 'admin-self', role: 'admin' }));
        }
        if (path.includes('/api/v1/auth/admin-status')) {
          return Promise.resolve(
            jsonResponse({ isAdmin: true, isSuperAdmin: false, permissions: ['role_binding.read'] })
          );
        }
        if (path.includes('/api/v1/users')) {
          return Promise.resolve(
            jsonResponse({
              users: [
                {
                  id: 'user-target-1',
                  email: 'target@example.com',
                  displayName: 'Target User',
                  role: 'member',
                  status: 'active',
                  created: '2026-01-01T00:00:00Z',
                  _capabilities: { actions: ['read', 'update'] },
                },
              ],
              totalCount: 1,
              _capabilities: { actions: ['list'] },
            })
          );
        }
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(jsonResponse({ items: [], totalCount: 0 }));
        }
        if (path.includes('/api/v1/admin/effective-permissions')) {
          return Promise.resolve(jsonResponse(samplePermissionSet()));
        }
        return Promise.resolve(jsonResponse([]));
      })
    );
    await import('../pages/admin-users.js');
  });

  afterEach(() => {
    pageEl?.remove();
    pageEl = null;
    clearAdminStatus();
    vi.unstubAllGlobals();
  });

  it('renders roles and permissions tabs, lazy-fetches permissions on tab show, and guards sl-request-close bubbling', async () => {
    pageEl = document.createElement('scion-page-admin-users') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    document.body.appendChild(pageEl);
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 100));
    await pageEl.updateComplete;

    // Click View Roles on the target row.
    const menuItems = Array.from(pageEl.shadowRoot?.querySelectorAll('sl-menu-item') ?? []);
    const viewRolesItem = menuItems.find((item) => item.textContent?.includes('View Roles'));
    expect(viewRolesItem).toBeDefined();
    (viewRolesItem as HTMLElement).click();
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 60));

    const dialog = pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog?.getAttribute('label')).toBe('Roles — Target User');

    const tabs = Array.from(dialog!.querySelectorAll('sl-tab'));
    expect(tabs.map((t) => t.getAttribute('panel'))).toEqual(['roles', 'permissions']);

    // Initial tab is roles -> no effective-permissions request yet.
    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(0);

    // Bubbled sl-request-close from a child inside the dialog MUST NOT close the outer dialog.
    const innerProvenance = dialog!.querySelector('scion-effective-role-provenance')!;
    innerProvenance.dispatchEvent(
      new CustomEvent('sl-request-close', { bubbles: true, composed: true })
    );
    await pageEl.updateComplete;
    expect(pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog')).not.toBeNull();

    // Switching to permissions tab triggers the lazy fetch.
    const tabGroup = dialog!.querySelector('sl-tab-group')!;
    tabGroup.dispatchEvent(
      new CustomEvent('sl-tab-show', {
        detail: { name: 'permissions' },
        bubbles: true,
        composed: true,
      })
    );
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 100));
    await pageEl.updateComplete;

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(1);

    // Direct sl-request-close on the dialog itself closes it.
    dialog!.dispatchEvent(new CustomEvent('sl-request-close', { bubbles: true, composed: true }));
    await pageEl.updateComplete;
    expect(pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog')).toBeNull();
  });
});
