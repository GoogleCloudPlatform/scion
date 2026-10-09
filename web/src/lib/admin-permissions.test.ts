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
 * admin-permissions — permission mapping tests.
 *
 * Validates the canonical access_constraint.read and access_constraint.admin
 * permission IDs used for Access Constraints routes.
 */

import { describe, it, expect } from 'vitest';
import {
  NAV_PERMISSION_MAP,
  ROUTE_PERMISSION_MAP,
  canEditHubEnvVars,
  hasAnyPermission,
  isSettingsTabVisible,
  type AdminStatus,
} from './admin-permissions.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function adminWithPermissions(...perms: string[]): AdminStatus {
  return { isAdmin: true, isSuperAdmin: false, permissions: perms };
}

// ---------------------------------------------------------------------------
// Canonical access_constraint.* permissions
// ---------------------------------------------------------------------------

describe('admin-permissions: access_constraint permissions', () => {
  it('NAV_PERMISSION_MAP uses access_constraint.read for the list route', () => {
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_constraint.read');
    expect(perms).toContain('access_constraint.admin');
  });

  it('NAV_PERMISSION_MAP does NOT contain access_boundary permissions', () => {
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries'];
    expect(perms).not.toContain('access_boundary.read');
    expect(perms).not.toContain('access_boundary.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_constraint.* for list page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundaries'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_constraint.read');
    expect(perms).toContain('access_constraint.admin');
    expect(perms).not.toContain('access_boundary.read');
    expect(perms).not.toContain('access_boundary.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_constraint.* for detail page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundary-detail'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_constraint.read');
    expect(perms).toContain('access_constraint.admin');
    expect(perms).not.toContain('access_boundary.read');
    expect(perms).not.toContain('access_boundary.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_constraint.admin for editor page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundary-editor'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_constraint.admin');
    expect(perms).not.toContain('access_boundary.admin');
  });

  it('no permission map entry contains access_boundary prefix', () => {
    const allPerms = [
      ...Object.values(NAV_PERMISSION_MAP).flat(),
      ...Object.values(ROUTE_PERMISSION_MAP).flat(),
    ];
    const wrongPerms = allPerms.filter((p) => p.startsWith('access_boundary.'));
    expect(wrongPerms).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// hasAnyPermission with access_constraint permissions
// ---------------------------------------------------------------------------

describe('hasAnyPermission: access_constraint permissions', () => {
  it('grants access when user holds access_constraint.read', () => {
    const admin = adminWithPermissions('access_constraint.read');
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(admin, perms)).toBe(true);
  });

  it('grants access when user holds access_constraint.admin', () => {
    const admin = adminWithPermissions('access_constraint.admin');
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(admin, perms)).toBe(true);
  });

  it('denies access when user holds only access_boundary.read (wrong ID)', () => {
    const admin = adminWithPermissions('access_boundary.read');
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(admin, perms)).toBe(false);
  });

  it('super-admin always passes', () => {
    const superAdmin: AdminStatus = {
      isAdmin: true,
      isSuperAdmin: true,
      permissions: [],
    };
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(superAdmin, perms)).toBe(true);
  });

  it('returns false for null admin status', () => {
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(null, perms)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Settings tab gating: each tab is gated on the permission its data needs
// ---------------------------------------------------------------------------

describe('admin-permissions: settings environment variables and secrets tabs', () => {
  // System-scope permissions of the built-in roles relevant to these tabs.
  const hubAdmin = adminWithPermissions(
    'hub.settings.read',
    'hub.settings.update',
    'hub.env_vars.read'
  );
  const hubMember: AdminStatus = {
    isAdmin: true,
    isSuperAdmin: false,
    permissions: ['hub.settings.read', 'template.read'],
  };
  const superAdmin: AdminStatus = { isAdmin: true, isSuperAdmin: true, permissions: [] };

  it('shows the environment variables tab read-only to a hub admin', () => {
    expect(isSettingsTabVisible(hubAdmin, 'env-vars')).toBe(true);
    expect(canEditHubEnvVars(hubAdmin)).toBe(false);
  });

  it('keeps the secrets tab gated on hub.settings.read for a hub admin', () => {
    expect(isSettingsTabVisible(hubAdmin, 'secrets')).toBe(true);
    expect(isSettingsTabVisible(adminWithPermissions('hub.env_vars.read'), 'secrets')).toBe(false);
  });

  it('hides the environment variables tab without hub.env_vars.read', () => {
    expect(isSettingsTabVisible(hubMember, 'env-vars')).toBe(false);
  });

  it('shows both tabs, editable, to a super-admin', () => {
    expect(isSettingsTabVisible(superAdmin, 'env-vars')).toBe(true);
    expect(isSettingsTabVisible(superAdmin, 'secrets')).toBe(true);
    expect(canEditHubEnvVars(superAdmin)).toBe(true);
  });

  it('shows the settings nav item to a holder of hub.env_vars.read only', () => {
    const readOnly = adminWithPermissions('hub.env_vars.read');
    expect(hasAnyPermission(readOnly, NAV_PERMISSION_MAP['/settings']!)).toBe(true);
    expect(hasAnyPermission(readOnly, ROUTE_PERMISSION_MAP['scion-page-settings']!)).toBe(true);
  });

  it('denies everything for a null admin status', () => {
    expect(isSettingsTabVisible(null, 'env-vars')).toBe(false);
    expect(canEditHubEnvVars(null)).toBe(false);
  });
});
