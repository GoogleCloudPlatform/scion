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
 * Spec 21 — Groups list shows full descriptions
 *
 * The Description column renders the whole description as visible wrapping
 * text: no ellipsis, no clipping, no hover-only title. Long unbroken tokens
 * must wrap rather than widen the table past its container. Where the column
 * is hidden (narrow container), it stays hidden.
 *
 * Assertions are made against rendered geometry in a real browser, seeded
 * through the real groups API.
 */

import { test, expect, type Page } from '@playwright/test';
import { getE2EEnv, createGroup, uniqueSlug } from './groups-setup.js';

const LONG_PROSE =
  'This group owns the release pipeline for the hosted web frontend, ' +
  'including build attestation, candidate promotion, and the rollback ' +
  'runbook. Members are expected to review layout regressions across ' +
  'phone, tablet, and desktop widths before any candidate is published.';

const UNBROKEN = `token-${'x'.repeat(180)}-end`;
const SHORT = 'Short description';

interface DescriptionGeometry {
  text: string;
  hasTitle: boolean;
  scrollWidth: number;
  clientWidth: number;
  scrollHeight: number;
  clientHeight: number;
  lineHeight: number;
  cellRight: number;
  spanRight: number;
}

async function measureRow(page: Page, slug: string): Promise<DescriptionGeometry> {
  const row = page.locator('tbody tr', { hasText: slug });
  await expect(row).toHaveCount(1);
  const span = row.locator('.description-text');
  await expect(span).toBeVisible();
  return span.evaluate((el) => {
    const cell = el.closest('td')!;
    const style = getComputedStyle(el);
    const lh = parseFloat(style.lineHeight);
    return {
      text: el.textContent ?? '',
      hasTitle: el.hasAttribute('title'),
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
      lineHeight: Number.isFinite(lh) ? lh : parseFloat(style.fontSize) * 1.2,
      cellRight: cell.getBoundingClientRect().right,
      spanRight: el.getBoundingClientRect().right,
    };
  });
}

async function expectTableFitsContainer(page: Page): Promise<void> {
  const fit = await page.locator('.table-container').evaluate((container) => {
    const table = container.querySelector('table')!;
    return {
      containerWidth: container.clientWidth,
      tableWidth: table.scrollWidth,
      containerRight: container.getBoundingClientRect().right,
      tableRight: table.getBoundingClientRect().right,
    };
  });
  expect(fit.tableWidth).toBeLessThanOrEqual(fit.containerWidth + 1);
  expect(fit.tableRight).toBeLessThanOrEqual(fit.containerRight + 1);
}

test.describe('Groups list — full description text (F-1)', () => {
  const env = getE2EEnv();
  // One shared token so a single search returns exactly the seeded groups.
  const tag = uniqueSlug('descfull');
  const slugs = {
    short: `${tag}-short`,
    long: `${tag}-long`,
    unbroken: `${tag}-unbroken`,
    empty: `${tag}-empty`,
  };

  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test.beforeAll(async () => {
    await createGroup(env.baseURL, env.devToken, {
      name: 'Desc Short',
      slug: slugs.short,
      description: SHORT,
    });
    await createGroup(env.baseURL, env.devToken, {
      name: 'Desc Long',
      slug: slugs.long,
      description: LONG_PROSE,
    });
    await createGroup(env.baseURL, env.devToken, {
      name: 'Desc Unbroken',
      slug: slugs.unbroken,
      description: UNBROKEN,
    });
    await createGroup(env.baseURL, env.devToken, {
      name: 'Desc Empty',
      slug: slugs.empty,
    });
  });

  async function openList(page: Page): Promise<void> {
    await page.goto(`/admin/groups?q=${encodeURIComponent(tag)}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.locator('tbody tr', { hasText: slugs.empty })).toBeVisible({
      timeout: 15_000,
    });
  }

  for (const vp of [
    { width: 1440, height: 900 },
    { width: 820, height: 1180 },
  ]) {
    test(`renders descriptions in full at ${vp.width}x${vp.height}`, async ({ page }) => {
      await page.setViewportSize(vp);
      await openList(page);

      const header = page.locator('th', { hasText: 'Description' });
      if (!(await header.isVisible())) {
        // Column is hidden at this container width (pre-existing responsive
        // behaviour): nothing to read, and nothing should leak into view.
        await expect(page.locator('.description-text').first()).toBeHidden();
        await expectTableFitsContainer(page);
        return;
      }

      const short = await measureRow(page, slugs.short);
      const long = await measureRow(page, slugs.long);
      const unbroken = await measureRow(page, slugs.unbroken);
      const empty = await measureRow(page, slugs.empty);

      expect(short.text.trim()).toBe(SHORT);
      expect(long.text.trim()).toBe(LONG_PROSE);
      expect(unbroken.text.trim()).toBe(UNBROKEN);
      expect(empty.text.trim()).toBe('—');

      for (const g of [short, long, unbroken, empty]) {
        // Readable without hover: no title fallback, nothing clipped.
        expect(g.hasTitle).toBe(false);
        expect(g.scrollWidth).toBeLessThanOrEqual(g.clientWidth + 1);
        expect(g.scrollHeight).toBeLessThanOrEqual(g.clientHeight + 1);
        expect(g.spanRight).toBeLessThanOrEqual(g.cellRight + 1);
      }

      // Long text and the unbroken token wrap onto multiple lines.
      expect(long.clientHeight).toBeGreaterThan(long.lineHeight * 1.5);
      expect(unbroken.clientHeight).toBeGreaterThan(unbroken.lineHeight * 1.5);

      await expectTableFitsContainer(page);
    });
  }

  test('hides the Description column at 390x844 and keeps name/type/navigation', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openList(page);

    await expect(page.locator('th', { hasText: 'Description' })).toBeHidden();
    const row = page.locator('tbody tr', { hasText: slugs.long });
    await expect(row.locator('.description-text')).toBeHidden();
    await expect(row.locator('.type-badge')).toBeVisible();
    await expectTableFitsContainer(page);

    await row.locator('.group-name-link').click();
    await expect(page.getByRole('heading', { name: 'Desc Long' })).toBeVisible({
      timeout: 15_000,
    });
  });

  test('row link still navigates at desktop width', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await openList(page);
    const row = page.locator('tbody tr', { hasText: slugs.unbroken });
    await row.locator('.group-name-link').click();
    await expect(page.getByRole('heading', { name: 'Desc Unbroken' })).toBeVisible({
      timeout: 15_000,
    });
  });
});
