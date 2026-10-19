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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { visibleInterval } from './visible-interval.js';

describe('visibleInterval', () => {
  let hidden = false;
  // Every interval a test starts, stopped in afterEach even if the test
  // failed before its own stop(), so one failure cannot leak a timer or a
  // listener into the next test.
  let stops: Array<() => void> = [];
  const start = (...args: Parameters<typeof visibleInterval>): (() => void) => {
    const stop = visibleInterval(...args);
    stops.push(stop);
    return stop;
  };

  const setHidden = (value: boolean): void => {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  };

  beforeEach(() => {
    vi.useFakeTimers();
    hidden = false;
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
  });

  afterEach(() => {
    for (const stop of stops) stop();
    stops = [];
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('ticks every interval while visible', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
    stop();
  });

  it('no ticks while hidden', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    setHidden(true);
    vi.advanceTimersByTime(60_000);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('no ticks while hidden when started hidden', () => {
    hidden = true;
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(60_000);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('exactly one catch-up tick on becoming visible', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    setHidden(true);
    vi.advanceTimersByTime(10_000);
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    // The normal cadence resumes from the catch-up tick.
    vi.advanceTimersByTime(999);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(2);
    stop();
  });

  it('no catch-up tick when no tick was missed while hidden', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(200);
    setHidden(true);
    vi.advanceTimersByTime(300);
    setHidden(false);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('no catch-up tick when a tick ran less than an interval before showing', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(500);
    setHidden(true);
    vi.advanceTimersByTime(400);
    // 900 ms since the last tick at t=1000, 1900 ms since start.
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('one catch-up tick when the hidden period spans the next due tick', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(900);
    setHidden(true);
    vi.advanceTimersByTime(200);
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('a short hide keeps the due time of the current interval', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(700);
    setHidden(true);
    vi.advanceTimersByTime(100);
    setHidden(false);
    vi.advanceTimersByTime(199);
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('rapid hide/show faster than the interval keeps ticking once per interval', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    // Visible 500 ms, hidden 100 ms, ten times: 6 s of wall clock. Due ticks
    // at 1 s, 2 s, 4 s and 5 s fall in visible stretches; the ones at 3 s and
    // 6 s fall exactly on a show and run as catch-up ticks.
    for (let i = 0; i < 10; i++) {
      vi.advanceTimersByTime(500);
      setHidden(true);
      vi.advanceTimersByTime(100);
      setHidden(false);
    }
    expect(fn).toHaveBeenCalledTimes(6);
    expect(vi.getTimerCount()).toBe(1);
    stop();
  });

  it('no catch-up tick when catchUpOnVisible is false', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000, { catchUpOnVisible: false });
    setHidden(true);
    vi.advanceTimersByTime(10_000);
    setHidden(false);
    expect(fn).not.toHaveBeenCalled();
    // A fresh full interval from the show, not the time left of the old one.
    vi.advanceTimersByTime(999);
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('repeated visible events do not start extra timers', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    setHidden(false);
    setHidden(false);
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
    stop();
  });

  it('keeps ticking after fn throws', () => {
    const fn = vi.fn(() => {
      if (fn.mock.calls.length === 1) throw new Error('boom');
    });
    const stop = start(fn, 1000);
    expect(() => vi.advanceTimersByTime(1000)).toThrow('boom');
    expect(vi.getTimerCount()).toBe(1);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(2);
    stop();
  });

  it('a catch-up tick keeps the next tick on its original cadence', () => {
    const fn = vi.fn();
    const stop = start(fn, 1000);
    vi.advanceTimersByTime(900);
    setHidden(true);
    vi.advanceTimersByTime(200);
    setHidden(false);
    // Catch-up at 1100 for the tick due at 1000; the next one is still due
    // at 2000, not a full interval after the catch-up.
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(899);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(2);
    stop();
  });

  it('a tick that runs late does not push the next one later', () => {
    vi.setSystemTime(0);
    const at: number[] = [];
    start(() => {
      at.push(Date.now());
      // The first tick takes 300 ms.
      if (at.length === 1) vi.setSystemTime(Date.now() + 300);
    }, 1000);
    vi.advanceTimersByTime(2000);
    expect(at).toEqual([1000, 2000]);
  });

  it('a tick more than an interval late runs once, then resumes the cadence', () => {
    vi.setSystemTime(0);
    const at: number[] = [];
    start(() => {
      at.push(Date.now());
      // The first tick takes 2.5 intervals.
      if (at.length === 1) vi.setSystemTime(Date.now() + 2500);
    }, 1000);
    vi.advanceTimersByTime(1000);
    expect(at).toEqual([1000]);
    // The overdue tick runs right away (a 0 ms timeout fires after 1 ms),
    // once, not once per missed interval...
    vi.advanceTimersByTime(1);
    expect(at).toEqual([1000, 3501]);
    vi.advanceTimersByTime(999);
    expect(at).toEqual([1000, 3501]);
    // ...and the cadence restarts from it.
    vi.advanceTimersByTime(1);
    expect(at).toEqual([1000, 3501, 4501]);
  });

  it('a wall clock stepping backwards delays no tick by more than an interval', () => {
    const fn = vi.fn();
    start(fn, 1000);
    vi.advanceTimersByTime(500);
    vi.setSystemTime(Date.now() - 3_600_000);
    vi.advanceTimersByTime(500);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('a wall clock stepping backwards while hidden delays no tick by more than an interval', () => {
    const fn = vi.fn();
    start(fn, 1000);
    vi.advanceTimersByTime(500);
    setHidden(true);
    vi.setSystemTime(Date.now() - 3_600_000);
    setHidden(false);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('stopping from inside a regular tick ends the chain', () => {
    let stop: () => void = () => {};
    const fn = vi.fn(() => stop());
    stop = start(fn, 1000);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(10_000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('stopping while hidden stays stopped after showing', () => {
    const removeSpy = vi.spyOn(document, 'removeEventListener');
    const fn = vi.fn();
    const stop = start(fn, 1000);
    setHidden(true);
    stop();
    expect(removeSpy).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
    vi.advanceTimersByTime(5000);
    setHidden(false);
    vi.advanceTimersByTime(5000);
    expect(fn).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('the cleanup function clears everything', () => {
    const removeSpy = vi.spyOn(document, 'removeEventListener');
    const fn = vi.fn();
    const stop = start(fn, 1000);
    expect(vi.getTimerCount()).toBe(1);
    stop();
    expect(vi.getTimerCount()).toBe(0);
    expect(removeSpy).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
    // Neither time nor visibility changes bring it back.
    vi.advanceTimersByTime(10_000);
    setHidden(true);
    vi.advanceTimersByTime(10_000);
    setHidden(false);
    vi.advanceTimersByTime(10_000);
    expect(fn).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
    // Calling it again is harmless.
    stop();
  });

  it('stopping from inside the catch-up tick does not restart the timer', () => {
    let stop: () => void = () => {};
    const fn = vi.fn(() => stop());
    stop = start(fn, 1000);
    setHidden(true);
    vi.advanceTimersByTime(5000);
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });
});
