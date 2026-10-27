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
 * Opening a starred message from the conversation header
 * (web.chat_starred_messages): the page resolves the star's current
 * target, then jumps. Newer navigation wins: a conversation switch or a
 * newer entry while the detail request runs cancels the jump, and so does
 * a toast Retry after the user has moved on.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

const navigateTo = vi.fn();
const showToastMock = vi.fn();

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return {
    navigateTo: (...args: unknown[]): unknown => navigateTo(...args) as unknown,
    pushRoute: vi.fn(() => Promise.resolve()),
    replaceRoute: vi.fn(() => Promise.resolve()),
    stateManager: state.stateManager,
  };
});

vi.mock('../../utils/toast.js', () => ({
  showToast: (...args: unknown[]): unknown => showToastMock(...args) as unknown,
}));

/** Held detail requests, by message ID; resolved by the test. */
const held = new Map<string, (res: Response) => void>();
const apiFetch = vi.fn((url: string) => {
  const id = decodeURIComponent(url.split('/').pop() ?? '');
  return new Promise<Response>((resolve) => held.set(id, resolve));
});

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: (url: string): Promise<Response> => apiFetch(url) };
});

interface FakeThread {
  jumpSequence: number;
  scrollToMessageById: ReturnType<typeof vi.fn>;
}

interface ChatInternals extends HTMLElement {
  v2Conversation: { conversationKey: string; projectId: string; isDM: boolean } | null;
  handleStarredMessageOpen(e: CustomEvent<{ messageId: string }>): void;
  threadElement(): FakeThread | null;
}

/** Give the page a fake open thread (the page is not rendered here). */
function withThread(page: ChatInternals): FakeThread {
  const thread: FakeThread = { jumpSequence: 1, scrollToMessageById: vi.fn() };
  page.threadElement = (): FakeThread => thread;
  // The page is not connected, so Lit's updateComplete would never settle.
  Object.defineProperty(page, 'updateComplete', { value: Promise.resolve(true) });
  return thread;
}

const M1 = '00000000-0000-4000-8000-000000000001';
const M2 = '00000000-0000-4000-8000-000000000002';

function star(id: string, key: string): Response {
  return new Response(
    JSON.stringify({
      messageId: id,
      conversationKey: key,
      conversationKind: 'thread',
      projectId: 'p1',
      conversationName: 'mock',
      senderName: 'Mock Sender',
      messageCreatedAt: '2026-10-20T10:00:00Z',
      starredAt: '2026-10-22T10:00:00Z',
      excerpt: 'mock',
      edited: false,
      permalink: `/chat/space/p1/thread/${key}#msg-${id}`,
    }),
    { status: 200 }
  );
}

function open(page: ChatInternals, id: string): void {
  page.handleStarredMessageOpen(
    new CustomEvent('starred-message-open', { detail: { messageId: id } })
  );
}

function conversation(key: string): ChatInternals['v2Conversation'] {
  return { conversationKey: key, projectId: 'p1', isDM: false };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  navigateTo.mockReset();
  showToastMock.mockReset();
  apiFetch.mockClear();
  held.clear();
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('opening a starred message from the header', () => {
  it('jumps to a star in another thread when nothing changed meanwhile', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    open(page, M1);
    held.get(M1)?.(star(M1, 'topic-b'));
    await settle();
    expect(navigateTo).toHaveBeenCalledWith(`/chat/space/p1/thread/topic-b#msg-${M1}`);
  });

  it('does not navigate when the user switched conversation during the request', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    open(page, M1);
    page.v2Conversation = conversation('topic-c'); // newer navigation
    held.get(M1)?.(star(M1, 'topic-b'));
    await settle();
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('lets a newer entry win over an older one still loading', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    open(page, M1);
    open(page, M2);
    held.get(M2)?.(star(M2, 'topic-d'));
    await settle();
    held.get(M1)?.(star(M1, 'topic-b'));
    await settle();
    expect(navigateTo.mock.calls).toEqual([[`/chat/space/p1/thread/topic-d#msg-${M2}`]]);
  });

  it('jumps within the open thread when nothing changed meanwhile', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    const thread = withThread(page);
    open(page, M1);
    held.get(M1)?.(star(M1, 'topic-a'));
    await settle();
    expect(thread.scrollToMessageById).toHaveBeenCalledWith(M1, true, true);
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('yields to a newer jump inside the same thread during the request', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    const thread = withThread(page);
    open(page, M1);
    thread.jumpSequence = 2; // a search or reply jump while the GET ran
    held.get(M1)?.(star(M1, 'topic-a'));
    await settle();
    expect(thread.scrollToMessageById).not.toHaveBeenCalled();
  });

  it('offers Retry on failure, which does nothing once the user has moved on', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    open(page, M1);
    held.get(M1)?.(new Response('{}', { status: 500 }));
    await settle();
    expect(showToastMock).toHaveBeenCalledTimes(1);
    const options = showToastMock.mock.calls[0][2] as {
      action: { label: string; run: () => void };
    };
    expect(options.action.label).toBe('Retry');
    page.v2Conversation = conversation('topic-c');
    const before = apiFetch.mock.calls.length;
    options.action.run();
    await settle();
    expect(apiFetch.mock.calls.length).toBe(before);
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('gives focus back to the star button when the star is no longer available or the request fails', async () => {
    const page = document.createElement('scion-page-chat') as ChatInternals;
    page.v2Conversation = conversation('topic-a');
    withThread(page);
    const refocus = vi.fn();
    (page as unknown as { refocusStarredTrigger: () => void }).refocusStarredTrigger = refocus;
    open(page, M1);
    held.get(M1)?.(new Response('{}', { status: 404 }));
    await settle();
    expect(refocus).toHaveBeenCalledTimes(1);
    expect(showToastMock).toHaveBeenCalledWith(
      'This starred message is no longer available',
      'warning'
    );

    open(page, M2);
    held.get(M2)?.(new Response('{}', { status: 500 }));
    await settle();
    expect(refocus).toHaveBeenCalledTimes(2);
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('the real refocus moves focus from the body to the star button, and leaves other focus alone', async () => {
    const PageClass = customElements.get('scion-page-chat') as unknown as {
      prototype: { refocusStarredTrigger(this: { shadowRoot: ShadowRoot | null }): void };
    };
    const refocus = PageClass.prototype.refocusStarredTrigger;
    // A connected stand-in for the page's shadow tree, holding a real list.
    const host = document.createElement('div');
    document.body.appendChild(host);
    const root = host.attachShadow({ mode: 'open' });
    const list = document.createElement('scion-chat-starred-list') as HTMLElement & {
      conversationKey: string;
      updateComplete: Promise<boolean>;
    };
    list.conversationKey = 'topic-a';
    root.appendChild(list);
    await list.updateComplete;
    const trigger = list.shadowRoot!.querySelector<HTMLButtonElement>('button.trigger')!;

    (document.activeElement as HTMLElement | null)?.blur();
    refocus.call({ shadowRoot: root });
    expect(list.shadowRoot!.activeElement).toBe(trigger);

    const other = document.createElement('button');
    document.body.appendChild(other);
    other.focus();
    refocus.call({ shadowRoot: root });
    expect(document.activeElement).toBe(other);
  });
});
