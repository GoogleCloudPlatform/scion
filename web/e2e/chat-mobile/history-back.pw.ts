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
 * A horizontal drag on the chat panels must never turn into browser history
 * navigation.
 *
 * Chromium hands a horizontal touch overscroll that nothing consumed to its
 * overscroll history navigation (swipe-to-go-back). With a history entry
 * behind the chat page, a rightward drag across the rail then pops back to
 * that entry, and the router rebuilds the whole chat page — tearing down
 * whatever was open, with the new page also landing on the rail, so the
 * drag looks like a swipe that worked. These tests tag the page element
 * and watch `popstate`, so they tell the two apart.
 *
 * Mobile-only: the panel track and its swipe handler apply only there.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, expandSpace, openGeneralThread, currentPanel } from './fixture.js';
import { touchSwipe, assertNoHorizontalOverflow } from './helpers.js';

/**
 * Tag the current chat page element and count `popstate` events from now
 * on. `assertSamePage` then checks that no history navigation happened and
 * that the tagged element is still the one in the document.
 */
async function watchForNavigation(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __popstates: string[] };
    w.__popstates = [];
    window.addEventListener('popstate', () => w.__popstates.push(location.pathname));
    document.querySelector('scion-page-chat')?.setAttribute('data-history-probe', '');
  });
}

async function assertSamePage(page: Page, context: string): Promise<void> {
  const state = await page.evaluate(() => ({
    popstates: (window as unknown as { __popstates: string[] }).__popstates,
    tagged: document.querySelectorAll('scion-page-chat[data-history-probe]').length,
    pages: document.querySelectorAll('scion-page-chat').length,
  }));
  expect(state.popstates, `${context}: no history navigation (popstate)`).toEqual([]);
  expect(state.tagged, `${context}: the chat page element survived`).toBe(1);
  expect(state.pages, `${context}: exactly one chat page`).toBe(1);
}

/** Settle time for the panel-track transform transition. */
const SETTLE_MS = 400;

test.describe('horizontal drags never navigate browser history', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'the panel track is mobile-only');
    test.skip(testInfo.project.name.startsWith('webkit'), 'CDP touch is Chromium-only');
  });

  test('a rightward drag across the rail leaves the page and the panel alone', async ({ page }) => {
    await openChatRail(page);
    await expandSpace(page);
    // The redirect from the legacy space URL left an entry to go back to.
    expect(await page.evaluate(() => history.length)).toBeGreaterThan(1);
    await watchForNavigation(page);

    const vp = page.viewportSize();
    const w = vp?.width ?? 375;
    const h = vp?.height ?? 812;
    const url = page.url();
    // The original reproduction: start near the left edge, drag most of the
    // way across, in a few quick steps.
    await touchSwipe(page, 40, h * 0.6, w - 55, h * 0.6);
    await page.waitForTimeout(SETTLE_MS);
    await assertSamePage(page, 'drag from near the left edge');

    // And a slower drag from the middle, the size of a deliberate swipe.
    await touchSwipe(page, w / 2, h / 2, w - 10, h / 2, 8, 600);
    await page.waitForTimeout(SETTLE_MS);
    await assertSamePage(page, 'slow drag from the middle');

    expect(page.url(), 'the URL is unchanged').toBe(url);
    expect(await currentPanel(page)).toBe('left');
    await assertNoHorizontalOverflow(page);
  });

  test('swiping from the conversation back to the rail is the app swipe, not history back', async ({
    page,
  }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    expect(await currentPanel(page)).toBe('center');
    await watchForNavigation(page);
    const url = page.url();

    const vp = page.viewportSize();
    const w = vp?.width ?? 375;
    const h = vp?.height ?? 812;
    await touchSwipe(page, w * 0.3, h / 2, w * 0.3 + 150, h / 2, 8, 300);
    await page.waitForTimeout(SETTLE_MS);

    await assertSamePage(page, 'conversation -> rail');
    expect(await currentPanel(page)).toBe('left');
    expect(page.url(), 'the URL is unchanged').toBe(url);
    await assertNoHorizontalOverflow(page);

    // Leftward swipes still move forward through the panels on the same page.
    await touchSwipe(page, w * 0.7, h / 2, w * 0.7 - 150, h / 2, 8, 300);
    await page.waitForTimeout(SETTLE_MS);
    expect(await currentPanel(page)).toBe('center');
    await assertSamePage(page, 'rail -> conversation');
  });

  test('a rightward drag on the header does not navigate either', async ({ page }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    await watchForNavigation(page);
    const w = page.viewportSize()?.width ?? 375;
    // The header row sits above the panels, at the top of the frame.
    await touchSwipe(page, 40, 30, w - 55, 30);
    await page.waitForTimeout(SETTLE_MS);
    await assertSamePage(page, 'drag on the header');
  });

  test('a wide code block inside a message still pans sideways', async ({ page }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    // The mock messages have no code wide enough to scroll, so add a fenced
    // block with one long line to a message on screen. It sits in the
    // message's own markdown container and takes its real code-block styles.
    const box = await page.evaluate(() => {
      const messages: Element[] = [];
      const collect = (root: ParentNode): void => {
        for (const el of root.querySelectorAll('*')) {
          if (el.tagName.toLowerCase() === 'scion-chat-message') messages.push(el);
          if (el.shadowRoot) collect(el.shadowRoot);
        }
      };
      collect(document);
      const md = messages
        .map((m) => m.shadowRoot?.querySelector('.md-content'))
        .find((el) => {
          if (!el) return false;
          const r = el.getBoundingClientRect();
          return r.width > 0 && r.top > 80 && r.top < window.innerHeight / 2;
        });
      if (!md) return null;
      const pre = document.createElement('pre');
      const code = document.createElement('code');
      code.textContent = `const wide = '${'x'.repeat(400)}';`;
      pre.appendChild(code);
      md.prepend(pre);
      pre.scrollIntoView({ block: 'center' });
      (window as unknown as { __wide: HTMLElement }).__wide = pre;
      const r = pre.getBoundingClientRect();
      return {
        left: r.left,
        top: r.top,
        width: r.width,
        height: r.height,
        overflowX: getComputedStyle(pre).overflowX,
        scrollable: pre.scrollWidth > pre.clientWidth,
      };
    });
    expect(box, 'the code block was placed').not.toBeNull();
    expect(box!.overflowX, 'the code block is its own sideways scroller').toBe('auto');
    expect(box!.scrollable, 'the long line overflows the code block').toBe(true);
    await watchForNavigation(page);
    const y = box!.top + box!.height / 2;
    // Drag leftward across the block, then back rightward.
    await touchSwipe(page, box!.left + box!.width - 20, y, box!.left + 40, y, 8, 400);
    await page.waitForTimeout(SETTLE_MS);
    const scrolled = await page.evaluate(
      () => (window as unknown as { __wide: HTMLElement }).__wide.scrollLeft
    );
    expect(scrolled, 'the code block panned sideways').toBeGreaterThan(100);
    await assertSamePage(page, 'leftward drag on a code block');

    // Back the other way, then once more with the block already at its
    // start: nothing is left to scroll, and that drag must not fall through
    // to history navigation either.
    for (let i = 0; i < 2; i++) {
      await touchSwipe(page, box!.left + 40, y, box!.left + box!.width - 20, y, 8, 400);
      await page.waitForTimeout(SETTLE_MS);
    }
    expect(
      await page.evaluate(() => (window as unknown as { __wide: HTMLElement }).__wide.scrollLeft),
      'the code block is back at its start'
    ).toBe(0);
    await assertSamePage(page, 'rightward drag on a code block at its start');
  });

  test('the panels and their scrollers keep pinch-zoom and vertical panning', async ({ page }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    const actions = await page.evaluate(() => {
      const find = (root: ParentNode, selector: string): Element | null => {
        const hit = root.querySelector(selector);
        if (hit) return hit;
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) {
            const nested = find(el.shadowRoot, selector);
            if (nested) return nested;
          }
        }
        return null;
      };
      const out: Record<string, string> = {};
      for (const selector of [
        'scion-chat-shell',
        '.v2-panels .v2-rail',
        '.v2-panels .v2-content',
        '.v2-panels .v2-members',
        '.rail-body',
        '.messages-scroll',
        '.members-body',
      ]) {
        const el = find(document, selector);
        out[selector] = el ? getComputedStyle(el).touchAction : 'missing';
      }
      return out;
    });
    for (const [selector, value] of Object.entries(actions)) {
      expect(value, `${selector} touch-action`).toBe('pan-y pinch-zoom');
    }
  });

  test('a rightward drag on the members panel goes back to the conversation only', async ({
    page,
  }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    await page.locator('.mobile-members').click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
    await page.waitForTimeout(SETTLE_MS);
    await watchForNavigation(page);

    const vp = page.viewportSize();
    const w = vp?.width ?? 375;
    const h = vp?.height ?? 812;
    await touchSwipe(page, 40, h * 0.6, w - 55, h * 0.6);
    await page.waitForTimeout(SETTLE_MS);

    await assertSamePage(page, 'members -> conversation');
    expect(await currentPanel(page)).toBe('center');
  });
});
