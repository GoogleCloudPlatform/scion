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
 * "Schedule send…" in the send button's right-click menu (ptone/scion#3666):
 * offered only when the parent enables it, disabled while attachments are
 * staged, and confirming the dialog clears the composer and dispatches
 * `chat-schedule` with the chosen instant instead of sending.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

beforeAll(async () => {
  await import('./chat-composer.js');
});

function contextmenu(): MouseEvent {
  return new MouseEvent('contextmenu', { bubbles: true, composed: true, cancelable: true });
}

describe('composer — schedule send', () => {
  let el: any;
  let sends: string[];
  let schedules: { text: string; fireAt: string; replyToId?: string }[];

  const sendBtn = (): HTMLElement => el.shadowRoot.querySelector('.send-btn');
  const menu = (): HTMLElement | null => el.shadowRoot.querySelector('.send-context-menu');
  const scheduleItem = (): HTMLElement | null =>
    el.shadowRoot.querySelector('.send-context-item.schedule-send-item');
  const dialog = (): any => el.shadowRoot.querySelector('scion-chat-schedule-dialog');

  async function openMenu(): Promise<void> {
    sendBtn().dispatchEvent(contextmenu());
    await el.updateComplete;
  }

  beforeEach(async () => {
    el = document.createElement('scion-chat-composer');
    el.conversationMode = true;
    el.projectId = 'proj-1';
    el.conversationKey = 'topic-1';
    document.body.appendChild(el);
    await el.updateComplete;
    el.text = 'send this tomorrow';
    el.runeCount = el.text.length;
    await el.updateComplete;
    sends = [];
    schedules = [];
    el.addEventListener('chat-send', (e: CustomEvent) => sends.push(e.detail.text));
    el.addEventListener('chat-schedule', (e: CustomEvent) =>
      schedules.push({
        text: e.detail.text,
        fireAt: e.detail.fireAt,
        replyToId: e.detail.replyToId,
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('is not offered unless enabled by the parent', async () => {
    await openMenu();
    expect(menu()).not.toBeNull();
    expect(scheduleItem()).toBeNull();
    expect(dialog()).toBeNull();
  });

  it('opens the dialog from the menu and schedules on confirm', async () => {
    el.scheduleSendEnabled = true;
    await el.updateComplete;
    await openMenu();
    expect(scheduleItem()?.textContent).toContain('Schedule send');

    scheduleItem()!.click();
    await el.updateComplete;
    expect(menu()).toBeNull();
    expect(dialog().open).toBe(true);

    const fireAt = new Date(Date.now() + 3_600_000).toISOString();
    dialog().dispatchEvent(
      new CustomEvent('schedule-confirm', { detail: { fireAt }, bubbles: true, composed: true })
    );
    await el.updateComplete;

    expect(schedules).toEqual([{ text: 'send this tomorrow', fireAt, replyToId: undefined }]);
    expect(sends).toEqual([]);
    expect(el.text).toBe('');
    expect(dialog().open).toBe(false);
  });

  it('carries the reply-to message and restores the draft on error', async () => {
    el.scheduleSendEnabled = true;
    el.replyTo = { messageId: 'm-1', senderName: 'Ada', content: 'hi' };
    await el.updateComplete;
    let onError: ((msg: string) => void) | undefined;
    el.addEventListener('chat-schedule', (e: CustomEvent) => (onError = e.detail.onError));

    await openMenu();
    scheduleItem()!.click();
    await el.updateComplete;
    dialog().dispatchEvent(
      new CustomEvent('schedule-confirm', {
        detail: { fireAt: new Date(Date.now() + 3_600_000).toISOString() },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(schedules[0]?.replyToId).toBe('m-1');
    expect(el.text).toBe('');

    onError?.('nope');
    await el.updateComplete;
    expect(el.text).toBe('send this tomorrow');
  });

  it('cancelling the dialog keeps the draft and sends nothing', async () => {
    el.scheduleSendEnabled = true;
    await el.updateComplete;
    await openMenu();
    scheduleItem()!.click();
    await el.updateComplete;
    dialog().dispatchEvent(new CustomEvent('schedule-cancel', { bubbles: true, composed: true }));
    await el.updateComplete;

    expect(dialog().open).toBe(false);
    expect(schedules).toEqual([]);
    expect(sends).toEqual([]);
    expect(el.text).toBe('send this tomorrow');
  });

  it('is disabled while attachments are staged', async () => {
    el.scheduleSendEnabled = true;
    el.pendingFiles = [{ id: 'a1', name: 'f.txt', mime: 'text/plain', size: 1, url: '' }];
    await el.updateComplete;
    await openMenu();
    expect(scheduleItem()?.getAttribute('aria-disabled')).toBe('true');

    scheduleItem()!.click();
    await el.updateComplete;
    expect(dialog().open).toBe(false);
  });
});
