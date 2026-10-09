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

// @vitest-environment happy-dom

import { describe, it, expect, afterEach, vi } from 'vitest';

import {
  areViewModeShortcutsEnabled,
  isInHiddenSubtree,
  isViewModeShortcutsStorageEvent,
  setViewModeShortcutsEnabled,
  VIEW_MODE_SHORTCUTS_CHANGED_EVENT,
  viewModeAriaKeyshortcuts,
  viewModeForShortcut,
  viewModeShortcutLabel,
} from './view-mode-shortcuts.js';

function key(init: KeyboardEventInit & { keyCode?: number }): KeyboardEvent {
  const e = new KeyboardEvent('keydown', init);
  if (init.keyCode !== undefined) {
    Object.defineProperty(e, 'keyCode', { value: init.keyCode });
  }
  return e;
}

afterEach(() => {
  localStorage.clear();
  document.body.innerHTML = '';
});

describe('viewModeForShortcut', () => {
  it('maps Ctrl+1/2/3 to dashboard, chat and terminals off macOS', () => {
    expect(viewModeForShortcut(key({ code: 'Digit1', key: '1', ctrlKey: true }), false)).toBe(
      'dashboard'
    );
    expect(viewModeForShortcut(key({ code: 'Digit2', key: '2', ctrlKey: true }), false)).toBe(
      'chat'
    );
    expect(viewModeForShortcut(key({ code: 'Digit3', key: '3', ctrlKey: true }), false)).toBe(
      'terminals'
    );
  });

  it('maps Cmd+1/2/3 on macOS', () => {
    expect(viewModeForShortcut(key({ code: 'Digit1', metaKey: true }), true)).toBe('dashboard');
    expect(viewModeForShortcut(key({ code: 'Digit2', metaKey: true }), true)).toBe('chat');
    expect(viewModeForShortcut(key({ code: 'Digit3', metaKey: true }), true)).toBe('terminals');
  });

  it('matches the physical key, whatever character the layout produces', () => {
    // AZERTY: the 1 key yields "&" without Shift.
    expect(viewModeForShortcut(key({ code: 'Digit1', key: '&', ctrlKey: true }), false)).toBe(
      'dashboard'
    );
  });

  it('requires the platform modifier and no other', () => {
    const cases: Array<[KeyboardEventInit, boolean]> = [
      [{ code: 'Digit1' }, false],
      [{ code: 'Digit1', metaKey: true }, false],
      [{ code: 'Digit1', ctrlKey: true }, true],
      [{ code: 'Digit1', ctrlKey: true, metaKey: true }, false],
      [{ code: 'Digit1', ctrlKey: true, metaKey: true }, true],
      [{ code: 'Digit1', ctrlKey: true, shiftKey: true }, false],
      [{ code: 'Digit1', ctrlKey: true, altKey: true }, false],
      [{ code: 'Digit1', metaKey: true, altKey: true }, true],
      [{ code: 'Digit1', metaKey: true, shiftKey: true }, true],
    ];
    for (const [init, isMac] of cases) {
      expect(viewModeForShortcut(key(init), isMac), JSON.stringify({ init, isMac })).toBeNull();
    }
  });

  it('ignores other digits and the numeric keypad', () => {
    expect(viewModeForShortcut(key({ code: 'Digit4', ctrlKey: true }), false)).toBeNull();
    expect(viewModeForShortcut(key({ code: 'Digit0', ctrlKey: true }), false)).toBeNull();
    expect(viewModeForShortcut(key({ code: 'Numpad1', ctrlKey: true }), false)).toBeNull();
  });

  it('ignores keys that are part of an IME composition', () => {
    expect(
      viewModeForShortcut(key({ code: 'Digit1', ctrlKey: true, isComposing: true }), false)
    ).toBeNull();
    expect(
      viewModeForShortcut(key({ code: 'Digit1', ctrlKey: true, keyCode: 229 }), false)
    ).toBeNull();
  });

  it('still matches a repeated keydown so the caller can hold it back from the browser', () => {
    expect(viewModeForShortcut(key({ code: 'Digit2', ctrlKey: true, repeat: true }), false)).toBe(
      'chat'
    );
  });
});

describe('shortcut labels', () => {
  it('uses the command symbol on macOS and Ctrl+ elsewhere', () => {
    expect(viewModeShortcutLabel('dashboard', true)).toBe('⌘1');
    expect(viewModeShortcutLabel('chat', true)).toBe('⌘2');
    expect(viewModeShortcutLabel('terminals', false)).toBe('Ctrl+3');
  });

  it('gives aria-keyshortcuts values', () => {
    expect(viewModeAriaKeyshortcuts('chat', true)).toBe('Meta+2');
    expect(viewModeAriaKeyshortcuts('terminals', false)).toBe('Control+3');
  });
});

describe('view mode shortcuts preference', () => {
  it('defaults to on', () => {
    expect(areViewModeShortcutsEnabled()).toBe(true);
  });

  it('persists off and back on, notifying listeners each time', () => {
    const listener = vi.fn();
    window.addEventListener(VIEW_MODE_SHORTCUTS_CHANGED_EVENT, listener);
    try {
      setViewModeShortcutsEnabled(false);
      expect(areViewModeShortcutsEnabled()).toBe(false);
      setViewModeShortcutsEnabled(true);
      expect(areViewModeShortcutsEnabled()).toBe(true);
      expect(listener).toHaveBeenCalledTimes(2);
    } finally {
      window.removeEventListener(VIEW_MODE_SHORTCUTS_CHANGED_EVENT, listener);
    }
  });
});

describe('isViewModeShortcutsStorageEvent', () => {
  it('matches a change to the preference key or a cleared store', () => {
    const own = new StorageEvent('storage', {
      key: 'scion-view-mode-shortcuts',
      storageArea: localStorage,
    });
    const cleared = new StorageEvent('storage', { key: null, storageArea: localStorage });
    expect(isViewModeShortcutsStorageEvent(own)).toBe(true);
    expect(isViewModeShortcutsStorageEvent(cleared)).toBe(true);
  });

  it('ignores other keys and session storage', () => {
    const other = new StorageEvent('storage', { key: 'scion-chime', storageArea: localStorage });
    const session = new StorageEvent('storage', {
      key: 'scion-view-mode-shortcuts',
      storageArea: sessionStorage,
    });
    expect(isViewModeShortcutsStorageEvent(other)).toBe(false);
    expect(isViewModeShortcutsStorageEvent(session)).toBe(false);
  });
});

describe('isInHiddenSubtree', () => {
  it('finds a hidden ancestor across a shadow root', () => {
    const outer = document.createElement('div');
    const host = document.createElement('div');
    const inner = document.createElement('span');
    host.attachShadow({ mode: 'open' }).appendChild(inner);
    outer.appendChild(host);
    document.body.appendChild(outer);

    expect(isInHiddenSubtree(inner)).toBe(false);
    outer.hidden = true;
    expect(isInHiddenSubtree(inner)).toBe(true);
  });
});
