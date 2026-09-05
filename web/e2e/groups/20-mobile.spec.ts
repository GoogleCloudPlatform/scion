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
 * Spec 20 — Mobile: specs 1, 4, 9, 14 re-run at 390×844 (AC21)
 *
 * These tests run at a mobile viewport size to verify:
 * - Name, type, and all actions are reachable on mobile.
 * - Critical workflows still function on small screens.
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  uniqueName,
  fillSlInput,
} from './groups-setup.js';

test.describe('Mobile viewport (AC21)', () => {
  const env = getE2EEnv();
  test.use({
    storageState: env.adminStorageState,
    baseURL: env.baseURL,
    viewport: { width: 390, height: 844 },
  });

  test('list: group name and type visible at mobile width', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({
      timeout: 15_000,
    });

    // Name should be visible
    const groupName = page.getByText('E2E Test Group');
    await expect(groupName).toBeVisible();

    // Type badge should be visible (explicit or project agents)
    const typeBadge = page.locator('.type-badge').first();
    await expect(typeBadge).toBeVisible();

    // Verify name and type are not hidden by mobile breakpoint
    const nameBox = await groupName.boundingBox();
    expect(nameBox).not.toBeNull();
    expect(nameBox!.width).toBeGreaterThan(0);
  });

  test('create group works at mobile width', async ({ page }) => {
    const groupName = uniqueName('Mobile Create');

    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(
      page.getByRole('button', { name: 'Create group' }),
    ).toBeVisible({ timeout: 15_000 });

    // Open create dialog
    await page.getByRole('button', { name: 'Create group' }).click();

    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Fill in name using Shoelace helper
    const nameInput = dialog.locator('sl-input').first();
    await fillSlInput(nameInput, groupName);

    // Submit
    const createBtn = dialog.getByRole('button', { name: 'Create group' });
    await createBtn.click();

    // Should navigate to detail
    await expect(page.getByRole('heading', { name: groupName })).toBeVisible({
      timeout: 15_000,
    });
  });

  test('add member works at mobile width', async ({ page }) => {
    const slug = uniqueSlug('mobile-member');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Mobile Member Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Mobile Member Test' }),
    ).toBeVisible({ timeout: 15_000 });

    // Add Member should be visible and clickable
    await page.getByRole('button', { name: 'Add Member' }).click();

    const dialog = page.locator('sl-dialog[label="Add Member"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Switch to agent type — target the Member Type select specifically
    const typeSelect = dialog.locator('sl-select[label="Member Type"]');
    await typeSelect.click();
    await page.locator('sl-option[value="agent"]').click();

    // Wait for agent input to appear, then fill using Shoelace helper
    const agentInput = dialog.locator('sl-input[label="Agent ID"]');
    await agentInput.waitFor({ state: 'visible', timeout: 5_000 });
    await fillSlInput(agentInput, `mobile-agent-${Date.now()}`);

    const addBtn = dialog.getByRole('button', { name: 'Add Member' });
    await addBtn.click();

    // The hub validates agent IDs exist — we expect either:
    // 1. Success (dialog closes) if the agent ID happens to exist
    // 2. An error message if it doesn't (normal path with fake agent ID)
    // Either outcome proves the mobile dialog flow works end-to-end.
    const errorOrClosed = await Promise.race([
      dialog.waitFor({ state: 'hidden', timeout: 5_000 }).then(() => 'closed'),
      dialog.locator('[role="alert"]').waitFor({ timeout: 5_000 }).then(() => 'error'),
    ]).catch(() => 'timeout');

    expect(['closed', 'error']).toContain(errorOrClosed);
  });

  test('delete group works at mobile width', async ({ page }) => {
    const slug = uniqueSlug('mobile-delete');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Mobile Delete Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Mobile Delete Test' }),
    ).toBeVisible({ timeout: 15_000 });

    // Open overflow menu and click Delete group
    const overflowBtn = page.locator('sl-dropdown sl-button[caret]');
    await overflowBtn.click();
    await page.getByRole('menuitem', { name: 'Delete group' }).click();

    const dialog = page.locator('sl-dialog[label*="Delete group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Type slug using Shoelace helper and delete
    const slugInput = dialog.locator('sl-input');
    await fillSlInput(slugInput, slug);

    const deleteBtn = dialog.getByRole('button', { name: 'Delete group' });
    await expect(deleteBtn).toBeEnabled({ timeout: 5_000 });
    await deleteBtn.click();

    // Should navigate to list
    await expect(page).toHaveURL(/\/admin\/groups/, { timeout: 15_000 });
  });
});
