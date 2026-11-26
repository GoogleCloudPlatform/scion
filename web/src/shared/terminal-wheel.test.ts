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

import { describe, expect, it } from 'vitest';
import { dragAxis, wheelNotches } from './terminal-wheel.js';

describe('wheelNotches', () => {
  it('emits nothing until a full row has been travelled', () => {
    const r = wheelNotches(9, 20);
    expect(r.notches).toBe(0);
  });

  // Without this a slow drag never scrolls: each move is under one row, and
  // discarding the remainder each time means the total is never reached.
  it('keeps the remainder so a slow drag accumulates', () => {
    let carry = 0;
    let emitted = 0;
    for (let i = 0; i < 4; i++) {
      carry += 9;
      const r = wheelNotches(carry, 20);
      emitted += r.notches;
      carry = r.remainder;
    }
    expect(emitted).toBe(1); // 36 travelled, one row of 20 crossed
    expect(carry).toBeCloseTo(16);
  });

  it('maps a finger moving up to wheel-down, and vice versa', () => {
    // Positive carry = the finger moved up the screen = content moves up =
    // later output = wheel-down.
    expect(wheelNotches(40, 20).up).toBe(false);
    expect(wheelNotches(-40, 20).up).toBe(true);
  });

  it('bounds a flung gesture', () => {
    expect(wheelNotches(10_000, 20).notches).toBe(20);
    expect(wheelNotches(10_000, 20, 5).notches).toBe(5);
  });

  it('refuses nonsense rather than emitting NaN notches', () => {
    for (const [carry, step] of [
      [Number.NaN, 20],
      [40, 0],
      [40, -1],
      [40, Number.NaN],
    ] as const) {
      expect(wheelNotches(carry, step).notches).toBe(0);
    }
  });
});

describe('dragAxis', () => {
  it('is undecided until the finger has moved the slop distance', () => {
    expect(dragAxis(0, 0)).toBeNull();
    expect(dragAxis(3, -3)).toBeNull();
    expect(dragAxis(Number.NaN, 10)).toBeNull();
  });

  it('is vertical for a mostly vertical or diagonal drag, in either direction', () => {
    expect(dragAxis(1, 12)).toBe('vertical');
    expect(dragAxis(-5, -12)).toBe('vertical');
    expect(dragAxis(10, 10)).toBe('vertical');
  });

  it('is horizontal for a mostly horizontal drag, in either direction', () => {
    expect(dragAxis(12, 3)).toBe('horizontal');
    expect(dragAxis(-12, -3)).toBe('horizontal');
  });

  it('takes the slop as a parameter', () => {
    expect(dragAxis(0, 6, 10)).toBeNull();
    expect(dragAxis(0, 10, 10)).toBe('vertical');
  });
});
