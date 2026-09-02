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
 * Spec 14 — Delete: typed-slug gate, success → list + toast (AC15)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  fillSlInput,
} from './groups-setup.js';

test.describe('Delete group (AC15)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('typed-slug gate prevents deletion until correct slug is entered', async ({ page }) => {
    const slug = uniqueSlug('delete-gate');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Delete Gate Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Delete Gate Test' })).toBeVisible({
      timeout: 15_000,
    });

    // Open the overflow menu and click Delete group
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    // Delete confirmation dialog should appear
    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // The "Delete group" button should be disabled (slug not typed yet)
    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeDisabled();

    // Type an incorrect slug
    const slugInput = dialog.locator('sl-input');
    await fillSlInput(slugInput, 'wrong-slug');
    await expect(deleteBtn).toBeDisabled();

    // Type the correct slug
    await fillSlInput(slugInput, slug);

    // Now the Delete button should be enabled
    await expect(deleteBtn).toBeEnabled({ timeout: 5_000 });
  });

  test('successful delete navigates to list page', async ({ page }) => {
    const slug = uniqueSlug('delete-success');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Delete Success Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Delete Success Test' })).toBeVisible({
      timeout: 15_000,
    });

    // Open overflow menu and click Delete group
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Type the correct slug
    const slugInput = dialog.locator('sl-input');
    await fillSlInput(slugInput, slug);

    // Click Delete group
    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeEnabled({ timeout: 5_000 });
    await deleteBtn.click();

    // Should navigate to the groups list
    await expect(page).toHaveURL(/\/admin\/groups(?:\?|$)/, { timeout: 15_000 });

    // The deleted group should not appear in the table
    const table = page.getByRole('table', { name: 'Groups' });
    await expect(table).toBeVisible({ timeout: 10_000 });
    await expect(table.getByText('Delete Success Test')).not.toBeVisible({
      timeout: 10_000,
    });
  });
});
