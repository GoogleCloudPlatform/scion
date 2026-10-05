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
 * The send button's menu on touch: a long-press on Send opens a one-item
 * action sheet offering "Send with interruption". The press never also
 * sends, choosing the item sends exactly once with the interrupt flag after
 * the sheet has closed, Cancel sends nothing and keeps the draft, and a
 * mouse right-click still opens the popup.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
import { LONG_PRESS_MS } from './long-press.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

beforeAll(async () => {
  await import('./chat-composer.js');
});

function pointer(type: string, pointerType = 'touch', pointerId = 1): PointerEvent {
  return new PointerEvent(type, {
    bubbles: true,
    composed: true,
    cancelable: true,
    pointerType,
    pointerId,
    isPrimary: true,
    clientX: 10,
    clientY: 10,
  });
}

function contextmenu(): MouseEvent {
  return new MouseEvent('contextmenu', { bubbles: true, composed: true, cancelable: true });
}

describe('composer — send menu on touch', () => {
  let el: any;
  let sends: { interrupt: boolean; text: string }[];

  const sendBtn = (): HTMLElement => el.shadowRoot.querySelector('.send-btn');
  const sheet = (): any => el.shadowRoot.querySelector('scion-action-sheet');
  const dialog = (): HTMLDialogElement => sheet().shadowRoot.querySelector('dialog');

  async function settle(): Promise<void> {
    await el.updateComplete;
    await sheet().updateComplete;
  }

  /** A touch long-press on Send, lifted, with the click the browser sends after it. */
  async function longPressSend(): Promise<void> {
    sendBtn().dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    window.dispatchEvent(pointer('pointerup'));
    sendBtn().dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle();
  }

  beforeEach(async () => {
    vi.useFakeTimers();
    el = document.createElement('scion-chat-composer');
    el.conversationMode = true;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await el.updateComplete;
    el.text = 'hold to interrupt';
    el.runeCount = el.text.length;
    await el.updateComplete;
    sends = [];
    el.addEventListener('chat-send', (e: CustomEvent) =>
      sends.push({ interrupt: e.detail.interrupt, text: e.detail.text })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  it('opens the sheet on a long-press, and the press does not send', async () => {
    await longPressSend();
    expect(sheet().open).toBe(true);
    expect(dialog().open).toBe(true);
    const rows = [...sheet().shadowRoot.querySelectorAll('.item')].map((r: Element) =>
      r.textContent?.trim()
    );
    expect(rows).toEqual(['Send with interruption']);
    // Named for screen readers, like the other chat sheets.
    expect(dialog().getAttribute('aria-label')).toBe('Send options');
    expect(sends).toEqual([]);
    expect(el.text).toBe('hold to interrupt');
  });

  it('sends exactly once, with interrupt, after the sheet closes', async () => {
    await longPressSend();
    // The send must come after the dialog has closed (and handed focus back),
    // so the send's own touch blur is the last word on focus.
    const dialogOpenAtSend: boolean[] = [];
    el.addEventListener('chat-send', () => dialogOpenAtSend.push(dialog().open));

    sheet().shadowRoot.querySelector('.item[data-id="send-interrupt"]').click();
    await settle();

    expect(sends).toEqual([{ interrupt: true, text: 'hold to interrupt' }]);
    expect(dialogOpenAtSend).toEqual([false]);
    expect(sheet().open).toBe(false);
    expect(el.text).toBe('');
  });

  it('Cancel sends nothing and keeps the draft', async () => {
    await longPressSend();
    sheet().shadowRoot.querySelector('.cancel').click();
    await settle();

    expect(sheet().open).toBe(false);
    expect(sends).toEqual([]);
    expect(el.text).toBe('hold to interrupt');
  });

  it('a short tap still sends normally', async () => {
    sendBtn().dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(100);
    window.dispatchEvent(pointer('pointerup'));
    sendBtn().dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await settle();

    expect(sheet().open).toBe(false);
    expect(sends).toEqual([{ interrupt: false, text: 'hold to interrupt' }]);
  });

  it("the browser's own touch long-press opens the sheet, not the popup", async () => {
    sendBtn().dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(300);
    sendBtn().dispatchEvent(contextmenu());
    vi.advanceTimersByTime(LONG_PRESS_MS);
    await settle();

    expect(sheet().open).toBe(true);
    expect(el.shadowRoot.querySelector('.send-context-menu')).toBeNull();
    expect(sends).toEqual([]);
  });

  it('a mouse right-click still opens the popup, not the sheet', async () => {
    sendBtn().dispatchEvent(pointer('pointerdown', 'mouse', 5));
    sendBtn().dispatchEvent(contextmenu());
    await settle();

    expect(el.shadowRoot.querySelector('.send-context-menu')).not.toBeNull();
    expect(sheet().open).toBe(false);
  });

  it('offers no sheet with nothing to send, or while editing', async () => {
    el.text = '   ';
    el.runeCount = 3;
    await el.updateComplete;
    await longPressSend();
    expect(sheet().open).toBe(false);

    el.text = 'edited';
    el.runeCount = 6;
    el.editMessage = { messageId: 'm1', content: 'edited' };
    await el.updateComplete;
    sendBtn().dispatchEvent(pointer('pointerdown', 'touch', 2));
    vi.advanceTimersByTime(LONG_PRESS_MS);
    await settle();
    expect(sheet().open).toBe(false);
  });
});
