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
 * Shared permission utilities for admin UI progressive exposure.
 *
 * Centralizes the AdminStatus type, permission checking helper, and
 * static mappings from nav items / routes to gate permissions. All nav,
 * route guard, and settings page changes import from this module.
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/**
 * Enhanced admin status returned by GET /api/v1/auth/admin-status.
 * Phase 1 added the `permissions` array to the response.
 */
export interface AdminStatus {
  isAdmin: boolean;
  isSuperAdmin: boolean;
  permissions: string[];
}

// ---------------------------------------------------------------------------
// Permission checker
// ---------------------------------------------------------------------------

/**
 * Check whether the user holds ANY of the specified gate permissions.
 * Super-admin users always pass (they hold all permissions implicitly).
 *
 * @param adminStatus - The user's admin status, or null if unauthenticated.
 * @param requiredPermissions - One or more permission IDs; the check uses
 *   OR logic (any single match is sufficient).
 * @returns true if the user should be granted access.
 */
export function hasAnyPermission(
  adminStatus: AdminStatus | null,
  requiredPermissions: string[]
): boolean {
  if (!adminStatus) return false;
  if (adminStatus.isSuperAdmin) return true;
  return requiredPermissions.some((p) => adminStatus.permissions?.includes(p));
}

// ---------------------------------------------------------------------------
// Settings tab gate permissions (union used for the /settings nav item)
// ---------------------------------------------------------------------------

/**
 * The complete set of permissions that gate individual settings tabs.
 * The `/settings` nav item is visible when the user holds ANY of these.
 */
const SETTINGS_PERMISSIONS: string[] = [
  'hub.settings.read',
  'template.list',
  'template.read',
  'harness_config.list',
  'harness_config.read',
  'hub.lifecycle_hooks.read',
  'gcp_service_account.list',
  'gcp_service_account.read',
  'skill.list',
  'skill.read',
  'hub.project_defaults.read',
];

// ---------------------------------------------------------------------------
// Nav-item-to-permission mapping
// ---------------------------------------------------------------------------

/**
 * Maps each admin-scopeable nav item path to the gate permissions that
 * control its visibility. An item is shown when the user holds ANY of
 * its gate permissions (OR logic).
 */
export const NAV_PERMISSION_MAP: Record<string, string[]> = {
  '/settings': SETTINGS_PERMISSIONS,
  '/admin/server-config': ['hub.config.read'],
  '/admin/federation': ['hub.federation.read'],
  '/admin/integrations': ['hub.integrations.read'],
  '/admin/scheduler': ['hub.scheduler.read'],
  '/admin/users': ['user.read', 'user.list'],
  '/admin/groups': ['group.read', 'group.list'],
  '/admin/roles': ['role.read'],
  '/admin/role-bindings': ['role_binding.read'],
  '/admin/access-boundaries': ['access_constraint.read', 'access_constraint.admin'],
  '/admin/quotas': ['quota.read'],
  '/health': ['hub.health.read'],
  '/admin/skill-registries': ['skill.register'],
};

// ---------------------------------------------------------------------------
// Route-to-permission mapping (keyed by component tag)
// ---------------------------------------------------------------------------

/**
 * Maps each admin route's component tag to the gate permissions that
 * control access. Mirrors the nav mapping but uses component tags for
 * route-guard integration.
 */
export const ROUTE_PERMISSION_MAP: Record<string, string[]> = {
  'scion-page-settings': SETTINGS_PERMISSIONS,
  'scion-page-admin-server-config': ['hub.config.read'],
  'scion-page-admin-federation': ['hub.federation.read'],
  'scion-page-admin-integrations': ['hub.integrations.read'],
  'scion-page-admin-scheduler': ['hub.scheduler.read'],
  'scion-page-admin-users': ['user.read', 'user.list'],
  'scion-page-admin-groups': ['group.read', 'group.list'],
  'scion-page-admin-group-detail': ['group.read', 'group.list'],
  'scion-page-admin-roles': ['role.read'],
  'scion-page-admin-role-bindings': ['role_binding.read'],
  'scion-page-admin-access-boundaries': ['access_constraint.read', 'access_constraint.admin'],
  'scion-page-admin-access-boundary-detail': ['access_constraint.read', 'access_constraint.admin'],
  'scion-page-admin-access-boundary-editor': ['access_constraint.admin'],
  'scion-page-admin-quotas': ['quota.read'],
  'scion-page-health-dashboard': ['hub.health.read'],
  'scion-page-admin-skill-registries': ['skill.register'],
  'scion-page-admin-skill-registry-detail': ['skill.register'],
};

// ---------------------------------------------------------------------------
// Settings tab gate permissions
// ---------------------------------------------------------------------------

/**
 * Maps each settings tab panel name to the gate permissions that control
 * its visibility. A tab is shown when the user holds ANY of its gate
 * permissions (OR logic). Super-admin users see all tabs.
 */
export const TAB_PERMISSION_MAP: Record<string, string[]> = {
  'env-vars': ['hub.settings.read'],
  secrets: ['hub.settings.read'],
  templates: ['template.list', 'template.read'],
  'harness-configs': ['harness_config.list', 'harness_config.read'],
  'pre-start-hooks': ['hub.lifecycle_hooks.read'],
  'service-accounts': ['gcp_service_account.list', 'gcp_service_account.read'],
  skills: ['skill.list', 'skill.read'],
  'project-templates': ['hub.project_defaults.read'],
};

// ---------------------------------------------------------------------------
// Super-admin-only routes
// ---------------------------------------------------------------------------

/**
 * Routes that require super-admin (`user.role === 'admin'`).
 * These are not gated by the permissions array — only super-admins
 * may access them.
 */
export const SUPERADMIN_ROUTES = new Set([
  'scion-page-diagnostics',
  'scion-page-admin-maintenance',
]);
