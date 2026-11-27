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
 * Tests for the visual-viewport frame module, against a mocked
 * `visualViewport` and a manually flushed animation-frame queue.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import { APP_FRAME_CLASS } from '../components/shared/app-frame.js';
import {
  installViewportFrame,
  isPlausibleViewportHeight,
  isViewportDebugEnabled,
  KEYBOARD_MIN_INSET_PX,
  MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX,
  SETTLE_RECHECK_MS,
  SHORT_FRAME_MAX_PX,
  STANDALONE_DISPLAY_QUERY,
  TIGHT_FRAME_MAX_PX,
  VIEWPORT_DEBUG_STORAGE_KEY,
  type ViewportDiagnosticSnapshot,
} from './viewport.js';
import { TOUCH_PRIMARY_QUERY } from '../utils/input-modality.js';

interface FakeVisualViewport extends EventTarget {
  scale: number;
  height: number;
  width?: number;
  offsetTop: number;
}

interface FakeWindow extends EventTarget {
  visualViewport: FakeVisualViewport | null;
  innerHeight: number;
  innerWidth?: number;
  matchMedia?: (query: string) => { matches: boolean };
  navigator?: { standalone?: boolean };
  location?: { search: string };
  localStorage?: Pick<Storage, 'getItem'>;
  getComputedStyle?: (el: Element) => { getPropertyValue: (prop: string) => string };
  __scionViewportDiag?: () => ViewportDiagnosticSnapshot;
  scrollY: number;
  document: Document;
  scrollTo: ReturnType<typeof vi.fn>;
  requestAnimationFrame: (cb: FrameRequestCallback) => number;
  cancelAnimationFrame: ReturnType<typeof vi.fn>;
  setTimeout: (cb: () => void, ms: number) => number;
  clearTimeout: (id: number) => void;
}

const LAYOUT_HEIGHT = 812;

let vv: FakeVisualViewport;
let win: FakeWindow;
let frames: Map<number, FrameRequestCallback>;
let nextFrameId: number;
let dispose: (() => void) | null;

const root = (): HTMLElement => document.documentElement;

/** Run every queued animation-frame callback, as the browser would next frame. */
function flushFrames(): void {
  const pending = [...frames.values()];
  frames.clear();
  for (const cb of pending) cb(0);
}

/** Change the fake viewport, fire its event, and let the next frame run. */
function emit(
  target: EventTarget,
  type: 'resize' | 'scroll',
  changes: Partial<FakeVisualViewport> = {}
): void {
  Object.assign(vv, changes);
  target.dispatchEvent(new Event(type));
  flushFrames();
}

function install(): void {
  dispose = installViewportFrame(win as unknown as Window);
  flushFrames();
}

beforeEach(() => {
  vi.useFakeTimers();
  frames = new Map();
  nextFrameId = 1;
  dispose = null;
  vv = Object.assign(new EventTarget(), { scale: 1, height: LAYOUT_HEIGHT, offsetTop: 0 });
  win = Object.assign(new EventTarget(), {
    visualViewport: vv,
    innerHeight: LAYOUT_HEIGHT,
    scrollY: 0,
    document,
    scrollTo: vi.fn(),
    requestAnimationFrame: (cb: FrameRequestCallback): number => {
      const id = nextFrameId++;
      frames.set(id, cb);
      return id;
    },
    cancelAnimationFrame: vi.fn((id: number) => {
      frames.delete(id);
    }),
    setTimeout: (cb: () => void, ms: number) => window.setTimeout(cb, ms),
    clearTimeout: (id: number) => window.clearTimeout(id),
  });
  root().removeAttribute('style');
  root().removeAttribute('data-keyboard');
  root().classList.remove(APP_FRAME_CLASS);
});

afterEach(() => {
  dispose?.();
  vi.useRealTimers();
  document.body.replaceChildren();
  root().removeAttribute('style');
  root().removeAttribute('data-keyboard');
  root().classList.remove(APP_FRAME_CLASS);
});

describe('installViewportFrame: keyboard state', () => {
  it('sets the frame height, data-keyboard and --scion-kb-open at scale 1 with a keyboard-sized inset', () => {
    install();
    emit(vv, 'resize', { height: 480 });

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('1');
    expect(root().dataset['keyboard']).toBe('open');
  });

  it('follows the visual viewport height while the keyboard stays open', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    emit(vv, 'resize', { height: 430 });

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('430px');
  });

  it('treats an inset at or below the threshold as no keyboard', () => {
    install();
    emit(vv, 'resize', { height: LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();

    emit(vv, 'resize', { height: LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX - 1 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe(
      `${LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX - 1}px`
    );
  });

  it('clears everything when the keyboard closes', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    emit(vv, 'resize', { height: LAYOUT_HEIGHT });

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('follows a rotation with the keyboard open, and clears when the keyboard closes after it', () => {
    install();
    emit(vv, 'resize', { height: 480 });

    // Portrait to landscape: both viewports change and the window resizes.
    win.innerHeight = 375;
    emit(win, 'resize', { height: 160 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('160px');
    expect(root().dataset['keyboard']).toBe('open');

    emit(vv, 'resize', { height: 375 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('does not count a rotation with no keyboard as a keyboard', () => {
    install();
    win.innerHeight = 375;
    emit(win, 'resize', { height: 375 });
    win.innerHeight = LAYOUT_HEIGHT;
    emit(win, 'resize', { height: LAYOUT_HEIGHT });

    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('does nothing while pinch-zoomed, even with a large inset', () => {
    install();
    emit(vv, 'resize', { scale: 2, height: 400 });

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('clears the keyboard state when the user zooms in with the keyboard open', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    emit(vv, 'resize', { scale: 1.5, height: 320 });

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('is a no-op on desktop: no inline style, no data attribute, no scroll', () => {
    install();
    emit(vv, 'resize', { height: LAYOUT_HEIGHT });
    emit(win, 'scroll');

    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
    expect(win.scrollTo).not.toHaveBeenCalled();
  });

  it('sets no frame state on Android with resizes-content, where both viewports shrink together', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.innerHeight = 480;
    emit(vv, 'resize', { height: 480 });

    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
    expect(win.scrollTo).not.toHaveBeenCalled();
  });
});

describe('installViewportFrame: Android keyboard (resizes-content)', () => {
  const PORTRAIT_WIDTH = 375;

  /** A touch-primary (or, with `touch` false, desktop) window in portrait. */
  function installAndroid(touch = true): void {
    win.innerWidth = PORTRAIT_WIDTH;
    win.matchMedia = (query: string) => ({
      matches: touch && query === TOUCH_PRIMARY_QUERY,
    });
    install();
  }

  /** Both viewports change together, as resizes-content does, and the window resizes. */
  function resizeLayout(height: number, width = win.innerWidth ?? PORTRAIT_WIDTH): void {
    win.innerHeight = height;
    win.innerWidth = width;
    emit(win, 'resize', { height });
  }

  function focusField(tag: 'textarea' | 'input' = 'textarea', type = 'text'): HTMLElement {
    const field = document.createElement(tag);
    if (field instanceof HTMLInputElement) field.type = type;
    document.body.append(field);
    field.focus();
    flushFrames();
    return field;
  }

  it('sets data-keyboard, and nothing else, when a focused field and a shrunk layout show the keyboard', () => {
    installAndroid();
    focusField();
    resizeLayout(480);

    expect(root().dataset['keyboard']).toBe('open');
    expect(root().hasAttribute('style')).toBe(false);
  });

  it('clears data-keyboard when the layout grows back', () => {
    installAndroid();
    focusField();
    resizeLayout(480);
    resizeLayout(LAYOUT_HEIGHT);

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('clears data-keyboard when the field loses focus', () => {
    installAndroid();
    const field = focusField();
    resizeLayout(480);
    field.blur();
    flushFrames();

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('sees a focused textarea inside a shadow root, as xterm uses', () => {
    installAndroid();
    const host = document.createElement('div');
    document.body.append(host);
    const field = document.createElement('textarea');
    host.attachShadow({ mode: 'open' }).append(field);
    field.focus();
    resizeLayout(480);

    expect(root().dataset['keyboard']).toBe('open');
  });

  it('does not count a shrink with no field focused, such as a split-screen resize', () => {
    installAndroid();
    resizeLayout(480);

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('does not count a focused control that takes no text', () => {
    installAndroid();
    focusField('input', 'checkbox');
    resizeLayout(480);

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('treats a shrink at or below the threshold as no keyboard', () => {
    installAndroid();
    focusField();
    resizeLayout(LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX);
    expect(root().hasAttribute('data-keyboard')).toBe(false);

    resizeLayout(LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX - 1);
    expect(root().dataset['keyboard']).toBe('open');
  });

  it('never triggers on a desktop, even with a focused field and a much shorter window', () => {
    installAndroid(false);
    focusField();
    resizeLayout(480);

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('never triggers with a hardware keyboard, which leaves the layout height alone', () => {
    installAndroid();
    focusField();
    emit(win, 'resize');

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('keeps a baseline per orientation across rotations', () => {
    installAndroid();
    focusField();
    resizeLayout(480);
    expect(root().dataset['keyboard']).toBe('open');

    // To landscape, keyboard closed: the new width starts its own baseline.
    resizeLayout(PORTRAIT_WIDTH, LAYOUT_HEIGHT);
    expect(root().hasAttribute('data-keyboard')).toBe(false);

    // The keyboard opens in landscape.
    resizeLayout(160, LAYOUT_HEIGHT);
    expect(root().dataset['keyboard']).toBe('open');

    // Back to portrait with the keyboard up: portrait's own baseline holds.
    resizeLayout(480, PORTRAIT_WIDTH);
    expect(root().dataset['keyboard']).toBe('open');
    resizeLayout(LAYOUT_HEIGHT, PORTRAIT_WIDTH);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('keeps one baseline across fractional widths that round to the same pixel', () => {
    installAndroid();
    resizeLayout(LAYOUT_HEIGHT, PORTRAIT_WIDTH + 0.4);
    focusField();
    // The keyboard opens at a width that differs only below a pixel.
    resizeLayout(480, PORTRAIT_WIDTH - 0.4);

    expect(root().dataset['keyboard']).toBe('open');
  });

  it('does nothing while pinch-zoomed', () => {
    installAndroid();
    focusField();
    win.innerHeight = 480;
    emit(win, 'resize', { height: 240, scale: 2 });

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('the disposer clears data-keyboard', () => {
    installAndroid();
    focusField();
    resizeLayout(480);
    dispose?.();
    dispose = null;

    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });
});

describe('installViewportFrame: install-time sync', () => {
  it('applies the keyboard state on install, before any viewport event', () => {
    vv.height = 480;
    install();

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');
    expect(root().dataset['keyboard']).toBe('open');
  });

  it('resets an existing window scroll on install in frame mode', () => {
    root().classList.add(APP_FRAME_CLASS);
    win.scrollY = 120;
    install();

    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });
});

describe('installViewportFrame: scroll reset', () => {
  it('resets a window scroll to 0 in frame mode', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.scrollY = 120;
    emit(win, 'scroll');

    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('resets a visual-viewport pan to 0 in frame mode', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    emit(vv, 'scroll', { height: 480, offsetTop: 210 });

    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('never scrolls a document-scrolling page (no frame mode)', () => {
    install();
    win.scrollY = 120;
    emit(win, 'scroll');
    emit(vv, 'scroll', { offsetTop: 40 });

    expect(win.scrollTo).not.toHaveBeenCalled();
  });

  it('never fights a pinch-pan, even in frame mode', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    emit(vv, 'scroll', { scale: 2, offsetTop: 300 });

    expect(win.scrollTo).not.toHaveBeenCalled();
  });

  it('keeps the browser pan while a bar shorter than a keyboard covers the page', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.scrollY = 90;
    emit(vv, 'scroll', { height: LAYOUT_HEIGHT - 60, offsetTop: 60 });

    expect(win.scrollTo).not.toHaveBeenCalled();
    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);

    // Once the bar goes away, the leftover pan is reset.
    emit(vv, 'resize', { height: LAYOUT_HEIGHT, offsetTop: 0 });
    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('keeps the browser pan for a bar exactly as tall as the keyboard threshold', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.scrollY = 90;
    emit(vv, 'scroll', { height: LAYOUT_HEIGHT - KEYBOARD_MIN_INSET_PX });

    // Not a keyboard (that needs more than the threshold), so the frame
    // keeps its full height, and still a short bar, so the pan stays.
    expect(win.scrollTo).not.toHaveBeenCalled();
    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
  });

  it('treats a sub-pixel shortfall as nothing covering the page', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.scrollY = 90;
    emit(vv, 'scroll', { height: LAYOUT_HEIGHT - 0.5 });

    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('ignores a sub-pixel window scroll but resets a whole-pixel one', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.scrollY = 0.3;
    emit(win, 'scroll');
    expect(win.scrollTo).not.toHaveBeenCalled();

    win.scrollY = 2;
    emit(win, 'scroll');
    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('ignores a sub-pixel visual-viewport offset but resets a whole-pixel one', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    emit(vv, 'scroll', { offsetTop: 0.3 });
    expect(win.scrollTo).not.toHaveBeenCalled();

    emit(vv, 'scroll', { offsetTop: 2 });
    expect(win.scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it('leaves an unscrolled frame alone', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    emit(vv, 'resize', { height: 480 });

    expect(win.scrollTo).not.toHaveBeenCalled();
  });
});

/** Shrink the visual viewport without any event, as a late accessory bar can. */
function shrinkSilently(height: number): void {
  vv.height = height;
}

/** A textarea inside an open shadow root, like the chat composer's. */
function shadowTextarea(): HTMLTextAreaElement {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const textarea = document.createElement('textarea');
  host.attachShadow({ mode: 'open' }).appendChild(textarea);
  return textarea;
}

describe('installViewportFrame: changes without a viewport event', () => {
  it('catches a late accessory bar with a follow-up read after the keyboard opens', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    // The AutoFill bar slides in above the keyboard; no viewport event follows.
    shrinkSilently(430);
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');

    vi.advanceTimersByTime(SETTLE_RECHECK_MS);
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('430px');
  });

  it('reads again shortly after the keyboard closes, too', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    vi.advanceTimersByTime(SETTLE_RECHECK_MS);
    flushFrames();
    emit(vv, 'resize', { height: LAYOUT_HEIGHT });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    // The visible height changes again with no event (a bar sliding in).
    shrinkSilently(480);
    vi.advanceTimersByTime(SETTLE_RECHECK_MS);
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');
  });

  it('keeps a single follow-up read when changes come faster than it', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    vi.advanceTimersByTime(SETTLE_RECHECK_MS / 2);
    emit(vv, 'resize', { height: 430 });

    expect(vi.getTimerCount()).toBe(1);
    // It was re-armed by the second change, not left on the first one's clock.
    vi.advanceTimersByTime(SETTLE_RECHECK_MS / 2);
    expect(frames.size).toBe(0);
    vi.advanceTimersByTime(SETTLE_RECHECK_MS / 2);
    expect(frames.size).toBe(1);
  });

  it('stops the follow-up reads once the height holds', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    vi.advanceTimersByTime(SETTLE_RECHECK_MS);
    flushFrames();

    expect(vi.getTimerCount()).toBe(0);
    expect(frames.size).toBe(0);
  });

  it('never arms a follow-up read on desktop', () => {
    install();
    emit(vv, 'resize', { height: LAYOUT_HEIGHT });
    emit(win, 'resize');

    expect(vi.getTimerCount()).toBe(0);
  });

  it('re-reads the viewport on text input in a field inside a shadow root', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    vi.advanceTimersByTime(SETTLE_RECHECK_MS);
    flushFrames();
    shrinkSilently(430);

    shadowTextarea().dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('430px');
  });

  it('re-reads the viewport when focus moves into a field', () => {
    install();
    shrinkSilently(480);

    shadowTextarea().dispatchEvent(new FocusEvent('focusin', { bubbles: true, composed: true }));
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');
    expect(root().dataset['keyboard']).toBe('open');
  });

  it('re-reads the viewport on a window resize', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    shrinkSilently(430);

    emit(win, 'resize');
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('430px');
  });

  it('the disposer removes the document listeners', () => {
    install();
    dispose!();
    dispose = null;
    shrinkSilently(480);

    shadowTextarea().dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    expect(frames.size).toBe(0);
  });
});

/** The short/tight frame state on the root, as one comparable value. */
function frameState(): Record<string, string | undefined> {
  const style = root().style;
  const get = (name: string): string | undefined => style.getPropertyValue(name) || undefined;
  return {
    short: get('--scion-kb-short'),
    shortDisplay: get('--scion-kb-short-display'),
    tight: get('--scion-kb-tight'),
    tightPosition: get('--scion-kb-tight-position'),
    tightVisibility: get('--scion-kb-tight-visibility'),
  };
}

const NO_FRAME_STATE = {
  short: undefined,
  shortDisplay: undefined,
  tight: undefined,
  tightPosition: undefined,
  tightVisibility: undefined,
};
const SHORT_STATE = { ...NO_FRAME_STATE, short: '1', shortDisplay: 'none' };
const TIGHT_STATE = {
  short: '1',
  shortDisplay: 'none',
  tight: '1',
  tightPosition: 'absolute',
  tightVisibility: 'hidden',
};

describe('installViewportFrame: short and tight frames', () => {
  it('marks nothing while the keyboard leaves a roomy frame', () => {
    install();
    emit(vv, 'resize', { height: SHORT_FRAME_MAX_PX });
    expect(root().dataset['keyboard']).toBe('open');
    expect(frameState()).toEqual(NO_FRAME_STATE);
  });

  it('marks a frame under the short limit as short', () => {
    install();
    emit(vv, 'resize', { height: SHORT_FRAME_MAX_PX - 1 });
    expect(frameState()).toEqual(SHORT_STATE);

    emit(vv, 'resize', { height: TIGHT_FRAME_MAX_PX });
    expect(frameState()).toEqual(SHORT_STATE);
  });

  it('marks a frame under the tight limit as tight, which implies short', () => {
    install();
    emit(vv, 'resize', { height: TIGHT_FRAME_MAX_PX - 1 });
    expect(frameState()).toEqual(TIGHT_STATE);
  });

  it('follows the frame back up through short to roomy', () => {
    install();
    emit(vv, 'resize', { height: 150 });
    expect(frameState()).toEqual(TIGHT_STATE);
    emit(vv, 'resize', { height: 300 });
    expect(frameState()).toEqual(SHORT_STATE);
    emit(vv, 'resize', { height: 480 });
    expect(frameState()).toEqual(NO_FRAME_STATE);
  });

  it('clears the frame state when the keyboard closes', () => {
    install();
    emit(vv, 'resize', { height: 150 });
    emit(vv, 'resize', { height: LAYOUT_HEIGHT });
    expect(frameState()).toEqual(NO_FRAME_STATE);
  });

  it('clears the frame state when the page is zoomed', () => {
    install();
    emit(vv, 'resize', { height: 150 });
    emit(vv, 'resize', { scale: 2, height: 100 });
    expect(frameState()).toEqual(NO_FRAME_STATE);
  });

  it('the disposer clears the frame state', () => {
    install();
    emit(vv, 'resize', { height: 150 });
    dispose!();
    dispose = null;
    expect(frameState()).toEqual(NO_FRAME_STATE);
  });
});

describe('installViewportFrame: throttling and teardown', () => {
  it('coalesces a burst of events into one sync per animation frame', () => {
    install();
    const setProperty = vi.spyOn(root().style, 'setProperty');
    vv.height = 480;
    vv.dispatchEvent(new Event('resize'));
    vv.dispatchEvent(new Event('scroll'));
    win.dispatchEvent(new Event('scroll'));
    vv.dispatchEvent(new Event('resize'));

    expect(frames.size).toBe(1);
    expect(setProperty).not.toHaveBeenCalled();
    flushFrames();
    expect(setProperty).toHaveBeenCalledWith('--scion-app-height', '480px');
    expect(setProperty.mock.calls.filter(([name]) => name === '--scion-app-height')).toHaveLength(
      1
    );
  });

  it('the disposer removes every listener, so later events do nothing', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    const vvRemove = vi.spyOn(vv, 'removeEventListener');
    const winRemove = vi.spyOn(win, 'removeEventListener');
    dispose!();
    dispose = null;

    expect(vvRemove.mock.calls.map(([type]) => type).sort()).toEqual(['resize', 'scroll']);
    expect(winRemove.mock.calls.map(([type]) => type).sort()).toEqual(['resize', 'scroll']);

    vv.height = 480;
    win.scrollY = 50;
    vv.dispatchEvent(new Event('resize'));
    vv.dispatchEvent(new Event('scroll'));
    win.dispatchEvent(new Event('scroll'));
    expect(frames.size).toBe(0);
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(win.scrollTo).not.toHaveBeenCalled();
  });

  it('the disposer cancels a pending frame and clears the keyboard state', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    vv.dispatchEvent(new Event('resize'));
    expect(frames.size).toBe(1);

    dispose!();
    dispose = null;

    expect(win.cancelAnimationFrame).toHaveBeenCalledTimes(1);
    expect(frames.size).toBe(0);
    expect(vi.getTimerCount(), 'the follow-up read is cancelled').toBe(0);
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('returns a harmless disposer when visualViewport is unavailable', () => {
    win.visualViewport = null;
    const addListener = vi.spyOn(win, 'addEventListener');
    const off = installViewportFrame(win as unknown as Window);

    expect(addListener).not.toHaveBeenCalled();
    expect(frames.size).toBe(0);
    expect(() => off()).not.toThrow();
  });
});

describe('installViewportFrame: implausible viewport heights and standalone mode', () => {
  it('validates viewport height plausibility', () => {
    expect(isPlausibleViewportHeight(0)).toBe(false);
    expect(isPlausibleViewportHeight(-50)).toBe(false);
    expect(isPlausibleViewportHeight(Number.NaN)).toBe(false);
    expect(isPlausibleViewportHeight(Number.POSITIVE_INFINITY)).toBe(false);
    expect(isPlausibleViewportHeight(Number.NEGATIVE_INFINITY)).toBe(false);
    expect(isPlausibleViewportHeight(MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX - 1)).toBe(false);
    expect(isPlausibleViewportHeight(MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX)).toBe(true);
    expect(isPlausibleViewportHeight(480)).toBe(true);
  });

  it.each([
    ['zero', 0],
    ['negative', -20],
    ['NaN', Number.NaN],
    ['Infinity', Number.POSITIVE_INFINITY],
    ['implausibly small', MIN_PLAUSIBLE_VIEWPORT_HEIGHT_PX - 1],
  ])('ignores %s visualViewport.height and leaves --scion-app-height unset', (_label, height) => {
    vv.height = height;
    install();

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('clears a previously applied --scion-app-height if visualViewport.height becomes implausible', () => {
    install();
    emit(vv, 'resize', { height: 480 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');

    emit(vv, 'resize', { height: 0 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('ignores implausible window.innerHeight values', () => {
    win.innerHeight = 0;
    vv.height = 480;
    install();

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('in standalone mode without a focused input, ignores a shrunken visualViewport.height', () => {
    win.matchMedia = (query: string) => ({
      matches: query === STANDALONE_DISPLAY_QUERY,
    });
    vv.height = 480;
    install();

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();

    // Pinch-zoom and return to scale 1: still does not collapse the app height.
    emit(vv, 'resize', { scale: 1.5, height: 480 });
    emit(vv, 'resize', { scale: 1, height: 480 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('in navigator.standalone mode without a focused input, ignores a shrunken visualViewport.height', () => {
    win.navigator = { standalone: true };
    vv.height = 480;
    install();

    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('in standalone mode with a focused editable field, preserves legitimate keyboard resizing', () => {
    win.matchMedia = (query: string) => ({
      matches: query === STANDALONE_DISPLAY_QUERY,
    });
    const field = document.createElement('textarea');
    document.body.append(field);
    field.focus();

    install();
    emit(vv, 'resize', { height: 480 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('480px');
    expect(root().style.getPropertyValue('--scion-kb-open')).toBe('1');
    expect(root().dataset['keyboard']).toBe('open');

    // Blurring the field clears the keyboard height even if vv.height lags.
    field.blur();
    flushFrames();
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });

  it('in standalone mode with a focused editable field, still guards against 0px visualViewport.height', () => {
    win.matchMedia = (query: string) => ({
      matches: query === STANDALONE_DISPLAY_QUERY,
    });
    const field = document.createElement('textarea');
    document.body.append(field);
    field.focus();

    install();
    emit(vv, 'resize', { height: 0 });
    expect(root().style.getPropertyValue('--scion-app-height')).toBe('');
    expect(root().dataset['keyboard']).toBeUndefined();
  });
});

describe('installViewportFrame: opt-in viewport diagnostic', () => {
  it('is inactive by default: no overlay element and no global helper', () => {
    install();

    expect(isViewportDebugEnabled(win as unknown as Window)).toBe(false);
    expect(document.getElementById('scion-viewport-debug')).toBeNull();
    expect(win.__scionViewportDiag).toBeUndefined();
  });

  it('activates via ?viewportDebug=1, reports measurements and updates on resize/zoom, and cleans up on dispose', () => {
    win.location = { search: '?viewportDebug=1' };
    win.matchMedia = (query: string) => ({
      matches: query === STANDALONE_DISPLAY_QUERY,
    });
    win.getComputedStyle = () => ({
      getPropertyValue: (prop: string) => (prop === '--scion-app-height' ? '100dvh' : ''),
    });
    vv.width = 390;
    vv.height = 0;

    const chatEl = document.createElement('scion-page-chat');
    chatEl.getBoundingClientRect = () =>
      ({
        top: 56,
        height: 756,
        width: 390,
        bottom: 812,
        left: 0,
        right: 390,
        x: 0,
        y: 56,
        toJSON: () => ({}),
      }) as DOMRect;
    document.body.append(chatEl);

    install();

    const overlay = document.getElementById('scion-viewport-debug');
    expect(overlay).not.toBeNull();
    expect(overlay?.style.pointerEvents).toBe('none');
    expect(win.__scionViewportDiag).toBeTypeOf('function');

    const initial = win.__scionViewportDiag!();
    expect(initial.event).toBe('init');
    expect(initial.vvHeight).toBe(0);
    expect(initial.vvScale).toBe(1);
    expect(initial.innerHeight).toBe(LAYOUT_HEIGHT);
    expect(initial.standalone).toBe(true);
    expect(initial.inlineAppHeight).toBe('');
    expect(initial.computedAppHeight).toBe('100dvh');
    expect(initial.guard).toBe('implausible-vv');
    expect(initial.chatRect).toEqual({ top: 56, height: 756, width: 390 });
    expect(overlay?.textContent).toContain('g:implausible-vv');

    emit(vv, 'resize', { scale: 1.5, height: 500 });
    const zoomed = win.__scionViewportDiag!();
    expect(zoomed.event).toBe('vv:resize');
    expect(zoomed.vvScale).toBe(1.5);
    expect(zoomed.guard).toBe('zoomed');
    expect(zoomed.count).toBeGreaterThan(initial.count);

    dispose!();
    dispose = null;
    expect(document.getElementById('scion-viewport-debug')).toBeNull();
    expect(win.__scionViewportDiag).toBeUndefined();
  });

  it('activates via localStorage flag and can be disabled by ?viewportDebug=0', () => {
    win.localStorage = {
      getItem: (key: string) => (key === VIEWPORT_DEBUG_STORAGE_KEY ? '1' : null),
    };
    expect(isViewportDebugEnabled(win as unknown as Window)).toBe(true);

    win.location = { search: '?viewportDebug=0' };
    expect(isViewportDebugEnabled(win as unknown as Window)).toBe(false);
  });
});
