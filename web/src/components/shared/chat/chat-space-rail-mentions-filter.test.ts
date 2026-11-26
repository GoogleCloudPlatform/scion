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
 * Tests for the left-rail "Mentions only" sub-filter (design §14.4, AC-8):
 * - RailFilter 'mentions' state, persistence, and applyUrlFilter hydration
 * - "@ Mentions only" chip visibility, aria-pressed, toggling, and Unread/All/Starred interaction
 * - Client-side space and thread filtering (unmuted mentions + selected thread)
 * - Unread DMs hidden under mentions
 * - Empty state "No unread mentions"
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { RAIL_FILTER_STORAGE_KEY, railFilterFromSearch } from '../../../client/chat-routes.js';
import { CHAT_STARRED_MESSAGES_EXPERIMENT } from '../../../client/chat-stars.js';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../../utils/feature-flags.js';
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

function mentionsChip(el: any): HTMLButtonElement | null {
  return el.shadowRoot.querySelector('[data-mentions-chip]') as HTMLButtonElement | null;
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

describe('space rail — mentions sub-filter (design §14.4, AC-8)', () => {
  it('shows @ Mentions only chip only while Unread is active and toggles unread <-> mentions', async () => {
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    // Hidden under All.
    expect(mentionsChip(el)).toBeNull();

    const events: RailFilterChangeDetail[] = [];
    el.addEventListener('rail-filter-change', (e: Event) => {
      events.push((e as CustomEvent<RailFilterChangeDetail>).detail);
    });

    // Click Unread -> chip appears, not pressed.
    filterButton(el, 'Unread')?.click();
    await el.updateComplete;
    expect(el.filter).toBe('unread');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('unread');
    expect(filterButton(el, 'Unread')?.getAttribute('aria-pressed')).toBe('true');
    const chip = mentionsChip(el);
    expect(chip).not.toBeNull();
    expect(chip?.getAttribute('aria-pressed')).toBe('false');

    // Click chip -> switches to mentions, Unread stays active, chip is pressed.
    chip?.click();
    await el.updateComplete;
    expect(el.filter).toBe('mentions');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('mentions');
    expect(filterButton(el, 'Unread')?.getAttribute('aria-pressed')).toBe('true');
    expect(mentionsChip(el)?.getAttribute('aria-pressed')).toBe('true');

    // Clicking Unread while in mentions is a no-op.
    filterButton(el, 'Unread')?.click();
    await el.updateComplete;
    expect(el.filter).toBe('mentions');

    // Clicking chip again returns to unread.
    mentionsChip(el)?.click();
    await el.updateComplete;
    expect(el.filter).toBe('unread');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBe('unread');
    expect(mentionsChip(el)?.getAttribute('aria-pressed')).toBe('false');

    // Clicking All clears it and hides the chip.
    mentionsChip(el)?.click();
    await el.updateComplete;
    expect(el.filter).toBe('mentions');
    filterButton(el, 'All')?.click();
    await el.updateComplete;
    expect(el.filter).toBe('all');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBeNull();
    expect(mentionsChip(el)).toBeNull();

    expect(events.map((d) => d.filter)).toEqual([
      'unread',
      'mentions',
      'unread',
      'mentions',
      'all',
    ]);
  });

  it('restores mentions from localStorage on mount', async () => {
    localStorage.setItem(RAIL_FILTER_STORAGE_KEY, 'mentions');
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    expect(el.filter).toBe('mentions');
    expect(filterButton(el, 'Unread')?.getAttribute('aria-pressed')).toBe('true');
    expect(mentionsChip(el)?.getAttribute('aria-pressed')).toBe('true');
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

  it('applyUrlFilter ignores starred when web.chat_starred_messages is off', async () => {
    setFeatureFlag(CHAT_STARRED_MESSAGES_EXPERIMENT, false);
    const el = await mountRail([SPACE_WITH_MENTION], new Map());

    el.applyUrlFilter('starred');
    await el.updateComplete;

    expect(el.filter).toBe('all');
    expect(localStorage.getItem(RAIL_FILTER_STORAGE_KEY)).toBeNull();
  });
});
