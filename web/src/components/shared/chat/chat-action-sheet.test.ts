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

import { afterEach, describe, expect, it, vi } from 'vitest';

import './chat-action-sheet.js';
import type { ActionSheetItem, ScionActionSheet } from './chat-action-sheet.js';

const ITEMS: ActionSheetItem[] = [
  { id: 'reply', label: 'Reply', icon: 'reply' },
  { id: 'move-up', label: 'Move up', disabled: true },
  { id: 'delete', label: 'Delete', icon: 'trash', destructive: true },
];

async function mount(): Promise<ScionActionSheet> {
  const el = document.createElement('scion-action-sheet');
  el.items = ITEMS;
  el.heading = '#general';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function openSheet(el: ScionActionSheet): Promise<HTMLDialogElement> {
  el.show();
  await el.updateComplete;
  const dialog = el.shadowRoot!.querySelector('dialog')!;
  expect(dialog.open).toBe(true);
  return dialog;
}

const TOUCH_TYPES = ['touchstart', 'touchmove', 'touchend', 'touchcancel'] as const;

/** The prototype that actually defines addEventListener for `node`. */
function listenerOwner(node: EventTarget): EventTarget {
  let proto: object | null = node;
  while (proto && !Object.prototype.hasOwnProperty.call(proto, 'addEventListener')) {
    proto = Object.getPrototypeOf(proto) as object | null;
  }
  return proto as EventTarget;
}

function item(el: ScionActionSheet, id: string): HTMLButtonElement {
  return el.shadowRoot!.querySelector<HTMLButtonElement>(`.item[data-id="${id}"]`)!;
}

describe('scion-action-sheet', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders one row per item, the heading, and a Cancel row', async () => {
    const el = await mount();
    const rows = [...el.shadowRoot!.querySelectorAll('.item')].map((r) => r.textContent?.trim());
    expect(rows).toEqual(['Reply', 'Move up', 'Delete']);
    expect(el.shadowRoot!.querySelector('.heading')?.textContent).toBe('#general');
    expect(el.shadowRoot!.querySelector('dialog')?.getAttribute('aria-label')).toBe('#general');
    expect(el.shadowRoot!.querySelector('.cancel')?.textContent?.trim()).toBe('Cancel');
  });

  it('marks destructive and disabled rows', async () => {
    const el = await mount();
    expect(item(el, 'delete').classList.contains('destructive')).toBe(true);
    expect(item(el, 'move-up').disabled).toBe(true);
    expect(item(el, 'reply').disabled).toBe(false);
  });

  it('emits action-sheet-select with the id, then closes', async () => {
    const el = await mount();
    const dialog = await openSheet(el);
    const events: string[] = [];
    el.addEventListener('action-sheet-select', (e) =>
      events.push(`select:${(e as CustomEvent<{ id: string }>).detail.id}`)
    );
    el.addEventListener('action-sheet-close', () => events.push('close'));

    item(el, 'delete').click();
    await el.updateComplete;

    expect(events).toEqual(['select:delete', 'close']);
    expect(el.open).toBe(false);
    expect(dialog.open).toBe(false);
  });

  it('does not emit select for a disabled row', async () => {
    const el = await mount();
    await openSheet(el);
    const onSelect = vi.fn();
    el.addEventListener('action-sheet-select', onSelect);
    item(el, 'move-up').click();
    await el.updateComplete;
    expect(onSelect).not.toHaveBeenCalled();
    expect(el.open).toBe(true);
  });

  it('closes on Cancel without selecting', async () => {
    const el = await mount();
    await openSheet(el);
    const onSelect = vi.fn();
    const onClose = vi.fn();
    el.addEventListener('action-sheet-select', onSelect);
    el.addEventListener('action-sheet-close', onClose);
    el.shadowRoot!.querySelector<HTMLButtonElement>('.cancel')!.click();
    await el.updateComplete;
    expect(onSelect).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledOnce();
    expect(el.open).toBe(false);
  });

  it('closes on a backdrop click but not on a click inside the sheet', async () => {
    const el = await mount();
    const dialog = await openSheet(el);
    vi.spyOn(dialog, 'getBoundingClientRect').mockReturnValue(
      new DOMRect(0, 400, 375, 300) as DOMRect
    );
    const onClose = vi.fn();
    el.addEventListener('action-sheet-close', onClose);

    dialog.dispatchEvent(new MouseEvent('click', { bubbles: true, clientX: 100, clientY: 500 }));
    await el.updateComplete;
    expect(onClose).not.toHaveBeenCalled();

    dialog.dispatchEvent(new MouseEvent('click', { bubbles: true, clientX: 100, clientY: 100 }));
    await el.updateComplete;
    expect(onClose).toHaveBeenCalledOnce();
    expect(el.open).toBe(false);
  });

  it('reports a native close (Esc) as action-sheet-close and resets open', async () => {
    const el = await mount();
    const dialog = await openSheet(el);
    const onClose = vi.fn();
    el.addEventListener('action-sheet-close', onClose);
    dialog.close();
    await el.updateComplete;
    expect(onClose).toHaveBeenCalledOnce();
    expect(el.open).toBe(false);
  });

  it('keeps clicks and touches inside the sheet from reaching the page', async () => {
    const el = await mount();
    await openSheet(el);
    const onDocClick = vi.fn();
    const onDocTouch = vi.fn();
    document.addEventListener('click', onDocClick);
    for (const type of TOUCH_TYPES) document.addEventListener(type, onDocTouch);
    try {
      el.shadowRoot!.querySelector('.heading')!.dispatchEvent(
        new MouseEvent('click', { bubbles: true, composed: true })
      );
      for (const type of TOUCH_TYPES) {
        item(el, 'reply').dispatchEvent(new Event(type, { bubbles: true, composed: true }));
      }
      expect(onDocClick).not.toHaveBeenCalled();
      expect(onDocTouch).not.toHaveBeenCalled();
    } finally {
      document.removeEventListener('click', onDocClick);
      for (const type of TOUCH_TYPES) document.removeEventListener(type, onDocTouch);
    }
  });

  it('registers its touch listeners as passive, since none of them calls preventDefault', async () => {
    // Lit passes a listener object to addEventListener as the options
    // argument too, so `passive` on the object must reach the registration.
    const add = vi.spyOn(listenerOwner(document.createElement('dialog')), 'addEventListener');
    try {
      const el = await mount();
      const dialog = el.shadowRoot!.querySelector('dialog')!;
      for (const type of TOUCH_TYPES) {
        const call = add.mock.calls.find(
          (args, i) => args[0] === type && add.mock.contexts[i] === dialog
        );
        expect(call, type).toBeDefined();
        expect(call![2], type).toMatchObject({ passive: true });
      }
    } finally {
      add.mockRestore();
    }
  });

  it('closes when the host sets open = false', async () => {
    const el = await mount();
    const dialog = await openSheet(el);
    const onClose = vi.fn();
    el.addEventListener('action-sheet-close', onClose);
    el.open = false;
    await el.updateComplete;
    expect(dialog.open).toBe(false);
    expect(onClose).toHaveBeenCalledOnce();
  });
});
