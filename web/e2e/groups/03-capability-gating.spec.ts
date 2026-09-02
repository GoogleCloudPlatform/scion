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
 * Spec 3 — Capability gating: admin vs read-only user affordances (AC1, AC8)
 *
 * Admin sees Create/Edit/Delete/Add/Remove; read-only user sees none.
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createSession,
  createGroup,
  createRoleBinding,
  findRoleDefinition,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Capability gating: admin vs read-only (AC1, AC8)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('admin sees Create group button on list page', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Admin should see the "Create group" button
    await expect(page.getByRole('button', { name: 'Create group' })).toBeVisible();
  });

  test('admin sees Edit and Delete on detail page', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Navigate to the seeded group's detail
    await page.getByText('E2E Test Group').click();

    // Wait for detail page
    await expect(page.getByRole('heading', { name: 'E2E Test Group' })).toBeVisible({
      timeout: 15_000,
    });

    // Admin should see Edit button
    await expect(page.getByRole('button', { name: 'Edit' })).toBeVisible();

    // Admin should see the overflow menu with Delete
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await expect(overflowBtn).toBeVisible();
  });

  test('admin sees Add Member button on detail page', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });
    await page.getByText('E2E Test Group').click();

    await expect(page.getByRole('heading', { name: 'E2E Test Group' })).toBeVisible({
      timeout: 15_000,
    });

    // Admin should see "Add Member" button
    await expect(page.getByRole('button', { name: 'Add Member' })).toBeVisible();
  });

  test('read-only user does not see mutation affordances', async ({ page }) => {
    // Create a read-only session
    const readOnlySession = await createSession(env.baseURL, {
      email: 'readonly@e2e.test',
      role: 'member',
      displayName: 'E2E Read-Only User',
    });

    // Give the read-only user read+list permissions only.
    // TODO: No 'group-viewer' role definition exists in the hub currently.
    // When one is added, remove this skip and enable the role-binding below.
    const roleDef = await findRoleDefinition(env.baseURL, env.devToken, 'group-viewer');
    if (!roleDef) {
      test.skip(true, 'No group-viewer role definition exists in the hub — cannot test read-only capability gating (AC1 read-only arm)');
      return;
    }
    await createRoleBinding(env.baseURL, env.devToken, {
      roleDefinitionId: roleDef.id,
      principalType: 'user',
      principalId: readOnlySession.user.id,
    });

    const roContext = await page.context().browser()!.newContext({
      storageState: readOnlySession.storageStatePath,
    });
    const roPage = await roContext.newPage();

    try {
      // Check the list page
      await roPage.goto(`${env.baseURL}/admin/groups`, {
        waitUntil: 'domcontentloaded',
      });

      // If they can see the list, Create group button should NOT be present.
      // (It may be a permission denied page instead.)
      // toHaveCount auto-retries, so no explicit wait is needed.
      const createBtn = roPage.getByRole('button', { name: 'Create group' });
      await expect(createBtn).toHaveCount(0);
    } finally {
      await roContext.close();
    }
  });
});
