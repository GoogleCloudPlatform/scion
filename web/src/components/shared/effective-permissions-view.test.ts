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
vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

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
        appliesInProjects: true,
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
        appliesInProjects: true,
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
        appliesInProjects: false,
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

function sampleProjectPermissionSet(): EffectivePermissionSet {
  return {
    principal: { type: 'user', id: 'user-target-1', status: 'active' },
    scope: {
      type: 'project',
      id: 'proj-alpha',
      name: 'Alpha Project',
      slug: 'alpha-proj',
    },
    statusGate: null,
    evaluatedAt: '2026-11-19T18:00:00Z',
    permissions: [
      {
        id: 'project.read',
        resource: 'project',
        action: 'read',
        description: 'Read project metadata',
        grants: [
          {
            bindingId: 'bind-proj-grp-1',
            role: { id: 'role-proj-member', name: 'project-member', system: true },
            scope: { type: 'project', id: 'proj-alpha' },
            via: { groupId: 'grp-eng', groupName: 'Project Eng', groupSlug: 'project-eng' },
            expiresAt: null,
          },
          {
            bindingId: 'bind-sys-direct-1',
            role: { id: 'role-hub-admin', name: 'hub-admin', system: true },
            scope: { type: 'system' },
            via: null,
            expiresAt: null,
          },
        ],
      },
    ],
    removed: [],
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

    // Hub-scope rows render "applies in all projects" vs "hub only" badges from appliesInProjects.
    const agentRow = host.querySelector('.permission-row[data-permission-id="agent.create"]');
    expect(agentRow?.querySelector('.scope-applicability-badge')?.textContent?.trim()).toBe(
      'applies in all projects'
    );
    const userRow = host.querySelector('.permission-row[data-permission-id="user.read"]');
    expect(userRow?.querySelector('.scope-applicability-badge')?.textContent?.trim()).toBe(
      'hub only'
    );
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

  it('renders Project role and Inherited from hub origin chips for project-scoped sets and omits applicability badges', () => {
    const data = sampleProjectPermissionSet();
    data.permissions[0].appliesInProjects = true;
    render(renderPermissionList(data), host);

    const chips = Array.from(host.querySelectorAll('.origin-chip'));
    const chipTexts = chips.map((c) => c.textContent?.replace(/\s+/g, ' ').trim());
    expect(chipTexts).toContain('Project role · project-member · via group: Project Eng');
    expect(chipTexts).toContain('Inherited from hub · hub-admin · direct');
    expect(host.querySelector('.scope-applicability-badge')).toBeNull();
  });

  it('renders a collapsed Removed by access constraints section with named and redacted boundaries', () => {
    const onCopy = vi.fn();
    const data = samplePermissionSet({
      removed: [
        {
          id: 'agent.delete',
          resource: 'agent',
          action: 'delete',
          description: 'Delete agents',
          grants: [
            {
              bindingId: 'bind-ops-1',
              role: { id: 'role-ops', name: 'ops-admin', system: false },
              scope: { type: 'system' },
              via: null,
              expiresAt: null,
            },
          ],
          removedBy: [
            {
              id: 'ac-1',
              name: 'contractors-cap',
              scopeType: 'system',
              redacted: false,
            },
          ],
        },
        {
          id: 'broker.delete',
          resource: 'broker',
          action: 'delete',
          description: 'Delete runtime brokers',
          grants: [
            {
              bindingId: 'bind-ops-2',
              role: { id: 'role-ops', name: 'ops-admin', system: false },
              scope: { type: 'system' },
              via: { groupId: 'grp-ops', groupName: 'Ops Team', groupSlug: 'ops-team' },
              expiresAt: null,
            },
          ],
          removedBy: [
            {
              id: '',
              name: '',
              scopeType: 'system',
              redacted: true,
            },
          ],
        },
      ],
    });
    render(renderPermissionList(data, { onCopyBindingId: onCopy }), host);

    const details = host.querySelector('sl-details.removed-permissions-details');
    expect(details).not.toBeNull();
    expect(details?.getAttribute('summary')).toBe('Removed by access constraints (2)');
    expect(details?.hasAttribute('open')).toBe(false);

    const rows = Array.from(details!.querySelectorAll('.removed-row'));
    expect(rows).toHaveLength(2);
    expect(rows[0].getAttribute('data-removed-permission-id')).toBe('agent.delete');
    expect(rows[0].textContent).toContain('ops-admin · direct');
    expect(rows[0].textContent).toContain('contractors-cap');
    expect(
      rows[0].querySelector('.removed-boundary-chip')?.getAttribute('data-boundary-redacted')
    ).toBe('false');

    expect(rows[1].getAttribute('data-removed-permission-id')).toBe('broker.delete');
    expect(rows[1].textContent).toContain('ops-admin · via group: Ops Team');
    expect(rows[1].textContent).toContain('Access constraint (details unavailable)');
    expect(
      rows[1].querySelector('.removed-boundary-chip')?.getAttribute('data-boundary-redacted')
    ).toBe('true');

    // Clicking the granting origin chip inside a removed row copies its binding ID.
    const removedGrantChip = rows[0].querySelector<HTMLButtonElement>('.origin-chip');
    expect(removedGrantChip).not.toBeNull();
    removedGrantChip!.click();
    expect(onCopy).toHaveBeenCalledWith('bind-ops-1');
  });

  it('renders statusGate banner instead of the empty list for suspended, invited, deleting, and missing users', () => {
    const cases: Array<{
      reason: 'suspended' | 'invited' | 'deleting' | 'missing';
      expectedText: string;
    }> = [
      { reason: 'suspended', expectedText: 'Suspended: holds no effective permissions' },
      { reason: 'invited', expectedText: 'Invited: holds no effective permissions' },
      { reason: 'deleting', expectedText: 'Deleting: holds no effective permissions' },
      { reason: 'missing', expectedText: 'Account unavailable: holds no effective permissions' },
    ];

    for (const tc of cases) {
      const data = samplePermissionSet({
        principal: { type: 'user', id: 'user-inactive', status: tc.reason },
        statusGate: { reason: tc.reason },
        permissions: [],
        removed: [],
      });
      render(renderPermissionList(data), host);

      const banner = host.querySelector('.status-gate-banner');
      expect(banner).not.toBeNull();
      expect(banner?.getAttribute('data-status-gate-reason')).toBe(tc.reason);
      expect(banner?.textContent).toContain(tc.expectedText);
      expect(host.querySelector('.empty-state')).toBeNull();
      expect(host.querySelector('.permission-groups')).toBeNull();
    }
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
          if (path.includes('projectId=proj-alpha')) {
            return Promise.resolve(jsonResponse(sampleProjectPermissionSet()));
          }
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

  it('does not fetch effective-permissions and renders explicit no-access state when caller lacks role_binding.read', async () => {
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
    const text = el.shadowRoot?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
    expect(text).toContain('You do not have permission to view effective permissions.');
  });

  it('shows explicit error state with Retry when loadAdminStatus fails, and loads on Retry', async () => {
    let adminStatusOk = false;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const path = requestPath(input);
        calls.push(path);
        if (path.includes('/api/v1/auth/admin-status')) {
          if (!adminStatusOk) {
            return Promise.resolve(jsonResponse({ error: { message: 'Temporary error' } }, 500));
          }
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

    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalId = 'user-target-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('/api/v1/auth/admin-status'))).toHaveLength(1);
    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(0);

    const errorEl = el.shadowRoot?.querySelector('.error-state');
    expect(errorEl).not.toBeNull();
    expect(errorEl?.textContent).toContain("Couldn't check your permissions");

    const retryBtn = errorEl?.querySelector<HTMLElement>('sl-button');
    expect(retryBtn).not.toBeNull();

    adminStatusOk = true;
    retryBtn!.click();
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('/api/v1/admin/effective-permissions'))).toHaveLength(1);
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('.perm-id').length).toBe(3);
  });

  it('renders collapsed By project section, shows picker on first open, and fetches ?projectId= on selection', async () => {
    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalType = 'user';
    el.principalId = 'user-target-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    const details = el.shadowRoot?.querySelector('sl-details.by-project-details');
    expect(details).not.toBeNull();
    expect(details?.getAttribute('summary')).toBe('By project');
    expect(details?.hasAttribute('open')).toBe(false);
    // Picker must not be mounted until first expand.
    expect(details?.querySelector('scion-project-picker')).toBeNull();

    // Expand By project -> picker mounts with hoist, still zero projectId requests.
    details!.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await el.updateComplete;
    const picker = details?.querySelector('scion-project-picker');
    expect(picker).not.toBeNull();
    expect(picker?.hasAttribute('hoist')).toBe(true);
    expect(calls.filter((c) => c.includes('projectId='))).toHaveLength(0);

    // Bubbled sl-hide from a child inside sl-details must NOT collapse sl-details.
    picker!.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(details?.hasAttribute('open')).toBe(true);

    // Partial search typing (displayLabel === '') must not fire a premature projectId request.
    picker!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'alp', displayLabel: '' },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(calls.filter((c) => c.includes('projectId='))).toHaveLength(0);

    // Selecting a project fetches ?projectId=proj-alpha and renders Project role / Inherited from hub chips.
    picker!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-alpha', displayLabel: 'Alpha Project' },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('projectId=proj-alpha'))).toHaveLength(1);
    const projSection = el.shadowRoot?.querySelector('.project-permissions-results');
    expect(projSection).not.toBeNull();
    const chipTexts = Array.from(projSection!.querySelectorAll('.origin-chip')).map((c) =>
      c.textContent?.replace(/\s+/g, ' ').trim()
    );
    expect(chipTexts).toContain('Project role · project-member · via group: Project Eng');
    expect(chipTexts).toContain('Inherited from hub · hub-admin · direct');
  });

  it('reloads selected project permissions lazily on reshow after invalidate() while inactive', async () => {
    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalType = 'user';
    el.principalId = 'user-target-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    // Expand By project and select proj-alpha.
    const details = el.shadowRoot?.querySelector('sl-details.by-project-details');
    expect(details).not.toBeNull();
    details!.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await el.updateComplete;

    const picker = details?.querySelector('scion-project-picker');
    expect(picker).not.toBeNull();
    picker!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-alpha', displayLabel: 'Alpha Project' },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('projectId=proj-alpha'))).toHaveLength(1);
    expect(el.shadowRoot?.querySelector('.project-permissions-results')).not.toBeNull();

    // Switch tab away and invalidate while inactive (D4d Add/Delete flow).
    el.active = false;
    await el.updateComplete;
    el.invalidate();
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));

    // Must not refetch while inactive.
    expect(calls.filter((c) => c.includes('projectId=proj-alpha'))).toHaveLength(1);

    // Reshow tab -> both hub and selected project permissions refetch and render.
    el.active = true;
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    expect(calls.filter((c) => c.includes('projectId=proj-alpha'))).toHaveLength(2);
    const projSection = el.shadowRoot?.querySelector('.project-permissions-results');
    expect(projSection).not.toBeNull();
    const chipTexts = Array.from(projSection!.querySelectorAll('.origin-chip')).map((c) =>
      c.textContent?.replace(/\s+/g, ' ').trim()
    );
    expect(chipTexts).toContain('Project role · project-member · via group: Project Eng');
    expect(chipTexts).toContain('Inherited from hub · hub-admin · direct');
  });

  it('renders ownership footnote, removed lists in hub and project sections, and guards nested removed sl-details events', async () => {
    const hubWithRemoved = samplePermissionSet({
      removed: [
        {
          id: 'agent.delete',
          resource: 'agent',
          action: 'delete',
          description: 'Delete agents',
          grants: [
            {
              bindingId: 'bind-direct-1',
              role: { id: 'role-hub-admin', name: 'hub-admin', system: true },
              scope: { type: 'system' },
              via: null,
              expiresAt: null,
            },
          ],
          removedBy: [
            {
              id: '',
              name: '',
              scopeType: 'system',
              redacted: true,
            },
          ],
        },
      ],
    });
    const projWithRemoved: EffectivePermissionSet = {
      ...sampleProjectPermissionSet(),
      removed: [
        {
          id: 'agent.delete',
          resource: 'agent',
          action: 'delete',
          description: 'Delete agents',
          grants: [
            {
              bindingId: 'bind-sys-direct-1',
              role: { id: 'role-hub-admin', name: 'hub-admin', system: true },
              scope: { type: 'system' },
              via: null,
              expiresAt: null,
            },
          ],
          removedBy: [
            {
              id: 'ac-proj-1',
              name: 'alpha-no-delete',
              scopeType: 'project',
              scopeId: 'proj-alpha',
              redacted: false,
            },
          ],
        },
      ],
    };

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
          if (path.includes('projectId=proj-alpha')) {
            return Promise.resolve(jsonResponse(projWithRemoved));
          }
          return Promise.resolve(jsonResponse(hubWithRemoved));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalType = 'user';
    el.principalId = 'user-target-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    // Ownership footnote is rendered.
    const footnote = el.shadowRoot?.querySelector('.ownership-footnote');
    expect(footnote).not.toBeNull();
    expect(footnote?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Some permissions (for example attaching to your own agents) are granted per resource by ownership and are not listed here.'
    );

    // Hub removed section is collapsed and shows redacted boundary text.
    const hubRemovedDetails = el.shadowRoot?.querySelector(
      'sl-details.removed-permissions-details'
    );
    expect(hubRemovedDetails).not.toBeNull();
    expect(hubRemovedDetails?.getAttribute('summary')).toBe('Removed by access constraints (1)');
    expect(hubRemovedDetails?.hasAttribute('open')).toBe(false);
    expect(hubRemovedDetails?.textContent).toContain('Access constraint (details unavailable)');

    // Expand By project, pick proj-alpha, verify project removed section and event isolation.
    const byProjectDetails = el.shadowRoot?.querySelector('sl-details.by-project-details');
    expect(byProjectDetails).not.toBeNull();
    byProjectDetails!.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await el.updateComplete;

    const picker = byProjectDetails!.querySelector('scion-project-picker');
    expect(picker).not.toBeNull();
    picker!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-alpha', displayLabel: 'Alpha Project' },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    const projRemovedDetails = byProjectDetails!.querySelector(
      'sl-details.removed-permissions-details'
    );
    expect(projRemovedDetails).not.toBeNull();
    expect(projRemovedDetails?.getAttribute('summary')).toBe('Removed by access constraints (1)');
    expect(projRemovedDetails?.textContent).toContain('alpha-no-delete');

    // Collapsing the nested removed-permissions sl-details must NOT collapse by-project-details.
    projRemovedDetails!.dispatchEvent(
      new CustomEvent('sl-hide', { bubbles: true, composed: true })
    );
    await el.updateComplete;
    expect(byProjectDetails?.hasAttribute('open')).toBe(true);
  });

  it('renders statusGate banner in component view for a suspended user', async () => {
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
          return Promise.resolve(
            jsonResponse(
              samplePermissionSet({
                principal: { type: 'user', id: 'user-susp-1', status: 'suspended' },
                statusGate: { reason: 'suspended' },
                permissions: [],
                removed: [],
              })
            )
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    el = document.createElement(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    el.principalType = 'user';
    el.principalId = 'user-susp-1';
    el.active = true;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await el.updateComplete;

    const banner = el.shadowRoot?.querySelector('.status-gate-banner');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('Suspended: holds no effective permissions');
    expect(el.shadowRoot?.querySelector('.empty-state')).toBeNull();
    expect(el.shadowRoot?.querySelector('.filter-input')).toBeNull();
  });
});

describe('scion-page-admin-users two-tab Roles dialog', () => {
  let pageEl: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;
  let calls: Array<{ method: string; path: string }> = [];

  beforeEach(async () => {
    clearAdminStatus();
    stateManager.setCurrentUserId('admin-self');
    calls = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request, init?: RequestInit) => {
        const path = requestPath(input);
        const method = init?.method ?? 'GET';
        calls.push({ method, path });
        if (path.includes('/auth/me')) {
          return Promise.resolve(jsonResponse({ id: 'admin-self', role: 'admin' }));
        }
        if (path.includes('/api/v1/auth/admin-status')) {
          return Promise.resolve(
            jsonResponse({
              isAdmin: true,
              isSuperAdmin: false,
              permissions: ['role_binding.read', 'role_binding.create', 'role_binding.delete'],
            })
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
        if (path.includes('/api/v1/admin/roles')) {
          return Promise.resolve(
            jsonResponse({
              items: [{ id: 'role-auditor', name: 'auditor', scopeType: 'system' }],
            })
          );
        }
        if (method === 'POST' && path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(jsonResponse({ id: 'bind-added-1' }, 201));
        }
        if (method === 'DELETE' && path.includes('/api/v1/admin/role-bindings/')) {
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        if (path.includes('/api/v1/admin/role-bindings')) {
          return Promise.resolve(
            jsonResponse({
              items: [
                {
                  id: 'bind-direct-1',
                  roleDefinitionId: 'role-hub-admin',
                  roleName: 'hub-admin',
                  principalType: 'user',
                  principalId: 'user-target-1',
                  scopeType: 'system',
                  scopeId: '',
                  createdAt: '2026-01-01T00:00:00Z',
                  source: 'direct',
                },
              ],
              totalCount: 1,
            })
          );
        }
        if (path.includes('/api/v1/admin/effective-permissions')) {
          if (path.includes('projectId=proj-alpha')) {
            return Promise.resolve(jsonResponse(sampleProjectPermissionSet()));
          }
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

    // Click Roles on the target row.
    const menuItems = Array.from(pageEl.shadowRoot?.querySelectorAll('sl-menu-item') ?? []);
    const rolesItem = menuItems.find((item) => item.textContent?.trim() === 'Roles');
    expect(rolesItem).toBeDefined();
    (rolesItem as HTMLElement).click();
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 60));

    const dialog = pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog');
    expect(dialog).not.toBeNull();
    expect(dialog?.getAttribute('label')).toBe('Roles — Target User');

    const tabs = Array.from(dialog!.querySelectorAll('sl-tab'));
    expect(tabs.map((t) => t.getAttribute('panel'))).toEqual(['roles', 'permissions']);

    // Initial tab is roles -> no effective-permissions request yet.
    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(0);

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

    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(1);

    // Collapsing the By project details or closing the project picker must NOT close the dialog.
    const permView = dialog!.querySelector(
      'scion-effective-permissions-view'
    ) as ScionEffectivePermissionsView;
    const details = permView.shadowRoot?.querySelector('sl-details.by-project-details');
    expect(details).not.toBeNull();
    details!.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await permView.updateComplete;
    await pageEl.updateComplete;

    const picker = details!.querySelector('scion-project-picker');
    expect(picker).not.toBeNull();
    picker!.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true, composed: true }));
    picker!.dispatchEvent(new CustomEvent('sl-request-close', { bubbles: true, composed: true }));
    details!.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true, composed: true }));
    await permView.updateComplete;
    await pageEl.updateComplete;
    expect(pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog')).not.toBeNull();

    // Direct sl-request-close on the dialog itself closes it.
    dialog!.dispatchEvent(new CustomEvent('sl-request-close', { bubbles: true, composed: true }));
    await pageEl.updateComplete;
    expect(pageEl.shadowRoot?.querySelector('sl-dialog.roles-dialog')).toBeNull();
  });

  it('invalidates cached permissions after adding or deleting a binding so the next tab show refetches', async () => {
    pageEl = document.createElement('scion-page-admin-users') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    document.body.appendChild(pageEl);
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 100));
    await pageEl.updateComplete;

    const rolesItem = Array.from(pageEl.shadowRoot?.querySelectorAll('sl-menu-item') ?? []).find(
      (item) => item.textContent?.trim() === 'Roles'
    ) as HTMLElement;
    expect(rolesItem).toBeDefined();
    rolesItem.click();
    await pageEl.updateComplete;
    await new Promise((r) => setTimeout(r, 100));
    await pageEl.updateComplete;

    const dialog = pageEl.shadowRoot!.querySelector('sl-dialog.roles-dialog')!;
    const tabGroup = dialog.querySelector('sl-tab-group')!;

    // 1. Show permissions tab -> initial fetch (#1).
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
    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(1);

    // 2. Switch back to roles tab and add a binding.
    tabGroup.dispatchEvent(
      new CustomEvent('sl-tab-show', {
        detail: { name: 'roles' },
        bubbles: true,
        composed: true,
      })
    );
    await pageEl.updateComplete;

    const provenance = dialog.querySelector('scion-effective-role-provenance')!;
    const addBtn = Array.from(provenance.shadowRoot?.querySelectorAll('sl-button') ?? []).find(
      (b) => b.textContent?.includes('Add Binding')
    ) as HTMLElement;
    expect(addBtn).toBeDefined();
    addBtn.click();
    await new Promise((r) => setTimeout(r, 80));

    const form = provenance.shadowRoot?.querySelector('scion-role-binding-assignment-form');
    expect(form).not.toBeNull();
    form!.dispatchEvent(
      new CustomEvent('form-change', {
        detail: { roleId: 'role-auditor', scopeType: 'system', scopeId: '' },
        bubbles: true,
        composed: true,
      })
    );
    await new Promise((r) => setTimeout(r, 30));

    const assignBtn = Array.from(
      provenance.shadowRoot?.querySelectorAll('sl-dialog sl-button') ?? []
    ).find((b) => b.textContent?.trim() === 'Assign Role') as HTMLElement;
    expect(assignBtn).toBeDefined();
    assignBtn.click();
    await new Promise((r) => setTimeout(r, 80));

    expect(
      calls.filter((c) => c.method === 'POST' && c.path.includes('/api/v1/admin/role-bindings'))
    ).toHaveLength(1);
    // While still on the roles tab, permissions was invalidated lazily (no eager fetch yet).
    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(1);

    // 3. Switch to permissions tab -> refetches (#2).
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
    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(2);

    // 4. Switch back to roles tab and delete the direct binding.
    tabGroup.dispatchEvent(
      new CustomEvent('sl-tab-show', {
        detail: { name: 'roles' },
        bubbles: true,
        composed: true,
      })
    );
    await pageEl.updateComplete;

    const trashBtn = provenance.shadowRoot?.querySelector<HTMLElement>(
      'sl-icon-button[name="trash"]'
    );
    expect(trashBtn).not.toBeNull();
    trashBtn!.click();
    await new Promise((r) => setTimeout(r, 80));

    expect(
      calls.filter((c) => c.method === 'DELETE' && c.path.includes('/api/v1/admin/role-bindings/'))
    ).toHaveLength(1);

    // 5. Switch to permissions tab -> refetches (#3).
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
    expect(
      calls.filter((c) => c.path.includes('/api/v1/admin/effective-permissions'))
    ).toHaveLength(3);
  });
});
