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
import { PALETTE_TYPEAHEAD_MAX_MS, PaletteTypeahead } from './palette-typeahead.js';

let typeahead: PaletteTypeahead;
let target: HTMLTextAreaElement;
let onTargetKeydown: Mock<(e: Event) => void>;
let onDocumentKeydown: Mock<(e: Event) => void>;

/** Dispatches a keydown at the focused stand-in for the composer or a terminal pane. */
function press(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const e = new KeyboardEvent('keydown', {
    key,
    bubbles: true,
    composed: true,
    cancelable: true,
    ...init,
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
  typeahead = new PaletteTypeahead();
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
