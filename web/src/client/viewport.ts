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
 * Keeps the app frame sized to the visible area while the on-screen
 * keyboard is open.
 *
 * iOS Safari ignores `interactive-widget=resizes-content`: when the keyboard
 * opens, the layout viewport (and so `100dvh`) keeps its full height and
 * Safari pans the page to reveal the focused input, scrolling the header
 * out of view. This module watches the visual viewport and, while the
 * keyboard is open, shrinks `--scion-app-height` to the visible height so
 * the header stays at the top and the composer sits just above the
 * keyboard. In frame mode it also undoes any window scroll the browser
 * applied, including a pan left over after the keyboard closes.
 *
 * A shorter occluder, such as the shortcut bar an iPad shows with a hardware
 * keyboard, leaves the frame at full height. While one is up the scroll
 * reset stands down too, so Safari's own pan to reveal the focused input is
 * kept rather than undone. The trade-off: a pan made while such a bar is up
 * is only reset once it goes away.
 *
 * The visible height can change without a visual-viewport event: iOS shows
 * its AutoFill bar (passwords, cards, locations) above the keyboard some
 * time after the keyboard opens, and a frame sized before that runs behind
 * the bar. So it also re-reads the viewport on a window resize, on focus
 * moving in or out of a field, on every text input, and once more shortly
 * after any change to the keyboard state, so a late change is caught even
 * before the next keystroke. Each of these is throttled to one read per
 * animation frame and writes to the DOM only on a change.
 *
 * It does nothing while the page is pinch-zoomed, so it never fights a
 * user's zoom-and-pan. On desktop, and on Android (where
 * `interactive-widget=resizes-content` shrinks the layout viewport itself),
 * the layout and visual viewports have the same height, so it is a no-op.
 *
 * State it exposes while the keyboard is open:
 * - an inline `--scion-app-height: <visual viewport height>px` on `<html>`;
 * - `html[data-keyboard="open"]`, for light-DOM styles;
 * - `--scion-kb-open: 1` on `<html>`, which inherits into shadow roots, so a
 *   component can drop its bottom safe-area padding with
 *   `calc(env(safe-area-inset-bottom) * (1 - var(--scion-kb-open, 0)))`.
 * - in a short frame (under `SHORT_FRAME_MAX_PX`), `--scion-kb-short: 1`
 *   and `--scion-kb-short-display: none`; in a tight one (under
 *   `TIGHT_FRAME_MAX_PX`) also `--scion-kb-tight: 1`,
 *   `--scion-kb-tight-position: absolute` and
 *   `--scion-kb-tight-visibility: hidden`. A component hides chrome it can
 *   spare while typing with `display: var(--scion-kb-short-display, <its
 *   usual display>)`, or takes it out of flow while keeping it measurable
 *   with the tight position and visibility, and can scale a length with the
 *   number.
 * All of these are cleared when the keyboard closes or the page is zoomed.
 */

import { APP_FRAME_CLASS } from '../components/shared/app-frame.js';

/**
 * How far the visual viewport must fall short of the layout viewport, in CSS
 * px, before it counts as an open keyboard. Well above what URL-bar and
 * toolbar changes produce, and well below any phone keyboard.
 */
export const KEYBOARD_MIN_INSET_PX = 150;

/**
 * A keyboard-shrunk frame shorter than this, in CSS px, is "short": too
 * short for the full chat chrome plus a few lines of draft and a message,
 * so the app's top bar can give way. With the keyboard up, phones in
 * portrait land around 200-380px and in landscape around 150-190px; tablets
 * stay well above.
 *
 * Both limits are free choices: crossing one never moves the composer's
 * field, because chat/composer-room.ts counts what is hidden here as shown
 * and phases what it frees in over the band between the two limits.
 */
export const SHORT_FRAME_MAX_PX = 360;

/**
 * A keyboard-shrunk frame shorter than this, in CSS px, is "tight": even
 * without the top bar there is not room for the composer's own chrome, a
 * couple of lines of draft and a message, as on a 320px-wide phone or any
 * phone in landscape, so secondary composer chrome gives way too.
 */
export const TIGHT_FRAME_MAX_PX = 290;

/**
 * Smallest shortfall, in CSS px, that counts as something covering the page.
 * Anything less is sub-pixel rounding between the two viewports.
 */
const OCCLUDER_MIN_INSET_PX = 1;

/** `scale` within this distance of 1 counts as "not zoomed". */
const ZOOM_1_TOLERANCE = 0.01;

/**
 * Largest window scroll or visual-viewport offset, in CSS px, that counts as
 * "not scrolled". On high-DPI screens either can settle on a sub-pixel value
 * that `scrollTo(0, 0)` cannot clear; resetting on that would loop forever.
 */
const SCROLL_TOLERANCE_PX = 0.5;

/**
 * Delay, in ms, before the follow-up read after a keyboard-state change:
 * long enough for an accessory bar that follows the keyboard to settle.
 */
export const SETTLE_RECHECK_MS = 400;

const APP_HEIGHT_PROPERTY = '--scion-app-height';
const KEYBOARD_OPEN_PROPERTY = '--scion-kb-open';
type FrameSize = 'short' | 'tight';

/**
 * Install the visual-viewport listeners. Call once, at startup. Returns a
 * disposer that removes every listener, cancels any pending frame and
 * clears the keyboard state it set.
 */
export function installViewportFrame(win: Window = window): () => void {
  const vv = win.visualViewport;
  if (!vv) return () => {};
  const doc = win.document;
  const root = doc.documentElement;

  // The height currently written to the inline custom property, or null when
  // nothing is written. Writes happen only on a change, so a resize storm
  // with the keyboard closed never touches the DOM.
  let appliedHeight: string | null = null;

  // The pending follow-up read, scheduled after a keyboard-state change.
  let settleTimer: number | null = null;
  const scheduleSettleRecheck = (): void => {
    if (settleTimer !== null) win.clearTimeout(settleTimer);
    settleTimer = win.setTimeout(() => {
      settleTimer = null;
      schedule();
    }, SETTLE_RECHECK_MS);
  };

  // Mark the frame short or tight (each implies the ones before it), or
  // clear both.
  const applyFrameSize = (size: FrameSize | null): void => {
    const set = (name: string, value: string, on: boolean): void => {
      if (on) root.style.setProperty(name, value);
      else root.style.removeProperty(name);
    };
    set('--scion-kb-short', '1', size !== null);
    set('--scion-kb-short-display', 'none', size !== null);
    set('--scion-kb-tight', '1', size === 'tight');
    set('--scion-kb-tight-position', 'absolute', size === 'tight');
    set('--scion-kb-tight-visibility', 'hidden', size === 'tight');
  };

  const clearKeyboardState = (): boolean => {
    if (appliedHeight === null) return false;
    root.style.removeProperty(APP_HEIGHT_PROPERTY);
    root.style.removeProperty(KEYBOARD_OPEN_PROPERTY);
    delete root.dataset['keyboard'];
    applyFrameSize(null);
    appliedHeight = null;
    return true;
  };

  const sync = (): void => {
    const atZoom1 = Math.abs(vv.scale - 1) < ZOOM_1_TOLERANCE;
    const keyboardInset = win.innerHeight - vv.height;
    const keyboardOpen = atZoom1 && keyboardInset > KEYBOARD_MIN_INSET_PX;
    // A bar shorter than a keyboard: leave Safari's reveal pan alone.
    const shortOccluder =
      keyboardInset >= OCCLUDER_MIN_INSET_PX && keyboardInset <= KEYBOARD_MIN_INSET_PX;

    let changed = false;
    if (keyboardOpen) {
      const height = `${vv.height}px`;
      if (height !== appliedHeight) {
        root.style.setProperty(APP_HEIGHT_PROPERTY, height);
        root.style.setProperty(KEYBOARD_OPEN_PROPERTY, '1');
        root.dataset['keyboard'] = 'open';
        applyFrameSize(
          vv.height < TIGHT_FRAME_MAX_PX ? 'tight' : vv.height < SHORT_FRAME_MAX_PX ? 'short' : null
        );
        appliedHeight = height;
        changed = true;
      }
    } else {
      changed = clearKeyboardState();
    }
    // A change can be followed by another the browser does not announce (a
    // late accessory bar), so look once more after it settles.
    if (changed) scheduleSettleRecheck();

    if (
      atZoom1 &&
      !shortOccluder &&
      root.classList.contains(APP_FRAME_CLASS) &&
      (Math.abs(win.scrollY) > SCROLL_TOLERANCE_PX || Math.abs(vv.offsetTop) > SCROLL_TOLERANCE_PX)
    ) {
      win.scrollTo(0, 0);
    }
  };

  // One sync per animation frame, however many events arrive.
  let frame: number | null = null;
  function schedule(): void {
    if (frame !== null) return;
    frame = win.requestAnimationFrame(() => {
      frame = null;
      sync();
    });
  }

  // focusin, focusout and input are composed, so they reach the document
  // from fields inside shadow roots.
  const docEvents = ['focusin', 'focusout', 'input'] as const;
  vv.addEventListener('resize', schedule);
  vv.addEventListener('scroll', schedule);
  win.addEventListener('scroll', schedule, { passive: true });
  win.addEventListener('resize', schedule);
  for (const type of docEvents) doc.addEventListener(type, schedule, { capture: true });
  schedule();

  return () => {
    vv.removeEventListener('resize', schedule);
    vv.removeEventListener('scroll', schedule);
    win.removeEventListener('scroll', schedule);
    win.removeEventListener('resize', schedule);
    for (const type of docEvents) doc.removeEventListener(type, schedule, { capture: true });
    if (frame !== null) {
      win.cancelAnimationFrame(frame);
      frame = null;
    }
    if (settleTimer !== null) {
      win.clearTimeout(settleTimer);
      settleTimer = null;
    }
    clearKeyboardState();
  };
}
