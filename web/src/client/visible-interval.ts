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
 * A `setInterval` that pauses while the tab is hidden.
 *
 * Nobody sees a hidden tab, so a timer that only refreshes what is on screen
 * (relative timestamps, a countdown) is wasted work there. `visibleInterval`
 * stops the timer when `document.hidden` turns true. When the tab is shown
 * again it calls `fn` once if at least one tick was missed while hidden
 * (unless `catchUpOnVisible` is false), then resumes the normal cadence.
 * If no tick was missed, the next one comes when it was due, so the time the
 * tab spent hidden still counts toward the current interval.
 */

export interface VisibleIntervalOptions {
  /**
   * Call `fn` once on becoming visible if at least one tick was due while
   * the tab was hidden. Defaults to true.
   */
  catchUpOnVisible?: boolean;
}

/**
 * Calls `fn` every `ms` milliseconds while the document is visible.
 * Returns a function that stops the timer and removes the listener; it is
 * safe to call more than once.
 */
export function visibleInterval(
  fn: () => void,
  ms: number,
  { catchUpOnVisible = true }: VisibleIntervalOptions = {}
): () => void {
  if (typeof document === 'undefined') {
    const id = setInterval(fn, ms);
    return () => clearInterval(id);
  }

  // A chain of timeouts rather than setInterval, so that showing the tab
  // again resumes with the time left in the current interval instead of a
  // full one. Otherwise switching tabs faster than `ms` would never tick.
  let timer: ReturnType<typeof setTimeout> | null = null;
  // Wall-clock time the next tick is due. Ticks are scheduled against it,
  // not `ms` after the previous one ran, so lateness does not add up. Wall
  // clock, not performance.now(), which can stop while a device sleeps.
  let dueAt = Date.now() + ms;
  let stopped = false;

  const schedule = (): void => {
    if (timer !== null) return;
    const now = Date.now();
    // The wall clock stepped backwards: never wait more than one interval.
    if (dueAt - now > ms) dueAt = now + ms;
    timer = setTimeout(tick, Math.max(0, dueAt - now));
  };

  const stopTimer = (): void => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  };

  function tick(): void {
    timer = null;
    const now = Date.now();
    dueAt += ms;
    // After a catch-up (or a very late tick) restart the cadence from now
    // rather than firing the missed ticks back to back.
    if (dueAt <= now) dueAt = now + ms;
    try {
      fn();
    } finally {
      // fn may have thrown, or called the returned stop function.
      if (!stopped) schedule();
    }
  }

  const onVisibilityChange = (): void => {
    if (stopped) return;
    if (document.hidden) {
      stopTimer();
      return;
    }
    if (timer !== null) return;
    if (Date.now() < dueAt) {
      schedule();
    } else if (catchUpOnVisible) {
      tick();
    } else {
      // A tick was missed but no catch-up was asked for: start a fresh
      // interval from now.
      dueAt = Date.now() + ms;
      schedule();
    }
  };

  document.addEventListener('visibilitychange', onVisibilityChange);
  if (!document.hidden) schedule();

  return () => {
    stopped = true;
    stopTimer();
    document.removeEventListener('visibilitychange', onVisibilityChange);
  };
}
