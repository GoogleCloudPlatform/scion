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

import { afterEach, describe, expect, it, vi } from 'vitest';

import './review-note-dialog.js';
import type { ScionReviewNoteDialog } from './review-note-dialog.js';

afterEach(() => {
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

async function mount(): Promise<ScionReviewNoteDialog> {
  const el = document.createElement('scion-review-note-dialog') as ScionReviewNoteDialog;
  document.body.append(el);
  await el.updateComplete;
  return el;
}

describe('scion-review-note-dialog', () => {
  it('claims sl-initial-focus so sl-dialog does not steal focus to its panel', async () => {
    const el = await mount();
    const pending = el.ask({ note: 'Existing', number: 1, quote: '', context: '' });
    await el.updateComplete;

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    const textarea = el.shadowRoot!.querySelector('textarea')!;
    const focusSpy = vi.spyOn(textarea, 'focus');
    const selectSpy = vi.spyOn(textarea, 'select');

    const ev = new CustomEvent('sl-initial-focus', { cancelable: true });
    slDialog.dispatchEvent(ev);

    expect(ev.defaultPrevented).toBe(true);
    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    expect(selectSpy).toHaveBeenCalledOnce();

    // Enter saves and closes.
    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }));
    await expect(pending).resolves.toBe('Existing');
    await el.updateComplete;
    expect(slDialog.hasAttribute('open')).toBe(false);
  });

  it('keeps textarea focus and selection if already focused before sl-initial-focus fires', async () => {
    const el = await mount();
    const pending = el.ask({ note: '', number: 0, quote: 'written', context: '' });
    await el.updateComplete;

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    const textarea = el.shadowRoot!.querySelector('textarea')!;
    textarea.focus();
    textarea.value = 'Why?';
    textarea.dispatchEvent(new Event('input'));

    const selectSpy = vi.spyOn(textarea, 'select');
    const ev = new CustomEvent('sl-initial-focus', { cancelable: true });
    slDialog.dispatchEvent(ev);

    expect(ev.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.activeElement).toBe(textarea);
    expect(selectSpy).not.toHaveBeenCalled();

    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }));
    await expect(pending).resolves.toBe('Why?');
  });

  it('ignores sl-initial-focus bubbling from a nested element and after close', async () => {
    const el = await mount();
    const pending = el.ask({ note: '', number: 0, quote: '', context: '' });
    await el.updateComplete;

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    const textarea = el.shadowRoot!.querySelector('textarea')!;
    const focusSpy = vi.spyOn(textarea, 'focus');

    // Bubbled event from a child inside sl-dialog is ignored.
    const nestedEv = new CustomEvent('sl-initial-focus', { bubbles: true, cancelable: true });
    textarea.dispatchEvent(nestedEv);
    expect(nestedEv.defaultPrevented).toBe(false);
    expect(focusSpy).not.toHaveBeenCalled();

    // Closing before sl-initial-focus still prevents Shoelace panel focus without focusing textarea.
    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
    await el.updateComplete;

    const lateEv = new CustomEvent('sl-initial-focus', { cancelable: true });
    slDialog.dispatchEvent(lateEv);
    expect(lateEv.defaultPrevented).toBe(true);
    expect(focusSpy).not.toHaveBeenCalled();

    slDialog.dispatchEvent(new CustomEvent('sl-after-show'));
    expect(focusSpy).not.toHaveBeenCalled();
  });

  it('restores textarea focus on sl-after-show if focus moved away during open, without clobbering focused textarea', async () => {
    const el = await mount();
    const pending = el.ask({ note: 'Note', number: 1, quote: '', context: '' });
    await el.updateComplete;

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    const textarea = el.shadowRoot!.querySelector('textarea')!;
    const focusSpy = vi.spyOn(textarea, 'focus');
    const selectSpy = vi.spyOn(textarea, 'select');

    // Focus was stolen before sl-after-show (e.g. second click of a double-click).
    slDialog.dispatchEvent(new CustomEvent('sl-after-show'));
    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    expect(selectSpy).toHaveBeenCalledOnce();

    // If textarea already has focus when sl-after-show fires, do not re-select.
    textarea.focus();
    focusSpy.mockClear();
    selectSpy.mockClear();
    slDialog.dispatchEvent(new CustomEvent('sl-after-show'));
    expect(focusSpy).not.toHaveBeenCalled();
    expect(selectSpy).not.toHaveBeenCalled();

    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
  });

  it('does not save on Shift+Enter', async () => {
    const el = await mount();
    const pending = el.ask({ note: 'Line 1', number: 1, quote: '', context: '' });
    await el.updateComplete;

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    const textarea = el.shadowRoot!.querySelector('textarea')!;

    const shiftEnter = new KeyboardEvent('keydown', {
      key: 'Enter',
      shiftKey: true,
      cancelable: true,
    });
    textarea.dispatchEvent(shiftEnter);
    expect(shiftEnter.defaultPrevented).toBe(false);
    await el.updateComplete;
    expect(slDialog.hasAttribute('open')).toBe(true);

    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
  });
});

describe('scion-review-note-dialog: visualViewport mobile keyboard tracking', () => {
  class FakeVisualViewport {
    height = 844;
    offsetTop = 0;
    private listeners = new Map<string, Set<() => void>>();

    addEventListener(type: string, listener: () => void): void {
      let set = this.listeners.get(type);
      if (!set) {
        set = new Set();
        this.listeners.set(type, set);
      }
      set.add(listener);
    }

    removeEventListener(type: string, listener: () => void): void {
      this.listeners.get(type)?.delete(listener);
    }

    fire(type: 'resize' | 'scroll', height: number, offsetTop = this.offsetTop): void {
      this.height = height;
      this.offsetTop = offsetTop;
      for (const listener of this.listeners.get(type) ?? []) listener();
    }

    listenerCount(type?: string): number {
      if (type) return this.listeners.get(type)?.size ?? 0;
      let total = 0;
      for (const set of this.listeners.values()) total += set.size;
      return total;
    }
  }

  it('tracks visualViewport height and offsetTop while open and scrolls focused textarea into view', async () => {
    const vv = new FakeVisualViewport();
    vi.stubGlobal('visualViewport', vv);
    const el = await mount();

    const pending = el.ask({ note: '', number: 0, quote: 'selected text', context: '' });
    await el.updateComplete;

    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('844px');
    expect(el.style.getPropertyValue('--note-dialog-vv-top')).toBe('0px');
    expect(vv.listenerCount('resize')).toBe(1);
    expect(vv.listenerCount('scroll')).toBe(1);

    const textarea = el.shadowRoot!.querySelector('textarea')!;
    const scrollSpy = vi.spyOn(textarea, 'scrollIntoView').mockImplementation(() => {});

    // Focusing textarea syncs viewport and scrolls it into view.
    textarea.focus();
    textarea.dispatchEvent(new Event('focus'));
    expect(scrollSpy).toHaveBeenCalledWith({ block: 'nearest', inline: 'nearest' });

    // Keyboard opens (resize with reduced height) and pans (scroll with non-zero offsetTop).
    scrollSpy.mockClear();
    vv.fire('resize', 370, 0);
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('370px');
    expect(el.style.getPropertyValue('--note-dialog-vv-top')).toBe('0px');
    expect(scrollSpy).toHaveBeenCalledWith({ block: 'nearest', inline: 'nearest' });

    scrollSpy.mockClear();
    vv.fire('scroll', 370, 48);
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('370px');
    expect(el.style.getPropertyValue('--note-dialog-vv-top')).toBe('48px');
    expect(scrollSpy).toHaveBeenCalledWith({ block: 'nearest', inline: 'nearest' });

    // Closing removes listeners and clears custom properties.
    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
    expect(vv.listenerCount()).toBe(0);
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('');
    expect(el.style.getPropertyValue('--note-dialog-vv-top')).toBe('');

    // A late sl-after-show after close does not re-seed the properties.
    slDialog.dispatchEvent(new CustomEvent('sl-after-show'));
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('');
    expect(el.style.getPropertyValue('--note-dialog-vv-top')).toBe('');
  });

  it('cleans up visualViewport listeners on disconnect and resumes on reconnect while open', async () => {
    const vv = new FakeVisualViewport();
    vi.stubGlobal('visualViewport', vv);
    const el = await mount();

    const pending = el.ask({ note: '', number: 0, quote: '', context: '' });
    await el.updateComplete;
    expect(vv.listenerCount()).toBe(2);

    el.remove();
    expect(vv.listenerCount()).toBe(0);
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('');

    document.body.append(el);
    await el.updateComplete;
    expect(vv.listenerCount()).toBe(2);
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('844px');

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
    expect(vv.listenerCount()).toBe(0);
  });

  it('handles missing window.visualViewport gracefully', async () => {
    vi.stubGlobal('visualViewport', undefined);
    const el = await mount();

    const pending = el.ask({ note: 'ok', number: 0, quote: '', context: '' });
    await el.updateComplete;
    expect(el.style.getPropertyValue('--note-dialog-vv-height')).toBe('');

    const slDialog = el.shadowRoot!.querySelector('sl-dialog')!;
    slDialog.dispatchEvent(new CustomEvent('sl-request-close'));
    await expect(pending).resolves.toBeNull();
  });
});
