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
 * the layout and visual viewports have the same height, so none of the
 * above applies.
 *
 * Android still gets `html[data-keyboard="open"]`, so light-DOM chrome can
 * give way to the keyboard there too (the terminal workspace does). It is
 * inferred, on a touch-primary device only, from a text field holding focus
 * while the layout height is more than `KEYBOARD_MIN_INSET_PX` below the
 * tallest layout height seen at the current width. Each width keeps its own
 * tallest height, so a rotation starts from that orientation's own
 * baseline; one opened before the keyboard was ever closed in that
 * orientation is missed until it closes. The focus check keeps a
 * split-screen resize from counting, and the touch check a desktop window
 * resize or a docked devtools panel. Only the attribute is set: no frame
 * height, `--scion-kb-open` or frame-size state, since the frame already
 * follows the layout viewport.
 *
 * State it exposes while the keyboard is open:
 * - an inline `--scion-app-height: <visual viewport height>px` on `<html>`;
 * - `html[data-keyboard="open"]`, for light-DOM styles (also set on Android,
 *   as described above);
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
import { TOUCH_PRIMARY_QUERY } from '../utils/input-modality.js';

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

/** `<input>` types that take text and so raise an on-screen keyboard. */
const TEXT_INPUT_TYPES = new Set(['text', 'search', 'email', 'url', 'tel', 'password', 'number']);

/**
 * Whether the focused element, looked up through open shadow roots, takes
 * typed text: a textarea (xterm's own input is one), a text-like input, or
 * an editable element.
 */
function editableFocused(doc: Document): boolean {
  let el: Element | null = doc.activeElement;
  while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
  if (!el) return false;
  if (el.tagName === 'TEXTAREA') return true;
  if (el.tagName === 'INPUT') return TEXT_INPUT_TYPES.has((el as HTMLInputElement).type);
  return (el as HTMLElement).isContentEditable === true;
}

/**
 * Delay, in ms, before the follow-up read after a keyboard-state change:
 * long enough for an accessory bar that follows the keyboard to settle.
 */
export const SETTLE_RECHECK_MS = 400;

/**
 * Minimum visual-viewport height, in CSS px, that counts as plausible for a
 * live keyboard-open frame. Even a small phone in landscape with an on-screen
 * keyboard leaves ~150-190px; anything below this (such as 0px or a bogus
 * status-bar-sized reading reported by iOS standalone web clips) is treated
 * as implausible so `--scion-app-height` is never collapsed to blank the UI.
 */
export const MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX = 120;

/** Media query matching home-screen / installed standalone web apps. */
export const STANDALONE_DISPLAY_QUERY = '(display-mode: standalone)';

/** localStorage key that opts into the viewport diagnostic overlay. */
export const VIEWPORT_DEBUG_STORAGE_KEY = 'scion:viewport-debug';

/** Query parameter (`?viewportDebug=1`) that opts into the diagnostic overlay. */
export const VIEWPORT_DEBUG_QUERY_PARAM = 'viewportDebug';

const APP_HEIGHT_PROPERTY = '--scion-app-height';
const KEYBOARD_OPEN_PROPERTY = '--scion-kb-open';
type FrameSize = 'short' | 'tight';

export interface ViewportDiagnosticSnapshot {
  event: string;
  count: number;
  vvHeight: number;
  vvWidth: number;
  vvScale: number;
  vvOffsetTop: number;
  innerHeight: number;
  innerWidth: number;
  clientHeight: number;
  standalone: boolean;
  editableFocused: boolean;
  inlineAppHeight: string;
  computedAppHeight: string;
  keyboardDataset: string;
  guard: string;
  chatRect: { top: number; height: number; width: number } | null;
}

type WindowWithViewportDiag = Window & {
  __scionViewportDiag?: () => ViewportDiagnosticSnapshot;
};

/** Whether a viewport height reading is finite and large enough to be real. */
export function isPlausibleViewportHeight(height: number): boolean {
  return Number.isFinite(height) && height >= MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX;
}

/** Whether the page is running as an installed / home-screen standalone app. */
function isStandaloneMode(win: Window): boolean {
  if (win.matchMedia?.(STANDALONE_DISPLAY_QUERY)?.matches === true) return true;
  const nav = win.navigator as (Navigator & { standalone?: boolean }) | undefined;
  return nav?.standalone === true;
}

/**
 * Whether the opt-in viewport diagnostic is enabled via `?viewportDebug=1`
 * or `localStorage['scion:viewport-debug'] = '1'`. Inactive by default.
 */
export function isViewportDebugEnabled(win: Window = window): boolean {
  try {
    const search = win.location?.search;
    if (search) {
      const param = new URLSearchParams(search).get(VIEWPORT_DEBUG_QUERY_PARAM);
      if (param === '1' || param === 'true') return true;
      if (param === '0' || param === 'false') return false;
    }
  } catch {
    // Ignore malformed location in mocked windows.
  }
  try {
    const stored = win.localStorage?.getItem(VIEWPORT_DEBUG_STORAGE_KEY);
    if (stored === '1' || stored === 'true') return true;
  } catch {
    // Ignore storage access errors (private browsing / sandboxed frames).
  }
  return false;
}

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
  const debugEnabled = isViewportDebugEnabled(win);
  let debugOverlay: HTMLDivElement | null = null;
  let debugCount = 0;
  let lastSnapshot: ViewportDiagnosticSnapshot | null = null;
  let pendingEvent = 'init';

  if (debugEnabled) {
    debugOverlay = doc.createElement('div');
    debugOverlay.id = 'scion-viewport-debug';
    debugOverlay.setAttribute('aria-hidden', 'true');
    debugOverlay.style.cssText =
      'position:fixed;left:0;right:0;bottom:env(safe-area-inset-bottom,0px);' +
      'z-index:2147483647;pointer-events:none;' +
      'background:var(--scion-surface);color:var(--scion-text);' +
      'border-top:1px solid var(--scion-border);' +
      'font:11px/1.3 monospace;padding:4px 6px;white-space:pre-wrap;opacity:0.92';
    (doc.body ?? root).append(debugOverlay);
    (win as WindowWithViewportDiag).__scionViewportDiag = () =>
      lastSnapshot ?? {
        event: pendingEvent,
        count: debugCount,
        vvHeight: vv.height,
        vvWidth: vv.width,
        vvScale: vv.scale,
        vvOffsetTop: vv.offsetTop,
        innerHeight: win.innerHeight,
        innerWidth: win.innerWidth,
        clientHeight: root.clientHeight,
        standalone: isStandaloneMode(win),
        editableFocused: editableFocused(doc),
        inlineAppHeight: root.style.getPropertyValue(APP_HEIGHT_PROPERTY),
        computedAppHeight: '',
        keyboardDataset: root.dataset['keyboard'] ?? '',
        guard: 'pending',
        chatRect: null,
      };
  }

  // The height currently written to the inline custom property, or null when
  // nothing is written. Writes happen only on a change, so a resize storm
  // with the keyboard closed never touches the DOM.
  let appliedHeight: string | null = null;

  // Android keyboard state (see the module comment). The tallest layout
  // height seen at each layout width: a rotation changes the width, so each
  // orientation keeps its own baseline.
  const tallestAtWidth = new Map<number, number>();
  // Only a touch-primary device has an on-screen keyboard to infer from a
  // shrinking layout viewport; on a desktop that is a window resize.
  const touchPrimary = win.matchMedia?.(TOUCH_PRIMARY_QUERY) ?? null;
  let layoutKeyboardOpen = false;

  // The pending follow-up read, scheduled after a keyboard-state change.
  let settleTimer: number | null = null;
  const scheduleSettleRecheck = (): void => {
    if (settleTimer !== null) win.clearTimeout(settleTimer);
    settleTimer = win.setTimeout(() => {
      settleTimer = null;
      schedule('settle');
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

  const setLayoutKeyboard = (open: boolean): boolean => {
    if (open === layoutKeyboardOpen) return false;
    layoutKeyboardOpen = open;
    if (open) root.dataset['keyboard'] = 'open';
    else if (appliedHeight === null) delete root.dataset['keyboard'];
    return true;
  };

  const clearKeyboardState = (): boolean => {
    if (appliedHeight === null) return false;
    root.style.removeProperty(APP_HEIGHT_PROPERTY);
    root.style.removeProperty(KEYBOARD_OPEN_PROPERTY);
    if (!layoutKeyboardOpen) delete root.dataset['keyboard'];
    applyFrameSize(null);
    appliedHeight = null;
    return true;
  };

  const updateDiagnostic = (
    eventName: string,
    standalone: boolean,
    focused: boolean,
    guard: string
  ): void => {
    if (!debugEnabled || !debugOverlay) return;
    debugCount++;
    const inlineAppHeight = root.style.getPropertyValue(APP_HEIGHT_PROPERTY).trim();
    const computedAppHeight =
      win.getComputedStyle?.(root)?.getPropertyValue(APP_HEIGHT_PROPERTY)?.trim() ?? '';
    const chatEl = doc.querySelector('scion-page-chat') ?? doc.querySelector('scion-chat-shell');
    const rect = chatEl?.getBoundingClientRect();
    const chatRect = rect
      ? {
          top: Math.round(rect.top),
          height: Math.round(rect.height),
          width: Math.round(rect.width),
        }
      : null;
    lastSnapshot = {
      event: eventName,
      count: debugCount,
      vvHeight: vv.height,
      vvWidth: vv.width,
      vvScale: vv.scale,
      vvOffsetTop: vv.offsetTop,
      innerHeight: win.innerHeight,
      innerWidth: win.innerWidth,
      clientHeight: root.clientHeight,
      standalone,
      editableFocused: focused,
      inlineAppHeight,
      computedAppHeight,
      keyboardDataset: root.dataset['keyboard'] ?? '',
      guard,
      chatRect,
    };
    const chatText = chatRect ? `${chatRect.width}x${chatRect.height}@${chatRect.top}` : 'none';
    debugOverlay.textContent =
      `[vp#${debugCount} ${eventName}] vv:${vv.height}x${vv.width} s:${vv.scale} ` +
      `in:${win.innerHeight} ch:${root.clientHeight} sa:${standalone ? 1 : 0} ` +
      `ed:${focused ? 1 : 0} g:${guard}\n` +
      `--app-h: inline="${inlineAppHeight || 'unset'}" comp="${computedAppHeight || 'unset'}" ` +
      `kb:${root.dataset['keyboard'] ?? '-'} chat:${chatText}`;
  };

  const sync = (eventName: string): void => {
    const vvScale = vv.scale;
    const vvHeight = vv.height;
    const innerHeight = win.innerHeight;
    const atZoom1 = Number.isFinite(vvScale) && Math.abs(vvScale - 1) < ZOOM_1_TOLERANCE;
    const vvHeightPlausible = isPlausibleViewportHeight(vvHeight);
    const innerHeightPlausible = isPlausibleViewportHeight(innerHeight);
    const standalone = isStandaloneMode(win);
    const focused = editableFocused(doc);

    // Guard against implausible visualViewport.height values (<= 0, NaN,
    // Infinity, or tiny values below MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX) and
    // against iOS standalone web clips reporting a shrunken visualViewport
    // when no text field is focused. In both cases, do not shrink
    // `--scion-app-height`; clear it so CSS `100dvh` applies.
    const keyboardInset = vvHeightPlausible && innerHeightPlausible ? innerHeight - vvHeight : 0;
    const standaloneAllowsKeyboard = !standalone || focused;
    const keyboardOpen =
      atZoom1 &&
      vvHeightPlausible &&
      innerHeightPlausible &&
      standaloneAllowsKeyboard &&
      keyboardInset > KEYBOARD_MIN_INSET_PX;
    // A bar shorter than a keyboard: leave Safari's reveal pan alone.
    const shortOccluder =
      vvHeightPlausible &&
      innerHeightPlausible &&
      keyboardInset >= OCCLUDER_MIN_INSET_PX &&
      keyboardInset <= KEYBOARD_MIN_INSET_PX;

    let changed = false;
    if (keyboardOpen) {
      const height = `${vvHeight}px`;
      if (height !== appliedHeight) {
        root.style.setProperty(APP_HEIGHT_PROPERTY, height);
        root.style.setProperty(KEYBOARD_OPEN_PROPERTY, '1');
        root.dataset['keyboard'] = 'open';
        applyFrameSize(
          vvHeight < TIGHT_FRAME_MAX_PX ? 'tight' : vvHeight < SHORT_FRAME_MAX_PX ? 'short' : null
        );
        appliedHeight = height;
        changed = true;
      }
    } else {
      changed = clearKeyboardState();
    }

    // Android: the keyboard shrinks the layout viewport itself, so the two
    // viewports stay equal and the check above never fires. Infer it from a
    // text field holding focus while the layout height sits well below the
    // tallest seen at this width. Only the attribute is set: the frame
    // already follows the smaller layout viewport through 100dvh.
    // Keyed on the whole-pixel width, so a fractional width (a high-DPI
    // screen, or a zoom) neither misses its orientation's baseline nor adds
    // an entry per sub-pixel step.
    let layoutKeyboard = false;
    if (innerHeightPlausible) {
      const width = Math.round(win.innerWidth);
      const tallest = Math.max(tallestAtWidth.get(width) ?? 0, innerHeight);
      tallestAtWidth.set(width, tallest);
      layoutKeyboard =
        !keyboardOpen &&
        atZoom1 &&
        touchPrimary?.matches === true &&
        tallest - innerHeight > KEYBOARD_MIN_INSET_PX &&
        focused;
    }
    if (setLayoutKeyboard(layoutKeyboard)) changed = true;
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

    if (debugEnabled) {
      const guard = !atZoom1
        ? 'zoomed'
        : !vvHeightPlausible
          ? 'implausible-vv'
          : !innerHeightPlausible
            ? 'implausible-inner'
            : !standaloneAllowsKeyboard && keyboardInset > KEYBOARD_MIN_INSET_PX
              ? 'standalone-unfocused'
              : keyboardOpen
                ? 'keyboard-open'
                : 'normal';
      updateDiagnostic(eventName, standalone, focused, guard);
    }
  };

  // One sync per animation frame, however many events arrive.
  let frame: number | null = null;
  function schedule(eventName = 'event'): void {
    pendingEvent = eventName;
    if (frame !== null) return;
    frame = win.requestAnimationFrame(() => {
      frame = null;
      sync(pendingEvent);
    });
  }

  const onVvResize = (): void => schedule('vv:resize');
  const onVvScroll = (): void => schedule('vv:scroll');
  const onWinScroll = (): void => schedule('win:scroll');
  const onWinResize = (): void => schedule('win:resize');
  const onDocEvent = (e: Event): void => schedule(`doc:${e.type}`);

  // focusin, focusout and input are composed, so they reach the document
  // from fields inside shadow roots.
  const docEvents = ['focusin', 'focusout', 'input'] as const;
  vv.addEventListener('resize', onVvResize);
  vv.addEventListener('scroll', onVvScroll);
  win.addEventListener('scroll', onWinScroll, { passive: true });
  win.addEventListener('resize', onWinResize);
  for (const type of docEvents) doc.addEventListener(type, onDocEvent, { capture: true });
  schedule('init');

  return () => {
    vv.removeEventListener('resize', onVvResize);
    vv.removeEventListener('scroll', onVvScroll);
    win.removeEventListener('scroll', onWinScroll);
    win.removeEventListener('resize', onWinResize);
    for (const type of docEvents) doc.removeEventListener(type, onDocEvent, { capture: true });
    if (frame !== null) {
      win.cancelAnimationFrame(frame);
      frame = null;
    }
    if (settleTimer !== null) {
      win.clearTimeout(settleTimer);
      settleTimer = null;
    }
    clearKeyboardState();
    setLayoutKeyboard(false);
    if (debugOverlay) {
      debugOverlay.remove();
      debugOverlay = null;
    }
    if (debugEnabled) {
      delete (win as WindowWithViewportDiag).__scionViewportDiag;
    }
  };
}
