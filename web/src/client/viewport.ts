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
 * All three are cleared when the keyboard closes or the page is zoomed.
 */

import { APP_FRAME_CLASS } from '../components/shared/app-frame.js';

/**
 * How far the visual viewport must fall short of the layout viewport, in CSS
 * px, before it counts as an open keyboard. Well above what URL-bar and
 * toolbar changes produce, and well below any phone keyboard.
 */
export const KEYBOARD_MIN_INSET_PX = 150;

/**
 * Smallest shortfall, in CSS px, that counts as something covering the page.
 * Anything less is sub-pixel rounding between the two viewports.
 */
const OCCLUDER_MIN_INSET_PX = 1;

/** `scale` within this distance of 1 counts as "not zoomed". */
const ZOOM_1_TOLERANCE = 0.01;

const APP_HEIGHT_PROPERTY = '--scion-app-height';
const KEYBOARD_OPEN_PROPERTY = '--scion-kb-open';

/**
 * Install the visual-viewport listeners. Call once, at startup. Returns a
 * disposer that removes every listener, cancels any pending frame and
 * clears the keyboard state it set.
 */
export function installViewportFrame(win: Window = window): () => void {
  const vv = win.visualViewport;
  if (!vv) return () => {};
  const root = win.document.documentElement;

  // The height currently written to the inline custom property, or null when
  // nothing is written. Writes happen only on a change, so a resize storm
  // with the keyboard closed never touches the DOM.
  let appliedHeight: string | null = null;

  const clearKeyboardState = (): void => {
    if (appliedHeight === null) return;
    root.style.removeProperty(APP_HEIGHT_PROPERTY);
    root.style.removeProperty(KEYBOARD_OPEN_PROPERTY);
    delete root.dataset['keyboard'];
    appliedHeight = null;
  };

  const sync = (): void => {
    const atZoom1 = Math.abs(vv.scale - 1) < ZOOM_1_TOLERANCE;
    const keyboardInset = win.innerHeight - vv.height;
    const keyboardOpen = atZoom1 && keyboardInset > KEYBOARD_MIN_INSET_PX;
    // A bar shorter than a keyboard: leave Safari's reveal pan alone.
    const shortOccluder =
      keyboardInset >= OCCLUDER_MIN_INSET_PX && keyboardInset <= KEYBOARD_MIN_INSET_PX;

    if (keyboardOpen) {
      const height = `${vv.height}px`;
      if (height !== appliedHeight) {
        root.style.setProperty(APP_HEIGHT_PROPERTY, height);
        root.style.setProperty(KEYBOARD_OPEN_PROPERTY, '1');
        root.dataset['keyboard'] = 'open';
        appliedHeight = height;
      }
    } else {
      clearKeyboardState();
    }

    if (
      atZoom1 &&
      !shortOccluder &&
      root.classList.contains(APP_FRAME_CLASS) &&
      (win.scrollY !== 0 || vv.offsetTop !== 0)
    ) {
      win.scrollTo(0, 0);
    }
  };

  // One sync per animation frame, however many resize/scroll events arrive.
  let frame: number | null = null;
  const schedule = (): void => {
    if (frame !== null) return;
    frame = win.requestAnimationFrame(() => {
      frame = null;
      sync();
    });
  };

  vv.addEventListener('resize', schedule);
  vv.addEventListener('scroll', schedule);
  win.addEventListener('scroll', schedule, { passive: true });
  schedule();

  return () => {
    vv.removeEventListener('resize', schedule);
    vv.removeEventListener('scroll', schedule);
    win.removeEventListener('scroll', schedule);
    if (frame !== null) {
      win.cancelAnimationFrame(frame);
      frame = null;
    }
    clearKeyboardState();
  };
}
