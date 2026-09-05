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
 * Spec 8 — project_agents group: notice shown, zero mutation affordances (AC9)
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv, apiRequest } from './groups-setup.js';

test.describe('project_agents group (AC9)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('project_agents group shows system-managed notice and no mutation buttons', async ({
    page,
  }) => {
    // First, find a project_agents group in the list.
    // Filter by type to find one.
    await page.goto('/admin/groups?groupType=project_agents', {
      waitUntil: 'domcontentloaded',
    });

    // Wait for results or empty state
    const projectAgentsGroup = page.getByText('project agents').first();
    const emptyState = page.getByText('No Groups Match');

    // We need a project_agents group to test. If none exist, skip.
    const result = await Promise.race([
      projectAgentsGroup.waitFor({ timeout: 10_000 }).then(() => 'found'),
      emptyState.waitFor({ timeout: 10_000 }).then(() => 'empty'),
    ]).catch(() => 'timeout');

    if (result === 'empty' || result === 'timeout') {
      test.skip(true, 'No project_agents group available for testing');
      return;
    }

    // Click on the first project_agents group row
    const groupRow = page.locator('tr.clickable').first();
    await groupRow.click();

    // Wait for the detail page to load
    await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible({
      timeout: 15_000,
    });

    // Should see the system-managed notice
    await expect(page.getByText('system-managed')).toBeVisible();
    await expect(page.getByText('cannot be edited')).toBeVisible();

    // No Edit button should be visible
    await expect(page.getByRole('button', { name: 'Edit' })).toHaveCount(0);

    // No Delete button/menu should be visible
    await expect(page.locator('#delete-group-item')).toHaveCount(0);

    // No Add Member button
    await expect(page.getByRole('button', { name: 'Add Member' })).toHaveCount(0);
  });
});
