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

import { coalescedRefresh } from './coalesced-refresh.js';

describe('coalescedRefresh', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('runs immediately on first call (leading)', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: true });

    r.schedule(10);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenLastCalledWith(10);

    // Nothing queued: the window closes without a trailing run.
    vi.advanceTimersByTime(5000);
    expect(fn).toHaveBeenCalledTimes(1);

    // The next quiet-period call is leading again.
    r.schedule(20);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('collapses N calls in window into one trailing run', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: true });

    r.schedule();
    for (let i = 0; i < 9; i++) {
      vi.advanceTimersByTime(50);
      r.schedule();
    }
    expect(fn).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(499);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(2);

    vi.advanceTimersByTime(10_000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('collapses calls into one trailing run without leading', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: false });

    r.schedule();
    r.schedule();
    r.schedule();
    expect(fn).not.toHaveBeenCalled();

    vi.advanceTimersByTime(500);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('maxWait fires even under continuous calls', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 2000, maxWaitMs: 6000, leading: false });

    // One call per 100 ms for 10 s: the quiet period never comes.
    for (let t = 0; t < 10_000; t += 100) {
      r.schedule();
      vi.advanceTimersByTime(100);
    }
    expect(fn.mock.calls.length).toBeGreaterThanOrEqual(1);
    vi.advanceTimersByTime(10_000);
    expect(fn.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(fn.mock.calls.length).toBeLessThanOrEqual(3);
  });

  it('maxWait bounds a leading throttle under continuous calls', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 2000, maxWaitMs: 6000, leading: true });

    for (let t = 0; t < 10_000; t += 100) {
      r.schedule();
      vi.advanceTimersByTime(100);
    }
    vi.advanceTimersByTime(10_000);
    expect(fn.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(fn.mock.calls.length).toBeLessThanOrEqual(3);
  });

  it('passes newest eventAt', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: true });

    r.schedule(100);
    expect(fn).toHaveBeenLastCalledWith(100);

    // Out of order: the newest wins, not the last.
    r.schedule(300);
    r.schedule(250);
    r.schedule(200);
    vi.advanceTimersByTime(500);
    expect(fn).toHaveBeenCalledTimes(2);
    expect(fn).toHaveBeenLastCalledWith(300);
  });

  it('defaults eventAt to now', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000 });
    const before = performance.now();
    r.schedule();
    const at = fn.mock.calls[0][0] as number;
    expect(at).toBeGreaterThanOrEqual(before);
    expect(at).toBeLessThanOrEqual(performance.now());
  });

  it('cancel prevents pending run', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: true });

    r.schedule();
    r.schedule();
    r.cancel();
    vi.advanceTimersByTime(10_000);
    expect(fn).toHaveBeenCalledTimes(1);

    // After cancel the next call starts a fresh window (leading).
    r.schedule();
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('flush runs a queued call at once', () => {
    const fn = vi.fn();
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: false });

    r.schedule(42);
    r.flush();
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenLastCalledWith(42);
    vi.advanceTimersByTime(10_000);
    expect(fn).toHaveBeenCalledTimes(1);

    // Nothing queued: flush is a no-op.
    r.flush();
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('a synchronous throw still closes the window after a quiet period', () => {
    let calls = 0;
    const fn = vi.fn(() => {
      calls++;
      if (calls === 1) throw new Error('boom');
    });
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000, leading: true });

    expect(() => r.schedule()).toThrow('boom');
    vi.advanceTimersByTime(5000);
    // The window closed on its quiet timer, so the next call leads.
    r.schedule();
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('swallows a rejected refresh', async () => {
    const fn = vi.fn(() => Promise.reject(new Error('offline')));
    const r = coalescedRefresh(fn, { waitMs: 500, maxWaitMs: 2000 });
    r.schedule();
    await Promise.resolve();
    expect(fn).toHaveBeenCalledTimes(1);
  });
});
