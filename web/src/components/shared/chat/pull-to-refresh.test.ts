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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { PullToRefreshController, PULL_THRESHOLD_PX, type PullState } from './pull-to-refresh.js';

interface Point {
  x?: number;
  y: number;
  id?: number;
}

/** A touch event carrying `touches`, dispatched on `target`; returns it for inspection. */
function touch(target: EventTarget, type: string, points: Point[]): Event {
  const e = new Event(type, { bubbles: true, cancelable: true, composed: true });
  const touches = points.map((p) => ({ clientX: p.x ?? 0, clientY: p.y, identifier: p.id ?? 0 }));
  Object.defineProperty(e, 'touches', { value: touches });
  target.dispatchEvent(e);
  return e;
}

/** A full pull from y=0 to `toY`, straight down, then let go. */
function pull(target: EventTarget, toY: number, x = 0): { move: Event } {
  touch(target, 'touchstart', [{ y: 0 }]);
  touch(target, 'touchmove', [{ x: x / 2, y: toY / 2 }]);
  const move = touch(target, 'touchmove', [{ x, y: toY }]);
  touch(target, 'touchend', []);
  return { move };
}

/** A promise with its resolve exposed. */
function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = r));
  return { promise, resolve };
}

describe('PullToRefreshController', () => {
  let scroller: HTMLDivElement;
  let onRefresh: ReturnType<typeof vi.fn<() => Promise<void>>>;
  let states: PullState[];
  let ctl: PullToRefreshController;

  beforeEach(() => {
    scroller = document.createElement('div');
    document.body.appendChild(scroller);
    onRefresh = vi.fn(() => Promise.resolve());
    states = [];
    ctl = new PullToRefreshController({
      scroller: () => scroller,
      onRefresh,
      onChange: (s) => states.push(s),
    });
    ctl.attach(scroller);
  });

  afterEach(() => {
    ctl.detach();
    document.body.innerHTML = '';
  });

  // Finger travel is damped 2:1, so this is past the threshold.
  const PAST = PULL_THRESHOLD_PX * 2 + 10;
  const SHORT = PULL_THRESHOLD_PX; // half the threshold after damping

  it('refreshes when a pull at the top passes the threshold', async () => {
    const { move } = pull(scroller, PAST);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    // The browser's own overscroll / page reload is suppressed during a pull.
    expect(move.defaultPrevented).toBe(true);
    expect(ctl.state.refreshing).toBe(true);
    expect(ctl.state.distance).toBe(PULL_THRESHOLD_PX);
    await Promise.resolve();
    await Promise.resolve();
    expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });
  });

  it('reports the pull distance and arms past the threshold', () => {
    touch(scroller, 'touchstart', [{ y: 0 }]);
    touch(scroller, 'touchmove', [{ y: SHORT }]);
    expect(ctl.state.distance).toBe(SHORT / 2);
    expect(ctl.state.armed).toBe(false);
    touch(scroller, 'touchmove', [{ y: PAST }]);
    expect(ctl.state.armed).toBe(true);
    expect(states.at(-1)?.armed).toBe(true);
  });

  it('does not refresh a pull let go short of the threshold, and clears the indicator', () => {
    pull(scroller, SHORT);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(ctl.state.distance).toBe(0);
    expect(states.at(-1)?.distance).toBe(0);
  });

  it('only starts at the top: a touch on a scrolled list is ordinary scrolling', () => {
    scroller.scrollTop = 40;
    // happy-dom keeps scrollTop as set only on a scrollable element; force it.
    Object.defineProperty(scroller, 'scrollTop', { value: 40, configurable: true });
    const { move } = pull(scroller, PAST);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(move.defaultPrevented).toBe(false);
    expect(ctl.state.distance).toBe(0);
  });

  it('ignores an upward move (scrolling down the list)', () => {
    const { move } = pull(scroller, -PAST);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(move.defaultPrevented).toBe(false);
  });

  it('ignores a mostly sideways swipe, so panel swipes keep working', () => {
    const { move } = pull(scroller, PAST, PAST * 2);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(move.defaultPrevented).toBe(false);
  });

  it('ignores a two-finger touch', () => {
    touch(scroller, 'touchstart', [{ y: 0 }, { y: 0, id: 1 }]);
    const move = touch(scroller, 'touchmove', [{ y: PAST }, { y: PAST, id: 1 }]);
    touch(scroller, 'touchend', []);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(move.defaultPrevented).toBe(false);
  });

  it('a cancelled touch does not refresh', () => {
    touch(scroller, 'touchstart', [{ y: 0 }]);
    touch(scroller, 'touchmove', [{ y: PAST }]);
    touch(scroller, 'touchcancel', []);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(ctl.state.distance).toBe(0);
  });

  it('ignores a touch that starts outside the scroller', () => {
    const outside = document.createElement('div');
    document.body.appendChild(outside);
    ctl.attach(document.body);
    pull(outside, PAST);
    expect(onRefresh).not.toHaveBeenCalled();
  });

  it('is single-flight: a pull or refresh() during a refresh joins it', async () => {
    const load = deferred();
    onRefresh.mockImplementation(() => load.promise);
    pull(scroller, PAST);
    const joined = ctl.refresh();
    pull(scroller, PAST);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(ctl.state.refreshing).toBe(true);
    load.resolve();
    await joined;
    expect(ctl.state.refreshing).toBe(false);
    // Once it is over, the next pull refreshes again.
    pull(scroller, PAST);
    expect(onRefresh).toHaveBeenCalledTimes(2);
  });

  it('clears the indicator when the refresh fails', async () => {
    onRefresh.mockImplementation(() => Promise.reject(new Error('offline')));
    await expect(ctl.refresh()).resolves.toBeUndefined();
    expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });
  });

  it('a small jitter down before an upward flick leaves native scrolling alone', () => {
    touch(scroller, 'touchstart', [{ y: 100 }]);
    const jitter = touch(scroller, 'touchmove', [{ y: 102 }]);
    const flick = touch(scroller, 'touchmove', [{ y: 60 }]);
    const more = touch(scroller, 'touchmove', [{ y: 20 }]);
    touch(scroller, 'touchend', []);
    expect(jitter.defaultPrevented).toBe(false);
    expect(flick.defaultPrevented).toBe(false);
    expect(more.defaultPrevented).toBe(false);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(ctl.state.distance).toBe(0);
  });

  it('holds off the browser inside the slop once the move is clearly downward', () => {
    touch(scroller, 'touchstart', [{ y: 0 }]);
    expect(touch(scroller, 'touchmove', [{ y: 4 }]).defaultPrevented).toBe(true);
  });

  it('a quiet refresh shows no indicator, and a pull may still start and join it', async () => {
    const load = deferred();
    onRefresh.mockImplementation(() => load.promise);
    const quiet = ctl.refresh({ quiet: true });
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });
    expect(states).toEqual([]);

    pull(scroller, PAST);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(ctl.state.refreshing).toBe(true);
    expect(ctl.state.distance).toBe(PULL_THRESHOLD_PX);

    load.resolve();
    await quiet;
    expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });
  });

  it('a quiet refresh ending mid-pull leaves the pull alone', async () => {
    const load = deferred();
    onRefresh.mockImplementation(() => load.promise);
    const quiet = ctl.refresh({ quiet: true });
    touch(scroller, 'touchstart', [{ y: 0 }]);
    touch(scroller, 'touchmove', [{ y: SHORT }]);
    load.resolve();
    await quiet;
    expect(ctl.state.distance).toBe(SHORT / 2);
  });

  it('a pull that turns sideways is dropped, and an ancestor swipe never sees it', () => {
    // The chat page's panel swipe: latches on a mostly-sideways move past
    // 10px and switches panels at touchend past 100px.
    let swiping = false;
    let switched = false;
    let startX = 0;
    let startY = 0;
    const parent = document.body;
    const onStart = (e: Event) => {
      const t = (e as TouchEvent).touches[0];
      startX = t.clientX;
      startY = t.clientY;
      swiping = false;
    };
    const onMove = (e: Event) => {
      const t = (e as TouchEvent).touches[0];
      const dx = t.clientX - startX;
      const dy = t.clientY - startY;
      if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 10) swiping = true;
    };
    const onEnd = () => {
      if (swiping) switched = true;
    };
    parent.addEventListener('touchstart', onStart);
    parent.addEventListener('touchmove', onMove);
    parent.addEventListener('touchend', onEnd);
    try {
      touch(scroller, 'touchstart', [{ y: 0 }]);
      touch(scroller, 'touchmove', [{ y: PAST / 2 }]);
      touch(scroller, 'touchmove', [{ y: PAST }]);
      expect(ctl.state.armed).toBe(true);
      // Now 150px sideways (more than down), then let go.
      const sideways = touch(scroller, 'touchmove', [{ x: PAST + 150, y: PAST }]);
      expect(ctl.state.distance).toBe(0);
      expect(sideways.defaultPrevented).toBe(true);
      touch(scroller, 'touchmove', [{ x: PAST + 200, y: PAST }]);
      touch(scroller, 'touchend', []);
      expect(onRefresh).not.toHaveBeenCalled();
      expect(swiping).toBe(false);
      expect(switched).toBe(false);
      expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });

      // A plain sideways swipe still reaches the ancestor.
      touch(scroller, 'touchstart', [{ y: 0 }]);
      touch(scroller, 'touchmove', [{ x: 150, y: 5 }]);
      touch(scroller, 'touchend', []);
      expect(switched).toBe(true);
    } finally {
      parent.removeEventListener('touchstart', onStart);
      parent.removeEventListener('touchmove', onMove);
      parent.removeEventListener('touchend', onEnd);
    }
  });

  it('a second finger mid-pull hands the touch to the browser for good', () => {
    touch(scroller, 'touchstart', [{ y: 0 }]);
    touch(scroller, 'touchmove', [{ y: PAST / 2 }]);
    touch(scroller, 'touchmove', [{ y: PAST }]);
    expect(ctl.state.armed).toBe(true);
    // The second finger lands: a pinch.
    touch(scroller, 'touchstart', [{ y: PAST }, { y: PAST, id: 1 }]);
    expect(ctl.state.distance).toBe(0);
    const pinch = touch(scroller, 'touchmove', [{ y: PAST + 20 }, { y: PAST - 20, id: 1 }]);
    // The second finger lifts; the first moves on and lifts past the threshold.
    touch(scroller, 'touchend', [{ y: PAST + 20 }]);
    const after = touch(scroller, 'touchmove', [{ y: PAST + 40 }]);
    touch(scroller, 'touchend', []);
    expect(pinch.defaultPrevented).toBe(false);
    expect(after.defaultPrevented).toBe(false);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(ctl.state).toEqual({ distance: 0, armed: false, refreshing: false });
  });

  it('a pull that comes back up above its start resets and does not fire', () => {
    touch(scroller, 'touchstart', [{ y: 100 }]);
    touch(scroller, 'touchmove', [{ y: 100 + PAST }]);
    expect(ctl.state.armed).toBe(true);
    touch(scroller, 'touchmove', [{ y: 80 }]);
    expect(ctl.state.distance).toBe(0);
    expect(ctl.state.armed).toBe(false);
    touch(scroller, 'touchend', []);
    expect(onRefresh).not.toHaveBeenCalled();
    expect(ctl.state.distance).toBe(0);
  });

  it('stops listening once detached', () => {
    ctl.detach();
    pull(scroller, PAST);
    expect(onRefresh).not.toHaveBeenCalled();
  });
});
