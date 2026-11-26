/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { test, expect } from '@playwright/test';

import { PROJ_PAYMENTS, PROJ_WEB, setupDecisionRetentionHub } from './mock-api.js';

const VIEWPORTS: Array<{ name: string; width: number; height: number; theme: 'light' | 'dark' }> = [
  { name: 'desktop light', width: 1100, height: 800, theme: 'light' },
  { name: 'desktop dark', width: 1100, height: 800, theme: 'dark' },
  { name: 'mobile 390px light', width: 390, height: 844, theme: 'light' },
  { name: 'mobile 390px dark', width: 390, height: 844, theme: 'dark' },
];

for (const vp of VIEWPORTS) {
  test.describe(`decision retention (${vp.name})`, () => {
    test.use({ viewport: { width: vp.width, height: vp.height } });

    test('renders Admin > Server Config Decision retention panel with overrides and preview estimate', async ({
      page,
    }) => {
      await setupDecisionRetentionHub(page, { theme: vp.theme });
      await page.goto('/admin/server-config', { waitUntil: 'domcontentloaded' });

      const panel = page.locator('scion-admin-decision-retention');
      await expect(panel.locator('.section-title')).toHaveText('Decision retention');
      await expect(panel.locator('.last-run')).toContainText(
        '1,284 decisions, 1,190 answers deleted in 6 projects'
      );
      await expect(panel.locator('.preview')).toHaveText(
        'Preview estimate: 2,310 decisions would be deleted now.'
      );
      await expect(panel.locator('tr.override-row')).toHaveCount(2);
      await expect(panel.locator('tr.override-row').first()).toContainText('payments-api');
      await expect(panel.locator('tr.override-row').first()).toContainText('Keep forever');

      // Mobile hides the Effective column cleanly without horizontal overflow.
      if (vp.width <= 390) {
        await expect(panel.locator('th.col-hide')).toBeHidden();
        const overflow = await page.evaluate(
          () => document.documentElement.scrollWidth > document.documentElement.clientWidth + 2
        );
        expect(overflow).toBe(false);
      }
    });

    test('renders Project Made retention notice with single-project wording', async ({ page }) => {
      await setupDecisionRetentionHub(page, { theme: vp.theme });
      await page.goto(`/projects/${PROJ_WEB}/decisions?tab=made`, {
        waitUntil: 'domcontentloaded',
      });

      const notice = page.locator('scion-page-project-decisions [data-retention-notice]');
      await expect(notice).toBeVisible();
      await expect(notice).toHaveText(
        'Settled decisions in this project are deleted 180 days after their last activity.'
      );
    });

    test('renders Profile Made general notice and switches to single-project wording when filtered', async ({
      page,
    }) => {
      await setupDecisionRetentionHub(page, { theme: vp.theme });
      await page.goto('/profile/decisions?tab=made&sort=recent', {
        waitUntil: 'domcontentloaded',
      });

      const notice = page.locator('scion-page-profile-decisions [data-retention-notice]');
      await expect(notice).toBeVisible();
      await expect(notice).toHaveText(
        'Some projects delete settled decisions after a retention period, so older decisions may be missing.'
      );

      await page.goto(`/profile/decisions?tab=made&sort=recent&project=${PROJ_WEB}`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(notice).toBeVisible();
      await expect(notice).toHaveText(
        'Settled decisions in this project are deleted 180 days after their last activity.'
      );

      // Held project (payments-api) has no active retention -> notice hidden.
      await page.goto(`/profile/decisions?tab=made&sort=recent&project=${PROJ_PAYMENTS}`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(notice).toHaveCount(0);
    });
  });
}

test('hides admin panel and Made notices when hub.decision_retention is off', async ({ page }) => {
  await setupDecisionRetentionHub(page, { retentionFlag: false });
  await page.goto('/admin/server-config', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('scion-page-admin-server-config')).toHaveCount(1);
  await expect(page.locator('scion-admin-decision-retention .decision-retention')).toHaveCount(0);

  await page.goto(`/projects/${PROJ_WEB}/decisions?tab=made`, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('scion-page-project-decisions')).toHaveCount(1);
  await expect(page.locator('scion-page-project-decisions [data-retention-notice]')).toHaveCount(0);
});
