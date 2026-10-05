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
 * Estimated hub clock (ptone/scion#2952).
 *
 * The delete lease (`deletion.leaseExpiresAt`) and the failed view's
 * `expiresAt` are hub timestamps. Comparing them with the browser's
 * `Date.now()` breaks when the browser clock is skewed: 40s ahead flickers
 * "Deleting…" / "Delete interrupted" on every renewal, 60s ahead always
 * reads interrupted. This module keeps an estimate of `hub - browser` from
 * the HTTP `Date` header of API responses (`apiFetch` records every one),
 * and {@link hubNow} returns browser time corrected by it.
 *
 * The `Date` header has one-second resolution and the request takes time,
 * so each sample is `(D + 500ms) - midpoint(sent, received)`: accurate to
 * about ±(0.5s + RTT/2), which is far below the 20s renewal cadence. The
 * estimate is the median of the last {@link MAX_SAMPLES} samples, so one
 * odd response (an HTTP-cached copy carrying an old `Date`) does not move it.
 */

/** Number of recent samples the median is taken over. */
const MAX_SAMPLES = 7;

/** Samples from requests slower than this are too imprecise to use. */
const MAX_SAMPLE_RTT_MS = 10_000;

let samples: number[] = [];
let offsetMs = 0;

function recompute(): void {
  const sorted = [...samples].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  offsetMs =
    sorted.length === 0
      ? 0
      : sorted.length % 2 === 1
        ? sorted[mid]
        : Math.round((sorted[mid - 1] + sorted[mid]) / 2);
}

/**
 * Record one observation of the hub's clock: the hub's time `hubMs` was
 * produced while the request ran between `sentMs` and `receivedMs`
 * (browser clock). Ignored if the inputs are not finite or the request was
 * too slow to be informative.
 */
export function recordHubTime(hubMs: number, sentMs: number, receivedMs: number): void {
  if (!Number.isFinite(hubMs) || !Number.isFinite(sentMs) || !Number.isFinite(receivedMs)) return;
  const rtt = receivedMs - sentMs;
  if (rtt < 0 || rtt > MAX_SAMPLE_RTT_MS) return;
  samples.push(hubMs - (sentMs + receivedMs) / 2);
  if (samples.length > MAX_SAMPLES) samples = samples.slice(-MAX_SAMPLES);
  recompute();
}

/**
 * Record the HTTP `Date` header of a hub response (see the module doc).
 * A missing or unparsable header is ignored.
 */
export function recordHubDateHeader(
  dateHeader: string | null | undefined,
  sentMs: number,
  receivedMs: number
): void {
  if (!dateHeader) return;
  const t = Date.parse(dateHeader);
  if (Number.isNaN(t)) return;
  // The header is truncated to the second: the hub's time was in [t, t+1s).
  recordHubTime(t + 500, sentMs, receivedMs);
}

/** Current estimate of `hub clock - browser clock`, in ms (0 until sampled). */
export function hubClockOffsetMs(): number {
  return offsetMs;
}

/** The browser's `Date.now()` corrected to the hub's clock. */
export function hubNow(): number {
  return Date.now() + offsetMs;
}

/** Test hook: forget every sample. */
export function _resetHubClock(): void {
  samples = [];
  offsetMs = 0;
}
