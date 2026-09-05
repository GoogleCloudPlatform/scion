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
 * Spec 4 — Create happy path: name-only → slug derived → detail page →
 *          creator is owner member (AC2)
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv, uniqueName, deleteGroupAPI } from './groups-setup.js';

test.describe('Create happy path (AC2)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('creating a group with name-only derives slug, lands on detail, creator is owner', async ({
    page,
  }) => {
    const groupName = uniqueName('Create Test');

    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByRole('button', { name: 'Create group' })).toBeVisible({
      timeout: 15_000,
    });

    // Click "Create group" button
    await page.getByRole('button', { name: 'Create group' }).click();

    // Dialog should open
    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Fill in only the name
    const nameInput = dialog.getByLabel('Name');
    await nameInput.fill(groupName);

    // The slug field should be auto-populated with a derived slug
    const slugInput = dialog.locator('#slug-input');
    // Give a moment for the slug to derive — check the component's value property
    await expect(async () => {
      const val = await slugInput.evaluate((el: any) => el.value);
      expect(val).toBeTruthy();
    }).toPass({ timeout: 5_000 });

    // Submit the form
    const createBtn = dialog.getByRole('button', { name: 'Create group' });
    await createBtn.click();

    // Should navigate to the detail page
    await expect(page.getByRole('heading', { name: groupName })).toBeVisible({
      timeout: 15_000,
    });

    // Should see the detail page URL has /admin/groups/
    await expect(page).toHaveURL(/\/admin\/groups\//, { timeout: 5_000 });

    // The creator should appear as an owner in the Members section
    // Use a role badge selector to avoid matching the "Owner" label/option
    const membersTable = page.getByRole('table', { name: 'Group members' });
    await expect(membersTable).toBeVisible({ timeout: 10_000 });
    await expect(membersTable.locator('.role-badge.owner')).toBeVisible();
  });
});
