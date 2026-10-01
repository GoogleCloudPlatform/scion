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
 * Chromium + axe: the palette dialog's label, combobox/listbox/group
 * relationships and active-option/status-region announcements have zero
 * critical/serious violations across normal, empty and error states, at
 * desktop and 480px width, in light and dark theme.
 */

import { test, expect, type Page } from '@playwright/test';
import { AxeBuilder } from '@axe-core/playwright';
import {
  setupApiMocks,
  SPACE_ALPHA,
  THREAD_ALPHA,
  USER_WITH_DM,
  AGENT_WITH_DM,
} from './mock-api.js';

const POPULATED_FIXTURE = {
  spaces: [SPACE_ALPHA],
  threadsByProjectId: { [SPACE_ALPHA.projectId]: [THREAD_ALPHA] },
  users: [USER_WITH_DM],
};

async function gotoChat(page: Page, overrides: Parameters<typeof setupApiMocks>[1] = {}) {
  await setupApiMocks(page, overrides);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

function paletteDialog(page: Page) {
  return page.locator('scion-chat-switcher sl-dialog[label="Quick switcher"]');
}

function groupOptions(page: Page, group: string) {
  return page.locator(
    `scion-chat-switcher [aria-labelledby="palette-heading-${group}"] .palette-option`
  );
}

/** Run an axe scan scoped to the palette subtree and assert zero critical/serious violations. The two-element selector array pierces scion-page-chat's shadow root to reach scion-chat-switcher. */
async function assertPaletteAxeClean(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    // A 2+ element array is axe-core's shadow-DOM-piercing selector path
    // (unlike a bare CSS-selector string, which only ever searches light
    // DOM and silently matches nothing here — `scion-chat-switcher` lives
    // inside `scion-page-chat`'s own shadow root). Scoped to the palette
    // itself, not the whole fixture page: this isolated fixture never mounts
    // the real app shell (header, base layout) that would normally supply
    // whatever sets the page's own default text/background colors, so a
    // full-page scan surfaces pre-existing, unrelated Shoelace-chrome
    // contrast findings this fixture has no way to reproduce faithfully.
    // Scoping to the palette itself still checks its real contrast for
    // real, against the real `theme.css` tokens loaded by this fixture.
    .include(['scion-page-chat', 'scion-chat-switcher'])
    .analyze();

  const serious = results.violations.filter(
    (v) => v.impact === 'serious' || v.impact === 'critical'
  );
  if (serious.length > 0) {
    const summary = serious
      .map((v) => `[${v.impact}] ${v.id}: ${v.description} (${v.nodes.length} instance(s))`)
      .join('\n');
    expect(serious, `Accessibility violations:\n${summary}`).toHaveLength(0);
  }
}

/** Mirrors header.ts's real dark-mode toggle exactly — both the class and the attribute, not just one. */
async function enableDarkTheme(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.documentElement.classList.add('sl-theme-dark');
    document.documentElement.setAttribute('data-theme', 'dark');
  });
  await page.waitForTimeout(200);
}

test('normal state (populated groups) passes axe — desktop, light theme', async ({ page }) => {
  await gotoChat(page, POPULATED_FIXTURE);
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'threads')).toHaveCount(1);
  await expect(groupOptions(page, 'people')).toHaveCount(1);
  await expect(paletteDialog(page)).toBeVisible();

  await assertPaletteAxeClean(page);
});

/** WCAG 2 relative-luminance contrast ratio for two opaque rgb() strings, used to assert the palette's muted text directly. */
function contrastRatio(rgbA: string, rgbB: string): number {
  const parse = (rgb: string): [number, number, number] => {
    const m = rgb.match(/\d+/g);
    if (!m || m.length < 3) throw new Error(`not an rgb() string: ${rgb}`);
    return [Number(m[0]), Number(m[1]), Number(m[2])];
  };
  const luminance = ([r, g, b]: [number, number, number]): number => {
    const channel = (c: number) => {
      const s = c / 255;
      return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    };
    return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
  };
  const [l1, l2] = [luminance(parse(rgbA)), luminance(parse(rgbB))].sort((a, b) => b - a);
  return (l1 + 0.05) / (l2 + 0.05);
}

/**
 * Filters to the one candidate with a known secondary label (the agent's
 * slug) so it's guaranteed to be the globally-active row — and so carries
 * the active row's own subtle-tinted background — then asserts its
 * secondary-label text, and the keyboard-hint `kbd` chips' own text, each
 * clear AA contrast against their own background. The `kbd` chips are
 * always rendered (not tied to which row is active) and carry their own
 * background, unlike the secondary label, which only means something
 * against the specific active row's tinted background.
 */
async function assertActiveSecondaryLabelContrast(page: Page): Promise<void> {
  await page.keyboard.press('Control+k');
  await page.locator('scion-chat-switcher #palette-query-input').fill(AGENT_WITH_DM.name);
  await expect(groupOptions(page, 'agents')).toHaveCount(1);
  const secondary = page.locator('scion-chat-switcher .palette-option.active .palette-secondary');
  await expect(secondary).toHaveText(AGENT_WITH_DM.slug);

  const { color, backgroundColor } = await secondary.evaluate((el) => {
    const optionEl = el.closest('.palette-option');
    return {
      color: getComputedStyle(el).color,
      backgroundColor: optionEl ? getComputedStyle(optionEl).backgroundColor : '',
    };
  });
  expect(contrastRatio(color, backgroundColor)).toBeGreaterThanOrEqual(4.5);

  const kbd = page.locator('scion-chat-switcher .palette-help kbd').first();
  const kbdColors = await kbd.evaluate((el) => {
    const style = getComputedStyle(el);
    return { color: style.color, backgroundColor: style.backgroundColor };
  });
  expect(contrastRatio(kbdColors.color, kbdColors.backgroundColor)).toBeGreaterThanOrEqual(4.5);
}

test("the active row's secondary label meets AA contrast against the selected-row background — light theme", async ({
  page,
}) => {
  await gotoChat(page, POPULATED_FIXTURE);
  await assertActiveSecondaryLabelContrast(page);
  await assertPaletteAxeClean(page);
});

test("the active row's secondary label meets AA contrast against the selected-row background — dark theme", async ({
  page,
}) => {
  // A color that only clears AA against the light-mode background (a raw
  // light-mode-appropriate value, rather than a theme-aware token) would
  // pass the light-theme test above yet fail badly here — dark mode needs a
  // *lighter* color against its own darker --scion-bg-subtle, not the same
  // fixed value.
  await gotoChat(page, POPULATED_FIXTURE);
  await enableDarkTheme(page);
  await assertActiveSecondaryLabelContrast(page);
  await assertPaletteAxeClean(page);
});

test('empty state (no query matches in any group) passes axe', async ({ page }) => {
  await gotoChat(page, POPULATED_FIXTURE);
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'threads')).toHaveCount(1);
  await page.locator('scion-chat-switcher #palette-query-input').fill('zzz-no-such-match-zzz');
  await expect(page.locator('scion-chat-switcher .palette-option')).toHaveCount(0);
  await expect(
    page.locator('scion-chat-switcher [aria-labelledby="palette-heading-agents"] .palette-empty')
  ).toBeVisible();

  await assertPaletteAxeClean(page);
});

test('error state (a group failed to load) passes axe', async ({ page }) => {
  await setupApiMocks(page, POPULATED_FIXTURE);
  await page.route('**/api/v1/agents*', (route) =>
    route.fulfill({ status: 500, json: { error: { code: 'internal', message: 'boom' } } })
  );
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.keyboard.press('Control+k');
  await expect(
    page.locator('scion-chat-switcher [data-palette-group="agents"] .palette-group-error')
  ).toBeVisible();

  await assertPaletteAxeClean(page);
});

test('normal state passes axe at a 480px viewport (single-column layout)', async ({ page }) => {
  await page.setViewportSize({ width: 480, height: 800 });
  await gotoChat(page, POPULATED_FIXTURE);
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'threads')).toHaveCount(1);

  await assertPaletteAxeClean(page);
});

test('normal state passes axe in dark theme', async ({ page }) => {
  await gotoChat(page, POPULATED_FIXTURE);
  await enableDarkTheme(page);
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'threads')).toHaveCount(1);

  await assertPaletteAxeClean(page);
});

test('empty state passes axe at 480px in dark theme', async ({ page }) => {
  await page.setViewportSize({ width: 480, height: 800 });
  await gotoChat(page, POPULATED_FIXTURE);
  await enableDarkTheme(page);
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'threads')).toHaveCount(1);
  await page.locator('scion-chat-switcher #palette-query-input').fill('zzz-no-such-match-zzz');
  await expect(page.locator('scion-chat-switcher .palette-option')).toHaveCount(0);

  await assertPaletteAxeClean(page);
});

test('Escape exits the dialog and returns focus to the invoker', async ({ page }) => {
  await gotoChat(page, POPULATED_FIXTURE);
  // No composer is mounted (no conversation open), so the page's own
  // focusable container stands in as the invoker — focusing it directly
  // (rather than relying on whatever the browser happens to focus by
  // default) is what makes the later "Escape returned focus here" assertion
  // meaningful rather than incidental.
  const invoker = page.locator('#palette-focus-fallback');
  await invoker.focus();
  await expect(invoker).toBeFocused();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(invoker).toBeFocused();
});
