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
 * Sideways scrollers inside a message own the sideways drag.
 *
 * A wide code block or table in a message scrolls sideways. A drag across
 * it that it can still absorb must scroll it and leave the panels alone;
 * once it is at its end in the drag direction, the drag is a panel swipe
 * again. Wide markdown tables get their own scroller, so their columns
 * keep a readable width and the page does not widen.
 *
 * Mobile-only: the panel swipe applies only there.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';
import { touchSwipe, touchPinch, assertNoHorizontalOverflow } from './helpers.js';

const SETTLE_MS = 400;

const WIDE_CODE = '```\nconst wide = "' + 'x'.repeat(400) + '";\n```';

const WIDE_TABLE = [
  '| Agent | Project | Phase | Activity | Harness | Template | Broker | Started |',
  '|---|---|---|---|---|---|---|---|',
  ...Array.from(
    { length: 3 },
    (_, i) =>
      `| coder-${i} | mobile-layout | running | thinking about it | claude | default | broker-${i} | 2026-09-0${i + 1} |`
  ),
].join('\n');

interface Scroller {
  left: number;
  top: number;
  width: number;
  height: number;
  overflowX: string;
  max: number;
}

/**
 * Give a message on screen the markdown `body`, and return the box of the
 * sideways scroller matching `selector` in its rendered content. The
 * element is kept as `window.__scroller`.
 */
async function wideMessage(page: Page, body: string, selector: string): Promise<Scroller> {
  await page.evaluate((text) => {
    const messages: Element[] = [];
    const collect = (root: ParentNode): void => {
      for (const el of root.querySelectorAll('*')) {
        if (el.tagName.toLowerCase() === 'scion-chat-message') messages.push(el);
        if (el.shadowRoot) collect(el.shadowRoot);
      }
    };
    collect(document);
    const msg = messages.find((m) => {
      const md = m.shadowRoot?.querySelector('.md-content');
      if (!md) return false;
      const r = md.getBoundingClientRect();
      return r.width > 0 && r.top > 80 && r.top < window.innerHeight / 2;
    });
    if (!msg) throw new Error('no message on screen');
    (msg as unknown as { body: string }).body = text;
    (window as unknown as { __message: Element }).__message = msg;
  }, body);
  await page.waitForFunction(
    (sel) =>
      !!(window as unknown as { __message: Element }).__message.shadowRoot?.querySelector(
        `.md-content ${sel}`
      ),
    selector
  );
  return page.evaluate((sel) => {
    const msg = (window as unknown as { __message: Element }).__message;
    const el = msg.shadowRoot!.querySelector(`.md-content ${sel}`) as HTMLElement;
    el.scrollIntoView({ block: 'center' });
    (window as unknown as { __scroller: HTMLElement }).__scroller = el;
    const r = el.getBoundingClientRect();
    return {
      left: r.left,
      top: r.top,
      width: r.width,
      height: r.height,
      overflowX: getComputedStyle(el).overflowX,
      max: el.scrollWidth - el.clientWidth,
    };
  }, selector);
}

const scrollLeft = (page: Page): Promise<number> =>
  page.evaluate(() => (window as unknown as { __scroller: HTMLElement }).__scroller.scrollLeft);

const setScrollLeft = (page: Page, left: number): Promise<void> =>
  page.evaluate((x) => {
    (window as unknown as { __scroller: HTMLElement }).__scroller.scrollLeft = x;
  }, left);

interface PinchMoves {
  /** Touchmoves with one finger down, and how many the page cancelled. */
  soloMoves: number;
  soloCancelled: number;
  /** Touchmoves with two fingers down, and how many the page cancelled. */
  moves: number;
  cancelled: number;
  pops: number;
}

/**
 * Count the touchmoves that reach the window, split by how many fingers are
 * down, and how many of them the page cancelled. Popstates are recorded too.
 */
async function watchPinchMoves(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __pinch: PinchMoves };
    w.__pinch = { soloMoves: 0, soloCancelled: 0, moves: 0, cancelled: 0, pops: 0 };
    window.addEventListener(
      'touchmove',
      (e) => {
        if (e.touches.length === 1) {
          w.__pinch.soloMoves++;
          if (e.defaultPrevented) w.__pinch.soloCancelled++;
        } else {
          w.__pinch.moves++;
          if (e.defaultPrevented) w.__pinch.cancelled++;
        }
      },
      { passive: true }
    );
    window.addEventListener('popstate', () => w.__pinch.pops++);
  });
}

/** The counts since the last call, which resets them. */
const takePinchMoves = (page: Page): Promise<PinchMoves> =>
  page.evaluate(() => {
    const w = window as unknown as { __pinch: PinchMoves };
    const seen = { ...w.__pinch };
    w.__pinch = { soloMoves: 0, soloCancelled: 0, moves: 0, cancelled: 0, pops: 0 };
    return seen;
  });

const pageScale = (page: Page): Promise<number> =>
  page.evaluate(() => window.visualViewport?.scale ?? 1);

/** Undo a pinch zoom. */
async function resetZoom(page: Page): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor: 1 });
  } finally {
    await cdp.detach();
  }
  await expect.poll(() => pageScale(page)).toBe(1);
}

/** Where the scroller is on screen now. */
const scrollerBox = (page: Page): Promise<{ left: number; width: number; y: number }> =>
  page.evaluate(() => {
    const r = (window as unknown as { __scroller: HTMLElement }).__scroller.getBoundingClientRect();
    return { left: r.left, width: r.width, y: r.top + r.height / 2 };
  });

/** A leftward drag across the middle of the scroller. */
async function dragLeft(page: Page, box: Scroller): Promise<void> {
  const y = box.top + box.height / 2;
  await touchSwipe(page, box.left + box.width - 20, y, box.left + 40, y, 8, 400);
  await page.waitForTimeout(SETTLE_MS);
}

/** A rightward drag across the middle of the scroller. */
async function dragRight(page: Page, box: Scroller): Promise<void> {
  const y = box.top + box.height / 2;
  await touchSwipe(page, box.left + 40, y, box.left + box.width - 20, y, 8, 400);
  await page.waitForTimeout(SETTLE_MS);
}

test.describe('a sideways scroller in a message owns the sideways drag', () => {
  test.beforeEach(async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'the panel swipe is mobile-only');
    await openChatRail(page);
    await openGeneralThread(page);
  });

  for (const kind of [
    { name: 'code block', body: WIDE_CODE, selector: 'pre' },
    { name: 'table', body: WIDE_TABLE, selector: '.md-table-scroll' },
  ]) {
    test(`a ${kind.name} scrolls, and the panel swipe waits for its end`, async ({ page }) => {
      const box = await wideMessage(page, kind.body, kind.selector);
      expect(box.overflowX, `the ${kind.name} scrolls sideways`).toBe('auto');
      expect(box.max, `the ${kind.name} is wider than its box`).toBeGreaterThan(40);
      expect(await currentPanel(page)).toBe('center');

      // Leftward: the scroller takes it, the conversation stays.
      await dragLeft(page, box);
      expect(await scrollLeft(page), `the ${kind.name} scrolled`).toBeGreaterThan(0);
      expect(await currentPanel(page), 'still on the conversation').toBe('center');

      // Rightward, from part-way along: the scroller takes it back.
      await setScrollLeft(page, box.max / 2);
      const midway = await scrollLeft(page);
      await dragRight(page, box);
      expect(await scrollLeft(page), `the ${kind.name} scrolled back`).toBeLessThan(midway);
      expect(await currentPanel(page), 'still on the conversation').toBe('center');

      // At its end in the drag direction, the drag is a panel swipe again.
      await setScrollLeft(page, box.max);
      expect(await scrollLeft(page)).toBeGreaterThanOrEqual(box.max - 1);
      await dragLeft(page, box);
      expect(await currentPanel(page), 'the leftward drag at the end swiped panels').toBe('right');
    });
  }

  for (const kind of [
    { name: 'code block', body: WIDE_CODE, selector: 'pre' },
    { name: 'table', body: WIDE_TABLE, selector: '.md-table-scroll' },
  ]) {
    test(`a rightward drag on a ${kind.name} at its start swipes panels, not history`, async ({
      page,
    }) => {
      const box = await wideMessage(page, kind.body, kind.selector);
      expect(await scrollLeft(page)).toBe(0);
      // The scroller starts its own touch-action chain, so the browser would
      // otherwise take the unused pan as a history swipe.
      await page.evaluate(() => {
        const w = window as unknown as { __popstates: string[] };
        w.__popstates = [];
        window.addEventListener('popstate', () => w.__popstates.push(location.pathname));
      });
      const url = page.url();
      await dragRight(page, box);
      expect(
        await page.evaluate(() => (window as unknown as { __popstates: string[] }).__popstates),
        'no history navigation'
      ).toEqual([]);
      expect(page.url(), 'the URL is unchanged').toBe(url);
      expect(await currentPanel(page), 'the drag swiped to the rail').toBe('left');
    });
  }

  for (const kind of [
    { name: 'code block', body: WIDE_CODE, selector: 'pre' },
    { name: 'table', body: WIDE_TABLE, selector: '.md-table-scroll' },
  ]) {
    test(`a pinch on a ${kind.name} at either end zooms the page`, async ({ page }) => {
      const box = await wideMessage(page, kind.body, kind.selector);
      await watchPinchMoves(page);
      const url = page.url();

      // At its start, both fingers landing together. Finger 0 moves right,
      // the way the scroller has no room to go: a one-finger drag like that
      // is cancelled, a pinch must not be. The fingers move apart: zoom in.
      await setScrollLeft(page, 0);
      let at = await scrollerBox(page);
      let mid = at.left + at.width / 2;
      await touchPinch(page, { y: at.y, x0: mid + 20, dx0: 80, x1: mid - 20, dx1: -80 });
      await page.waitForTimeout(SETTLE_MS);
      let seen = await takePinchMoves(page);
      expect(seen.moves, 'two-finger moves reached the page').toBeGreaterThan(4);
      expect(seen.cancelled, 'no two-finger move was cancelled').toBe(0);
      expect(await pageScale(page), 'the pinch zoomed the page').toBeGreaterThan(1);
      expect(seen.pops, 'no history navigation').toBe(0);

      // At its end, finger 0 lands alone and moves left past the touch slop
      // before finger 1 lands. Until then it is a one-finger drag towards
      // the end, and those moves are cancelled; the pinch after them must
      // still zoom, and none of its moves be cancelled.
      await resetZoom(page);
      await setScrollLeft(page, box.max);
      at = await scrollerBox(page);
      mid = at.left + at.width / 2;
      await touchPinch(page, {
        y: at.y,
        x0: mid,
        solo: { steps: 4, dx: -48 },
        dx0: -60,
        x1: mid + 20,
        dx1: 80,
      });
      await page.waitForTimeout(SETTLE_MS);
      seen = await takePinchMoves(page);
      expect(seen.soloMoves, 'one-finger moves reached the page').toBeGreaterThan(0);
      expect(seen.soloCancelled, 'the one-finger drag at the end was cancelled').toBeGreaterThan(0);
      expect(seen.moves, 'two-finger moves reached the page').toBeGreaterThan(4);
      expect(seen.cancelled, 'no two-finger move was cancelled').toBe(0);
      expect(await pageScale(page), 'the pinch zoomed the page').toBeGreaterThan(1);

      expect(seen.pops, 'no history navigation').toBe(0);
      expect(page.url()).toBe(url);
      expect(await currentPanel(page), 'a pinch is not a panel swipe').toBe('center');
    });
  }

  test('a wide table keeps readable columns inside its own scroller', async ({ page }) => {
    const box = await wideMessage(page, WIDE_TABLE, '.md-table-scroll');
    const cells = await page.evaluate(() => {
      const el = (window as unknown as { __scroller: HTMLElement }).__scroller;
      const table = el.querySelector('table')!;
      const widths = [...table.querySelectorAll('tr:first-child > *')].map(
        (c) => c.getBoundingClientRect().width
      );
      return {
        narrowest: Math.min(...widths),
        fontSize: parseFloat(getComputedStyle(table.querySelector('td')!).fontSize),
        wrapperRight: el.getBoundingClientRect().right,
        bubbleRight: el.closest('.md-content')!.getBoundingClientRect().right,
      };
    });
    expect(cells.narrowest, 'no column squashed below six ems').toBeGreaterThanOrEqual(
      cells.fontSize * 6 - 0.5
    );
    expect(cells.wrapperRight, 'the scroller stays inside the message').toBeLessThanOrEqual(
      cells.bubbleRight + 0.5
    );
    expect(box.max, 'the table scrolls inside its wrapper').toBeGreaterThan(0);
    await assertNoHorizontalOverflow(page);
  });
});
