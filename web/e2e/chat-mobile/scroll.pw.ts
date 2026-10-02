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
 * Touch-scrolling the rail list and the message list to both ends
 * (including overscroll past each end, and a drag that starts on
 * non-scrolling chrome such as the rail header, the composer or the
 * thread header) must never move the document — the header-pinned,
 * no-dark-band guarantee. Mobile-only: desktop has no touch input.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, expandSpace } from './fixture.js';
import { touchScroll, assertFramePinned, addStrayTallElement } from './helpers.js';

interface ScrollState {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/** Stroke distance to reach the top from wherever the scroller is now. */
function distanceToTop(state: ScrollState, margin = 500): number {
  return state.scrollTop + margin;
}

/** Stroke distance to reach the bottom from wherever the scroller is now. */
function distanceToBottom(state: ScrollState, margin = 500): number {
  return state.scrollHeight - state.clientHeight - state.scrollTop + margin;
}

async function railBodyScrollState(page: Page): Promise<ScrollState | null> {
  return page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const rail = pageEl?.shadowRoot?.querySelector('scion-chat-space-rail') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const body = rail?.shadowRoot?.querySelector('.rail-body') as HTMLElement | null;
    if (!body) return null;
    return {
      scrollTop: body.scrollTop,
      scrollHeight: body.scrollHeight,
      clientHeight: body.clientHeight,
    };
  });
}

/**
 * Touch-scroll up until the scroller's `scrollTop` is exactly 0. A single
 * very long stroke is split into many CDP sub-strokes internally, and can
 * lose more than a handful of pixels to touch-event rounding over enough of
 * them, or undershoot if the scroller's content reflows mid-gesture (e.g.
 * async code-block highlighting changing scrollHeight) — a short corrective
 * stroke, sized from a freshly re-read state rather than the original
 * estimate, closes either gap without needing a much larger (and much
 * slower) initial overshoot.
 */
async function scrollToExactTop(
  page: Page,
  x: number,
  y: number,
  readState: () => Promise<ScrollState | null>
): Promise<ScrollState> {
  let state = await readState();
  for (let attempt = 0; attempt < 3 && state && state.scrollTop > 0; attempt++) {
    await touchScroll(page, x, y, 0, state.scrollTop + 200);
    state = await readState();
  }
  if (!state) throw new Error('scrollToExactTop: scroller disappeared');
  return state;
}

/** How far a scroller still is from its bottom edge. */
function remainingToBottom(state: ScrollState): number {
  return state.scrollHeight - state.clientHeight - state.scrollTop;
}

/**
 * Touch-scroll down until the scroller's bottom edge (`scrollTop +
 * clientHeight`) is within 1px of `scrollHeight`. The mirror image of
 * scrollToExactTop, for the same reason: a corrective stroke sized from a
 * fresh read closes a touch-rounding or mid-gesture-reflow gap that a
 * single long stroke sized from a stale estimate can leave short.
 */
async function scrollToExactBottom(
  page: Page,
  x: number,
  y: number,
  readState: () => Promise<ScrollState | null>
): Promise<ScrollState> {
  let state = await readState();
  for (let attempt = 0; attempt < 3 && state && remainingToBottom(state) > 1; attempt++) {
    await touchScroll(page, x, y, 0, -(remainingToBottom(state) + 200));
    state = await readState();
  }
  if (!state) throw new Error('scrollToExactBottom: scroller disappeared');
  return state;
}

async function messagesScrollState(page: Page): Promise<ScrollState | null> {
  return page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const thread = pageEl?.shadowRoot?.querySelector('scion-chat-thread') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const scroller = thread?.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!scroller) return null;
    return {
      scrollTop: scroller.scrollTop,
      scrollHeight: scroller.scrollHeight,
      clientHeight: scroller.clientHeight,
    };
  });
}

test('touch-scrolling the rail list (45 threads) reaches both ends and keeps the frame pinned', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch scrolling is mobile-only');
  // Many real CDP touch round trips at the narrowest width (tallest wrapped
  // content, so the longest scroll distance) can run past the suite's
  // default per-test timeout.
  test.setTimeout(120_000);
  await openChatRail(page);
  await expandSpace(page);

  const vp = page.viewportSize()!;
  const x = vp.width / 2;
  const y = vp.height / 2;

  // The rail opens scrolled to the top. Confirm that explicitly, then
  // derive every stroke distance from the scroller's current position
  // (never a fixed guess, and never an assumed starting point) so the
  // stroke is always long enough to actually reach the far end, regardless
  // of how tall the list renders at this width.
  const initial = await railBodyScrollState(page);
  expect(initial, 'the rail scroller was found').not.toBeNull();
  expect(initial!.scrollTop, 'the rail list opens scrolled to the top').toBe(0);

  // Scroll to the bottom of the list.
  await touchScroll(page, x, y, 0, -distanceToBottom(initial!));
  await assertFramePinned(page);
  const atBottom = await scrollToExactBottom(page, x, y, () => railBodyScrollState(page));
  expect(
    atBottom.scrollTop + atBottom.clientHeight,
    'the rail list reached its bottom'
  ).toBeGreaterThanOrEqual(atBottom.scrollHeight - 1);

  // Overscroll past the bottom.
  await touchScroll(page, x, y, 0, -600);
  await assertFramePinned(page);

  // Back up to the top, then overscroll past it.
  await touchScroll(page, x, y, 0, distanceToTop(atBottom));
  await assertFramePinned(page);
  const atTop = await scrollToExactTop(page, x, y, () => railBodyScrollState(page));
  expect(atTop.scrollTop, 'the rail list reached its top').toBe(0);
  await touchScroll(page, x, y, 0, 600);
  await assertFramePinned(page);

  // A drag starting on the fixed rail header (not a scroller) must not move
  // the frame either — including with a stray oversized element present,
  // which is the root-rubber-band scenario this guards against.
  const removeStray = await addStrayTallElement(page);
  try {
    const header = page.locator('.rail-header');
    const headerBox = await header.boundingBox();
    expect(headerBox, 'the rail header was found').not.toBeNull();
    await touchScroll(
      page,
      headerBox!.x + headerBox!.width / 2,
      headerBox!.y + headerBox!.height / 2,
      0,
      300
    );
    await assertFramePinned(page);
  } finally {
    await removeStray();
  }
});

test('touch-scrolling the message list reaches both ends and keeps the frame pinned', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch scrolling is mobile-only');
  // Needs more than the rail-list test's budget above: this thread's long
  // prose and code block make it the tallest content in the fixtures, and
  // the narrowest width (chromium-320) wraps that content into the most
  // scrollable distance, so the two full-length touch-scroll round trips
  // (to the top, then back to the bottom) ran close to a 120s budget even
  // before accounting for normal CI/CDP latency variance.
  test.setTimeout(180_000);
  await openChatRail(page);
  await openGeneralThread(page);

  const vp = page.viewportSize()!;
  const x = vp.width / 2;
  const y = vp.height / 2;

  // Unlike the rail list above, the thread opens scrolled to the *bottom*
  // (the most recent message), not the top. Confirm that explicitly, then
  // scroll to the top first — every later stroke distance is derived from
  // the scroller's current position rather than assumed, so "reached the
  // bottom" after the later down-scroll is a real measurement and not
  // something that was already true before the stroke ran. Long prose and
  // a code block can also make this thread taller at narrower widths than
  // a fixed guess would anticipate, which the current-position derivation
  // handles the same way.
  const initial = await messagesScrollState(page);
  expect(initial, 'the message scroller was found').not.toBeNull();
  expect(
    initial!.scrollTop + initial!.clientHeight,
    'the thread opens scrolled to the bottom'
  ).toBeGreaterThanOrEqual(initial!.scrollHeight - 1);

  // Scroll to the top of the thread.
  await touchScroll(page, x, y, 0, distanceToTop(initial!));
  await assertFramePinned(page);
  const atTop = await scrollToExactTop(page, x, y, () => messagesScrollState(page));
  expect(atTop.scrollTop, 'the message list reached its top').toBe(0);

  // Overscroll past the top.
  await touchScroll(page, x, y, 0, 600);
  await assertFramePinned(page);

  // Back down to the bottom — a real measurement now, since the scroller
  // actually moved away from it above.
  await touchScroll(page, x, y, 0, -distanceToBottom(atTop));
  await assertFramePinned(page);
  const atBottom = await scrollToExactBottom(page, x, y, () => messagesScrollState(page));
  expect(
    atBottom.scrollTop + atBottom.clientHeight,
    'the message list reached its bottom'
  ).toBeGreaterThanOrEqual(atBottom.scrollHeight - 1);

  // Overscroll past the bottom.
  await touchScroll(page, x, y, 0, -600);
  await assertFramePinned(page);

  // A drag starting on the composer (not a scroller). The composer sits at
  // the bottom of the viewport, so dragging downward from its centre would
  // leave almost no room before the viewport edge — not enough for a single
  // stroke to clear touchScroll's minimum, and not a meaningful drag at any
  // width. Dragging upward instead starts from the same non-scrolling
  // chrome but has the whole upper half of the viewport to work with.
  const composer = page.locator('sl-textarea').first();
  const composerBox = await composer.boundingBox();
  expect(composerBox, 'the composer textarea was found').not.toBeNull();
  await touchScroll(
    page,
    composerBox!.x + composerBox!.width / 2,
    composerBox!.y + composerBox!.height / 2,
    0,
    -300
  );
  await assertFramePinned(page);

  // A drag starting on the thread header (not a scroller).
  const threadHeader = page.locator('.v2-thread-header');
  const threadHeaderBox = await threadHeader.boundingBox();
  expect(threadHeaderBox, 'the thread header was found').not.toBeNull();
  await touchScroll(
    page,
    threadHeaderBox!.x + threadHeaderBox!.width / 2,
    threadHeaderBox!.y + threadHeaderBox!.height / 2,
    0,
    300
  );
  await assertFramePinned(page);
});
