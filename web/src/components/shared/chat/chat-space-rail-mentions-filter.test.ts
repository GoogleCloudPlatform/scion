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
 * Tests for the left-rail Unread/Mentions dropdown filter (ptone/scion#9659):
 * - RailFilter 'mentions' state, persistence, and applyUrlFilter hydration
 * - Unread/Mentions dropdown trigger label, aria-pressed, aria-haspopup, aria-expanded, menu selection
 * - Client-side space and thread filtering (unmuted mentions + selected thread)
 * - Unread DMs hidden under mentions
 * - Empty state "No unread mentions"
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import {
  RAIL_FILTER_STORAGE_KEY,
  hydrateRailFilterFromSearch,
  railFilterFromSearch,
  type ChatUrlRailFilter,
} from '../../../client/chat-routes.js';
import { resetServerFlagStateForTests } from '../../../utils/feature-flags.js';
import type { ChatSpace, ChatSpaceThread, RailFilterChangeDetail } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const SPACE_WITH_MENTION: ChatSpace = {
  projectId: 'proj-mention',
  projectName: 'Mentions Space',
  projectSlug: 'mentions-space',
  unreadCount: 2,
  hasUnreadMention: true,
};

const SPACE_UNREAD_ONLY: ChatSpace = {
  projectId: 'proj-unread',
  projectName: 'Unread Only Space',
  projectSlug: 'unread-only-space',
  unreadCount: 3,
  hasUnreadMention: false,
};

const SPACE_CLEAN: ChatSpace = {
  projectId: 'proj-clean',
  projectName: 'Clean Space',
  projectSlug: 'clean-space',
  unreadCount: 0,
  hasUnreadMention: false,
};

function thread(overrides: Partial<ChatSpaceThread> = {}): ChatSpaceThread {
  return {
    id: 'topic-1',
    name: 'thread-one',
    isGeneral: false,
    pinned: false,
    muted: false,
    hasUnread: false,
    hasUnreadMention: false,
    ...overrides,
  };
}

async function mountRail(
  spaces: ChatSpace[],
  threadsBySpace: Map<string, ChatSpaceThread[]>
): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await new Promise((resolve) => setTimeout(resolve, 0));
  el.spaces = spaces;
  el.threadsBySpace = threadsBySpace;
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  await el.updateComplete;
  return el;
}

function spaceNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.space-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function threadNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-item .thread-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function filterButton(el: any, label: string): HTMLButtonElement | null {
  const btns = Array.from(
    el.shadowRoot.querySelectorAll('.filter-toggle button')
  ) as HTMLButtonElement[];
  return btns.find((b) => b.textContent?.trim() === label) ?? null;
}

function unreadDropdownTrigger(el: any): HTMLButtonElement | null {
  return el.shadowRoot.querySelector('[data-unread-dropdown-trigger]') as HTMLButtonElement | null;
}

function unreadMainButton(el: any): HTMLButtonElement | null {
  return el.shadowRoot.querySelector('[data-unread-filter-main]') as HTMLButtonElement | null;
}

function selectUnreadMenuItem(el: any, value: 'unread' | 'mentions'): void {
  const menu = el.shadowRoot.querySelector('[data-unread-filter-menu]') as HTMLElement | null;
  const item = menu?.querySelector(`sl-menu-item[value="${value}"]`) as HTMLElement | null;
  if (!menu || !item) throw new Error(`missing unread dropdown menu item: ${value}`);
  menu.dispatchEvent(new CustomEvent('sl-select', { detail: { item }, bubbles: true }));
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  localStorage.clear();
  delete window.__SCION_FEATURES__;
  resetServerFlagStateForTests();
  apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
  localStorage.clear();
  delete window.__SCION_FEATURES__;
  resetServerFlagStateForTests();
});

describe('railFilterFromSearch', () => {
  it('parses valid filter values and ignores invalid or absent ones', () => {
    expect(railFilterFromSearch('?filter=unread')).toBe('unread');
    expect(railFilterFromSearch('?filter=mentions')).toBe('mentions');
    expect(railFilterFromSearch('?filter=starred')).toBe('starred');
    expect(railFilterFromSearch('?filter=all')).toBeNull();
    expect(railFilterFromSearch('?filter=bogus')).toBeNull();
    expect(railFilterFromSearch('')).toBeNull();
  });
});

describe('hydrateRailFilterFromSearch (design §7.1, §14.4)', () => {
  const runHydrate = (search: string) => {
    let replacedParams: URLSearchParams | null = null;
    const persisted: ChatUrlRailFilter[] = [];
    const applied: ChatUrlRailFilter[] = [];
    const result = hydrateRailFilterFromSearch({
      search,
      replaceSearch: (p) => {
        replacedParams = p;
      },
      persistFilter: (f) => persisted.push(f),
      applyFilter: (f) => applied.push(f),
    });
    return { result, replacedParams, persisted, applied };
  };

  it('hydrates unread, persists, strips filter, and preserves other query params', () => {
    const out = runHydrate('?filter=unread&messages=starred');
    expect(out.result).toBe('unread');
    expect(out.persisted).toEqual(['unread']);
    expect(out.applied).toEqual(['unread']);
    expect(out.replacedParams?.toString()).toBe('messages=starred');
  });

  it('hydrates mentions, persists, and strips filter', () => {
    const out = runHydrate('?filter=mentions');
    expect(out.result).toBe('mentions');
    expect(out.persisted).toEqual(['mentions']);
    expect(out.applied).toEqual(['mentions']);
    expect(out.replacedParams?.toString()).toBe('');
  });

  it('hydrates starred, persists, and strips filter', () => {
    const out = runHydrate('?filter=starred');
    expect(out.result).toBe('starred');
    expect(out.persisted).toEqual(['starred']);
    expect(out.applied).toEqual(['starred']);
    expect(out.replacedParams?.toString()).toBe('');
  });

  it('strips and ignores invalid filter values', () => {
    const out = runHydrate('?filter=bogus');
    expect(out.result).toBeNull();
    expect(out.persisted).toEqual([]);
    expect(out.applied).toEqual([]);
    expect(out.replacedParams?.toString()).toBe('');
  });
});

describe('space rail — Unread/Mentions split-button filter (ptone/scion#9659, #9710)', () => {
  it('places sort in the rail header and renders the split button with chevron left of icon+label', async () => {
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    // Sort button lives in the Projects header row, not the filter toolbar.
    expect(el.shadowRoot.querySelector('.rail-header .sort-btn')).not.toBeNull();
    expect(el.shadowRoot.querySelector('.rail-toolbar .sort-btn')).toBeNull();

    // No separate sub-filter chip row.
    expect(el.shadowRoot.querySelector('[data-mentions-chip]')).toBeNull();
    expect(el.shadowRoot.querySelector('[data-rail-subfilter]')).toBeNull();

    const split = el.shadowRoot.querySelector('.filter-split') as HTMLElement | null;
    expect(split).not.toBeNull();
    const chevronTrigger = unreadDropdownTrigger(el);
    const mainBtn = unreadMainButton(el);
    expect(chevronTrigger).not.toBeNull();
    expect(mainBtn).not.toBeNull();

    // Chevron trigger is to the left of the main icon+label button inside .filter-split.
    const splitChildren = Array.from(split!.children);
    expect(splitChildren[0]?.contains(chevronTrigger)).toBe(true);
    expect(splitChildren[1]).toBe(mainBtn);

    // Chevron trigger owns dropdown ARIA; main button owns pressed state and envelope icon.
    expect(chevronTrigger?.getAttribute('aria-haspopup')).toBe('menu');
    expect(chevronTrigger?.getAttribute('aria-expanded')).toBe('false');
    expect(chevronTrigger?.querySelector('sl-icon')?.getAttribute('name')).toBe('chevron-down');
    expect(mainBtn?.textContent?.trim()).toBe('Unread');
    expect(mainBtn?.getAttribute('aria-pressed')).toBe('false');
    expect(mainBtn?.querySelector('sl-icon')?.getAttribute('name')).toBe('envelope');

    const dropdown = el.shadowRoot.querySelector('.filter-dropdown') as HTMLElement;
    dropdown.dispatchEvent(new CustomEvent('sl-show'));
    await el.updateComplete;
    expect(unreadDropdownTrigger(el)?.getAttribute('aria-expanded')).toBe('true');
    dropdown.dispatchEvent(new CustomEvent('sl-hide'));
    await el.updateComplete;
    expect(unreadDropdownTrigger(el)?.getAttribute('aria-expanded')).toBe('false');

    const events: RailFilterChangeDetail[] = [];
    el.addEventListener('rail-filter-change', (e: Event) => {
      events.push((e as CustomEvent<RailFilterChangeDetail>).detail);
    });

    // Clicking the main button selects Unread directly without opening the dropdown.
    mainBtn!.click();
    await el.updateComplete;
    expect(el.filter).toBe('unread');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('unread');
    expect(unreadMainButton(el)?.textContent?.trim()).toBe('Unread');
    expect(unreadMainButton(el)?.getAttribute('aria-pressed')).toBe('true');
    expect(unreadMainButton(el)?.querySelector('sl-icon')?.getAttribute('name')).toBe('envelope');
    expect(unreadDropdownTrigger(el)?.getAttribute('aria-expanded')).toBe('false');
    expect(
      el.shadowRoot.querySelector('sl-menu-item[value="unread"]')?.hasAttribute('checked')
    ).toBe(true);
    expect(
      el.shadowRoot.querySelector('sl-menu-item[value="mentions"]')?.hasAttribute('checked')
    ).toBe(false);

    // Re-selecting the already-active option in the menu preserves its checked state cleanly.
    const unreadItem = el.shadowRoot.querySelector(
      'sl-menu-item[value="unread"]'
    ) as HTMLElement & { checked?: boolean };
    unreadItem.removeAttribute('checked');
    unreadItem.checked = false;
    selectUnreadMenuItem(el, 'unread');
    await el.updateComplete;
    expect(unreadItem.hasAttribute('checked')).toBe(true);

    // Pick Mentions via dropdown -> active, icon switches to @, label switches to Mentions, persisted.
    selectUnreadMenuItem(el, 'mentions');
    await el.updateComplete;
    expect(el.filter).toBe('mentions');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('mentions');
    expect(unreadMainButton(el)?.textContent?.trim()).toBe('Mentions');
    expect(unreadMainButton(el)?.getAttribute('aria-pressed')).toBe('true');
    expect(unreadMainButton(el)?.querySelector('sl-icon')?.getAttribute('name')).toBe('at');
    expect(
      el.shadowRoot.querySelector('sl-menu-item[value="mentions"]')?.hasAttribute('checked')
    ).toBe(true);
    expect(
      el.shadowRoot.querySelector('sl-menu-item[value="unread"]')?.hasAttribute('checked')
    ).toBe(false);

    // Switch to All -> clears persistence, remembers Mentions mode on the split button,
    // and clicking the main button selects Mentions again without opening the dropdown.
    filterButton(el, 'All')?.click();
    await el.updateComplete;
    expect(el.filter).toBe('all');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBeNull();
    expect(unreadMainButton(el)?.textContent?.trim()).toBe('Mentions');
    expect(unreadMainButton(el)?.getAttribute('aria-pressed')).toBe('false');
    expect(unreadMainButton(el)?.querySelector('sl-icon')?.getAttribute('name')).toBe('at');

    unreadMainButton(el)!.click();
    await el.updateComplete;
    expect(el.filter).toBe('mentions');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('mentions');
    expect(unreadMainButton(el)?.getAttribute('aria-pressed')).toBe('true');

    // Pick Unread via dropdown -> returns to unread, icon back to envelope, label back to Unread.
    selectUnreadMenuItem(el, 'unread');
    await el.updateComplete;
    expect(el.filter).toBe('unread');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('unread');
    expect(unreadMainButton(el)?.textContent?.trim()).toBe('Unread');
    expect(unreadMainButton(el)?.querySelector('sl-icon')?.getAttribute('name')).toBe('envelope');

    expect(events.map((d) => d.filter)).toEqual([
      'unread',
      'mentions',
      'all',
      'mentions',
      'unread',
    ]);
  });

  it('restores mentions from localStorage on mount', async () => {
    localStorage.setItem(RAIL_FILTER_STORAGE_KEY, 'mentions');
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    expect(el.filter).toBe('mentions');
    expect(unreadMainButton(el)?.textContent?.trim()).toBe('Mentions');
    expect(unreadMainButton(el)?.getAttribute('aria-pressed')).toBe('true');
    expect(unreadMainButton(el)?.querySelector('sl-icon')?.getAttribute('name')).toBe('at');
    expect(
      el.shadowRoot.querySelector('sl-menu-item[value="mentions"]')?.hasAttribute('checked')
    ).toBe(true);
  });

  it('filters spaces and threads to unmuted unread mentions plus selected thread, and hides Unread DMs', async () => {
    const tMention = thread({
      id: 't-mention',
      name: 'mentioned-thread',
      hasUnread: true,
      hasUnreadMention: true,
    });
    const tPlainUnread = thread({
      id: 't-unread',
      name: 'plain-unread',
      hasUnread: true,
      hasUnreadMention: false,
    });
    const tMutedMention = thread({
      id: 't-muted',
      name: 'muted-mention',
      muted: true,
      hasUnread: true,
      hasUnreadMention: true,
    });
    const tSelectedClean = thread({
      id: 't-selected',
      name: 'open-clean-thread',
      hasUnread: false,
      hasUnreadMention: false,
    });

    const el = await mountRail(
      [SPACE_WITH_MENTION, SPACE_UNREAD_ONLY, SPACE_CLEAN],
      new Map([
        [SPACE_WITH_MENTION.projectId, [tMention, tPlainUnread, tMutedMention]],
        [
          SPACE_UNREAD_ONLY.projectId,
          [thread({ id: 't-u2', name: 'other-unread', hasUnread: true })],
        ],
        [SPACE_CLEAN.projectId, [tSelectedClean]],
      ])
    );

    // Give the rail an unread DM to confirm it is shown under unread and hidden under mentions.
    el.dms = [
      {
        conversationKey: 'dm:user:me:user:alice',
        peerId: 'alice',
        peerKind: 'user',
        displayName: 'Alice',
        hasUnread: true,
        lastActivityAt: '2026-11-26T10:00:00Z',
      },
    ];
    el.selectedKey = 't-selected';
    el.selectedProjectId = SPACE_CLEAN.projectId;
    await el.updateComplete;

    // Under Unread: Unread DMs section is shown.
    el.applyUrlFilter('unread');
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.dm-section')).not.toBeNull();

    // Under Mentions: Unread DMs hidden; only SPACE_WITH_MENTION + SPACE_CLEAN (selected) visible;
    // only mentioned-thread and open-clean-thread visible.
    el.applyUrlFilter('mentions');
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.dm-section')).toBeNull();
    expect(spaceNames(el)).toEqual(['Mentions Space', 'Clean Space']);
    expect(threadNames(el)).toEqual(['mentioned-thread', 'open-clean-thread']);
  });

  it('shows "No unread mentions" empty state when no spaces have mentions', async () => {
    const el = await mountRail([SPACE_UNREAD_ONLY], new Map());
    el.dms = [
      {
        conversationKey: 'dm:user:me:user:alice',
        peerId: 'alice',
        peerKind: 'user',
        displayName: 'Alice',
        hasUnread: true,
        lastActivityAt: '2026-11-26T10:00:00Z',
      },
    ];
    el.applyUrlFilter('mentions');
    await el.updateComplete;

    expect(el.shadowRoot.querySelector('.dm-section')).toBeNull();
    expect(el.shadowRoot.textContent).toContain('No unread mentions');
  });

  it('applyUrlFilter activates and persists starred', async () => {
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    el.applyUrlFilter('starred');
    await el.updateComplete;

    expect(el.filter).toBe('starred');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('starred');
  });

  it('derives space mention visibility from loaded threads even when space.hasUnreadMention is false', async () => {
    const spaceWithoutRollup: ChatSpace = {
      projectId: 'proj-no-rollup',
      projectName: 'Loaded Mentions Space',
      projectSlug: 'loaded-mentions-space',
      unreadCount: 2,
      hasUnreadMention: false,
    };
    const tMention = thread({
      id: 't-m1',
      name: 'thread-with-mention',
      hasUnread: true,
      hasUnreadMention: true,
    });
    const tPlain = thread({
      id: 't-p1',
      name: 'thread-plain-unread',
      hasUnread: true,
      hasUnreadMention: false,
    });
    const el = await mountRail(
      [spaceWithoutRollup],
      new Map([[spaceWithoutRollup.projectId, [tMention, tPlain]]])
    );

    el.applyUrlFilter('mentions');
    await el.updateComplete;

    expect(spaceNames(el)).toEqual(['Loaded Mentions Space']);
    expect(threadNames(el)).toEqual(['thread-with-mention']);
  });
});
