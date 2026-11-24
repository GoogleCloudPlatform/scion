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

/** Wheel-event helpers for the web terminal's touch scrolling. */

/**
 * Splits accumulated drag distance into whole wheel notches plus the remainder
 * to carry into the next move. Carrying it is what lets a drag of less than one
 * row per event still scroll. `max` bounds a fling.
 */
export function wheelNotches(
  carry: number,
  step: number,
  max = 20
): { notches: number; up: boolean; remainder: number } {
  if (!Number.isFinite(carry) || !Number.isFinite(step) || step <= 0) {
    return { notches: 0, up: false, remainder: 0 };
  }
  const whole = Math.trunc(carry / step);
  if (whole === 0) return { notches: 0, up: false, remainder: carry };
  return {
    notches: Math.min(Math.abs(whole), max),
    // Negative carry = finger moved down = earlier output = wheel-up.
    up: whole < 0,
    remainder: carry - whole * step,
  };
}

/**
 * Which way a one-finger drag is going, once it has moved `slop` pixels from
 * where it started; null until then. A vertical drag scrolls the terminal; a
 * horizontal one is left to the browser. A diagonal drag counts as vertical.
 */
export function dragAxis(dx: number, dy: number, slop = 4): 'vertical' | 'horizontal' | null {
  const ax = Math.abs(dx);
  const ay = Math.abs(dy);
  if (!(Math.max(ax, ay) >= slop)) return null;
  return ax > ay ? 'horizontal' : 'vertical';
}
