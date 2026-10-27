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
 * The starred-only center of <scion-chat-thread> (messageFilter 'starred',
 * web.chat_starred_messages): the filtered history request, standalone
 * rendering, the banner in place of the composer, read-state invariance,
 * no live appends, refetch on star invalidations and deletions, and the
 * ways back to the full conversation (jump, Show all, Reply, reply preview).
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

class FakeStateManager extends EventTarget {
  currentScope: { type: string; userId: string } | null = null;
  isConnected = false;
  getAgent(): undefined {
    return undefined;
  }
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();
const showToastMock = vi.fn();

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  navigateTo: vi.fn(),
  get stateManager() {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string) => Promise.resolve(fallback),
}));

vi.mock('../../../utils/toast.js', () => ({
  showToast: (...args: unknown[]) => showToastMock(...args) as unknown,
}));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type MessageFilterChangeDetail = import('./chat-thread.js').MessageFilterChangeDetail;
type Message = import('../../../shared/types.js').Message;

const TOPIC = 'topic-1';
const S1 = '00000000-0000-4000-8000-000000000001';
const S2 = '00000000-0000-4000-8000-000000000002';
const U1 = '00000000-0000-4000-8000-000000000009';

interface MenuActionLike {
  id: string;
  label: string;
  icon?: string;
  run: () => void;
}

interface Internals {
  messageMenuActions(msg: Message): MenuActionLike[];
  composerReplyTo: { messageId: string } | null;
  messages: Message[];
}

function message(id: string, minute: number, sender = 'user:other@example.com'): Message {
  return {
    id,
    projectId: 'p1',
    sender,
    senderId: sender === 'user:me@example.com' ? 'user-me' : 'user-other',
    recipient: '',
    recipientId: '',
    msg: 'body ' + id,
    type: 'chat',
    createdAt: `2026-10-20T10:0${minute}:00Z`,
  } as Message;
}

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function calls(match: (url: string, init?: RequestInit) => boolean): string[] {
  return apiFetch.mock.calls
    .filter((c) => match(String(c[0]), c[1] as RequestInit | undefined))
    .map((c) => String(c[0]));
}

const historyCalls = (): string[] => calls((u) => u.includes('/messages?'));
const readCalls = (): string[] => calls((u) => u.includes('/read'));

/**
 * A fake hub: the starred-only history answers `starred()` (newest first,
 * as the hub does), ordinary history answers every message.
 */
function routeHub(opts: {
  starred: () => Message[];
  all?: Message[];
  detail?: (id: string) => Response;
}): void {
  apiFetch.mockImplementation((url: string) => {
    if (url.includes('/messages?')) {
      const filtered = new URL(url, 'http://x').searchParams.get('starred') === 'true';
      const msgs = filtered ? opts.starred() : (opts.all ?? opts.starred());
      return Promise.resolve(json({ messages: [...msgs].reverse(), totalCount: msgs.length }));
    }
    if (url.startsWith('/api/v1/chat/stars/state')) {
      return Promise.resolve(json({ starredMessageIds: [] }));
    }
    if (url.startsWith('/api/v1/chat/stars/') && opts.detail) {
      return Promise.resolve(opts.detail(decodeURIComponent(url.split('/').pop() ?? '')));
    }
    return Promise.resolve(json({}));
  });
}

async function mount(filter: '' | 'starred' = 'starred'): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = TOPIC;
  el.currentUserId = 'user-me';
  el.messageFilter = filter;
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => expect(historyCalls().length).toBeGreaterThan(0));
  await vi.waitFor(() => expect(el.shadowRoot?.querySelector('sl-spinner')).toBeNull());
  return el;
}

function rendered(el: ScionChatThread): Array<HTMLElement & { showHeader: boolean }> {
  return [...(el.shadowRoot?.querySelectorAll('scion-chat-message') ?? [])] as Array<
    HTMLElement & { showHeader: boolean }
  >;
}

function renderedIds(el: ScionChatThread): string[] {
  return rendered(el).map((m) => m.getAttribute('messageId') ?? '');
}

function action(el: ScionChatThread, msg: Message, id: string): MenuActionLike | undefined {
  return (el as unknown as Internals).messageMenuActions(msg).find((a) => a.id === id);
}

function setFlag(on: boolean): void {
  (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
    'web.chat_starred_messages': on,
  };
}

function filterEvents(el: ScionChatThread): MessageFilterChangeDetail[] {
  const seen: MessageFilterChangeDetail[] = [];
  el.addEventListener('chat-message-filter-change', (e) =>
    seen.push((e as CustomEvent<MessageFilterChangeDetail>).detail)
  );
  return seen;
}

describe('scion-chat-thread starred-only center', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
    setFlag(true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  it('loads only starred messages, standalone, with the banner in place of the composer', async () => {
    // Same sender, a minute apart: grouped in the full history, standalone here.
    routeHub({ starred: () => [message(S1, 1), message(S2, 2)] });
    const el = await mount();
    expect(historyCalls()[0]).toContain('starred=true');
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, S2]));
    expect(rendered(el).every((m) => m.showHeader)).toBe(true);
    expect(el.shadowRoot?.querySelector('scion-chat-composer')).toBeNull();
    expect(el.shadowRoot?.querySelector('scion-chat-scheduled-list')).toBeNull();
    const banner = el.shadowRoot?.querySelector('.star-filter-banner');
    expect(banner?.textContent).toContain('Starred messages only');
    expect(banner?.textContent).toContain('omitted');
    expect(banner?.textContent).toContain('Show all messages');
    // The page confirms these are starred: no state request.
    await new Promise((r) => setTimeout(r, 80));
    expect(calls((u) => u.startsWith('/api/v1/chat/stars/state'))).toEqual([]);
    expect(action(el, message(S1, 1), 'star')?.label).toBe('Unstar message');
  });

  it('never reads: no read-state request, no watermark, no unread divider', async () => {
    routeHub({ starred: () => [message(S1, 1), message(S2, 2)] });
    const el = await mount();
    window.dispatchEvent(new Event('focus'));
    const scroller = el.shadowRoot?.querySelector('.messages-scroll');
    scroller?.dispatchEvent(new Event('scroll'));
    await new Promise((r) => setTimeout(r, 1300));
    expect(readCalls()).toEqual([]);
    expect(el.shadowRoot?.querySelector('.unread-divider')).toBeNull();
  });

  it('does not append live messages or show typing indicators', async () => {
    routeHub({ starred: () => [message(S1, 1)] });
    const el = await mount();
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { ...message(U1, 5), conversationKey: TOPIC } },
      })
    );
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-typing-received', {
        detail: { data: { threadId: TOPIC, userId: 'user-other', displayName: 'Other' } },
      })
    );
    await el.updateComplete;
    expect(renderedIds(el)).toEqual([S1]);
    expect(el.shadowRoot?.querySelector('.typing-indicator')).toBeNull();
  });

  it('refetches on a star invalidation, dropping an unstarred message', async () => {
    let starred = [message(S1, 1), message(S2, 2)];
    routeHub({ starred: () => starred });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, S2]));
    starred = [message(S2, 2)];
    const before = historyCalls().length;
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S2]));
    const refetch = historyCalls().slice(before);
    expect(refetch.length).toBeGreaterThan(0);
    expect(refetch.every((u) => u.includes('starred=true'))).toBe(true);
  });

  it('removes a deleted message at once', async () => {
    let starred = [message(S1, 1), message(S2, 2)];
    routeHub({ starred: () => starred });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, S2]));
    starred = [message(S2, 2)];
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-deleted', {
        detail: {
          data: { conversationKey: TOPIC, messageId: S1, deletedAt: '2026-10-21T00:00:00Z' },
        },
      })
    );
    await el.updateComplete;
    expect(renderedIds(el)).toEqual([S2]);
  });

  it('shows the empty state with Show all when nothing is starred', async () => {
    routeHub({ starred: () => [] });
    const el = await mount();
    const seen = filterEvents(el);
    const empty = el.shadowRoot?.querySelector('.star-filter-empty');
    expect(empty?.textContent).toContain('No starred messages in this conversation');
    (empty?.querySelector('sl-button') as HTMLElement).click();
    expect(seen).toEqual([{ filter: '' }]);
  });

  it('jump resolves the star, then asks for the full conversation at the message', async () => {
    const all = [message(S1, 1), message(U1, 2), message(S2, 3)];
    routeHub({
      starred: () => [message(S1, 1), message(S2, 3)],
      all,
      detail: (id) => json({ messageId: id, conversationKey: TOPIC, permalink: '/x' }),
    });
    const el = await mount();
    const seen = filterEvents(el);
    const jump = action(el, message(S1, 1), 'jump');
    expect(jump?.label).toBe('Jump to message in conversation');
    jump?.run();
    await vi.waitFor(() => expect(seen).toEqual([{ filter: '', messageId: S1 }]));
    expect(calls((u) => u === `/api/v1/chat/stars/${S1}`)).toHaveLength(1);

    // The page clears the filter: ordinary history owns the thread.
    const before = historyCalls().length;
    el.messageFilter = '';
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, U1, S2]));
    const after = historyCalls().slice(before);
    expect(after.length).toBeGreaterThan(0);
    expect(after.some((u) => u.includes('starred=true'))).toBe(false);
    expect(el.shadowRoot?.querySelector('.star-filter-banner')).toBeNull();
    expect(el.shadowRoot?.querySelector('scion-chat-composer')).not.toBeNull();
  });

  it('a star no longer available is reported and the list refetched', async () => {
    routeHub({ starred: () => [message(S1, 1)], detail: () => json({}, 404) });
    const el = await mount();
    const seen = filterEvents(el);
    const before = historyCalls().length;
    action(el, message(S1, 1), 'jump')?.run();
    await vi.waitFor(() =>
      expect(showToastMock).toHaveBeenCalledWith(
        'This starred message is no longer available',
        'warning'
      )
    );
    await vi.waitFor(() => expect(historyCalls().length).toBeGreaterThan(before));
    expect(seen).toEqual([]);
  });

  it('a star whose message moved opens its new conversation', async () => {
    routeHub({
      starred: () => [message(S1, 1)],
      detail: (id) => json({ messageId: id, conversationKey: 'topic-2', permalink: '/chat/x' }),
    });
    const el = await mount();
    const opened: unknown[] = [];
    el.addEventListener('chat-star-open', (e) => opened.push((e as CustomEvent).detail));
    action(el, message(S1, 1), 'jump')?.run();
    await vi.waitFor(() => expect(opened).toHaveLength(1));
    expect(opened[0]).toMatchObject({ item: { conversationKey: 'topic-2' } });
  });

  it('Reply opens the full conversation first, then seeds the composer there', async () => {
    routeHub({ starred: () => [message(S1, 1)], all: [message(S1, 1), message(U1, 2)] });
    const el = await mount();
    const seen = filterEvents(el);
    action(el, message(S1, 1), 'reply')?.run();
    expect(seen).toEqual([{ filter: '', messageId: S1 }]);
    expect((el as unknown as Internals).composerReplyTo).toBeNull();
    el.messageFilter = '';
    await vi.waitFor(() =>
      expect((el as unknown as Internals).composerReplyTo?.messageId).toBe(S1)
    );
  });

  it('offers no Delete in the filtered menu (omitted replies are not visible)', async () => {
    routeHub({ starred: () => [message(S1, 1, 'user:me@example.com')] });
    const el = await mount();
    const ids = (el as unknown as Internals)
      .messageMenuActions(message(S1, 1, 'user:me@example.com'))
      .map((a) => a.id);
    expect(ids).toContain('edit');
    expect(ids).not.toContain('delete');
  });

  it('a filtered page arriving after the filter changed is dropped', async () => {
    let releaseFiltered!: () => void;
    const gate = new Promise<void>((r) => (releaseFiltered = r));
    apiFetch.mockImplementation(async (url: string) => {
      if (url.includes('/messages?')) {
        if (url.includes('starred=true')) {
          await gate;
          return json({ messages: [message(S1, 1)] });
        }
        return json({ messages: [message(U1, 2), message(S1, 1)] });
      }
      return json({ starredMessageIds: [] });
    });
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = TOPIC;
    el.currentUserId = 'user-me';
    el.messageFilter = 'starred';
    document.body.appendChild(el);
    await vi.waitFor(() => expect(historyCalls().length).toBe(1));
    el.messageFilter = '';
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, U1]));
    releaseFiltered();
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;
    expect(renderedIds(el)).toEqual([S1, U1]);
    expect(el.shadowRoot?.querySelector('.star-filter-banner')).toBeNull();
  });
});
