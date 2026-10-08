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
 * Both trays announce their list size through TRAY_COUNT_EVENT whenever the
 * list changes: when a fetch is applied, when the list is cleared on a user
 * change or sign-out, and when an item is acknowledged or marked read. A
 * response dropped because it belongs to a previous user announces nothing.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

interface Pending {
  url: string;
  method: string;
  resolve: (body: unknown) => void;
}

const pending: Pending[] = [];

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(
    (url: string, init?: RequestInit) =>
      new Promise<Response>((resolve) => {
        pending.push({
          url,
          method: init?.method ?? 'GET',
          resolve: (body) => resolve(new Response(JSON.stringify(body), { status: 200 })),
        });
      })
  ),
}));

vi.mock('../../client/state.js', () => import('../../client/__fixtures__/state-stub.js'));

const { TRAY_COUNT_EVENT } = await import('../../client/tray-count-events.js');

function user(id: string): any {
  return { id, email: `${id}@example.com`, displayName: id };
}

/** Lets pending promise chains (fetch, json, Lit update) settle. */
async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
}

interface TrayCase {
  tag: string;
  source: 'inbox' | 'notifications';
  /** A list response holding the given item ids. */
  body: (...ids: string[]) => unknown;
  /** Starts a list fetch, as a poll or a real-time event does. */
  refetch: (tray: any) => Promise<void>;
  removeOne: (tray: any, id: string) => Promise<void>;
  removeAll: (tray: any) => Promise<void>;
  /** Selects the text of each rendered list row. */
  rowText: string;
}

const CASES: TrayCase[] = [
  {
    tag: 'scion-inbox-tray',
    source: 'inbox',
    body: (...ids) => ({
      items: ids.map((id) => ({
        id,
        sender: 'agent:helper',
        msg: id,
        type: 'instruction',
        createdAt: new Date().toISOString(),
      })),
    }),
    refetch: (tray) => tray.fetchMessages(),
    removeOne: (tray, id) => tray.markOne(id),
    removeAll: (tray) => tray.markAll(),
    rowText: '.msg-text',
  },
  {
    tag: 'scion-notification-tray',
    source: 'notifications',
    body: (...ids) =>
      ids.map((id) => ({
        id,
        agentId: 'agent-1',
        status: 'COMPLETED',
        message: id,
        createdAt: new Date().toISOString(),
      })),
    refetch: (tray) => tray.fetchNotifications(),
    removeOne: (tray, id) => tray.ackOne(id),
    removeAll: (tray) => tray.ackAll(),
    rowText: '.notif-message',
  },
];

describe.each(CASES)('$tag: count events', (c) => {
  /** Every TRAY_COUNT_EVENT seen at the document. */
  let events: CustomEvent[] = [];
  const record = (e: Event): void => {
    events.push(e as CustomEvent);
  };

  beforeAll(async () => {
    await import('./inbox-tray.js');
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    pending.length = 0;
    events = [];
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    document.addEventListener(TRAY_COUNT_EVENT, record);
  });

  afterEach(() => {
    document.removeEventListener(TRAY_COUNT_EVENT, record);
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  function counts(): number[] {
    return events.map((e) => e.detail.count);
  }

  /** Answers every outstanding list request with body. */
  async function answerLists(body: unknown): Promise<void> {
    const lists = pending.filter((p) => p.method === 'GET');
    expect(lists.length).toBeGreaterThan(0);
    for (const req of lists) pending.splice(pending.indexOf(req), 1);
    for (const req of lists) req.resolve(body);
    await flush();
  }

  /** Answers every outstanding action request. */
  async function answerActions(): Promise<void> {
    const posts = pending.filter((p) => p.method === 'POST');
    expect(posts.length).toBeGreaterThan(0);
    for (const req of posts) pending.splice(pending.indexOf(req), 1);
    for (const req of posts) req.resolve({});
    await flush();
  }

  async function mount(id: string): Promise<any> {
    const tray: any = document.createElement(c.tag);
    tray.user = user(id);
    document.body.appendChild(tray);
    await tray.updateComplete;
    await flush();
    return tray;
  }

  async function setUser(tray: any, u: any): Promise<void> {
    tray.user = u;
    await tray.updateComplete;
    await flush();
  }

  it('dispatches a bubbling, composed event from the tray with its source', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));

    expect(events.length).toBeGreaterThan(0);
    for (const e of events) {
      expect(e.bubbles).toBe(true);
      expect(e.composed).toBe(true);
      expect(e.target).toBe(tray);
      expect(e.detail.source).toBe(c.source);
    }
  });

  it('dispatches the list size when a fetch is applied', async () => {
    await mount('u1');
    events = [];
    await answerLists(c.body('a', 'b'));
    expect(counts()).toEqual([2]);
  });

  it('dispatches zero when the list is cleared on a user change and on sign-out', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));

    events = [];
    await setUser(tray, user('u2'));
    expect(counts()).toEqual([0]);

    await answerLists(c.body('c'));
    expect(counts()).toEqual([0, 1]);

    events = [];
    await setUser(tray, null);
    expect(counts().length).toBeGreaterThan(0);
    expect(counts().every((n) => n === 0)).toBe(true);
  });

  it('dispatches the new size after a single item is removed', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));

    events = [];
    const done = c.removeOne(tray, 'a');
    await answerActions();
    await done;
    await tray.updateComplete;
    expect(counts()).toEqual([1]);
  });

  it('dispatches zero after all items are removed', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));

    events = [];
    const done = c.removeAll(tray);
    await answerActions();
    await done;
    await tray.updateComplete;
    expect(counts()).toEqual([0]);
  });

  it('dispatches nothing for a dropped response for a previous user', async () => {
    const tray = await mount('u1');
    const stale = pending.filter((p) => p.method === 'GET');
    expect(stale.length).toBeGreaterThan(0);
    pending.length = 0;

    await setUser(tray, user('u2'));
    events = [];

    for (const req of stale) req.resolve(c.body('old-1', 'old-2', 'old-3'));
    await flush();
    await tray.updateComplete;
    expect(events).toEqual([]);

    await answerLists(c.body('c'));
    expect(counts()).toEqual([1]);
  });

  it('dispatches nothing for a dropped action response for a previous user', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));

    const done = c.removeAll(tray);
    await flush();
    const posts = pending.filter((p) => p.method === 'POST');
    for (const req of posts) pending.splice(pending.indexOf(req), 1);
    await setUser(tray, user('u2'));

    events = [];
    for (const req of posts) req.resolve({});
    await done;
    await flush();
    await tray.updateComplete;
    expect(events).toEqual([]);
  });

  it('dispatches the size and shows the new rows when a fetch returns the same number of different items', async () => {
    const tray = await mount('u1');
    await answerLists(c.body('a', 'b'));
    events = [];

    void c.refetch(tray);
    await answerLists(c.body('c', 'd'));
    expect(counts()).toEqual([2]);

    tray.open = true;
    await tray.updateComplete;
    const rows = [...tray.shadowRoot.querySelectorAll(c.rowText)].map((r: Element) =>
      r.textContent?.trim()
    );
    expect(rows).toEqual(['c', 'd']);
    expect(rows).not.toContain('a');
  });
});
