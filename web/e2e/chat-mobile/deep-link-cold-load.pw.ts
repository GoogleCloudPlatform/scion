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
 * A cold load of a readable thread URL opens that thread from the projects
 * lookup by slug alone. The space rail's own list is held for the whole
 * test, so the rail cannot supply the slug.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupChatMobileMocks, PROJECT_A } from './mock-api.js';

const THREAD_ID = 'thread-05';

/** The thread component's conversation key. */
function openKey(page: Page): Promise<string> {
  return page.evaluate(
    () =>
      (
        document
          .querySelector('scion-page-chat')
          ?.shadowRoot?.querySelector('scion-chat-thread') as
          | (HTMLElement & { conversationKey: string })
          | null
      )?.conversationKey ?? ''
  );
}

test('a cold load of /chat/{slug}/{thread} opens that thread before the rail loads', async ({
  page,
}) => {
  await setupChatMobileMocks(page);

  let releaseSpaces: () => void = () => {};
  const spacesHeld = new Promise<void>((resolve) => (releaseSpaces = resolve));
  await page.route(/\/api\/v1\/chat\/spaces$/, async (route) => {
    await spacesHeld;
    await route.fulfill({ json: { spaces: [] } });
  });

  const slugQueries: string[] = [];
  await page.route(/\/api\/v1\/projects(\?|$)/, (route) => {
    const slug = new URL(route.request().url()).searchParams.get('slug');
    if (slug !== null) slugQueries.push(slug);
    const projects = slug === PROJECT_A.slug ? [PROJECT_A] : [];
    void route.fulfill({ json: { projects, totalCount: projects.length } });
  });

  try {
    await page.goto(`/chat/${PROJECT_A.slug}/${THREAD_ID}`, { waitUntil: 'domcontentloaded' });

    await expect.poll(() => openKey(page), { timeout: 15_000 }).toBe(THREAD_ID);
    await expect(page.getByText(`(#10)`).first()).toBeVisible({ timeout: 10_000 });
    await expect(page).toHaveURL(`/chat/${PROJECT_A.slug}/${THREAD_ID}`);
    expect(slugQueries).toContain(PROJECT_A.slug);
  } finally {
    releaseSpaces();
  }
});
