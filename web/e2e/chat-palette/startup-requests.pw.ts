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
 * Chromium: the requests a cold chat startup makes, and when the rail is
 * usable. The tab-title unread counter, the page and the rail all need the
 * space and DM lists at startup; they must share one request each. Thread
 * lists load only for the spaces the user can see, and the rail shows
 * project names without waiting for slow thread responses.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks, type TrackedRequest } from './mock-api.js';

const SPACE_COUNT = 7;
const SPACES = Array.from({ length: SPACE_COUNT }, (_, i) => ({
  projectId: `project-${i}`,
  projectName: `Project ${i}`,
  projectSlug: `project-${i}`,
  unreadCount: i === 2 ? 3 : 0,
  hasUnreadMention: false,
  // Newest first by index, so activity order is Project 0..6.
  lastActivityAt: new Date(Date.UTC(2026, 9, 5, 12 - i)).toISOString(),
}));
const THREADS_BY_PROJECT_ID = Object.fromEntries(
  SPACES.map((s, i) => [
    s.projectId,
    [
      {
        id: `thread-${i}-general`,
        projectId: s.projectId,
        name: 'general',
        isGeneral: true,
        lastActivityAt: s.lastActivityAt,
      },
      {
        id: `thread-${i}-design`,
        projectId: s.projectId,
        name: `design-${i}`,
        isGeneral: false,
        lastActivityAt: s.lastActivityAt,
      },
    ],
  ])
);

const LIST_DELAY_MS = 300;
/** Long enough that a rail gated on thread lists would be visibly late. */
const THREAD_DELAY_MS = 1500;

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Routes registered after `setupApiMocks` win over its handlers. Delays
 * stand in for a loaded hub: list endpoints take a little while and thread
 * lists take much longer, so a rail gated on threads is visibly late.
 */
interface SlowListOptions {
  threadDelayMs?: number;
  /** Serve spaces without `lastActivityAt`, as a hub that predates the field does. */
  omitSpaceActivity?: boolean;
  /** Most thread requests the browser had open at once, updated as they run. */
  concurrency?: { current: number; max: number };
  /** Thread responses wait for this as well as the delay (a test-held gate). */
  threadGate?: Promise<void>;
}

async function routeSlowChatLists(
  page: Page,
  options: SlowListOptions = {}
): Promise<TrackedRequest[]> {
  const {
    threadDelayMs = THREAD_DELAY_MS,
    omitSpaceActivity = false,
    concurrency,
    threadGate,
  } = options;
  const spaces = omitSpaceActivity
    ? SPACES.map(({ lastActivityAt: _omitted, ...rest }) => rest)
    : SPACES;
  // Counted from the browser's own request stream: the handlers below take
  // precedence over the fixture's, so its own request log never sees these.
  const requests: TrackedRequest[] = [];
  page.on('request', (request) => {
    if (!new URL(request.url()).pathname.startsWith('/api/v1/')) return;
    requests.push({ method: request.method(), url: request.url(), postData: request.postData() });
  });
  await page.route('**/api/v1/chat/spaces', async (route) => {
    await delay(LIST_DELAY_MS);
    await route.fulfill({ json: { spaces } });
  });
  await page.route('**/api/v1/chat/dms', async (route) => {
    await delay(LIST_DELAY_MS);
    await route.fulfill({ json: { dms: [] } });
  });
  await page.route(/\/api\/v1\/chat\/spaces\/[^/]+\/threads$/, async (route) => {
    const projectId = decodeURIComponent(
      new URL(route.request().url()).pathname.split('/').at(-2) ?? ''
    );
    if (concurrency) {
      concurrency.current++;
      concurrency.max = Math.max(concurrency.max, concurrency.current);
    }
    await Promise.all([delay(threadDelayMs), threadGate]);
    if (concurrency) concurrency.current--;
    await route.fulfill({ json: { threads: THREADS_BY_PROJECT_ID[projectId] ?? [] } });
  });
  return requests;
}

function countPath(requests: TrackedRequest[], pathname: string | RegExp): number {
  return requests.filter((r) => {
    if (r.method !== 'GET') return false;
    const path = new URL(r.url).pathname;
    return typeof pathname === 'string' ? path === pathname : pathname.test(path);
  }).length;
}

function threadCalls(requests: TrackedRequest[]): string[] {
  return requests
    .filter((r) => r.method === 'GET')
    .map((r) => new URL(r.url).pathname)
    .filter((p) => /^\/api\/v1\/chat\/spaces\/[^/]+\/threads$/.test(p))
    .map((p) => decodeURIComponent(p.split('/').at(-2) ?? ''));
}

/** Milliseconds from navigation start until the rail shows project names. */
async function waitForRailNames(page: Page): Promise<number> {
  const handle = await page.waitForFunction(
    (count) => {
      const rail = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-chat-space-rail');
      const names = rail?.shadowRoot?.querySelectorAll('.space-name') ?? [];
      return names.length >= count ? performance.now() : false;
    },
    SPACE_COUNT,
    { polling: 'raf', timeout: 10_000 }
  );
  return (await handle.jsonValue()) as number;
}

function railThreadNames(page: Page) {
  return page.locator('scion-chat-space-rail .thread-item .thread-name');
}

/** Report one measurement as a test annotation, so the numbers are recorded with the run. */
function record(name: string, value: string): void {
  test.info().annotations.push({ type: name, description: value });
  console.log(`[startup] ${test.info().title} :: ${name}=${value}`);
}

test('a cold /chat startup makes one spaces and one DMs request and no collapsed-space thread requests', async ({
  page,
}) => {
  await setupApiMocks(page);
  const requests = await routeSlowChatLists(page);
  await page.goto('/e2e/chat-palette/fixture.html?unread=1', { waitUntil: 'domcontentloaded' });

  // Recorded for the report, not asserted: it includes the fixture's own
  // module loading and varies with machine load. That the rail does not
  // wait for thread lists is asserted below (none is requested) and,
  // deterministically, in the deep-link test.
  const railAt = await waitForRailNames(page);
  record('rail-names-ms', railAt.toFixed(0));
  // The rollup badge is shown with the names.
  await expect(page.locator('scion-chat-space-rail .unread-badge')).toHaveText('3');

  // Let any trailing startup work settle before counting.
  await page.waitForTimeout(THREAD_DELAY_MS + 500);
  record('spaces', String(countPath(requests, '/api/v1/chat/spaces')));
  record('dms', String(countPath(requests, '/api/v1/chat/dms')));
  record('threads', String(threadCalls(requests).length));
  record('agents', String(countPath(requests, '/api/v1/agents')));
  record('api-total', String(requests.filter((r) => r.method === 'GET').length));

  expect.soft(countPath(requests, '/api/v1/chat/spaces')).toBe(1);
  expect.soft(countPath(requests, '/api/v1/chat/dms')).toBe(1);
  // The members sidebar walks the hub's agents once: the re-parse after
  // rail-loaded joins the finished walk instead of starting another.
  expect.soft(countPath(requests, '/api/v1/agents')).toBe(1);
  // Every space starts collapsed and none is selected.
  expect.soft(threadCalls(requests)).toEqual([]);
  // Activity order comes from the spaces response, not from thread lists.
  await expect(page.locator('scion-chat-space-rail .space-name')).toHaveText(
    SPACES.map((s) => s.projectName)
  );
});

test('expanding a collapsed space loads only its threads, with a loading state meanwhile', async ({
  page,
}) => {
  await setupApiMocks(page);
  const requests = await routeSlowChatLists(page, { threadDelayMs: 800 });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await waitForRailNames(page);

  // Mobile-style expansion without selecting a thread: the chevron path on
  // desktop also opens #general, which needs the threads first.
  await page.evaluate(() => {
    const rail = document
      .querySelector('scion-page-chat')
      ?.shadowRoot?.querySelector('scion-chat-space-rail') as
      | (HTMLElement & { expandSpace(id: string): void })
      | null;
    rail?.expandSpace('project-4');
  });
  await expect(page.locator('scion-chat-space-rail .threads-loading')).toHaveCount(1);
  await expect(railThreadNames(page)).toHaveText(['general', 'design-4']);
  await expect(page.locator('scion-chat-space-rail .threads-loading')).toHaveCount(0);
  expect(threadCalls(requests)).toEqual(['project-4']);
});

test('a thread deep link loads only the selected space threads and expands it', async ({
  page,
}) => {
  await setupApiMocks(page);
  // Held until the rail shows its names: names that render while the only
  // thread request is still unanswered prove the rail does not wait for it.
  let releaseThreads: () => void = () => {};
  const threadGate = new Promise<void>((resolve) => {
    releaseThreads = resolve;
  });
  const requests = await routeSlowChatLists(page, { threadDelayMs: 0, threadGate });
  const route = encodeURIComponent('/chat/space/project-5/thread/thread-5-design');
  await page.goto(`/e2e/chat-palette/fixture.html?unread=1&route=${route}`, {
    waitUntil: 'domcontentloaded',
  });

  const railAt = await waitForRailNames(page);
  record('rail-names-ms', railAt.toFixed(0));
  await expect(page.locator('scion-chat-space-rail .threads-loading')).toHaveCount(1);
  await expect.poll(() => threadCalls(requests)).toEqual(['project-5']);
  releaseThreads();

  await expect(page.locator('scion-chat-space-rail .thread-item.selected .thread-name')).toHaveText(
    'design-5'
  );
  await page.waitForTimeout(500);
  record('spaces', String(countPath(requests, '/api/v1/chat/spaces')));
  record('dms', String(countPath(requests, '/api/v1/chat/dms')));
  record('threads', String(threadCalls(requests).length));
  expect.soft(countPath(requests, '/api/v1/chat/spaces')).toBe(1);
  expect.soft(countPath(requests, '/api/v1/chat/dms')).toBe(1);
  expect(threadCalls(requests)).toEqual(['project-5']);
});

test('without per-space activity, activity sort loads the other spaces in the background, two at a time', async ({
  page,
}) => {
  await setupApiMocks(page);
  const concurrency = { current: 0, max: 0 };
  const requests = await routeSlowChatLists(page, {
    threadDelayMs: 400,
    omitSpaceActivity: true,
    concurrency,
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });

  // Names still render without waiting for any thread list.
  const railAt = await waitForRailNames(page);
  record('rail-names-ms', railAt.toFixed(0));

  // Activity order is derived from the thread lists, so every space's list
  // loads — off the critical path, and never more than two at once.
  await expect.poll(() => threadCalls(requests).length).toBe(SPACE_COUNT);
  await expect(page.locator('scion-chat-space-rail .space-name')).toHaveText(
    SPACES.map((s) => s.projectName)
  );
  expect(concurrency.max).toBeLessThanOrEqual(2);
  expect(countPath(requests, '/api/v1/chat/spaces')).toBe(1);
});
