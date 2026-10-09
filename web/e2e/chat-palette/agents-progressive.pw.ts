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
 * Chromium: the palette's Agents group publishing candidates progressively
 * against a real, multi-page `/api/v1/agents`, and a refresh of an
 * already-populated group never regressing to a smaller, partial list.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';
import { routeAgentPages } from './route-agent-pages.js';

const PAGE_ONE_AGENT = { id: 'agent-page-one', name: 'Page One Agent', slug: 'page-one' };
const PAGE_TWO_AGENT = { id: 'agent-page-two', name: 'Page Two Agent', slug: 'page-two' };

/**
 * Opens the chat page. Agent-list routes are registered before this: the
 * page's members sidebar starts the agent store's hub walk on mount, and the
 * palette reads (or joins) that walk.
 */
async function gotoChat(page: Page): Promise<void> {
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

function paletteOptions(page: Page) {
  return page.locator('scion-quick-palette .palette-option');
}

function agentsLoading(page: Page) {
  return page.locator('scion-quick-palette [data-palette-group="agents"] .palette-loading');
}

/**
 * Dispatch the event the member editors send after a membership change,
 * which makes the agent store revalidate every list it holds.
 */
async function simulateMembershipChange(page: Page): Promise<void> {
  await page.evaluate(() => {
    window.dispatchEvent(new CustomEvent('scion:membership-changed'));
  });
}

test('the first page is published as soon as it arrives, without waiting for a slower second page to land', async ({
  page,
}) => {
  await setupApiMocks(page);
  // Route added after setupApiMocks so it takes precedence over the
  // fixture's own default `/api/v1/agents` handler (last-registered wins).
  const { callCount } = routeAgentPages(
    page,
    [{ agents: [PAGE_ONE_AGENT], nextCursor: 'page-2' }, { agents: [PAGE_TWO_AGENT] }],
    { '': 0, 'page-2': 2_000 }
  );
  await gotoChat(page);

  // Opened while the walk the page started on mount is still reading its
  // slower second page: the palette joins it.
  await page.keyboard.press('Control+k');

  // The first page is visible well before the second page's own 2s
  // artificial delay would have elapsed — proof this is not held back behind
  // the slower page. A 1s assertion window against a 2s delay keeps the same
  // discrimination with headroom against scheduling jitter.
  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible({
    timeout: 1_000,
  });
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toHaveCount(0);

  // The second page eventually lands too, and the group settles — no
  // leftover "Loading…" placeholder once every page is in.
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toBeVisible();
  await expect(agentsLoading(page)).toHaveCount(0);

  expect(callCount()).toBeGreaterThanOrEqual(2);
});

test('a refresh of an already-populated Agents group keeps showing its full list instead of shrinking to a partial first page', async ({
  page,
}) => {
  await setupApiMocks(page);
  const pages = [{ agents: [PAGE_ONE_AGENT], nextCursor: 'page-2' }, { agents: [PAGE_TWO_AGENT] }];
  routeAgentPages(page, pages, {});
  await gotoChat(page);

  await page.keyboard.press('Control+k');
  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible();
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toBeVisible();
  await expect(agentsLoading(page)).toHaveCount(0);

  // Then revalidate the store's list while the group already holds the
  // full, ready list above. The refreshed load's own first page resolves quickly, while its
  // second page stays pending — the window in which a bug would be visible:
  // if partial pages were published during this revalidation (instead of
  // only on a first load/retry), the list would shrink to just Page One
  // Agent the instant that first page lands, well before page two's own
  // request even goes out. A single long delay on page one cannot catch
  // that — the assertion would run before any progress tick existed at all,
  // regardless of whether partial pages are published. Checking the state right
  // after page one *fulfills* but before page two does is what actually
  // exercises it.
  const { requestedCursors } = routeAgentPages(page, pages, { '': 200, 'page-2': 3000 });
  await simulateMembershipChange(page);

  // Wait until page two's own request has actually gone out — proof page
  // one has already fulfilled and the pagination loop has moved on.
  await expect
    .poll(() => requestedCursors().filter((c) => c === 'page-2').length, { timeout: 5_000 })
    .toBeGreaterThan(0);

  // Page one has landed and page two is still in flight (it stays pending
  // for a further 3s) — both rows must still be visible right now, not
  // shrunk to just Page One Agent. `.count()` is a direct, non-retrying
  // read: unlike `toBeVisible()`/`toHaveCount()`, which poll for up to their
  // own timeout and so would still pass if Page Two Agent reappeared only
  // once page two eventually (3s later) resolves, this is what actually
  // catches the row being *transiently* gone in between.
  expect(await paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name }).count()).toBe(1);
  expect(await paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name }).count()).toBe(1);

  // The refresh eventually settles normally.
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toBeVisible();
  await expect(agentsLoading(page)).toHaveCount(0);
});
