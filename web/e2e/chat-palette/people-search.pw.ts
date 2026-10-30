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
 * Chromium: the People group lists the recent-users page on open, and a
 * person outside that page is found by typing a search and opened with
 * Enter.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks, SELF_USER_ID, USER_WITH_DM, USER_WITHOUT_DM } from './mock-api.js';

function peopleOptions(page: Page) {
  return page.locator(
    'scion-quick-palette [aria-labelledby="palette-heading-people"] .palette-option'
  );
}

test('a person outside the recent-users page is found by search and Enter opens the DM', async ({
  page,
}) => {
  const requests = await setupApiMocks(page, {
    users: [USER_WITH_DM, USER_WITHOUT_DM],
    recentUsers: 1,
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  const input = page.locator('scion-quick-palette #palette-query-input');
  await expect(input).toBeFocused();
  await expect(peopleOptions(page)).toHaveCount(1);
  await expect(peopleOptions(page)).toContainText(USER_WITH_DM.displayName);
  await expect(peopleOptions(page)).not.toContainText(USER_WITHOUT_DM.displayName);

  await input.fill(USER_WITHOUT_DM.displayName);
  await expect(peopleOptions(page)).toHaveCount(1);
  await expect(peopleOptions(page)).toContainText(USER_WITHOUT_DM.displayName);
  const searched = requests.map((r) => new URL(r.url).searchParams.get('search'));
  expect(searched).toContain(USER_WITHOUT_DM.displayName);

  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:user:${SELF_USER_ID}:user:${USER_WITHOUT_DM.id}`)}$`
    )
  );
});
