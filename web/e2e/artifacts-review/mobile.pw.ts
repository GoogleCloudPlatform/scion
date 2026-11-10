/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The graphical review on a phone (ptone/scion#6929): in a long document
 * the review toolbar stays pinned to the top of the content area, Comment
 * there opens the note dialog, a caret moved under the toolbar is scrolled
 * clear of it, and a refusal hint stays in view with the toolbar.
 *
 * Phone viewports and touch are emulated (Pixel and iPhone), in whichever
 * engine runs the suite. Firefox cannot emulate a mobile browser (isMobile),
 * so there it runs with the phone's viewport and touch only. A desktop-width
 * case checks the same pinning, caret and focus clearance with a mouse.
 * Emulation shows no soft keyboard: how a pinned toolbar behaves with the
 * iOS keyboard open is a device check. What emulation can check is that no
 * field typed in during a review is under 16px on a touch screen, the size
 * below which iOS zooms in on focus and leaves the toolbar out of view.
 */

import { devices, expect, test } from '@playwright/test';
import type { Locator, Page } from '@playwright/test';

import {
  createProject,
  editorText,
  openArtifact,
  publish,
  setExperiments,
  startGraphicalReview,
} from './fixtures.js';

/**
 * A device's viewport, touch and user agent, without its default engine
 * (the suite's project picks the engine).
 */
function phone(name: string): Omit<(typeof devices)[string], 'defaultBrowserType'> {
  const { defaultBrowserType: _engine, ...rest } = devices[name];
  return rest;
}

const PARAGRAPHS = 60;
const DOC = [
  '# A long review',
  '',
  ...Array.from({ length: PARAGRAPHS }, (_, i) => [`Paragraph ${i + 1} line.`, '']).flat(),
].join('\n');

const editor = (page: Page): Locator => page.locator('scion-review-editor .ProseMirror');
const toolbar = (page: Page): Locator =>
  page.locator('scion-page-artifact-detail .g-sticky .g-toolbar');
const sticky = (page: Page): Locator => page.locator('scion-page-artifact-detail .g-sticky');
const hint = (page: Page): Locator =>
  page.locator('scion-page-artifact-detail .g-sticky .hint-strip');

/**
 * Scrolls the page's scroll container (the app shell's content area,
 * through the shadow roots) so paragraph n sits under the pinned toolbar.
 * Returns the container's top edge in viewport pixels.
 */
async function scrollUnderToolbar(page: Page, n: number): Promise<number> {
  return page.locator('scion-review-editor').evaluate((host, n) => {
    let scroller: Element | null = null;
    for (let node: Node | null = host; node; ) {
      if (node instanceof Element) {
        const o = getComputedStyle(node).overflowY;
        if ((o === 'auto' || o === 'scroll') && node.scrollHeight > node.clientHeight) {
          scroller = node;
          break;
        }
      }
      // Up the flattened tree: slotted content into its slot, a shadow
      // root to its host.
      node =
        (node instanceof Element ? node.assignedSlot : null) ??
        node.parentNode ??
        (node instanceof ShadowRoot ? node.host : null);
    }
    if (!scroller) throw new Error('no scroll container');
    const p = Array.from(host.shadowRoot!.querySelectorAll('.ProseMirror p')).find(
      (e) => e.textContent === `Paragraph ${n} line.`
    )!;
    const top = scroller.getBoundingClientRect().top;
    scroller.scrollTop += p.getBoundingClientRect().top - top - 4;
    return top;
  }, n);
}

/** The viewport rectangle of text in the editor. */
async function textBox(page: Page, text: string): Promise<{ x: number; y: number; top: number }> {
  const box = await page.locator('scion-review-editor').evaluate((host, t) => {
    const root = host.shadowRoot!.querySelector('.ProseMirror')!;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const i = n.textContent!.indexOf(t);
      if (i < 0) continue;
      const r = document.createRange();
      r.setStart(n, i);
      r.setEnd(n, i + t.length);
      const b = r.getBoundingClientRect();
      return { x: b.x + 2, y: b.y + b.height / 2, top: b.top };
    }
    return null;
  }, text);
  if (!box) throw new Error(`${text} not in the editor`);
  return box;
}

/** The top edge of the review toolbar, in viewport pixels, rounded. */
async function toolbarTop(page: Page): Promise<number> {
  return toolbar(page).evaluate((e) => Math.round(e.getBoundingClientRect().top));
}

/** The bottom edge of the pinned block, in viewport pixels. */
async function stickyBottom(page: Page): Promise<number> {
  return sticky(page).evaluate((e) => e.getBoundingClientRect().bottom);
}

/** The computed font size, in px, of the element at selector in host's shadow root. */
async function shadowFontPx(host: Locator, selector: string): Promise<number> {
  return host.evaluate(
    (e, sel) => Number.parseFloat(getComputedStyle(e.shadowRoot!.querySelector(sel)!).fontSize),
    selector
  );
}

/** Whether the page's primary pointer is coarse (a touch screen). */
async function coarsePointer(page: Page): Promise<boolean> {
  return page.evaluate(() => matchMedia('(pointer: coarse)').matches);
}

/**
 * The page's visual viewport and where the pinned toolbar and its Comment
 * button sit in it, in CSS px.
 */
async function pinnedInView(page: Page): Promise<{
  scale: number;
  offsetTop: number;
  toolbarTop: number;
  commentTop: number;
  commentBottom: number;
  viewportHeight: number;
}> {
  return toolbar(page).evaluate((bar) => {
    const vv = window.visualViewport!;
    const comment = bar.querySelector('sl-button.comment')!.getBoundingClientRect();
    return {
      scale: vv.scale,
      offsetTop: vv.offsetTop,
      // Layout-viewport coordinates, made relative to the visual viewport.
      toolbarTop: bar.getBoundingClientRect().top - vv.offsetTop,
      commentTop: comment.top - vv.offsetTop,
      commentBottom: comment.bottom - vv.offsetTop,
      viewportHeight: vv.height,
    };
  });
}

/**
 * The keyboard flow of the pinned header bug: every place text is typed in
 * a review is 16px or more on a touch screen, so iOS Safari never zooms in
 * on focus (and so never leaves the page zoomed, with the toolbar out of
 * view, once the keyboard closes); and after typing in the note dialog,
 * closing it and scrolling on, the toolbar and Comment are still in view.
 * Emulation shows no soft keyboard and does not zoom on focus: the font
 * sizes are what decide whether iOS zooms.
 */
async function keyboardHideJourney(page: Page, title: string, engine: string): Promise<void> {
  const a = await publish({
    project,
    title,
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  const coarse = await coarsePointer(page);
  // Chromium's phone emulation reports a coarse pointer, so there the 16px
  // checks cannot pass vacuously. Firefox (no isMobile) and WebKit are not
  // held to it here; where they report a fine pointer the floor does not
  // apply and is not checked.
  if (engine === 'chromium') expect(coarse, 'phone emulation has a coarse pointer').toBe(true);
  const floor = (px: number, what: string): void => {
    if (coarse) expect(px, what).toBeGreaterThanOrEqual(16);
  };

  floor(await shadowFontPx(page.locator('scion-review-editor'), '.ProseMirror'), 'review text');

  // Select a word below the pinned block and open the note dialog.
  await scrollUnderToolbar(page, 20);
  const target = await firstClear(page, 21);
  const word = await textBox(page, `Paragraph ${target} line.`);
  await page.touchscreen.tap(word.x, word.y);
  await expect(editor(page)).toBeFocused();
  await settleAfterFocus(page);
  await page.keyboard.press('Home');
  for (let i = 0; i < 'Paragraph'.length; i++) await page.keyboard.press('Shift+ArrowRight');
  await toolbar(page).locator('sl-button.comment').tap();
  const dialog = page.locator('scion-review-note-dialog sl-dialog');
  await expect(dialog).toHaveAttribute('open', '');
  const note = page.locator('scion-review-note-dialog textarea');
  await expect(note).toBeFocused();
  floor(await shadowFontPx(page.locator('scion-review-note-dialog'), 'textarea'), 'note');
  await page.keyboard.type('Keyboard');

  // Save, which blurs the field and so hides the keyboard on a phone.
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  await expect(note).not.toBeFocused();
  expect(await editorText(page)).toContain(`{==Paragraph==}{>>Keyboard<<} ${target} line.`);

  // Scroll on: the toolbar is pinned at the top of the visible area, not
  // zoomed or panned above it, and Comment is fully in view.
  await scrollUnderToolbar(page, 45);
  await expect(toolbar(page)).toBeInViewport();
  const view = await pinnedInView(page);
  expect(view.scale).toBeCloseTo(1, 2);
  expect(view.offsetTop).toBeCloseTo(0, 0);
  expect(view.toolbarTop).toBeGreaterThanOrEqual(0);
  expect(view.commentTop).toBeGreaterThanOrEqual(0);
  expect(view.commentBottom).toBeLessThanOrEqual(view.viewportHeight);

  // The Source view's editor is typed in too.
  await page
    .locator('scion-page-artifact-detail .view-toggle sl-radio-button[value="source"]')
    .click();
  const source = page.locator('scion-page-artifact-detail scion-code-editor.review-editor');
  await expect(source).toBeVisible();
  await expect
    .poll(() => source.evaluate((e) => !!e.shadowRoot?.querySelector('.cm-content')))
    .toBe(true);
  // The element that takes focus, and so decides whether iOS zooms, is
  // .cm-content, not the .cm-editor wrapper around it.
  const typed = await source.evaluate((e) => {
    const content = e.shadowRoot!.querySelector<HTMLElement>('.cm-content')!;
    content.focus();
    return {
      focused: e.shadowRoot!.activeElement === content,
      px: Number.parseFloat(getComputedStyle(content).fontSize),
    };
  });
  expect(typed.focused, 'the source text takes focus').toBe(true);
  floor(typed.px, 'source text');
}

let project = '';
const errors: string[] = [];

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  project = await createProject(`e2e-review-mobile-${Date.now().toString(36)}`);
});

test.afterAll(async () => {
  await setExperiments(null);
});

test.beforeEach(({ page }) => {
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    if (m.text().includes('review editor') || m.location().url.includes('review-editor')) {
      errors.push(`console: ${m.text()}`);
    }
  });
});

test.afterEach(() => {
  const found = errors.splice(0);
  expect(found, 'no page errors or console errors').toEqual([]);
});

/**
 * Waits out the editor's settling after it takes focus. On focus,
 * prosemirror-view (handlers.focus) schedules a 20ms timer that writes its
 * own selection back to the DOM (selectionToDOM) if the two differ, and it
 * reads selectionchange asynchronously; a key pressed sooner (a test's
 * speed, not a person's) can be undone by that write. Waits until the
 * selection has been quiet, with no selectionchange, for longer than that
 * timer: a write-back fires selectionchange and restarts the wait.
 */
async function settleAfterFocus(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        const QUIET_MS = 30; // longer than prosemirror-view's 20ms focus timer
        let timer = setTimeout(done, QUIET_MS);
        const giveUp = setTimeout(done, 2000);
        function onChange(): void {
          clearTimeout(timer);
          timer = setTimeout(done, QUIET_MS);
        }
        function done(): void {
          clearTimeout(timer);
          clearTimeout(giveUp);
          document.removeEventListener('selectionchange', onChange);
          resolve();
        }
        document.addEventListener('selectionchange', onChange);
      })
  );
}

/** The first paragraph from n on whose text is fully below the pinned block. */
async function firstClear(page: Page, n: number): Promise<number> {
  const below = await stickyBottom(page);
  while ((await textBox(page, `Paragraph ${n} line.`)).top < below + 2) n++;
  return n;
}

/**
 * Scrolls paragraph n, which holds the comment badge, under the pinned
 * toolbar and moves keyboard focus to the badge from elsewhere (as Tab or
 * Shift+Tab does; sequential focus through the nested shadow roots is not
 * reliable under emulation). The badge is inside the scrolled view but
 * under the toolbar, so only its scroll-margin tells the browser to scroll
 * it clear. Returns the badge's top and the pinned block's bottom edge.
 */
async function focusBadgeUnderToolbar(
  page: Page,
  n: number
): Promise<{ badge: number; bar: number }> {
  const badge = editor(page).locator('.critic-comment').first();
  await page.evaluate(() => {
    let a: Element | null = document.activeElement;
    while (a?.shadowRoot?.activeElement) a = a.shadowRoot.activeElement;
    (a as HTMLElement | null)?.blur();
  });
  await expect(badge).not.toBeFocused();
  // Blurring closes the comment card: let that render finish first.
  await page
    .locator('scion-review-editor')
    .evaluate((e) => (e as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete);
  await scrollUnderToolbar(page, n);
  expect(await badge.evaluate((e) => e.getBoundingClientRect().top)).toBeLessThan(
    await stickyBottom(page)
  );
  await badge.focus();
  await expect(badge).toBeFocused();
  return {
    badge: await badge.evaluate((e) => e.getBoundingClientRect().top),
    bar: await stickyBottom(page),
  };
}

/**
 * The journey: the toolbar pins flush at the top while the document scrolls
 * under it, Comment there opens the note dialog, a caret moved up under the
 * toolbar is scrolled clear of it, a refusal hint shows with the toolbar,
 * and a comment badge given keyboard focus lands below the toolbar.
 */
async function pinnedJourney(page: Page, title: string, touch: boolean): Promise<void> {
  const a = await publish({
    project,
    title,
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  // One toolbar, never a second, pinned copy.
  await expect(page.locator('scion-page-artifact-detail [role="toolbar"]')).toHaveCount(1);
  const comment = toolbar(page).locator('sl-button.comment');
  /** Taps (or clicks) into the editor and waits until keys apply there. */
  const tapIntoEditor = async (x: number, y: number): Promise<void> => {
    await (touch ? page.touchscreen.tap(x, y) : page.mouse.click(x, y));
    await expect(editor(page)).toBeFocused();
    await settleAfterFocus(page);
  };

  // Scrolled well down, the toolbar sits flush at the top of the content area.
  const scrollerTop = await scrollUnderToolbar(page, 30);
  await expect.poll(() => toolbarTop(page)).toBe(Math.round(scrollerTop));
  await expect(editor(page).locator('h1')).not.toBeInViewport();
  await expect(comment).toBeInViewport();
  if (touch) {
    // A tap target of at least 44px.
    const height = await comment.evaluate((e) => e.getBoundingClientRect().height);
    expect(height).toBeGreaterThanOrEqual(44);
  }

  // Select by tap and keyboard, then use the pinned Comment.
  const target = await firstClear(page, 31);
  const word = await textBox(page, `Paragraph ${target} line.`);
  await tapIntoEditor(word.x, word.y);
  await page.keyboard.press('Home');
  for (let i = 0; i < 'Paragraph'.length; i++) await page.keyboard.press('Shift+ArrowRight');
  await (touch ? comment.tap() : comment.click());
  const dialog = page.locator('scion-review-note-dialog sl-dialog');
  await expect(dialog).toHaveAttribute('open', '');
  await expect(page.locator('scion-review-note-dialog .quote')).toHaveText('Paragraph');
  await page.locator('scion-review-note-dialog textarea').fill('Pinned');
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  expect(await editorText(page)).toContain(`{==Paragraph==}{>>Pinned<<} ${target} line.`);

  // A caret moved up under the toolbar is scrolled clear of it.
  await scrollUnderToolbar(page, 40);
  const n = await firstClear(page, 40);
  const start = await textBox(page, `Paragraph ${n} line.`);
  await tapIntoEditor(start.x, start.y);
  await page.keyboard.press('Home');
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('ArrowUp');
  await page.keyboard.type('ZQ');
  await expect.poll(() => editorText(page)).toContain(`{++ZQ++}Paragraph ${n - 2} line.`);
  const typed = await textBox(page, 'ZQ');
  expect(typed.top).toBeGreaterThanOrEqual(await stickyBottom(page));

  // Keyboard focus scrolled into view is not hidden by the toolbar (WCAG
  // 2.4.11): a comment badge under the toolbar is scrolled clear of it
  // when it takes focus.
  const cleared = await focusBadgeUnderToolbar(page, target);
  expect(cleared.badge).toBeGreaterThanOrEqual(cleared.bar);
  // Control: with the editor's inset at 0 (its one input), the same focus
  // stays under the toolbar. The page sets it back when the toolbar next
  // changes size (the hint below).
  await page.locator('scion-review-editor').evaluate(async (e) => {
    const el = e as HTMLElement & { scrollInset: number; updateComplete: Promise<boolean> };
    el.scrollInset = 0;
    await el.updateComplete;
  });
  const hidden = await focusBadgeUnderToolbar(page, target);
  expect(hidden.badge).toBeLessThan(hidden.bar);
  // Back into the text for the hint check (after the commented paragraph,
  // whose text the highlight splits).
  const again = await textBox(page, `Paragraph ${await firstClear(page, target + 1)} line.`);
  await tapIntoEditor(again.x, again.y);

  // A refusal hint shows in the pinned block, in view.
  await page.keyboard.press('End');
  await page.keyboard.press('Enter');
  await expect(hint(page)).toContainText("Paragraph breaks can't be suggested here yet.");
  await expect(hint(page)).toBeInViewport();
  await expect(toolbar(page)).toBeInViewport();
}

for (const device of ['Pixel 7', 'iPhone 13']) {
  test.describe(device, () => {
    const emulated = phone(device);
    test.use({
      ...emulated,
      // Firefox has no isMobile: it runs with the phone's viewport and touch.
      isMobile: async ({ browserName }, use) => {
        await use(browserName !== 'firefox' && emulated.isMobile);
      },
    });

    test('the review toolbar stays pinned while the document scrolls under it', async ({
      page,
    }) => {
      await pinnedJourney(page, `Mobile ${device}`, true);
    });

    test('the toolbar stays in view after typing a comment and hiding the keyboard', async ({
      page,
      browserName,
    }) => {
      await keyboardHideJourney(page, `Keyboard ${device}`, browserName);
    });
  });
}

test.describe('desktop width', () => {
  test('the review toolbar stays pinned flush with the top', async ({ page }) => {
    await pinnedJourney(page, 'Desktop', false);
  });
});
