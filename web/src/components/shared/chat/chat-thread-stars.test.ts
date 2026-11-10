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
 * Starred messages in <scion-chat-thread> (web.chat_starred_messages):
 * the message menu's Star/Unstar toggle (pending, error with retry,
 * announcement), the private indicator, the per-page state fetch and the
 * SSE invalidation. Flag off keeps the prior menu exactly.
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
const { renderMenuRows } = await import('./context-menu.js');
const { render } = await import('lit');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type Message = import('../../../shared/types.js').Message;

const TOPIC = 'topic-1';
const M1 = '00000000-0000-4000-8000-000000000001';
const M2 = '00000000-0000-4000-8000-000000000002';

interface MenuActionLike {
  id: string;
  label: string;
  icon?: string;
  disabled?: boolean;
  checked?: boolean;
  run: () => void;
}

interface Internals {
  messageMenuActions(msg: Message): MenuActionLike[];
  starAnnouncement: string;
}

function message(id: string): Message {
  return {
    id,
    projectId: 'p1',
    sender: 'user:other@example.com',
    senderId: 'user-other',
    recipient: '',
    recipientId: '',
    msg: 'hello ' + id,
    type: 'chat',
    createdAt: '2026-10-20T10:00:00Z',
  } as Message;
}

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

/** Route fake hub answers: history, star state, PUT and DELETE. */
function routeHub(opts: {
  starred?: string[];
  put?: () => Response | Promise<Response>;
  del?: () => Response | Promise<Response>;
}): void {
  apiFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url.includes('/messages?')) {
      return Promise.resolve(json({ messages: [message(M1), message(M2)] }));
    }
    if (url.startsWith('/api/v1/chat/stars/state')) {
      return Promise.resolve(json({ starredMessageIds: opts.starred ?? [] }));
    }
    if (url.startsWith('/api/v1/chat/stars/') && init?.method === 'PUT') {
      return Promise.resolve(
        opts.put ? opts.put() : json({ starred: true, starredAt: '2026-10-22T10:00:00Z' })
      );
    }
    if (url.startsWith('/api/v1/chat/stars/') && init?.method === 'DELETE') {
      return Promise.resolve(opts.del ? opts.del() : json(null, 204));
    }
    return Promise.resolve(json({}));
  });
}

function stateCalls(): string[] {
  return apiFetch.mock.calls
    .map((c) => String(c[0]))
    .filter((u) => u.startsWith('/api/v1/chat/stars/state'));
}

async function mount(key = TOPIC): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = key;
  el.currentUserId = 'user-me';
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() =>
    expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('/messages?'))).toBe(true)
  );
  return el;
}

function starAction(el: ScionChatThread, id: string): MenuActionLike | undefined {
  return (el as unknown as Internals).messageMenuActions(message(id)).find((a) => a.id === 'star');
}

function setFlag(on: boolean): void {
  (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
    'web.chat_starred_messages': on,
  };
}

describe('scion-chat-thread starred messages', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  it('flag off: no star action and no state request (prior menu exactly)', async () => {
    setFlag(false);
    routeHub({});
    const el = await mount();
    await new Promise((r) => setTimeout(r, 80));
    expect(stateCalls()).toEqual([]);
    expect(starAction(el, M1)).toBeUndefined();
  });

  it('fetches the state of a loaded page once and marks starred messages', async () => {
    setFlag(true);
    routeHub({ starred: [M2] });
    const el = await mount();
    await vi.waitFor(() => expect(stateCalls().length).toBe(1));
    expect(stateCalls()[0]).toContain(`messageId=${M1}`);
    expect(stateCalls()[0]).toContain(`messageId=${M2}`);
    await vi.waitFor(() => expect(starAction(el, M2)?.label).toBe('Unstar message'));
    const unstarred = starAction(el, M1);
    expect(unstarred).toMatchObject({ label: 'Star message', icon: 'star', checked: false });
    expect(starAction(el, M2)).toMatchObject({ icon: 'star-fill', checked: true });

    await el.updateComplete;
    const rendered = [...(el.shadowRoot?.querySelectorAll('scion-chat-message') ?? [])] as Array<
      HTMLElement & { starred: boolean }
    >;
    expect(rendered.filter((m) => m.starred)).toHaveLength(1);
  });

  it('is disabled with a loading label until the state is known', async () => {
    setFlag(true);
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    apiFetch.mockImplementation(async (url: string) => {
      if (url.includes('/messages?')) return json({ messages: [message(M1)] });
      if (url.startsWith('/api/v1/chat/stars/state')) {
        await gate;
        return json({ starredMessageIds: [] });
      }
      return json({});
    });
    const el = await mount();
    expect(starAction(el, M1)).toMatchObject({ disabled: true, label: 'Star message (loading)' });
    release();
    await vi.waitFor(() => expect(starAction(el, M1)?.disabled).toBeFalsy());
  });

  it('stars with a pending state, then announces politely', async () => {
    setFlag(true);
    let resolvePut!: (r: Response) => void;
    routeHub({
      put: () => new Promise<Response>((r) => (resolvePut = r)),
    });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.disabled).toBeFalsy());

    starAction(el, M1)?.run();
    await vi.waitFor(() =>
      expect(starAction(el, M1)).toMatchObject({ disabled: true, checked: true })
    );
    const put = apiFetch.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'PUT'
    );
    expect(put?.[0]).toBe(`/api/v1/chat/stars/${M1}`);
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      conversationKey: TOPIC,
    });

    resolvePut(json({ starred: true, starredAt: '2026-10-22T10:00:00Z' }));
    await vi.waitFor(() =>
      expect((el as unknown as Internals).starAnnouncement).toBe('Message starred')
    );
    expect(starAction(el, M1)).toMatchObject({ label: 'Unstar message', disabled: false });
    await el.updateComplete;
    const region = el.shadowRoot?.querySelector('.star-live-region');
    expect(region?.getAttribute('aria-live')).toBe('polite');
    expect(region?.textContent).toContain('Message starred');
  });

  it('restores the prior state and offers a retry when unstarring fails', async () => {
    setFlag(true);
    let failures = 1;
    routeHub({
      starred: [M1],
      del: () => (failures-- > 0 ? json({ error: { message: 'boom' } }, 500) : json(null, 204)),
    });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));

    starAction(el, M1)?.run();
    await vi.waitFor(() => expect(showToastMock).toHaveBeenCalled());
    const [text, variant, options] = showToastMock.mock.calls[0] as [
      string,
      string,
      { action: { label: string; run: () => void } },
    ];
    expect(text).toBe('Could not unstar message');
    expect(variant).toBe('danger');
    expect(starAction(el, M1)?.checked).toBe(true);

    expect(options.action.label).toBe('Retry');
    options.action.run();
    await vi.waitFor(() =>
      expect((el as unknown as Internals).starAnnouncement).toBe('Message unstarred')
    );
    expect(starAction(el, M1)?.checked).toBe(false);
  });

  it('refetches state on the SSE invalidation', async () => {
    setFlag(true);
    const starred: string[] = [];
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?')) return Promise.resolve(json({ messages: [message(M1)] }));
      if (url.startsWith('/api/v1/chat/stars/state')) {
        return Promise.resolve(json({ starredMessageIds: [...starred] }));
      }
      return Promise.resolve(json({}));
    });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(false));
    starred.push(M1); // starred in another tab
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-stars-updated', { detail: { data: { action: 'changed' } } })
    );
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));
  });

  it('re-announces the same text for a second star', async () => {
    setFlag(true);
    routeHub({});
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M2)?.disabled).toBeFalsy());
    const region = el.shadowRoot!.querySelector('.star-live-region')!;
    const seen: string[] = [];
    const observer = new MutationObserver(() => seen.push(region.textContent?.trim() ?? ''));
    observer.observe(region, { childList: true, characterData: true, subtree: true });

    starAction(el, M1)?.run();
    await vi.waitFor(() => expect(seen.at(-1)).toBe('Message starred'));
    starAction(el, M2)?.run();
    await vi.waitFor(() => expect(starAction(el, M2)?.checked).toBe(true));
    await vi.waitFor(() => expect(seen.filter((t) => t === 'Message starred')).toHaveLength(2));
    observer.disconnect();
    // Emptied between the two announcements, so the second is not ignored.
    const second = seen.lastIndexOf('Message starred');
    expect(seen.slice(seen.indexOf('Message starred') + 1, second)).toContain('');
  });

  it('offers no star action for an unsent local row', async () => {
    setFlag(true);
    routeHub({});
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.disabled).toBeFalsy());
    expect(starAction(el, 'local-1700000000000')).toBeUndefined();
  });

  it('renders the desktop toggle row as a checked menu item', () => {
    const host = document.createElement('div');
    render(
      renderMenuRows([{ id: 'star', label: 'Unstar message', checked: true, run: () => {} }]),
      host
    );
    const row = host.querySelector('.context-menu-item')!;
    expect(row.textContent).toContain('Unstar message');
    expect(row.getAttribute('role')).toBe('menuitemcheckbox');
    expect(row.getAttribute('aria-checked')).toBe('true');
  });

  it('sends no state request for a live message, which starts unstarred', async () => {
    setFlag(true);
    routeHub({ starred: [M1] });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));
    expect(stateCalls()).toHaveLength(1);

    const M3 = '00000000-0000-4000-8000-000000000003';
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: {
          data: {
            id: M3,
            conversationKey: TOPIC,
            projectId: 'p1',
            sender: 'user:other@example.com',
            senderId: 'user-other',
            msg: 'live hello',
            type: 'chat',
            createdAt: '2026-10-20T11:00:00Z',
          },
        },
      })
    );
    await vi.waitFor(() =>
      expect((el as unknown as { messages: Message[] }).messages.some((m) => m.id === M3)).toBe(
        true
      )
    );
    await new Promise((r) => setTimeout(r, 120));
    expect(stateCalls()).toHaveLength(1);
    expect(starAction(el, M3)).toMatchObject({ label: 'Star message', disabled: false });
  });

  it('a failure Retry does nothing after the conversation changed', async () => {
    setFlag(true);
    routeHub({
      starred: [M1],
      del: () => json({ error: { message: 'boom' } }, 500),
    });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));
    starAction(el, M1)?.run();
    await vi.waitFor(() => expect(showToastMock).toHaveBeenCalled());
    const deletes = (): number =>
      apiFetch.mock.calls.filter((c) => (c[1] as RequestInit | undefined)?.method === 'DELETE')
        .length;
    expect(deletes()).toBe(1);
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    const options = showToastMock.mock.calls[0][2] as { action: { run: () => void } };
    options.action.run();
    await new Promise((r) => setTimeout(r, 30));
    expect(deletes()).toBe(1);
  });

  it('fetches state for the window of an around jump', async () => {
    setFlag(true);
    const M8 = '00000000-0000-4000-8000-000000000008';
    const M9 = '00000000-0000-4000-8000-000000000009';
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?') && url.includes('around=')) {
        return Promise.resolve(json({ messages: [message(M8), message(M9)] }));
      }
      if (url.includes('/messages?')) {
        return Promise.resolve(json({ messages: [message(M1), message(M2)] }));
      }
      if (url.startsWith('/api/v1/chat/stars/state')) {
        return Promise.resolve(json({ starredMessageIds: url.includes(M9) ? [M9] : [] }));
      }
      return Promise.resolve(json({}));
    });
    const el = await mount();
    await vi.waitFor(() => expect(stateCalls()).toHaveLength(1));
    void el.scrollToMessageById(M9);
    await vi.waitFor(() => expect(stateCalls()).toHaveLength(2));
    expect(stateCalls()[1]).toContain(`messageId=${M8}`);
    expect(stateCalls()[1]).toContain(`messageId=${M9}`);
    await vi.waitFor(() =>
      expect(starAction(el, M9)).toMatchObject({ checked: true, disabled: false })
    );
  });

  it('fetches state only for the new messages of a catch-up backfill', async () => {
    setFlag(true);
    const M3 = '00000000-0000-4000-8000-000000000003';
    let page = [message(M1), message(M2)];
    apiFetch.mockImplementation((url: string) => {
      if (url.includes('/messages?')) return Promise.resolve(json({ messages: page }));
      if (url.startsWith('/api/v1/chat/stars/state')) {
        return Promise.resolve(json({ starredMessageIds: [] }));
      }
      return Promise.resolve(json({}));
    });
    const el = await mount();
    await vi.waitFor(() => expect(stateCalls()).toHaveLength(1));
    page = [message(M1), message(M2), message(M3)];
    // A lightweight event (no payload) makes the thread catch up.
    fakeStateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.waitFor(() => expect(stateCalls()).toHaveLength(2));
    expect(stateCalls()[1]).toContain(`messageId=${M3}`);
    expect(stateCalls()[1]).not.toContain(`messageId=${M1}`);
    await vi.waitFor(() => expect(starAction(el, M3)?.disabled).toBeFalsy());
  });

  it('offers the star action in a DM and stars under the DM key', async () => {
    setFlag(true);
    const key = 'dm:agent:a1:user:user-me';
    routeHub({ starred: [M2] });
    const el = await mount(key);
    await vi.waitFor(() => expect(stateCalls()).toHaveLength(1));
    await vi.waitFor(() =>
      expect(starAction(el, M2)).toMatchObject({ checked: true, disabled: false })
    );
    expect(starAction(el, M1)).toMatchObject({ checked: false, disabled: false });
    starAction(el, M1)!.run();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));
    const put = apiFetch.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'PUT'
    );
    expect(put).toBeTruthy();
    expect(JSON.parse((put![1] as RequestInit).body as string)).toEqual({ conversationKey: key });
  });

  it('a deleted message loses its star indicator at once, from any surface', async () => {
    setFlag(true);
    routeHub({ starred: [M1] });
    const el = await mount();
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).toBe(true));
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-deleted', {
        detail: {
          data: { conversationKey: TOPIC, messageId: M1, deletedAt: '2026-10-22T10:00:00Z' },
        },
      })
    );
    // The action may be gone with the message; it is never shown starred.
    await vi.waitFor(() => expect(starAction(el, M1)?.checked).not.toBe(true));
    expect((el as unknown as { stars: { state(id: string): string } }).stars.state(M1)).toBe(
      'unstarred'
    );
    expect(stateCalls()).toHaveLength(1);
  });
});
