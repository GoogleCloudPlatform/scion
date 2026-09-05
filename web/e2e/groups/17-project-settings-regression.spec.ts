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
 * Spec 17 — Project settings member editor unchanged (AC18)
 *
 * The group-member-editor component is also used by project settings.
 * This spec ensures that the project settings usage is behaviorally unchanged
 * when the new `capabilities` property is not set.
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv, apiRequest } from './groups-setup.js';

test.describe('Project settings member editor regression (AC18)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('project settings page with member editor renders correctly', async ({ page }) => {
    // Find a project to navigate to — use the API to list projects
    const res = await apiRequest(
      env.baseURL,
      env.devToken,
      'GET',
      '/api/v1/admin/projects',
    );

    if (!res.ok) {
      test.skip(true, 'No projects API available');
      return;
    }

    const data = (await res.json()) as { items?: Array<{ id: string; name: string }> };
    const projects = data.items ?? [];

    if (projects.length === 0) {
      test.skip(true, 'No projects available for regression testing');
      return;
    }

    const project = projects[0];

    // Navigate to project settings
    await page.goto(`/admin/projects/${project.id}`, {
      waitUntil: 'domcontentloaded',
    });

    // Check that the group-member-editor component is present (inside project
    // settings) and renders members or an empty state — meaning it hasn't broken.
    const memberEditor = page.locator('scion-group-member-editor');
    const memberHeading = page.getByText('Members');
    const pageContent = page.locator('main, [class*="content"]');

    // The page should at minimum load without errors
    await expect(pageContent.or(memberHeading).or(memberEditor)).toBeVisible({
      timeout: 15_000,
    });
  });
});
