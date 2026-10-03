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

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/main.js', () => ({ stateManager: new EventTarget() }));
vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}'))),
  extractApiError: vi.fn(),
}));

await import('./chat-thread.js');

afterEach(() => {
  document.body.replaceChildren();
  localStorage.clear();
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
