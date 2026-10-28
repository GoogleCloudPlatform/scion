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
  handleChatSendV2(e: CustomEvent<Record<string, unknown>>): Promise<void>;
  exitFilter(target: { messageId: string; highlight: boolean } | null, jump: boolean): void;
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

  it('a send completing after the switch to the starred center does not backfill unfiltered history', async () => {
    let releasePost!: () => void;
    const post = new Promise<void>((r) => (releasePost = r));
    apiFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.endsWith('/messages')) {
        await post;
        return json({ id: '00000000-0000-4000-8000-0000000000aa' });
      }
      if (url.includes('/messages?')) {
        return url.includes('starred=true')
          ? json({ messages: [message(S1, 1)] })
          : json({ messages: [message(U1, 2), message(S1, 1)] });
      }
      return json({ starredMessageIds: [] });
    });
    const el = await mount('');
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, U1]));
    void (el as unknown as Internals).handleChatSendV2(
      new CustomEvent('chat-send', {
        detail: {
          text: 'Mock new message',
          plain: false,
          interrupt: false,
          onSuccess: () => {},
          mentions: [],
          attachmentIds: [],
        },
      })
    );
    await vi.waitFor(() =>
      expect(calls((u, i) => i?.method === 'POST' && u.endsWith('/messages'))).toHaveLength(1)
    );
    el.messageFilter = 'starred';
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    const before = historyCalls().length;
    releasePost();
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;
    const after = historyCalls().slice(before);
    expect(after.filter((u) => !u.includes('starred=true'))).toEqual([]);
    expect(renderedIds(el)).toEqual([S1]);
  });

  it('a filter exit target is used only in its own conversation', async () => {
    // The other conversation's history does not hold the target, so a
    // misapplied target would show as an around request there.
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?')) {
        if (url.includes('topic-2')) return Promise.resolve(json({ messages: [message(U1, 2)] }));
        return Promise.resolve(json({ messages: [message(S1, 1)] }));
      }
      return Promise.resolve(json({ starredMessageIds: [] }));
    });
    const el = await mount();
    // The page ignores the event (no listener): the target stays pending.
    (el as unknown as Internals).exitFilter({ messageId: S1, highlight: true }, true);
    el.conversationKey = 'topic-2';
    el.messageFilter = '';
    await el.updateComplete;
    await vi.waitFor(() => expect(historyCalls().some((u) => u.includes('topic-2'))).toBe(true));
    await new Promise((r) => setTimeout(r, 50));
    // No around request for the other conversation's message.
    expect(calls((u) => u.includes('topic-2') && u.includes('around='))).toEqual([]);
  });

  it('a refresh far up the starred list refetches around the reader instead of the newest page', async () => {
    const many = Array.from({ length: 260 }, (_, i) =>
      message(`00000000-0000-4000-8000-${(1000 + i).toString(16).padStart(12, '0')}`, 0)
    );
    many.forEach((m, i) => {
      m.createdAt = new Date(Date.parse('2026-10-20T00:00:00Z') + i * 60_000).toISOString();
    });
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?')) {
        const params = new URL(url, 'http://x').searchParams;
        const around = params.get('around');
        const cursor = params.get('cursor');
        if (around) {
          const at = many.findIndex((m) => m.id === around);
          return Promise.resolve(
            json({ messages: many.slice(Math.max(0, at - 25), at + 25).reverse() })
          );
        }
        const end = cursor ? Number(cursor.slice(2)) : many.length;
        const page = many.slice(Math.max(0, end - 50), end);
        return Promise.resolve(
          json({
            messages: [...page].reverse(),
            ...(end - 50 > 0 ? { nextCursor: `c-${end - 50}` } : {}),
          })
        );
      }
      return Promise.resolve(json({ starredMessageIds: [] }));
    });
    const el = await mount();
    // Load older pages until 260 are loaded, as a reader scrolling up would.
    const internals = el as unknown as {
      loadOlderMessagesV2(scroller: HTMLElement): Promise<void>;
      _scrollAnchor: {
        conversationKey: string;
        messageId: string;
        offset: number;
        pinnedToBottom: boolean;
      } | null;
      pinnedToBottom: boolean;
      captureScrollAnchor(): void;
    };
    const scroller = el.shadowRoot?.querySelector('.messages-scroll') as HTMLElement;
    for (let i = 0; i < 5; i++) await internals.loadOlderMessagesV2(scroller);
    await vi.waitFor(() => expect(renderedIds(el).length).toBe(260));
    // The reader sits at the 10th oldest starred message.
    const target = many[10].id;
    internals.captureScrollAnchor = (): void => {
      internals._scrollAnchor = {
        conversationKey: TOPIC,
        messageId: target,
        offset: 0,
        pinnedToBottom: false,
      };
    };
    internals.pinnedToBottom = false;
    const before = historyCalls().length;
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
    await vi.waitFor(() => expect(historyCalls().length).toBeGreaterThan(before));
    const refetch = historyCalls().slice(before);
    expect(refetch[0]).toContain(`around=${target}`);
    expect(refetch[0]).toContain('starred=true');
    await vi.waitFor(() => expect(renderedIds(el)).toContain(target));

    // A second invalidation (an unstar here is followed by the hub's own
    // stars event) keeps refetching around the reader, never the tail page.
    const second = historyCalls().length;
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
    await vi.waitFor(() => expect(historyCalls().length).toBeGreaterThan(second));
    await new Promise((r) => setTimeout(r, 30));
    const refetch2 = historyCalls().slice(second);
    expect(refetch2.every((u) => u.includes('around='))).toBe(true);
    expect(renderedIds(el)).toContain(target);
  });

  it('a refresh whose around targets are all gone falls back to the tail and leaves the around state', async () => {
    const many = Array.from({ length: 260 }, (_, i) =>
      message(`00000000-0000-4000-8000-${(2000 + i).toString(16).padStart(12, '0')}`, 0)
    );
    many.forEach((m, i) => {
      m.createdAt = new Date(Date.parse('2026-10-20T00:00:00Z') + i * 60_000).toISOString();
    });
    let aroundGone = false;
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?')) {
        const params = new URL(url, 'http://x').searchParams;
        const around = params.get('around');
        if (around) {
          if (aroundGone) return Promise.resolve(json({}, 404));
          const at = many.findIndex((m) => m.id === around);
          return Promise.resolve(
            json({ messages: many.slice(Math.max(0, at - 25), at + 25).reverse() })
          );
        }
        const cursor = params.get('cursor');
        const limit = Number(params.get('limit') ?? 50);
        const end = cursor ? Number(cursor.slice(2)) : many.length;
        const page = many.slice(Math.max(0, end - limit), end);
        return Promise.resolve(
          json({
            messages: [...page].reverse(),
            ...(end - limit > 0 ? { nextCursor: `c-${end - limit}` } : {}),
          })
        );
      }
      return Promise.resolve(json({ starredMessageIds: [] }));
    });
    const el = await mount();
    const internals = el as unknown as {
      loadOlderMessagesV2(scroller: HTMLElement): Promise<void>;
      _scrollAnchor: {
        conversationKey: string;
        messageId: string;
        offset: number;
        pinnedToBottom: boolean;
      } | null;
      pinnedToBottom: boolean;
      viewingAroundMessage: boolean;
      captureScrollAnchor(): void;
    };
    const scroller = el.shadowRoot?.querySelector('.messages-scroll') as HTMLElement;
    for (let i = 0; i < 5; i++) await internals.loadOlderMessagesV2(scroller);
    await vi.waitFor(() => expect(renderedIds(el).length).toBe(260));
    const target = many[10].id;
    internals.captureScrollAnchor = (): void => {
      internals._scrollAnchor = {
        conversationKey: TOPIC,
        messageId: target,
        offset: 0,
        pinnedToBottom: false,
      };
    };
    internals.pinnedToBottom = false;
    // First refresh: around the reader (an around window).
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
    await vi.waitFor(() => expect(internals.viewingAroundMessage).toBe(true));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.jump-to-latest')).not.toBeNull();

    // Every around target is gone now: the next refresh falls back to the tail.
    aroundGone = true;
    const before = historyCalls().length;
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
    await vi.waitFor(() => expect(internals.viewingAroundMessage).toBe(false));
    await el.updateComplete;
    const refetch = historyCalls().slice(before);
    const last = refetch[refetch.length - 1];
    expect(last).toContain('limit=');
    expect(last).not.toContain('around=');
    expect(last).toContain('starred=true');
    expect(internals.pinnedToBottom).toBe(true);
    expect(el.shadowRoot?.querySelector('.jump-to-latest')).toBeNull();
  });

  // Up to the history limit a refresh reloads every loaded row; past it, it
  // refetches around the reader. Both end in the empty state.
  it.each([
    { size: 80, first: 'limit=80' },
    { size: 260, first: 'around=' },
  ])(
    'a refresh far up a starred list of $size that leaves nothing starred shows the empty state',
    async ({ size, first }) => {
      const many = Array.from({ length: size }, (_, i) =>
        message(`00000000-0000-4000-8000-${(3000 + i).toString(16).padStart(12, '0')}`, 0)
      );
      many.forEach((m, i) => {
        m.createdAt = new Date(Date.parse('2026-10-20T00:00:00Z') + i * 60_000).toISOString();
      });
      let starred = many;
      apiFetch.mockImplementation((url: string) => {
        if (url.includes('/messages?')) {
          const params = new URL(url, 'http://x').searchParams;
          const around = params.get('around');
          if (around) {
            const at = starred.findIndex((m) => m.id === around);
            if (at < 0) return Promise.resolve(json({}, 404));
            return Promise.resolve(
              json({ messages: starred.slice(Math.max(0, at - 25), at + 25).reverse() })
            );
          }
          const cursor = params.get('cursor');
          const limit = Number(params.get('limit') ?? 50);
          const end = cursor ? Number(cursor.slice(2)) : starred.length;
          const page = starred.slice(Math.max(0, end - limit), end);
          return Promise.resolve(
            json({
              messages: [...page].reverse(),
              ...(end - limit > 0 ? { nextCursor: `c-${end - limit}` } : {}),
            })
          );
        }
        return Promise.resolve(json({ starredMessageIds: [] }));
      });
      const el = await mount();
      const internals = el as unknown as {
        loadOlderMessagesV2(scroller: HTMLElement): Promise<void>;
        _scrollAnchor: {
          conversationKey: string;
          messageId: string;
          offset: number;
          pinnedToBottom: boolean;
        } | null;
        pinnedToBottom: boolean;
        viewingAroundMessage: boolean;
        captureScrollAnchor(): void;
      };
      const scroller = el.shadowRoot?.querySelector('.messages-scroll') as HTMLElement;
      while (renderedIds(el).length < size) {
        const loaded = renderedIds(el).length;
        await internals.loadOlderMessagesV2(scroller);
        await vi.waitFor(() => expect(renderedIds(el).length).toBeGreaterThan(loaded));
      }
      expect(renderedIds(el).length).toBe(size);
      // The reader sits far up the list when every star is removed elsewhere.
      const target = many[5].id;
      internals.captureScrollAnchor = (): void => {
        internals._scrollAnchor = {
          conversationKey: TOPIC,
          messageId: target,
          offset: 0,
          pinnedToBottom: false,
        };
      };
      internals.pinnedToBottom = false;
      starred = [];
      const seen = filterEvents(el);
      const before = historyCalls().length;
      fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated', { detail: {} }));
      await vi.waitFor(() => expect(renderedIds(el)).toEqual([]));
      await el.updateComplete;
      const refetch = historyCalls().slice(before);
      expect(refetch[0]).toContain(first === 'around=' ? `around=${target}` : first);
      expect(refetch.every((u) => u.includes('starred=true'))).toBe(true);
      expect(internals.viewingAroundMessage).toBe(false);
      expect(el.shadowRoot?.querySelector('.jump-to-latest')).toBeNull();
      const empty = el.shadowRoot?.querySelector('.star-filter-empty');
      expect(empty?.textContent).toContain('No starred messages in this conversation');
      expect(el.shadowRoot?.querySelector('sl-spinner')).toBeNull();
      // Show all still leaves the filter.
      (empty?.querySelector('sl-button') as HTMLElement).click();
      expect(seen).toEqual([{ filter: '' }]);
    }
  );

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

describe('scion-chat-thread starred-only center: reconcile and semantics', () => {
  let now = 1_000_000;

  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
    setFlag(true);
    vi.spyOn(Date, 'now').mockImplementation(() => now);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
    delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  function returnToPage(): void {
    now += 10_000;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  const stateCalls = (): string[] => calls((u) => u.startsWith('/api/v1/chat/stars/state'));

  it('refetches the starred list on returning to the page', async () => {
    routeHub({ starred: () => [message(S1, 1)] });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    const before = historyCalls().length;
    returnToPage();
    await vi.waitFor(() => expect(historyCalls().length).toBe(before + 1));
    expect(historyCalls().at(-1)).toContain('starred=true');
  });

  it('the full conversation refetches star state on return, not history', async () => {
    routeHub({ starred: () => [], all: [message(S1, 1), message(S2, 2)] });
    const el = await mount('');
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, S2]));
    await vi.waitFor(() => expect(stateCalls().length).toBe(1));
    const history = historyCalls().length;
    returnToPage();
    await vi.waitFor(() => expect(stateCalls().length).toBe(2));
    expect(historyCalls().length).toBe(history);
  });

  it('stops reconciling once removed', async () => {
    routeHub({ starred: () => [message(S1, 1)] });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    el.remove();
    const before = apiFetch.mock.calls.length;
    returnToPage();
    await new Promise((r) => setTimeout(r, 20));
    expect(apiFetch.mock.calls.length).toBe(before);
  });

  it('labels the starred list region and announces the empty state', async () => {
    routeHub({ starred: () => [message(S1, 1)] });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    const region = el.shadowRoot!.querySelector('.messages-scroll')!;
    expect(region.getAttribute('role')).toBe('region');
    expect(region.getAttribute('aria-label')).toBe('Starred messages');

    document.body.innerHTML = '';
    apiFetch.mockReset();
    routeHub({ starred: () => [] });
    const empty = await mount();
    await vi.waitFor(() =>
      expect(empty.shadowRoot!.querySelector('.star-filter-empty')?.getAttribute('role')).toBe(
        'status'
      )
    );
  });

  it('the full conversation keeps its unlabelled scroller', async () => {
    routeHub({ starred: () => [], all: [message(S1, 1)] });
    const el = await mount('');
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    const scroller = el.shadowRoot!.querySelector('.messages-scroll')!;
    expect(scroller.hasAttribute('role')).toBe(false);
    expect(scroller.hasAttribute('aria-label')).toBe(false);
  });
});

describe('scion-chat-thread starred-only center: focus repair', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
    setFlag(true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  type Msg = import('./chat-message.js').ScionChatMessage;

  it('when the focused starred row goes, focus moves to the closest row, then to Show all', async () => {
    let starred = [message(S1, 1), message(S2, 2)];
    routeHub({ starred: () => starred });
    const el = await mount();
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1, S2]));
    const row = (id: string): Msg =>
      el.shadowRoot!.querySelector<Msg>(`#msg-${id}`)! as unknown as Msg;
    await row(S2).updateComplete;
    row(S2).actionsButton!.focus();

    starred = [message(S1, 1)];
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() => expect(renderedIds(el)).toEqual([S1]));
    await vi.waitFor(() => expect(row(S1).shadowRoot!.activeElement).toBe(row(S1).actionsButton));

    starred = [];
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() =>
      expect(el.shadowRoot!.querySelector('.star-filter-empty')).not.toBeNull()
    );
    const showAll = el.shadowRoot!.querySelector('.star-filter-empty sl-button');
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(showAll));
  });
});
