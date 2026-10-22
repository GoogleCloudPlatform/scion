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

function button(el: ScionChatStarredList): HTMLElement {
  return el.shadowRoot!.querySelector('sl-icon-button')!;
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
    expect(b.getAttribute('label')).toBe('Starred messages in this conversation');
    expect(b.getAttribute('aria-expanded')).toBe('false');
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
