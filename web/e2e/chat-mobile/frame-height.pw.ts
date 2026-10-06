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
 * Viewport-relative caps follow the app frame height, not `100vh`.
 *
 * `--scion-app-height` is the frame height: the dynamic viewport normally,
 * and the visual viewport while the iOS keyboard is open (set inline on the
 * root by `client/viewport.ts`). A cap written against `100vh` ignores both
 * and lets a dialog or scroller run past the frame. These tests set the
 * custom property to a height well short of the viewport, as the keyboard
 * would, mount each component through the dev server, and check that its
 * cap and its box follow the frame.
 */

import { test, expect, type Page } from '@playwright/test';
import { expandSpace, openChatRail, openGeneralThread } from './fixture.js';
import { GENERAL_THREAD_ID } from './mock-api.js';
import { assertNoHorizontalOverflow } from './helpers.js';

const REM = 16;

/** Shrink the frame the way the open keyboard does, and return its height. */
async function shrinkFrame(page: Page): Promise<number> {
  const vh = page.viewportSize()?.height ?? 812;
  // Short of the viewport by a keyboard's worth, yet tall enough that the
  // largest offset below (22rem) still leaves a positive cap.
  const frame = vh - 150;
  await page.evaluate((px) => {
    document.documentElement.style.setProperty('--scion-app-height', `${px}px`);
  }, frame);
  return frame;
}

/** Mount `tag` from `modulePath` at the end of the body, and wait for its first render. */
async function mount(page: Page, modulePath: string, tag: string, props: Record<string, unknown>) {
  await page.evaluate(
    async ({ modulePath, tag, props }) => {
      await import(/* @vite-ignore */ modulePath);
      await customElements.whenDefined(tag);
      const el = document.createElement(tag) as HTMLElement & {
        updateComplete?: Promise<unknown>;
      };
      el.setAttribute('data-frame-probe', '');
      Object.assign(el, props);
      // A fixed, frame-sized host keeps the probe from adding to the page.
      const host = document.createElement('div');
      host.style.cssText = 'position: fixed; inset: 0; overflow: auto; z-index: 10000;';
      host.appendChild(el);
      document.body.appendChild(host);
      await el.updateComplete;
    },
    { modulePath, tag, props }
  );
}

/** Computed `max-height` in px of `inner` inside the probe's shadow root. */
async function probeMaxHeight(page: Page, inner: string): Promise<number> {
  await page.waitForFunction(
    (sel) => !!document.querySelector('[data-frame-probe]')?.shadowRoot?.querySelector(sel),
    inner
  );
  return page.evaluate((sel) => {
    const el = document.querySelector('[data-frame-probe]')!.shadowRoot!.querySelector(sel)!;
    return parseFloat(getComputedStyle(el).maxHeight);
  }, inner);
}

test.describe('viewport-relative caps follow the app frame', () => {
  test.beforeEach(async ({ page }) => {
    await openChatRail(page);
  });

  test('the security review dialog fits the frame on mobile', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'the full-height layout is mobile-only');
    const frame = await shrinkFrame(page);
    const boundaries = Array.from({ length: 30 }, (_, i) => ({
      boundaryId: `b${i}`,
      name: `boundary-with-a-fairly-long-name-${i}`,
      principalCount: 3,
      principalLabel: 'members',
      permissionCount: 12,
    }));
    await mount(
      page,
      '/src/components/shared/security-review-dialog.ts',
      'scion-security-review-dialog',
      {
        open: true,
        detail: {
          entityLabel: 'agent mobile-layout-test',
          contextLabel: 'project mobile-layout-test-project-with-a-long-name',
          boundaries,
          canCommit: true,
        },
      }
    );
    await page.waitForFunction(() => {
      const dlg = document
        .querySelector('[data-frame-probe]')
        ?.shadowRoot?.querySelector('sl-dialog');
      const panel = dlg?.shadowRoot?.querySelector('[part~="panel"]');
      return !!panel && panel.getBoundingClientRect().height > 0;
    });
    // Let the open animation finish before measuring.
    await page.waitForTimeout(400);
    const box = await page.evaluate(() => {
      const dlg = document
        .querySelector('[data-frame-probe]')!
        .shadowRoot!.querySelector('sl-dialog')!;
      const panel = dlg.shadowRoot!.querySelector('[part~="panel"]')!;
      const body = dlg.shadowRoot!.querySelector('[part~="body"]')!;
      return {
        panelHeight: panel.getBoundingClientRect().height,
        panelMax: parseFloat(getComputedStyle(panel).maxHeight),
        bodyMax: parseFloat(getComputedStyle(body).maxHeight),
        bodyScrolls: body.scrollHeight > body.clientHeight,
      };
    });
    expect(box.panelMax, 'panel cap is the frame height').toBeCloseTo(frame, 0);
    expect(box.bodyMax, 'body cap is the frame height less the header and footer').toBeCloseTo(
      frame - 8 * REM,
      0
    );
    expect(box.panelHeight, 'the panel fits in the frame').toBeLessThanOrEqual(frame + 1);
    expect(box.bodyScrolls, 'the long boundary list scrolls inside the body').toBe(true);
    await assertNoHorizontalOverflow(page);
  });

  test('the markdown preview caps against the frame', async ({ page }, testInfo) => {
    const frame = await shrinkFrame(page);
    await mount(page, '/src/components/shared/markdown-preview.ts', 'scion-markdown-preview', {
      content: Array.from({ length: 80 }, (_, i) => `Paragraph ${i}`).join('\n\n'),
    });
    expect(await probeMaxHeight(page, '.preview-container')).toBeCloseTo(frame - 16 * REM, 0);
    if (testInfo.project.name !== 'desktop-1440') await assertNoHorizontalOverflow(page);
  });

  test('the code editor caps against the frame', async ({ page }, testInfo) => {
    const frame = await shrinkFrame(page);
    await mount(page, '/src/components/shared/code-editor.ts', 'scion-code-editor', {
      content: Array.from({ length: 80 }, (_, i) => `line ${i}`).join('\n'),
      language: 'plaintext',
    });
    expect(await probeMaxHeight(page, '.cm-editor')).toBeCloseTo(frame - 16 * REM, 0);
    if (testInfo.project.name !== 'desktop-1440') await assertNoHorizontalOverflow(page);
  });

  test('the log viewer caps against the frame', async ({ page }) => {
    // An empty log, and a live stream that stays open with nothing to say.
    await page.route('**/api/v1/admin/diagnostics/logs/stream**', () => {});
    await page.route('**/api/v1/admin/diagnostics/logs?**', (route) =>
      route.fulfill({ json: { entries: [] } })
    );
    const frame = await shrinkFrame(page);
    await mount(
      page,
      '/src/components/shared/unified-log-viewer.ts',
      'scion-unified-log-viewer',
      {}
    );
    expect(await probeMaxHeight(page, '.log-scroller')).toBeCloseTo(frame - 22 * REM, 0);
  });
});

/** Top and bottom of the composer's text field and of the chat shell, in CSS px. */
async function composerAndFrame(page: Page) {
  return page.evaluate(() => {
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
    const composer = find(document, 'scion-chat-composer')!.getBoundingClientRect();
    const field = find(document, 'scion-chat-composer textarea, textarea')!.getBoundingClientRect();
    const shell = document.querySelector('scion-chat-shell')!.getBoundingClientRect();
    return {
      composerBottom: composer.bottom,
      fieldTop: field.top,
      fieldBottom: field.bottom,
      frameBottom: shell.bottom,
    };
  });
}

test.describe('a short frame keeps the composer on screen', () => {
  test('with the keyboard open on a small phone', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'the keyboard frame is a phone case');
    await openChatRail(page);
    await openGeneralThread(page);
    // A 568px-tall phone with a 270px keyboard leaves about 300px.
    await page.evaluate(() => {
      document.documentElement.style.setProperty('--scion-app-height', '300px');
    });
    await expect.poll(async () => (await composerAndFrame(page)).frameBottom).toBeCloseTo(300, 0);
    const box = await composerAndFrame(page);
    expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(
      box.frameBottom + 0.5
    );
    expect(box.fieldBottom, 'text field inside the frame').toBeLessThanOrEqual(
      box.frameBottom + 0.5
    );
  });

  test.describe('touch landscape (740x360)', () => {
    test.use({ viewport: { width: 740, height: 360 }, isMobile: true, hasTouch: true });

    test('the composer stays inside the frame', async ({ page }, testInfo) => {
      test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
      await openChatRail(page);
      await openGeneralThread(page);
      const box = await composerAndFrame(page);
      expect(box.frameBottom).toBeCloseTo(360, 0);
      expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(
        box.frameBottom + 0.5
      );
      expect(box.fieldBottom, 'text field inside the frame').toBeLessThanOrEqual(
        box.frameBottom + 0.5
      );
    });
  });

  test.describe('touch landscape with a notch (844x390)', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    /** A landscape notch on both sides and the home indicator below. */
    const INSETS = { top: 0, right: 47, bottom: 21, left: 47 };
    /** A draft tall enough that the composer outgrows the empty state's spare room. */
    const TALL_DRAFT = ['one', 'two', 'three', 'four', 'five', 'six'].join('\n');

    /** Type a tall draft and check the composer and Send stay inside the frame. */
    async function expectComposerInFrame(page: Page): Promise<void> {
      await expect(page.getByText('No messages yet')).toBeVisible();
      await page.locator('scion-chat-composer textarea').fill(TALL_DRAFT);
      await page.waitForTimeout(300);
      const box = await composerAndFrame(page);
      const send = await page.locator('scion-chat-composer .send-btn').boundingBox();
      expect(box.frameBottom).toBeCloseTo(390, 0);
      expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(
        box.frameBottom + 0.5
      );
      expect(send, 'Send is rendered').not.toBeNull();
      expect(send!.y + send!.height, 'Send above the bottom inset').toBeLessThanOrEqual(
        box.frameBottom - INSETS.bottom + 0.5
      );
    }

    test('an empty thread keeps the composer inside the frame', async ({ page }, testInfo) => {
      test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
      await openChatRail(page);
      const cdp = await page.context().newCDPSession(page);
      await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: INSETS });
      await expandSpace(page);
      await page.locator('.thread-item', { hasText: 'thread-02' }).first().click();
      await expectComposerInFrame(page);
    });

    test('an empty general channel keeps the composer inside the frame', async ({
      page,
    }, testInfo) => {
      test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
      await openChatRail(page, async (p) => {
        await p.route(
          new RegExp(`/api/v1/chat/conversations/${GENERAL_THREAD_ID}/messages`),
          (route) => route.fulfill({ json: { items: [], messages: [] } })
        );
      });
      const cdp = await page.context().newCDPSession(page);
      await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: INSETS });
      await expect(page).toHaveURL(new RegExp(`/${GENERAL_THREAD_ID}$`));
      await expectComposerInFrame(page);
    });
  });
});
