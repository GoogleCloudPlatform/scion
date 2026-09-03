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
 * Validates the canonical permission names used for Access Boundary
 * routes after the access_constraint → access_boundary rename.
 */

import { describe, it, expect } from 'vitest';
import {
  NAV_PERMISSION_MAP,
  ROUTE_PERMISSION_MAP,
  hasAnyPermission,
  type AdminStatus,
} from './admin-permissions.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function adminWithPermissions(...perms: string[]): AdminStatus {
  return { isAdmin: true, isSuperAdmin: false, permissions: perms };
}

// ---------------------------------------------------------------------------
// Permission rename: access_constraint.* → access_boundary.*
// ---------------------------------------------------------------------------

describe('admin-permissions: access_boundary permission rename', () => {
  it('NAV_PERMISSION_MAP uses access_boundary.read for the list route', () => {
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_boundary.read');
    expect(perms).toContain('access_boundary.admin');
  });

  it('NAV_PERMISSION_MAP does NOT contain legacy access_constraint permissions', () => {
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries'];
    expect(perms).not.toContain('access_constraint.read');
    expect(perms).not.toContain('access_constraint.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_boundary.* for list page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundaries'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_boundary.read');
    expect(perms).toContain('access_boundary.admin');
    expect(perms).not.toContain('access_constraint.read');
    expect(perms).not.toContain('access_constraint.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_boundary.* for detail page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundary-detail'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_boundary.read');
    expect(perms).toContain('access_boundary.admin');
    expect(perms).not.toContain('access_constraint.read');
    expect(perms).not.toContain('access_constraint.admin');
  });

  it('ROUTE_PERMISSION_MAP uses access_boundary.admin for editor page', () => {
    const perms = ROUTE_PERMISSION_MAP['scion-page-admin-access-boundary-editor'];
    expect(perms).toBeDefined();
    expect(perms).toContain('access_boundary.admin');
    expect(perms).not.toContain('access_constraint.admin');
  });

  it('no permission map entry contains the legacy access_constraint prefix', () => {
    const allPerms = [
      ...Object.values(NAV_PERMISSION_MAP).flat(),
      ...Object.values(ROUTE_PERMISSION_MAP).flat(),
    ];
    const legacyPerms = allPerms.filter((p) => p.startsWith('access_constraint.'));
    expect(legacyPerms).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// hasAnyPermission with access_boundary permissions
// ---------------------------------------------------------------------------

describe('hasAnyPermission: access_boundary permissions', () => {
  it('grants access when user holds access_boundary.read', () => {
    const admin = adminWithPermissions('access_boundary.read');
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(admin, perms)).toBe(true);
  });

  it('grants access when user holds access_boundary.admin', () => {
    const admin = adminWithPermissions('access_boundary.admin');
    const perms = NAV_PERMISSION_MAP['/admin/access-boundaries']!;
    expect(hasAnyPermission(admin, perms)).toBe(true);
  });

  it('denies access when user holds only legacy access_constraint.read', () => {
    const admin = adminWithPermissions('access_constraint.read');
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
