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
 * Focusing the composer while its panel is off-screen must never scroll
 * `.v2-panels` sideways — that used to drift the swipe track permanently,
 * leaving the conversation shifted left with the members panel peeking in
 * from the right once the user swiped back. Covers the three ways a focus
 * call can land on the off-screen composer: a direct `focus()` call, the
 * quick switcher's post-selection focus restore, and the composer's own
 * reply/cancel-reply focus.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';
import { touchSwipe, assertNoHorizontalOverflow } from './helpers.js';

/** Swipe from the centre panel back to the rail, settle, and confirm we landed there. */
async function swipeToLeftFromCenter(page: Page): Promise<{ width: number; y: number }> {
  const vp = page.viewportSize()!;
  const y = vp.height / 2;
  await touchSwipe(page, vp.width / 2, y, vp.width, y, 8, 150);
  await page.waitForTimeout(400);
  expect(await currentPanel(page)).toBe('left');
  return { width: vp.width, y };
}

/** Swipe from the rail back to the centre panel and assert no overflow stuck. */
async function swipeBackToCenterAndAssertClean(
  page: Page,
  width: number,
  y: number
): Promise<void> {
  await touchSwipe(page, width / 2, y, 0, y, 8, 150);
  await page.waitForTimeout(400);
  expect(await currentPanel(page)).toBe('center');
  await assertNoHorizontalOverflow(page);
}

test('focusing the off-screen composer directly does not drift the swipe track', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'desktop-1440',
    'off-screen/inert panels and the swipe track are mobile-only'
  );
  await openChatRail(page);
  await openGeneralThread(page); // mobilePanel -> 'center', composer mounts
  const { width, y } = await swipeToLeftFromCenter(page);

  const result = await page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const content = pageEl?.shadowRoot?.querySelector('.v2-content') as HTMLElement | null;
    const thread = content?.querySelector('scion-chat-thread') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const composer = thread?.shadowRoot?.querySelector('scion-chat-composer') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const textarea = composer?.shadowRoot?.querySelector('sl-textarea') as HTMLElement | null;
    textarea?.focus();
    const panels = pageEl?.shadowRoot?.querySelector('.v2-panels') as HTMLElement | null;
    return {
      textareaFound: textarea !== null,
      contentIsInert: content?.hasAttribute('inert') ?? null,
      panelsScrollLeft: panels?.scrollLeft ?? null,
      focusLandedOnTextarea: ((): boolean => {
        let el: Element | null = document.activeElement;
        while (el && (el as HTMLElement).shadowRoot?.activeElement) {
          el = (el as HTMLElement).shadowRoot!.activeElement;
        }
        return el === textarea;
      })(),
    };
  });

  expect(result.textareaFound, 'composer sl-textarea was found').toBe(true);
  expect(result.contentIsInert, 'off-screen centre panel is inert').toBe(true);
  // inert excludes the subtree from focus — the browser refuses the
  // programmatic focus() call, which is half of the fix for this drift.
  expect(result.focusLandedOnTextarea, 'focus() on an inert composer is a no-op').toBe(false);
  expect(result.panelsScrollLeft, '.v2-panels.scrollLeft stays 0').toBe(0);

  await swipeBackToCenterAndAssertClean(page, width, y);
});

test('@static focusing the off-screen composer directly does not scroll the panel track', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'off-screen/inert panels are mobile-only');
  // A WebKit-safe variant of the same check above, without the CDP touch
  // swipe: drive `mobilePanel` directly, the same way the Tab-focus check
  // does, so this runs on engines without the Chromium-only touch helpers.
  await openChatRail(page);
  await openGeneralThread(page);
  await page.evaluate(() => {
    (document.querySelector('scion-page-chat') as unknown as { mobilePanel: string }).mobilePanel =
      'left';
  });
  await page.waitForTimeout(400);
  expect(await currentPanel(page)).toBe('left');

  const result = await page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const content = pageEl?.shadowRoot?.querySelector('.v2-content') as HTMLElement | null;
    const thread = content?.querySelector('scion-chat-thread') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const composer = thread?.shadowRoot?.querySelector('scion-chat-composer') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const textarea = composer?.shadowRoot?.querySelector('sl-textarea') as HTMLElement | null;
    textarea?.focus();
    const panels = pageEl?.shadowRoot?.querySelector('.v2-panels') as HTMLElement | null;
    return { textareaFound: textarea !== null, panelsScrollLeft: panels?.scrollLeft ?? null };
  });
  expect(result.textareaFound, 'composer sl-textarea was found').toBe(true);
  expect(result.panelsScrollLeft, '.v2-panels.scrollLeft stays 0').toBe(0);
});

test('the quick-switcher post-selection focus does not drift the swipe track', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'desktop-1440',
    'off-screen/inert panels and the swipe track are mobile-only'
  );
  await openChatRail(page);
  await openGeneralThread(page);
  const { width, y } = await swipeToLeftFromCenter(page);

  // Calling the page's own post-selection focus method directly exercises
  // the same focus() call against the same off-screen, inert composer that
  // a real quick-switcher selection would produce, without driving the
  // palette's open/close UI end to end.
  const scrollLeft = await page.evaluate(async () => {
    const pageEl = document.querySelector('scion-page-chat') as unknown as {
      _focusComposerAfterPaletteSelection: () => Promise<void>;
      shadowRoot: ShadowRoot;
    };
    await pageEl._focusComposerAfterPaletteSelection();
    const panels = pageEl.shadowRoot.querySelector('.v2-panels');
    return panels?.scrollLeft ?? null;
  });
  expect(scrollLeft, '.v2-panels.scrollLeft stays 0 after the post-selection focus call').toBe(0);

  await swipeBackToCenterAndAssertClean(page, width, y);
});

test('composer reply and cancel-reply focus do not drift the swipe track', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'desktop-1440',
    'off-screen/inert panels and the swipe track are mobile-only'
  );
  await openChatRail(page);
  await openGeneralThread(page);
  const { width, y } = await swipeToLeftFromCenter(page);

  async function panelsScrollLeftAfter(action: 'reply' | 'cancel'): Promise<number | null> {
    return page.evaluate(async (which) => {
      const pageEl = document.querySelector('scion-page-chat') as HTMLElement & {
        shadowRoot: ShadowRoot;
      };
      const content = pageEl.shadowRoot.querySelector('.v2-content');
      const thread = content?.querySelector('scion-chat-thread') as
        | (HTMLElement & { shadowRoot: ShadowRoot })
        | null;
      const composer = thread?.shadowRoot?.querySelector('scion-chat-composer') as unknown as {
        replyTo: { messageId: string; senderName: string; content: string } | null;
        cancelReply: () => void;
        updateComplete: Promise<boolean>;
      } | null;
      if (!composer) return null;
      if (which === 'reply') {
        // Parent-owned property (the thread component sets this the same
        // way on a real Reply tap) — setting it fires the composer's own
        // reply-focus path.
        composer.replyTo = { messageId: 'm1', senderName: 'Ada Lovelace', content: 'hi' };
      } else {
        // The reply bar's own Cancel ("x") button calls this directly.
        composer.cancelReply();
      }
      await composer.updateComplete;
      // The composer's focus call defers via updateComplete.then() plus a
      // requestAnimationFrame — flush two frames so that has run.
      await new Promise<void>((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
      );
      const panels = pageEl.shadowRoot.querySelector('.v2-panels');
      return panels?.scrollLeft ?? null;
    }, action);
  }

  expect(
    await panelsScrollLeftAfter('reply'),
    '.v2-panels.scrollLeft stays 0 after setting replyTo'
  ).toBe(0);
  expect(
    await panelsScrollLeftAfter('cancel'),
    '.v2-panels.scrollLeft stays 0 after cancelReply()'
  ).toBe(0);

  await swipeBackToCenterAndAssertClean(page, width, y);
});
