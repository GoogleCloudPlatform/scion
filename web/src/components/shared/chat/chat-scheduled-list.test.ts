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
 * The scheduled-message list at the bottom of a thread: loads the user's own
 * scheduled messages for the conversation, shows the send time in the display
 * zone with Cancel, follows the user-scoped SSE events (sent removes the
 * bubble, failed shows the reason), ignores other conversations, and shows
 * nothing while disabled.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { ScheduledMessage } from '../../../client/chat-scheduled.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const listScheduledMessages = vi.fn();
const cancelScheduledMessage = vi.fn();
const sendNowScheduledMessage = vi.fn();
const dismissScheduledMessage = vi.fn();
vi.mock('../../../client/chat-scheduled.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../client/chat-scheduled.js')>();
  return {
    ...actual,
    listScheduledMessages: (...a: unknown[]) => listScheduledMessages(...a),
    cancelScheduledMessage: (...a: unknown[]) => cancelScheduledMessage(...a),
    sendNowScheduledMessage: (...a: unknown[]) => sendNowScheduledMessage(...a),
    dismissScheduledMessage: (...a: unknown[]) => dismissScheduledMessage(...a),
  };
});
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

let stateManager: EventTarget;
let setPreferredTimeZone: (zone: string | null) => void;

beforeAll(async () => {
  ({ stateManager } = await import('../../../client/state.js'));
  ({ setPreferredTimeZone } = await import('../../../utils/time.js'));
  await import('./chat-scheduled-list.js');
});

function sm(id: string, over: Partial<ScheduledMessage> = {}): ScheduledMessage {
  return {
    id,
    conversationKey: 't1',
    content: `content ${id}`,
    fireAt: '2099-10-08T07:00:00Z',
    status: 'pending',
    createdAt: '2026-10-07T10:00:00Z',
    updatedAt: '2026-10-07T10:00:00Z',
    ...over,
  };
}

function emit(m: ScheduledMessage, action = 'created'): void {
  stateManager.dispatchEvent(
    new CustomEvent('chat-scheduled-updated', {
      detail: { state: {}, data: { action, scheduledMessage: m } },
    })
  );
}

async function flush(el: any): Promise<void> {
  for (let i = 0; i < 3; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

describe('scion-chat-scheduled-list', () => {
  let el: any;
  const items = (): HTMLElement[] => [...el.shadowRoot.querySelectorAll('.item')];

  beforeEach(async () => {
    setPreferredTimeZone('Europe/Berlin');
    listScheduledMessages.mockReset();
    cancelScheduledMessage.mockReset();
    sendNowScheduledMessage.mockReset();
    dismissScheduledMessage.mockReset();
    listScheduledMessages.mockResolvedValue([sm('b', { fireAt: '2099-10-09T07:00:00Z' }), sm('a')]);
    el = document.createElement('scion-chat-scheduled-list');
    el.conversationKey = 't1';
    el.enabled = true;
    document.body.appendChild(el);
    await flush(el);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone(null);
  });

  it('loads and shows the messages ordered by fire time, with the time in the display zone', () => {
    expect(listScheduledMessages).toHaveBeenCalledWith('t1');
    expect(items().map((i) => i.dataset.id)).toEqual(['a', 'b']);
    const banner = items()[0]!.querySelector('.banner')!.textContent!;
    expect(banner).toContain('Scheduled for');
    expect(banner).toContain('09:00'); // 07:00Z in Berlin (CEST)
    expect(banner).toContain('Europe/Berlin');
    expect(items()[0]!.querySelector('.cancel-btn')).not.toBeNull();
  });

  it('follows SSE: sent removes, failed shows the reason, other conversations are ignored', async () => {
    emit(sm('c', { conversationKey: 'other' }));
    await flush(el);
    expect(items().map((i) => i.dataset.id)).toEqual(['a', 'b']);

    emit(sm('a', { status: 'sent', messageId: 'm1' }), 'sent');
    await flush(el);
    expect(items().map((i) => i.dataset.id)).toEqual(['b']);

    emit(
      sm('b', { fireAt: '2099-10-09T07:00:00Z', status: 'failed', failureReason: 'no_access' }),
      'failed'
    );
    await flush(el);
    expect(items()[0]!.dataset.status).toBe('failed');
    expect(items()[0]!.querySelector('.banner.failed')!.textContent).toContain(
      'no longer have access'
    );
    expect(items()[0]!.querySelector('.cancel-btn')).toBeNull();
  });

  it('a released message is pending again, with Cancel', async () => {
    emit(sm('a', { status: 'sending' }), 'sending');
    await flush(el);
    expect(items()[0]!.querySelector('.cancel-btn')).toBeNull();
    emit(sm('a', { status: 'pending' }), 'released');
    await flush(el);
    expect(items()[0]!.dataset.status).toBe('pending');
    expect(items()[0]!.querySelector('.cancel-btn')).not.toBeNull();
  });

  it('cancels, removes the bubble and offers the text back to the composer', async () => {
    const restores: { text: string; onlyIfEmpty: boolean }[] = [];
    el.addEventListener('chat-scheduled-restore', (e: CustomEvent) => restores.push(e.detail));
    cancelScheduledMessage.mockResolvedValue(undefined);
    (items()[0]!.querySelector('.cancel-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(cancelScheduledMessage).toHaveBeenCalledWith('t1', 'a');
    expect(items().map((i) => i.dataset.id)).toEqual(['b']);
    expect(restores).toEqual([{ text: 'content a', onlyIfEmpty: true }]);
  });

  it('a failed cancel offers nothing to the composer', async () => {
    const restores: unknown[] = [];
    el.addEventListener('chat-scheduled-restore', (e: CustomEvent) => restores.push(e.detail));
    cancelScheduledMessage.mockRejectedValue(new Error('This message is already being sent'));
    (items()[0]!.querySelector('.cancel-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(restores).toEqual([]);
  });

  it('a missed message offers Send now, Copy to composer and Dismiss', async () => {
    emit(sm('a', { status: 'failed', failureReason: 'missed' }), 'failed');
    await flush(el);
    const row = items()[0]!;
    expect(row.querySelector('.banner.failed')!.textContent).toContain(
      'more than an hour after its time'
    );
    expect(row.querySelector('.send-now-btn')).not.toBeNull();
    expect(row.querySelector('.copy-btn')).not.toBeNull();
    expect(row.querySelector('.dismiss-btn')).not.toBeNull();

    sendNowScheduledMessage.mockResolvedValue(
      sm('a', { status: 'pending', fireAt: '2026-10-08T07:00:00Z' })
    );
    (row.querySelector('.send-now-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(sendNowScheduledMessage).toHaveBeenCalledWith('t1', 'a');
    expect(items()[0]!.dataset.id).toBe('a');
    expect(items()[0]!.dataset.status).toBe('pending');
    expect(items()[0]!.querySelector('.cancel-btn')).not.toBeNull();
  });

  it('an interrupted message offers Send now; other failures do not', async () => {
    emit(sm('a', { status: 'failed', failureReason: 'interrupted' }), 'failed');
    emit(
      sm('b', { fireAt: '2099-10-09T07:00:00Z', status: 'failed', failureReason: 'no_access' }),
      'failed'
    );
    await flush(el);
    expect(items()[0]!.querySelector('.send-now-btn')).not.toBeNull();
    expect(items()[1]!.querySelector('.send-now-btn')).toBeNull();
    expect(items()[1]!.querySelector('.dismiss-btn')).not.toBeNull();
  });

  it('Copy to composer offers the text without the empty-composer condition', async () => {
    const restores: { text: string; onlyIfEmpty: boolean }[] = [];
    el.addEventListener('chat-scheduled-restore', (e: CustomEvent) => restores.push(e.detail));
    emit(sm('a', { status: 'failed', failureReason: 'no_access' }), 'failed');
    await flush(el);
    (items()[0]!.querySelector('.copy-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(restores).toEqual([{ text: 'content a', onlyIfEmpty: false }]);
    expect(items()[0]!.dataset.status).toBe('failed');
  });

  it('Dismiss removes a failed message; a failure keeps it and reloads', async () => {
    emit(sm('a', { status: 'failed', failureReason: 'no_access' }), 'failed');
    await flush(el);
    dismissScheduledMessage.mockRejectedValueOnce(new Error('nope'));
    listScheduledMessages.mockResolvedValue([
      sm('a', { status: 'failed', failureReason: 'no_access' }),
      sm('b', { fireAt: '2099-10-09T07:00:00Z' }),
    ]);
    (items()[0]!.querySelector('.dismiss-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(items().map((i) => i.dataset.id)).toEqual(['a', 'b']);

    dismissScheduledMessage.mockResolvedValue(undefined);
    (items()[0]!.querySelector('.dismiss-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(dismissScheduledMessage).toHaveBeenCalledWith('t1', 'a');
    expect(items().map((i) => i.dataset.id)).toEqual(['b']);
  });

  it('a dismissed event from another tab removes the message', async () => {
    emit(sm('a', { status: 'cancelled', failureReason: 'missed' }), 'dismissed');
    await flush(el);
    expect(items().map((i) => i.dataset.id)).toEqual(['b']);
  });

  it('keeps the bubble and reloads when cancel loses to delivery', async () => {
    cancelScheduledMessage.mockRejectedValue(new Error('This message is already being sent'));
    listScheduledMessages.mockResolvedValue([sm('a', { status: 'sending' }), sm('b')]);
    (items()[0]!.querySelector('.cancel-btn') as HTMLButtonElement).click();
    await flush(el);
    expect(items()[0]!.dataset.status).toBe('sending');
  });

  it('a load that started before an SSE update does not overwrite it', async () => {
    let resolveList: (v: ScheduledMessage[]) => void = () => {};
    listScheduledMessages.mockReturnValue(
      new Promise<ScheduledMessage[]>((r) => {
        resolveList = r;
      })
    );
    // Reconnect starts a reload; while it is in flight, 'a' is sent and 'c' is created.
    stateManager.dispatchEvent(new CustomEvent('connected'));
    emit(sm('a', { status: 'sent', messageId: 'm1' }), 'sent');
    emit(sm('c', { fireAt: '2099-10-10T07:00:00Z' }));
    await flush(el);
    // The stale GET result still lists 'a' as pending and lacks 'c'.
    resolveList([sm('a'), sm('b', { fireAt: '2099-10-09T07:00:00Z' })]);
    await flush(el);
    expect(items().map((i) => i.dataset.id)).toEqual(['b', 'c']);
  });

  it('shows nothing and ignores events while disabled', async () => {
    el.enabled = false;
    await flush(el);
    expect(items()).toEqual([]);
    emit(sm('z'));
    await flush(el);
    expect(items()).toEqual([]);
  });
});
