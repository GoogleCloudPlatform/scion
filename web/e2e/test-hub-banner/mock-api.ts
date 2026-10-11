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
 * A mocked hub for the test-hub banner suite (ptone/scion#4240, phase W).
 *
 * Every request is answered here: GET /api/v1/test-infra/status returns the
 * status the test chooses, /auth/me the chosen user (or 401 when signed
 * out), admin-status matches the user's role, and every other hub request
 * gets an empty but well-formed body so the pages render their shells.
 */

import type { Page } from '@playwright/test';

export interface TestInfraStatus {
  testIdentities: boolean;
  testHubAdmin: boolean;
  testSuperAdmin: boolean;
}

export const OFF: TestInfraStatus = {
  testIdentities: false,
  testHubAdmin: false,
  testSuperAdmin: false,
};
export const MEMBER_TIER: TestInfraStatus = { ...OFF, testIdentities: true };
export const SUPER_ADMIN_TIER: TestInfraStatus = {
  testIdentities: true,
  testHubAdmin: true,
  testSuperAdmin: true,
};

export type Role = 'viewer' | 'member' | 'admin';

export const PROJECT_ID = 'p-1';

const ADMIN_PERMISSIONS = ['user.read', 'user.list', 'group.read', 'group.list', 'role.read'];

export interface MockHub {
  /** How many times the page asked for the test-infra status. */
  statusRequests(): number;
}

export async function setupHub(
  page: Page,
  options: { status: TestInfraStatus; role: Role | null }
): Promise<MockHub> {
  const { status, role } = options;
  let statusRequests = 0;

  await page.addInitScript(() => {
    // No SSE server behind the mocks.
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
  });

  // Registered first, so the specific routes below take precedence.
  await page.route('**/api/**', (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/agents') || path.endsWith('/agents/')) {
      return route.fulfill({ json: { agents: [] } });
    }
    if (path.endsWith('/projects')) return route.fulfill({ json: { projects: [] } });
    if (path.endsWith('/users')) return route.fulfill({ json: { users: [] } });
    if (path.endsWith('/groups')) return route.fulfill({ json: { groups: [] } });
    return route.fulfill({ json: {} });
  });

  await page.route('**/auth/me', (route) =>
    role
      ? route.fulfill({
          json: {
            id: `fixture-${role}`,
            email: `${role}@example.test`,
            displayName: `Fixture ${role}`,
            role,
          },
        })
      : route.fulfill({ status: 401, json: { error: 'unauthorized' } })
  );
  await page.route('**/auth/providers', (route) =>
    route.fulfill({ json: { providers: [{ id: 'google', name: 'Google', enabled: true }] } })
  );
  await page.route('**/api/v1/settings/public', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/experiments', (route) =>
    route.fulfill({ json: { experiments: {} } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({
      json:
        role === 'admin'
          ? { isAdmin: true, isSuperAdmin: true, permissions: ADMIN_PERMISSIONS }
          : { isAdmin: false, isSuperAdmin: false, permissions: [] },
    })
  );
  await page.route(`**/api/v1/projects/${PROJECT_ID}`, (route) =>
    route.fulfill({
      json: {
        id: PROJECT_ID,
        name: 'Project One',
        slug: 'project-one',
        _capabilities: { actions: ['read'] },
      },
    })
  );
  await page.route('**/api/v1/test-infra/status', (route) => {
    statusRequests++;
    return route.fulfill({ json: status });
  });

  return { statusRequests: () => statusRequests };
}
