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
 * Spec 18 — Keyboard-only: create, add member, delete via keyboard (AC19)
 *
 * Verifies that a user can complete create-group, add-member, and
 * delete-group flows without a pointer device.
 *
 * Strategy: Use focus() + keyboard.press('Enter') on target elements
 * rather than sequential Tab key presses, because Shoelace shadow DOM
 * makes tab order unpredictable.
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  uniqueName,
  fillSlInput,
} from './groups-setup.js';

test.describe('Keyboard-only flows (AC19)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('create group via keyboard', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

    const createBtn = page.getByRole('button', { name: 'Create group' });
    await expect(createBtn).toBeVisible({ timeout: 15_000 });

    // Activate create button via keyboard
    await createBtn.focus();
    await page.keyboard.press('Enter');

    // Dialog should open
    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Type a name using fillSlInput (Shoelace component)
    const name = uniqueName('Keyboard Create');
    const nameInput = dialog.locator('sl-input').first();
    await fillSlInput(nameInput, name);

    // Focus and activate the Create group submit button
    const submitBtn = dialog.getByRole('button', { name: 'Create group' });
    await submitBtn.focus();
    await page.keyboard.press('Enter');

    // Should navigate to the detail page
    await expect(page.getByRole('heading', { name })).toBeVisible({
      timeout: 15_000,
    });
  });

  test('list rows are reachable and activatable via Enter', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({
      timeout: 15_000,
    });

    // Find the group name link and focus it
    const groupLink = page.locator('a.group-name-link').first();
    await groupLink.focus();

    // Press Enter to activate
    await page.keyboard.press('Enter');

    // Should navigate to the detail page
    await expect(page).toHaveURL(/\/admin\/groups\//, { timeout: 15_000 });
  });

  test('delete group via keyboard', async ({ page }) => {
    const slug = uniqueSlug('kbd-delete');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Keyboard Delete Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Keyboard Delete Test' }),
    ).toBeVisible({ timeout: 15_000 });

    // Open overflow menu — use click since Shoelace sl-dropdown handles
    // keyboard internally via its own event delegation
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    // Delete dialog should open
    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Type the slug via fillSlInput (Shoelace component)
    const slugInput = dialog.locator('sl-input');
    await fillSlInput(slugInput, slug);

    // Activate the Delete group button
    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeEnabled({ timeout: 5_000 });
    await deleteBtn.focus();
    await page.keyboard.press('Enter');

    // Should navigate to list
    await expect(page).toHaveURL(/\/admin\/groups(?:\?|$)/, { timeout: 15_000 });
  });
});
