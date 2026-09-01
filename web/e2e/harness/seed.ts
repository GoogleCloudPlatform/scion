// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Seed helpers for E2E tests.
 *
 * All operations call the real hub API using an admin access token.
 * The admin session must be created (via auth.ts) before calling these.
 */

// ── Types ─────────────────────────────────────────────────────────────────

export interface SeedUser {
  id: string;
  email: string;
  displayName: string;
  role: string;
}

export interface SeedGroup {
  id: string;
  name: string;
  slug: string;
  groupType: string;
}

export interface SeedMember {
  memberType: string;
  memberId: string;
  role: string;
}

export interface SeedRoleBinding {
  id: string;
  roleDefinitionId: string;
  principalType: string;
  principalId: string;
}

export interface SeedData {
  adminUser: SeedUser;
  groups: SeedGroup[];
  users: SeedUser[];
  roleBindings: SeedRoleBinding[];
}

// ── API helpers ───────────────────────────────────────────────────────────

/**
 * Make an authenticated API request to the hub.
 */
async function apiRequest(
  baseURL: string,
  accessToken: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<Response> {
  const url = `${baseURL}${path}`;
  const headers: Record<string, string> = {
    Authorization: `Bearer ${accessToken}`,
  };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }

  const res = await fetch(url, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });

  return res;
}

/**
 * Make an API request and parse the JSON response, throwing on non-2xx.
 */
async function apiJSON<T>(
  baseURL: string,
  accessToken: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const res = await apiRequest(baseURL, accessToken, method, path, body);
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`API ${method} ${path} failed (${res.status}): ${text}`);
  }
  return (await res.json()) as T;
}

// ── Seed functions ────────────────────────────────────────────────────────

/**
 * Create a group via the API.
 */
export async function createGroup(
  baseURL: string,
  accessToken: string,
  opts: {
    name: string;
    slug?: string;
    description?: string;
    labels?: Record<string, string>;
  },
): Promise<SeedGroup> {
  const group = await apiJSON<SeedGroup>(
    baseURL,
    accessToken,
    'POST',
    '/api/v1/groups',
    {
      name: opts.name,
      slug: opts.slug,
      description: opts.description,
      labels: opts.labels,
    },
  );
  console.log(`[seed] Created group: ${group.name} (${group.id})`);
  return group;
}

/**
 * Add a member to a group.
 */
export async function addGroupMember(
  baseURL: string,
  accessToken: string,
  groupId: string,
  member: SeedMember,
): Promise<void> {
  await apiJSON(baseURL, accessToken, 'POST', `/api/v1/groups/${groupId}/members`, {
    memberType: member.memberType,
    memberId: member.memberId,
    role: member.role,
  });
  console.log(
    `[seed] Added ${member.memberType} ${member.memberId} to group ${groupId} as ${member.role}`,
  );
}

/**
 * Create a role binding.
 */
export async function createRoleBinding(
  baseURL: string,
  accessToken: string,
  opts: {
    roleDefinitionId: string;
    principalType: string;
    principalId: string;
    scopeType?: string;
    scopeId?: string;
  },
): Promise<SeedRoleBinding> {
  const binding = await apiJSON<SeedRoleBinding>(
    baseURL,
    accessToken,
    'POST',
    '/api/v1/admin/role-bindings',
    {
      roleDefinitionId: opts.roleDefinitionId,
      principalType: opts.principalType,
      principalId: opts.principalId,
      scopeType: opts.scopeType || 'system',
      scopeId: opts.scopeId || '*',
    },
  );
  console.log(
    `[seed] Created role binding: ${opts.roleDefinitionId} for ${opts.principalType}:${opts.principalId}`,
  );
  return binding;
}

/**
 * List role definitions to find a role by name.
 */
export async function findRoleDefinition(
  baseURL: string,
  accessToken: string,
  name: string,
): Promise<{ id: string; name: string } | null> {
  const res = await apiJSON<{ items: Array<{ id: string; name: string }> }>(
    baseURL,
    accessToken,
    'GET',
    '/api/v1/admin/roles',
  );
  return res.items.find((r) => r.name === name) || null;
}

/**
 * Set the max_members_per_group quota to a low value.
 *
 * This finds the seeded limit definition and updates its default value.
 * Used to test quota-exceeded scenarios in E2.
 */
export async function setMaxMembersPerGroupQuota(
  baseURL: string,
  accessToken: string,
  maxMembers: number,
): Promise<void> {
  // List limit definitions to find max_members_per_group
  const limits = await apiJSON<{
    items: Array<{ id: string; name: string; defaultValue: number }>;
  }>(baseURL, accessToken, 'GET', '/api/v1/admin/limits');

  const memberLimit = limits.items.find(
    (l) => l.name === 'max_members_per_group',
  );

  if (!memberLimit) {
    console.warn(
      '[seed] max_members_per_group limit not found — skipping quota setup',
    );
    return;
  }

  await apiRequest(
    baseURL,
    accessToken,
    'PUT',
    `/api/v1/admin/limits/${memberLimit.id}`,
    {
      ...memberLimit,
      defaultValue: maxMembers,
    },
  );

  console.log(
    `[seed] Set max_members_per_group quota to ${maxMembers}`,
  );
}

/**
 * Create an access constraint (boundary) on a group.
 *
 * This requires the preview flow: first create a preview, then use its
 * token to finalize the constraint. Used for constraint-gate E2E scenarios
 * in E2.
 */
export async function createAccessBoundary(
  baseURL: string,
  accessToken: string,
  opts: {
    name: string;
    purpose: string;
    groupId: string;
  },
): Promise<{ id: string }> {
  // Step 1: Create a preview
  const preview = await apiJSON<{ previewToken: string; id: string }>(
    baseURL,
    accessToken,
    'POST',
    '/api/v1/admin/access-constraint-previews',
    {
      name: opts.name,
      purpose: opts.purpose,
      subject: {
        kind: 'group_closure',
        groupId: opts.groupId,
      },
      rules: [],
    },
  );

  // Step 2: Finalize with the preview token
  const constraint = await apiJSON<{ id: string }>(
    baseURL,
    accessToken,
    'POST',
    '/api/v1/admin/access-constraints',
    {
      name: opts.name,
      purpose: opts.purpose,
      previewToken: preview.previewToken,
      subject: {
        kind: 'group_closure',
        groupId: opts.groupId,
      },
      rules: [],
    },
  );

  console.log(
    `[seed] Created access boundary "${opts.name}" on group ${opts.groupId}`,
  );
  return constraint;
}

// ── Default E2E seed ──────────────────────────────────────────────────────

/**
 * Seed the minimum data needed for the E2E smoke test:
 * - A test group visible on /admin/groups
 *
 * The `devToken` is the dev-auth Bearer token printed by the hub on startup.
 * The dev user has a super-admin role binding, so it can create any resource.
 *
 * Returns the seeded data for test assertions.
 */
export async function seedSmokeData(
  baseURL: string,
  devToken: string,
): Promise<SeedData> {
  // Create a test group
  const group = await createGroup(baseURL, devToken, {
    name: 'E2E Test Group',
    slug: 'e2e-test-group',
    description: 'Group created by E2E smoke test seeding',
    labels: { env: 'e2e', purpose: 'smoke-test' },
  });

  return {
    adminUser: {
      id: 'dev',
      email: 'dev@localhost',
      displayName: 'Development User',
      role: 'admin',
    },
    groups: [group],
    users: [],
    roleBindings: [],
  };
}
