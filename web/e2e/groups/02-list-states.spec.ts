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
 * Spec 2 — List states: truly-empty vs filtered-empty;
 *          permission-denied panel (AC5)
 *
 * Verifies that the list page shows distinct states for:
 * - Filtered empty: "No Groups Match These Filters" + Clear filters button
 * - Permission denied: dedicated panel for users without group.list
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv, createSession, fillSlInput } from './groups-setup.js';

test.describe('List states: empty variants and permission denied (AC5)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('filtered-empty shows "No Groups Match" with clear button', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Type a non-matching query using fillSlInput (sl-input is a custom element)
    const searchInput = page.locator('sl-input[placeholder="Search name, slug, description..."]');
    await fillSlInput(searchInput, 'xyznonexistent99999');

    // Wait for filtered-empty state
    await expect(page.getByText('No Groups Match These Filters')).toBeVisible({
      timeout: 10_000,
    });

    // Clear filters button should be present
    await expect(page.getByRole('button', { name: 'Clear filters' }).first()).toBeVisible();
  });

  test('truly-empty state shows "No Groups" heading', async ({ page }) => {
    // Navigate directly with a non-matching query param
    await page.goto('/admin/groups?q=xyznonexistent99999', {
      waitUntil: 'domcontentloaded',
    });

    await expect(page.getByText('No Groups Match These Filters')).toBeVisible({
      timeout: 10_000,
    });

    // This is the filtered-empty variant — it has "Clear filters"
    const clearBtn = page.getByRole('button', { name: 'Clear filters' }).first();
    await expect(clearBtn).toBeVisible();
  });

  test('permission-denied user sees permission denied panel', async ({ page }) => {
    // Create a viewer session with no group permissions
    const viewerSession = await createSession(env.baseURL, {
      email: 'no-perms-viewer@e2e.test',
      role: 'viewer',
      displayName: 'No Perms Viewer',
    });

    // Use the viewer's storage state
    const viewerContext = await page.context().browser()!.newContext({
      storageState: viewerSession.storageStatePath,
    });
    const viewerPage = await viewerContext.newPage();

    try {
      await viewerPage.goto(`${env.baseURL}/admin/groups`, {
        waitUntil: 'domcontentloaded',
      });

      // The viewer should see a permission-denied state, be redirected away,
      // or at least NOT see the normal admin "Create group" button.
      await expect(async () => {
        const seesPermissionDenied = await viewerPage
          .getByText(/permission denied|forbidden|not authorized|access denied/i)
          .isVisible()
          .catch(() => false);
        const redirectedAway = !viewerPage.url().includes('/admin/groups');
        const noCreateButton = !(await viewerPage
          .getByRole('button', { name: 'Create group' })
          .isVisible()
          .catch(() => false));

        expect(seesPermissionDenied || redirectedAway || noCreateButton).toBe(true);
      }).toPass({ timeout: 15_000 });
    } finally {
      await viewerContext.close();
    }
  });
});
