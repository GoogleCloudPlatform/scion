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
 * Coalesces event-driven refetches: a burst of events costs one refresh per
 * resource, not one per event.
 *
 * Leading + trailing throttle with a max wait:
 *
 * - The first call in a quiet period runs `fn` at once (with `leading`, the
 *   default), so an isolated event gets no added latency.
 * - Calls during the window collapse into ONE trailing run after `waitMs` of
 *   quiet, but never later than `maxWaitMs` after the first queued call, so a
 *   steady event stream cannot starve the refresh.
 * - After a run the window stays open for another `waitMs`: a call landing
 *   right after a trailing run is queued, not run at once again.
 * - `eventAt` (an `Event.timeStamp`, on the `performance.now()` clock) is
 *   tracked; `fn` receives the newest one since its last run, so callers can
 *   pass `{ startedAfter }` to the shared loaders (client/chat-list-cache.ts)
 *   and join a request another owner started after the burst.
 *
 * Owners call `cancel()` when they go away (in `disconnectedCallback`).
 */

export interface CoalescedRefreshOptions {
  /** Quiet time after the last call before the trailing run. */
  waitMs: number;
  /** Longest a queued call waits for its run, however steady the calls. */
  maxWaitMs: number;
  /** Run the first call of a quiet period at once (default true). */
  leading?: boolean;
}

export interface CoalescedRefresh {
  /** Asks for a run that reflects an event delivered at `eventAt` (default now). */
  schedule(eventAt?: number): void;
  /** Runs a queued call now, if there is one, and ends the window. */
  flush(): void;
  /** Drops a queued call and ends the window. */
  cancel(): void;
}

export function coalescedRefresh(
  fn: (newestEventAt: number) => void | Promise<unknown>,
  opts: CoalescedRefreshOptions
): CoalescedRefresh {
  const leading = opts.leading ?? true;
  let quietTimer: ReturnType<typeof setTimeout> | null = null;
  let maxTimer: ReturnType<typeof setTimeout> | null = null;
  /** A call is queued for the trailing run. */
  let pending = false;
  /** Inside a window: the next call is queued rather than run at once. */
  let inWindow = false;
  /** Newest eventAt not yet passed to fn. */
  let newest = -Infinity;

  const clearTimers = (): void => {
    if (quietTimer !== null) clearTimeout(quietTimer);
    if (maxTimer !== null) clearTimeout(maxTimer);
    quietTimer = null;
    maxTimer = null;
  };

  const run = (): void => {
    pending = false;
    if (maxTimer !== null) {
      clearTimeout(maxTimer);
      maxTimer = null;
    }
    const at = newest;
    newest = -Infinity;
    const result = fn(at);
    // Owners fire and forget; a rejected refresh must not surface as an
    // unhandled rejection from a timer.
    if (result && typeof (result as Promise<unknown>).catch === 'function') {
      (result as Promise<unknown>).catch(() => {});
    }
  };

  const onQuiet = (): void => {
    quietTimer = null;
    if (!pending) {
      inWindow = false;
      return;
    }
    run();
    armQuiet();
  };

  const armQuiet = (): void => {
    if (quietTimer !== null) clearTimeout(quietTimer);
    quietTimer = setTimeout(onQuiet, opts.waitMs);
  };

  const onMaxWait = (): void => {
    maxTimer = null;
    if (!pending) return;
    run();
    armQuiet();
  };

  return {
    schedule(eventAt?: number): void {
      const at = eventAt ?? performance.now();
      newest = Math.max(newest, at);
      if (!inWindow && leading) {
        inWindow = true;
        run();
        armQuiet();
        return;
      }
      inWindow = true;
      if (!pending) {
        pending = true;
        maxTimer = setTimeout(onMaxWait, opts.maxWaitMs);
      }
      armQuiet();
    },
    flush(): void {
      clearTimers();
      inWindow = false;
      if (pending) run();
    },
    cancel(): void {
      clearTimers();
      inWindow = false;
      pending = false;
      newest = -Infinity;
    },
  };
}
