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

  let timer: ReturnType<typeof setInterval> | null = null;
  // Wall-clock time of the last tick (or of the start). A tick is missed
  // when a full interval has elapsed since then.
  let lastTickAt = Date.now();
  let stopped = false;

  const tick = (): void => {
    lastTickAt = Date.now();
    fn();
  };

  const startTimer = (): void => {
    if (timer === null) timer = setInterval(tick, ms);
  };

  const stopTimer = (): void => {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };

  const onVisibilityChange = (): void => {
    if (stopped) return;
    if (document.hidden) {
      stopTimer();
      return;
    }
    if (timer !== null) return;
    if (catchUpOnVisible && Date.now() - lastTickAt >= ms) {
      tick();
    }
    // tick() may have called the returned stop function.
    if (!stopped) startTimer();
  };

  document.addEventListener('visibilitychange', onVisibilityChange);
  if (!document.hidden) startTimer();

  return () => {
    stopped = true;
    stopTimer();
    document.removeEventListener('visibilitychange', onVisibilityChange);
  };
}
