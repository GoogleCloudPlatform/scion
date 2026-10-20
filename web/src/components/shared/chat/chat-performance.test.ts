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

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/main.js', () => import('../../../client/__fixtures__/main-stub.js'));
vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}'))),
  extractApiError: vi.fn(),
}));

await import('./chat-thread.js');
const { stateManager } = await import('../../../client/__fixtures__/main-stub.js');
const { setPreferredTimeZone } = await import('../../../utils/time.js');
const { formatChatDate } = await import('./chat-date-divider.js');

afterEach(() => {
  document.body.replaceChildren();
  localStorage.clear();
  setPreferredTimeZone('');
  vi.restoreAllMocks();
});

async function mountThread(): Promise<any> {
  const thread = document.createElement('scion-chat-thread') as any;
  thread.loadHistory = vi.fn();
  thread.conversationKey = 'performance-thread';
  thread.currentUserId = 'me';
  thread.mergeMessages([
    {
      id: 'm1',
      sender: 'user:me',
      senderId: 'me',
      msg: 'hello',
      createdAt: '2026-01-01T12:00:00Z',
      type: 'chat',
      plain: true,
      dispatchState: 'dispatched',
    },
  ]);
  document.body.append(thread);
  await thread.updateComplete;
  await thread.shadowRoot.querySelector('scion-chat-message').updateComplete;
  return thread;
}

describe('chat transcript update isolation', () => {
  it('updates typing and scroll controls without rebuilding message rows', async () => {
    const thread = await mountThread();
    const rows = vi.spyOn(thread, 'renderMessages');
    const bubble = thread.shadowRoot.querySelector('scion-chat-message');
    const renderBubble = vi.spyOn(bubble, 'render');

    thread.typingUsers = new Map([['peer', { displayName: 'Peer', timer: 0 }]]);
    thread.agents = [{ id: 'agent-new', name: 'New agent' }];
    thread.pinnedToBottom = false;
    await thread.updateComplete;
    expect(thread.shadowRoot.querySelector('.typing-text').textContent).toContain('Peer');
    expect(thread.shadowRoot.querySelector('.jump-to-latest')).not.toBeNull();
    expect(thread.shadowRoot.querySelector('scion-chat-composer').agents).toBe(thread.agents);
    expect(rows).not.toHaveBeenCalled();
    expect(renderBubble).not.toHaveBeenCalled();
  });

  it('keeps empty attachment props stable across unrelated full thread updates', async () => {
    const thread = await mountThread();
    const bubble = thread.shadowRoot.querySelector('scion-chat-message');
    const renderBubble = vi.spyOn(bubble, 'render');
    thread.sendError = 'Temporary error';
    await thread.updateComplete;
    await bubble.updateComplete;
    expect(thread.shadowRoot.querySelector('.send-error').textContent).toContain('Temporary');
    expect(renderBubble).not.toHaveBeenCalled();
  });

  it('invalidates rows for edits, attachment metadata, member names, and deletions', async () => {
    const thread = await mountThread();
    const getBubble = () => thread.shadowRoot.querySelector('scion-chat-message');
    const attachment = { id: 'a1', name: 'notes.txt', mime: 'text/plain', size: 10 };
    thread.v2AttachmentMap.set('m1', [attachment]);
    thread.handleV2MessageEdited(
      new CustomEvent('chat-message-edited', {
        detail: {
          conversationKey: thread.conversationKey,
          messageId: 'm1',
          content: 'edited',
          editedAt: '2026-01-01T12:01:00Z',
        },
      })
    );
    await thread.updateComplete;
    expect(getBubble().body).toBe('edited');
    expect(getBubble().editedAt).toBe('2026-01-01T12:01:00Z');
    expect(getBubble().attachmentRefs).toEqual([attachment]);

    thread.members = [{ id: 'me', kind: 'user', name: 'Updated name', email: '' }];
    await thread.updateComplete;
    expect(getBubble().senderName).toBe('Updated name');

    thread.handleV2MessageDeleted(
      new CustomEvent('chat-message-deleted', {
        detail: {
          conversationKey: thread.conversationKey,
          messageId: 'm1',
          deletedAt: '2026-01-01T12:02:00Z',
        },
      })
    );
    await thread.updateComplete;
    expect(getBubble().deletedAt).toBe('2026-01-01T12:02:00Z');
  });

  it('refreshes receipt expiry on explicit updates without changed properties', async () => {
    const thread = await mountThread();
    thread.peerReadMessageId = 'm1';
    thread.peerReadAt = Date.now();
    await thread.updateComplete;
    const bubble = thread.shadowRoot.querySelector('scion-chat-message');
    expect(bubble.dispatchState).toBe('dispatched');

    vi.spyOn(Date, 'now').mockReturnValue(thread.peerReadAt + 5 * 60 * 1000 + 1);
    thread.requestUpdate();
    await thread.updateComplete;
    expect(bubble.dispatchState).toBe('');
  });

  it('refreshes receipt expiry even when the timer update batches with typing', async () => {
    const thread = await mountThread();
    thread.peerReadMessageId = 'm1';
    thread.peerReadAt = Date.now();
    await thread.updateComplete;
    const bubble = thread.shadowRoot.querySelector('scion-chat-message');
    expect(bubble.dispatchState).toBe('dispatched');

    vi.spyOn(Date, 'now').mockReturnValue(thread.peerReadAt + 5 * 60 * 1000 + 1);
    thread.typingUsers = new Map([['peer', { displayName: 'Peer', timer: 0 }]]);
    thread.requestUpdate();
    await thread.updateComplete;
    expect(bubble.dispatchState).toBe('');
  });
});

/** A chat message from `senderId`, with the fields the row model reads. */
function chatMessage(id: string, senderId: string, createdAt: string, msg = id): any {
  return {
    id,
    sender: `user:${senderId}`,
    senderId,
    msg,
    createdAt,
    type: 'chat',
    plain: true,
  };
}

/**
 * Webperf C9a: the row model (renderMessages) is rebuilt only when its
 * inputs change, and the per-message values it needs (time, date label,
 * sender) are derived once per message, never served stale.
 */
describe('chat thread row model memoization', () => {
  const dateLabels = (thread: any): string[] =>
    [...thread.shadowRoot.querySelectorAll('.date-label')].map((el: Element) =>
      (el.textContent ?? '').split(' · ')[0].trim()
    );
  const bubbles = (thread: any): any[] => [
    ...thread.shadowRoot.querySelectorAll('scion-chat-message'),
  ];

  it('typing event does not rebuild row model', async () => {
    const thread = await mountThread();
    thread.startStreamV2();
    const rows = vi.spyOn(thread, 'renderMessages');

    stateManager.dispatchEvent(
      new CustomEvent('chat-typing-received', {
        detail: {
          data: { threadId: thread.conversationKey, userId: 'peer', displayName: 'Peer' },
        },
      })
    );
    await thread.updateComplete;

    expect(thread.shadowRoot.querySelector('.typing-text').textContent).toContain('Peer');
    expect(rows).toHaveBeenCalledTimes(0);
  });

  it('a new message rebuilds the row model exactly once', async () => {
    const thread = await mountThread();
    const rows = vi.spyOn(thread, 'renderMessages');

    thread.mergeMessages([chatMessage('m2', 'peer', '2026-01-01T12:05:00Z', 'second')]);
    await thread.updateComplete;

    expect(rows).toHaveBeenCalledTimes(1);
    expect(bubbles(thread).map((b) => b.body)).toEqual(['hello', 'second']);
  });

  it('an edit rebuilds the row model exactly once', async () => {
    const thread = await mountThread();
    const rows = vi.spyOn(thread, 'renderMessages');

    thread.handleV2MessageEdited(
      new CustomEvent('chat-message-edited', {
        detail: {
          conversationKey: thread.conversationKey,
          messageId: 'm1',
          content: 'edited',
          editedAt: '2026-01-01T12:01:00Z',
        },
      })
    );
    await thread.updateComplete;

    expect(rows).toHaveBeenCalledTimes(1);
    expect(bubbles(thread)[0].body).toBe('edited');
  });

  it('a member change rebuilds the row model exactly once with the new name', async () => {
    const thread = await mountThread();
    thread.members = [{ id: 'me', kind: 'user', name: 'First name', email: '' }];
    await thread.updateComplete;
    expect(bubbles(thread)[0].senderName).toBe('First name');
    const rows = vi.spyOn(thread, 'renderMessages');

    thread.members = [{ id: 'me', kind: 'user', name: 'Renamed', email: '' }];
    await thread.updateComplete;

    expect(rows).toHaveBeenCalledTimes(1);
    expect(bubbles(thread)[0].senderName).toBe('Renamed');
  });

  it('does not rebuild the row model for context menu, preview, reply or edit state', async () => {
    const thread = await mountThread();
    const rows = vi.spyOn(thread, 'renderMessages');
    const msg = thread.messages[0];

    thread.contextMenuMessage = msg;
    thread.contextMenuPosition = { x: 10, y: 10 };
    thread.contextMenuAsSheet = false;
    await thread.updateComplete;
    thread.contextMenuMessage = null;
    thread.composerReplyTo = { messageId: msg.id, senderName: 'me', content: 'hello' };
    await thread.updateComplete;
    thread.composerReplyTo = null;
    thread.composerEditMessage = { messageId: msg.id, content: msg.msg };
    await thread.updateComplete;
    thread.composerEditMessage = null;
    thread.filePreview = { kind: 'gcs', messageId: msg.id, bucket: 'b', object: 'o', name: 'o' };
    await thread.updateComplete;
    thread.filePreview = null;
    await thread.updateComplete;

    expect(rows).toHaveBeenCalledTimes(0);
  });

  it('labels dates and names senders correctly after ingest', async () => {
    const thread = await mountThread();
    thread.members = [
      { id: 'me', kind: 'user', name: 'Me Myself', email: 'me@example.com' },
      { id: 'agent-1', kind: 'agent', name: 'Helper', email: '' },
    ];
    thread.mergeMessages([
      chatMessage('m2', 'peer', '2026-01-02T09:00:00Z'),
      {
        ...chatMessage('m3', 'agent-1', '2026-01-03T09:00:00Z'),
        sender: 'agent:helper',
        type: 'assistant-reply',
      },
    ]);
    await thread.updateComplete;

    expect(dateLabels(thread)).toEqual([
      formatChatDate('2026-01-01T12:00:00Z'),
      formatChatDate('2026-01-02T09:00:00Z'),
      formatChatDate('2026-01-03T09:00:00Z'),
    ]);
    const [own, peer, agent] = bubbles(thread);
    expect(own.senderName).toBe('Me Myself');
    // Not a member: the name falls back to the sender string.
    expect(peer.senderName).toBe('peer');
    expect(agent.senderName).toBe('Helper');
    expect(agent.senderIsAgent).toBe(true);
    expect(peer.senderIsAgent).toBe(false);
  });

  it('keeps the first matching member, by id or email, as a linear find would', async () => {
    const thread = await mountThread();
    thread.members = [
      { id: 'other', kind: 'agent', name: 'Email match', email: 'user:me' },
      { id: 'me', kind: 'user', name: 'Id match', email: '' },
    ];
    await thread.updateComplete;
    // m1 has senderId "me" and sender "user:me": the email match comes first.
    expect(bubbles(thread)[0].senderName).toBe('Email match');
  });

  it('re-derives a message replaced by a new object', async () => {
    const thread = await mountThread();
    thread.mergeMessages([
      { ...chatMessage('m1', 'peer', '2026-03-04T12:00:00Z', 'hello'), sender: 'user:peer' },
    ]);
    await thread.updateComplete;

    expect(dateLabels(thread)).toEqual([formatChatDate('2026-03-04T12:00:00Z')]);
    expect(bubbles(thread)[0].senderName).toBe('peer');
  });

  it('re-derives a message whose sender fields were mutated in place', async () => {
    const thread = await mountThread();
    thread.members = [
      { id: 'me', kind: 'user', name: 'Me', email: '' },
      { id: 'peer', kind: 'user', name: 'Peer', email: '' },
    ];
    await thread.updateComplete;
    expect(bubbles(thread)[0].senderName).toBe('Me');

    const msg = thread.messages[0];
    msg.senderId = 'peer';
    msg.sender = 'user:peer';
    thread.messages = [...thread.messages];
    await thread.updateComplete;

    expect(bubbles(thread)[0].senderName).toBe('Peer');
  });

  it('relabels dates when the display zone changes', async () => {
    setPreferredTimeZone('UTC');
    const thread = await mountThread();
    // 23:30 UTC on Jan 1 is already Jan 2 in Tokyo.
    thread.mergeMessages([chatMessage('m2', 'peer', '2026-01-01T23:30:00Z')]);
    await thread.updateComplete;
    expect(dateLabels(thread)).toEqual([formatChatDate('2026-01-01T12:00:00Z')]);

    setPreferredTimeZone('Asia/Tokyo');
    await thread.updateComplete;

    const labels = dateLabels(thread);
    expect(labels).toEqual([
      formatChatDate('2026-01-01T12:00:00Z'),
      formatChatDate('2026-01-01T23:30:00Z'),
    ]);
    expect(labels[0]).not.toBe(labels[1]);
  });
});

describe('composer input work', () => {
  it('does not render closed autocomplete menus while typing ordinary text', async () => {
    const composer = document.createElement('scion-chat-composer') as any;
    document.body.append(composer);
    await composer.updateComplete;
    const mention = composer.shadowRoot.querySelector('scion-mention-autocomplete');
    const slash = composer.shadowRoot.querySelector('scion-slash-autocomplete');
    await Promise.all([mention.updateComplete, slash.updateComplete]);
    const mentionRender = vi.spyOn(mention, 'render');
    const slashRender = vi.spyOn(slash, 'render');
    const textarea = document.createElement('textarea');
    vi.spyOn(composer, 'getTextareaElement').mockReturnValue(textarea);
    for (const value of ['h', 'he', 'hello']) {
      textarea.value = value;
      composer.handleInput({ target: { value } });
      await composer.updateComplete;
      await Promise.all([mention.updateComplete, slash.updateComplete]);
    }
    expect(mentionRender).not.toHaveBeenCalled();
    expect(slashRender).not.toHaveBeenCalled();
  });

  it('preserves grapheme counts and does not rebuild the agent menu while typing', async () => {
    const composer = document.createElement('scion-chat-composer') as any;
    composer.conversationMode = 'thread';
    composer.members = [{ id: 'a', name: 'Coder', kind: 'agent', email: '' }];
    document.body.append(composer);
    await composer.updateComplete;
    const menu = vi.spyOn(composer, 'renderAgentMenu');
    composer.handleInput({ target: { value: '👨‍👩‍👧‍👦e\u0301' } });
    await composer.updateComplete;
    expect(composer.runeCount).toBe(2);
    expect(menu).not.toHaveBeenCalled();
    composer.members = [
      ...composer.members,
      { id: 'b', name: 'Reviewer', kind: 'agent', email: '' },
    ];
    await composer.updateComplete;
    expect(menu).toHaveBeenCalledOnce();
    expect(composer.shadowRoot.textContent).toContain('Reviewer');
  });
});
