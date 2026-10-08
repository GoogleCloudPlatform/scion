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
 * Byte sequences for the terminal pane's touch key bar.
 *
 * A phone's on-screen keyboard has no Esc, Tab, Ctrl, Alt or arrow keys.
 * The key bar sends them, and its sticky Ctrl/Alt modifiers apply to the
 * next character typed on the on-screen keyboard. The sequences follow the
 * encodings xterm.js uses for the real keys (src/common/input/Keyboard.ts).
 */

/** A key the bar sends as a fixed sequence (not a typed character). */
export type TerminalBarKey =
  | 'escape'
  | 'tab'
  | 'backtab'
  | 'up'
  | 'down'
  | 'right'
  | 'left'
  | 'home'
  | 'end'
  | 'pageup'
  | 'pagedown';

/**
 * A sticky modifier: off, armed for the next key only, or locked on until
 * tapped again. Tapping cycles off → armed → locked → off.
 */
export type ModifierState = 'off' | 'armed' | 'locked';

export interface Modifiers {
  ctrl: boolean;
  alt: boolean;
}

export function nextModifierState(state: ModifierState): ModifierState {
  return state === 'off' ? 'armed' : state === 'armed' ? 'locked' : 'off';
}

/** The state after a key consumed the modifier: armed clears, locked stays. */
export function consumeModifier(state: ModifierState): ModifierState {
  return state === 'armed' ? 'off' : state;
}

/** xterm's modifier parameter: 1 + (Shift 1) + (Alt 2) + (Ctrl 4). */
function modifierParam({ ctrl, alt }: Modifiers): number {
  return 1 + (alt ? 2 : 0) + (ctrl ? 4 : 0);
}

const CURSOR_FINAL: Partial<Record<TerminalBarKey, string>> = {
  up: 'A',
  down: 'B',
  right: 'C',
  left: 'D',
  home: 'H',
  end: 'F',
};

const TILDE_CODE: Partial<Record<TerminalBarKey, number>> = { pageup: 5, pagedown: 6 };

/**
 * The sequence for a bar key. `applicationCursor` is xterm's DECCKM state
 * (`terminal.modes.applicationCursorKeysMode`): unmodified arrows and
 * Home/End send SS3 (ESC O A) in it and CSI (ESC [ A) otherwise. A modified
 * key always uses the CSI form with a modifier parameter (ESC [ 1 ; 5 A for
 * Ctrl+Up), as xterm does.
 */
export function barKeySequence(
  key: TerminalBarKey,
  applicationCursor: boolean,
  mods: Modifiers = { ctrl: false, alt: false }
): string {
  const modified = mods.ctrl || mods.alt;
  const cursorFinal = CURSOR_FINAL[key];
  if (cursorFinal) {
    if (modified) return `\x1b[1;${modifierParam(mods)}${cursorFinal}`;
    return (applicationCursor ? '\x1bO' : '\x1b[') + cursorFinal;
  }
  const tildeCode = TILDE_CODE[key];
  if (tildeCode) {
    return modified ? `\x1b[${tildeCode};${modifierParam(mods)}~` : `\x1b[${tildeCode}~`;
  }
  // Esc, Tab and Shift+Tab: Alt prefixes ESC, Ctrl does not change them.
  const base = key === 'escape' ? '\x1b' : key === 'tab' ? '\t' : '\x1b[Z';
  return mods.alt ? `\x1b${base}` : base;
}

/**
 * The control code for Ctrl plus a typed character, or null if the
 * character has none: a-z and A-Z → 0x01-0x1a, @ and space → NUL,
 * [ \ ] ^ _ → 0x1b-0x1f, ? → DEL.
 */
export function ctrlCode(char: string): string | null {
  if (/^[a-zA-Z]$/.test(char)) return String.fromCharCode(char.toUpperCase().charCodeAt(0) - 64);
  if (char === ' ' || char === '@') return '\x00';
  const index = '[\\]^_'.indexOf(char);
  if (char.length === 1 && index >= 0) return String.fromCharCode(0x1b + index);
  if (char === '?') return '\x7f';
  return null;
}

/**
 * Applies sticky modifiers to data typed on the on-screen keyboard. Only a
 * single character is modified: anything longer (a paste, a predictive-text
 * word, a terminal's reply to a status query) is returned unchanged and
 * `consumed` is false, so the modifier stays armed for the next key. A
 * character with no control code keeps its plain value under Ctrl.
 */
export function applyModifiers(data: string, mods: Modifiers): { data: string; consumed: boolean } {
  if ((!mods.ctrl && !mods.alt) || [...data].length !== 1) return { data, consumed: false };
  const base = mods.ctrl ? (ctrlCode(data) ?? data) : data;
  return { data: mods.alt ? `\x1b${base}` : base, consumed: true };
}
