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

import type { Page } from '@playwright/test';

export const PROJECT_ID = '33333333-3333-3333-3333-333333333333';
export const SELF_USER_ID = 'user-owner';
export const OTHER_USER_ID = 'user-member';
export const LONG_SELF_NAME = 'Maximiliana Bartholomew-Featherstonehaugh';
export const LONG_OTHER_NAME = 'jonathan.fitzgerald-smith@example-corp.com';

export interface ProjectMembersMockOptions {
  showVisitorsTab?: boolean;
  longNames?: boolean;
}

export async function setupProjectMembersHub(
  page: Page,
  opts: ProjectMembersMockOptions = {}
): Promise<void> {
  const showVisitorsTab = opts.showVisitorsTab ?? true;
  const longNames = opts.longNames ?? false;

  await page.addInitScript(
    ({ visitorsFlag }) => {
      window.localStorage.setItem('scion-theme', 'dark');
      window.__SCION_FEATURES__ = {
        'federation.project_visitors': visitorsFlag,
      };
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        onerror: (() => void) | null = null;
        readyState = 1;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    { visitorsFlag: showVisitorsTab }
  );

  await page.route('**/api/**', (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/projects')) {
      return route.fulfill({
        json: { projects: [{ id: PROJECT_ID, name: 'web-console', slug: 'web-console' }] },
      });
    }
    if (path.endsWith('/agents')) return route.fulfill({ json: { agents: [] } });
    return route.fulfill({ json: {} });
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: {
        id: SELF_USER_ID,
        email: 'owner@example.test',
        displayName: 'Preston Holmes',
        role: 'admin',
      },
    })
  );

  await page.route('**/api/v1/settings/public', (route) => route.fulfill({ json: {} }));

  await page.route('**/api/v1/experiments', (route) =>
    route.fulfill({
      json: {
        experiments: {
          'federation.project_visitors': showVisitorsTab,
        },
      },
    })
  );

  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  await page.route(`**/api/v1/projects/${PROJECT_ID}`, (route) =>
    route.fulfill({
      json: {
        id: PROJECT_ID,
        name: 'web-console',
        slug: 'web-console',
        _capabilities: {
          actions: showVisitorsTab
            ? ['read', 'update', 'manage', 'manage_visitors']
            : ['read', 'update', 'manage'],
        },
      },
    })
  );

  await page.route(`**/api/v1/projects/${PROJECT_ID}/settings`, (route) =>
    route.fulfill({ json: { settings: {} } })
  );

  await page.route(`**/api/v1/projects/${PROJECT_ID}/settings/resolved`, (route) =>
    route.fulfill({ json: { project: {}, settings: {} } })
  );

  await page.route(`**/api/v1/projects/${PROJECT_ID}/members/assignable-roles`, (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: 'role-owner',
            name: 'project-owner',
            description: 'Full project control',
            roleKind: 'builtin',
            grantable: true,
          },
          {
            id: 'role-admin',
            name: 'project-admin',
            description: 'Manage project settings and members',
            roleKind: 'builtin',
            grantable: true,
          },
          {
            id: 'role-member',
            name: 'project-member',
            description: 'Standard project access',
            roleKind: 'builtin',
            grantable: true,
          },
        ],
      },
    })
  );

  await page.route(`**/api/v1/projects/${PROJECT_ID}/members**`, (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/assignable-roles')) return route.fallback();
    const selfBindings = [
      {
        id: 'b-1',
        roleDefinitionId: 'role-owner',
        roleName: 'project-owner',
        roleKind: 'builtin',
        principalType: 'user',
        principalId: SELF_USER_ID,
        scopeType: 'project',
        scopeId: PROJECT_ID,
        createdAt: '2026-11-20T00:00:00Z',
        source: 'direct',
      },
      ...(longNames
        ? [
            {
              id: 'b-1-custom',
              roleDefinitionId: 'role-release-mgr',
              roleName: 'release-manager',
              roleKind: 'custom',
              principalType: 'user',
              principalId: SELF_USER_ID,
              scopeType: 'project',
              scopeId: PROJECT_ID,
              createdAt: '2026-11-20T00:00:00Z',
              source: 'direct',
            },
          ]
        : []),
    ];
    const otherBindings = [
      {
        id: 'b-2',
        roleDefinitionId: 'role-member',
        roleName: 'project-member',
        roleKind: 'builtin',
        principalType: 'user',
        principalId: OTHER_USER_ID,
        scopeType: 'project',
        scopeId: PROJECT_ID,
        createdAt: '2026-11-20T00:00:00Z',
        source: 'direct',
      },
      ...(longNames
        ? [
            {
              id: 'b-2-custom',
              roleDefinitionId: 'role-ci-operator',
              roleName: 'ci-operator',
              roleKind: 'custom',
              principalType: 'user',
              principalId: OTHER_USER_ID,
              scopeType: 'project',
              scopeId: PROJECT_ID,
              createdAt: '2026-11-20T00:00:00Z',
              source: 'direct',
            },
          ]
        : []),
    ];
    return route.fulfill({
      json: {
        items: [
          {
            principalType: 'user',
            principalId: SELF_USER_ID,
            principalDisplayName: longNames ? LONG_SELF_NAME : 'Preston Holmes',
            principalStatus: 'active',
            bindings: selfBindings,
          },
          {
            principalType: 'user',
            principalId: OTHER_USER_ID,
            principalDisplayName: longNames ? LONG_OTHER_NAME : 'Alice Anderson',
            principalStatus: 'active',
            bindings: otherBindings,
          },
        ],
        totalCount: 2,
        _capabilities: {
          canManageMembers: true,
          canManageAdmins: true,
          canManageOwners: true,
          canTransfer: true,
          canManageCustomRoles: true,
          canLeave: false,
          leaveBlockedReason: 'last_owner',
          actions: ['read', 'update', 'manage'],
        },
      },
    });
  });
}
