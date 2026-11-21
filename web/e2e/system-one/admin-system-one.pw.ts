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
 * Offline Playwright E2E spec for System One Providers admin UI (`/admin/system-one`).
 *
 * Uses mocked Hub API routes only — never calls a live provider.
 * Exercises real `<sl-select>` dropdown interactions inside `<sl-dialog>` to guard
 * against nested `sl-after-hide` bubbling (C1), plus list -> create -> test (with
 * required project) -> delete.
 */

import { test, expect, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

interface MockInstance {
  id: string;
  name: string;
  adapter: string;
  model: string;
  endpoint: string;
  auth: {
    kind: string;
    secretName?: string;
    secretRef?: { name: string; scope: string };
    scopes?: string[];
    quotaProject?: string;
    credentialType?: string;
  };
  enabled: boolean;
  revision: number;
  createdAt: string;
  updatedAt: string;
}

const ADAPTERS = [
  {
    id: 'gemini_decisions',
    version: '0.1.0',
    displayName: 'Gemini Decisions',
    defaultEndpoint: 'https://generativelanguage.googleapis.com',
    modelRequired: true,
    supportsModelListing: false,
    authModes: [
      {
        mode: 'owned_api_key',
        authKind: 'api_key',
        transportScheme: 'header:x-goog-api-key',
        customEndpointAllowed: true,
      },
      {
        mode: 'secret_ref',
        authKind: 'api_key',
        transportScheme: 'header:x-goog-api-key',
        customEndpointAllowed: false,
      },
      {
        mode: 'google_access_token',
        authKind: 'google_access_token',
        transportScheme: 'google_access_token',
        customEndpointAllowed: false,
        scopesRequired: true,
      },
    ],
    allowedSecretNames: ['GEMINI_API_KEY', 'GEMINI_TEAM_KEY'],
    defaultSecretName: 'GEMINI_API_KEY',
    allowedSecretScopes: ['hub', 'project', 'project_or_hub'],
    defaultSecretScope: 'project_or_hub',
  },
  {
    id: 'typesafe',
    version: '0.1.0',
    displayName: 'TypeSafe',
    defaultEndpoint: 'https://api.typesafe.ai',
    modelRequired: true,
    supportsModelListing: true,
    authModes: [
      {
        mode: 'owned_api_key',
        authKind: 'api_key',
        transportScheme: 'bearer',
        customEndpointAllowed: true,
      },
    ],
  },
];

async function installMockHub(page: Page) {
  const instances: MockInstance[] = [
    {
      id: 'inst-1',
      name: 'gemini-named',
      adapter: 'gemini_decisions',
      model: 'gemini-decision-1',
      endpoint: 'https://generativelanguage.googleapis.com',
      auth: {
        kind: 'api_key',
        secretRef: { name: 'GEMINI_API_KEY', scope: 'project_or_hub' },
      },
      enabled: true,
      revision: 1,
      createdAt: '2026-11-20T10:00:00Z',
      updatedAt: '2026-11-20T10:00:00Z',
    },
  ];
  const probePayloads: Array<{ name: string; body: Record<string, unknown> }> = [];
  const deletedNames: string[] = [];

  await installEventSourceStub(page);

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'admin-1',
        email: 'admin@example.test',
        name: 'Test Admin',
        role: 'admin',
      }),
    })
  );

  await page.route('**/api/**', async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const p = url.pathname;

    if (p === '/api/v1/system/status') {
      return route.fulfill({ status: 200, contentType: 'application/json', body: '{"complete":true}' });
    }
    if (p === '/api/v1/settings/public') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ appName: 'Scion', onboardingComplete: true }),
      });
    }
    if (p === '/api/v1/experiments') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ experiments: { 'hub.system_one': true } }),
      });
    }
    if (p === '/api/v1/auth/admin-status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          isAdmin: true,
          isSuperAdmin: true,
          permissions: [
            'system_one_instance.read',
            'system_one_instance.create',
            'system_one_instance.delete',
            'system_one_instance.execute',
          ],
        }),
      });
    }
    if (p === '/api/v1/projects') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          projects: [{ id: 'proj-alpha', name: 'Alpha Project', slug: 'alpha' }],
        }),
      });
    }
    if (p === '/api/v1/admin/system-one/adapters') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ adapters: ADAPTERS }),
      });
    }
    if (p === '/api/v1/admin/system-one/instances' && req.method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ instances, totalCount: instances.length }),
      });
    }
    if (p === '/api/v1/admin/system-one/instances' && req.method() === 'POST') {
      const body = JSON.parse(req.postData() || '{}') as Record<string, unknown>;
      const auth = (body.auth || {}) as Record<string, unknown>;
      const created: MockInstance = {
        id: `inst-${instances.length + 1}`,
        name: String(body.name || ''),
        adapter: String(body.adapter || ''),
        model: String(body.model || ''),
        endpoint: String(body.endpoint || ''),
        auth: auth.secretRef
          ? {
              kind: 'api_key',
              secretRef: auth.secretRef as { name: string; scope: string },
            }
          : {
              kind: 'api_key',
              secretName: `systemone.instance.inst-${instances.length + 1}`,
            },
        enabled: true,
        revision: 1,
        createdAt: '2026-11-21T19:00:00Z',
        updatedAt: '2026-11-21T19:00:00Z',
      };
      instances.push(created);
      return route.fulfill({
        status: 201,
        contentType: 'application/json',
        body: JSON.stringify(created),
      });
    }
    const probeMatch = p.match(/^\/api\/v1\/admin\/system-one\/instances\/([^/]+)\/probe$/);
    if (probeMatch && req.method() === 'POST') {
      probePayloads.push({
        name: decodeURIComponent(probeMatch[1] || ''),
        body: JSON.parse(req.postData() || '{}') as Record<string, unknown>,
      });
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          class: 'success',
          decision: { kind: 'judgment_bool', bool: true },
          evaluation: {
            eval_id: 'ev-pw-1',
            configured_model: 'gemini-decision-1',
            resolved_model: '',
            class: 'success',
            notes: ['model_unreported', 'usage_unreported'],
            elapsed: 110_000_000,
            auth: {
              auth_kind: 'api_key',
              secret_ref: {
                name: 'GEMINI_API_KEY',
                scope: 'project_or_hub',
                scope_used: 'project',
              },
              secret_version: 2,
            },
          },
        }),
      });
    }
    const nameMatch = p.match(/^\/api\/v1\/admin\/system-one\/instances\/([^/]+)$/);
    if (nameMatch && req.method() === 'DELETE') {
      const name = decodeURIComponent(nameMatch[1] || '');
      deletedNames.push(name);
      const idx = instances.findIndex((i) => i.name === name);
      if (idx >= 0) instances.splice(idx, 1);
      return route.fulfill({ status: 204, body: '' });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  return { probePayloads, deletedNames };
}

test('list -> create (real sl-select) -> Test (required project via real sl-select) -> delete', async ({
  page,
}) => {
  const { probePayloads, deletedNames } = await installMockHub(page);

  await page.goto('/admin/system-one', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('tr[data-instance-name="gemini-named"]')).toBeVisible();
  expect(probePayloads).toHaveLength(0);

  // Create dialog: use real clicks on sl-select (adapter & secret name) to catch C1
  await page.locator('sl-button:has-text("Add Provider Instance")').first().click();
  const createDialog = page.locator('sl-dialog[label="Add System One Provider Instance"]');
  await expect(createDialog).toHaveAttribute('open', '');

  await createDialog.locator('sl-radio[value="secret_ref"]').click();
  await createDialog.locator('sl-select[name="secretName"]').click();
  await page.locator('sl-option[value="GEMINI_TEAM_KEY"]').click();
  // C1 assertion: Create dialog must still be open after sl-select closes
  await expect(createDialog).toHaveAttribute('open', '');

  await createDialog.locator('sl-input[name="name"] input').fill('gemini-e2e');
  await createDialog.locator('sl-input[name="model"] input').fill('gemini-decision-1');
  await createDialog.locator('sl-button:has-text("Create Instance")').click();

  await expect(page.locator('tr[data-instance-name="gemini-e2e"]')).toBeVisible();

  // Test dialog: Run Test disabled until project selected via real sl-select click (catches C1)
  await page.locator('tr[data-instance-name="gemini-named"] sl-button:has-text("Test")').click();
  const testDialog = page.locator('sl-dialog[label="Test Provider Instance: gemini-named"]');
  await expect(testDialog).toHaveAttribute('open', '');

  const runBtn = testDialog.locator('sl-button:has-text("Run Test")');
  await expect(runBtn).toHaveAttribute('disabled', '');

  await testDialog.locator('sl-select[name="testProject"]').click();
  await page.locator('sl-option[value="proj-alpha"]').click();
  // C1 assertion: Test dialog must remain open after sl-select closes
  await expect(testDialog).toHaveAttribute('open', '');
  await expect(runBtn).not.toHaveAttribute('disabled', '');

  await runBtn.click();
  const resultBox = testDialog.locator('[data-testid="probe-result"]');
  await expect(resultBox).toBeVisible();
  await expect(resultBox).toContainText('Model unreported');
  await expect(resultBox).toContainText('Usage unavailable');
  expect(probePayloads).toHaveLength(1);
  expect(probePayloads[0]?.body.project).toBe('proj-alpha');

  await testDialog.locator('sl-button:has-text("Close")').click();

  // Delete with confirmation
  await page.locator('tr[data-instance-name="gemini-e2e"] sl-button:has-text("Delete")').click();
  const deleteDialog = page.locator('sl-dialog[label="Delete Provider Instance"]');
  await expect(deleteDialog).toHaveAttribute('open', '');
  await deleteDialog.locator('sl-button:has-text("Delete Instance")').click();
  await expect(page.locator('tr[data-instance-name="gemini-e2e"]')).toHaveCount(0);
  expect(deletedNames).toEqual(['gemini-e2e']);
});
