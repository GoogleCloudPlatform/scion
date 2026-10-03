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

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import { APP_FRAME_CLASS } from '../components/shared/app-frame.js';
import { installViewportFrame, KEYBOARD_MIN_INSET_PX } from './viewport.js';

interface FakeVisualViewport extends EventTarget {
  scale: number;
  height: number;
  offsetTop: number;
}

interface FakeWindow extends EventTarget {
  visualViewport: FakeVisualViewport | null;
  innerHeight: number;
  scrollY: number;
  document: Document;
  scrollTo: ReturnType<typeof vi.fn>;
  requestAnimationFrame: (cb: FrameRequestCallback) => number;
  cancelAnimationFrame: ReturnType<typeof vi.fn>;
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
  });
  root().removeAttribute('style');
  root().removeAttribute('data-keyboard');
  root().classList.remove(APP_FRAME_CLASS);
});

afterEach(() => {
  dispose?.();
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

  it('is a no-op on Android with resizes-content, where both viewports shrink together', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    win.innerHeight = 480;
    emit(vv, 'resize', { height: 480 });

    expect(root().hasAttribute('style')).toBe(false);
    expect(root().hasAttribute('data-keyboard')).toBe(false);
    expect(win.scrollTo).not.toHaveBeenCalled();
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

  it('leaves an unscrolled frame alone', () => {
    root().classList.add(APP_FRAME_CLASS);
    install();
    emit(vv, 'resize', { height: 480 });

    expect(win.scrollTo).not.toHaveBeenCalled();
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
    expect(winRemove.mock.calls.map(([type]) => type)).toEqual(['scroll']);

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
