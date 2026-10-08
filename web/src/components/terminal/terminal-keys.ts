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
 * next character typed on the on-screen keyboard. Unmodified keys follow
 * xterm.js (src/common/input/Keyboard.ts); see ctrlCode for the Ctrl map.
 * Modified cursor and page keys use the standard xterm CSI form,
 * CSI 1 ; mod X, with mod = 1 + Alt 2 + Ctrl 4. xterm.js differs here:
 * - it remaps Alt+arrows: Alt+Left/Right to word movement (ESC b / ESC f on
 *   macOS, CSI 1 ; 5 D / C elsewhere), and Alt+Up/Down to CSI 1 ; 5 A / B
 *   outside macOS;
 * - it ignores Alt on PgUp/PgDn, Tab and Shift+Tab.
 * The bar sends the plain forms (CSI 1 ; 3 X for Alt, ESC prefix for Alt+Tab
 * and Alt+Shift+Tab), which tmux and readline also understand.
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
  // (xterm.js ignores Alt on Tab and Shift+Tab; ESC ESC [ Z is the plain
  // meta-sends-escape form.)
  const base = key === 'escape' ? '\x1b' : key === 'tab' ? '\t' : '\x1b[Z';
  return mods.alt ? `\x1b${base}` : base;
}

/**
 * The control code for Ctrl plus a typed character, or null if the
 * character has none. xterm.js 5.5 maps a-z, space, 3-7, 8, [ \ ] and
 * Backspace; the rest is xterm/VT convention (also gnome-terminal and
 * iTerm2): a-z and A-Z → 0x01-0x1a, @, space and 2 → NUL, [ \ ] ^ _ →
 * 0x1b-0x1f, 3-7 → 0x1b-0x1f, / → 0x1f, 8 and ? → DEL, Backspace (DEL)
 * → BS.
 */
export function ctrlCode(char: string): string | null {
  if (/^[a-zA-Z]$/.test(char)) return String.fromCharCode(char.toUpperCase().charCodeAt(0) - 64);
  if (char === ' ' || char === '@' || char === '2') return '\x00';
  const index = '[\\]^_'.indexOf(char);
  if (char.length === 1 && index >= 0) return String.fromCharCode(0x1b + index);
  if (/^[3-7]$/.test(char)) return String.fromCharCode(0x1b + Number(char) - 3);
  if (char === '/') return '\x1f';
  if (char === '8' || char === '?') return '\x7f';
  if (char === '\x7f') return '\b';
  return null;
}

/**
 * Applies sticky modifiers to data on its way from xterm to the PTY. Only a
 * single character is modified; a character with no control code keeps its
 * plain value under Ctrl. Longer data is returned unchanged:
 * - user input (a paste, dictation, a predictive-text or IME word) counts
 *   as the key the modifier was for, so `consumed` is true and an armed
 *   modifier clears rather than hitting the next keystroke;
 * - data that starts with ESC is xterm's own reply (status reports, focus
 *   and mouse reports) or a bar key's sequence, so `consumed` is false and
 *   the modifier stays armed. xterm never replies with a single character.
 */
export function applyModifiers(data: string, mods: Modifiers): { data: string; consumed: boolean } {
  if (!mods.ctrl && !mods.alt) return { data, consumed: false };
  if ([...data].length !== 1) return { data, consumed: !data.startsWith('\x1b') };
  const base = mods.ctrl ? (ctrlCode(data) ?? data) : data;
  return { data: mods.alt ? `\x1b${base}` : base, consumed: true };
}
