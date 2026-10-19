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
import { visibleInterval } from './visible-interval';

describe('visibleInterval', () => {
  let hidden = false;

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
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('ticks every interval while visible', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
    stop();
  });

  it('no ticks while hidden', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
    setHidden(true);
    vi.advanceTimersByTime(60_000);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('no ticks while hidden when started hidden', () => {
    hidden = true;
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
    vi.advanceTimersByTime(60_000);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('exactly one catch-up tick on becoming visible', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
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
    const stop = visibleInterval(fn, 1000);
    vi.advanceTimersByTime(200);
    setHidden(true);
    vi.advanceTimersByTime(300);
    setHidden(false);
    expect(fn).not.toHaveBeenCalled();
    stop();
  });

  it('no catch-up tick when a tick ran less than an interval before showing', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
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
    const stop = visibleInterval(fn, 1000);
    vi.advanceTimersByTime(900);
    setHidden(true);
    vi.advanceTimersByTime(200);
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('no catch-up tick when catchUpOnVisible is false', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000, { catchUpOnVisible: false });
    setHidden(true);
    vi.advanceTimersByTime(10_000);
    setHidden(false);
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    stop();
  });

  it('repeated visible events do not start extra timers', () => {
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
    setHidden(false);
    setHidden(false);
    vi.advanceTimersByTime(3000);
    expect(fn).toHaveBeenCalledTimes(3);
    stop();
  });

  it('the cleanup function clears everything', () => {
    const removeSpy = vi.spyOn(document, 'removeEventListener');
    const fn = vi.fn();
    const stop = visibleInterval(fn, 1000);
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
    stop = visibleInterval(fn, 1000);
    setHidden(true);
    vi.advanceTimersByTime(5000);
    setHidden(false);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });
});
