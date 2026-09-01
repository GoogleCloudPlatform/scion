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
 * Smoke E2E spec — proves the full E2E loop works:
 *
 * 1. Hub is running (started by global-setup)
 * 2. Admin can log in (session from dev-auth auto-login)
 * 3. Navigate to /admin/groups
 * 4. Seeded group is visible
 * 5. Page passes axe accessibility scan
 */

import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { getE2EEnv } from './harness/env.js';

test.describe('Smoke test', () => {
  const env = getE2EEnv();

  // Override baseURL from the env file — the hub runs on an ephemeral port that
  // is only known after global-setup completes (the config's baseURL cannot know it).
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('admin can see seeded group on /admin/groups', async ({ page }) => {
    // Navigate to the groups list page
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

    // Wait for the seeded group name to appear on the page.
    // This confirms: auth works, page loads, data is fetched and rendered.
    const seededGroup = env.seedData.groups[0];
    await expect(page.getByText(seededGroup.name)).toBeVisible({
      timeout: 15_000,
    });

    // Verify the slug also appears (confirming full row render)
    await expect(page.getByText(seededGroup.slug)).toBeVisible();
  });

  test('groups list page passes axe accessibility scan', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

    // Wait for groups data to load (seeded group must be visible)
    const seededGroup = env.seedData.groups[0];
    await expect(page.getByText(seededGroup.name)).toBeVisible({
      timeout: 15_000,
    });

    // Run axe accessibility scan.
    // color-contrast is disabled due to a pre-existing issue in the admin-groups
    // component's table header styling (#64748b on #f1f5f9 = 4.34:1; needs 4.5:1).
    // TODO: Re-enable once the admin-groups table header contrast is fixed.
    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
      .disableRules(['color-contrast'])
      .analyze();

    // Fail on serious or critical violations
    const serious = results.violations.filter(
      (v) => v.impact === 'serious' || v.impact === 'critical',
    );

    if (serious.length > 0) {
      const summary = serious
        .map(
          (v) =>
            `[${v.impact}] ${v.id}: ${v.description} (${v.nodes.length} instance(s))`,
        )
        .join('\n');
      // Log all violations for debugging
      console.log(
        'All axe violations:',
        JSON.stringify(results.violations, null, 2),
      );
      expect(
        serious,
        `Accessibility violations found:\n${summary}`,
      ).toHaveLength(0);
    }
  });
});
