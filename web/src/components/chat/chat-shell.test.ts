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
 * Tests for the chat shell's text-entry state: set while a text field in the
 * shell (outside the top bar and any dialog) has focus, and kept in step
 * with programmatic focus moves through the focus-moved helpers even when
 * no focus event reaches the shell.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import './chat-shell.js';
import {
  blurElement,
  FOCUS_MOVED_EVENT,
  focusElement,
  withFocusMove,
} from '../shared/focus-moved.js';

let shell: HTMLElement & { updateComplete: Promise<unknown> };
let field: HTMLTextAreaElement;
let button: HTMLButtonElement;

/** Swallow focus events before they reach anything, as a silent move would. */
const swallow = (e: Event): void => e.stopImmediatePropagation();
function silenceFocusEvents(on: boolean): void {
  for (const type of ['focus', 'focusin', 'focusout', 'blur']) {
    if (on) window.addEventListener(type, swallow, true);
    else window.removeEventListener(type, swallow, true);
  }
}

const textEntry = (): boolean => shell.hasAttribute('text-entry');

/** happy-dom has no visualViewport: a stand-in the shell can listen to. */
const visualViewport = new EventTarget();

beforeEach(async () => {
  Object.defineProperty(window, 'visualViewport', {
    configurable: true,
    get: () => visualViewport,
  });
  shell = document.createElement('scion-chat-shell') as typeof shell;
  // The page is slotted into the shell, in a shadow root of its own.
  const page = document.createElement('div');
  const root = page.attachShadow({ mode: 'open' });
  field = document.createElement('textarea');
  button = document.createElement('button');
  root.append(field, button);
  shell.appendChild(page);
  document.body.appendChild(shell);
  await shell.updateComplete;
});

afterEach(() => {
  silenceFocusEvents(false);
  shell.remove();
});

describe('chat shell text-entry state', () => {
  it('follows focus events into and out of a text field', async () => {
    field.focus();
    expect(textEntry()).toBe(true);
    button.focus();
    await new Promise((r) => requestAnimationFrame(r));
    expect(textEntry()).toBe(false);
  });

  it('misses a silent focus move made without the helper', () => {
    silenceFocusEvents(true);
    field.focus();
    expect(textEntry()).toBe(false);
  });

  it('follows a silent focus move made through focusElement', () => {
    silenceFocusEvents(true);
    focusElement(field);
    expect(textEntry()).toBe(true);
    focusElement(button);
    expect(textEntry()).toBe(false);
  });

  it('follows a silent blur made through blurElement', () => {
    focusElement(field);
    silenceFocusEvents(true);
    blurElement(field);
    expect(textEntry()).toBe(false);
  });

  it('follows a silent move made inside withFocusMove', () => {
    silenceFocusEvents(true);
    withFocusMove(() => field.focus());
    expect(textEntry()).toBe(true);
  });

  it('typing re-reads focus, as a safety net', () => {
    silenceFocusEvents(true);
    field.focus();
    expect(textEntry()).toBe(false);
    field.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    expect(textEntry()).toBe(true);
  });

  it('ignores a text field inside a dialog', () => {
    const dialog = document.createElement('div');
    dialog.setAttribute('role', 'dialog');
    const input = document.createElement('input');
    dialog.appendChild(input);
    field.getRootNode().appendChild(dialog);
    focusElement(input);
    expect(textEntry()).toBe(false);
  });

  it('removes every listener it added on disconnect', () => {
    shell.remove();
    const added: Array<[EventTarget, string, unknown]> = [];
    const removed: Array<[EventTarget, string, unknown]> = [];
    const targets: EventTarget[] = [window, shell, visualViewport];
    for (const target of targets) {
      const add = target.addEventListener.bind(target);
      const remove = target.removeEventListener.bind(target);
      vi.spyOn(target, 'addEventListener').mockImplementation((type, listener, options) => {
        added.push([target, type, listener]);
        add(type, listener, options);
      });
      vi.spyOn(target, 'removeEventListener').mockImplementation((type, listener, options) => {
        removed.push([target, type, listener]);
        remove(type, listener, options);
      });
    }
    document.body.appendChild(shell);
    const types = added.map(([, type]) => type);
    for (const type of ['focusin', 'focusout', 'input', FOCUS_MOVED_EVENT]) {
      expect(types).toContain(type);
    }
    expect(added.some(([target, type]) => target === visualViewport && type === 'resize')).toBe(
      true
    );
    shell.remove();
    for (const entry of added) expect(removed).toContainEqual(entry);
    vi.restoreAllMocks();
    document.body.appendChild(shell);
  });

  it('re-reads focus when the keyboard opens or closes', async () => {
    silenceFocusEvents(true);
    field.focus();
    expect(textEntry()).toBe(false);
    visualViewport.dispatchEvent(new Event('resize'));
    await new Promise((r) => requestAnimationFrame(r));
    expect(textEntry()).toBe(true);
  });

  it('publishes the top bar height on a change only, synchronously', async () => {
    const callbacks: ResizeObserverCallback[] = [];
    const original = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      constructor(cb: ResizeObserverCallback) {
        callbacks.push(cb);
      }
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    } as unknown as typeof ResizeObserver;
    try {
      shell.remove();
      document.body.appendChild(shell);
      await shell.updateComplete;
      await Promise.resolve();
      const header = shell.shadowRoot!.querySelector('scion-header')!;
      let height = 61;
      header.getBoundingClientRect = () => ({ height }) as DOMRect;
      const set = vi.spyOn(shell.style, 'setProperty');
      const fire = (): void => callbacks.at(-1)!([], {} as ResizeObserver);
      fire();
      expect(shell.style.getPropertyValue('--scion-chat-top-bar-h')).toBe('61px');
      fire();
      fire();
      expect(set).toHaveBeenCalledTimes(1);
      height = 0; // hidden: keep the last visible height
      fire();
      expect(set).toHaveBeenCalledTimes(1);
      height = 70;
      fire();
      expect(set).toHaveBeenCalledTimes(2);
      expect(shell.style.getPropertyValue('--scion-chat-top-bar-h')).toBe('70px');
    } finally {
      globalThis.ResizeObserver = original;
    }
  });
});
