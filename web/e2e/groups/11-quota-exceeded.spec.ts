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
 * Spec 11 — Quota: seeded limit reached → inline quota copy (AC12)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  createSession,
  setMaxMembersPerGroupQuota,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Quota exceeded (AC12)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let quotaGroupId: string;

  test.beforeAll(async () => {
    // Set a very low quota
    await setMaxMembersPerGroupQuota(env.baseURL, env.devToken, 2);

    // Create a group
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Quota Test Group',
      slug: uniqueSlug('quota-test'),
    });
    quotaGroupId = group.id;

    // The group creator is automatically an owner member (1 member).
    // Add one more member to reach the limit of 2.
    const session = await createSession(env.baseURL, {
      email: 'quota-filler@e2e.test',
      role: 'member',
      displayName: 'Quota Filler',
    });
    await addGroupMember(env.baseURL, env.devToken, quotaGroupId, {
      memberType: 'user',
      memberId: session.user.id,
      role: 'member',
    });
  });

  test.afterAll(async () => {
    // Restore quota to a high value
    await setMaxMembersPerGroupQuota(env.baseURL, env.devToken, 1000);
  });

  test('adding a member when quota is reached shows quota error inline', async ({ page }) => {
    await page.goto(`/admin/groups/${quotaGroupId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Quota Test Group' })).toBeVisible({
      timeout: 15_000,
    });

    // Open Add Member dialog
    await page.getByRole('button', { name: 'Add Member' }).click();

    const dialog = page.locator('sl-dialog[label="Add Member"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Try to add an agent (simplest member type — just an ID)
    const typeSelect = dialog.locator('sl-select[label="Member Type"]');
    await typeSelect.click();
    await page.locator('sl-option[value="agent"]').click();

    const agentInput = dialog.locator('sl-input[label="Agent ID"]');
    await agentInput.waitFor({ state: 'visible', timeout: 5_000 });
    await agentInput.evaluate((el: any, val: string) => {
      el.value = val;
      el.dispatchEvent(new Event('sl-input', { bubbles: true }));
    }, 'quota-over-agent');

    // Submit
    const addBtn = dialog.getByRole('button', { name: 'Add Member' });
    await addBtn.click();

    // Should see quota error inline
    const quotaError = dialog.getByText(/member limit|quota/i);
    await expect(quotaError).toBeVisible({ timeout: 10_000 });

    // Dialog should stay open
    await expect(dialog).toBeVisible();
  });
});
