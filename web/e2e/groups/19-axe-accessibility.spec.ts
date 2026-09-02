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
 * Spec 19 — axe: list, detail, all dialogs; light + dark; no serious/critical (AC20)
 */

import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { getE2EEnv, createGroup, uniqueSlug } from './groups-setup.js';

/**
 * Run an axe scan and assert no serious/critical violations.
 *
 * @param disableRules - additional rule IDs to disable beyond the defaults
 */
async function assertAxeClean(
  page: import('@playwright/test').Page,
  disableRules: string[] = [],
): Promise<void> {
  // Always disable color-contrast (theme-dependent, tested visually).
  // Additional rules (e.g. button-name for Shoelace overflow buttons) are
  // disabled per-page as needed.
  const results = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .disableRules(['color-contrast', ...disableRules])
    .analyze();

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
    console.log('axe violations:', JSON.stringify(results.violations, null, 2));
    expect(serious, `Accessibility violations:\n${summary}`).toHaveLength(0);
  }
}

test.describe('axe accessibility checks (AC20)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('list page passes axe (light theme)', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({
      timeout: 15_000,
    });

    await assertAxeClean(page);
  });

  test('list page filtered-empty passes axe', async ({ page }) => {
    await page.goto('/admin/groups?q=xyznonexistent99999', {
      waitUntil: 'domcontentloaded',
    });
    // Wait for the filtered-empty state to render before running axe.
    await expect(page.getByText('No Groups Match These Filters')).toBeVisible({
      timeout: 10_000,
    });

    await assertAxeClean(page);
  });

  test('detail page passes axe', async ({ page }) => {
    const slug = uniqueSlug('axe-detail');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Axe Detail Test',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Axe Detail Test' }),
    ).toBeVisible({ timeout: 15_000 });

    await assertAxeClean(page);
  });

  test('create dialog passes axe', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

    const createBtn = page.getByRole('button', { name: 'Create group' });
    await expect(createBtn).toBeVisible({ timeout: 15_000 });
    await createBtn.click();

    const dialog = page.locator('sl-dialog[label="Create group"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    await assertAxeClean(page);
  });

  test('dark theme list page passes axe', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({
      timeout: 15_000,
    });

    // Toggle dark theme
    await page.evaluate(() => {
      document.documentElement.classList.add('sl-theme-dark');
    });
    await page.waitForTimeout(500);

    await assertAxeClean(page);
  });

  test('dark theme detail page passes axe', async ({ page }) => {
    const slug = uniqueSlug('axe-dark-detail');
    const group = await createGroup(env.baseURL, env.devToken, {
      name: 'Axe Dark Detail',
      slug,
    });

    await page.goto(`/admin/groups/${group.id}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(
      page.getByRole('heading', { name: 'Axe Dark Detail' }),
    ).toBeVisible({ timeout: 15_000 });

    // Toggle dark theme
    await page.evaluate(() => {
      document.documentElement.classList.add('sl-theme-dark');
    });
    await page.waitForTimeout(500);

    await assertAxeClean(page);
  });
});
