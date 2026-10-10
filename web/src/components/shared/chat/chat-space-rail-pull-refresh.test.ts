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
 * Pull-to-refresh on the space rail: a pull at the top of the rail body, in
 * either the All or the Unread view, reloads the spaces fresh (never a shared
 * earlier response), asks the page to refresh its own data through
 * `rail-refresh`, and shows a spinner until all of it is done.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { showToast } from '../../../utils/toast.js';
import { PULL_THRESHOLD_PX } from './pull-to-refresh.js';
import { chatSpacesLoad } from '../../../client/chat-list-cache.js';
import type { RailRefreshDetail } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

const apiFetchMock = vi.mocked(apiFetch);

function spacesCalls(): number {
  return apiFetchMock.mock.calls.filter(([url]) => String(url) === '/api/v1/chat/spaces').length;
}

function touch(target: EventTarget, type: string, y: number | null): void {
  const e = new Event(type, { bubbles: true, cancelable: true, composed: true });
  Object.defineProperty(e, 'touches', {
    value: y === null ? [] : [{ clientX: 0, clientY: y, identifier: 0 }],
  });
  target.dispatchEvent(e);
}

function pull(el: any): void {
  const body = el.shadowRoot.querySelector('.rail-body') as HTMLElement;
  const far = PULL_THRESHOLD_PX * 2 + 10;
  touch(body, 'touchstart', 0);
  touch(body, 'touchmove', far / 2);
  touch(body, 'touchmove', far);
  touch(body, 'touchend', null);
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function mount(): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await settle();
  await el.updateComplete;
  return el;
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  localStorage.clear();
  // Page-wide shared load: one test's spaces must not answer the next.
  chatSpacesLoad.invalidate();
  apiFetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify({ spaces: [] }), { status: 200 }))
  );
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
  localStorage.clear();
});

describe('space rail — pull to refresh', () => {
  for (const filter of ['all', 'unread'] as const) {
    it(`a pull in the ${filter} view reloads the spaces and asks the page to refresh`, async () => {
      if (filter === 'unread') localStorage.setItem('scion-chat-space-filter', 'unread');
      const el = await mount();
      expect(el.spaceFilter).toBe(filter);
      const before = spacesCalls();
      const events: CustomEvent<RailRefreshDetail>[] = [];
      el.addEventListener('rail-refresh', (e) => events.push(e as CustomEvent));

      pull(el);
      await settle();

      expect(events).toHaveLength(1);
      expect(spacesCalls()).toBe(before + 1);
    });
  }

  it('shows a spinner until the page’s own refresh finishes too', async () => {
    const el = await mount();
    let finish!: () => void;
    el.addEventListener('rail-refresh', (e) => {
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });

    pull(el);
    await settle();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).not.toBeNull();

    finish();
    await settle();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).toBeNull();
    expect((el.shadowRoot.querySelector('.pull-indicator') as HTMLElement).style.height).toBe(
      '0px'
    );
  });

  it('refresh() calls made during a refresh join it (single-flight)', async () => {
    const el = await mount();
    let finish!: () => void;
    let events = 0;
    el.addEventListener('rail-refresh', (e) => {
      events++;
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });
    const before = spacesCalls();

    const first = el.refresh();
    const second = el.refresh();
    pull(el);
    await settle();
    expect(events).toBe(1);
    expect(spacesCalls()).toBe(before + 1);

    finish();
    await Promise.all([first, second]);
  });

  it('a short pull does nothing', async () => {
    const el = await mount();
    const before = spacesCalls();
    const body = el.shadowRoot.querySelector('.rail-body') as HTMLElement;
    touch(body, 'touchstart', 0);
    touch(body, 'touchmove', PULL_THRESHOLD_PX);
    touch(body, 'touchend', null);
    await settle();
    expect(spacesCalls()).toBe(before);
  });

  it('a quiet refresh shows no indicator', async () => {
    const el = await mount();
    let finish!: () => void;
    el.addEventListener('rail-refresh', (e: Event) => {
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });
    const before = spacesCalls();
    const done = el.refresh({ quiet: true });
    await settle();
    await el.updateComplete;
    expect(spacesCalls()).toBe(before + 1);
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).toBeNull();
    expect((el.shadowRoot.querySelector('.pull-indicator') as HTMLElement).style.height).toBe(
      '0px'
    );
    finish();
    await done;
  });

  it('keeps the spinner until the thread lists on screen have refetched', async () => {
    const space = {
      projectId: 'p1',
      projectName: 'One',
      projectSlug: 'one',
      unreadCount: 0,
      hasUnreadMention: false,
    };
    const threadResponses: Array<() => void> = [];
    apiFetchMock.mockImplementation((url: string | URL | Request) => {
      if (String(url).endsWith('/threads')) {
        return new Promise<Response>((resolve) =>
          threadResponses.push(() =>
            resolve(new Response(JSON.stringify({ threads: [] }), { status: 200 }))
          )
        );
      }
      return Promise.resolve(new Response(JSON.stringify({ spaces: [space] }), { status: 200 }));
    });
    const el = await mount();
    el.collapsedSpaces = new Set<string>();
    await settle();
    // Answer the first load's thread list.
    threadResponses.splice(0).forEach((answer) => answer());
    await settle();
    await el.updateComplete;

    pull(el);
    await vi.waitFor(() => expect(threadResponses.length).toBe(1));
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).not.toBeNull();

    threadResponses.splice(0).forEach((answer) => answer());
    await settle();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).toBeNull();
  });

  it('says so when a pull fails, but not when a quiet refresh does', async () => {
    const el = await mount();
    apiFetchMock.mockImplementation(() => Promise.resolve(new Response('', { status: 503 })));
    await el.refresh({ quiet: true });
    expect(showToast).not.toHaveBeenCalled();

    pull(el);
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledTimes(1));
  });
});
