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
 * Answers the project page's requests from buildFixture() (fixture.mjs): a
 * deterministic 100-agent project in the hub's real response shape, as a
 * non-admin project member. fixture.test.mjs keeps its field names equal
 * to the hub's (fixture-schema.json, written by pkg/hub/perf_budget_test.go).
 *
 * A request with no generated response is answered 404 and recorded in
 * `unexpected`, and the budget test fails on it, so a new request the page
 * starts sending cannot silently render an empty or error state.
 */

import type { Page } from '@playwright/test';

import { buildFixture } from './fixture.mjs';

export type BudgetFixture = ReturnType<typeof buildFixture>;

export function loadFixture(): BudgetFixture {
  return buildFixture();
}

interface RecordedResponse {
  status: number;
  body: unknown;
}

/**
 * Installs the mocks on page and returns the list that collects requests
 * with no generated response. Call before `page.goto`.
 */
export async function setupBudgetMocks(page: Page, fx: BudgetFixture): Promise<string[]> {
  const unexpected: string[] = [];
  // No server behind the mocks: replace EventSource with an inert stub that
  // reports open, as e2e/chat-mobile does.
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });
  await page.route(/\/(api|auth)\//, (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const key = url.pathname + url.search;
    if (req.method() === 'GET' && url.pathname === '/auth/me') {
      return route.fulfill({ json: fx.me });
    }
    const rec =
      req.method() === 'GET' ? (fx.responses as Record<string, RecordedResponse>)[key] : undefined;
    if (!rec) {
      unexpected.push(`${req.method()} ${key}`);
      return route.fulfill({ status: 404, json: { error: 'no recorded response' } });
    }
    if (typeof rec.body === 'string') {
      return route.fulfill({ status: rec.status, body: rec.body });
    }
    return route.fulfill({ status: rec.status, json: rec.body });
  });
  return unexpected;
}
