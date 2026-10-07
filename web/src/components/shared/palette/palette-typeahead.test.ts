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

import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import {
  PALETTE_KEYBOARD_PROXY_MAX_MS,
  PALETTE_TYPEAHEAD_MAX_MS,
  PaletteTypeahead,
} from './palette-typeahead.js';

let typeahead: PaletteTypeahead;
let target: HTMLTextAreaElement;
let onTargetKeydown: Mock<(e: Event) => void>;
let onDocumentKeydown: Mock<(e: Event) => void>;

/**
 * Dispatches a keydown at the focused stand-in for the composer or a
 * terminal pane. AltGraph is reported only when `altGraph` is set, as in
 * browsers: happy-dom reports it whenever Alt is held.
 */
function press(key: string, init: KeyboardEventInit = {}, altGraph = false): KeyboardEvent {
  const e = new KeyboardEvent('keydown', {
    key,
    bubbles: true,
    composed: true,
    cancelable: true,
    ...init,
  });
  const modifierState = e.getModifierState.bind(e);
  Object.defineProperty(e, 'getModifierState', {
    value: (name: string): boolean => (name === 'AltGraph' ? altGraph : modifierState(name)),
  });
  target.dispatchEvent(e);
  return e;
}

function expectCaptured(e: KeyboardEvent): void {
  expect(e.defaultPrevented).toBe(true);
  expect(onTargetKeydown).not.toHaveBeenCalled();
  expect(onDocumentKeydown).not.toHaveBeenCalled();
}

function expectPassedThrough(e: KeyboardEvent): void {
  expect(e.defaultPrevented).toBe(false);
  expect(onTargetKeydown).toHaveBeenCalledTimes(1);
  expect(onDocumentKeydown).toHaveBeenCalledTimes(1);
}

beforeEach(() => {
  // Alt+key is a shortcut here, as on Windows and Linux; the macOS tests
  // below build their own.
  typeahead = new PaletteTypeahead({ mac: false });
  target = document.createElement('textarea');
  document.body.append(target);
  target.focus();
  onTargetKeydown = vi.fn<(e: Event) => void>();
  onDocumentKeydown = vi.fn<(e: Event) => void>();
  target.addEventListener('keydown', onTargetKeydown);
  document.addEventListener('keydown', onDocumentKeydown, true);
});

afterEach(() => {
  typeahead.stop();
  document.removeEventListener('keydown', onDocumentKeydown, true);
  target.remove();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('PaletteTypeahead', () => {
  it('captures nothing until started', () => {
    expect(typeahead.isCapturing).toBe(false);
    expectPassedThrough(press('a'));
    expect(typeahead.take()).toBe('');
  });

  it('captures printable keys before any other listener, and take() returns them', () => {
    typeahead.start();
    expect(typeahead.isCapturing).toBe(true);
    for (const key of ['b', 'o', 'B', ' ', '1', 'é']) expectCaptured(press(key));
    expect(typeahead.pending).toBe('boB 1é');
    expect(typeahead.take()).toBe('boB 1é');
    expect(typeahead.pending).toBe('');
  });

  it('captures a character outside the Basic Multilingual Plane as one key', () => {
    typeahead.start();
    expectCaptured(press('😀'));
    press('Backspace');
    expect(typeahead.take()).toBe('');
  });

  it('Backspace removes the last captured character, and is swallowed even with none left', () => {
    typeahead.start();
    press('a');
    press('b');
    expectCaptured(press('Backspace'));
    expect(typeahead.pending).toBe('a');
    press('Backspace');
    expectCaptured(press('Backspace'));
    expect(typeahead.pending).toBe('');
  });

  it('swallows Enter, Tab and navigation keys without adding text, so they act on nothing', () => {
    typeahead.start();
    press('x');
    for (const key of [
      'Enter',
      'Tab',
      'Delete',
      'ArrowUp',
      'ArrowDown',
      'ArrowLeft',
      'ArrowRight',
      'Home',
      'End',
      'PageUp',
      'PageDown',
    ]) {
      expectCaptured(press(key));
    }
    expectCaptured(press('Tab', { shiftKey: true }));
    expect(typeahead.pending).toBe('x');
  });

  it('lets Escape through, for the palette host to cancel the open', () => {
    typeahead.start();
    expectPassedThrough(press('Escape'));
    expect(typeahead.isCapturing).toBe(true);
  });

  it.each([
    ['Ctrl', { ctrlKey: true }],
    ['Meta', { metaKey: true }],
    ['Alt', { altKey: true }],
  ])('lets a %s chord through, such as the shortcut that toggles the palette', (_name, init) => {
    typeahead.start();
    expectPassedThrough(press('k', init));
    expect(typeahead.pending).toBe('');
  });

  it('captures a character typed with AltGr, which Windows reports as Ctrl+Alt', () => {
    typeahead.start();
    expectCaptured(press('@', { ctrlKey: true, altKey: true }, true));
    expectCaptured(press('€', { altKey: true }, true));
    expect(typeahead.pending).toBe('@€');
  });

  it('lets a non-printable key through even with AltGr held, Enter and arrows included', () => {
    typeahead.start();
    press('a');
    for (const key of ['Enter', 'ArrowLeft', 'Home']) {
      const e = press(key, { ctrlKey: true, altKey: true }, true);
      expect(e.defaultPrevented).toBe(false);
    }
    expect(onTargetKeydown).toHaveBeenCalledTimes(3);
    expect(typeahead.pending).toBe('a');
  });

  it.each([
    ['Ctrl', { ctrlKey: true }, false, 'my '],
    ['Ctrl+Shift (a word delete by choice)', { ctrlKey: true, shiftKey: true }, false, 'my '],
    ['Shift', { shiftKey: true }, false, 'my lon'],
    ['Alt', { altKey: true }, false, 'my long'],
    ['Meta', { metaKey: true }, false, 'my long'],
    ['Ctrl+Alt', { ctrlKey: true, altKey: true }, false, 'my long'],
    ['AltGr', { ctrlKey: true, altKey: true }, true, 'my long'],
  ])(
    'swallows %s+Backspace outside macOS and applies the chosen edit to the captured text',
    (_name, init, altGraph, rest) => {
      typeahead.start();
      for (const key of 'my long') press(key);
      expectCaptured(press('Backspace', init, altGraph));
      expect(typeahead.pending).toBe(rest);
    }
  );

  it.each([
    ['Option', { altKey: true }, 'my '],
    ['Cmd', { metaKey: true }, ''],
    ['Ctrl', { ctrlKey: true }, 'my lon'],
    ['Shift', { shiftKey: true }, 'my lon'],
  ])(
    'swallows %s+Backspace on macOS and applies the chosen edit to the captured text',
    (_name, init, rest) => {
      typeahead = new PaletteTypeahead({ mac: true });
      typeahead.start();
      for (const key of 'my long') press(key);
      expectCaptured(press('Backspace', init));
      expect(typeahead.pending).toBe(rest);
    }
  );

  it.each([
    ['my long ', 'my '],
    ['my long', 'my '],
    ['a  b  ', 'a  '],
    ['foo-bar', 'foo-'],
    ['co@', 'co'],
    ['user@host.x', 'user@host.'],
    ['é😀 ab', 'é😀 '],
    ['foo_bar', ''],
    ['coder2', ''],
    ['abc12', ''],
    ['a!!', 'a'],
    ['--', ''],
    ['', ''],
  ])('a word delete of %j leaves %j', (typed, rest) => {
    for (const mac of [false, true]) {
      const t = new PaletteTypeahead({ mac });
      t.start();
      try {
        for (const key of Array.from(typed)) press(key);
        expectCaptured(press('Backspace', mac ? { altKey: true } : { ctrlKey: true }));
        expect(t.pending).toBe(rest);
      } finally {
        t.stop();
        onTargetKeydown.mockClear();
        onDocumentKeydown.mockClear();
      }
    }
  });

  it('a Backspace that is part of an IME composition passes through and leaves the captured text', () => {
    typeahead.start();
    press('a');
    expectPassedThrough(press('Backspace', { isComposing: true }));
    expect(typeahead.pending).toBe('a');
  });

  it.each([
    ['plain', {}],
    ['Ctrl', { ctrlKey: true }],
    ['Alt', { altKey: true }],
    ['Meta', { metaKey: true }],
  ])('swallows %s Delete without changing the captured text', (_name, init) => {
    typeahead.start();
    press('a');
    expectCaptured(press('Delete', init));
    expect(typeahead.pending).toBe('a');
  });

  it('lets a Meta chord through even with AltGr held', () => {
    typeahead.start();
    expectPassedThrough(press('v', { metaKey: true, altKey: true }, true));
    expect(typeahead.pending).toBe('');
  });

  describe('on macOS', () => {
    beforeEach(() => {
      typeahead = new PaletteTypeahead({ mac: true });
    });

    it('captures a character typed with Option as text', () => {
      typeahead.start();
      expectCaptured(press('@', { altKey: true }));
      expectCaptured(press('∫', { altKey: true }));
      expect(typeahead.pending).toBe('@∫');
    });

    it('lets Option with Ctrl or Meta, and a navigation key with Option, through', () => {
      typeahead.start();
      press('k', { altKey: true, ctrlKey: true });
      press('k', { altKey: true, metaKey: true });
      press('ArrowLeft', { altKey: true });
      press('Home', { altKey: true });
      expect(onTargetKeydown).toHaveBeenCalledTimes(4);
      expect(typeahead.pending).toBe('');
    });
  });

  it('defaults to treating Option characters as text on macOS only', () => {
    const platform = Object.getOwnPropertyDescriptor(window.navigator, 'platform');
    const results: boolean[] = [];
    try {
      for (const value of ['MacIntel', 'Win32']) {
        Object.defineProperty(window.navigator, 'platform', { value, configurable: true });
        const t = new PaletteTypeahead();
        t.start();
        try {
          results.push(press('@', { altKey: true }).defaultPrevented);
        } finally {
          t.stop();
        }
      }
    } finally {
      if (platform) Object.defineProperty(window.navigator, 'platform', platform);
      else delete (window.navigator as unknown as Record<string, unknown>).platform;
    }
    expect(results).toEqual([true, false]);
  });

  it('lets a dead key through', () => {
    typeahead.start();
    expectPassedThrough(press('Dead'));
    expect(typeahead.pending).toBe('');
  });

  it('keeps a captured key from window capture listeners added after it started', () => {
    typeahead.start();
    const later = vi.fn();
    window.addEventListener('keydown', later, true);
    try {
      press('a');
      expect(later).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('keydown', later, true);
    }
  });

  it('lets keys that are part of an IME composition through', () => {
    typeahead.start();
    expectPassedThrough(press('a', { isComposing: true }));
    expect(typeahead.pending).toBe('');
  });

  it('lets lone modifiers and function keys through', () => {
    typeahead.start();
    for (const key of ['Shift', 'Control', 'F5', 'Process']) press(key);
    expect(onTargetKeydown).toHaveBeenCalledTimes(4);
    expect(typeahead.pending).toBe('');
  });

  it('Shift with a printable key is captured as the shifted character', () => {
    typeahead.start();
    expectCaptured(press('A', { shiftKey: true }));
    expect(typeahead.pending).toBe('A');
  });

  it('take() stops capturing: later keys reach their target', () => {
    typeahead.start();
    press('a');
    typeahead.take();
    expect(typeahead.isCapturing).toBe(false);
    expectPassedThrough(press('b'));
    expect(typeahead.take()).toBe('');
  });

  it('stop() stops capturing and discards the captured text', () => {
    typeahead.start();
    press('a');
    typeahead.stop();
    expect(typeahead.isCapturing).toBe(false);
    expect(typeahead.take()).toBe('');
    expectPassedThrough(press('b'));
  });

  it('start() while capturing keeps the text captured so far', () => {
    typeahead.start();
    press('a');
    typeahead.start();
    press('b');
    expect(typeahead.take()).toBe('ab');
  });

  it('a fresh start() after a stop begins with no text', () => {
    typeahead.start();
    press('a');
    typeahead.stop();
    typeahead.start();
    press('b');
    expect(typeahead.take()).toBe('b');
  });

  it('stops capturing on its own after the time limit, keeping the text for a late take()', () => {
    vi.useFakeTimers();
    typeahead.start();
    press('a');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS - 1);
    expect(typeahead.isCapturing).toBe(true);
    vi.advanceTimersByTime(1);
    expect(typeahead.isCapturing).toBe(false);
    expectPassedThrough(press('b'));
    expect(typeahead.take()).toBe('a');
  });

  it('start() while capturing restarts the time limit', () => {
    vi.useFakeTimers();
    typeahead.start();
    press('a');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS - 1);
    typeahead.start();
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS - 1);
    expect(typeahead.isCapturing).toBe(true);
    expectCaptured(press('b'));
    vi.advanceTimersByTime(1);
    expect(typeahead.isCapturing).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
    expect(typeahead.take()).toBe('ab');
  });

  it('a start() after a capture reached its time limit begins with no text', () => {
    vi.useFakeTimers();
    typeahead.start();
    press('c');
    press('o');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    expect(typeahead.pending).toBe('co');
    typeahead.start();
    expect(typeahead.pending).toBe('');
  });

  it('a stop before the time limit leaves no timer behind', () => {
    vi.useFakeTimers();
    typeahead.start();
    typeahead.stop();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('adds one window capture listener on start and removes that same listener on stop', () => {
    const add = vi.spyOn(window, 'addEventListener');
    const remove = vi.spyOn(window, 'removeEventListener');
    typeahead.start();
    typeahead.start();
    expect(add).toHaveBeenCalledTimes(1);
    expect(add).toHaveBeenCalledWith('keydown', expect.any(Function), true);
    typeahead.stop();
    typeahead.stop();
    expect(remove).toHaveBeenCalledTimes(1);
    expect(remove.mock.calls[0]).toEqual(add.mock.calls[0]);
  });
});

describe('PaletteTypeahead: holding the on-screen keyboard', () => {
  function proxies(): NodeListOf<HTMLInputElement> {
    return document.querySelectorAll<HTMLInputElement>('input[data-palette-keyboard-proxy]');
  }

  function touchTypeahead(): PaletteTypeahead {
    return new PaletteTypeahead({ mac: false, holdsKeyboard: () => true });
  }

  afterEach(() => {
    for (const proxy of proxies()) proxy.remove();
  });

  it('focuses a hidden text field synchronously from start(), so a tap shows the keyboard', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy;
    expect(proxy).toBeInstanceOf(HTMLInputElement);
    expect(proxy?.type).toBe('text');
    expect(proxy?.isConnected).toBe(true);
    expect(document.activeElement).toBe(proxy);
  });

  it('keeps the field from scrolling, zooming or showing', () => {
    typeahead = touchTypeahead();
    const focus = vi.spyOn(HTMLElement.prototype, 'focus');
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    expect(proxy.style.position).toBe('fixed');
    expect(proxy.style.opacity).toBe('0');
    expect(proxy.style.fontSize).toBe('16px');
    expect(proxy.style.pointerEvents).toBe('none');
    expect(proxy.tabIndex).toBe(-1);
    expect(proxy.readOnly).toBe(false);
  });

  it('reads holdsKeyboard at each start, and uses no field where it does not hold', () => {
    let touch = false;
    typeahead = new PaletteTypeahead({ mac: false, holdsKeyboard: (): boolean => touch });
    typeahead.start();
    expect(typeahead.keyboardProxy).toBeNull();
    expect(document.activeElement).toBe(target);
    typeahead.stop();
    touch = true;
    typeahead.start();
    expect(document.activeElement).toBe(typeahead.keyboardProxy);
  });

  it('does not hold the keyboard by default off a touch-primary device', () => {
    typeahead.start();
    expect(typeahead.keyboardProxy).toBeNull();
    expect(proxies()).toHaveLength(0);
    expect(document.activeElement).toBe(target);
  });

  it('holds the keyboard by default on a touch-primary device', () => {
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        ({
          matches: query === '(hover: none) and (pointer: coarse)',
          media: query,
        }) as MediaQueryList
    );
    typeahead = new PaletteTypeahead({ mac: false });
    typeahead.start();
    expect(document.activeElement).toBe(typeahead.keyboardProxy);
  });

  it('a start while capturing keeps the one field', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy;
    typeahead.start();
    expect(typeahead.keyboardProxy).toBe(proxy);
    expect(proxies()).toHaveLength(1);
  });

  it('still captures keys typed at the field', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    const e = new KeyboardEvent('keydown', { key: 'q', bubbles: true, cancelable: true });
    proxy.dispatchEvent(e);
    expect(e.defaultPrevented).toBe(true);
    expect(typeahead.pending).toBe('q');
  });

  it('take() once another field has focus removes the field without moving focus, keeping its text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    press('a');
    const proxy = typeahead.keyboardProxy!;
    // Text the field took itself: a key the capture lets through, like IME input.
    proxy.value = 'b';
    const query = document.createElement('input');
    document.body.append(query);
    query.focus();
    expect(typeahead.take()).toBe('ab');
    expect(proxy.isConnected).toBe(false);
    expect(typeahead.keyboardProxy).toBeNull();
    expect(document.activeElement).toBe(query);
    query.remove();
  });

  it('a stop while the field still has focus gives focus back to what had it', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    typeahead.stop();
    expect(proxy.isConnected).toBe(false);
    expect(document.activeElement).toBe(target);
  });

  it('a stop leaves focus alone when what had it has left the page', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    target.remove();
    typeahead.stop();
    expect(proxies()).toHaveLength(0);
    expect(document.activeElement).not.toBe(target);
  });

  it('the field outlives the capture time limit, keeping keys typed after it for take()', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    press('c');
    const proxy = typeahead.keyboardProxy!;
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    expect(typeahead.isCapturing).toBe(false);
    expect(proxy.isConnected).toBe(true);
    expect(document.activeElement).toBe(proxy);
    // Past the limit, keys reach the field itself.
    proxy.value = 'o';
    const query = document.createElement('input');
    document.body.append(query);
    query.focus();
    expect(typeahead.take()).toBe('co');
    expect(proxy.isConnected).toBe(false);
    expect(document.activeElement).toBe(query);
    query.remove();
  });

  it('the field is dropped at its own cap, giving focus back and keeping its text for a late take()', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    press('c');
    typeahead.keyboardProxy!.value = 'o';
    vi.advanceTimersByTime(PALETTE_KEYBOARD_PROXY_MAX_MS - 1);
    expect(proxies()).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(proxies()).toHaveLength(0);
    expect(document.activeElement).toBe(target);
    expect(typeahead.take()).toBe('co');
  });

  it('a start restarts the field cap', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    vi.advanceTimersByTime(PALETTE_KEYBOARD_PROXY_MAX_MS - 1);
    typeahead.start();
    vi.advanceTimersByTime(PALETTE_KEYBOARD_PROXY_MAX_MS - 1);
    expect(proxies()).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(proxies()).toHaveLength(0);
  });

  it('a fresh capture after the time limit starts with no text, in the field either', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    press('a');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    typeahead.keyboardProxy!.value = 'b';
    typeahead.start();
    press('c');
    expect(typeahead.take()).toBe('c');
  });

  it('take() and stop() leave no timer behind', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    typeahead.take();
    expect(vi.getTimerCount()).toBe(0);
    typeahead.start();
    typeahead.stop();
    expect(vi.getTimerCount()).toBe(0);
  });

  /**
   * Commits `text` into the field as Chromium would: composed (input events
   * while composing, then compositionend), or inserted directly.
   */
  function commitAtField(proxy: HTMLInputElement, text: string, composed: boolean): void {
    if (composed) {
      proxy.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true }));
      proxy.value += text;
      proxy.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: true }));
      proxy.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true, data: text }));
    } else {
      proxy.value += text;
      proxy.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: false }));
    }
  }

  /**
   * Composes `marked` and confirms it as `confirmed` in WebKit's order:
   * compositionend while the field still holds the marked text, then the
   * confirmed text replaces it with an input event outside the composition.
   * `between` runs after compositionend, before the insertion.
   */
  function composeAtFieldWebKit(
    proxy: HTMLInputElement,
    marked: string,
    confirmed: string,
    between: () => void = () => {}
  ): void {
    const start = proxy.value;
    proxy.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true }));
    proxy.value = start + marked;
    proxy.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: true }));
    proxy.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true, data: confirmed }));
    between();
    proxy.value = proxy.value.endsWith(marked)
      ? proxy.value.slice(0, -marked.length) + confirmed
      : proxy.value + confirmed;
    proxy.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: false }));
  }

  it('text composed in WebKit order joins once, in its place among captured keys', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    composeAtFieldWebKit(proxy, 'にほん', '日本');
    press('b');
    expect(typeahead.take()).toBe('a日本b');
  });

  it('an IME-processed key between WebKit compositionend and the insertion does not split the text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    composeAtFieldWebKit(proxy, 'にほん', '日本', () => {
      press('Enter', { keyCode: 229 } as KeyboardEventInit);
    });
    expect(typeahead.take()).toBe('a日本');
  });

  it('an IME-processed Backspace after field text passes to the field, which edits its own text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, '日本', true);
    const e = press('Backspace', { keyCode: 229 } as KeyboardEventInit);
    expect(e.defaultPrevented).toBe(false);
    // The field's own Backspace.
    proxy.value = proxy.value.slice(0, -1);
    expect(typeahead.take()).toBe('a日');
  });

  it('an IME-processed character after field text passes to the field, keeping typing order', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, 'hello', false);
    const e = press('x', { keyCode: 229 } as KeyboardEventInit);
    expect(e.defaultPrevented).toBe(false);
    // The field takes the character itself.
    proxy.value += 'x';
    expect(typeahead.take()).toBe('ahellox');
  });

  /** An IME-processed Backspace, applied by the field itself when the capture lets it through. */
  function imeBackspace(proxy: HTMLInputElement): KeyboardEvent {
    const e = press('Backspace', { keyCode: 229 } as KeyboardEventInit);
    if (!e.defaultPrevented) proxy.value = proxy.value.slice(0, -1);
    return e;
  }

  it('IME-processed Backspaces delete the field text, then the captured text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, '日本', true);
    expect(imeBackspace(proxy).defaultPrevented).toBe(false);
    expect(imeBackspace(proxy).defaultPrevented).toBe(false);
    expect(imeBackspace(proxy).defaultPrevented).toBe(true);
    expect(typeahead.take()).toBe('');
  });

  it('an IME-processed Backspace with the field empty deletes captured text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    press('b');
    expect(imeBackspace(proxy).defaultPrevented).toBe(true);
    expect(typeahead.take()).toBe('a');
  });

  it('an IME-processed character passed to the field keeps its place before a later captured key', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    const e = press('x', { keyCode: 229 } as KeyboardEventInit);
    expect(e.defaultPrevented).toBe(false);
    proxy.value += 'x';
    press('y');
    expect(typeahead.take()).toBe('axy');
  });

  it('an IME-processed Delete passes to the field and loses nothing, with or without field text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    expect(press('Delete', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(false);
    commitAtField(proxy, 'hi', false);
    expect(press('Delete', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(false);
    expect(typeahead.take()).toBe('ahi');
  });

  it('with the field out of focus, the capture takes IME-processed keys', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, 'hi', false);
    target.focus();
    expect(press('x', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(true);
    expect(press('Backspace', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(true);
    expect(typeahead.take()).toBe('ahi');
  });

  it('an IME-processed Enter is swallowed without moving field text', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    commitAtField(proxy, 'hi', false);
    const e = press('Enter', { keyCode: 229 } as KeyboardEventInit);
    expect(e.defaultPrevented).toBe(true);
    expect(proxy.value).toBe('hi');
    expect(typeahead.pending).toBe('');
  });

  it('without the field, the capture takes an IME-processed key', () => {
    typeahead.start();
    expect(press('x', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(true);
    press('y');
    expect(press('Backspace', { keyCode: 229 } as KeyboardEventInit).defaultPrevented).toBe(true);
    expect(typeahead.take()).toBe('x');
  });

  it('after the capture time limit, Backspace in the field edits the text it holds', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('x');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    commitAtField(proxy, 'y', false);
    // The field's own Backspace.
    proxy.value = proxy.value.slice(0, -1);
    proxy.dispatchEvent(
      new InputEvent('input', { bubbles: true, inputType: 'deleteContentBackward' })
    );
    expect(typeahead.take()).toBe('x');
  });

  it('text composed in the field keeps its place among captured keys', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, '你好', true);
    press('b');
    expect(typeahead.pending).toBe('a你好b');
    expect(typeahead.take()).toBe('a你好b');
  });

  it('text inserted into the field without a key (dictation, a suggestion) keeps its place', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    commitAtField(proxy, 'hello', false);
    press('b');
    expect(typeahead.take()).toBe('ahellob');
  });

  it('Backspace after composed text deletes from its end', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    commitAtField(typeahead.keyboardProxy!, 'ok', true);
    press('Backspace');
    expect(typeahead.take()).toBe('o');
  });

  it('text still being composed stays in the field once, and joins at take()', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    press('a');
    proxy.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true }));
    proxy.value = 'ni';
    proxy.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: true }));
    expect(typeahead.pending).toBe('a');
    expect(proxy.value).toBe('ni');
    expect(typeahead.take()).toBe('ani');
  });

  it('after the capture time limit, text typed into the field keeps its order with captured keys', () => {
    vi.useFakeTimers();
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    commitAtField(proxy, '日', true);
    press('x');
    vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    commitAtField(proxy, 'y', false);
    expect(typeahead.take()).toBe('日xy');
  });

  it('stop({ restoreFocus: false }) drops the field without giving focus back', () => {
    typeahead = touchTypeahead();
    typeahead.start();
    const proxy = typeahead.keyboardProxy!;
    typeahead.stop({ restoreFocus: false });
    expect(proxy.isConnected).toBe(false);
    expect(document.activeElement).not.toBe(target);
    expect(typeahead.take()).toBe('');
  });
});
