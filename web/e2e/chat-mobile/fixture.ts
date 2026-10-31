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

/** Shared navigation helpers for the chat-mobile spec files. */

import { expect, type Page } from '@playwright/test';
import { setupChatMobileMocks, PROJECT_A } from './mock-api.js';

/**
 * Install the mocks and land on the rail for PROJECT_A (mobilePanel defaults to 'left').
 * On desktop, return once PROJECT_A's #general has opened on the page element
 * that stays, so callers never measure an element the router is about to replace.
 *
 * `overrideMocks`, when given, runs after the shared mocks are installed and
 * before navigation, so any route it adds takes precedence over the shared
 * one for the same URL (Playwright runs the most recently added route first).
 */
export async function openChatRail(
  page: Page,
  overrideMocks?: (page: Page) => Promise<void>
): Promise<void> {
  await setupChatMobileMocks(page);
  if (overrideMocks) await overrideMocks(page);
  await page.goto(`/chat/space/${PROJECT_A.id}`, { waitUntil: 'domcontentloaded' });
  // The legacy /chat/space/{id} URL is rewritten in place to the readable
  // slug URL once spaces load (parseV2Route's legacySpaceMatch ->
  // replaceRoute), on the same page element. On desktop the space then
  // opens #general with a navigateTo, which does replace the page element.
  // Waiting for the URL to settle to the readable form first keeps callers
  // off the page before the space has opened.
  if (isWideLayout(page)) {
    await waitForDesktopGeneralThread(page);
    return;
  }
  await page.waitForURL(/\/chat\/[^/]+$/, { timeout: 15_000 });
  await expect(
    page.locator('.space-header', { hasText: PROJECT_A.name }),
    'the space header shows on the rail'
  ).toBeVisible({ timeout: 15_000 });
}

/**
 * The chat page's phone breakpoint: MOBILE_BREAKPOINT_PX in
 * src/components/pages/chat.ts, where a width at or below it is a phone.
 */
const CHAT_MOBILE_BREAKPOINT_PX = 768;

/** Is the viewport above the chat page's phone breakpoint? */
function isWideLayout(page: Page): boolean {
  return (page.viewportSize()?.width ?? 0) > CHAT_MOBILE_BREAKPOINT_PX;
}

/**
 * A thread URL in PROJECT_A: /chat/{slug}/{thread id}. Anchored on the slug
 * because the legacy landing URL /chat/space/{id} has the same shape.
 */
const THREAD_PATH = new RegExp(`^/chat/${PROJECT_A.slug}/[^/?#]+$`);

/** The path the router last rendered a page for, without query or hash. */
async function renderedPath(page: Page): Promise<string> {
  return page.evaluate(() => {
    const shell = document.querySelector('scion-chat-shell') as unknown as {
      currentPath?: string;
    } | null;
    return (shell?.currentPath ?? '').split(/[?#]/)[0];
  });
}

/**
 * Wait for the desktop landing to finish opening #general.
 *
 * chat.ts's selectSpaceBySlug opens #general on the page element that
 * handled the legacy URL, then calls navigateTo with the thread URL. The
 * router pushes the URL first and swaps in a new page element after an
 * await, so the URL and the thread header can both show before the swap.
 * The shell's currentPath is set in the same step as the swap, so once it
 * names the thread the page in the DOM is the one that stays.
 */
async function waitForDesktopGeneralThread(page: Page): Promise<void> {
  await expect
    .poll(() => new URL(page.url()).pathname, {
      message: 'the URL names a thread in the space',
      timeout: 15_000,
    })
    .toMatch(THREAD_PATH);
  await expect
    .poll(() => renderedPath(page), {
      message: 'the router renders the thread URL on a new page element',
      timeout: 15_000,
    })
    .toMatch(THREAD_PATH);
  await expect(
    page.locator('.space-header', { hasText: PROJECT_A.name }),
    'the space header shows on the new page element'
  ).toBeVisible({ timeout: 15_000 });
  await expect(
    page.locator('.v2-thread-header'),
    'the #general thread header shows on the new page element'
  ).toBeVisible({ timeout: 15_000 });
}

/**
 * Wait for PROJECT_A's thread list to expand.
 *
 * Landing on the legacy `/chat/space/{projectId}` URL triggers chat.ts's own
 * rewrite-and-select cascade (parseV2Route's legacySpaceMatch ->
 * replaceRoute(the readable slug URL) -> singleMatch -> selectSpaceBySlug),
 * which calls the rail's `expandSpace(projectId)` once the rail's
 * `rail-loaded` event fires — no click needed, and clicking the header
 * ourselves races that cascade: landing between "not yet expanded" and
 * "about to auto-expand" would toggle it right back to collapsed.
 */
export async function expandSpace(page: Page): Promise<void> {
  const generalRow = page.locator('.thread-item', { hasText: 'general' });
  await expect(generalRow.first()).toBeVisible({ timeout: 15_000 });
}

/** How long the panel-track CSS transform transition takes to settle. */
const PANEL_TRANSITION_SETTLE_MS = 400;

/** Expand the space and open the #general thread — lands on the centre panel. */
export async function openGeneralThread(page: Page): Promise<void> {
  await expandSpace(page);
  await page.locator('.thread-item', { hasText: 'general' }).first().click();
  await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 10_000 });
  // Let the 0.3s panel transform transition finish before anything measures
  // the active panel's position.
  await page.waitForTimeout(PANEL_TRANSITION_SETTLE_MS);
}

/** Read the current `data-panel` value off `.v2-panels`. */
export async function currentPanel(page: Page): Promise<string | null> {
  return page.locator('.v2-panels').getAttribute('data-panel');
}
