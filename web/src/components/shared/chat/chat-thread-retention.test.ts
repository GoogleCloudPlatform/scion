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
 * Chat retention in the thread (hub.chat_retention): the oldest history
 * page's `retention.purgedBefore` renders a divider at the top of the
 * thread, and a reply whose quoted message is gone renders a muted
 * "[no longer available]" quote.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { CHAT_RETENTION_FLAG, setFeatureFlag } from '../../../utils/feature-flags.js';
import { setPreferredTimeZone } from '../../../utils/time.js';

const stateManager = vi.hoisted(() =>
  Object.assign(new EventTarget(), {
    currentScope: null,
    isConnected: false,
    getAgent: (): undefined => undefined,
    seedAgents: (): void => {},
  })
);

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: unknown) => Promise<Response>>());

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function message(id: string, createdAt: string, msg = id) {
  return {
    id,
    projectId: '',
    sender: 'user:alice',
    senderId: 'u-alice',
    recipient: '',
    recipientId: '',
    msg,
    type: 'chat',
    agentId: '',
    createdAt,
  };
}

/** The history body each test serves for the conversation's messages. */
let history: Record<string, unknown>;

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function open(key: string): Promise<ScionChatThread> {
  const thread = document.createElement('scion-chat-thread');
  thread.conversationKey = key;
  thread.projectId = 'proj-1';
  document.body.appendChild(thread);
  await thread.updateComplete;
  await vi.waitFor(() =>
    expect(apiFetch.mock.calls.some(([path]) => path.includes('/messages?'))).toBe(true)
  );
  await settle();
  await thread.updateComplete;
  return thread;
}

function divider(el: ScionChatThread): Element | null {
  return el.shadowRoot?.querySelector('.retention-divider') ?? null;
}

const RETENTION = { purgedBefore: '2026-07-31T00:00:00Z' };
const DIVIDER_TEXT = "Messages before Jul 31, 2026 are removed by this hub's retention policy.";

beforeEach(() => {
  setPreferredTimeZone('UTC');
  setFeatureFlag(CHAT_RETENTION_FLAG, true);
  history = { messages: [] };
  apiFetch.mockReset();
  apiFetch.mockImplementation((path) => {
    if (path.includes('/messages?')) return Promise.resolve(json(history));
    return Promise.resolve(json({ items: [] }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
  delete window.__SCION_FEATURES__;
  setPreferredTimeZone('');
});

describe('retention divider', () => {
  it('renders at the top of the thread when the oldest page carries the marker', async () => {
    history = {
      messages: [message('m2', '2026-08-02T10:00:00Z'), message('m1', '2026-08-01T10:00:00Z')],
      retention: RETENTION,
    };
    const el = await open('topic-retained');

    const row = divider(el);
    expect(row?.textContent?.trim()).toBe(DIVIDER_TEXT);
    // It sits above every message row.
    const list = el.shadowRoot?.querySelector('.messages-list');
    const first = list?.querySelector('.retention-divider, .date-divider, scion-chat-message');
    expect(first).toBe(row);
  });

  it('renders on an empty thread whose history was all removed', async () => {
    history = { messages: [], retention: RETENTION };
    const el = await open('topic-emptied');

    expect(divider(el)?.textContent?.trim()).toBe(DIVIDER_TEXT);
  });

  it('is not rendered when the page carries no marker', async () => {
    history = { messages: [message('m1', '2026-08-01T10:00:00Z')] };
    const el = await open('topic-plain');

    expect(divider(el)).toBeNull();
  });

  it('is not rendered while older pages remain', async () => {
    history = {
      messages: [message('m1', '2026-08-01T10:00:00Z')],
      nextCursor: 'older',
      retention: RETENTION,
    };
    const el = await open('topic-more');

    expect(divider(el)).toBeNull();
  });

  it('is not rendered with the hub.chat_retention web flag off', async () => {
    setFeatureFlag(CHAT_RETENTION_FLAG, false);
    history = { messages: [message('m1', '2026-08-01T10:00:00Z')], retention: RETENTION };
    const el = await open('topic-flag-off');

    expect(divider(el)).toBeNull();
  });

  it('is cleared when the thread switches to a conversation without the marker', async () => {
    history = { messages: [message('m1', '2026-08-01T10:00:00Z')], retention: RETENTION };
    const el = await open('topic-a');
    expect(divider(el)).not.toBeNull();

    history = { messages: [message('m9', '2026-08-03T10:00:00Z')] };
    el.conversationKey = 'topic-b';
    await el.updateComplete;
    await settle();
    await el.updateComplete;

    expect(divider(el)).toBeNull();
  });
});

describe('unavailable reply quote', () => {
  it('hands the unavailable preview to the message, which renders it muted', async () => {
    history = {
      messages: [message('reply-1', '2026-08-01T10:00:00Z', 'answering')],
      messageExtensions: { 'reply-1': { messageId: 'reply-1', replyToId: 'gone-1' } },
      replyPreviews: {
        'gone-1': {
          messageId: 'gone-1',
          senderName: '',
          content: '[no longer available]',
          unavailable: true,
        },
      },
    };
    const el = await open('topic-reply');

    const msg = el.shadowRoot?.querySelector('scion-chat-message');
    expect(msg?.replyPreview).toEqual({
      messageId: 'gone-1',
      senderName: '',
      content: '[no longer available]',
      unavailable: true,
    });
    await msg?.updateComplete;
    const quote = msg?.shadowRoot?.querySelector('.reply-preview.unavailable');
    expect(quote?.textContent?.trim()).toBe('[no longer available]');
  });
});
