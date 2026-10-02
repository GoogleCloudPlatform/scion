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
 * The swipe sequence between the rail, conversation and members panels
 * works both as a fast flick and as a slower drag past the distance
 * threshold, and a vertical touch-scroll inside any panel is never
 * mistaken for a horizontal swipe. Mobile-only: swipe and the panel track
 * don't apply on desktop, where all three panels are visible at once.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';
import { touchSwipe, touchScroll, assertNoHorizontalOverflow } from './helpers.js';

/** Swipe horizontally across the middle of the viewport. */
async function swipeAcross(
  page: Page,
  dx: number,
  { steps, ms }: { steps: number; ms: number }
): Promise<void> {
  const vp = page.viewportSize();
  const w = vp?.width ?? 375;
  const h = vp?.height ?? 812;
  const x1 = w / 2;
  const y = h / 2;
  await touchSwipe(page, x1, y, x1 + dx, y, steps, ms);
  // The panel-track transform transition must settle before the next step.
  await page.waitForTimeout(400);
}

async function runSequence(
  page: Page,
  { dx, steps, ms }: { dx: number; steps: number; ms: number }
): Promise<void> {
  await openChatRail(page);
  expect(await currentPanel(page)).toBe('left');

  await swipeAcross(page, -dx, { steps, ms }); // rail -> conversation
  expect(await currentPanel(page)).toBe('center');
  await assertNoHorizontalOverflow(page);

  await swipeAcross(page, -dx, { steps, ms }); // conversation -> members
  expect(await currentPanel(page)).toBe('right');
  await assertNoHorizontalOverflow(page);

  await swipeAcross(page, dx, { steps, ms }); // members -> conversation
  expect(await currentPanel(page)).toBe('center');
  await assertNoHorizontalOverflow(page);

  await swipeAcross(page, dx, { steps, ms }); // conversation -> rail
  expect(await currentPanel(page)).toBe('left');
  await assertNoHorizontalOverflow(page);
}

test.describe('the swipe sequence moves between rail, conversation and members', () => {
  test('a fast flick moves one panel per swipe', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'swipe is mobile-only');
    // chat.ts's swipe handler decides "fast" by the real elapsed time
    // between touchstart and touchend (Date.now()), which otherwise makes
    // this test depend on CDP round-trip and renderer latency rather than
    // on the gesture itself. setFixedTime freezes only Date.now()/new
    // Date() — real timers and CSS transitions keep running — so every
    // touchend below computes the same (zero) elapsed time regardless of
    // how long the real gesture actually took under load.
    await page.clock.setFixedTime(Date.now());
    // Short distance, few touchmove steps: fast flicks qualify by speed
    // (now controlled by the frozen clock above), not distance.
    await runSequence(page, { dx: 70, steps: 3, ms: 60 });
  });

  test('a slow drag past the distance threshold also moves one panel per swipe', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'swipe is mobile-only');
    // Long distance, long duration: slow drags qualify by distance alone.
    await runSequence(page, { dx: 150, steps: 8, ms: 700 });
  });
});

test('a vertical touch-scroll inside any panel never changes the active panel', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'the mobile panel track is mobile-only');

  const vp = page.viewportSize();
  const w = vp?.width ?? 375;
  const h = vp?.height ?? 812;

  // The rail: a mostly-vertical drag (the swipe handler's axis lock) must
  // be treated as scrolling the thread list, not as a horizontal swipe.
  await openChatRail(page);
  expect(await currentPanel(page)).toBe('left');
  await touchScroll(page, w / 2, h / 2, 0, -300);
  expect(await currentPanel(page)).toBe('left');
  await touchScroll(page, w / 2, h / 2, 0, 300);
  expect(await currentPanel(page)).toBe('left');

  // The conversation panel: the message list is the long vertical scroller
  // sitting right next to the swipe track, so this is the panel most at
  // risk of a false-positive swipe.
  await openGeneralThread(page);
  expect(await currentPanel(page)).toBe('center');
  await touchScroll(page, w / 2, h / 2, 0, -300);
  expect(await currentPanel(page)).toBe('center');
  await touchScroll(page, w / 2, h / 2, 0, 300);
  expect(await currentPanel(page)).toBe('center');

  // The members panel.
  await page.locator('.mobile-members').click();
  await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  await page.waitForTimeout(400);
  await touchScroll(page, w / 2, h / 2, 0, -300);
  expect(await currentPanel(page)).toBe('right');
  await touchScroll(page, w / 2, h / 2, 0, 300);
  expect(await currentPanel(page)).toBe('right');
});
