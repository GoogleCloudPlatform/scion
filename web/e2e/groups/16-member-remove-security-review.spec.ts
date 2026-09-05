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
 * Spec 16 — Member remove: confirmation dialog and successful removal (AC17)
 *
 * The security-review/lockout dialog is triggered when the hub returns a
 * structured 403 on member removal for constraint-bearing groups. Since the
 * access boundary seeding API is not yet available in the Go hub, we verify
 * the member removal confirmation dialog and successful removal flow.
 * The security-review dialog component is tested in the unit test layer.
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  createSession,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Member remove confirmation (AC17)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('remove member shows confirmation dialog and completes removal', async ({
    page,
  }) => {
    const slug = uniqueSlug('remove-confirm');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Remove Confirm Group',
      slug,
    });

    // Add a second user member
    const member = await createSession(env.baseURL, {
      email: `remove-member-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Removable Member',
    });
    await addGroupMember(env.baseURL, env.devToken, group.id, {
      memberType: 'user',
      memberId: member.user.id,
      role: 'member',
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Remove Confirm Group' }),
    ).toBeVisible({ timeout: 15_000 });

    // Wait for members table to load — use exact match to avoid matching dialog text
    await expect(page.getByText('Removable Member', { exact: true })).toBeVisible({
      timeout: 10_000,
    });

    // Click the remove button for the removable member (non-owner, not disabled)
    const removeButtons = page.locator(
      'sl-icon-button[name="trash"]:not([disabled])',
    );
    await removeButtons.first().click();

    // The showConfirm dialog should appear with title "Confirm"
    // and the message "Remove user "Removable Member" from this group?"
    const confirmDialog = page.locator('sl-dialog[label="Confirm"]');
    await expect(confirmDialog).toBeVisible({ timeout: 5_000 });
    await expect(confirmDialog.getByText(/remove.*from this group/i)).toBeVisible();

    // Accept the confirmation (button text is "Confirm")
    await confirmDialog.getByRole('button', { name: 'Confirm' }).click();

    // Wait for the confirm dialog to close first
    await expect(confirmDialog).not.toBeVisible({ timeout: 5_000 });

    // The member should be removed from the members table
    const membersTable = page.getByRole('table', { name: 'Group members' });
    await expect(membersTable.getByText('Removable Member')).not.toBeVisible({
      timeout: 10_000,
    });
  });

  test('cancel removal keeps the member', async ({ page }) => {
    const slug = uniqueSlug('remove-cancel');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Remove Cancel Group',
      slug,
    });

    // Add a member
    const member = await createSession(env.baseURL, {
      email: `cancel-remove-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Kept Member',
    });
    await addGroupMember(env.baseURL, env.devToken, group.id, {
      memberType: 'user',
      memberId: member.user.id,
      role: 'member',
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Remove Cancel Group' }),
    ).toBeVisible({ timeout: 15_000 });

    // Wait for members table — use exact match to avoid matching dialog text
    await expect(page.getByText('Kept Member', { exact: true })).toBeVisible({
      timeout: 10_000,
    });

    // Click remove button
    const removeButtons = page.locator(
      'sl-icon-button[name="trash"]:not([disabled])',
    );
    await removeButtons.first().click();

    // Wait for confirmation dialog
    const confirmDialog = page.locator('sl-dialog[label="Confirm"]');
    await expect(confirmDialog).toBeVisible({ timeout: 5_000 });

    // Click cancel
    await confirmDialog.getByRole('button', { name: 'Cancel' }).click();

    // Wait for the confirm dialog to close
    await expect(confirmDialog).not.toBeVisible({ timeout: 5_000 });

    // Member should still be visible in the table
    const membersTable = page.getByRole('table', { name: 'Group members' });
    await expect(membersTable.getByText('Kept Member')).toBeVisible({ timeout: 5_000 });
  });
});
