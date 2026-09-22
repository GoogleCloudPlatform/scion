import { describe, expect, it } from 'vitest';
import { WHEEL_DOWN, WHEEL_UP, clampCell, sgrWheel, wheelNotches } from './terminal-wheel';

describe('sgrWheel', () => {
  // The button numbers are the whole contract with tmux. Getting them the wrong
  // way round scrolls backwards, which is the kind of bug that survives review
  // because both directions "work".
  it('encodes wheel-up as button 64 and wheel-down as 65', () => {
    expect(sgrWheel(true, 1, 1)).toBe('\x1b[<64;1;1M');
    expect(sgrWheel(false, 1, 1)).toBe('\x1b[<65;1;1M');
    expect(WHEEL_UP).toBe(64);
    expect(WHEEL_DOWN).toBe(65);
  });

  it('carries the cell the wheel is over, so the right tmux pane scrolls', () => {
    expect(sgrWheel(true, 42, 7)).toBe('\x1b[<64;42;7M');
  });
});

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

describe('clampCell', () => {
  it('keeps coordinates inside the terminal, 1-based', () => {
    expect(clampCell(0.2, 80)).toBe(1);
    expect(clampCell(-5, 80)).toBe(1);
    expect(clampCell(1000, 80)).toBe(80);
    expect(clampCell(40.1, 80)).toBe(41);
  });

  it('survives a terminal that has not been sized yet', () => {
    expect(clampCell(Number.NaN, 80)).toBe(1);
    expect(clampCell(5, 0)).toBe(1);
  });
});
