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
 * Spec 13 — Sole-owner: disabled remove + tooltip; 2nd owner enables (AC14)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  createSession,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Sole-owner protection (AC14)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let soleOwnerGroupId: string;

  test.beforeAll(async () => {
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Sole Owner Test',
      slug: uniqueSlug('sole-owner'),
    });
    soleOwnerGroupId = group.id;
  });

  test('sole owner remove button is disabled with tooltip', async ({ page }) => {
    await page.goto(`/admin/groups/${soleOwnerGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Sole Owner Test' })).toBeVisible({
      timeout: 15_000,
    });

    // Wait for members to load — the members table should appear
    await expect(page.getByRole('table', { name: 'Group members' })).toBeVisible({
      timeout: 10_000,
    });

    // The sole owner's remove button should be disabled
    const disabledRemoveBtn = page.getByRole('button', { name: 'Remove member', disabled: true });
    await expect(disabledRemoveBtn).toBeVisible({ timeout: 5_000 });
  });

  test('adding a second owner enables the remove button for the first', async ({ page }) => {
    // Add a second user as owner
    const secondOwner = await createSession(env.baseURL, {
      email: 'second-owner@e2e.test',
      role: 'member',
      displayName: 'Second Owner',
    });
    await addGroupMember(env.baseURL, env.devToken, soleOwnerGroupId, {
      memberType: 'user',
      memberId: secondOwner.user.id,
      role: 'owner',
    });

    // Navigate to the group
    await page.goto(`/admin/groups/${soleOwnerGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Sole Owner Test' })).toBeVisible({
      timeout: 15_000,
    });

    // Wait for members to load — should see two owners now
    await expect(page.getByText('Second Owner')).toBeVisible({ timeout: 10_000 });

    // Now remove buttons should NOT be disabled (both owners can be removed)
    const enabledRemoveButtons = page.locator(
      'sl-icon-button[name="trash"]:not([disabled])',
    );
    // There should be at least one enabled remove button
    await expect(enabledRemoveButtons.first()).toBeVisible({ timeout: 5_000 });
  });
});
