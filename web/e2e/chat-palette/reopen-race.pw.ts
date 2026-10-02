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
 * Chromium regression coverage for the close-animation reopen race: a
 * Cmd/Ctrl+K that arrives while the palette's own close (from a prior
 * selection) is still animating out must yield a visibly open, focused
 * palette that receives typed keys — never an invisible open palette that a
 * still-pending close later hands off to the composer underneath it.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks, AGENT_WITH_DM, SELF_USER_ID } from './mock-api.js';

const DM_KEY = `dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`;
const DM_ROUTE = `/chat/dm/${encodeURIComponent(DM_KEY)}`;

/** Comfortably longer than Shoelace's 250ms default dialog hide animation — see below. */
const SETTLE_MS = 700;

/**
 * The delay-loop tests below deliberately stretch the palette's own dialog
 * hide animation to this duration (see `setPaletteDialogHideDuration`)
 * rather than relying on Shoelace's real 250ms default. The real gap between
 * pressing Enter and the browser actually starting that animation (event
 * dispatch, this app's own close-path work) varies with machine load, so an
 * unloaded run's longest fixed delay below (350ms) is not reliably still
 * inside a real 250ms hide — stretching the animation well past every delay
 * in the list is what makes each one deterministically land mid-close
 * instead of depending on how fast this particular run happens to be.
 */
const DETERMINISTIC_HIDE_MS = 1500;
const DETERMINISTIC_SETTLE_MS = DETERMINISTIC_HIDE_MS + 700;

async function gotoChat(page: Page, route?: string): Promise<void> {
  await setupApiMocks(page);
  const url = route
    ? `/e2e/chat-palette/fixture.html?route=${encodeURIComponent(route)}`
    : '/e2e/chat-palette/fixture.html';
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

function paletteInput(page: Page) {
  return page.locator('scion-chat-switcher #palette-query-input');
}

function paletteDialog(page: Page) {
  return page.locator('scion-chat-switcher sl-dialog[label="Quick switcher"]');
}

function paletteOptions(page: Page) {
  return page.locator('scion-chat-switcher .palette-option');
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

async function openPaletteAndFillAgent(page: Page): Promise<void> {
  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(AGENT_WITH_DM.name);
  await expect(paletteOptions(page)).toHaveCount(1);
}

const REOPEN_DELAYS_MS = [0, 100, 200, 350];

for (const delayMs of REOPEN_DELAYS_MS) {
  test(`reopening ${delayMs}ms after a selection yields a visible, focused palette that receives typed keys, not the composer`, async ({
    page,
  }) => {
    await gotoChat(page);
    await openPaletteAndFillAgent(page);
    // Stretch the close's hide animation well past every delay this loop
    // tests — see DETERMINISTIC_HIDE_MS's own comment for why a fixed delay
    // measured from the selection can't otherwise guarantee that.
    await page.evaluate(
      (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
      DETERMINISTIC_HIDE_MS
    );

    await page.keyboard.press('Enter'); // commits the selection and starts the palette's own close
    // The close animation is still in flight for every delay in this list —
    // this is the exact race: no other spec in this suite skips waiting for
    // it to finish before reopening.
    await page.waitForTimeout(delayMs);
    await page.keyboard.press('Control+k');

    // The close's own `sl-after-hide` (stretched to DETERMINISTIC_HIDE_MS
    // above) can still be pending at this point for every delay above, and
    // arrives well after this reopen attempt — a reopen that doesn't defer
    // past it lets that stale event hand focus to the composer out from
    // under whatever looked fine immediately after the second press.
    // Settling for longer than that stretched animation before asserting
    // anything is what actually proves the reopened palette *stays*
    // correct, not just that it looked correct for an instant.
    await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

    await expect(paletteDialog(page)).toBeVisible();
    await expect(paletteInput(page)).toBeFocused();
    await expect(composerTextarea(page)).not.toBeFocused();

    await page.keyboard.type('probe-text');
    await expect(paletteInput(page)).toHaveValue('probe-text');
    await page.waitForSelector('scion-chat-composer sl-textarea', {
      state: 'attached',
      timeout: 10_000,
    });
    await expect(composerTextarea(page)).not.toHaveValue(/probe-text/);
  });
}

/** Read `scion-page-chat`'s own `_palettePendingReopen` flag directly. */
async function isReopenPending(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { _palettePendingReopen: boolean })
        ._palettePendingReopen
  );
}

test('a reopen queued behind a still-animating close is abandoned, not opened, if an unrelated modal opens before the close settles', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  // Stretched so a real, non-deterministic 250ms hide can't finish (and let
  // the ordinary open guard reject the fresh open normally) before the
  // queued-reopen guard this test is actually about ever gets exercised.
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape'); // starts the (stretched) close animation
  await page.waitForTimeout(50); // still animating — the close's own sl-after-hide has not fired yet
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  expect(await isReopenPending(page)).toBe(true);

  await page.evaluate(() => {
    const dialog = document.createElement('sl-dialog');
    dialog.setAttribute('label', 'Unrelated');
    dialog.id = 'reopen-race-unrelated-dialog';
    document.body.appendChild(dialog);
    (dialog as unknown as { open: boolean }).open = true;
  });
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

  // The unrelated dialog stays the one modal in control; the queued reopen
  // must not stack the palette over it.
  await expect(page.locator('#reopen-race-unrelated-dialog')).toHaveJSProperty('open', true);
  await expect(paletteDialog(page)).toBeHidden();
});

test('a reopen queued behind a still-animating close is abandoned, not opened, if the route leaves /chat before the close settles', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape'); // starts the (stretched) close animation
  await page.waitForTimeout(50); // still animating — the close's own sl-after-hide has not fired yet
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  expect(await isReopenPending(page)).toBe(true);

  await page.evaluate(() => {
    history.pushState({}, '', '/terminals');
    window.dispatchEvent(new PopStateEvent('popstate'));
  });

  // Sample every 25ms across the whole (stretched) settle window rather than
  // checking only the final state: the palette's own 250ms visibility
  // watchdog would catch and close a briefly-opened palette well before the
  // window elapses, masking a version of this bug that opens the palette
  // for a few hundred milliseconds and only then closes it again. A
  // regression here must never report open at all, not just "eventually
  // closed."
  const samples: boolean[] = [];
  const sampleCount = Math.ceil(DETERMINISTIC_SETTLE_MS / 25);
  for (let i = 0; i < sampleCount; i++) {
    await page.waitForTimeout(25);
    samples.push(
      await page.evaluate(
        () =>
          (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
            .v2PaletteOpen
      )
    );
  }

  expect(samples.some(Boolean), `v2PaletteOpen samples: ${samples.join(',')}`).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();
});

test('reopening well after the close has actually finished opens normally (not a queued regression)', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteAndFillAgent(page);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
  await page.waitForSelector('scion-chat-composer sl-textarea', {
    state: 'attached',
    timeout: 10_000,
  });
  await expect(paletteDialog(page)).toBeHidden();
  await expect(composerTextarea(page)).toBeFocused();

  await page.keyboard.press('Control+k');

  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();
});

test('closing a reopen queued behind a selection focuses the new conversation, not a stale target or the fallback', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteAndFillAgent(page);

  await page.keyboard.press('Enter'); // selects the DM, starts the close
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  await page.waitForTimeout(SETTLE_MS);
  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();

  await page.keyboard.press('Escape'); // close the reopened palette
  await page.waitForSelector('scion-chat-composer sl-textarea', {
    state: 'attached',
    timeout: 10_000,
  });
  await page.waitForTimeout(SETTLE_MS);

  // The selection this reopen superseded would have focused the new DM's
  // composer; the reopen itself must not have discarded that in favor of
  // whatever was focused mid-animation (the fallback heading, or nothing).
  await expect(composerTextarea(page)).toBeFocused();
});

test('two presses during the same pending close cancel the queued reopen and end closed', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape'); // starts the (stretched) close animation
  await page.waitForTimeout(50); // still animating
  await page.keyboard.press('Control+k'); // queues a reopen
  expect(await isReopenPending(page)).toBe(true);
  await page.keyboard.press('Control+k'); // cancels the queued reopen — the shortcut is a toggle
  expect(await isReopenPending(page)).toBe(false);
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

  await expect(paletteDialog(page)).toBeHidden();
});

test('a second Escape while a reopen is queued behind a still-animating close cancels the queue, ending closed', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape'); // starts the (stretched) close animation
  await page.waitForTimeout(50); // still animating — the close's own sl-after-hide has not fired yet
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  expect(await isReopenPending(page)).toBe(true);

  // Shoelace is already mid-hide, so this second Escape never reaches the
  // dialog's own sl-request-close — nothing but the global keydown handler
  // itself can clear the queue.
  await page.keyboard.press('Escape');
  expect(await isReopenPending(page)).toBe(false);
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

  await expect(paletteDialog(page)).toBeHidden();
});

test('closing a reopen queued behind Escape restores the original composer focus and selection, not the fallback', async ({
  page,
}) => {
  await gotoChat(page, DM_ROUTE);
  await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft text');
  await textarea.evaluate((el) => (el as HTMLTextAreaElement).setSelectionRange(2, 5));
  await expect(textarea).toBeFocused();

  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape'); // starts the (stretched) close animation — not a selection
  await page.waitForTimeout(50); // still animating
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  expect(await isReopenPending(page)).toBe(true);
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);
  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();

  await page.keyboard.press('Escape'); // close the reopened palette normally
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

  // The original composer invoker — captured before the palette was ever
  // opened, and never a selection — must be exactly what this ordinary
  // dismissal restores, with its original selection intact, not the
  // fallback heading (nothing was ever selected, so nothing should have
  // retargeted the invoker along the way).
  await expect(textarea).toBeFocused();
  const selection = await textarea.evaluate((el) => [
    (el as HTMLTextAreaElement).selectionStart,
    (el as HTMLTextAreaElement).selectionEnd,
  ]);
  expect(selection).toEqual([2, 5]);
});

test('closing a reopen queued behind Escape restores a non-composer invoker, even with a real composer mounted elsewhere', async ({
  page,
}) => {
  // A composer exists in the page (a real one, from the DM route), but it
  // is deliberately not what's focused before the palette ever opens — this
  // is what discriminates "only retarget for an actual selection close"
  // from "retarget to the newest composer for any dismissal kind at all."
  //
  // The invoker itself is a plain button outside <scion-page-chat> entirely
  // — not the page's own #palette-focus-fallback — so "the invoker was
  // actually restored" and "focus fell back to the page's own default" are
  // distinguishable outcomes, not the same element either way.
  await gotoChat(page, DM_ROUTE);
  await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
  const outsideInvoker = page.locator('#outside-invoker');
  await outsideInvoker.focus();
  await expect(outsideInvoker).toBeFocused();

  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_HIDE_MS
  );
  await page.keyboard.press('Escape');
  await page.waitForTimeout(50);
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  expect(await isReopenPending(page)).toBe(true);
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);
  await expect(paletteDialog(page)).toBeVisible();

  await page.keyboard.press('Escape'); // close the reopened palette normally
  await page.waitForTimeout(DETERMINISTIC_SETTLE_MS);

  await expect(outsideInvoker).toBeFocused();
  await expect(composerTextarea(page)).not.toBeFocused();
});
