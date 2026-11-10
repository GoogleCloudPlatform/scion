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
 * Real-Chromium check that the markdown boundary (renderFragment) removes
 * disallowed markup. The unit tests (src/utils/markdown-sanitize.test.ts) cover
 * the same cases in the test DOM, but that DOM's parser does not reproduce
 * the namespace-confusion forms that only turn disallowed when a browser parses
 * serialized markup again. Here the browser's own parser runs every case,
 * before and after a serialize/parse round trip.
 */

import { test, expect } from '@playwright/test';
import type { CaseResult } from './markdown-sanitize-fixture.js';
import { DISALLOWED_CASES } from '../../src/utils/__fixtures__/disallowed-markup.js';

test('renderFragment removes disallowed markup in a real browser', async ({ page }) => {
  // Only the renderer is under test; the fixture page's API calls are not needed.
  await page.route('**/api/**', (route) => route.abort());
  await page.goto('/e2e/chat-file-preview/fixture.html');
  const results: CaseResult[] = await page.evaluate(async () => {
    // Resolved by the page, which Vite serves from the web root.
    const url = '/e2e/chat-file-preview/markdown-sanitize-fixture.ts';
    const mod = (await import(url)) as typeof import('./markdown-sanitize-fixture.js');
    return mod.runDisallowedCases();
  });

  expect(results.map((r) => r.name)).toEqual(DISALLOWED_CASES.map((c) => c.name));
  for (const r of results) {
    // Each case is disallowed as written (once parsed, or after a round trip).
    expect([...r.rawParsed, ...r.rawRoundTrip], `${r.name}: input`).not.toEqual([]);
    // Nothing disallowed is left, even if the nodes are serialized and parsed again.
    expect(r.rendered, `${r.name}: rendered`).toEqual([]);
    expect(r.renderedRoundTrip, `${r.name}: rendered, round trip`).toEqual([]);
  }
});
