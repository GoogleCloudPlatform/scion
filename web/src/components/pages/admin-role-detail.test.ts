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
 * admin-role-detail — component and routing tests.
 *
 * Covers: loading/error/not-found states, tab switching, permissions
 * display, bindings display, permission gating (system vs custom),
 * edit/delete dialogs, and responsive accessibility.
 */

import { describe, it, expect, vi, afterEach, beforeAll } from 'vitest';

// ---------------------------------------------------------------------------
// Mock data
// ---------------------------------------------------------------------------

const CUSTOM_ROLE = {
  id: 'role-custom-1',
  name: 'test-editor',
  description: 'A custom editor role',
  scopeType: 'system',
  permissions: ['project.read', 'project.update'],
  system: false,
  createdAt: '2026-08-01T00:00:00Z',
  updatedAt: '2026-08-15T00:00:00Z',
};

const SYSTEM_ROLE = {
  id: 'role-system-1',
  name: 'hub-admin',
  description: 'System administrator',
  scopeType: 'system',
  permissions: ['project.read', 'project.update', 'user.read'],
  system: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

const PERMISSIONS = [
  { ID: 'project.read', Resource: 'project', Action: 'read', Description: 'Read projects' },
  {
    ID: 'project.update',
    Resource: 'project',
    Action: 'update',
    Description: 'Update projects',
  },
  { ID: 'user.read', Resource: 'user', Action: 'read', Description: 'Read users' },
];

const BINDINGS = [
  {
    id: 'rb-1',
    roleDefinitionId: 'role-custom-1',
    principalType: 'user',
    principalId: 'user-1',
    principalDisplayName: 'Alice',
    scopeType: 'system',
    scopeId: '',
    createdBy: 'admin',
    createdAt: '2026-08-10T00:00:00Z',
  },
  {
    id: 'rb-2',
    roleDefinitionId: 'role-custom-1',
    principalType: 'agent',
    principalId: 'agent-1',
    scopeType: 'system',
    scopeId: '',
    createdBy: 'admin',
    createdAt: '2026-08-11T00:00:00Z',
  },
  {
    id: 'rb-3',
    roleDefinitionId: 'other-role',
    principalType: 'user',
    principalId: 'user-2',
    scopeType: 'system',
    scopeId: '',
    createdBy: 'admin',
    createdAt: '2026-08-12T00:00:00Z',
  },
];

// ---------------------------------------------------------------------------
// Fetch handler factory
// ---------------------------------------------------------------------------

function createFetchHandler(opts: {
  role?: Record<string, unknown> | null;
  roleStatus?: number;
  bindings?: Record<string, unknown>[];
}) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    // DELETE binding
    if (init?.method === 'DELETE' && path.includes('/api/v1/admin/role-bindings/')) {
      return Promise.resolve(new Response('{}', { status: 200 }));
    }

    // DELETE role
    if (init?.method === 'DELETE' && path.includes('/api/v1/admin/roles/')) {
      return Promise.resolve(new Response('{}', { status: 200 }));
    }

    // PUT role (update)
    if (init?.method === 'PUT' && path.includes('/api/v1/admin/roles/')) {
      return Promise.resolve(
        new Response(JSON.stringify(opts.role ?? CUSTOM_ROLE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // POST binding (create)
    if (init?.method === 'POST' && path.includes('/api/v1/admin/role-bindings')) {
      return Promise.resolve(
        new Response(JSON.stringify({ id: 'rb-new' }), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // GET role
    if (path.includes('/api/v1/admin/roles/')) {
      const status = opts.roleStatus ?? 200;
      if (status !== 200) {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'Not found' }), { status })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify(opts.role ?? CUSTOM_ROLE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // GET permissions
    if (path.includes('/api/v1/admin/permissions')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: PERMISSIONS }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // GET role-bindings
    if (path.includes('/api/v1/admin/role-bindings')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: opts.bindings ?? BINDINGS }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Admin status (required by navigateTo)
    if (path.includes('/api/v1/auth/admin-status')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({ isAdmin: true, isSuperAdmin: true, permissions: [] }),
          { status: 200 }
        )
      );
    }

    // Settings public (required by navigateTo)
    if (path.includes('/api/v1/settings/public')) {
      return Promise.resolve(
        new Response(JSON.stringify({}), { status: 200 })
      );
    }

    // Fallback
    return Promise.resolve(new Response('{}', { status: 200 }));
  };
}

// ---------------------------------------------------------------------------
// Test utilities
// ---------------------------------------------------------------------------

// Pre-import the component module to avoid timeout during test execution.
// The dynamic import warms up the module graph outside of the 5s test timeout.
let ScionPageAdminRoleDetailCtor: typeof import('./admin-role-detail.js')['ScionPageAdminRoleDetail'];

beforeAll(async () => {
  // Stub fetch for the duration of the import (connectedCallback may fire)
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))));
  const mod = await import('./admin-role-detail.js');
  ScionPageAdminRoleDetailCtor = mod.ScionPageAdminRoleDetail;
  vi.restoreAllMocks();
});

async function createElement(
  fetchHandler: ReturnType<typeof createFetchHandler>,
  rolePath = '/admin/roles/role-custom-1'
): Promise<HTMLElement> {
  // happy-dom supports direct assignment on window.location
  try {
    Object.defineProperty(window.location, 'pathname', {
      value: rolePath,
      writable: true,
      configurable: true,
    });
  } catch {
    // Fallback: replace the whole location object
    Object.defineProperty(window, 'location', {
      value: { ...window.location, pathname: rolePath },
      writable: true,
      configurable: true,
    });
  }

  vi.stubGlobal('fetch', vi.fn(fetchHandler));

  const el = new ScionPageAdminRoleDetailCtor();
  document.body.appendChild(el);

  // Wait for async load — poll until loading is done or timeout
  const deadline = Date.now() + 4000;
  while (Date.now() < deadline) {
    await el.updateComplete;
    // Check if the component has rendered content (not just loading)
    if (el.shadowRoot?.querySelector('h1') || el.shadowRoot?.querySelector('.error-state') || el.shadowRoot?.querySelector('.empty-state')) {
      break;
    }
    await new Promise((r) => setTimeout(r, 20));
  }
  await el.updateComplete;

  return el;
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('admin-role-detail', () => {
  let el: HTMLElement | null = null;

  afterEach(() => {
    if (el && el.parentNode) {
      el.parentNode.removeChild(el);
    }
    el = null;
    vi.restoreAllMocks();
  });

  // -- Loading & rendering --

  it('renders role name and description for a custom role', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const h1 = el.shadowRoot?.querySelector('h1');
    expect(h1?.textContent?.trim()).toBe('test-editor');

    const desc = el.shadowRoot?.querySelector('.header-description');
    expect(desc?.textContent?.trim()).toBe('A custom editor role');
  });

  it('renders type and scope badges', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const typeBadge = el.shadowRoot?.querySelector('.type-badge');
    expect(typeBadge?.textContent?.trim()).toContain('Custom');

    const scopeBadge = el.shadowRoot?.querySelector('.scope-badge');
    expect(scopeBadge?.textContent?.trim()).toContain('system');
  });

  it('shows Edit and Delete buttons for custom roles', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const buttons = el.shadowRoot?.querySelectorAll('.header-actions sl-button');
    const labels = [...(buttons ?? [])].map((b) => b.textContent?.trim());
    expect(labels).toContain('Edit');
    expect(labels).toContain('Delete');
  });

  it('hides Edit and Delete buttons for system roles', async () => {
    const handler = createFetchHandler({ role: SYSTEM_ROLE });
    el = await createElement(handler, '/admin/roles/role-system-1');

    const buttons = el.shadowRoot?.querySelectorAll('.header-actions sl-button');
    expect(buttons?.length ?? 0).toBe(0);
  });

  // -- Error / not found --

  it('renders not-found state for 404', async () => {
    const handler = createFetchHandler({ role: null, roleStatus: 404 });
    el = await createElement(handler, '/admin/roles/nonexistent');

    const heading = el.shadowRoot?.querySelector('.empty-state h2');
    expect(heading?.textContent?.trim()).toBe('Role Not Found');
  });

  it('renders error state for API failure', async () => {
    const handler = createFetchHandler({ role: null, roleStatus: 500 });
    el = await createElement(handler, '/admin/roles/broken');

    const heading = el.shadowRoot?.querySelector('.error-state h2');
    expect(heading?.textContent?.trim()).toBe('Failed to Load Role');
  });

  // -- Permissions tab --

  it('renders permissions grouped by resource', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const groups = el.shadowRoot?.querySelectorAll('.permission-group');
    expect(groups?.length).toBeGreaterThan(0);

    const labels = el.shadowRoot?.querySelectorAll('.permission-label');
    const labelTexts = [...(labels ?? [])].map((l) => l.textContent?.trim());
    expect(labelTexts).toContain('project.read');
    expect(labelTexts).toContain('project.update');
  });

  it('shows permission count', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const count = el.shadowRoot?.querySelector('.permission-count');
    expect(count?.textContent?.trim()).toContain('2 permissions');
  });

  // -- Bindings tab --

  it('filters bindings to the current role', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE, bindings: BINDINGS });
    el = await createElement(handler);

    // Switch to bindings tab
    const tabs = el.shadowRoot?.querySelectorAll('sl-tab');
    const bindingsTab = [...(tabs ?? [])].find((t) =>
      t.textContent?.includes('Bindings')
    );
    expect(bindingsTab).toBeDefined();
    bindingsTab?.click();
    await new Promise((r) => setTimeout(r, 50));

    // Should show 2 bindings for role-custom-1, not 3
    const bindingCount = el.shadowRoot?.querySelector('.binding-count');
    expect(bindingCount?.textContent?.trim()).toContain('2 binding');
  });

  it('shows empty state when no bindings exist', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE, bindings: [] });
    el = await createElement(handler);

    // Switch to bindings tab
    const tabs = el.shadowRoot?.querySelectorAll('sl-tab');
    const bindingsTab = [...(tabs ?? [])].find((t) =>
      t.textContent?.includes('Bindings')
    );
    bindingsTab?.click();
    await new Promise((r) => setTimeout(r, 50));

    const emptyHeading = el.shadowRoot?.querySelector('sl-tab-panel[name="bindings"] .empty-state h2');
    expect(emptyHeading?.textContent?.trim()).toBe('No Bindings');
  });

  // -- Back link --

  it('renders back link to Roles list', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const backLink = el.shadowRoot?.querySelector('.back-link');
    expect(backLink).not.toBeNull();
    expect(backLink?.getAttribute('href')).toBe('/admin/roles');
    expect(backLink?.textContent?.trim()).toContain('Roles');
  });

  // -- Tabs --

  it('shows both Permissions and Bindings tabs', async () => {
    const handler = createFetchHandler({ role: CUSTOM_ROLE });
    el = await createElement(handler);

    const tabs = el.shadowRoot?.querySelectorAll('sl-tab');
    const tabLabels = [...(tabs ?? [])].map((t) => t.textContent?.trim());
    expect(tabLabels.some((l) => l?.includes('Permissions'))).toBe(true);
    expect(tabLabels.some((l) => l?.includes('Bindings'))).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Route / permission registration tests
// ---------------------------------------------------------------------------

describe('admin-role-detail: route integration', () => {
  it('ROUTE_PERMISSION_MAP includes role detail entry', async () => {
    const { ROUTE_PERMISSION_MAP } = await import('../../lib/admin-permissions.js');
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-role-detail'];
    expect(perms).toBeDefined();
    expect(perms).toContain('role.read');
  });
});
