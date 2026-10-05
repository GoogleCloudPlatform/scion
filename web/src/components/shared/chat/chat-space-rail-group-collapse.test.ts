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
 * Tests for persisting thread-group collapse state across reloads.
 *
 * Thread groups always came back fully expanded after a reload because
 * `collapsedGroups` only ever lived in component state. These tests cover
 * the localStorage round trip: a collapse must survive re-instantiating the
 * component (the reload case), a fresh install must default to expanded,
 * corrupt/unavailable storage must degrade to that same default rather than
 * throwing, pruning must not fire on a partial load failure (round-1 review
 * finding R1), a deep-linked thread inside a collapsed group must still be
 * reachable without un-collapsing it for good (N1), and the storage key must
 * be scoped per user so an account switch in the same browser can't prune
 * the other account's entries (F2).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { html, render } from 'lit';
import { apiFetch } from '../../../client/api.js';
import { chatSpacesLoad } from '../../../client/chat-list-cache.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(),
}));

const apiFetchMock = vi.mocked(apiFetch);

const STORAGE_KEY = 'scion-chat-group-collapse';

function space(id: string, name: string): any {
  return {
    projectId: id,
    projectName: name,
    projectSlug: name.toLowerCase(),
    unreadCount: 0,
    hasUnreadMention: false,
  };
}

function railPrefs(threadGroups: Record<string, unknown[]>): any {
  return {
    spaceSortMode: 'activity',
    threadSortMode: 'activity',
    spaceOrder: undefined,
    threadOrder: undefined,
    threadGroups,
  };
}

/**
 * Mocks a single space, "p-a", with the given thread groups and (optional)
 * threads — the shape every auto-expand test needs: one space, its thread
 * list, and its groups, all 200s. Each call replaces the mock wholesale, so
 * a test can call this again mid-test (e.g. after a server-side change) to
 * simulate a follow-up reload seeing new data.
 */
function serveGroups(threadGroups: Record<string, unknown[]>, threads: unknown[] = []): void {
  apiFetchMock.mockImplementation((path: string) => {
    if (path === '/api/v1/chat/spaces') {
      return Promise.resolve(
        new Response(JSON.stringify({ spaces: [space('p-a', 'Alpha')] }), { status: 200 })
      );
    }
    if (path.startsWith('/api/v1/chat/spaces/')) {
      return Promise.resolve(new Response(JSON.stringify({ threads }), { status: 200 }));
    }
    if (path === '/api/v1/chat/user-prefs') {
      return Promise.resolve(
        new Response(JSON.stringify({ threadGroups: JSON.stringify(threadGroups) }), {
          status: 200,
        })
      );
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

/** Default mock: both endpoints return empty, successful responses. */
function serveDefaults(): void {
  apiFetchMock.mockImplementation((path: string) => {
    if (path === '/api/v1/chat/spaces') {
      return Promise.resolve(new Response(JSON.stringify({ spaces: [] }), { status: 200 }));
    }
    if (path === '/api/v1/chat/user-prefs') {
      return Promise.resolve(new Response('{}', { status: 200 }));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

function mount(): any {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  return el;
}

/**
 * Resolve once the rail's `loadData()` finishes — its `finally` block
 * dispatches `rail-loaded` regardless of success or failure. Used to flush
 * the *initial*, connectedCallback-triggered load without calling
 * `reload()` a second time, so cold-deep-link tests (R4) observe exactly
 * what the first load produced.
 */
function waitForRailLoaded(el: EventTarget): Promise<void> {
  return new Promise((resolve) => {
    el.addEventListener('rail-loaded', () => resolve(), { once: true });
  });
}

/**
 * Original `window.localStorage`, saved so the private-mode test can swap in
 * a throwing fake and this file can put the real one back afterwards.
 *
 * `vi.spyOn(window.localStorage, 'getItem')` does not reliably un-spy on
 * happy-dom's Storage implementation — `vi.restoreAllMocks()` leaves the
 * throwing implementation in place for every test that runs afterwards.
 * Swapping the whole `localStorage` property instead, and restoring it by
 * hand, sidesteps that.
 */
let originalLocalStorage: Storage;

beforeAll(async () => {
  await import('./chat-space-rail.js');
  originalLocalStorage = window.localStorage;
});

beforeEach(() => {
  serveDefaults();
  // The rail's first load reuses a recent shared spaces load; one test's
  // spaces must not answer the next test's first load.
  chatSpacesLoad.invalidate();
});

afterEach(() => {
  vi.clearAllMocks();
  Object.defineProperty(window, 'localStorage', {
    value: originalLocalStorage,
    configurable: true,
    writable: true,
  });
  document.body.innerHTML = '';
  localStorage.clear();
});

describe('space rail — thread-group collapse persistence', () => {
  it('persists a collapse to localStorage', () => {
    const el = mount();
    el.toggleGroupCollapse('g-abc123');

    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).toContain('g-abc123');
    expect(el.collapsedGroups.has('g-abc123')).toBe(true);
  });

  it('an expand removes the entry from localStorage', () => {
    const el = mount();
    el.toggleGroupCollapse('g-abc123');
    el.toggleGroupCollapse('g-abc123');

    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).not.toContain('g-abc123');
    expect(el.collapsedGroups.has('g-abc123')).toBe(false);
  });

  it('restores collapsed state when the component is re-instantiated (reload)', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-abc123']));

    // Restoration happens in willUpdate, before the first render — no flash
    // — but that's still the first update cycle, which is scheduled async,
    // so the assertion has to wait for it.
    const el = mount();
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-abc123')).toBe(true);
  });

  it('defaults to expanded when there is no stored state', () => {
    const el = mount();

    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored state is corrupt JSON', async () => {
    localStorage.setItem(STORAGE_KEY, '{not valid json');

    let el: any;
    expect(() => {
      el = mount();
    }).not.toThrow();
    await el.updateComplete;
    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored state is not an array', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ foo: 'bar' }));

    const el = mount();
    await el.updateComplete;

    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored entries are not strings', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-real', 7, null, { id: 'g-fake' }]));

    const el = mount();
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-real')).toBe(true);
    expect(el.collapsedGroups.size).toBe(1);
  });

  it('falls back to the default when localStorage throws (private mode)', async () => {
    Object.defineProperty(window, 'localStorage', {
      value: {
        getItem: (key: string) => {
          if (key === STORAGE_KEY) throw new Error('SecurityError');
          return null;
        },
        setItem: () => {},
        removeItem: () => {},
        clear: () => {},
      } as unknown as Storage,
      configurable: true,
      writable: true,
    });

    let el: any;
    expect(() => {
      el = mount();
    }).not.toThrow();
    await el.updateComplete;
    expect(el.collapsedGroups.size).toBe(0);
  });
});

describe('space rail — pruning stale entries', () => {
  it('prunes stale group ids once the current groups are known', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['stale-group', 'live-group']));
    const el = mount();
    await el.updateComplete; // restore, before hand-setting spaces/prefs below
    el.spaces = [space('p-a', 'Alpha')];
    el.prefs = railPrefs({ 'p-a': [{ id: 'live-group', name: 'Live', threadIds: [] }] });

    el.pruneCollapsedGroups();

    expect(el.collapsedGroups.has('stale-group')).toBe(false);
    expect(el.collapsedGroups.has('live-group')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).not.toContain('stale-group');
    expect(stored).toContain('live-group');
  });

  it('does not prune anything when spaces have not loaded (transient failure)', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-a', 'g-b']));
    const el = mount();
    await el.updateComplete;
    el.spaces = [];

    el.pruneCollapsedGroups();

    expect(el.collapsedGroups.has('g-a')).toBe(true);
    expect(el.collapsedGroups.has('g-b')).toBe(true);
  });
});

describe('space rail — prune gating on partial load failures (R1 regression)', () => {
  it('does not wipe stored collapse state when spaces load but prefs return 503', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-live']));
    apiFetchMock.mockImplementation((path: string) => {
      if (path === '/api/v1/chat/spaces') {
        return Promise.resolve(
          new Response(JSON.stringify({ spaces: [space('p-a', 'Alpha')] }), { status: 200 })
        );
      }
      if (path.startsWith('/api/v1/chat/spaces/')) {
        return Promise.resolve(new Response(JSON.stringify({ threads: [] }), { status: 200 }));
      }
      if (path === '/api/v1/chat/user-prefs') {
        return Promise.resolve(new Response('{}', { status: 503 }));
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const el = mount();
    // Drive a full, deterministic load/prune pass through the real wiring
    // (loadData -> loadSpaces + loadPrefs -> pruneCollapsedGroups), not the
    // private method directly.
    await el.reload();

    expect(el.collapsedGroups.has('g-live')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).toContain('g-live');
  });

  it('does not wipe stored collapse state when prefs load but spaces return 503', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-live']));
    apiFetchMock.mockImplementation((path: string) => {
      if (path === '/api/v1/chat/spaces') {
        return Promise.resolve(new Response('{}', { status: 503 }));
      }
      if (path === '/api/v1/chat/user-prefs') {
        return Promise.resolve(
          new Response(JSON.stringify({ threadGroups: JSON.stringify({}) }), { status: 200 })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const el = mount();
    await el.reload();

    expect(el.collapsedGroups.has('g-live')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).toContain('g-live');
  });

  it('prunes once both spaces and prefs load successfully in the same pass', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['stale-group']));
    serveGroups({ 'p-a': [] });

    const el = mount();
    await el.reload();

    expect(el.collapsedGroups.has('stale-group')).toBe(false);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).not.toContain('stale-group');
  });
});

describe('space rail — auto-expand group for a selected/deep-linked thread (N1)', () => {
  it('sets a transient autoExpandedGroupId override without mutating collapsedGroups', () => {
    const el = document.createElement('scion-chat-space-rail') as any;
    el.collapsedGroups = new Set(['g-live']);
    el.selectedKey = 'thread-1';
    el.prefs = railPrefs({ 'p-a': [{ id: 'g-live', name: 'Live', threadIds: ['thread-1'] }] });
    el._prefsLoaded = true;
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-live']));

    el.maybeAutoExpandGroupForSelectedKey();

    expect(el.autoExpandedGroupId).toBe('g-live');
    // The user's real preference is untouched — only the transient override
    // changed. See the R2 tests below for what breaks if this mutates
    // collapsedGroups instead.
    expect(el.collapsedGroups.has('g-live')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).toContain('g-live');
  });

  it('does nothing when the selected thread is not inside a collapsed group', () => {
    const el = document.createElement('scion-chat-space-rail') as any;
    el.collapsedGroups = new Set(['g-live']);
    el.selectedKey = 'not-in-any-group';
    el.prefs = railPrefs({ 'p-a': [{ id: 'g-live', name: 'Live', threadIds: ['thread-1'] }] });
    el._prefsLoaded = true;

    el.maybeAutoExpandGroupForSelectedKey();

    expect(el.autoExpandedGroupId).toBeNull();
    expect(el.collapsedGroups.has('g-live')).toBe(true);
  });

  it('runs from the reactive updated() hook when selectedKey changes on a live element', async () => {
    const el = mount();
    // Let the connectedCallback-triggered load settle on the (empty) default
    // mocks before overriding prefs/collapsedGroups by hand, so there is no
    // race between that load and this setup.
    await el.reload();

    el.prefs = railPrefs({ 'p-a': [{ id: 'g-live', name: 'Live', threadIds: ['thread-1'] }] });
    el.collapsedGroups = new Set(['g-live']);
    await el.updateComplete;

    el.selectedKey = 'thread-1';
    await el.updateComplete;

    expect(el.autoExpandedGroupId).toBe('g-live');
    expect(el.collapsedGroups.has('g-live')).toBe(true);
  });

  it('clears the override (without touching collapsedGroups) when selectedKey is cleared', async () => {
    const el = mount();
    await el.reload();

    el.prefs = railPrefs({ 'p-a': [{ id: 'g-live', name: 'Live', threadIds: ['thread-1'] }] });
    el.collapsedGroups = new Set(['g-live']);
    el.selectedKey = 'thread-1';
    await el.updateComplete;
    expect(el.autoExpandedGroupId).toBe('g-live');

    el.selectedKey = '';
    await el.updateComplete;

    expect(el.autoExpandedGroupId).toBeNull();
    expect(el.collapsedGroups.has('g-live')).toBe(true);
  });
});

describe('space rail — auto-expand must never leak into the persisted set (R2 regression)', () => {
  it('toggling a different group afterward does not drop the auto-expanded group from storage', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
    await el.updateComplete; // restore ['g-sel'], before hand-setting prefs below
    el.selectedKey = 'thread-1';
    el.prefs = railPrefs({ 'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }] });
    el._prefsLoaded = true;

    el.maybeAutoExpandGroupForSelectedKey();
    expect(el.autoExpandedGroupId).toBe('g-sel');

    // A real, unrelated toggle — this is what used to write the mutated
    // (auto-expanded) collapsedGroups back out, silently dropping g-sel.
    el.toggleGroupCollapse('g-other');

    const stored = JSON.parse(localStorage.getItem(`${STORAGE_KEY}:user-1`) ?? '[]') as string[];
    expect(stored).toContain('g-sel');
    expect(stored).toContain('g-other');
  });

  it('a later prune does not drop the auto-expanded group from storage', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel', 'g-stale']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
    await el.updateComplete; // restore ['g-sel', 'g-stale'], before hand-setting below
    el.selectedKey = 'thread-1';
    el.spaces = [space('p-a', 'Alpha')];
    // g-stale no longer exists server-side; g-sel still does and still holds
    // the selected thread.
    el.prefs = railPrefs({ 'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }] });
    el._prefsLoaded = true;

    el.maybeAutoExpandGroupForSelectedKey();
    expect(el.autoExpandedGroupId).toBe('g-sel');

    el.pruneCollapsedGroups();

    expect(el.collapsedGroups.has('g-sel')).toBe(true);
    expect(el.collapsedGroups.has('g-stale')).toBe(false);
    const stored = JSON.parse(localStorage.getItem(`${STORAGE_KEY}:user-1`) ?? '[]') as string[];
    expect(stored).toContain('g-sel');
    expect(stored).not.toContain('g-stale');
  });

  it("does not override the user's collapse on a later reload with the same selectedKey (R3 regression)", async () => {
    serveGroups({ 'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }] });

    const el = document.createElement('scion-chat-space-rail') as any;
    el.selectedKey = 'thread-1';
    document.body.appendChild(el);
    await el.reload();

    // The user collapses the group that holds their own currently-open
    // thread — a deliberate, real toggle.
    el.toggleGroupCollapse('g-sel');
    expect(el.collapsedGroups.has('g-sel')).toBe(true);

    // A later SSE-triggered reload with the *same* selectedKey (chat.ts
    // calls reload() on every message, topic change, etc.) must not force
    // the group back open.
    await el.reload();

    expect(el.collapsedGroups.has('g-sel')).toBe(true);
    expect(el.autoExpandedGroupId).not.toBe('g-sel');
  });
});

/** Space "p-a" has one collapsed group, `g-sel`, holding `thread-1` — the
 * deep-link target for the R4 and R5 tests. */
function serveColdDeepLinkFixture(): void {
  serveGroups({ 'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }] }, [
    {
      id: 'thread-1',
      name: 'sel-thread',
      isGeneral: false,
      pinned: false,
      hasUnread: false,
      hasUnreadMention: false,
    },
  ]);
}

describe('space rail — cold deep link auto-expands before any reload (R4 regression)', () => {
  it('auto-expands the group on the very first load — selectedKey set before the element connects', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    serveColdDeepLinkFixture();

    const el = document.createElement('scion-chat-space-rail') as any;
    // Set before appending, the same order chat.ts's attribute bindings
    // produce (round-2 review, F5) — this is what makes the very first
    // updated() fire before any data has loaded, which is the case R4
    // regressed on.
    el.selectedKey = 'thread-1';
    el.currentUserId = 'user-1';
    const loaded = waitForRailLoaded(el);
    document.body.appendChild(el);
    // Flush the *initial* connectedCallback-triggered load — not reload(),
    // which round 2's tests used and which is what masked this regression.
    await loaded;

    expect(el.autoExpandedGroupId).toBe('g-sel');
  });

  it('auto-expands when rendered the way chat.ts actually renders it (lit.render)', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    serveColdDeepLinkFixture();

    const container = document.createElement('div');
    document.body.appendChild(container);
    render(
      html`<scion-chat-space-rail
        selectedKey=${'thread-1'}
        currentUserId=${'user-1'}
      ></scion-chat-space-rail>`,
      container
    );
    const el = container.querySelector('scion-chat-space-rail') as any;
    await waitForRailLoaded(el);

    expect(el.autoExpandedGroupId).toBe('g-sel');
  });
});

describe('space rail — toggling the auto-expanded group (R5 regression)', () => {
  it('clicking the auto-expanded group only clears the override — nothing is saved or removed', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    serveColdDeepLinkFixture();

    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    el.selectedKey = 'thread-1';
    document.body.appendChild(el);
    await waitForRailLoaded(el);
    expect(el.autoExpandedGroupId).toBe('g-sel');

    // The click that's supposed to visually re-collapse the group.
    el.toggleGroupCollapse('g-sel');

    expect(el.autoExpandedGroupId).toBeNull();
    // The real preference was already collapsed and stays that way — this
    // click didn't expand it (that's the M2 mutation: without the
    // override branch, this toggle instead deletes g-sel and persists the
    // deletion).
    expect(el.collapsedGroups.has('g-sel')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(`${STORAGE_KEY}:user-1`) ?? '[]') as string[];
    expect(stored).toContain('g-sel');

    // A later reload with the same selectedKey must not re-open it: it is
    // now indistinguishable from any other group the user has collapsed.
    await el.reload();
    expect(el.autoExpandedGroupId).not.toBe('g-sel');
    expect(el.collapsedGroups.has('g-sel')).toBe(true);
  });

  it('renders the auto-expanded group open, and collapsed again once the override clears', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    serveColdDeepLinkFixture();

    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    el.selectedKey = 'thread-1';
    document.body.appendChild(el);
    await waitForRailLoaded(el);
    // Force the space itself open so the group header actually renders
    // (spaces collapse by default on first load — a separate mechanism).
    el.collapsedSpaces = new Set();
    await el.updateComplete;
    // Expanding the space loads its thread list; the group renders once
    // its threads are known.
    await vi.waitFor(() => expect(el.shadowRoot.querySelector('.thread-group')).not.toBeNull());

    expect(el.autoExpandedGroupId).toBe('g-sel');
    let chevron = el.shadowRoot.querySelector('.thread-group-header .chevron');
    expect(chevron?.classList.contains('collapsed')).toBe(false);
    expect(el.shadowRoot.querySelector('.thread-group')).not.toBeNull();

    // Same action a click on the header performs.
    el.toggleGroupCollapse('g-sel');
    await el.updateComplete;

    chevron = el.shadowRoot.querySelector('.thread-group-header .chevron');
    expect(chevron?.classList.contains('collapsed')).toBe(true);
    expect(el.shadowRoot.querySelector('.thread-group')).toBeNull();
  });
});

describe('space rail — storage key scoped per user (F2)', () => {
  it('persists under a per-user key, leaking neither to the unscoped key nor another user', () => {
    const elA = document.createElement('scion-chat-space-rail') as any;
    elA.currentUserId = 'user-a';
    document.body.appendChild(elA);

    elA.toggleGroupCollapse('g-shared-id');

    const storedForA = JSON.parse(
      localStorage.getItem(`${STORAGE_KEY}:user-a`) ?? '[]'
    ) as string[];
    expect(storedForA).toContain('g-shared-id');
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();

    const elB = document.createElement('scion-chat-space-rail') as any;
    elB.currentUserId = 'user-b';
    document.body.appendChild(elB);

    expect(elB.collapsedGroups.has('g-shared-id')).toBe(false);
  });

  it("restores only the current user's collapsed groups, ignoring another user's stored entries", async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-a`, JSON.stringify(['g-a-group']));
    localStorage.setItem(`${STORAGE_KEY}:user-b`, JSON.stringify(['g-b-group']));

    const elA = document.createElement('scion-chat-space-rail') as any;
    elA.currentUserId = 'user-a';
    document.body.appendChild(elA);
    await elA.updateComplete;
    expect(elA.collapsedGroups.has('g-a-group')).toBe(true);
    expect(elA.collapsedGroups.has('g-b-group')).toBe(false);

    const elB = document.createElement('scion-chat-space-rail') as any;
    elB.currentUserId = 'user-b';
    document.body.appendChild(elB);
    await elB.updateComplete;
    expect(elB.collapsedGroups.has('g-b-group')).toBe(true);
    expect(elB.collapsedGroups.has('g-a-group')).toBe(false);
  });
});

describe('space rail — currentUserId restore covers every arrival timing (N7)', () => {
  it('restores from the scoped key when currentUserId is already set before connecting (the normal chat.ts case)', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-1']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-1')).toBe(true);
  });

  it('does not clobber in-memory state on an unrelated update once already loaded', async () => {
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
    await el.updateComplete;

    // Set collapsedGroups directly rather than via toggleGroupCollapse, so
    // nothing is on disk for this value — a spurious re-read on *any*
    // update (not just a currentUserId change) would silently replace it
    // with whatever's actually stored (nothing), and a toggle-then-check
    // couldn't tell that apart from a correct no-op re-read, since the
    // toggle would have already persisted the same value either way.
    el.collapsedGroups = new Set(['g-in-memory-only']);
    el.selectedKey = 'thread-1'; // unrelated property change
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-in-memory-only')).toBe(true);
  });

  it('P1 — restores correctly when currentUserId is set after appendChild but before the first update', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-1']));
    const el = document.createElement('scion-chat-space-rail') as any;
    document.body.appendChild(el);
    // Old (round-2) guard compared against Lit's changedProperties old
    // value, which reads as `undefined` (not `''`) for a same-tick,
    // post-append set like this — so it never fired. This is what that
    // missed (round-4 review, N7, probe P1).
    el.currentUserId = 'user-1';
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-1')).toBe(true);

    // A save from here on goes to the scoped bucket, not the unscoped one
    // a synchronous connectedCallback-time read would have used.
    el.toggleGroupCollapse('g-2');
    const stored = JSON.parse(localStorage.getItem(`${STORAGE_KEY}:user-1`) ?? '[]') as string[];
    expect(stored).toContain('g-1');
    expect(stored).toContain('g-2');
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
  });

  it('P2 — restores the new set when currentUserId switches directly from one user to another', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-1']));
    localStorage.setItem(`${STORAGE_KEY}:user-2`, JSON.stringify(['g-2']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.collapsedGroups.has('g-1')).toBe(true);

    // A live switch with no reload — e.g. an in-app account switcher. The
    // old guard required the *old* value to be `''`, so this never fired
    // (round-4 review, N7, probe P2).
    el.currentUserId = 'user-2';
    await el.updateComplete;

    expect(el.collapsedGroups.has('g-2')).toBe(true);
    expect(el.collapsedGroups.has('g-1')).toBe(false);

    // And a save now goes to user-2's bucket, not user-1's stale one.
    el.toggleGroupCollapse('g-3');
    const storedForUser1 = JSON.parse(
      localStorage.getItem(`${STORAGE_KEY}:user-1`) ?? '[]'
    ) as string[];
    const storedForUser2 = JSON.parse(
      localStorage.getItem(`${STORAGE_KEY}:user-2`) ?? '[]'
    ) as string[];
    expect(storedForUser1).not.toContain('g-3');
    expect(storedForUser2).toContain('g-3');
  });
});

describe('space rail — auto-expand override cleared when the thread changes groups (N5)', () => {
  it('clears (without re-picking) the override when the selected thread moves to a different group', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-a', 'g-b']));

    // thread-1 starts in g-a.
    serveGroups({
      'p-a': [
        { id: 'g-a', name: 'A', threadIds: ['thread-1'] },
        { id: 'g-b', name: 'B', threadIds: [] },
      ],
    });

    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    el.selectedKey = 'thread-1';
    document.body.appendChild(el);
    await el.reload();
    expect(el.autoExpandedGroupId).toBe('g-a');

    // The thread moves server-side from g-a to g-b; both stay collapsed.
    serveGroups({
      'p-a': [
        { id: 'g-a', name: 'A', threadIds: [] },
        { id: 'g-b', name: 'B', threadIds: ['thread-1'] },
      ],
    });

    await el.reload();

    // Cleared, not re-picked to g-b. collapsedGroups (the user's real
    // preference for each group) is untouched by any of this.
    expect(el.autoExpandedGroupId).toBeNull();
    expect(el.collapsedGroups.has('g-a')).toBe(true);
    expect(el.collapsedGroups.has('g-b')).toBe(true);
  });

  it('clears the override when the group itself is deleted', () => {
    const el = document.createElement('scion-chat-space-rail') as any;
    el.selectedKey = 'thread-1';
    el.autoExpandedGroupId = 'g-gone';
    el.prefs = railPrefs({ 'p-a': [] }); // g-gone no longer exists

    el.clearStaleAutoExpand();

    expect(el.autoExpandedGroupId).toBeNull();
  });
});

describe('space rail — a new selection re-decides the override (N6, intended behavior)', () => {
  it('reopens a group the user just collapsed when a different thread in it is selected next', async () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-shared']));
    serveGroups({
      'p-a': [{ id: 'g-shared', name: 'Shared', threadIds: ['thread-a', 'thread-b'] }],
    });

    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    el.selectedKey = 'thread-a';
    document.body.appendChild(el);
    await el.reload();
    expect(el.autoExpandedGroupId).toBe('g-shared');

    // The user re-collapses it for this view.
    el.toggleGroupCollapse('g-shared');
    expect(el.autoExpandedGroupId).toBeNull();

    // A new selection — even one already in the same group — gets its own,
    // independent decision. This is deliberate (round-3 review, N6): a
    // newly selected thread should be visible, and "decide once per key"
    // means each key gets to make that call for itself.
    el.selectedKey = 'thread-b';
    await el.updateComplete;

    expect(el.autoExpandedGroupId).toBe('g-shared');
  });
});
