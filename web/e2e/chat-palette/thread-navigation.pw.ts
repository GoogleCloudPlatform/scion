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
 * Real-Chromium coverage for cross-project thread selection: selecting
 * another-space thread updates route/context/title/members and the mobile
 * center view without recreating the page. Escape makes no navigation.
 * Back/forward retains correct route meaning.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  setupApiMocks,
  SPACE_ALPHA,
  THREAD_ALPHA,
  SPACE_BETA,
  THREAD_BETA,
  SPACE_MEMBERS,
} from './mock-api.js';

const TWO_SPACE_FIXTURE = {
  spaces: [SPACE_ALPHA, SPACE_BETA],
  threadsByProjectId: {
    [SPACE_ALPHA.projectId]: [THREAD_ALPHA],
    [SPACE_BETA.projectId]: [THREAD_BETA],
  },
  membersByProjectId: SPACE_MEMBERS,
};

async function gotoChatOnAlphaThread(page: Page) {
  await setupApiMocks(page, TWO_SPACE_FIXTURE);
  const route = `/chat/${SPACE_ALPHA.projectSlug}/${THREAD_ALPHA.id}`;
  await page.goto(`/e2e/chat-palette/fixture.html?route=${encodeURIComponent(route)}`, {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  // Record page-title events (bubbling/composed from dispatchPageTitle) —
  // nothing in this isolated fixture otherwise consumes them.
  await page.evaluate(() => {
    (window as unknown as { __titleSegments: string[] }).__titleSegments = [];
    document.addEventListener('scion:page-title', (e) => {
      (window as unknown as { __titleSegments: string[] }).__titleSegments = (
        e as CustomEvent<{ segments: string[] }>
      ).detail.segments;
    });
  });
  // Mark the mounted page element so a later check can prove the exact same
  // DOM node is still there (no navigateTo-driven recreation).
  await page.evaluate(() => {
    (document.querySelector('scion-page-chat') as unknown as { __e2eMarker: string }).__e2eMarker =
      'still-the-same-element';
  });
}

test('selecting a cross-project thread updates route/title/members without recreating the page', async ({
  page,
}) => {
  await gotoChatOnAlphaThread(page);
  await expect(page).toHaveURL(new RegExp(`/chat/${SPACE_ALPHA.projectSlug}/${THREAD_ALPHA.id}$`));

  await page.keyboard.press('Control+k');
  await page.locator('scion-quick-palette #palette-query-input').fill(THREAD_BETA.name);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');

  await expect(page).toHaveURL(new RegExp(`/chat/${SPACE_BETA.projectSlug}/${THREAD_BETA.id}$`));

  // Context: the page's own in-memory conversation state switched projects.
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            document.querySelector('scion-page-chat') as unknown as {
              v2Conversation?: { projectId?: string };
            }
          ).v2Conversation?.projectId
      )
    )
    .toBe(SPACE_BETA.projectId);

  // Title: dispatchPageTitle fired with the new thread's name.
  await expect
    .poll(() =>
      page.evaluate(() =>
        (window as unknown as { __titleSegments: string[] }).__titleSegments.join(' ')
      )
    )
    .toContain(THREAD_BETA.name);

  // Members: Beta's own member ("Beta Member"), not Alpha's, now shows in
  // the members panel — proving loadV2Members actually re-ran for the new
  // project rather than keeping Alpha's roster.
  await expect(page.locator('scion-page-chat').getByText('Beta Member')).toBeVisible();

  // No page recreation: the exact same scion-page-chat DOM node is present.
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (document.querySelector('scion-page-chat') as unknown as { __e2eMarker?: string })
            .__e2eMarker
      )
    )
    .toBe('still-the-same-element');
});

test('Escape makes no navigation: the URL and conversation are unchanged', async ({ page }) => {
  await gotoChatOnAlphaThread(page);
  const before = page.url();

  await page.keyboard.press('Control+k');
  await page.locator('scion-quick-palette #palette-query-input').fill(THREAD_BETA.name);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Escape');

  await expect(page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')).toBeHidden();
  expect(page.url()).toBe(before);
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            document.querySelector('scion-page-chat') as unknown as {
              v2Conversation?: { projectId?: string };
            }
          ).v2Conversation?.projectId
      )
    )
    .toBe(SPACE_ALPHA.projectId);
});

test('back/forward through browser history retains the correct URL for each conversation', async ({
  page,
}) => {
  // In-page thread selection uses history.pushState directly (chat.ts:
  // navigateToThread), which creates real browser history entries even
  // though this isolated fixture's stubbed main.ts has no popstate-driven
  // router of its own to re-render on back/forward (that re-render is owned
  // by the existing page route machinery, not this feature's own code). What
  // this feature must get right is that pushState always writes the
  // *correct* URL for each selection — this proves the browser's own
  // history stack (not app re-render) has the right sequence of real,
  // distinct URLs to hand back.
  await gotoChatOnAlphaThread(page);
  const alphaUrl = page.url();

  await page.keyboard.press('Control+k');
  await page.locator('scion-quick-palette #palette-query-input').fill(THREAD_BETA.name);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');
  const betaUrl = page.url();
  expect(betaUrl).not.toBe(alphaUrl);
  expect(betaUrl).toContain(THREAD_BETA.id);

  await page.goBack();
  await expect.poll(() => page.url()).toBe(alphaUrl);

  await page.goForward();
  await expect.poll(() => page.url()).toBe(betaUrl);
});

test('at a narrow (mobile) viewport, selecting a thread sets the mobile panel to center', async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await gotoChatOnAlphaThread(page);
  // handleThreadSelect/navigateToThread's own cold-load parse already set
  // mobilePanel to 'center' for the starting thread — reset it to 'left' (the
  // rail) so the assertion below actually proves the *palette selection*
  // path sets it, not just that it was already 'center' from page load.
  await page.evaluate(() => {
    (document.querySelector('scion-page-chat') as unknown as { mobilePanel: string }).mobilePanel =
      'left';
  });

  await page.keyboard.press('Control+k');
  await page.locator('scion-quick-palette #palette-query-input').fill(THREAD_BETA.name);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');

  await expect(page).toHaveURL(new RegExp(`/chat/${SPACE_BETA.projectSlug}/${THREAD_BETA.id}$`));
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (document.querySelector('scion-page-chat') as unknown as { mobilePanel: string })
            .mobilePanel
      )
    )
    .toBe('center');
});
