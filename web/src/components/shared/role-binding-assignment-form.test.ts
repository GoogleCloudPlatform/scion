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
 * scion-role-binding-assignment-form — behavioral tests.
 *
 * Covers:
 *   - Project scope renders scion-project-picker, not raw sl-input
 *   - Cross-entry-point: shared form is used and Scope renders before Role
 *   - Switching scope clears incompatible selected role
 *   - Selection/change event updates scopeId
 *   - Switching project → system clears the project value
 *   - Reopening does not reuse a stale project
 *   - System-scoped binding flow: no picker renders for system scope
 *   - Locked principal is rendered read-only
 *   - Form validity requires role + project for project scope
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { AssignmentFormValues } from './role-binding-assignment-form.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let FormCtor: any;

const MOCK_ROLES = [
  { id: 'role-sys-1', name: 'hub-admin', scopeType: 'system' },
  { id: 'role-sys-2', name: 'hub-viewer', scopeType: 'system' },
  { id: 'role-proj-1', name: 'project-member', scopeType: 'project' },
  { id: 'role-proj-2', name: 'project-admin', scopeType: 'project' },
];

beforeAll(async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
  );
  const mod = await import('./role-binding-assignment-form.js');
  FormCtor = mod.ScionRoleBindingAssignmentForm;
  vi.restoreAllMocks();
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

async function createElement(
  props: Record<string, unknown> = {}
): Promise<InstanceType<typeof FormCtor>> {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
  );
  const el = new FormCtor();
  el.roles = MOCK_ROLES;
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

// ---------------------------------------------------------------------------
// Helper to query inside shadow DOM
// ---------------------------------------------------------------------------

function query(el: HTMLElement, sel: string): Element | null {
  return el.shadowRoot?.querySelector(sel) ?? null;
}

function queryAll(el: HTMLElement, sel: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(sel) ?? []);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('scion-role-binding-assignment-form', () => {
  // ── Field order: Scope renders before Role ──

  it('renders Scope selector before Role selector (field ordering)', async () => {
    const el = await createElement();

    // Get all sl-select elements in the shadow DOM
    const selects = queryAll(el, 'sl-select');
    const labels = selects.map((s) => s.getAttribute('label'));

    const scopeIdx = labels.indexOf('Scope');
    const roleIdx = labels.indexOf('Role');

    expect(scopeIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThanOrEqual(0);
    expect(scopeIdx).toBeLessThan(roleIdx);
  });

  // ── Project scope renders scion-project-picker ──

  it('renders scion-project-picker when scope is project, not raw sl-input', async () => {
    const el = await createElement();

    // Set scope to project
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (el as any)._scopeType = 'project';
    el.requestUpdate();
    await el.updateComplete;

    const picker = query(el, 'scion-project-picker');
    expect(picker).not.toBeNull();

    // Must not have a raw sl-input with label="Project ID"
    const inputs = queryAll(el, 'sl-input');
    const projectIdInput = inputs.find(
      (inp) => inp.getAttribute('label') === 'Project ID'
    );
    expect(projectIdInput).toBeUndefined();
  });

  // ── System scope: no picker ──

  it('does not render project picker for system scope', async () => {
    const el = await createElement();

    // Default scope is system
    const picker = query(el, 'scion-project-picker');
    expect(picker).toBeNull();
  });

  // ── Switching scope clears incompatible role ──

  it('clears selected role when scope changes to incompatible type', async () => {
    const el = await createElement();

    // Select a system-scope role
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._roleId = 'role-sys-1';
    comp._scopeType = 'system';
    comp.requestUpdate();
    await comp.updateComplete;

    // Change scope to project — the system role should be cleared
    comp._handleScopeChange('project');
    await comp.updateComplete;

    expect(comp._roleId).toBe('');
  });

  it('keeps compatible role when scope changes to same type', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._roleId = 'role-proj-1';
    comp._scopeType = 'project';
    comp.requestUpdate();
    await comp.updateComplete;

    // "Change" scope to project again — role should remain
    comp._handleScopeChange('project');
    await comp.updateComplete;

    expect(comp._roleId).toBe('role-proj-1');
  });

  // ── Switching project → system clears project value ──

  it('clears project value when scope changes from project to system', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._scopeType = 'project';
    comp._scopeId = 'proj-uuid-123';
    comp.requestUpdate();
    await comp.updateComplete;

    // Switch to system
    comp._handleScopeChange('system');
    await comp.updateComplete;

    expect(comp._scopeId).toBe('');
  });

  // ── form-change event with scopeId ──

  it('emits form-change with scopeId when project is selected', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._scopeType = 'project';
    comp._principalId = 'user-1';
    comp._roleId = 'role-proj-1';
    comp.requestUpdate();
    await comp.updateComplete;

    const events: AssignmentFormValues[] = [];
    el.addEventListener('form-change', (e: Event) => {
      events.push((e as CustomEvent<AssignmentFormValues>).detail);
    });

    // Simulate project picker emitting project-change
    comp._scopeId = 'proj-uuid-abc';
    comp._emitChange();
    await comp.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    const last = events[events.length - 1];
    expect(last.scopeId).toBe('proj-uuid-abc');
    expect(last.valid).toBe(true);
  });

  // ── Reset clears stale project ──

  it('reset() clears stale project and role values', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._scopeType = 'project';
    comp._scopeId = 'stale-project';
    comp._roleId = 'role-proj-1';
    comp.requestUpdate();
    await comp.updateComplete;

    // Reset should clear everything
    comp.reset();
    await comp.updateComplete;

    expect(comp._scopeType).toBe('system');
    expect(comp._scopeId).toBe('');
    expect(comp._roleId).toBe('');
  });

  // ── Locked principal ──

  it('renders locked principal as read-only (not editable)', async () => {
    const el = await createElement({
      lockedPrincipalType: 'user',
      lockedPrincipalId: 'user-uuid-123',
    });

    const lockedDiv = query(el, '.locked-principal');
    expect(lockedDiv).not.toBeNull();
    expect(lockedDiv?.textContent).toContain('user');
    expect(lockedDiv?.textContent).toContain('user-uuid-123');

    // Lock icon should be present
    const lockIcon = lockedDiv?.querySelector('sl-icon[name="lock"]');
    expect(lockIcon).not.toBeNull();

    // Principal picker should NOT be rendered
    const picker = query(el, 'scion-principal-picker');
    expect(picker).toBeNull();
  });

  it('renders editable principal picker when not locked', async () => {
    const el = await createElement();

    const picker = query(el, 'scion-principal-picker');
    expect(picker).not.toBeNull();

    const lockedDiv = query(el, '.locked-principal');
    expect(lockedDiv).toBeNull();
  });

  // ── Form validity ──

  it('form is invalid when role is not selected', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._principalId = 'user-1';
    comp._roleId = '';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.valid).toBe(false);
  });

  it('form is invalid when project scope but no project selected', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._principalId = 'user-1';
    comp._roleId = 'role-proj-1';
    comp._scopeType = 'project';
    comp._scopeId = '';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.valid).toBe(false);
  });

  it('form is valid when all required fields are set', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._principalId = 'user-1';
    comp._roleId = 'role-proj-1';
    comp._scopeType = 'project';
    comp._scopeId = 'proj-uuid-1';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.valid).toBe(true);
  });

  it('form is valid for system scope without project', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._principalId = 'user-1';
    comp._roleId = 'role-sys-1';
    comp._scopeType = 'system';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.valid).toBe(true);
  });

  // ── Locked scope ──

  it('hides scope selector when scope is locked', async () => {
    const el = await createElement({ lockedScopeType: 'project' });

    const scopeSelect = queryAll(el, 'sl-select').find(
      (s) => s.getAttribute('label') === 'Scope'
    );
    expect(scopeSelect).toBeUndefined();

    // Project picker should still render because locked scope is project
    const picker = query(el, 'scion-project-picker');
    expect(picker).not.toBeNull();
  });

  // ── Locked role ──

  it('hides role selector when role is locked', async () => {
    const el = await createElement({ lockedRoleId: 'role-sys-1' });

    const roleSelect = queryAll(el, 'sl-select').find(
      (s) => s.getAttribute('label') === 'Role'
    );
    expect(roleSelect).toBeUndefined();
  });

  // ── getFormValues includes locked values ──

  it('getFormValues returns locked principal values', async () => {
    const el = await createElement({
      lockedPrincipalType: 'user',
      lockedPrincipalId: 'locked-user-id',
    });

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._roleId = 'role-sys-1';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.principalType).toBe('user');
    expect(values.principalId).toBe('locked-user-id');
    expect(values.roleId).toBe('role-sys-1');
  });

  // ── Role filtering by scope type ──

  it('filters roles by current scope type', async () => {
    const el = await createElement();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;

    // System scope — should only show system roles
    comp._scopeType = 'system';
    comp.requestUpdate();
    await comp.updateComplete;

    const systemRoles = comp._filteredRoles;
    expect(systemRoles.every((r: { scopeType: string }) => r.scopeType === 'system')).toBe(true);
    expect(systemRoles.length).toBe(2);

    // Project scope — should only show project roles
    comp._scopeType = 'project';
    comp.requestUpdate();
    await comp.updateComplete;

    const projectRoles = comp._filteredRoles;
    expect(projectRoles.every((r: { scopeType: string }) => r.scopeType === 'project')).toBe(true);
    expect(projectRoles.length).toBe(2);
  });

  // ── Scope change emits form-change event ──

  it('emits form-change when scope changes', async () => {
    const el = await createElement();

    const events: AssignmentFormValues[] = [];
    el.addEventListener('form-change', (e: Event) => {
      events.push((e as CustomEvent<AssignmentFormValues>).detail);
    });

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (el as any)._handleScopeChange('project');
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].scopeType).toBe('project');
  });

  // ── Submitted body: getFormValues returns correct scopeId ──

  it('getFormValues returns selected scopeId and locked principal', async () => {
    const el = await createElement({
      lockedPrincipalType: 'user',
      lockedPrincipalId: 'user-abc',
    });

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._scopeType = 'project';
    comp._scopeId = 'proj-slug-or-uuid';
    comp._roleId = 'role-proj-1';
    comp.requestUpdate();
    await comp.updateComplete;

    const values = comp.getFormValues();
    expect(values.scopeId).toBe('proj-slug-or-uuid');
    expect(values.principalType).toBe('user');
    expect(values.principalId).toBe('user-abc');
    expect(values.roleId).toBe('role-proj-1');
    expect(values.scopeType).toBe('project');
    expect(values.valid).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Cross-entry-point: effective-role-provenance uses shared form
// ---------------------------------------------------------------------------

describe('effective-role-provenance uses shared assignment form', () => {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let EffectiveRoleCtor: any;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/role-bindings') && init?.method === 'POST') {
          return Promise.resolve(new Response(JSON.stringify({}), { status: 400 }));
        }
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(
            new Response(JSON.stringify({ items: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (path.includes('/api/v1/admin/roles')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                items: MOCK_ROLES,
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(new Response('{}', { status: 200 }));
      })
    );
    const mod = await import('./effective-role-provenance.js');
    EffectiveRoleCtor = mod.ScionEffectiveRoleProvenance;
    vi.restoreAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  async function createEffectiveRoleEl(): Promise<HTMLElement> {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/role-bindings') && init?.method === 'POST') {
          return Promise.resolve(new Response(JSON.stringify({}), { status: 400 }));
        }
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(
            new Response(JSON.stringify({ items: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (path.includes('/api/v1/admin/roles')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                items: MOCK_ROLES,
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(new Response('{}', { status: 200 }));
      })
    );

    const el = new EffectiveRoleCtor();
    el.principalType = 'user';
    el.principalId = 'test-user-id';
    document.body.appendChild(el);

    // Wait for data loading
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 300));
    await el.updateComplete;

    return el;
  }

  it('uses scion-role-binding-assignment-form in the add dialog', async () => {
    const el = await createEffectiveRoleEl();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    // Bypass capability gate
    comp._canCreate = true;
    comp._mutationPreChecked = true;
    comp.requestUpdate();
    await comp.updateComplete;

    // Open the add dialog
    await comp.openAddDialog();
    await new Promise((r) => setTimeout(r, 100));
    await comp.updateComplete;

    // The dialog should contain the shared form component
    const form = el.shadowRoot?.querySelector('scion-role-binding-assignment-form');
    expect(form).not.toBeNull();

    // Should NOT contain a raw sl-input for project ID
    const rawInput = el.shadowRoot?.querySelector('sl-input[label="Project ID"]');
    expect(rawInput).toBeNull();
  });

  it('renders project picker via shared form when scope is project', async () => {
    const el = await createEffectiveRoleEl();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._canCreate = true;
    comp._mutationPreChecked = true;
    comp.requestUpdate();
    await comp.updateComplete;

    await comp.openAddDialog();
    await new Promise((r) => setTimeout(r, 100));
    await comp.updateComplete;

    // Set scope to project via the shared form's internal state
    const form = el.shadowRoot?.querySelector(
      'scion-role-binding-assignment-form'
    ) as HTMLElement;
    expect(form).not.toBeNull();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const formComp = form as any;
    formComp._scopeType = 'project';
    formComp.requestUpdate();
    await formComp.updateComplete;

    // The shared form should render a project picker
    const picker = form?.shadowRoot?.querySelector('scion-project-picker');
    expect(picker).not.toBeNull();
  });

  it('locked principal is passed to the shared form', async () => {
    const el = await createEffectiveRoleEl();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._canCreate = true;
    comp._mutationPreChecked = true;
    comp.requestUpdate();
    await comp.updateComplete;

    await comp.openAddDialog();
    await new Promise((r) => setTimeout(r, 100));
    await comp.updateComplete;

    const form = el.shadowRoot?.querySelector(
      'scion-role-binding-assignment-form'
    ) as HTMLElement;
    expect(form).not.toBeNull();

    // Verify locked principal props are set
    expect(form?.getAttribute('lockedprincipaltype') || (form as Record<string, unknown>)['lockedPrincipalType']).toBeTruthy();

    // The shared form should show locked principal display
    const lockedDiv = form?.shadowRoot?.querySelector('.locked-principal');
    expect(lockedDiv).not.toBeNull();
    expect(lockedDiv?.textContent).toContain('test-user-id');
  });

  it('submitted body contains scopeId from the shared form', async () => {
    const postCalls: Array<{ body: string }> = [];

    const fetchMock = vi.fn((url: string | URL | Request, init?: RequestInit) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (init?.method === 'POST' && path.includes('/api/v1/admin/role-bindings')) {
        postCalls.push({ body: init.body as string });
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'rb-new' }), {
            status: 201,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (path.includes('/api/v1/admin/role-bindings')) {
        return Promise.resolve(
          new Response(JSON.stringify({ items: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (path.includes('/api/v1/admin/roles')) {
        return Promise.resolve(
          new Response(JSON.stringify({ items: MOCK_ROLES }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    vi.stubGlobal('fetch', fetchMock);

    const el = new EffectiveRoleCtor();
    el.principalType = 'user';
    el.principalId = 'user-submit-test';
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 300));
    await el.updateComplete;

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp._canCreate = true;
    comp._mutationPreChecked = true;
    comp.requestUpdate();
    await comp.updateComplete;

    // Set form state directly (as the form-change event handler does)
    // Wait for the update to propagate before calling createBinding
    comp._addScopeType = 'project';
    comp._addScopeId = 'proj-from-picker';
    comp._addRoleId = 'role-proj-1';
    comp._showAddDialog = true;
    comp.requestUpdate();
    await comp.updateComplete;
    await new Promise((r) => setTimeout(r, 50));

    // Clear any earlier POST calls from the capability probing
    postCalls.length = 0;

    await comp.createBinding();

    expect(postCalls.length).toBeGreaterThan(0);
    const body = JSON.parse(postCalls[0].body);
    expect(body.scopeId).toBe('proj-from-picker');
    expect(body.principalType).toBe('user');
    expect(body.principalId).toBe('user-submit-test');
    expect(body.scopeType).toBe('project');
    expect(body.roleDefinitionId).toBe('role-proj-1');
  });
});

// ---------------------------------------------------------------------------
// Cross-entry-point: admin-role-bindings uses shared form
// ---------------------------------------------------------------------------

describe('admin-role-bindings uses shared assignment form', () => {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let RoleBindingsCtor: any;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    const mod = await import('../pages/admin-role-bindings.js');
    RoleBindingsCtor = mod.ScionPageAdminRoleBindings;
    vi.restoreAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  async function createBindingsEl(): Promise<HTMLElement> {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({ items: [], totalCount: 0 }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        if (path.includes('/api/v1/admin/roles')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({ items: MOCK_ROLES }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(new Response('{}', { status: 200 }));
      })
    );

    const el = new RoleBindingsCtor();
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 300));
    await el.updateComplete;

    return el;
  }

  it('renders shared assignment form in the create dialog', async () => {
    const el = await createBindingsEl();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp.showCreateDialog = true;
    comp.requestUpdate();
    await comp.updateComplete;

    const form = el.shadowRoot?.querySelector('scion-role-binding-assignment-form');
    expect(form).not.toBeNull();
  });

  it('shared form in role-bindings renders Scope before Role', async () => {
    const el = await createBindingsEl();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp.showCreateDialog = true;
    comp.requestUpdate();
    await comp.updateComplete;

    const form = el.shadowRoot?.querySelector(
      'scion-role-binding-assignment-form'
    ) as HTMLElement;
    expect(form).not.toBeNull();

    const selects = Array.from(
      form?.shadowRoot?.querySelectorAll('sl-select') ?? []
    );
    const labels = selects.map((s) => s.getAttribute('label'));

    const scopeIdx = labels.indexOf('Scope');
    const roleIdx = labels.indexOf('Role');
    expect(scopeIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThanOrEqual(0);
    expect(scopeIdx).toBeLessThan(roleIdx);
  });
});

// ---------------------------------------------------------------------------
// Cross-entry-point: admin-role-detail uses shared form
// ---------------------------------------------------------------------------

describe('admin-role-detail uses shared assignment form', () => {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let RoleDetailCtor: any;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    const mod = await import('../pages/admin-role-detail.js');
    RoleDetailCtor = mod.ScionPageAdminRoleDetail;
    vi.restoreAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('renders shared assignment form when add binding form is shown', async () => {
    const PROJECT_ROLE = {
      id: 'role-proj-test',
      name: 'test-project-role',
      description: 'A test project role',
      scopeType: 'project',
      permissions: ['project.read'],
      system: false,
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/roles/')) {
          return Promise.resolve(
            new Response(JSON.stringify(PROJECT_ROLE), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(
            new Response(JSON.stringify({ items: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (path.includes('/api/v1/admin/permissions')) {
          return Promise.resolve(
            new Response(JSON.stringify({ items: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(new Response('{}', { status: 200 }));
      })
    );

    // Set location pathname
    try {
      Object.defineProperty(window.location, 'pathname', {
        value: '/admin/roles/role-proj-test',
        writable: true,
        configurable: true,
      });
    } catch {
      Object.defineProperty(window, 'location', {
        value: { ...window.location, pathname: '/admin/roles/role-proj-test' },
        writable: true,
        configurable: true,
      });
    }

    const el = new RoleDetailCtor();
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 500));
    await el.updateComplete;

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const comp = el as any;
    comp.showAddBindingForm = true;
    comp.requestUpdate();
    await comp.updateComplete;

    const form = el.shadowRoot?.querySelector('scion-role-binding-assignment-form');
    expect(form).not.toBeNull();
  });
});
