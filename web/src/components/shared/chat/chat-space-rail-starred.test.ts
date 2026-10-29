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
 * The Starred rail filter (web.chat_starred_messages): the third option of
 * the rail's filter control, fed by GET /api/v1/chat/starred-conversations
 * (never by the loaded All/Unread rail), grouped into a DMs section and
 * project headings, with Load more, its distinct states and selection into
 * the starred-only center.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { stateManager } from '../../../client/state.js';
import type { StarredConversationRow } from '../../../client/chat-starred-rail.js';
import type { ChatSpace } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  extractApiError: vi.fn((_res: Response, fallback: string) => Promise.resolve(fallback)),
}));

const apiFetchMock = vi.mocked(apiFetch);
const ME = '00000000-0000-4000-8000-0000000000aa';
const PEER = '00000000-0000-4000-8000-0000000000bb';
const AGENT = '00000000-0000-4000-8000-0000000000cc';

const SPACE: ChatSpace = {
  projectId: 'proj-1',
  projectName: 'Rail Project',
  projectSlug: 'rail-project',
  unreadCount: 0,
  hasUnreadMention: false,
};

const ROWS: StarredConversationRow[] = [
  {
    conversationKey: 'topic-b',
    conversationKind: 'thread',
    conversationName: 'beta',
    lastActivityAt: '2026-10-22T10:00:00Z',
    projectId: 'proj-1',
    projectName: 'Rail Project',
  },
  {
    conversationKey: 'topic-a',
    conversationKind: 'thread',
    conversationName: 'Alpha',
    lastActivityAt: '2026-10-22T09:00:00Z',
    projectId: 'proj-1',
    projectName: 'Rail Project',
  },
  {
    conversationKey: `dm:user:${ME}:user:${PEER}`,
    conversationKind: 'dm',
    conversationName: 'Mia',
    lastActivityAt: '2026-10-22T08:00:00Z',
  },
  {
    conversationKey: `dm:agent:${AGENT}:user:${ME}`,
    conversationKind: 'dm',
    conversationName: 'Zed agent',
    projectId: 'proj-1',
    projectName: 'Rail Project',
  },
];

/** Starred-conversation answers in order; other routes answer `{}`. */
let starredPages: Array<{ items: StarredConversationRow[]; nextCursor?: string } | Error> = [];
const starredCalls: URLSearchParams[] = [];

function urlOf(input: RequestInfo | URL): string {
  return typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
}

function respond(input: RequestInfo | URL): Promise<Response> {
  const url = urlOf(input);
  if (url.startsWith('/api/v1/chat/starred-conversations')) {
    starredCalls.push(new URL(url, 'http://x').searchParams);
    const next = starredPages.shift() ?? { items: [] };
    if (next instanceof Error) return Promise.resolve(new Response('{}', { status: 500 }));
    return Promise.resolve(new Response(JSON.stringify(next), { status: 200 }));
  }
  return Promise.resolve(new Response('{}', { status: 200 }));
}

function setFlag(on: boolean): void {
  (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
    'web.chat_starred_messages': on,
  };
}

async function settle(el: any): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  el.currentUserId = ME;
  document.body.appendChild(el);
  await settle(el);
  el.spaces = [SPACE];
  el.threadsBySpace = new Map([[SPACE.projectId, []]]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  await settle(el);
  return el;
}

function filterButtons(el: any): HTMLButtonElement[] {
  return Array.from(el.shadowRoot.querySelectorAll('.filter-toggle button'));
}

function button(el: any, label: string): HTMLButtonElement {
  const b = filterButtons(el).find((x) => x.textContent?.trim() === label);
  if (!b) throw new Error(`no ${label} button`);
  return b;
}

function rowNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('[data-starred-key] .thread-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function headings(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.rail-body .space-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function text(el: any): string {
  return (el.shadowRoot.querySelector('.rail-body')?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  setFlag(true);
  localStorage.clear();
  starredPages = [];
  starredCalls.length = 0;
  apiFetchMock.mockImplementation(respond as typeof apiFetch);
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
  localStorage.clear();
  setFlag(false);
});

describe('space rail — Starred filter control', () => {
  it('offers All, Unread and Starred as one labelled group with pressed state', async () => {
    const el = await mount();
    const group = el.shadowRoot.querySelector('.filter-toggle');
    expect(group.getAttribute('role')).toBe('group');
    expect(group.getAttribute('aria-label')).toBeTruthy();
    expect(filterButtons(el).map((b) => b.textContent?.trim())).toEqual([
      'All',
      'Unread',
      'Starred',
    ]);
    expect(button(el, 'All').getAttribute('aria-pressed')).toBe('true');
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(button(el, 'Starred').getAttribute('aria-pressed')).toBe('true');
    expect(button(el, 'All').getAttribute('aria-pressed')).toBe('false');
  });

  it('has no Starred option while the experiment is off, and ignores a stored Starred filter', async () => {
    setFlag(false);
    localStorage.setItem('scion-chat-space-filter', 'starred');
    const el = await mount();
    expect(filterButtons(el).map((b) => b.textContent?.trim())).toEqual(['All', 'Unread']);
    expect(el.spaceFilter).toBe('all');
    expect(starredCalls).toHaveLength(0);
  });

  it('restores a stored Starred filter and persists only the filter', async () => {
    localStorage.setItem('scion-chat-space-filter', 'starred');
    starredPages = [{ items: ROWS }];
    const el = await mount();
    expect(el.spaceFilter).toBe('starred');
    expect(rowNames(el)).toContain('beta');
    button(el, 'All').click();
    await settle(el);
    expect(localStorage.getItem('scion-chat-space-filter')).toBeNull();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(localStorage.getItem('scion-chat-space-filter')).toBe('starred');
    expect(Object.keys(localStorage)).toEqual(['scion-chat-space-filter']);
  });

  it('tells the page about a picked filter (rail-filter-change)', async () => {
    const el = await mount();
    const seen: string[] = [];
    el.addEventListener('rail-filter-change', (e: CustomEvent) => seen.push(e.detail.filter));
    starredPages = [{ items: [] }];
    button(el, 'Starred').click();
    button(el, 'Unread').click();
    button(el, 'All').click();
    await settle(el);
    expect(seen).toEqual(['starred', 'unread', 'all']);
  });
});

describe('space rail — Starred rows', () => {
  it('lists the endpoint rows, not the loaded rail, in a DMs section and project headings', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(starredCalls).toHaveLength(1);
    expect(starredCalls[0].get('sort')).toBe('activity');
    expect(starredCalls[0].get('limit')).toBe('30');
    expect(headings(el)).toEqual(['Direct messages', 'Rail Project']);
    // Recent: newest first within each group; agent DMs are in the DMs section.
    expect(rowNames(el)).toEqual(['Mia', 'Zed agent', 'beta', 'Alpha']);
  });

  it('uses Alphabetical from the shared sort preference, and Recent for Custom', async () => {
    const el = await mount();
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await settle(el);
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(starredCalls[0].get('sort')).toBe('alpha');
    expect(rowNames(el)).toEqual(['Mia', 'Zed agent', 'Alpha', 'beta']);

    starredPages = [{ items: ROWS }];
    el.prefs = { ...el.prefs, spaceSortMode: 'custom' };
    await settle(el);
    expect(starredCalls[1].get('sort')).toBe('activity');
    expect(el.prefs.spaceSortMode, 'the custom setting is kept').toBe('custom');
  });

  it('shows No starred conversations yet with the message action for an empty collection', async () => {
    const el = await mount();
    starredPages = [{ items: [] }];
    button(el, 'Starred').click();
    await settle(el);
    expect(text(el)).toContain('No starred conversations yet');
    expect(text(el)).toContain('Star message');
    expect(el.shadowRoot.querySelector('.starred-more')).toBeNull();
  });

  it('keeps a retryable error apart from the empty state', async () => {
    const el = await mount();
    starredPages = [new Error('down')];
    button(el, 'Starred').click();
    await settle(el);
    expect(text(el)).toContain('Couldn’t load starred conversations');
    expect(text(el)).not.toContain('No starred conversations yet');
    starredPages = [{ items: ROWS }];
    (el.shadowRoot.querySelector('.starred-state sl-button') as HTMLElement).click();
    await settle(el);
    expect(rowNames(el)).toContain('beta');
  });

  it('Load more conversations merges rows into an existing heading without a duplicate', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS.slice(0, 1), nextCursor: 'c1' }];
    button(el, 'Starred').click();
    await settle(el);
    const more = el.shadowRoot.querySelector('.starred-more sl-button') as HTMLElement;
    expect(more.textContent?.trim()).toBe('Load more conversations');
    starredPages = [{ items: [ROWS[0], ROWS[1]] }];
    more.click();
    await settle(el);
    expect(starredCalls[1].get('cursor')).toBe('c1');
    expect(headings(el)).toEqual(['Rail Project']);
    expect(rowNames(el)).toEqual(['beta', 'Alpha']);
    expect(el.shadowRoot.querySelector('.starred-more')).toBeNull();
  });

  it('a page with no visible rows but a cursor offers Load more, not the empty state', async () => {
    const el = await mount();
    starredPages = [{ items: [], nextCursor: 'c1' }];
    button(el, 'Starred').click();
    await settle(el);
    expect(text(el)).not.toContain('No starred conversations yet');
    expect(el.shadowRoot.querySelector('.starred-more sl-button')).not.toBeNull();
  });

  it('refetches on a stars invalidation: a conversation that lost its last star leaves', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    starredPages = [{ items: ROWS.slice(1) }];
    stateManager.dispatchEvent(new Event('chat-stars-updated'));
    await settle(el);
    expect(rowNames(el)).not.toContain('beta');
  });

  it('a refresh (pull or menu) refetches the endpoint', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    starredPages = [{ items: ROWS }];
    await el.refresh({ quiet: true });
    await settle(el);
    expect(starredCalls.length).toBeGreaterThanOrEqual(2);
  });
});

describe('space rail — selecting a Starred row', () => {
  it('opens a thread or DM in the starred-only center', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    const threads: any[] = [];
    const dms: any[] = [];
    el.addEventListener('thread-select', (e: CustomEvent) => threads.push(e.detail));
    el.addEventListener('dm-select', (e: CustomEvent) => dms.push(e.detail));
    (el.shadowRoot.querySelector('[data-starred-key="topic-a"]') as HTMLElement).click();
    (
      el.shadowRoot.querySelector(`[data-starred-key="dm:user:${ME}:user:${PEER}"]`) as HTMLElement
    ).click();
    expect(threads).toEqual([
      {
        conversationKey: 'topic-a',
        projectId: 'proj-1',
        projectSlug: 'rail-project',
        threadName: 'Alpha',
        defaultAgent: '',
        messageFilter: 'starred',
      },
    ]);
    expect(dms).toEqual([
      {
        conversationKey: `dm:user:${ME}:user:${PEER}`,
        peerId: PEER,
        peerKind: 'user',
        displayName: 'Mia',
        messageFilter: 'starred',
      },
    ]);
  });

  it('opens a row from the keyboard', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    const dms: any[] = [];
    el.addEventListener('dm-select', (e: CustomEvent) => dms.push(e.detail));
    const row = el.shadowRoot.querySelector(
      `[data-starred-key="dm:agent:${AGENT}:user:${ME}"]`
    ) as HTMLElement;
    expect(row.getAttribute('tabindex')).toBe('0');
    row.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(dms[0]).toMatchObject({ peerId: AGENT, peerKind: 'agent', messageFilter: 'starred' });
  });
});

describe('space rail — Starred refetch triggers and the reconcile poll', () => {
  function setVisibility(state: 'visible' | 'hidden'): void {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
  }

  afterEach(() => {
    vi.useRealTimers();
    setVisibility('visible');
  });

  it('polls once per 60 s while visible, never while hidden, and stops after All and on disconnect', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    setVisibility('visible');
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    const base = starredCalls.length;

    starredPages = [{ items: ROWS }];
    vi.advanceTimersByTime(60_000);
    await settle(el);
    expect(starredCalls.length).toBe(base + 1);

    setVisibility('hidden');
    vi.advanceTimersByTime(120_000);
    await settle(el);
    expect(starredCalls.length).toBe(base + 1);

    setVisibility('visible');
    button(el, 'All').click();
    await settle(el);
    vi.advanceTimersByTime(180_000);
    await settle(el);
    expect(starredCalls.length).toBe(base + 1);

    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    const restarted = starredCalls.length;
    el.remove();
    vi.advanceTimersByTime(180_000);
    await settle(el);
    expect(starredCalls.length).toBe(restarted);
  });

  it('reconnect and a return to the tab each refetch once; focus alone does not', async () => {
    setVisibility('visible');
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    const base = starredCalls.length;

    starredPages = [{ items: ROWS }];
    stateManager.dispatchEvent(new Event('connected'));
    await settle(el);
    expect(starredCalls.length).toBe(base + 1);

    starredPages = [{ items: ROWS }];
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('focus'));
    await settle(el);
    expect(starredCalls.length).toBe(base + 2);
  });

  it('no trigger fetches under All or Unread', async () => {
    const el = await mount();
    stateManager.dispatchEvent(new Event('chat-stars-updated'));
    stateManager.dispatchEvent(new Event('connected'));
    document.dispatchEvent(new Event('visibilitychange'));
    await settle(el);
    expect(starredCalls).toHaveLength(0);
  });
});

describe('space rail — Starred and the experiment', () => {
  it('a sort change shows loading, not the empty collection, until the new page arrives', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    apiFetchMock.mockImplementation(((input: RequestInfo | URL) =>
      urlOf(input).startsWith('/api/v1/chat/starred-conversations')
        ? gate.then(() => respond(input))
        : respond(input)) as typeof apiFetch);
    starredPages = [{ items: ROWS }];
    el.prefs = { ...el.prefs, spaceSortMode: 'alpha', threadSortMode: 'alpha' };
    await settle(el);
    expect(text(el)).not.toContain('No starred conversations yet');
    expect(el.shadowRoot.querySelector('.rail-body sl-spinner')).not.toBeNull();
    release();
    await settle(el);
    expect(rowNames(el)).toEqual(['Mia', 'Zed agent', 'Alpha', 'beta']);
  });

  it('leaves Starred and stops polling when the hub answers 404, keeping the stored filter', async () => {
    localStorage.setItem('scion-chat-space-filter', 'starred');
    starredPages = [{ items: ROWS }];
    const el = await mount();
    expect(el.spaceFilter).toBe('starred');
    apiFetchMock.mockImplementation(((input: RequestInfo | URL) =>
      urlOf(input).startsWith('/api/v1/chat/starred-conversations')
        ? Promise.resolve(new Response('{}', { status: 404 }))
        : respond(input)) as typeof apiFetch);
    await el.refresh({ quiet: true });
    await settle(el);
    expect(el.spaceFilter).toBe('all');
    expect(el._starredPoll).toBeNull();
    expect(localStorage.getItem('scion-chat-space-filter')).toBe('starred');
  });

  it('leaves Starred when the experiment is turned off in the browser', async () => {
    starredPages = [{ items: ROWS }];
    const el = await mount();
    button(el, 'Starred').click();
    await settle(el);
    setFlag(false);
    el.requestUpdate();
    await settle(el);
    expect(el.spaceFilter).toBe('all');
    expect(el._starredPoll).toBeNull();
  });

  it('a flag change with the experiment off leaves Starred at once', async () => {
    const { setFeatureFlag } = await import('../../../utils/feature-flags.js');
    starredPages = [{ items: ROWS }];
    const el = await mount();
    button(el, 'Starred').click();
    await settle(el);
    setFeatureFlag('web.chat_starred_messages', false);
    await settle(el);
    expect(el.spaceFilter).toBe('all');
    expect(filterButtons(el).map((b) => b.textContent?.trim())).toEqual(['All', 'Unread']);
  });

  it('a page-driven change to All keeps the stored Starred preference', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(localStorage.getItem('scion-chat-space-filter')).toBe('starred');
    el.selectedKey = 'topic-a';
    el.centerFilter = '';
    await settle(el);
    expect(el.spaceFilter).toBe('all');
    expect(localStorage.getItem('scion-chat-space-filter')).toBe('starred');
  });
});

describe('space rail — the filter follows the open center', () => {
  it('an open starred-only center shows Starred; full context returns to All', async () => {
    const el = await mount();
    starredPages = [{ items: ROWS }, { items: ROWS }];
    el.selectedKey = 'topic-a';
    el.centerFilter = 'starred';
    await settle(el);
    expect(el.spaceFilter).toBe('starred');

    // Jump to message / Show all: the page clears the center filter.
    el.centerFilter = '';
    await settle(el);
    expect(el.spaceFilter).toBe('all');
  });

  it('a user pick of Starred is not undone before the page applies it', async () => {
    const el = await mount();
    el.selectedKey = 'topic-a';
    await settle(el);
    starredPages = [{ items: ROWS }];
    button(el, 'Starred').click();
    await settle(el);
    expect(el.spaceFilter).toBe('starred');
  });

  it('Unread and All keep working as before (regression)', async () => {
    const el = await mount();
    button(el, 'Unread').click();
    await settle(el);
    expect(el.spaceFilter).toBe('unread');
    expect(text(el)).toContain('All caught up!');
    button(el, 'All').click();
    await settle(el);
    expect(el.spaceFilter).toBe('all');
    expect(starredCalls).toHaveLength(0);
  });
});
