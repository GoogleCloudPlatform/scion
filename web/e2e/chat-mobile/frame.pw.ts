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
 * Each mobile panel stays within the viewport and the document frame stays
 * pinned (no horizontal or vertical document scroll), on every panel, with
 * the expected computed `overscroll-behavior` values, and on desktop with a
 * mouse wheel too. Also confirms the frame-mode class is present only on
 * shell routes, and that a stray oversized element in the document can't
 * make the page scroll while frame mode is engaged.
 */

import { test, expect } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { assertNoHorizontalOverflow, assertFramePinned, addStrayTallElement } from './helpers.js';

test.describe('every panel stays framed and non-overflowing', () => {
  test('@static the rail panel on load', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'panel concept is mobile-only');
    await openChatRail(page);
    await assertNoHorizontalOverflow(page);
    await assertFramePinned(page);
  });

  test('@static the conversation panel', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'panel concept is mobile-only');
    await openChatRail(page);
    await openGeneralThread(page);
    await assertNoHorizontalOverflow(page);
    await assertFramePinned(page);
  });

  test('@static the members panel', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'panel concept is mobile-only');
    await openChatRail(page);
    await openGeneralThread(page);
    await page.locator('.mobile-members').click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
    await page.waitForTimeout(400); // let the panel transform transition settle
    await assertNoHorizontalOverflow(page);
    await assertFramePinned(page);
  });
});

test('@static overscroll-behavior is none on the root and contain on inner scrollers', async ({
  page,
}) => {
  await openChatRail(page);
  await openGeneralThread(page);

  const values = await page.evaluate(() => {
    const read = (el: Element | null | undefined): string | null =>
      el
        ? getComputedStyle(el).overscrollBehaviorY || getComputedStyle(el).overscrollBehavior
        : null;
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const panels = pageEl?.shadowRoot;
    const rail = panels?.querySelector('scion-chat-space-rail') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const railBody = rail?.shadowRoot?.querySelector('.rail-body');
    const thread = panels?.querySelector('scion-chat-thread') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const messagesScroll = thread?.shadowRoot?.querySelector('.messages-scroll');
    const members = panels?.querySelector('scion-chat-members') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const membersBody = members?.shadowRoot?.querySelector('.members-body');
    return {
      html: read(document.documentElement),
      body: read(document.body),
      railBody: read(railBody),
      messagesScroll: read(messagesScroll),
      membersBody: read(membersBody),
    };
  });

  expect(values.html).toBe('none');
  expect(values.body).toBe('none');
  expect(values.railBody).toBe('contain');
  expect(values.messagesScroll).toBe('contain');
  expect(values.membersBody).toBe('contain');
});

test.describe('desktop never scrolls the document', () => {
  test('wheel-scrolling the rail and the message list stays pinned', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop-1440', 'desktop-only');
    await openChatRail(page);
    await openGeneralThread(page);

    const rail = page.locator('.rail-body');
    await rail.hover();
    await page.mouse.wheel(0, 5000);
    await assertFramePinned(page);
    await page.mouse.wheel(0, -5000);
    await assertFramePinned(page);

    const messages = page.locator('.messages-scroll');
    await messages.hover();
    await page.mouse.wheel(0, 5000);
    await assertFramePinned(page);
    await page.mouse.wheel(0, -5000);
    await assertFramePinned(page);
  });

  test('a stray oversized element appended to the document cannot scroll the page', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop-1440', 'desktop-only');
    await openChatRail(page);
    const removeStray = await addStrayTallElement(page);
    try {
      // A direct scrollTo is deterministic regardless of which element a
      // wheel event happens to land on: with the stray element making the
      // document genuinely taller than the viewport, this only stays at 0
      // if something (frame mode's `overflow: hidden` on <html>) actually
      // stops the document from scrolling at all.
      const scrollY = await page.evaluate(() => {
        window.scrollTo(0, 2000);
        return window.scrollY;
      });
      expect(scrollY, 'a stray 3000px element must not make the document scroll').toBe(0);
    } finally {
      await removeStray();
    }
  });

  for (const route of ['/', '/projects', '/agents']) {
    test(`the document never scrolls on ${route}`, async ({ page }, testInfo) => {
      test.skip(testInfo.project.name !== 'desktop-1440', 'desktop-only');
      await openChatRail(page); // installs the mocks this route's shell also needs
      await page.goto(route, { waitUntil: 'domcontentloaded' });
      // domcontentloaded fires before the SPA router mounts the shell —
      // wait for it, or assertFramePinned's shell/header checks fail before
      // the app has had a chance to render at all.
      await expect(page.locator('scion-header')).toBeVisible({ timeout: 10_000 });
      await assertFramePinned(page);
    });
  }
});

test('@static the frame-mode class is not set on a document-scrolling route', async ({ page }) => {
  await openChatRail(page); // installs the mocks every route needs
  await page.goto('/login', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('h1')).toHaveText('Sign in', { timeout: 10_000 });
  const hasFrameClass = await page.evaluate(() =>
    document.documentElement.classList.contains('scion-app-frame')
  );
  expect(hasFrameClass, 'a document-scrolling page must never set the frame-mode class').toBe(
    false
  );
});
