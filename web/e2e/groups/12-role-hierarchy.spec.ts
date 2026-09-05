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
 * Spec 12 — Role hierarchy display and governance role copy
 *
 * Tests role display at different levels (owner/admin/member) and verifies
 * the governance role helper copy in the Add Member dialog.
 *
 * NOTE: This spec does NOT test AC13 (hierarchy denial — "admin can add
 * member but not owner"). That requires a group-admin principal who has
 * the RBAC addMember permission, which no hub role definition currently
 * grants. AC13 is untested until a backend decision resolves this gap.
 * See QA findings F-11 and F-13.
 *
 * TODO: Once a role granting group-admin addMember exists, add a test
 * that logs in as a group admin, attempts to add an owner (expects
 * hierarchy denial inline), and adds a member (expects success).
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  createSession,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Role hierarchy (AC13)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('members at different role levels are displayed correctly', async ({ page }) => {
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Hierarchy Display Test',
      slug: uniqueSlug('hierarchy-display'),
    });

    // Add a user as a member via API
    const memberUser = await createSession(env.baseURL, {
      email: `hierarchy-member-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Hierarchy Member',
    });
    await addGroupMember(env.baseURL, env.devToken, group.id, {
      memberType: 'user',
      memberId: memberUser.user.id,
      role: 'member',
    });

    // Add a user as an admin via API
    const adminUser = await createSession(env.baseURL, {
      email: `hierarchy-admin-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Hierarchy Admin',
    });
    await addGroupMember(env.baseURL, env.devToken, group.id, {
      memberType: 'user',
      memberId: adminUser.user.id,
      role: 'admin',
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Hierarchy Display Test' })).toBeVisible({
      timeout: 15_000,
    });

    // Verify all members are visible with their roles
    const membersTable = page.getByRole('table', { name: 'Group members' });
    await expect(membersTable).toBeVisible({ timeout: 10_000 });

    // The owner (creator), admin, and member should all be visible
    await expect(page.getByText('Hierarchy Member')).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText('Hierarchy Admin')).toBeVisible({ timeout: 5_000 });

    // Verify role badges exist
    await expect(membersTable.locator('.role-badge.owner')).toBeVisible();
    await expect(membersTable.locator('.role-badge.admin')).toBeVisible();
    await expect(membersTable.locator('.role-badge.member')).toBeVisible();
  });

  test('role select shows governance role helper copy', async ({ page }) => {
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Role Helper Test',
      slug: uniqueSlug('role-helper'),
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Role Helper Test' })).toBeVisible({
      timeout: 15_000,
    });

    await page.getByRole('button', { name: 'Add Member' }).click();

    const dialog = page.locator('sl-dialog[label="Add Member"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // The role select should have helper text explaining governance roles
    const helperText = dialog.getByText(/governance role/i);
    await expect(helperText).toBeVisible();
  });
});
