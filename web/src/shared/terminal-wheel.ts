/** Wheel-event helpers for the web terminal's touch scrolling. */

/** Wheel-up button code in SGR mouse encoding. */
export const WHEEL_UP = 64;
/** Wheel-down button code in SGR mouse encoding. */
export const WHEEL_DOWN = 65;

/**
 * Encodes one wheel notch as an SGR mouse report.
 *
 * col and row are 1-based cell coordinates naming the pane the wheel is over,
 * so a split tmux window scrolls the pane under the finger, not the active one.
 */
export function sgrWheel(up: boolean, col: number, row: number): string {
  return `\x1b[<${up ? WHEEL_UP : WHEEL_DOWN};${col};${row}M`;
}

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

/** Clamps a 1-based cell coordinate into a terminal of the given size. */
export function clampCell(value: number, size: number): number {
  if (!Number.isFinite(value) || size < 1) return 1;
  return Math.min(size, Math.max(1, Math.ceil(value)));
}
