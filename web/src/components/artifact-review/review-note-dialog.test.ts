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
