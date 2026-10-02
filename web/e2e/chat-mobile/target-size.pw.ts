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
 * Touch targets on mobile meet the design's minimum sizes: rail thread rows
 * at least 48px tall, space headers, the All/Unread filter and other rows
 * at least 44px tall (with the filter's target proven reachable, not just
 * sized, via `elementFromPoint`), thread-header icon buttons at least 33px
 * visible with a 44px-tall hit area, a centred 44px icon-only Send square,
 * and every Shoelace menu item at least 44px tall. Desktop is excluded —
 * these minimums are a mobile/touch concern, not a desktop one.
 */

import { test, expect, type Page, type TestInfo } from '@playwright/test';
import { openChatRail, openGeneralThread, expandSpace } from './fixture.js';
import { computedBoxDeepRetrying } from './helpers.js';

const MIN_ROW_PX = 44;
const MIN_MENU_ITEM_PX = 44;
const MIN_RAIL_ROW_PX = 48;
const MIN_RAIL_FONT_PX = 17;
const MIN_SPACE_HEADER_FONT_PX = 14;
const MIN_FILTER_LABEL_FONT_PX = 15;
const MIN_RAIL_TITLE_FONT_PX = 18;

/**
 * Explicit timeout for every `expect(async () => {...}).toPass()` retry
 * loop in this file. `toPass()` defaults to no timeout of its own (it
 * retries until the enclosing test's own timeout), which turns a genuinely
 * missing element into a slow failure instead of a fast, clearly-labelled
 * one.
 */
const TOPASS_TIMEOUT_MS = 10_000;

function skipOnDesktop(testInfo: TestInfo): void {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch-target minimums are mobile-only');
}

/** Resolve the border-box height of `innerSelector` inside `hostSelector`'s own shadow root. */
async function partHeight(
  page: Page,
  hostSelector: string,
  innerSelector: string
): Promise<number> {
  const box = await computedBoxDeepRetrying(page, hostSelector, innerSelector);
  return box.heightPx;
}

test('@static rail thread rows are at least 48px tall with 17px text', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await expandSpace(page);
  const box = await computedBoxDeepRetrying(page, '.thread-item');
  expect(box.heightPx).toBeGreaterThanOrEqual(MIN_RAIL_ROW_PX);
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_RAIL_FONT_PX);
});

test('@static the rail space header is at least 44px tall with 14px text', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  const box = await computedBoxDeepRetrying(page, '.space-header');
  expect(box.heightPx).toBeGreaterThanOrEqual(MIN_ROW_PX);
  expect(box.fontSizePx).toBeGreaterThanOrEqual(MIN_SPACE_HEADER_FONT_PX);
});

test('@static the rail filter labels and title meet their mobile font-size minimums', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  const filterBtn = await computedBoxDeepRetrying(page, '.rail-toolbar .filter-toggle button');
  expect(filterBtn.fontSizePx).toBeGreaterThanOrEqual(MIN_FILTER_LABEL_FONT_PX);

  const railTitle = await computedBoxDeepRetrying(page, '.rail-header');
  expect(railTitle.fontSizePx).toBeGreaterThanOrEqual(MIN_RAIL_TITLE_FONT_PX);
});

for (const filterIndex of [0, 1] as const) {
  const filterName = filterIndex === 0 ? 'All' : 'Unread';

  test(`@static the ${filterName} filter button is a real 44px hit area, not clipped`, async ({
    page,
  }, testInfo) => {
    skipOnDesktop(testInfo);
    await openChatRail(page);

    // A size assertion alone does not prove the target is reachable: an
    // ancestor's overflow: hidden (the segmented toggle's rounded border, in
    // this case) can clip a pseudo-element hit area down to whatever the
    // ancestor's own box allows, even though the button's own computed size
    // looks correct in isolation. elementFromPoint checks what a tap at each
    // claimed edge would actually hit. Both segments share the same
    // ancestor and CSS, so both are checked here — a rule scoped to one
    // cannot hide behind the other passing.
    const box = await computedBoxDeepRetrying(
      page,
      `.rail-toolbar .filter-toggle button:nth-of-type(${filterIndex + 1})`
    );
    expect(box.heightPx).toBeGreaterThanOrEqual(MIN_ROW_PX);

    let hitsNearTop = false;
    let hitsNearBottom = false;
    await expect(async () => {
      const result = await page.evaluate((index) => {
        const findIn = (root: ParentNode, sel: string): Element | null => {
          const hit = root.querySelector(sel);
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            if (el.shadowRoot) {
              const nested = findIn(el.shadowRoot, sel);
              if (nested) return nested;
            }
          }
          return null;
        };
        const root = findIn(document, 'scion-chat-space-rail')?.shadowRoot;
        const buttons = root ? [...root.querySelectorAll('.filter-toggle button')] : [];
        const button = buttons[index] ?? null;
        if (!root || !button) return null;
        const rect = button.getBoundingClientRect();
        const x = rect.left + rect.width / 2;
        const atTop = root.elementFromPoint(x, rect.top + 2);
        const atBottom = root.elementFromPoint(x, rect.bottom - 2);
        return {
          hitsTop: atTop === button || button.contains(atTop),
          hitsBottom: atBottom === button || button.contains(atBottom),
        };
      }, filterIndex);
      expect(result, 'the filter-toggle button was found').not.toBeNull();
      hitsNearTop = result!.hitsTop;
      hitsNearBottom = result!.hitsBottom;
    }).toPass({ timeout: TOPASS_TIMEOUT_MS });

    expect(hitsNearTop, 'a tap 2px inside the top edge must hit the button').toBe(true);
    expect(hitsNearBottom, 'a tap 2px inside the bottom edge must hit the button').toBe(true);
  });
}

test('@static the rail sort and space-actions icon buttons have a 44px hit area', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  // Scoped by label, not by the bare '.sort-btn' class: chat-members.ts
  // reuses that same class for its own (off-screen, ~19px) sort button, so
  // computedBoxDeep's first-visible-match search could land on either one
  // depending on load order. computedBoxDeepRetrying also retries, since
  // right after openChatRail the rail's sl-icon-button hosts may not have
  // upgraded (no shadowRoot yet) on the first check.
  const sortBase = await computedBoxDeepRetrying(
    page,
    'sl-icon-button[label="Sort"]',
    '[part="base"]'
  );
  expect(sortBase.widthPx).toBeGreaterThanOrEqual(MIN_ROW_PX);
  expect(sortBase.heightPx).toBeGreaterThanOrEqual(MIN_ROW_PX);

  const kebabBase = await computedBoxDeepRetrying(
    page,
    'sl-icon-button[label="Space actions"]',
    '[part="base"]'
  );
  expect(kebabBase.widthPx).toBeGreaterThanOrEqual(MIN_ROW_PX);
  expect(kebabBase.heightPx).toBeGreaterThanOrEqual(MIN_ROW_PX);
});

test('@static thread-header icon buttons have a real 44px-tall hit area with no overlap', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);

  // The visible box stays at its native size (several of these sit in one
  // row at 320px, and growing each to a full visible box pushes the row
  // past the viewport) — the hit area grows instead, via a ::before
  // overlay that is still part of the button for click purposes. A
  // pseudo-element's computed style can read as the right inset even when
  // it is not actually hit-testable (clipped by an ancestor's overflow, or
  // not rendered at all because `content` was stripped), so this hit-tests
  // the real target with `elementFromPoint` instead of trusting the
  // computed inset.
  //
  // `elementFromPoint` only ever returns the single topmost element at a
  // point, so a plain "probe the midpoint between the two buttons" check
  // cannot detect an overlap: if one button's hit area extends into its
  // neighbour's territory, the midpoint still resolves to exactly one of
  // them, same as a non-overlapping pair would — and a single-topmost probe
  // at one button's own edge has the same blind spot in the other
  // direction, since which of two overlapping hit areas paints on top
  // depends on sibling order, not on which one is doing the overlapping.
  // `elementsFromPoint` returns the full stack at a point, not just the
  // topmost, so overlap is instead caught by asserting the *neighbouring*
  // button's host is absent from the stack at a point just inside each
  // button's own visible edge, the side facing that neighbour — present
  // anywhere in the stack (not just on top) means that neighbour's hit area
  // has reached across the gap onto this button's own box.
  type EdgeProbe = { hitsAboveTop: boolean; hitsBelowBottom: boolean };
  let search: EdgeProbe | undefined;
  let members: EdgeProbe | undefined;
  let searchOwnRightEdgeHasNoMembersOverlap = false;
  let membersOwnLeftEdgeHasNoSearchOverlap = false;
  await expect(async () => {
    const result = await page.evaluate(() => {
      const findIn = (root: ParentNode, sel: string): Element | null => {
        const hit = root.querySelector(sel);
        if (hit) return hit;
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) {
            const nested = findIn(el.shadowRoot, sel);
            if (nested) return nested;
          }
        }
        return null;
      };
      const pageRoot = findIn(document, 'scion-page-chat')?.shadowRoot ?? null;
      const hostAndBase = (
        hostSelector: string
      ): { host: Element; base: Element; rect: DOMRect } | null => {
        const host = pageRoot?.querySelector(hostSelector);
        const base = host?.shadowRoot?.querySelector('[part="base"]');
        return host && base ? { host, base, rect: base.getBoundingClientRect() } : null;
      };
      const probe = (hostSelector: string): EdgeProbe | null => {
        if (!pageRoot) return null;
        const found = hostAndBase(hostSelector);
        if (!found) return null;
        const { host, rect } = found;
        const x = rect.left + rect.width / 2;
        const atAbove = pageRoot.elementFromPoint(x, rect.top - 3);
        const atBelow = pageRoot.elementFromPoint(x, rect.bottom + 3);
        return {
          hitsAboveTop: atAbove === host || host.contains(atAbove),
          hitsBelowBottom: atBelow === host || host.contains(atBelow),
        };
      };
      const searchSelector = '.v2-thread-header sl-icon-button[label="Search messages"]';
      const membersSelector = '.v2-thread-header sl-icon-button.mobile-members';
      const searchResult = probe(searchSelector);
      const membersResult = probe(membersSelector);

      // Probe 1px inside each button's own visible right/left edge, the
      // side that faces its neighbour, and check the *entire* hit-test
      // stack there (not just the topmost element) for the neighbour's
      // host — present anywhere in the stack means that neighbour's hit
      // area has reached across the gap onto this button's own box,
      // regardless of which of the two overlapping overlays happens to
      // paint on top.
      const stackContainsHost = (stack: Element[], host: Element): boolean =>
        stack.some((el) => el === host || host.contains(el));
      let searchOwnRightEdgeHasNoMembersOverlap = false;
      let membersOwnLeftEdgeHasNoSearchOverlap = false;
      if (pageRoot) {
        const searchFound = hostAndBase(searchSelector);
        const membersFound = hostAndBase(membersSelector);
        if (searchFound && membersFound) {
          const y = searchFound.rect.top + searchFound.rect.height / 2;
          const stackAtSearchRightEdge = pageRoot.elementsFromPoint(searchFound.rect.right - 1, y);
          searchOwnRightEdgeHasNoMembersOverlap = !stackContainsHost(
            stackAtSearchRightEdge,
            membersFound.host
          );
        }
        if (searchFound && membersFound) {
          const y = membersFound.rect.top + membersFound.rect.height / 2;
          const stackAtMembersLeftEdge = pageRoot.elementsFromPoint(membersFound.rect.left + 1, y);
          membersOwnLeftEdgeHasNoSearchOverlap = !stackContainsHost(
            stackAtMembersLeftEdge,
            searchFound.host
          );
        }
      }
      return {
        search: searchResult,
        members: membersResult,
        searchOwnRightEdgeHasNoMembersOverlap,
        membersOwnLeftEdgeHasNoSearchOverlap,
      };
    });
    expect(result.search, 'the search icon button base was found').not.toBeNull();
    expect(result.members, 'the mobile-members icon button base was found').not.toBeNull();
    search = result.search!;
    members = result.members!;
    searchOwnRightEdgeHasNoMembersOverlap = result.searchOwnRightEdgeHasNoMembersOverlap;
    membersOwnLeftEdgeHasNoSearchOverlap = result.membersOwnLeftEdgeHasNoSearchOverlap;
  }).toPass({ timeout: TOPASS_TIMEOUT_MS });

  expect(search!.hitsAboveTop, 'a tap 3px above the search button must hit it').toBe(true);
  expect(search!.hitsBelowBottom, 'a tap 3px below the search button must hit it').toBe(true);
  expect(members!.hitsAboveTop, 'a tap 3px above the members button must hit it').toBe(true);
  expect(members!.hitsBelowBottom, 'a tap 3px below the members button must hit it').toBe(true);
  expect(
    searchOwnRightEdgeHasNoMembersOverlap,
    "the members button's hit area must not reach across the gap onto the search button's own visible edge"
  ).toBe(true);
  expect(
    membersOwnLeftEdgeHasNoSearchOverlap,
    "the search button's hit area must not reach across the gap onto the members button's own visible edge"
  ).toBe(true);
});

test('@static the members back button is at least 44px', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);
  await page.locator('.mobile-members').click();
  await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  await page.waitForTimeout(400);

  const height = await partHeight(page, '.v2-members-header .mobile-back', '[part="base"]');
  expect(height).toBeGreaterThanOrEqual(MIN_ROW_PX);
});

test('@static the composer Send control has the accessible name "Send"', async ({ page }) => {
  // Checked on both mobile and desktop: the button's name must survive
  // regardless of which layout hides the visible text. getByRole resolves
  // through the real accessibility tree (including Shoelace's shadow DOM),
  // so this exercises the actual focusable control a screen reader
  // reaches — not the sl-button host, whose own aria-label (if any) never
  // reaches the native button Shoelace renders inside its shadow root.
  await openChatRail(page);
  await openGeneralThread(page);
  await expect(page.getByRole('button', { name: 'Send', exact: true })).toHaveCount(1);
});

test('@static on mobile, the composer Send control is a centred icon-only 44px square', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);

  const box = await computedBoxDeepRetrying(page, '.send-btn', '[part="base"]');
  expect(box.widthPx).toBeGreaterThanOrEqual(MIN_ROW_PX);
  expect(box.heightPx).toBeGreaterThanOrEqual(MIN_ROW_PX);

  // The label is visually hidden (clipped to 1x1), not removed — this
  // asserts the specific hiding technique, so a future regression to
  // `display: none` (which would also drop the accessible name checked
  // above) is caught.
  const labelBox = await computedBoxDeepRetrying(page, '.send-btn .send-label');
  expect(labelBox.widthPx).toBeLessThanOrEqual(1);

  // The label slot wrapper keeps its own padding even once the slotted
  // content collapses to 1x1 — without stripping it, the icon sits
  // noticeably off-centre in the square button instead of in its middle.
  const SEND_ICON_CENTRE_TOLERANCE_PX = 2;
  let offsetX = Infinity;
  let offsetY = Infinity;
  await expect(async () => {
    const geometry = await page.evaluate(() => {
      const findIn = (root: ParentNode, sel: string): Element | null => {
        const hit = root.querySelector(sel);
        if (hit) return hit;
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) {
            const nested = findIn(el.shadowRoot, sel);
            if (nested) return nested;
          }
        }
        return null;
      };
      const host = findIn(document, '.send-btn');
      const base = host?.shadowRoot?.querySelector('[part="base"]');
      const icon = host?.querySelector('sl-icon');
      if (!base || !icon) return null;
      const baseRect = base.getBoundingClientRect();
      const iconRect = icon.getBoundingClientRect();
      return {
        baseCentreX: baseRect.left + baseRect.width / 2,
        baseCentreY: baseRect.top + baseRect.height / 2,
        iconCentreX: iconRect.left + iconRect.width / 2,
        iconCentreY: iconRect.top + iconRect.height / 2,
      };
    });
    expect(geometry, 'the send button base and its icon were both found').not.toBeNull();
    offsetX = Math.abs(geometry!.baseCentreX - geometry!.iconCentreX);
    offsetY = Math.abs(geometry!.baseCentreY - geometry!.iconCentreY);
  }).toPass({ timeout: TOPASS_TIMEOUT_MS });
  expect(offsetX).toBeLessThanOrEqual(SEND_ICON_CENTRE_TOLERANCE_PX);
  expect(offsetY).toBeLessThanOrEqual(SEND_ICON_CENTRE_TOLERANCE_PX);
});

test('@static a long message bubble uses at least 85% of the message column', async ({
  page,
}, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);
  await openGeneralThread(page);

  // The seeded long-prose message is long enough to hit the bubble's
  // max-width cap rather than shrinking to fit its content.
  //
  // "The message column" is the space beside the avatar, not the whole
  // row: the design asks for a bubble width close to the full column
  // beside the avatar, and the avatar (plus its gap) is a fixed-width
  // gutter the bubble was never meant to claim. Measuring against the full
  // row width instead would make 85% architecturally unreachable at the
  // narrowest supported viewport purely because the avatar gutter is a
  // larger fraction of a smaller row — not because the bubble is failing
  // to use the space actually available to it.
  //
  // Every measurement below (bubble, its wrapper, that wrapper's own
  // avatar) comes from the *same* <scion-chat-message> host's shadow root
  // — `.avatar` is also used by the separate chat-avatar component
  // elsewhere in the page, so picking the first match of each class
  // anywhere in the document could mix parts of unrelated messages.
  let bubbleWidth = 0;
  let columnWidth = 0;
  await expect(async () => {
    const result = await page.evaluate(() => {
      const findIn = (root: ParentNode, sel: string): Element[] => {
        const out: Element[] = [...root.querySelectorAll(sel)];
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) out.push(...findIn(el.shadowRoot, sel));
        }
        return out;
      };
      const messageHosts = findIn(document, 'scion-chat-message');
      let widestBubbleWidth = 0;
      let columnWidth = 0;
      for (const host of messageHosts) {
        const root = host.shadowRoot;
        if (!root) continue;
        const bubble = root.querySelector('.bubble');
        const wrapper = root.querySelector('.message-wrapper');
        if (!bubble || !wrapper) continue;
        const bubbleWidth = bubble.getBoundingClientRect().width;
        if (bubbleWidth <= widestBubbleWidth) continue;
        const wrapperStyle = getComputedStyle(wrapper);
        const avatar = root.querySelector('.avatar') ?? root.querySelector('.avatar-spacer');
        const avatarWidth = avatar?.getBoundingClientRect().width ?? 0;
        const gap = Number.parseFloat(wrapperStyle.gap || '0');
        const paddingLeft = Number.parseFloat(wrapperStyle.paddingLeft || '0');
        const paddingRight = Number.parseFloat(wrapperStyle.paddingRight || '0');
        const rowWidth = wrapper.getBoundingClientRect().width;
        widestBubbleWidth = bubbleWidth;
        columnWidth = rowWidth - paddingLeft - paddingRight - avatarWidth - gap;
      }
      return { bubbleWidth: widestBubbleWidth, columnWidth };
    });
    expect(result.columnWidth, 'a message bubble, wrapper and avatar were found').toBeGreaterThan(
      0
    );
    bubbleWidth = result.bubbleWidth;
    columnWidth = result.columnWidth;
  }).toPass({ timeout: TOPASS_TIMEOUT_MS });

  expect(bubbleWidth / columnWidth).toBeGreaterThanOrEqual(0.85);
});

test('@static space-actions sl-menu items are at least 44px', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  await page.locator('sl-icon-button[label="Space actions"]').first().click();
  await page.locator('sl-menu-item[value="new-thread"]').first().waitFor({ state: 'visible' });
  await page.waitForTimeout(250);

  const height = await partHeight(page, 'sl-menu-item[value="new-thread"]', '[part="base"]');
  expect(height).toBeGreaterThanOrEqual(MIN_MENU_ITEM_PX);
});

test('@static the rail sort dropdown menu items are at least 44px', async ({ page }, testInfo) => {
  skipOnDesktop(testInfo);
  await openChatRail(page);

  // '.sort-btn' is reused by chat-members.ts's own sort dropdown, which is
  // present (inert, off-screen) in the DOM alongside the rail's; the label
  // disambiguates which one gets clicked.
  await page.locator('sl-icon-button[label="Sort"]').first().click();
  await page.locator('sl-menu-item[value="activity"]').first().waitFor({ state: 'visible' });
  await page.waitForTimeout(250);

  const height = await partHeight(page, 'sl-menu-item[value="activity"]', '[part="base"]');
  expect(height).toBeGreaterThanOrEqual(MIN_MENU_ITEM_PX);
});
