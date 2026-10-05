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
 * Chromium, real <scion-chat-shell>: the unread conversation badge on the
 * header's chat mode selector. At phone widths (the mode dropdown) and at
 * desktop width (the labelled segmented control) the badge shows the count
 * from the unread endpoint, keeps the header at its fixed 60px, never
 * covers the "Chat" label, and the old envelope button is gone.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';

async function gotoShell(page: Page, unreadConversations: number): Promise<void> {
  await setupApiMocks(page, { unreadConversations });
  await page.goto('/e2e/chat-palette/fixture.html?shell=1&unread=1', {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

interface Box {
  left: number;
  right: number;
  top: number;
  bottom: number;
  width: number;
  height: number;
}

interface Geometry {
  headerHeight: number;
  headerTop: number;
  /** The visible badge's text and box, or null when none is shown. */
  badge: { text: string; box: Box } | null;
  /** The visible "Chat" label next to the badge, if any. */
  label: Box | null;
  /** The control the badge belongs to. */
  control: Box | null;
  envelopes: number;
}

/** Reads the header's geometry through the shell and header shadow roots. */
function readGeometry(page: Page): Promise<Geometry> {
  return page.evaluate(() => {
    const header = document
      .querySelector('scion-chat-shell')!
      .shadowRoot!.querySelector('scion-header')!;
    const root = header.shadowRoot!;
    const box = (el: Element): Box => {
      const r = el.getBoundingClientRect();
      return {
        left: r.left,
        right: r.right,
        top: r.top,
        bottom: r.bottom,
        width: r.width,
        height: r.height,
      };
    };
    const visible = (el: Element): boolean => {
      const r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    };
    const badgeEl = [...root.querySelectorAll('.chat-unread-badge')].find(visible) ?? null;
    const control = badgeEl?.closest('button') ?? null;
    const labelEl = control
      ? ([...control.querySelectorAll('.mode-label, .mode-dropdown-label')].find(visible) ?? null)
      : null;
    const headerBox = header.getBoundingClientRect();
    return {
      headerHeight: headerBox.height,
      headerTop: headerBox.top,
      badge: badgeEl ? { text: badgeEl.textContent?.trim() ?? '', box: box(badgeEl) } : null,
      label: labelEl ? box(labelEl) : null,
      control: control ? box(control) : null,
      envelopes: root.querySelectorAll('sl-icon-button[name="envelope"]').length,
    };
  });
}

function overlaps(a: Box, b: Box): boolean {
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
}

for (const width of [320, 375, 390, 1440]) {
  test(`at ${width}px the chat selector shows the unread count without growing the header or covering the label`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 800 });
    await gotoShell(page, 3);

    await expect
      .poll(async () => (await readGeometry(page)).badge?.text ?? null, { timeout: 5000 })
      .toBe('3');
    const g = await readGeometry(page);

    // The fixed 60px header row (no safe-area inset in this headless run).
    expect(g.headerHeight).toBe(61); // 60px row plus the 1px bottom border
    expect(g.envelopes).toBe(0);

    const badge = g.badge!.box;
    // Inside the header and inside its control.
    expect(badge.top).toBeGreaterThanOrEqual(g.headerTop);
    expect(badge.bottom).toBeLessThanOrEqual(g.headerTop + g.headerHeight);
    expect(g.control).not.toBeNull();
    expect(badge.left).toBeGreaterThanOrEqual(g.control!.left);
    expect(badge.right).toBeLessThanOrEqual(g.control!.right);
    expect(badge.top).toBeGreaterThanOrEqual(g.control!.top);

    if (width > 1100) {
      // The labelled segmented control: the badge never covers "Chat".
      expect(g.label).not.toBeNull();
      expect(overlaps(badge, g.label!)).toBe(false);
    }
  });
}

test('at 1440px a three-digit count caps at 99+ and still clears the label', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 800 });
  await gotoShell(page, 250);
  await expect
    .poll(async () => (await readGeometry(page)).badge?.text ?? null, { timeout: 5000 })
    .toBe('99+');
  const g = await readGeometry(page);
  expect(g.headerHeight).toBe(61);
  expect(overlaps(g.badge!.box, g.label!)).toBe(false);
});

test('shows no badge at zero, and the header height is the same', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await gotoShell(page, 0);
  // Give the counter's request time to land.
  await page.waitForTimeout(500);
  const g = await readGeometry(page);
  expect(g.badge).toBeNull();
  expect(g.headerHeight).toBe(61);
});

test('at phone width the Chat item in the mode menu carries the count', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await gotoShell(page, 4);
  const trigger = page.locator('scion-header .compact-mode-dropdown .mode-trigger');
  await expect(trigger).toHaveAttribute('aria-label', 'Switch view mode, 4 unread conversations');
  await trigger.click();
  const count = page.locator('scion-header sl-menu-item[value="chat"] .chat-count');
  await expect(count).toBeVisible();
  await expect(count).toHaveText('4');
});
