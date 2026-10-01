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
 * Chromium: the palette's Agents group against a real, deliberately slow,
 * multi-page `/api/v1/agents`, invalidated repeatedly while the fetch is
 * still in flight (what a busy hub's per-agent status SSE traffic produces
 * on a hub with a long agent list). The group must still resolve to its
 * real rows, in a small, bounded number of requests — not restart its fetch
 * once per invalidation and never finish.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';

const PAGE_ONE_AGENT = { id: 'agent-page-one', name: 'Page One Agent', slug: 'page-one' };
const PAGE_TWO_AGENT = { id: 'agent-page-two', name: 'Page Two Agent', slug: 'page-two' };

/**
 * Serves `/api/v1/agents*` as a sequence of pages, keyed by the request's
 * own `cursor` query param (`''` for the first page) rather than by a
 * global call counter — a coalesced follow-up reload starts an entirely new
 * pagination run from `cursor=''` again, which a call-counter-indexed mock
 * would wrongly hand the *next* page in sequence instead of page one again.
 * `delaysMsByCursor[cursor]` is how long to wait before fulfilling the page
 * that cursor selects (0 if unspecified). An unrecognized cursor repeats the
 * last page, so a test is never surprised by an unmocked extra request.
 *
 * Returns both `callCount` (incremented when a request *starts*, i.e. before
 * its own artificial delay) and `fulfilledCount` (incremented only once
 * `route.fulfill()` has actually completed). A started-but-not-yet-fulfilled
 * request — the follow-up cycle's own page-2 request, still sitting in its
 * multi-second delay — must not read as "the follow-up has settled": only
 * `fulfilledCount` reaching the expected total means every request in that
 * cycle has actually finished, which is what the test below polls on.
 */
function routeAgentPages(
  page: Page,
  pages: Array<{ agents: Array<{ id: string; name: string; slug: string }>; nextCursor?: string }>,
  delaysMsByCursor: Record<string, number> = {}
): { callCount: () => number; fulfilledCount: () => number } {
  const pageByCursor = new Map<string, (typeof pages)[number]>();
  pageByCursor.set('', pages[0]);
  for (let i = 0; i + 1 < pages.length; i++) {
    const nextCursor = pages[i].nextCursor;
    if (nextCursor) pageByCursor.set(nextCursor, pages[i + 1]);
  }

  let started = 0;
  let fulfilled = 0;
  void page.route('**/api/v1/agents*', async (route) => {
    started++;
    const cursor = new URL(route.request().url()).searchParams.get('cursor') ?? '';
    const body = pageByCursor.get(cursor) ?? pages[pages.length - 1];
    const delayMs = delaysMsByCursor[cursor] ?? 0;
    if (delayMs > 0) await new Promise((resolve) => setTimeout(resolve, delayMs));
    await route.fulfill({
      json: {
        agents: body.agents.map((a) => ({ ...a, _capabilities: { actions: ['attach'] } })),
        ...(body.nextCursor ? { nextCursor: body.nextCursor } : {}),
      },
    });
    fulfilled++;
  });
  return { callCount: () => started, fulfilledCount: () => fulfilled };
}

/**
 * Waits until `count()` has not changed for `quietMs`, up to `timeoutMs`
 * total — used to let the fixture's own unrelated pre-palette traffic to
 * this endpoint (see `routeAgentPages`'s caller) finish arriving before a
 * test takes its baseline, rather than guessing a fixed settle delay that
 * could still race a late request.
 */
async function waitForCountToSettle(
  count: () => number,
  quietMs = 300,
  timeoutMs = 3_000
): Promise<number> {
  const deadline = Date.now() + timeoutMs;
  let last = count();
  let lastChangeAt = Date.now();
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50));
    const current = count();
    if (current !== last) {
      last = current;
      lastChangeAt = Date.now();
    } else if (Date.now() - lastChangeAt >= quietMs) {
      return last;
    }
  }
  return last;
}

function paletteOptions(page: Page) {
  return page.locator('scion-chat-switcher .palette-option');
}

function agentsLoading(page: Page) {
  return page.locator('scion-chat-switcher [data-palette-group="agents"] .palette-loading');
}

/** Dispatch the same invalidation a real per-agent status SSE event produces. */
async function simulateAgentsUpdatedEvent(page: Page): Promise<void> {
  await page.evaluate(() => {
    (
      document.querySelector('scion-page-chat') as unknown as { _handleAgentsUpdated(): void }
    )._handleAgentsUpdated();
  });
}

test('Agents invalidations during a slow multi-page fetch defer to it instead of restarting it', async ({
  page,
}) => {
  await setupApiMocks(page);
  // Registered before `goto`, so it is already in effect for every request
  // this fixture's own page load makes — including the unrelated ones it
  // makes to this same endpoint before the palette ever opens (see
  // mock-api.ts) — rather than racing them.
  const { fulfilledCount } = routeAgentPages(
    page,
    [{ agents: [PAGE_ONE_AGENT], nextCursor: 'page-2' }, { agents: [PAGE_TWO_AGENT] }],
    { '': 100, 'page-2': 4000 }
  );
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  // That pre-palette traffic settles on its own time, not a fixed delay —
  // wait for it to actually *fulfill* (not just start) before taking the
  // baseline below, so a late completion can never be mistaken for one of
  // this test's own requests.
  await waitForCountToSettle(fulfilledCount);
  const fulfilledBeforeOpen = fulfilledCount();

  // One full two-page cycle takes ~4.1s uninterrupted (100ms + 4000ms).
  // Invalidations below are spaced 600ms apart — past the 500ms debounce
  // window, so each one's own debounce tick actually fires on schedule
  // rather than being coalesced away by the next invalidation arriving too
  // soon to matter — and they all land (and finish arriving) well before the
  // cycle completes, with the last one's own debounce tick (500ms later)
  // still landing while the fetch is genuinely in flight, about 1s before
  // the cycle's own natural completion — comfortable margin against
  // scheduling jitter on a loaded CI box. No further invalidation arrives
  // after that: the only thing that can still notice the fetch has since
  // finished and start the deferred follow-up is
  // `_refreshDirtyPaletteGroups` itself rescheduling its own recheck — a
  // dropped reschedule there leaves the group dirty with no follow-up ever
  // starting, so the fulfilled count below never climbs past 2 and the poll
  // fails.
  await page.keyboard.press('Control+k');
  await expect(agentsLoading(page)).toBeVisible();

  for (let i = 0; i < 4; i++) {
    await page.waitForTimeout(600);
    await simulateAgentsUpdatedEvent(page);
  }

  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible({
    timeout: 10_000,
  });
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toBeVisible();
  await expect(agentsLoading(page)).toHaveCount(0);

  // Exactly two full two-page cycles — the original load, already settled
  // above, plus the one coalesced follow-up the 4 invalidations collapse
  // into — regardless of how many of them landed during either cycle.
  // Polling on *fulfilled* requests (not merely started ones) until the
  // follow-up's own page-2 request has actually completed, then holding
  // past the debounce window, is what actually catches both failure
  // directions: a dropped follow-up never reaches 4 fulfilled requests at
  // all (it stays at 2, the poll times out), and a follow-up that keeps
  // reloading (or restarts once per invalidation) keeps climbing past 4
  // during the hold below.
  await expect.poll(() => fulfilledCount() - fulfilledBeforeOpen, { timeout: 12_000 }).toBe(4);
  await page.waitForTimeout(1_500);
  expect(fulfilledCount() - fulfilledBeforeOpen).toBe(4);
  await expect(agentsLoading(page)).toHaveCount(0);
});
