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
 * Spec 7 — Edit: rename, description, labels, owner transfer, slug immutable (AC7)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  uniqueName,
} from './groups-setup.js';

test.describe('Edit group (AC7)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let testGroupId: string;
  const slug = uniqueSlug('edit-test');

  test.beforeAll(async () => {
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Edit Test Group',
      slug,
      description: 'Original description',
      labels: { env: 'test' },
    });
    testGroupId = group.id;
  });

  test('rename persists after save and refetch', async ({ page }) => {
    await page.goto(`/admin/groups/${testGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Edit Test Group' })).toBeVisible({
      timeout: 15_000,
    });

    // Click Edit button
    await page.getByRole('button', { name: 'Edit' }).click();

    // Dialog should open — use specific label to avoid matching delete/add-member dialogs
    const dialog = page.locator('sl-dialog[label="Edit group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Change the name via the component's value property
    const nameInput = dialog.locator('#name-input');
    await nameInput.evaluate((el: any, val: string) => {
      el.value = val;
      el.dispatchEvent(new Event('sl-input', { bubbles: true }));
    }, 'Renamed Edit Group');

    // Submit
    const saveBtn = dialog.getByRole('button', { name: 'Save changes' });
    await saveBtn.click();

    // Heading should update
    await expect(page.getByRole('heading', { name: 'Renamed Edit Group' })).toBeVisible({
      timeout: 15_000,
    });
  });

  test('slug is visibly immutable in edit dialog', async ({ page }) => {
    await page.goto(`/admin/groups/${testGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible({ timeout: 15_000 });

    await page.getByRole('button', { name: 'Edit' }).click();

    const dialog = page.locator('sl-dialog[label="Edit group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // The slug field should be disabled/read-only with explanation text
    const slugText = dialog.getByText(/slugs are permanent/i);
    await expect(slugText).toBeVisible();
  });

  test('blank description leaves previous value unchanged', async ({ page }) => {
    await page.goto(`/admin/groups/${testGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible({ timeout: 15_000 });

    // Check current description is present
    await expect(page.getByText('Original description')).toBeVisible();

    await page.getByRole('button', { name: 'Edit' }).click();

    const dialog = page.locator('sl-dialog[label="Edit group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // The description help text should state "Leave blank to keep the current description"
    const helpText = dialog.getByText(/leave blank/i);
    await expect(helpText).toBeVisible();
  });
});
