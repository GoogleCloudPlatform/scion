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
 * Keyboard shortcuts for the Dashboard / Chat / Terminal view modes:
 * Cmd+1/2/3 on macOS, Ctrl+1/2/3 elsewhere.
 *
 * Keys are matched on `event.code` (the physical digit key), so the chord
 * is the same on every keyboard layout, and the modifier set must match
 * exactly: on Windows AltGr arrives as Ctrl+Alt and must keep typing its
 * characters.
 *
 * The on/off preference lives in localStorage, like the chat chime
 * preference, and defaults to on.
 */

export type ViewModeTarget = 'dashboard' | 'chat' | 'terminals';

const STORAGE_KEY = 'scion-view-mode-shortcuts';

/** Fired on window when the on/off preference changes. */
export const VIEW_MODE_SHORTCUTS_CHANGED_EVENT = 'scion-view-mode-shortcuts-changed';

const CODE_TO_MODE: Readonly<Record<string, ViewModeTarget>> = {
  Digit1: 'dashboard',
  Digit2: 'chat',
  Digit3: 'terminals',
};

const MODE_TO_DIGIT: Readonly<Record<ViewModeTarget, string>> = {
  dashboard: '1',
  chat: '2',
  terminals: '3',
};

/** Whether the view-mode shortcuts are on. Default: on. */
export function areViewModeShortcutsEnabled(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) !== 'false';
  } catch {
    return true;
  }
}

/** Turns the view-mode shortcuts on or off and notifies listeners. */
export function setViewModeShortcutsEnabled(on: boolean): void {
  try {
    if (on) {
      localStorage.removeItem(STORAGE_KEY); // default is on
    } else {
      localStorage.setItem(STORAGE_KEY, 'false');
    }
  } catch {
    // Storage unavailable: the preference cannot persist.
  }
  window.dispatchEvent(new CustomEvent(VIEW_MODE_SHORTCUTS_CHANGED_EVENT));
}

/**
 * The view mode a keydown asks for, or null when it is not a view-mode
 * shortcut. Key repeat still matches, so the caller can keep a held chord
 * from reaching the browser; IME composition never matches.
 */
export function viewModeForShortcut(e: KeyboardEvent, isMac: boolean): ViewModeTarget | null {
  if (e.isComposing || e.keyCode === 229) return null;
  if (e.altKey || e.shiftKey) return null;
  if (isMac ? !e.metaKey || e.ctrlKey : !e.ctrlKey || e.metaKey) return null;
  return CODE_TO_MODE[e.code] ?? null;
}

/** Display label for a mode's shortcut, e.g. "⌘2" or "Ctrl+2". */
export function viewModeShortcutLabel(mode: ViewModeTarget, isMac: boolean): string {
  const digit = MODE_TO_DIGIT[mode];
  return isMac ? `⌘${digit}` : `Ctrl+${digit}`;
}

/** aria-keyshortcuts value for a mode's shortcut, e.g. "Meta+2". */
export function viewModeAriaKeyshortcuts(mode: ViewModeTarget, isMac: boolean): string {
  const digit = MODE_TO_DIGIT[mode];
  return isMac ? `Meta+${digit}` : `Control+${digit}`;
}

/**
 * Whether `node` sits under an element with the `hidden` attribute,
 * crossing shadow-root boundaries. The app shell stays connected but
 * hidden while the terminal workspace is shown, so each header uses this
 * to leave the keys to whichever header is on screen.
 */
export function isInHiddenSubtree(node: Node): boolean {
  let current: Node | null = node;
  while (current) {
    if (current instanceof Element && current.hasAttribute('hidden')) return true;
    if (current.parentNode) {
      current = current.parentNode;
    } else if (current instanceof ShadowRoot) {
      current = current.host;
    } else {
      current = null;
    }
  }
  return false;
}
