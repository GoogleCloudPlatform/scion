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
 * Refresh spaces, in the rail's sort menu: the keyboard and screen-reader way
 * to run the same refresh as a pull.
 *
 * Shoelace's menu fires `sl-select` with the item for a click and for Enter
 * or Space on the focused item (Tab reaches the menu's trigger; the arrow
 * keys move between items). The real menu components cannot be registered
 * here: happy-dom upgrades them mid template clone and Shoelace throws. So
 * these tests stand in for the menu by firing `sl-select` from it, which is
 * the rail's whole contract with the menu.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { showToast } from '../../../utils/toast.js';
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
  return apiFetchMock.mock.calls.filter(([url]) => url === '/api/v1/chat/spaces').length;
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

function sortMenu(el: any): HTMLElement {
  return el.shadowRoot.querySelector('.rail-toolbar sl-menu');
}

function refreshItem(el: any): HTMLElement {
  return el.shadowRoot.querySelector('.rail-toolbar sl-menu-item[value="refresh"]');
}

/** What the sort menu does for a click, Enter or Space on the item. */
function select(el: any): void {
  sortMenu(el).dispatchEvent(
    new CustomEvent('sl-select', {
      detail: { item: refreshItem(el) },
      bubbles: true,
      composed: true,
    })
  );
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

describe('space rail — Refresh spaces in the sort menu', () => {
  it('is a plain menu item named "Refresh spaces" in the sort menu', async () => {
    const el = await mount();
    const item = refreshItem(el);
    expect(item).not.toBeNull();
    expect(item.parentElement).toBe(sortMenu(el));
    expect(item.textContent?.trim()).toBe('Refresh spaces');
    // Not a checkbox like the sort modes: choosing it never sticks.
    expect(item.hasAttribute('type')).toBe(false);
    expect(item.querySelector('sl-icon')?.getAttribute('aria-hidden')).toBe('true');
    const trigger = el.shadowRoot.querySelector('.rail-toolbar .sort-btn') as HTMLElement;
    expect(trigger.getAttribute('slot')).toBe('trigger');
    expect(trigger.getAttribute('label')).toBe('Sort');
  });

  it('choosing it runs the same refresh as a pull, with its indicator and announcement', async () => {
    const el = await mount();
    const refresh = vi.spyOn(el, 'refresh');
    const events: CustomEvent<RailRefreshDetail>[] = [];
    let finish!: () => void;
    el.addEventListener('rail-refresh', (e: Event) => {
      events.push(e as CustomEvent<RailRefreshDetail>);
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });
    const before = spacesCalls();

    select(el);
    await settle();
    await el.updateComplete;

    // Shown, not quiet: the same call a pull makes.
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(refresh).toHaveBeenCalledWith();
    expect(events).toHaveLength(1);
    expect(spacesCalls()).toBe(before + 1);
    const region = (): HTMLElement => el.shadowRoot.querySelector('.pull-indicator') as HTMLElement;
    expect(region().getAttribute('aria-live')).toBe('polite');
    expect(region().querySelector('sl-spinner')).not.toBeNull();
    expect(region().textContent?.trim()).toBe('Refreshing');

    finish();
    await settle();
    await el.updateComplete;
    expect(region().querySelector('sl-spinner')).toBeNull();
    expect(region().textContent?.trim()).toBe('Updated');
  });

  it('choosing it again during a refresh joins it', async () => {
    const el = await mount();
    let finish!: () => void;
    let events = 0;
    el.addEventListener('rail-refresh', (e: Event) => {
      events++;
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });
    const before = spacesCalls();

    select(el);
    await settle();
    select(el);
    select(el);
    await settle();

    expect(events).toBe(1);
    expect(spacesCalls()).toBe(before + 1);
    finish();
    await settle();
  });

  it('joins a quiet refresh already running, and shows it', async () => {
    const el = await mount();
    let finish!: () => void;
    let events = 0;
    el.addEventListener('rail-refresh', (e: Event) => {
      events++;
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(
        new Promise<void>((resolve) => (finish = resolve))
      );
    });

    const quiet = el.refresh({ quiet: true });
    await settle();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).toBeNull();

    select(el);
    await el.updateComplete;
    expect(events).toBe(1);
    expect(el.shadowRoot.querySelector('.pull-indicator sl-spinner')).not.toBeNull();

    finish();
    await quiet;
  });

  it('says so once, and does not announce Updated, when the refresh fails', async () => {
    const el = await mount();
    el.addEventListener('rail-refresh', (e: Event) => {
      (e as CustomEvent<RailRefreshDetail>).detail.waitUntil(Promise.reject(new Error('offline')));
    });

    select(el);
    await settle();
    await el.updateComplete;

    expect(showToast).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showToast).mock.calls[0]?.[1]).toBe('warning');
    const region = el.shadowRoot.querySelector('.pull-indicator') as HTMLElement;
    expect(region.querySelector('sl-spinner')).toBeNull();
    expect(region.textContent?.trim()).toBe('');
  });

  it('leaves the sort mode and saved preferences alone', async () => {
    const el = await mount();
    const mode = el.prefs.spaceSortMode;
    const checkedModes = (): (string | null)[] =>
      [
        ...el.shadowRoot.querySelectorAll('.rail-toolbar sl-menu-item[type="checkbox"][checked]'),
      ].map((i: Element) => i.getAttribute('value'));
    const before = checkedModes();
    expect(before).toHaveLength(1);

    select(el);
    await settle();
    await el.updateComplete;

    expect(el.prefs.spaceSortMode).toBe(mode);
    expect(checkedModes()).toEqual(before);
    expect(
      apiFetchMock.mock.calls.some(
        ([url, init]) =>
          url === '/api/v1/chat/user-prefs' && (init as RequestInit | undefined)?.method === 'PUT'
      )
    ).toBe(false);
  });
});
