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
 * Spec 5 — Create errors: duplicate slug inline + focus;
 *          project: prefix rejected pre-submit (AC3)
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv, fillSlInput } from './groups-setup.js';

test.describe('Create errors (AC3)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('duplicate slug shows inline error and dialog stays open', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByRole('button', { name: 'Create group' })).toBeVisible({
      timeout: 15_000,
    });

    // Open create dialog
    await page.getByRole('button', { name: 'Create group' }).click();

    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Fill in name using fillSlInput
    const nameInput = dialog.locator('sl-input').first();
    await fillSlInput(nameInput, 'Duplicate Slug Test');

    // Set slug to an already-existing slug (from the seeded data)
    const slugInput = dialog.locator('#slug-input');
    await fillSlInput(slugInput, 'e2e-test-group');

    // Submit
    const createBtn = dialog.getByRole('button', { name: 'Create group' });
    await createBtn.click();

    // The dialog should stay open with an error about the duplicate slug
    await expect(dialog).toBeVisible({ timeout: 10_000 });

    // Look for the error text about slug already existing (in the help-text slot)
    await expect(dialog.getByText(/already exists/i)).toBeVisible({ timeout: 10_000 });
  });

  test('project: prefix slug is rejected on submit', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByRole('button', { name: 'Create group' })).toBeVisible({
      timeout: 15_000,
    });

    // Open create dialog
    await page.getByRole('button', { name: 'Create group' }).click();

    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Fill in name
    const nameInput = dialog.locator('sl-input').first();
    await fillSlInput(nameInput, 'Project Prefix Test');

    // Set slug with project: prefix
    const slugInput = dialog.locator('#slug-input');
    await fillSlInput(slugInput, 'project:bad-slug');

    // Click submit — validation runs on submit
    const createBtn = dialog.getByRole('button', { name: 'Create group' });
    await createBtn.click();

    // The error should appear inline (slug validation error about reserved prefix)
    await expect(dialog.getByText(/reserved for system/i)).toBeVisible({ timeout: 5_000 });

    // Dialog should stay open
    await expect(dialog).toBeVisible();
  });
});
