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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const fakeStateManager = new EventTarget();
const apiFetch = vi.fn();

vi.mock('../../../client/state.js', () => ({
  get stateManager() {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string) => Promise.resolve(fallback),
}));

await import('./chat-starred-list.js');
const { chatStarsLocalChanges } = await import('../../../client/chat-stars.js');
type ScionChatStarredList = import('./chat-starred-list.js').ScionChatStarredList;
type StarItem = import('../../../client/chat-stars.js').StarItem;

function item(n: number): StarItem {
  const id = `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
  return {
    messageId: id,
    conversationKey: 'topic-1',
    conversationKind: 'thread',
    projectId: 'p1',
    projectName: 'Example project',
    conversationName: 'Planning',
    senderName: 'Example person',
    messageCreatedAt: '2026-10-20T10:00:00Z',
    starredAt: '2026-10-22T10:00:00Z',
    excerpt: `Mock excerpt ${n}`,
    edited: false,
    permalink: `/chat/space/p1/thread/topic-1#msg-${id}`,
  };
}

function json(body: unknown, status = 200): Response {
  return { ok: status < 300, status, json: () => Promise.resolve(body) } as unknown as Response;
}

async function mount(): Promise<ScionChatStarredList> {
  const el = document.createElement('scion-chat-starred-list') as ScionChatStarredList;
  el.conversationKey = 'topic-1';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** The trigger: the focusable control itself, which carries the ARIA state. */
function button(el: ScionChatStarredList): HTMLButtonElement {
  return el.shadowRoot!.querySelector<HTMLButtonElement>('button.trigger')!;
}

async function openPanel(el: ScionChatStarredList): Promise<void> {
  button(el).click();
  await el.updateComplete;
}

function entries(el: ScionChatStarredList): HTMLAnchorElement[] {
  return [...el.shadowRoot!.querySelectorAll<HTMLAnchorElement>('a.entry')];
}

describe('scion-chat-starred-list', () => {
  beforeEach(() => apiFetch.mockReset());
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('is a labelled, collapsed button until opened', async () => {
    const el = await mount();
    const b = button(el);
    expect(b).toBeInstanceOf(HTMLButtonElement);
    expect(b.getAttribute('aria-label')).toBe('Starred messages in this conversation');
    expect(b.getAttribute('aria-haspopup')).toBe('dialog');
    expect(b.getAttribute('aria-expanded')).toBe('false');
    b.focus();
    expect(el.shadowRoot!.activeElement).toBe(b);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el.shadowRoot!.querySelector('.panel')).toBeNull();
  });

  it('opens a labelled dialog listing this conversation, newest star first, with Load more', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(2), item(1)], nextCursor: 'c1.next' }))
      .mockResolvedValueOnce(json({ items: [item(1), item(0)] }));
    const el = await mount();
    await openPanel(el);
    expect(String(apiFetch.mock.calls[0][0])).toBe(
      '/api/v1/chat/stars?conversationKey=topic-1&limit=20'
    );
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    const panel = el.shadowRoot!.querySelector('.panel')!;
    expect(panel.getAttribute('role')).toBe('dialog');
    expect(button(el).getAttribute('aria-expanded')).toBe('true');
    expect(button(el).getAttribute('aria-controls')).toBe(panel.id);
    // aria-controls resolves within the same shadow root.
    expect(el.shadowRoot!.getElementById(panel.id)).toBe(panel);
    expect(el.shadowRoot!.querySelector('ul')?.getAttribute('aria-label')).toBe(
      'Starred messages in this conversation'
    );
    expect(el.shadowRoot!.querySelector('.sort-label')?.textContent).toBe('Recently starred');
    expect(entries(el)[0].textContent).toContain('Mock excerpt 2');
    expect(entries(el)[0].getAttribute('href')).toBe(item(2).permalink);

    const more = [...el.shadowRoot!.querySelectorAll('sl-button')].find((b) =>
      b.textContent?.includes('Load more')
    ) as HTMLElement;
    more.click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(3), 'deduplicated by message ID');
    expect(String(apiFetch.mock.calls[1][0])).toContain('cursor=c1.next');
    expect(
      [...el.shadowRoot!.querySelectorAll('sl-button')].some((b) =>
        b.textContent?.includes('Load more')
      )
    ).toBe(false);
  });

  it('drops a deleted message at once, from any conversation, without a request', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [item(2), item(1)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    // The state manager wraps SSE payloads as { state, data }.
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-deleted', {
        detail: { data: { conversationKey: 'other', messageId: item(2).messageId } },
      })
    );
    await el.updateComplete;
    expect(entries(el)).toHaveLength(1);
    expect(entries(el)[0].textContent).toContain('Mock excerpt 1');
    expect(el.shadowRoot!.textContent).not.toContain('Mock excerpt 2');
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it('a page already in flight when a message is deleted cannot bring its row back', async () => {
    let release!: (r: Response) => void;
    apiFetch.mockImplementationOnce(() => new Promise<Response>((resolve) => (release = resolve)));
    const el = await mount();
    await openPanel(el);
    // The first page is in flight; the message is deleted meanwhile.
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-deleted', {
        detail: { data: { conversationKey: 'topic-1', messageId: item(2).messageId } },
      })
    );
    release(json({ items: [item(2), item(1)] }));
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    expect(el.shadowRoot!.textContent).not.toContain('Mock excerpt 2');
  });

  it('lists a DM conversation with its DM permalink', async () => {
    const dm = {
      ...item(3),
      conversationKey: 'dm:user:a:user:b',
      conversationKind: 'dm' as const,
      projectId: undefined,
      projectName: undefined,
      permalink: `/chat/dm/dm%3Auser%3Aa%3Auser%3Ab#msg-${item(3).messageId}`,
    };
    apiFetch.mockResolvedValueOnce(json({ items: [dm] }));
    const el = document.createElement('scion-chat-starred-list') as ScionChatStarredList;
    el.conversationKey = 'dm:user:a:user:b';
    document.body.appendChild(el);
    await el.updateComplete;
    await openPanel(el);
    expect(String(apiFetch.mock.calls[0][0])).toBe(
      '/api/v1/chat/stars?conversationKey=dm%3Auser%3Aa%3Auser%3Ab&limit=20'
    );
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    expect(entries(el)[0].getAttribute('href')).toBe(dm.permalink);
  });

  it('shows the empty state with the action hint', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot!.querySelector('.status')?.textContent).toContain(
        'No starred messages in this conversation'
      )
    );
  });

  it('shows a retryable error', async () => {
    apiFetch.mockResolvedValueOnce(json({}, 500)).mockResolvedValueOnce(json({ items: [item(1)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(el.shadowRoot!.querySelector('[role="alert"]')).not.toBeNull());
    (el.shadowRoot!.querySelector('[role="alert"] sl-button') as HTMLElement).click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
  });

  it('closes and fires starred-message-open when an entry is chosen', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [item(1)] }));
    const el = await mount();
    const opened = vi.fn();
    el.addEventListener('starred-message-open', opened);
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    entries(el)[0].dispatchEvent(
      new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 })
    );
    await el.updateComplete;
    expect(opened).toHaveBeenCalledTimes(1);
    expect((opened.mock.calls[0][0] as CustomEvent).detail).toEqual({
      messageId: item(1).messageId,
    });
    expect(el.shadowRoot!.querySelector('.panel')).toBeNull();
  });

  it('closes on Escape', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    const el = await mount();
    await openPanel(el);
    el.shadowRoot!.querySelector('.panel')!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })
    );
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.panel')).toBeNull();
    expect(button(el).getAttribute('aria-expanded')).toBe('false');
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(button(el)));
  });

  it('refetches while open on the SSE invalidation and on a local change here', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(1)] }))
      .mockResolvedValueOnce(json({ items: [item(2), item(1)] }))
      .mockResolvedValueOnce(json({ items: [item(2)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    chatStarsLocalChanges.dispatchEvent(
      new CustomEvent('changed', { detail: { conversationKey: 'other' } })
    );
    await new Promise((r) => setTimeout(r, 20));
    expect(apiFetch).toHaveBeenCalledTimes(2);
    chatStarsLocalChanges.dispatchEvent(
      new CustomEvent('changed', { detail: { conversationKey: 'topic-1' } })
    );
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
  });

  it('does not fetch on invalidations while closed', async () => {
    const el = await mount();
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await new Promise((r) => setTimeout(r, 10));
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el).toBeTruthy();
  });
});

/** A Load more / Retry button by its text. */
function slButton(el: ScionChatStarredList, text: string): HTMLElement | undefined {
  return [...el.shadowRoot!.querySelectorAll<HTMLElement>('sl-button')].find((b) =>
    b.textContent?.includes(text)
  );
}

/** Make the chat's menu-sheet breakpoint match (a phone) or not. */
function setNarrow(narrow: boolean): void {
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query: string) =>
      ({
        matches: narrow && query.includes('max-width'),
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }) as unknown as MediaQueryList
  );
}

describe('scion-chat-starred-list: final presentation', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    setNarrow(false);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders each entry as two compact lines with the saved date, author and full sent time', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [{ ...item(1), edited: true }] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    const entry = entries(el)[0];
    const time = entry.querySelector('time')!;
    expect(time.getAttribute('datetime')).toBe(item(1).starredAt);
    expect(entry.querySelector('.entry-sender')?.textContent).toBe('Example person');
    expect(entry.querySelector('.entry-excerpt')?.textContent).toBe('Mock excerpt 1');
    expect(entry.querySelector('.sr-only:last-child')?.textContent).toMatch(/^Sent .+, edited$/);
    expect(entry.getAttribute('title')).toMatch(/^Sent .+ by Example person\. Starred .+\.$/);
    // The dialog is named by its visible heading and is not modal on desktop.
    const panel = el.shadowRoot!.querySelector('.panel')!;
    const heading = el.shadowRoot!.getElementById(panel.getAttribute('aria-labelledby')!);
    expect(heading?.textContent).toBe('Starred messages');
    expect(panel.getAttribute('aria-modal')).toBe('false');
  });

  it('a reconcile reloads every loaded row and keeps them on screen meanwhile', async () => {
    const first = Array.from({ length: 20 }, (_, n) => item(40 - n));
    const second = Array.from({ length: 20 }, (_, n) => item(20 - n));
    let release: (r: Response) => void = () => {};
    apiFetch
      .mockResolvedValueOnce(json({ items: first, nextCursor: 'c1' }))
      .mockResolvedValueOnce(json({ items: second, nextCursor: 'c2' }))
      .mockImplementationOnce(() => new Promise<Response>((r) => (release = r)));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(20));
    slButton(el, 'Load more')!.click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(40));

    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(3));
    expect(String(apiFetch.mock.calls[2][0])).toBe(
      '/api/v1/chat/stars?conversationKey=topic-1&limit=40'
    );
    // Still showing the rows while the reload runs, marked busy.
    expect(entries(el)).toHaveLength(40);
    expect(el.shadowRoot!.querySelector('ul')?.getAttribute('aria-busy')).toBe('true');
    // Busy (announced by the list's aria-busy), but Load more is not disabled:
    // a keyboard user on it keeps focus.
    expect(slButton(el, 'Load more')?.hasAttribute('disabled')).toBe(false);

    release(json({ items: [item(99), ...first, ...second].slice(0, 40), nextCursor: 'c3' }));
    await vi.waitFor(() => expect(entries(el)[0].textContent).toContain('Mock excerpt 99'));
    expect(entries(el)).toHaveLength(40);
    expect(el.shadowRoot!.querySelector('ul')?.getAttribute('aria-busy')).toBe('false');
  });

  it('stops at a short page: the rest of a hub scan runs only on Load more', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [], nextCursor: 'scan.1' }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot!.querySelector('.status')?.textContent).toContain(
        'No starred messages found yet'
      )
    );
    expect(apiFetch).toHaveBeenCalledTimes(1);
    // Not the empty-collection text: there may be more.
    expect(el.shadowRoot!.textContent).not.toContain('No starred messages in this conversation');
    apiFetch.mockResolvedValueOnce(json({ items: [item(1)] }));
    slButton(el, 'Load more')!.click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    expect(String(apiFetch.mock.calls[1][0])).toContain('cursor=scan.1');
  });

  it('a failed reconcile keeps the rows and offers a retry; a failed Load more offers its own', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(2)], nextCursor: 'c1' }))
      .mockResolvedValueOnce(json({}, 503));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    slButton(el, 'Load more')!.click();
    await vi.waitFor(() => expect(slButton(el, 'Retry loading more')).toBeDefined());
    expect(entries(el)).toHaveLength(1);

    apiFetch.mockResolvedValueOnce(json({}, 503));
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() =>
      expect(el.shadowRoot!.textContent).toContain('Could not refresh starred messages')
    );
    expect(entries(el)).toHaveLength(1);
    apiFetch.mockResolvedValueOnce(json({ items: [item(3), item(2)] }));
    slButton(el, 'Retry')!.click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    expect(el.shadowRoot!.querySelector('[role="alert"]')).toBeNull();
  });

  it('an invalidation during Load more drops that page and reloads instead', async () => {
    let releaseMore: (r: Response) => void = () => {};
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(2)], nextCursor: 'c1' }))
      .mockImplementationOnce(
        (_url: string, init?: RequestInit) =>
          new Promise<Response>((r, reject) => {
            releaseMore = r;
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('', 'AbortError'))
            );
          })
      )
      .mockResolvedValueOnce(json({ items: [item(5), item(2)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    slButton(el, 'Load more')!.click();
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(2));
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    releaseMore(json({ items: [item(1)] }));
    await new Promise((r) => setTimeout(r, 20));
    expect(entries(el).map((a) => a.textContent?.includes('Mock excerpt 1'))).not.toContain(true);
  });

  it('a conversation switch drops a request still in flight', async () => {
    let release: (r: Response) => void = () => {};
    apiFetch.mockImplementationOnce(() => new Promise<Response>((r) => (release = r)));
    const el = await mount();
    await openPanel(el);
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    release(json({ items: [item(1)] }));
    await new Promise((r) => setTimeout(r, 20));
    expect(el.isOpen).toBe(false);
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    await openPanel(el);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(2));
    expect(String(apiFetch.mock.calls[1][0])).toContain('conversationKey=topic-2');
    expect(entries(el)).toHaveLength(0);
  });

  it('moves keyboard focus to the first new row after Load more', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(3), item(2)], nextCursor: 'c1' }))
      .mockResolvedValueOnce(json({ items: [item(1)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    const more = slButton(el, 'Load more')!;
    more.focus();
    more.click();
    await vi.waitFor(() => expect(entries(el)).toHaveLength(3));
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(entries(el)[2]));
  });

  it('keeps focus in the list when the focused row disappears', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(3), item(2), item(1)] }))
      .mockResolvedValueOnce(json({ items: [item(3), item(1)] }));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(3));
    entries(el)[1].focus();
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await vi.waitFor(() => expect(entries(el)).toHaveLength(2));
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(entries(el)[1]));
  });

  it('closes when focus moves outside the nonmodal panel, without taking focus back', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [item(1)] }));
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    entries(el)[0].dispatchEvent(
      new FocusEvent('focusout', { bubbles: true, composed: true, relatedTarget: outside })
    );
    await el.updateComplete;
    expect(el.isOpen).toBe(false);
    expect(el.shadowRoot!.activeElement).not.toBe(button(el));
  });

  it('reconciles once a minute while open and visible, and never while closed', async () => {
    vi.useFakeTimers();
    apiFetch.mockResolvedValue(json({ items: [item(1)] }));
    const el = await mount();
    await openPanel(el);
    await vi.advanceTimersByTimeAsync(10);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(59_000);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1_100);
    expect(apiFetch).toHaveBeenCalledTimes(2);
    button(el).click();
    await el.updateComplete;
    await vi.advanceTimersByTimeAsync(180_000);
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

  it('on a narrow screen opens a modal sheet with a heading and Close, and returns focus on close', async () => {
    setNarrow(true);
    apiFetch.mockResolvedValueOnce(json({ items: [item(1)] }));
    const el = await mount();
    await openPanel(el);
    const dialog = el.shadowRoot!.querySelector<HTMLDialogElement>('dialog.sheet')!;
    expect(dialog).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.panel')).toBeNull();
    expect(dialog.open).toBe(true);
    expect(
      el.shadowRoot!.getElementById(dialog.getAttribute('aria-labelledby')!)?.textContent
    ).toBe('Starred messages');
    expect(button(el).getAttribute('aria-controls')).toBe(dialog.id);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    const close = el.shadowRoot!.querySelector<HTMLButtonElement>('button.close')!;
    expect(close.getAttribute('aria-label')).toBe('Close starred messages');
    close.click();
    await el.updateComplete;
    expect(el.isOpen).toBe(false);
    expect(el.shadowRoot!.querySelector('dialog.sheet')).toBeNull();
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(button(el)));
  });

  it('the sheet closes on Escape (dialog cancel) and keeps touches to itself', async () => {
    setNarrow(true);
    apiFetch.mockResolvedValueOnce(json({ items: [item(1)] }));
    const el = await mount();
    const swipe = vi.fn();
    document.body.addEventListener('touchmove', swipe);
    await openPanel(el);
    await vi.waitFor(() => expect(entries(el)).toHaveLength(1));
    entries(el)[0].dispatchEvent(new Event('touchmove', { bubbles: true, composed: true }));
    expect(swipe).not.toHaveBeenCalled();
    const dialog = el.shadowRoot!.querySelector<HTMLDialogElement>('dialog.sheet')!;
    dialog.dispatchEvent(new Event('cancel', { cancelable: true }));
    await el.updateComplete;
    expect(el.isOpen).toBe(false);
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(button(el)));
  });

  it('Back/Forward closes the list', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    const el = await mount();
    await openPanel(el);
    window.dispatchEvent(new PopStateEvent('popstate'));
    await el.updateComplete;
    expect(el.isOpen).toBe(false);
  });

  it('names its panel in aria-controls only while open', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    const el = await mount();
    expect(button(el).hasAttribute('aria-controls')).toBe(false);
    await openPanel(el);
    const panel = el.shadowRoot!.querySelector('.panel')!;
    expect(button(el).getAttribute('aria-controls')).toBe(panel.id);
  });

  it('Escape with focus on the star closes the open list', async () => {
    apiFetch.mockResolvedValueOnce(json({ items: [] }));
    const el = await mount();
    await openPanel(el);
    button(el).focus();
    button(el).dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Escape',
        bubbles: true,
        composed: true,
        cancelable: true,
      })
    );
    await el.updateComplete;
    expect(el.isOpen).toBe(false);
    await vi.waitFor(() => expect(el.shadowRoot!.activeElement).toBe(button(el)));
  });

  it('focusTrigger puts focus back on the star', async () => {
    const el = await mount();
    el.focusTrigger();
    expect(el.shadowRoot!.activeElement).toBe(button(el));
  });

  it('a background reconcile keeps keyboard focus on Load more', async () => {
    apiFetch
      .mockResolvedValueOnce(json({ items: [item(2)], nextCursor: 'c1' }))
      .mockImplementationOnce(() => new Promise<Response>(() => {}));
    const el = await mount();
    await openPanel(el);
    await vi.waitFor(() => expect(slButton(el, 'Load more')).toBeDefined());
    const more = slButton(el, 'Load more')!;
    more.focus();
    fakeStateManager.dispatchEvent(new CustomEvent('chat-stars-updated'));
    await el.updateComplete;
    // The reconcile really started (it is held open by the second request).
    expect(el.shadowRoot!.querySelector('ul[aria-busy="true"]')).not.toBeNull();
    expect(el.shadowRoot!.activeElement).toBe(more);
    expect(more.hasAttribute('disabled')).toBe(false);
  });
});
