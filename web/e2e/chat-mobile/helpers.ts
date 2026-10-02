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
 * CDP touch gesture helpers and the shared layout assertions used across the
 * mobile chat-layout guard suite.
 *
 * The gesture helpers dispatch raw `Input.dispatchTouchEvent` CDP commands
 * — Chromium-only — so they exercise the actual touch event path the page's
 * swipe handlers and the browser's own scrolling/rubber-band machinery
 * listen to, the same way a real finger would, rather than Playwright's
 * higher-level (and WebKit/Firefox-portable, but synthetic) `locator.tap()`.
 */

import { expect, type Page } from '@playwright/test';

/**
 * Selectors for things that count as "a popup opened" — a sign a touch
 * stroke was short enough to read as a tap instead of a scroll. Scoped to
 * elements this suite's gestures could plausibly trigger (the message
 * context menu today; any Shoelace overlay as a general catch-all for
 * future tap-triggered UI).
 */
const OPEN_POPUP_SELECTOR =
  '.context-menu-overlay, sl-dropdown[open], sl-menu[open], sl-dialog[open], scion-action-sheet[open]';

/**
 * Fails fast, with a clear message naming the stroke that caused it, if an
 * element matching `OPEN_POPUP_SELECTOR` exists anywhere in the page —
 * including inside any shadow root, at any depth, since the context menu
 * this guards against renders inside `scion-chat-thread`'s shadow DOM.
 * Call after every touch stroke that could misfire as a tap, so a bad
 * stroke fails immediately instead of leaving the test stuck behind an
 * overlay it can't scroll past until it times out.
 */
async function assertNoOpenPopup(page: Page, context: string): Promise<void> {
  const found = await page.evaluate((selector) => {
    const findIn = (root: ParentNode): string | null => {
      const hit = root.querySelector(selector);
      if (hit)
        return `${hit.tagName.toLowerCase()}${hit.className ? `.${String(hit.className).split(' ').join('.')}` : ''}`;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          const nested = findIn(el.shadowRoot);
          if (nested) return nested;
        }
      }
      return null;
    };
    return findIn(document);
  }, OPEN_POPUP_SELECTOR);

  expect(found, `no popup/context-menu open after ${context} (found: ${found})`).toBeNull();
}

/**
 * Minimum stroke length, in CSS px, `touchScroll` will ever send. Chromium's
 * touch slop is roughly 15 DIP; a stroke at or below that can register as a
 * tap instead of a drag; `MIN_STROKE_PX` leaves comfortable headroom above
 * it. A caller whose starting point leaves too little room to clear this on
 * every sub-stroke gets a clear error instead of a silently tap-sized drag.
 */
const MIN_STROKE_PX = 48;

/**
 * Touch-scroll a container by dragging from (x, y). Negative `dy` means the
 * finger moves up (content scrolls down); positive means the finger moves
 * down (content scrolls up, including rubber-band overscroll at the top).
 * Long scrolls are split into sub-1-viewport strokes so the stroke's start
 * and end points never leave the viewport.
 *
 * The split is even (`n = ceil(|dy| / room)` equal-length strokes), not
 * greedy (take the largest stroke the room allows, repeat for whatever's
 * left). A greedy split can leave an arbitrarily short final stroke — on a
 * touchscreen (and Chromium's touch emulation), a stroke short enough reads
 * as a tap instead of a scroll, misfiring whatever the page binds to a tap
 * at that point instead of scrolling. An even split keeps every stroke
 * equal to `dy / n`, which is close to `room` when `room` itself is large —
 * but if the caller's starting point leaves little room before the edge the
 * stroke moves toward (for example, a point near the bottom of the
 * viewport), every one of those equal strokes can still land under tap
 * slop; splitting evenly only spreads a short distance evenly, it cannot
 * make a short distance long. `MIN_STROKE_PX` turns that case into a clear
 * error instead of a silent tap-sized drag, so the fix is always "start
 * somewhere with more room", not a larger split. After every stroke that
 * does run, assert no popup opened, so a misfire from some other cause
 * fails immediately with a clear message instead of leaving the gesture
 * stuck behind an overlay it can't scroll past until the test times out.
 */
export async function touchScroll(
  page: Page,
  x: number,
  y: number,
  dx: number,
  dy: number,
  { stepPx = 25, strokeMs = 250 }: { stepPx?: number; strokeMs?: number } = {}
): Promise<void> {
  if (dx === 0 && dy === 0) return;
  const viewport = page.viewportSize();
  const height = viewport?.height ?? 800;
  const room = dy < 0 ? y - 20 : height - y - 20;
  if (room <= 0) {
    throw new Error(
      `touchScroll: no room to stroke from y=${y} toward dy=${dy} (viewport height ${height}); ` +
        'pick a start point at least 20px from the edge the stroke moves toward.'
    );
  }
  const strokeCount = Math.max(1, Math.ceil(Math.abs(dy) / room));
  const d = dy / strokeCount;
  if (Math.abs(d) < MIN_STROKE_PX) {
    throw new Error(
      `touchScroll: a ${Math.abs(d).toFixed(1)}px stroke from y=${y} toward dy=${dy} ` +
        `is at or below touch tap slop (minimum ${MIN_STROKE_PX}px); it would likely register ` +
        'as a tap instead of a drag. Pick a start point with more room in the direction of travel.'
    );
  }
  const strokeDx = dx / strokeCount;
  const cdp = await page.context().newCDPSession(page);
  try {
    for (let strokeIndex = 0; strokeIndex < strokeCount; strokeIndex++) {
      const startY = y;
      const steps = Math.max(4, Math.round(Math.abs(d) / stepPx));
      await cdp.send('Input.dispatchTouchEvent', {
        type: 'touchStart',
        touchPoints: [{ x, y: startY }],
      });
      for (let i = 1; i <= steps; i++) {
        await cdp.send('Input.dispatchTouchEvent', {
          type: 'touchMove',
          touchPoints: [{ x: x + (strokeDx * i) / steps, y: startY + (d * i) / steps }],
        });
        await page.waitForTimeout(strokeMs / steps);
      }
      await page.waitForTimeout(120); // hold briefly to suppress fling
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await page.waitForTimeout(150);
      await assertNoOpenPopup(
        page,
        `touchScroll stroke ${strokeIndex + 1}/${strokeCount} (d=${d.toFixed(1)}px) at (${x}, ${startY})`
      );
    }
  } finally {
    await cdp.detach();
  }
}

/** A CDP touch swipe — the gesture the page's own swipe handler listens to. */
export async function touchSwipe(
  page: Page,
  x1: number,
  y1: number,
  x2: number,
  y2: number,
  steps = 8,
  ms = 120
): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', {
      type: 'touchStart',
      touchPoints: [{ x: x1, y: y1 }],
    });
    for (let i = 1; i <= steps; i++) {
      await cdp.send('Input.dispatchTouchEvent', {
        type: 'touchMove',
        touchPoints: [{ x: x1 + ((x2 - x1) * i) / steps, y: y1 + ((y2 - y1) * i) / steps }],
      });
      await page.waitForTimeout(ms / steps);
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}

/** Press and hold at one point — the gesture a long-press controller listens to. */
export async function touchHold(page: Page, x: number, y: number, holdMs = 600): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
    await page.waitForTimeout(holdMs);
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}

/** CSS class of the panel that is on screen for a given `data-panel` value. */
const ACTIVE_PANEL_SELECTOR: Record<string, string> = {
  left: '.v2-rail',
  center: '.v2-content',
  right: '.v2-members',
};

/**
 * Nothing is wider than the viewport, the swipe track hasn't drifted
 * horizontally, and the active panel fills the screen.
 *
 * The overflow scan visits the active panel itself, every light-DOM
 * descendant at any depth, and (recursively) every element inside any
 * shadow root anywhere in that subtree — not just the panel's own rect or
 * its immediate children's shadow roots. Content like the thread header's
 * title or the members header lives several plain-DOM levels below the
 * panel inside the same shadow root, so a shallow scan would miss it.
 *
 * `sideScrollers` names elements that are meant to scroll sideways (a
 * toolbar row that scrolls when its buttons do not fit). Each must itself
 * fit and must actually be a sideways scroller; what it clips is reachable
 * by scrolling it, so its descendants are not scanned.
 */
export async function assertNoHorizontalOverflow(
  page: Page,
  sideScrollers: readonly string[] = []
): Promise<void> {
  const result = await page.evaluate(
    ([activePanelSelectors, sideScrollers]) => {
      const se = document.scrollingElement as HTMLElement;
      const pageEl = document.querySelector('scion-page-chat') as
        | (HTMLElement & { shadowRoot: ShadowRoot })
        | null;
      const panels = pageEl?.shadowRoot?.querySelector('.v2-panels') as HTMLElement | null;
      const dataPanel = panels?.getAttribute('data-panel') || 'left';
      const activeSelector = activePanelSelectors[dataPanel] || '.v2-rail';
      const active = panels?.querySelector(activeSelector) as HTMLElement | null;
      const activeRect = active?.getBoundingClientRect();

      let maxRight = -Infinity;
      const considerRect = (el: HTMLElement): void => {
        const style = getComputedStyle(el);
        if (style.display === 'none' || style.visibility === 'hidden') return;
        const rect = el.getBoundingClientRect();
        if (rect.width > 0 && rect.height > 0) {
          maxRight = Math.max(maxRight, rect.right);
        }
      };
      const notScrollers: string[] = [];
      const isSideScroller = (el: HTMLElement): boolean => {
        if (!sideScrollers.some((selector) => el.matches(selector))) return false;
        const overflowX = getComputedStyle(el).overflowX;
        if (overflowX !== 'auto' && overflowX !== 'scroll') {
          notScrollers.push(`${el.tagName.toLowerCase()}.${el.className}`);
          return false;
        }
        return true;
      };
      // Recurse into every shadow root under `root`, at any depth, skipping
      // what a declared sideways scroller clips.
      const walkSubtree = (root: ParentNode): void => {
        const clipped: HTMLElement[] = [];
        for (const el of root.querySelectorAll('*')) {
          if (!(el instanceof HTMLElement)) continue;
          if (clipped.some((scroller) => scroller.contains(el))) continue;
          considerRect(el);
          if (isSideScroller(el)) {
            clipped.push(el);
            continue;
          }
          if (el.shadowRoot) walkSubtree(el.shadowRoot);
        }
      };
      if (active) {
        considerRect(active);
        walkSubtree(active);
      }

      return {
        activeFound: active !== null,
        panelsFound: panels !== null,
        scrollWidth: se.scrollWidth,
        clientWidth: se.clientWidth,
        panelsScrollLeft: panels?.scrollLeft ?? null,
        activeLeft: activeRect?.left ?? null,
        activeWidth: activeRect?.width ?? null,
        innerWidth: window.innerWidth,
        maxRight,
        notScrollers,
      };
    },
    [ACTIVE_PANEL_SELECTOR, sideScrollers] as const
  );

  expect(result.notScrollers, 'declared sideways scrollers scroll sideways').toEqual([]);
  expect(result.panelsFound, '.v2-panels was found').toBe(true);
  expect(result.activeFound, 'the active panel element was found').toBe(true);
  expect(
    result.scrollWidth,
    'document.scrollingElement.scrollWidth <= clientWidth'
  ).toBeLessThanOrEqual(result.clientWidth);
  expect(result.panelsScrollLeft, '.v2-panels.scrollLeft === 0').toBe(0);
  expect(result.activeLeft, 'active panel left === 0').toBeCloseTo(0, 0);
  expect(result.activeWidth, 'active panel width === innerWidth').toBeCloseTo(result.innerWidth, 0);
  expect(result.maxRight, 'no visible element right edge past innerWidth').toBeLessThanOrEqual(
    result.innerWidth + 0.5
  );
}

/**
 * The document never scrolls, frame mode is actually engaged (the
 * `scion-app-frame` class on `<html>` and its computed `overflow: hidden`
 * — not just an absence of visible scrolling, which a disabled or removed
 * frame mode would also show in Chromium emulation), the header stays
 * pinned to the top, and the shell fills the viewport height.
 */
export async function assertFramePinned(page: Page): Promise<void> {
  const result = await page.evaluate(() => {
    const se = document.scrollingElement as HTMLElement;
    const shell = (document.querySelector('scion-chat-shell') ||
      document.querySelector('scion-app') ||
      document.querySelector('scion-profile-shell')) as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const header = shell?.shadowRoot?.querySelector('scion-header') as HTMLElement | null;
    const vv = window.visualViewport;
    return {
      shellFound: shell !== null,
      headerFound: header !== null,
      hasFrameClass: document.documentElement.classList.contains('scion-app-frame'),
      htmlOverflow: getComputedStyle(document.documentElement).overflow,
      scrollY: window.scrollY,
      offsetTop: vv ? vv.offsetTop : 0,
      scrollHeight: se.scrollHeight,
      clientHeight: se.clientHeight,
      headerTop: header?.getBoundingClientRect().top ?? null,
      shellHeight: shell?.getBoundingClientRect().height ?? null,
      innerHeight: window.innerHeight,
    };
  });

  expect(
    result.shellFound,
    'a shell element (scion-chat-shell/scion-app/scion-profile-shell) was found'
  ).toBe(true);
  expect(result.headerFound, 'scion-header was found inside the shell').toBe(true);
  expect(result.hasFrameClass, 'html.scion-app-frame is set').toBe(true);
  expect(result.htmlOverflow, 'html computed overflow is hidden').toBe('hidden');
  expect(result.scrollY, 'window.scrollY === 0').toBe(0);
  expect(result.offsetTop, 'visualViewport.offsetTop === 0').toBe(0);
  expect(result.scrollHeight, 'scrollingElement.scrollHeight <= clientHeight').toBeLessThanOrEqual(
    result.clientHeight
  );
  expect(result.headerTop, 'scion-header top === 0').toBeCloseTo(0, 0);
  expect(result.shellHeight, 'shell height === innerHeight').toBeCloseTo(result.innerHeight, 0);
}

/**
 * Finds a visible element matching `hostSelector` anywhere in the document,
 * including inside any shadow root at any depth (a single CSS selector
 * cannot cross a shadow boundary, so `hostSelector` must resolve entirely
 * within one root — typically this app's own component shadow roots, since
 * slotted light-DOM content such as an `sl-menu`'s `sl-menu-item` children
 * stays queryable from its parent's root). Where `hostSelector` matches more
 * than once at a given level (for example, the same menu-item value appears
 * once per rendered space, or a Shoelace dropdown that is open renders its
 * closed siblings too), the first match with a non-zero rendered size wins,
 * so an open dropdown's items are measured rather than a closed one's. If
 * `innerSelector` is given, the result is then one more `querySelector`
 * inside the host's *own* shadow root — the one level needed to reach a
 * Shoelace shadow part such as `[part="base"]` or `[part="textarea"]`,
 * which a plain CSS selector can never reach from outside.
 *
 * Returns the computed font-size and border-box size in CSS px, or `null`
 * if nothing matches, so callers can assert a clear "element not found"
 * failure instead of a confusing NaN comparison.
 */
export async function computedBoxDeep(
  page: Page,
  hostSelector: string,
  innerSelector?: string
): Promise<{ fontSizePx: number; widthPx: number; heightPx: number } | null> {
  return page.evaluate(
    ({ hostSelector, innerSelector }) => {
      const isVisible = (el: Element): boolean => {
        const rect = el.getBoundingClientRect();
        if (rect.width <= 0 || rect.height <= 0) return false;
        const style = getComputedStyle(el);
        return style.display !== 'none' && style.visibility !== 'hidden';
      };
      const findIn = (root: ParentNode): Element | null => {
        const matches = [...root.querySelectorAll(hostSelector)];
        const visible = matches.find(isVisible);
        if (visible) return visible;
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) {
            const nested = findIn(el.shadowRoot);
            if (nested) return nested;
          }
        }
        // No visible match anywhere, including nested shadow roots: fall
        // back to the first match in this root, if any, so a genuinely
        // hidden-but-present element (not the case any current caller
        // hits) still reports its size rather than a confusing "not found".
        return matches[0] ?? null;
      };
      const host = findIn(document);
      if (!host) return null;
      const el = innerSelector ? (host.shadowRoot?.querySelector(innerSelector) ?? null) : host;
      if (!el) return null;
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return {
        fontSizePx: Number.parseFloat(style.fontSize),
        widthPx: rect.width,
        heightPx: rect.height,
      };
    },
    { hostSelector, innerSelector }
  );
}

/**
 * How long `computedBoxDeepRetrying` retries before giving up. `toPass()`
 * has no timeout of its own by default — without one explicitly set here,
 * a genuinely missing element would retry silently until the *enclosing
 * test's* timeout, turning a fast, clearly-labelled failure into a slow,
 * confusing one.
 */
const COMPUTED_BOX_RETRY_TIMEOUT_MS = 10_000;

/**
 * Like `computedBoxDeep`, but retries (for up to
 * `COMPUTED_BOX_RETRY_TIMEOUT_MS`) until a match is found, instead of only
 * checking once.
 *
 * Several of this suite's fixtures land on a URL that redirects once mock
 * data loads (see `openChatRail`'s doc comment): that redirect is a full
 * client-side navigation, and the app's router handles every navigation by
 * removing the old page element and creating a new one. A single
 * `computedBoxDeep` call that happens to run in that gap — or before a late-
 * upgrading custom element's shadow root exists — finds nothing even though
 * the page is correct a moment later. Fixing each fixture's own wait
 * narrows the window but does not provably close it for every call site, so
 * every measurement in this suite that does not already sit behind an
 * explicit `locator.waitFor()` uses this instead of the one-shot version.
 *
 * Only "not found" is retried: the returned box's values are asserted by
 * the caller, outside this retry loop, so a genuine size/font regression
 * still fails on the first (and every) attempt rather than being masked.
 */
export async function computedBoxDeepRetrying(
  page: Page,
  hostSelector: string,
  innerSelector?: string
): Promise<{ fontSizePx: number; widthPx: number; heightPx: number }> {
  let result: { fontSizePx: number; widthPx: number; heightPx: number } | null = null;
  await expect(async () => {
    result = await computedBoxDeep(page, hostSelector, innerSelector);
    expect(
      result,
      `${hostSelector}${innerSelector ? ' ' + innerSelector : ''} was found`
    ).not.toBeNull();
  }).toPass({ timeout: COMPUTED_BOX_RETRY_TIMEOUT_MS });
  return result!;
}

/**
 * The deepest actually-focused element, following `shadowRoot.activeElement`
 * down through every open shadow root a plain `document.activeElement`
 * would otherwise stop at (it only reports the top-level custom element
 * hosting the real focus target, e.g. `scion-chat-composer` rather than its
 * inner `<textarea>`). Returns the element's tag name in lowercase, or
 * `null` if nothing is focused (`document.activeElement` is `<body>`).
 */
export async function deepActiveElementTagName(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el) {
      const root = (el as Element & { shadowRoot?: ShadowRoot }).shadowRoot;
      const next = root?.activeElement ?? null;
      if (!next || next === el) break;
      el = next;
    }
    if (!el || el === document.body) return null;
    return el.tagName.toLowerCase();
  });
}

/**
 * Appends a very tall, zero-opacity element directly to `<body>` and
 * returns a cleanup function. Used to confirm frame mode actually stops a
 * stray oversized element from making the document scrollable, rather than
 * the page merely happening not to have one today.
 */
export async function addStrayTallElement(page: Page): Promise<() => Promise<void>> {
  await page.evaluate(() => {
    const el = document.createElement('div');
    el.setAttribute('data-stray-tall-probe', '');
    el.style.cssText = 'position:static;width:10px;height:3000px;opacity:0;pointer-events:none;';
    document.body.appendChild(el);
  });
  return async () => {
    await page.evaluate(() => {
      document.querySelector('[data-stray-tall-probe]')?.remove();
    });
  };
}
