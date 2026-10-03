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
  // The legacy /chat/space/{id} URL triggers a client-side redirect to the
  // readable slug URL once spaces load (parseV2Route's legacySpaceMatch ->
  // navigateTo). That redirect is a full client-side navigation, and
  // main.ts's renderRoute handles every navigation (other than returning
  // from a hidden terminal view) by removing the old page element and
  // creating a new one — so a caller that proceeds as soon as the *first*
  // <scion-page-chat> instance renders can measure or click into a page
  // that is about to be torn down and replaced. Waiting for the URL to
  // settle to the redirected form first means every caller sees the one
  // instance that is actually going to stick around.
  await page.waitForURL(/\/chat\/[^/]+$/, { timeout: 15_000 });
  await expect(page.locator('.space-header', { hasText: PROJECT_A.name })).toBeVisible({
    timeout: 15_000,
  });
}

/**
 * Wait for PROJECT_A's thread list to expand.
 *
 * Landing on the legacy `/chat/space/{projectId}` URL triggers chat.ts's own
 * redirect-and-select cascade (parseV2Route's legacySpaceMatch ->
 * navigateTo(the readable slug URL) -> singleMatch -> selectSpaceBySlug),
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
