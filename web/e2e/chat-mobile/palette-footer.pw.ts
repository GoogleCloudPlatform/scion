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
 * The quick-switcher button pinned to the bottom of the chat columns, per
 * viewport: at the bottom of the spaces column on every width, at the
 * bottom of the members column too on phones, never in the header. Each
 * button is full width with a tap target of at least 44px on phones, stays
 * put while its list scrolls and opens the palette. The bottom safe-area
 * inset is covered in safe-area.pw.ts.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';

const PANEL_SETTLE_MS = 400;
/** The footer's 0.375rem padding, plus a pixel of rounding. */
const FOOTER_PAD_PX = 7;

function railButton(page: Page): Locator {
  return page.locator('.v2-rail > .palette-footer > .palette-footer-btn');
}

function membersButton(page: Page): Locator {
  return page.locator('.v2-members > .palette-footer > .palette-footer-btn');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

const isDesktop = (projectName: string): boolean => projectName === 'desktop-1440';

async function openMembersPanel(page: Page): Promise<void> {
  await openGeneralThread(page);
  await page.locator('.mobile-members').click();
  await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  await page.waitForTimeout(PANEL_SETTLE_MS);
}

/** The button is full width in its column and sits at the column's bottom. */
async function expectPinnedAtColumnBottom(button: Locator, column: Locator): Promise<void> {
  await expect(button).toBeVisible();
  const btn = (await button.boundingBox())!;
  const col = (await column.boundingBox())!;
  expect(btn.x, 'inside the column, left').toBeGreaterThanOrEqual(col.x);
  expect(btn.x + btn.width, 'inside the column, right').toBeLessThanOrEqual(col.x + col.width);
  expect(btn.width, 'full width').toBeGreaterThanOrEqual(col.width - 2 * FOOTER_PAD_PX);
  expect(col.y + col.height - (btn.y + btn.height), 'at the bottom').toBeLessThanOrEqual(
    FOOTER_PAD_PX
  );
  expect(col.y + col.height - (btn.y + btn.height), 'at the bottom').toBeGreaterThanOrEqual(0);
}

test('@static the header has no quick-switcher button', async ({ page }) => {
  await openChatRail(page);
  await expect(railButton(page)).toBeVisible();
  await expect(page.locator('scion-header .palette-button')).toHaveCount(0);
});

test('@static the button sits at the bottom of the spaces column', async ({ page }, testInfo) => {
  await openChatRail(page);
  await expectPinnedAtColumnBottom(railButton(page), page.locator('.v2-rail'));
  const btn = (await railButton(page).boundingBox())!;
  if (!isDesktop(testInfo.project.name)) {
    // The spaces column fills the screen: the button is at the screen's bottom.
    const viewport = page.viewportSize()!;
    expect(btn.y + btn.height).toBeLessThanOrEqual(viewport.height);
    expect(viewport.height - (btn.y + btn.height)).toBeLessThanOrEqual(FOOTER_PAD_PX);
    expect(Math.round(btn.height), '44px tap target').toBeGreaterThanOrEqual(44);
  }
});

test('@static wide layout: no button in the members column', async ({ page }, testInfo) => {
  test.skip(!isDesktop(testInfo.project.name), 'side-by-side layout only');
  await openChatRail(page);
  await openGeneralThread(page);
  await expect(railButton(page)).toBeVisible();
  await expect(membersButton(page)).toHaveCount(0);
  await expect(page.locator('.palette-footer-btn')).toHaveCount(1);
});

test('@static phone: the members column has its own button at the bottom', async ({
  page,
}, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'panel concept is mobile-only');
  await openChatRail(page);
  await openMembersPanel(page);
  await expectPinnedAtColumnBottom(membersButton(page), page.locator('.v2-members'));
  const btn = (await membersButton(page).boundingBox())!;
  const viewport = page.viewportSize()!;
  expect(viewport.height - (btn.y + btn.height)).toBeLessThanOrEqual(FOOTER_PAD_PX);
  expect(Math.round(btn.height), '44px tap target').toBeGreaterThanOrEqual(44);
});

test('@static the spaces column button opens the palette', async ({ page }) => {
  await openChatRail(page);
  await railButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(railButton(page)).toBeFocused();
});

test('@static phone: the members column button opens the palette', async ({ page }, testInfo) => {
  test.skip(isDesktop(testInfo.project.name), 'panel concept is mobile-only');
  await openChatRail(page);
  await openMembersPanel(page);
  await membersButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(membersButton(page)).toBeFocused();
});

test('@static the button stays put while the spaces list scrolls', async ({ page }) => {
  await openChatRail(page);
  await expect(railButton(page)).toBeVisible();
  const before = await railButton(page).boundingBox();

  const scrolled = await page.locator('scion-chat-space-rail .rail-body').evaluate((body) => {
    // A tall spacer makes the list overflow at every viewport.
    const spacer = document.createElement('div');
    spacer.style.height = '3000px';
    body.append(spacer);
    body.scrollTop = body.scrollHeight;
    return body.scrollTop;
  });
  expect(scrolled).toBeGreaterThan(0);
  expect(await railButton(page).boundingBox()).toEqual(before);
});
