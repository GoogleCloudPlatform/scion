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
 * Chromium: from nested sl-textarea, both modifier variants open/focus the
 * palette's query input without changing the composer draft;
 * Escape/backdrop/toggle restore deep focus and cursor/selection. Repeat,
 * Alt/Shift and IME do not accidentally toggle/commit.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks, AGENT_WITH_DM, SELF_USER_ID, type TrackedRequest } from './mock-api.js';
import { paletteInputHasFocus, slowPaletteModule } from '../palette-focus.js';

const DM_KEY = `dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`;
const ROUTE = `/chat/dm/${encodeURIComponent(DM_KEY)}`;

async function gotoWithComposer(page: Page): Promise<TrackedRequest[]> {
  const requests = await setupApiMocks(page);
  await page.goto(`/e2e/chat-palette/fixture.html?route=${encodeURIComponent(ROUTE)}`, {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForSelector('scion-chat-composer sl-textarea', {
    state: 'attached',
    timeout: 10_000,
  });
  return requests;
}

/** Deep-query into the real composer's native textarea, through both shadow roots. */
function composerTextarea(page: Page) {
  return page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

function paletteInput(page: Page) {
  return page.locator('scion-quick-palette #palette-query-input');
}

test('Ctrl+K opens the palette and focuses its input without touching the composer draft', async ({
  page,
}) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft in progress');

  await page.keyboard.press('Control+k');

  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();
  await expect(textarea).toHaveValue('draft in progress');
});

test('Meta+K also opens the palette from the composer', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();

  await page.keyboard.press('Meta+k');

  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();
});

function paletteOptions(page: Page) {
  return page.locator('scion-quick-palette .palette-option');
}

/**
 * Types `coder` straight after Ctrl+K, without waiting for the palette, and
 * checks that it all became the query: the input has focus right after the
 * open, the results are filtered, and the composer draft is untouched.
 */
async function expectTypingRightAfterOpenFilters(page: Page): Promise<void> {
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft');

  await page.keyboard.press('Control+k');
  await page.keyboard.type('coder');

  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(paletteInput(page)).toHaveValue('coder');
  await expect(paletteOptions(page)).toHaveText([/Coder One/]);
  await expect(textarea).toHaveValue('draft');
}

test('typing straight after Ctrl+K becomes the query, not composer text', async ({ page }) => {
  await gotoWithComposer(page);
  await expectTypingRightAfterOpenFilters(page);
});

test('typing straight after Ctrl+K becomes the query while the palette module is still loading', async ({
  page,
}) => {
  await slowPaletteModule(page);
  await gotoWithComposer(page);
  await expectTypingRightAfterOpenFilters(page);
});

test('typing straight after a reopen becomes the new query', async ({ page }) => {
  await gotoWithComposer(page);
  await page.keyboard.press('Control+k');
  await page.keyboard.type('review');
  await expect(paletteInput(page)).toHaveValue('review');
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();

  await expectTypingRightAfterOpenFilters(page);
});

test('typing straight after a Ctrl+K pressed during the close animation becomes the reopened query', async ({
  page,
}) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft');
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.keyboard.type('review');

  // No wait between the Escape and the reopen: the close is still animating.
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+k');
  await page.keyboard.type('coder');

  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toHaveValue('coder');
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(paletteOptions(page)).toHaveText([/Coder One/]);
  await expect(textarea).toHaveValue('draft');
});

test('Escape restores focus and the caret position in the composer', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('hello world');
  // Select "world" (indices 6-11) before opening the palette.
  await textarea.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(6, 11, 'forward'));

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(textarea).toBeFocused();
  await expect(textarea).toHaveValue('hello world');
  const selection = await textarea.evaluate((el: HTMLTextAreaElement) => [
    el.selectionStart,
    el.selectionEnd,
    el.selectionDirection,
  ]);
  expect(selection).toEqual([6, 11, 'forward']);
});

test('Escape restores focus and selection in a plain text input, not just a textarea', async ({
  page,
}) => {
  // The composer is a textarea, so it alone can't discriminate the
  // HTMLInputElement branch of capture/restore from a build that only ever
  // recognized HTMLTextAreaElement. Inject a plain <input> as the palette
  // invoker instead.
  //
  // Chromium keeps an <input>'s selection across blur and refocus on its
  // own, so just checking the selection survives Escape would still pass
  // even without any capture/restore code at all — focus alone getting
  // restored to the input would be enough to make it pass. The selection is
  // deliberately changed while the palette is open (mirroring the
  // happy-dom unit test's approach) so that only an actual restore can put
  // it back.
  await gotoWithComposer(page);
  await page.evaluate(() => {
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'e2e-text-input-invoker';
    document.body.appendChild(input);
  });
  const input = page.locator('#e2e-text-input-invoker');
  await input.click();
  await input.fill('hello world');
  await input.evaluate((el: HTMLInputElement) => el.setSelectionRange(6, 11, 'forward'));

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await input.evaluate((el: HTMLInputElement) => el.setSelectionRange(0, 0));
  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(input).toBeFocused();
  await expect(input).toHaveValue('hello world');
  const selection = await input.evaluate((el: HTMLInputElement) => [
    el.selectionStart,
    el.selectionEnd,
    el.selectionDirection,
  ]);
  expect(selection).toEqual([6, 11, 'forward']);
});

test('Escape restores focus and selection to a visible position:fixed invoker', async ({
  page,
}) => {
  // `offsetParent` is null for a `position: fixed` element even when it's
  // fully visible, so a visibility test built on `offsetParent === null`
  // alone wrongly treats this invoker as hidden. Focus alone can't
  // discriminate this in this app: `<sl-dialog>` independently remembers
  // whatever had focus when it opened and refocuses it after hide on its
  // own, regardless of what this app's own restore code decides, so a fixed
  // invoker gets focus back either way. Selection is not part of that
  // built-in behavior — it is only restored by this app's own code, and
  // only on the branch that treats the invoker as visible — so a lost
  // selection is what actually surfaces the `offsetParent` bug here. The
  // selection is deliberately perturbed while the palette is open (as in
  // the plain-input selection test above) so only a real restore can put it
  // back.
  await gotoWithComposer(page);
  await page.evaluate(() => {
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'e2e-fixed-invoker';
    input.style.position = 'fixed';
    input.style.top = '0';
    input.style.left = '0';
    input.style.width = '100px';
    input.style.height = '20px';
    document.body.appendChild(input);
  });
  const input = page.locator('#e2e-fixed-invoker');
  await input.click();
  await input.fill('hello world');
  await input.evaluate((el: HTMLInputElement) => el.setSelectionRange(6, 11, 'forward'));
  expect(await input.evaluate((el: HTMLElement) => el.offsetParent)).toBeNull();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await input.evaluate((el: HTMLInputElement) => el.setSelectionRange(0, 0));
  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(input).toBeFocused();
  const selection = await input.evaluate((el: HTMLInputElement) => [
    el.selectionStart,
    el.selectionEnd,
    el.selectionDirection,
  ]);
  expect(selection).toEqual([6, 11, 'forward']);
});

test('Escape falls back when the invoker turned display:none while the palette was open', async ({
  page,
}) => {
  await gotoWithComposer(page);
  await page.evaluate(() => {
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'e2e-hidden-invoker';
    document.body.appendChild(input);
  });
  const input = page.locator('#e2e-hidden-invoker');
  await input.click();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await input.evaluate((el: HTMLElement) => {
    el.style.display = 'none';
  });
  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(input).not.toBeFocused();
  await expect(page.locator('scion-page-chat').locator('#palette-focus-fallback')).toBeFocused();
});

test('Escape falls back when the invoker turned visibility:hidden while the palette was open', async ({
  page,
}) => {
  // `checkVisibility()` does not check the CSS `visibility` property unless
  // asked to — without passing `visibilityProperty`/`checkVisibilityCSS`, a
  // `visibility: hidden` invoker reports itself as visible, so restore
  // would call `.focus()` on it. That call is a real no-op in Chromium (the
  // element cannot actually receive focus), so if restore treats the
  // invoker as visible here, focus stays wherever it already was instead of
  // moving to the fallback target.
  await gotoWithComposer(page);
  await page.evaluate(() => {
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'e2e-invisible-invoker';
    document.body.appendChild(input);
  });
  const input = page.locator('#e2e-invisible-invoker');
  await input.click();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await input.evaluate((el: HTMLElement) => {
    el.style.visibility = 'hidden';
  });
  await page.keyboard.press('Escape');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(input).not.toBeFocused();
  await expect(page.locator('scion-page-chat').locator('#palette-focus-fallback')).toBeFocused();
});

test('backdrop click restores deep focus', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  // Click the dialog's backdrop (::part(overlay)), not the panel content.
  await page
    .locator('scion-quick-palette sl-dialog')
    .locator('[part~="overlay"]')
    .click({
      position: { x: 5, y: 5 },
      force: true,
    });

  await expect(paletteDialog(page)).toBeHidden();
  await expect(textarea).toBeFocused();
});

test('a second Ctrl+K toggles the palette closed and restores focus', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
  await page.keyboard.press('Control+k');

  await expect(paletteDialog(page)).toBeHidden();
  await expect(textarea).toBeFocused();
});

/**
 * Reads the page's own `v2PaletteOpen` state directly rather than asserting
 * on DOM visibility. Opening is async (dynamic import, then
 * Lit's updateComplete), so a `toBeHidden()`/`toBeVisible()` check made
 * *immediately* after a negative keypress passes regardless of whether the
 * guard under test actually ran — the async open simply hasn't had time to
 * happen yet even with the guard removed, so the assertion can't fail. A
 * settle wait plus reading the real flag (not the animation-lagged DOM
 * state) closes that hole; each of the three mutations below was manually
 * verified to make its assertion fail once the guard it protects is
 * removed from chat.ts, and to still pass with the guard present.
 */
async function paletteOpenState(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
}

test('repeat, Alt+K and Shift+K do not toggle the palette open', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();

  await page.keyboard.press('Alt+Control+k');
  await page.waitForTimeout(600); // let an (incorrectly) opened palette's async open finish
  expect(await paletteOpenState(page)).toBe(false);

  await page.keyboard.press('Shift+Control+k');
  await page.waitForTimeout(600);
  expect(await paletteOpenState(page)).toBe(false);

  // A held-down key repeat: dispatch a synthetic repeat keydown directly,
  // since Playwright's keyboard API does not simulate OS key-repeat timing.
  await page.evaluate(() => {
    document.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, repeat: true, bubbles: true })
    );
  });
  await page.waitForTimeout(600);
  expect(await paletteOpenState(page)).toBe(false);

  // Positive control: a plain Ctrl+K still opens it — proves the page and
  // its listener are alive, so the three `false`s above are the guards
  // actually working, not a dead handler.
  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
});

test('IME composition does not commit or toggle while the palette is open', async ({ page }) => {
  await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  const input = paletteInput(page);
  const urlBeforeEnter = page.url();
  await input.dispatchEvent('compositionstart');
  await input.evaluate((el: HTMLInputElement) => {
    el.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, bubbles: true })
    );
  });
  await page.waitForTimeout(600); // let an (incorrectly) committed selection's navigation finish

  // Still open, and no selection was committed: no navigation happened.
  expect(await paletteOpenState(page)).toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
  expect(page.url()).toBe(urlBeforeEnter);

  // Positive control: ending composition and pressing Enter now *does*
  // commit — proves the guard above is what blocked it, not something else
  // (e.g. the dialog being broken). Retype to select a *different* agent
  // than the one this fixture already starts on (the global best for an
  // empty query is the same agent the route already points at, so an
  // empty-query commit would produce an identical URL and prove nothing).
  await input.fill('Review');
  await input.dispatchEvent('compositionend');
  await page.keyboard.press('Enter');
  await expect(paletteDialog(page)).toBeHidden();
  expect(page.url()).not.toBe(urlBeforeEnter);
});

test('Ctrl+K while IME composing does not toggle the palette open', async ({ page }) => {
  // The "IME composition does not commit" test above only covers
  // `handlePaletteKeydown`'s own Enter-composing guard, once the palette is
  // already open. It never sends a composing Ctrl+K, so this covers
  // `_handleGlobalKeydown`'s own `e.isComposing` guard — the one that keeps
  // IME composition from accidentally toggling the palette via the global
  // shortcut.
  //
  // This drives a genuine CDP-level IME composition via
  // `Input.imeSetComposition`, then presses Ctrl+K for real with
  // `page.keyboard.press` — the resulting keydown is trusted, with
  // `isComposing: true` set by Chromium itself, confirmed by capturing it
  // directly below.
  const requests = await gotoWithComposer(page);
  const textarea = composerTextarea(page);
  await textarea.click();
  // Only palette-relevant endpoints, not the composer's own typing
  // indicator — driving a genuine IME composition (below) actually inserts
  // "に" into the focused textarea, which the real composer legitimately
  // reacts to with its own unrelated POST .../typing request.
  const paletteRequests = () =>
    requests.filter((r) => /\/api\/v1\/agents|\/api\/v1\/chat\/dms/.test(r.url));
  const paletteRequestCountBefore = paletteRequests().length;

  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.imeSetComposition', { text: 'に', selectionStart: 1, selectionEnd: 1 });
  await page.evaluate(() => {
    document.addEventListener('keydown', (e) => {
      if (e.key.toLowerCase() === 'k') {
        (
          window as unknown as {
            composingKeydownObserved?: { isComposing: boolean; defaultPrevented: boolean };
          }
        ).composingKeydownObserved = {
          isComposing: e.isComposing,
          defaultPrevented: e.defaultPrevented,
        };
      }
    });
  });

  await page.keyboard.press('Control+k');
  await page.waitForTimeout(600); // let an (incorrectly) opened palette's async open finish

  const observed = await page.evaluate(
    () =>
      (
        window as unknown as {
          composingKeydownObserved?: { isComposing: boolean; defaultPrevented: boolean };
        }
      ).composingKeydownObserved
  );
  // Confirms the keydown Chromium actually delivered was mid-composition —
  // not an assumption about what imeSetComposition does.
  expect(observed?.isComposing).toBe(true);
  expect(observed?.defaultPrevented).toBe(false);
  expect(await paletteOpenState(page)).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();
  expect(paletteRequests().length).toBe(paletteRequestCountBefore);

  // Positive control: clear the composition, then the identical Ctrl+K
  // (still a real key press) does open the palette — proves the guard
  // above is what blocked it, not a dead listener or the composer eating
  // the event for some unrelated reason.
  await cdp.send('Input.imeSetComposition', { text: '', selectionStart: 0, selectionEnd: 0 });
  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
});
