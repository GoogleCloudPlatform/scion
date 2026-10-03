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

// @vitest-environment happy-dom

import type { ReactiveController, ReactiveControllerHost } from 'lit';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';

import {
  LONG_PRESS_MS,
  LONG_PRESS_SLOP_PX,
  LongPressController,
  type LongPressPoint,
} from './long-press.js';

function pointer(
  type: string,
  init: { x?: number; y?: number; pointerType?: string; pointerId?: number } = {}
): PointerEvent {
  return new PointerEvent(type, {
    bubbles: true,
    composed: true,
    cancelable: true,
    pointerType: init.pointerType ?? 'touch',
    pointerId: init.pointerId ?? 1,
    isPrimary: true,
    clientX: init.x ?? 100,
    clientY: init.y ?? 100,
  });
}

describe('LongPressController', () => {
  let row: HTMLElement;
  let controller: LongPressController;
  let onFire: Mock<(point: LongPressPoint) => void>;
  let rowClicks: number;

  beforeEach(() => {
    vi.useFakeTimers();
    row = document.createElement('div');
    document.body.appendChild(row);
    controller = new LongPressController();
    onFire = vi.fn<(point: LongPressPoint) => void>();
    rowClicks = 0;
    row.addEventListener('pointerdown', (e) => controller.pointerDown(e, onFire));
    row.addEventListener('click', () => rowClicks++);
  });

  afterEach(() => {
    controller.dispose();
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  it('reports a pending touch press as pressing, until it fires or ends', () => {
    expect(controller.pressing).toBe(false);
    row.dispatchEvent(pointer('pointerdown'));
    expect(controller.pressing).toBe(true);
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(controller.pressing).toBe(false);

    row.dispatchEvent(pointer('pointerdown', { pointerId: 2 }));
    expect(controller.pressing).toBe(true);
    window.dispatchEvent(pointer('pointerup', { pointerId: 2 }));
    expect(controller.pressing).toBe(false);

    row.dispatchEvent(pointer('pointerdown', { pointerType: 'mouse', pointerId: 3 }));
    expect(controller.pressing).toBe(false);
  });

  it('fires after LONG_PRESS_MS with the press point', () => {
    row.dispatchEvent(pointer('pointerdown', { x: 40, y: 60 }));
    vi.advanceTimersByTime(LONG_PRESS_MS - 1);
    expect(onFire).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(onFire).toHaveBeenCalledOnce();
    expect(onFire).toHaveBeenCalledWith({ x: 40, y: 60 });
  });

  it('ignores mouse and pen presses', () => {
    row.dispatchEvent(pointer('pointerdown', { pointerType: 'mouse' }));
    row.dispatchEvent(pointer('pointerdown', { pointerType: 'pen' }));
    vi.advanceTimersByTime(LONG_PRESS_MS * 2);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('cancels when the finger lifts early', () => {
    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_MS / 2);
    row.dispatchEvent(pointer('pointerup'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('cancels on pointercancel', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointercancel'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('tolerates movement within the slop but cancels beyond it', () => {
    row.dispatchEvent(pointer('pointerdown', { x: 100, y: 100 }));
    row.dispatchEvent(pointer('pointermove', { x: 100 + LONG_PRESS_SLOP_PX, y: 100 }));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).toHaveBeenCalledOnce();

    onFire.mockClear();
    row.dispatchEvent(pointer('pointerup'));
    row.dispatchEvent(pointer('pointerdown', { x: 100, y: 100, pointerId: 2 }));
    row.dispatchEvent(
      pointer('pointermove', { x: 100, y: 100 + LONG_PRESS_SLOP_PX + 1, pointerId: 2 })
    );
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('cancels when a scroll container under the finger scrolls', () => {
    const scroller = document.createElement('div');
    scroller.style.overflowY = 'auto';
    document.body.appendChild(scroller);
    scroller.appendChild(row);

    row.dispatchEvent(pointer('pointerdown'));
    scroller.dispatchEvent(new Event('scroll'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('cancels when a scroll container inside a shadow root scrolls', () => {
    // A scroll event stays inside the shadow root it was fired in, so a
    // window listener alone would never see this one.
    const host = document.createElement('div');
    document.body.appendChild(host);
    const scroller = document.createElement('div');
    scroller.style.overflowY = 'auto';
    host.attachShadow({ mode: 'open' }).appendChild(scroller);
    scroller.appendChild(row);
    const onWindowScroll = vi.fn();
    window.addEventListener('scroll', onWindowScroll, true);
    try {
      row.dispatchEvent(pointer('pointerdown'));
      scroller.dispatchEvent(new Event('scroll'));
      vi.advanceTimersByTime(LONG_PRESS_MS);
      expect(onWindowScroll).not.toHaveBeenCalled();
      expect(onFire).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('scroll', onWindowScroll, true);
    }
  });

  it('swallows the click that follows a fired long-press, and only that one', () => {
    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    row.dispatchEvent(pointer('pointerup'));
    const tail = new MouseEvent('click', { bubbles: true, cancelable: true });
    row.dispatchEvent(tail);
    expect(rowClicks).toBe(0);
    expect(tail.defaultPrevented).toBe(true);

    row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(rowClicks).toBe(1);
  });

  it('lets a short tap click through', () => {
    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(100);
    row.dispatchEvent(pointer('pointerup'));
    row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(rowClicks).toBe(1);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('stops swallowing shortly after lift when no click arrives', () => {
    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    row.dispatchEvent(pointer('pointerup'));
    vi.advanceTimersByTime(1000);
    row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(rowClicks).toBe(1);
  });

  it('does not swallow a tap on something else, such as a sheet item', () => {
    const item = document.createElement('button');
    let itemClicks = 0;
    item.addEventListener('click', () => itemClicks++);
    document.body.appendChild(item);

    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    // The finger slides off and lifts; no click is dispatched for it.
    row.dispatchEvent(pointer('pointermove', { x: 300, y: 300 }));
    row.dispatchEvent(pointer('pointercancel'));
    item.dispatchEvent(pointer('pointerdown', { pointerId: 2 }));
    item.dispatchEvent(pointer('pointerup', { pointerId: 2 }));
    item.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(itemClicks).toBe(1);
  });

  describe('contextmenu de-duplication', () => {
    it('suppresses contextmenu after the long-press fired', () => {
      row.dispatchEvent(pointer('pointerdown'));
      vi.advanceTimersByTime(LONG_PRESS_MS);
      const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
      expect(controller.contextMenu(e)).toBe(true);
      expect(e.defaultPrevented).toBe(true);
      expect(onFire).toHaveBeenCalledOnce();
    });

    it('lets contextmenu win when it arrives first, and cancels the timer', () => {
      row.dispatchEvent(pointer('pointerdown'));
      vi.advanceTimersByTime(LONG_PRESS_MS - 50);
      const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
      expect(controller.contextMenu(e)).toBe(false);
      vi.advanceTimersByTime(LONG_PRESS_MS);
      expect(onFire).not.toHaveBeenCalled();

      // Its click tail is swallowed too.
      row.dispatchEvent(pointer('pointerup'));
      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      expect(rowClicks).toBe(0);
    });

    it('passes a plain right-click through', () => {
      const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
      expect(controller.contextMenu(e)).toBe(false);
      expect(e.defaultPrevented).toBe(false);
    });

    it('treats a right-click after a finished long-press as a fresh request', () => {
      row.dispatchEvent(pointer('pointerdown'));
      vi.advanceTimersByTime(LONG_PRESS_MS);
      row.dispatchEvent(pointer('pointerup'));
      vi.advanceTimersByTime(1000);
      row.dispatchEvent(pointer('pointerdown', { pointerType: 'mouse', pointerId: 5 }));
      const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
      expect(controller.contextMenu(e)).toBe(false);
    });
  });

  it('dispose removes pending timers', () => {
    row.dispatchEvent(pointer('pointerdown'));
    controller.dispose();
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('a host disconnecting after a fired long-press leaves no click swallowing behind', () => {
    const controllers: ReactiveController[] = [];
    const host: ReactiveControllerHost = {
      addController: (c) => controllers.push(c),
      removeController: () => {},
      requestUpdate: () => {},
      updateComplete: Promise.resolve(true),
    };
    const hosted = new LongPressController(host);
    expect(controllers).toEqual([hosted]);
    const other = document.createElement('button');
    document.body.appendChild(other);
    let otherClicks = 0;
    other.addEventListener('click', () => otherClicks++);

    // Its own row: the shared controller must not take part.
    const hostedRow = document.createElement('div');
    document.body.appendChild(hostedRow);
    hostedRow.addEventListener('pointerdown', (e) => hosted.pointerDown(e, onFire));
    hostedRow.dispatchEvent(pointer('pointerdown', { pointerId: 7 }));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(onFire).toHaveBeenCalled();

    // The finger is still down when the host goes away.
    for (const c of controllers) c.hostDisconnected?.();

    const click = new MouseEvent('click', { bubbles: true, cancelable: true });
    other.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(false);
    expect(otherClicks).toBe(1);
  });
});
