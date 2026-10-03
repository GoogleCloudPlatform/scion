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
 * Races `promise` against a `ms`-millisecond timer (tz-refactor task 11,
 * review round 3 R3-1).
 *
 * Used to bound a startup fetch that is cosmetic but was put on the
 * first-render critical path: if `promise` settles first, this resolves (or
 * rejects) with its outcome. If the timer fires first, this resolves to
 * `undefined` and lets the caller proceed — `promise` is **not** cancelled
 * or awaited further here; it keeps running, and a caller like
 * `loadPreferredTimeZone` that has its own side effect (`setPreferredTimeZone`,
 * which dispatches `DISPLAY_TIMEZONE_CHANGED_EVENT`) still applies that
 * effect whenever it eventually lands, correcting anything already
 * rendered. This is why a bounded wait loses nothing over an unbounded one
 * once a subscriber (`DisplayZoneController`) exists.
 */
export function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T | undefined> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => resolve(undefined), ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err: unknown) => {
        clearTimeout(timer);
        reject(err as Error);
      }
    );
  });
}
