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

import type { Page } from '@playwright/test';

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
 * Returns `callCount` (incremented when a request *starts*, i.e. before its
 * own artificial delay), `fulfilledCount` (incremented only once
 * `route.fulfill()` has actually completed — a started-but-not-yet-fulfilled
 * request must not read as settled), and `requestedCursors` (every cursor
 * seen, in request order, for callers that need to tell which page a given
 * request asked for rather than just how many have arrived).
 */
export function routeAgentPages(
  page: Page,
  pages: Array<{ agents: Array<{ id: string; name: string; slug: string }>; nextCursor?: string }>,
  delaysMsByCursor: Record<string, number> = {}
): { callCount: () => number; fulfilledCount: () => number; requestedCursors: () => string[] } {
  const pageByCursor = new Map<string, (typeof pages)[number]>();
  pageByCursor.set('', pages[0]);
  for (let i = 0; i + 1 < pages.length; i++) {
    const nextCursor = pages[i].nextCursor;
    if (nextCursor) pageByCursor.set(nextCursor, pages[i + 1]);
  }

  let started = 0;
  let fulfilled = 0;
  const requested: string[] = [];
  void page.route('**/api/v1/agents*', async (route) => {
    started++;
    const cursor = new URL(route.request().url()).searchParams.get('cursor') ?? '';
    requested.push(cursor);
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
  return {
    callCount: () => started,
    fulfilledCount: () => fulfilled,
    requestedCursors: () => requested,
  };
}
