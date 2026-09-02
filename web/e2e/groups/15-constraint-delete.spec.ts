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
 * Spec 15 — Constraint-bearing delete: protection dialog + boundaries link (AC16)
 *
 * The constraint_gate protection dialog is triggered when DELETE /api/v1/groups/{id}
 * returns a 403 with kind=constraint_gate (group has access boundaries and
 * the caller lacks access_constraint.admin). The access boundary seeding API
 * is not yet implemented in the Go hub, so we verify the delete dialog
 * structure and confirm the happy-path delete still works. The constraint_gate
 * UI path is architecturally tested via the component's unit test layer.
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  fillSlInput,
} from './groups-setup.js';

test.describe('Constraint-bearing delete (AC16)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('delete dialog shows impact copy and slug-confirm input', async ({ page }) => {
    const slug = uniqueSlug('constraint-dialog');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Constraint Dialog Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Constraint Dialog Test' }),
    ).toBeVisible({ timeout: 15_000 });

    // Open overflow menu and click Delete group
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    // Delete confirmation dialog should appear with the group name in label
    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Impact copy mentions memberships
    await expect(dialog.getByText(/membership/i)).toBeVisible();

    // Slug confirmation label with the slug in a <code> element
    await expect(dialog.locator('code')).toContainText(slug);

    // Cancel button is present
    await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeVisible();

    // Delete button is present but disabled until slug matches
    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeDisabled();
  });

  test('delete of unconstrained group succeeds normally', async ({ page }) => {
    const slug = uniqueSlug('constraint-ok');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Unconstrained Delete',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Unconstrained Delete' }),
    ).toBeVisible({ timeout: 15_000 });

    // Open overflow menu → Delete group
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Type the correct slug
    const slugInput = dialog.locator('sl-input');
    await fillSlInput(slugInput, slug);

    // Delete button should enable
    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeEnabled({ timeout: 5_000 });
    await deleteBtn.click();

    // Should navigate back to the list
    await expect(page).toHaveURL(/\/admin\/groups(?:\?|$)/, { timeout: 15_000 });
  });
});
