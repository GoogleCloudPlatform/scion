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
 * Safe-area insets and the visual-viewport frame module.
 *
 * Emulated devices report every `env(safe-area-inset-*)` as 0, so the inset
 * tests force non-zero insets through Chromium's
 * `Emulation.setSafeAreaInsetsOverride` and measure the computed padding.
 * They are Chromium-only; the zero-inset baseline is `@static` and also runs
 * on webkit-iphone.
 *
 * The keyboard cannot be emulated either: the open-keyboard composer rule is
 * driven by setting the same root custom property the frame module sets.
 * The module's own logic is unit-tested in `src/client/viewport.test.ts`.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { assertFramePinned, assertNoHorizontalOverflow } from './helpers.js';

interface Insets {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

/** A notched phone in portrait: status bar on top, home indicator below. */
const PORTRAIT: Insets = { top: 47, right: 0, bottom: 34, left: 0 };
/** The same phone in landscape: notch on one side, rounded corner on the other. */
const LANDSCAPE: Insets = { top: 0, right: 47, bottom: 21, left: 47 };

const HEADER_ROW_PX = 60;
const HEADER_BORDER_PX = 1;
/** The composer's own bottom padding (0.75rem) when no inset applies. */
const COMPOSER_PAD_PX = 12;

async function forceSafeAreaInsets(page: Page, insets: Insets): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets });
}

/**
 * The computed box of the first element matching `selector` found anywhere
 * in the page, searching every open shadow root. A selector cannot cross a
 * shadow boundary, so `inner`, when given, is then looked up inside that
 * element's own shadow root (e.g. the composer's `.composer`). Lengths are
 * in CSS px; `null` when nothing matches.
 */
async function deepBox(
  page: Page,
  selector: string,
  inner?: string
): Promise<{
  top: number;
  left: number;
  right: number;
  height: number;
  paddingTop: number;
  paddingRight: number;
  paddingBottom: number;
  paddingLeft: number;
  borderLeft: number;
  borderRight: number;
} | null> {
  return page.evaluate(
    ({ selector, inner }) => {
      const findIn = (root: ParentNode): Element | null => {
        const hit = root.querySelector(selector);
        if (hit) return hit;
        for (const el of root.querySelectorAll('*')) {
          if (el.shadowRoot) {
            const nested = findIn(el.shadowRoot);
            if (nested) return nested;
          }
        }
        return null;
      };
      const host = findIn(document);
      const el = inner ? (host?.shadowRoot?.querySelector(inner) ?? null) : host;
      if (!el) return null;
      const cs = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return {
        top: rect.top,
        left: rect.left,
        right: rect.right,
        height: rect.height,
        paddingTop: parseFloat(cs.paddingTop),
        paddingRight: parseFloat(cs.paddingRight),
        paddingBottom: parseFloat(cs.paddingBottom),
        paddingLeft: parseFloat(cs.paddingLeft),
        borderLeft: parseFloat(cs.borderLeftWidth),
        borderRight: parseFloat(cs.borderRightWidth),
      };
    },
    { selector, inner: inner ?? null }
  );
}

/** Like `deepBox`, but waits until the element exists (the page may still be mounting). */
async function deepBoxRetrying(
  page: Page,
  selector: string,
  inner?: string
): Promise<NonNullable<Awaited<ReturnType<typeof deepBox>>>> {
  let box: Awaited<ReturnType<typeof deepBox>> = null;
  await expect(async () => {
    box = await deepBox(page, selector, inner);
    expect(box, `${selector}${inner ? ` ${inner}` : ''} was found`).not.toBeNull();
  }).toPass({ timeout: 10_000 });
  return box!;
}

const skipUnlessChromium = (projectName: string): void => {
  test.skip(projectName.startsWith('webkit'), 'safe-area override is a Chromium CDP command');
};

/** The document neither scrolls nor overflows sideways. */
async function assertDocumentStill(page: Page): Promise<void> {
  const doc = await page.evaluate(() => {
    const se = document.scrollingElement as HTMLElement;
    return { scrollWidth: se.scrollWidth, clientWidth: se.clientWidth, scrollY: window.scrollY };
  });
  expect(doc.scrollWidth, 'no horizontal document overflow').toBeLessThanOrEqual(doc.clientWidth);
  expect(doc.scrollY, 'window.scrollY').toBe(0);
}

/**
 * Side-by-side layout with a conversation open: the edge columns pad their
 * own content while their surfaces still reach the screen edges (no band
 * of page background beside them), and the track itself has no padding.
 * Then, with the members panel hidden, the conversation is the right edge:
 * its header and composer still reach the screen edge, while the message
 * text clears the inset.
 */
async function assertSideBySideInsets(page: Page, insets: Insets): Promise<void> {
  const innerWidth = await page.evaluate(() => window.innerWidth);
  await expect(async () => {
    const track = await deepBoxRetrying(page, '.v2-panels');
    expect(track.paddingLeft, '.v2-panels padding-left').toBe(0);
    expect(track.paddingRight, '.v2-panels padding-right').toBe(0);

    const rail = await deepBoxRetrying(page, '.v2-panels .v2-rail');
    expect(rail.paddingLeft, 'rail padding-left').toBe(insets.left);
    expect(rail.left, 'rail surface starts at the screen edge').toBeCloseTo(0, 0);

    const members = await deepBoxRetrying(page, '.v2-panels .v2-members');
    expect(members.paddingRight, 'members padding-right').toBe(insets.right);
    expect(members.right, 'members surface ends at the screen edge').toBeCloseTo(innerWidth, 0);

    const content = await deepBoxRetrying(page, '.v2-panels .v2-content');
    expect(content.paddingLeft, 'content padding-left').toBe(0);
    expect(content.paddingRight, 'content padding-right').toBe(0);
    const header = await deepBoxRetrying(page, '.v2-thread-header');
    expect(header.borderRight, 'thread header right inset while members show').toBe(0);
  }).toPass({ timeout: 5_000 });
  await assertDocumentStill(page);

  await page.getByRole('button', { name: 'Show/Hide members' }).click();
  await expect(async () => {
    const content = await deepBoxRetrying(page, '.v2-panels .v2-content');
    expect(content.right, 'content ends at the screen edge').toBeCloseTo(innerWidth, 0);
    expect(content.paddingRight, 'the column itself stays unpadded').toBe(0);

    const header = await deepBoxRetrying(page, '.v2-thread-header');
    expect(header.right, 'thread header surface ends at the screen edge').toBeCloseTo(
      innerWidth,
      0
    );
    expect(header.borderRight, 'thread header right inset').toBe(insets.right);

    const composer = await deepBoxRetrying(page, 'scion-chat-composer', '.composer');
    expect(composer.right, 'composer surface ends at the screen edge').toBeCloseTo(innerWidth, 0);
    expect(composer.borderRight, 'composer right inset').toBe(insets.right);

    const messages = await deepBoxRetrying(page, 'scion-chat-thread', '.messages-list');
    expect(messages.right, 'message text clears the right inset').toBeLessThanOrEqual(
      innerWidth - insets.right + 0.5
    );
  }).toPass({ timeout: 5_000 });
  await assertDocumentStill(page);
}

/**
 * Mobile layout with a conversation open: the conversation panel itself is
 * unpadded, so its header and composer surfaces span the full screen width,
 * and each carries both side insets as transparent borders. The message
 * text clears both insets, and the document stays still.
 */
async function assertMobileConversationInsets(page: Page, insets: Insets): Promise<void> {
  const innerWidth = await page.evaluate(() => window.innerWidth);
  await expect(async () => {
    const content = await deepBoxRetrying(page, '.v2-panels .v2-content');
    expect(content.paddingLeft, 'content padding-left').toBe(0);
    expect(content.paddingRight, 'content padding-right').toBe(0);

    const header = await deepBoxRetrying(page, '.v2-thread-header');
    expect(header.left, 'thread header surface starts at the screen edge').toBeCloseTo(0, 0);
    expect(header.right, 'thread header surface ends at the screen edge').toBeCloseTo(
      innerWidth,
      0
    );
    expect(header.borderLeft, 'thread header left inset').toBe(insets.left);
    expect(header.borderRight, 'thread header right inset').toBe(insets.right);

    const composer = await deepBoxRetrying(page, 'scion-chat-composer', '.composer');
    expect(composer.left, 'composer surface starts at the screen edge').toBeCloseTo(0, 0);
    expect(composer.right, 'composer surface ends at the screen edge').toBeCloseTo(innerWidth, 0);
    expect(composer.borderLeft, 'composer left inset').toBe(insets.left);
    expect(composer.borderRight, 'composer right inset').toBe(insets.right);

    const messages = await deepBoxRetrying(page, 'scion-chat-thread', '.messages-list');
    expect(messages.left, 'message text clears the left inset').toBeGreaterThanOrEqual(
      insets.left - 0.5
    );
    expect(messages.right, 'message text clears the right inset').toBeLessThanOrEqual(
      innerWidth - insets.right + 0.5
    );
  }).toPass({ timeout: 5_000 });
  await assertDocumentStill(page);
}

/**
 * No visible content in the conversation panel sits inside a side inset.
 * Walks every element (through shadow roots) and checks the innermost
 * ones: elements with no child elements, and no shadow root or slot of
 * their own, so wrappers whose box spans a row are not counted. Rows that
 * carry an inset as a border are wrappers by this rule and skipped too.
 */
async function assertNoContentInInsets(page: Page, insets: Insets): Promise<void> {
  await expect(async () => {
    const offenders = await page.evaluate(
      ({ left, right }) => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('.v2-panels .v2-content');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            if (el.shadowRoot) {
              const nested = find(el.shadowRoot);
              if (nested) return nested;
            }
          }
          return null;
        };
        const content = find(document);
        if (!content) return ['.v2-content not found'];
        const width = window.innerWidth;
        const found: string[] = [];
        const walk = (root: ParentNode): void => {
          for (const el of root.querySelectorAll('*')) {
            if (!(el instanceof HTMLElement)) continue;
            if (el.shadowRoot) {
              walk(el.shadowRoot);
              continue;
            }
            if (el.children.length > 0 || el.tagName === 'SLOT') continue;
            const cs = getComputedStyle(el);
            const rect = el.getBoundingClientRect();
            const visible =
              cs.display !== 'none' &&
              cs.visibility !== 'hidden' &&
              rect.width > 0 &&
              rect.height > 0 &&
              rect.bottom > 0 &&
              rect.top < window.innerHeight;
            if (visible && (rect.left < left - 0.5 || rect.right > width - right + 0.5)) {
              found.push(
                `${el.tagName.toLowerCase()}.${el.className} ${rect.left.toFixed(0)}..${rect.right.toFixed(0)}`
              );
            }
          }
        };
        walk(content);
        return found;
      },
      { left: insets.left, right: insets.right }
    );
    expect(offenders, 'content inside a side inset').toEqual([]);
  }).toPass({ timeout: 5_000 });
}

test('@static with no insets the header and composer keep their usual spacing', async ({
  page,
}, testInfo) => {
  await openChatRail(page);
  await openGeneralThread(page);

  const header = await deepBoxRetrying(page, 'scion-header');
  expect(header.paddingTop, 'header padding-top').toBe(0);
  expect(header.height, 'header height').toBeCloseTo(HEADER_ROW_PX + HEADER_BORDER_PX, 0);
  // 1.5rem side padding at every width (the narrow tier keeps it).
  expect(header.paddingLeft, 'header padding-left').toBe(24);
  expect(header.paddingRight, 'header padding-right').toBe(24);

  const composer = await deepBoxRetrying(page, 'scion-chat-composer', '.composer');
  expect(composer.paddingBottom, 'composer padding-bottom').toBe(COMPOSER_PAD_PX);

  if (testInfo.project.name !== 'desktop-1440') {
    await assertFramePinned(page);
    await assertNoHorizontalOverflow(page);
  }
});

test('@static the frame module leaves the page alone when no keyboard is open', async ({
  page,
}) => {
  await openChatRail(page);
  await openGeneralThread(page);
  // Let a few animation frames run so any pending viewport sync has happened.
  await page.evaluate(
    () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
  );
  const state = await page.evaluate(() => ({
    inlineHeight: document.documentElement.style.getPropertyValue('--scion-app-height'),
    kbOpen: document.documentElement.style.getPropertyValue('--scion-kb-open'),
    keyboard: document.documentElement.dataset['keyboard'] ?? null,
  }));
  expect(state).toEqual({ inlineHeight: '', kbOpen: '', keyboard: null });
});

test('portrait insets pad the header top and the composer bottom', async ({ page }, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  await openChatRail(page);
  await openGeneralThread(page);
  await forceSafeAreaInsets(page, PORTRAIT);

  await expect(async () => {
    const header = await deepBoxRetrying(page, 'scion-header');
    expect(header.top, 'header stays at the top').toBeCloseTo(0, 0);
    expect(header.paddingTop, 'header padding-top = top inset').toBe(PORTRAIT.top);
    // The inset is added above the usual row, not taken out of it.
    expect(header.height, 'header height').toBeCloseTo(
      HEADER_ROW_PX + HEADER_BORDER_PX + PORTRAIT.top,
      0
    );
  }).toPass({ timeout: 5_000 });

  const composer = await deepBoxRetrying(page, 'scion-chat-composer', '.composer');
  expect(composer.paddingBottom, 'composer padding-bottom = bottom inset').toBe(PORTRAIT.bottom);

  if (testInfo.project.name !== 'desktop-1440') {
    await assertFramePinned(page);
    await assertNoHorizontalOverflow(page);
  }
});

test('with the keyboard open the composer drops the bottom inset', async ({ page }, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  await openChatRail(page);
  await openGeneralThread(page);
  await forceSafeAreaInsets(page, PORTRAIT);
  await expect
    .poll(
      async () => (await deepBoxRetrying(page, 'scion-chat-composer', '.composer')).paddingBottom
    )
    .toBe(PORTRAIT.bottom);

  // What client/viewport.ts sets on the root while the keyboard is open.
  await page.evaluate(() => document.documentElement.style.setProperty('--scion-kb-open', '1'));
  expect((await deepBoxRetrying(page, 'scion-chat-composer', '.composer')).paddingBottom).toBe(
    COMPOSER_PAD_PX
  );

  await page.evaluate(() => document.documentElement.style.removeProperty('--scion-kb-open'));
  expect((await deepBoxRetrying(page, 'scion-chat-composer', '.composer')).paddingBottom).toBe(
    PORTRAIT.bottom
  );
});

test('landscape side insets pad the header and the chat columns', async ({ page }, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  const mobile = testInfo.project.name !== 'desktop-1440';
  await openChatRail(page);
  await forceSafeAreaInsets(page, LANDSCAPE);

  await expect(async () => {
    const header = await deepBoxRetrying(page, 'scion-header');
    expect(header.paddingLeft, 'header padding-left').toBe(LANDSCAPE.left);
    expect(header.paddingRight, 'header padding-right').toBe(LANDSCAPE.right);
    expect(header.paddingTop, 'header padding-top').toBe(0);
  }).toPass({ timeout: 5_000 });

  if (mobile) {
    // The rail and members panels have one surface each, so they pad;
    // the conversation's rows take the insets as borders instead.
    for (const panel of ['.v2-rail', '.v2-members']) {
      const box = await deepBoxRetrying(page, `.v2-panels ${panel}`);
      expect(box.paddingLeft, `${panel} padding-left`).toBe(LANDSCAPE.left);
      expect(box.paddingRight, `${panel} padding-right`).toBe(LANDSCAPE.right);
    }
    await assertFramePinned(page);
    await assertNoHorizontalOverflow(page);
    await openGeneralThread(page);
    await assertMobileConversationInsets(page, LANDSCAPE);
  } else {
    await openGeneralThread(page);
    await assertSideBySideInsets(page, LANDSCAPE);
  }
});

/*
 * An iPhone in landscape is wider than the mobile breakpoint, so it gets
 * the side-by-side columns with real side insets. The viewport is set
 * here, so one Chromium project is enough.
 */
test.describe('side-by-side touch landscape (844x390)', () => {
  test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

  test('the edge columns carry the insets, with no overflow and a pinned frame', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openChatRail(page);
    await openGeneralThread(page);
    await forceSafeAreaInsets(page, LANDSCAPE);

    await expect(async () => {
      const header = await deepBoxRetrying(page, 'scion-header');
      expect(header.paddingLeft, 'header padding-left').toBe(LANDSCAPE.left);
      expect(header.paddingRight, 'header padding-right').toBe(LANDSCAPE.right);
    }).toPass({ timeout: 5_000 });
    await assertSideBySideInsets(page, LANDSCAPE);
    await assertFramePinned(page);
  });
});

/*
 * A landscape phone narrower than the breakpoint (a common Android size,
 * or an iPhone with Display Zoom) gets the mobile layout with real side
 * insets. The viewport is set here, so one Chromium project is enough.
 */
test.describe('mobile layout touch landscape (740x360)', () => {
  test.use({ viewport: { width: 740, height: 360 }, isMobile: true, hasTouch: true });

  test('the conversation rows reach both screen edges and carry both insets', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openChatRail(page);
    await openGeneralThread(page);
    await forceSafeAreaInsets(page, LANDSCAPE);

    await assertMobileConversationInsets(page, LANDSCAPE);
    await assertNoContentInInsets(page, LANDSCAPE);
    await assertFramePinned(page);

    // The search panel replaces the thread in the same column.
    await page.getByRole('button', { name: 'Search messages' }).click();
    await expect(async () => {
      const header = await deepBoxRetrying(page, 'scion-chat-search', '.search-header');
      expect(header.borderLeft, 'search header left inset').toBe(LANDSCAPE.left);
      expect(header.borderRight, 'search header right inset').toBe(LANDSCAPE.right);
    }).toPass({ timeout: 5_000 });
    await assertNoContentInInsets(page, LANDSCAPE);
  });
});

/*
 * Hiding the members panel side by side marks the conversation as the right
 * edge, and that choice outlives a rotation into the mobile layout. The
 * right inset must then still be applied once, not by both layouts' rules.
 */
test.describe('rotating from side by side into the mobile layout', () => {
  test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

  test('with members hidden, the right inset is applied exactly once', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openChatRail(page);
    await openGeneralThread(page);
    await forceSafeAreaInsets(page, LANDSCAPE);
    await page.getByRole('button', { name: 'Show/Hide members' }).click();
    await expect(async () => {
      const header = await deepBoxRetrying(page, '.v2-thread-header');
      expect(header.borderRight, 'side by side: header right inset').toBe(LANDSCAPE.right);
      expect(header.borderLeft, 'side by side: no left inset beside the rail').toBe(0);
    }).toPass({ timeout: 5_000 });

    await page.setViewportSize({ width: 740, height: 360 });
    await expect(async () => {
      const content = await deepBoxRetrying(page, '.v2-panels .v2-content');
      const header = await deepBoxRetrying(page, '.v2-thread-header');
      const composer = await deepBoxRetrying(page, 'scion-chat-composer', '.composer');
      expect(content.paddingRight + header.borderRight, 'header right inset, in total').toBe(
        LANDSCAPE.right
      );
      expect(content.paddingRight + composer.borderRight, 'composer right inset, in total').toBe(
        LANDSCAPE.right
      );
    }).toPass({ timeout: 5_000 });
    await assertMobileConversationInsets(page, LANDSCAPE);
    await assertFramePinned(page);
  });
});

test('the action sheet clears the home indicator and the side insets', async ({
  page,
}, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  await openChatRail(page);
  await forceSafeAreaInsets(page, LANDSCAPE);
  await page.evaluate(async () => {
    const sheet = document.createElement('scion-action-sheet') as HTMLElement & {
      items: Array<{ id: string; label: string }>;
      updateComplete: Promise<unknown>;
      show(): void;
    };
    sheet.setAttribute('data-safe-area-probe', '');
    sheet.items = [{ id: 'a', label: 'Probe' }];
    document.body.appendChild(sheet);
    await sheet.updateComplete;
    sheet.show();
  });
  try {
    const dialog = await deepBoxRetrying(
      page,
      'scion-action-sheet[data-safe-area-probe]',
      'dialog'
    );
    expect(dialog.paddingBottom, 'sheet padding-bottom').toBe(LANDSCAPE.bottom);
    expect(dialog.paddingLeft, 'sheet padding-left').toBe(LANDSCAPE.left);
    expect(dialog.paddingRight, 'sheet padding-right').toBe(LANDSCAPE.right);
  } finally {
    await page.evaluate(() => document.querySelector('[data-safe-area-probe]')?.remove());
  }
});

test('app-shell content clears the home indicator', async ({ page }, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  await openChatRail(page); // installs the mocks the app-shell route also needs
  await page.goto('/agents', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('scion-header')).toBeVisible({ timeout: 10_000 });

  const before = await deepBoxRetrying(page, 'scion-app', '.content');
  // 1.5rem above 640px, 1rem at phone widths.
  const usual = testInfo.project.name === 'desktop-1440' ? 24 : 16;
  expect(before.paddingBottom, '.content padding-bottom with no inset').toBe(usual);

  await forceSafeAreaInsets(page, PORTRAIT);
  await expect
    .poll(async () => (await deepBoxRetrying(page, 'scion-app', '.content')).paddingBottom)
    .toBe(PORTRAIT.bottom);
});

test('in frame mode a window scroll is reset to the top', async ({ page }, testInfo) => {
  skipUnlessChromium(testInfo.project.name);
  await openChatRail(page);
  // Make the frame-mode document genuinely taller than the viewport, so a
  // programmatic scroll succeeds and only the frame module can undo it.
  await page.addStyleTag({ content: 'html.scion-app-frame { height: 300% !important; }' });
  const scrolledTo = await page.evaluate(() => {
    window.scrollTo(0, 400);
    return window.scrollY;
  });
  expect(scrolledTo, 'the probe scroll itself landed').toBeGreaterThan(0);
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0);
});
