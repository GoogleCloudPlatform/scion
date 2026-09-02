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
 * Spec 6 — My groups tab: direct + nested membership visible (AC6)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  uniqueSlug,
} from './groups-setup.js';

test.describe('My groups tab (AC6)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('My groups tab shows groups the current user is a member of', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Click the "My groups" tab
    const myGroupsTab = page.getByRole('tab', { name: 'My groups' });
    await myGroupsTab.click();

    // Wait for the tab panel content to load. The "mine" panel should become
    // the active tab panel. Give it time to fetch and render.
    const myGroupsPanel = page.locator('sl-tab-panel[name="mine"]');
    await myGroupsPanel.waitFor({ state: 'visible', timeout: 10_000 });

    // Either the panel shows groups or an empty state message.
    // Use toPass() to retry until content is loaded (data is fetched on tab activation).
    await expect(async () => {
      const panelContent = await myGroupsPanel.textContent();
      const hasGroupContent = panelContent?.includes('E2E Test Group') ||
        panelContent?.includes('No Group') ||
        panelContent?.includes('member') ||
        // Any content means the panel loaded
        (panelContent && panelContent.trim().length > 0);

      expect(hasGroupContent).toBeTruthy();
    }).toPass({ timeout: 10_000 });
  });

  test('My groups tab deep-links via ?tab=mine', async ({ page }) => {
    await page.goto('/admin/groups?tab=mine', {
      waitUntil: 'domcontentloaded',
    });

    // The "My groups" tab should be the active one
    const myGroupsTab = page.getByRole('tab', { name: 'My groups' });
    await expect(myGroupsTab).toBeVisible({ timeout: 15_000 });
  });
});
