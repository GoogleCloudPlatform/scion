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

  it('restores collapsed state when the component is re-instantiated (reload)', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-abc123']));

    // Restoration happens synchronously in connectedCallback, before the
    // first render — no need to await updateComplete to observe it.
    const el = mount();

    expect(el.collapsedGroups.has('g-abc123')).toBe(true);
  });

  it('defaults to expanded when there is no stored state', () => {
    const el = mount();

    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored state is corrupt JSON', () => {
    localStorage.setItem(STORAGE_KEY, '{not valid json');

    let el: any;
    expect(() => {
      el = mount();
    }).not.toThrow();
    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored state is not an array', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ foo: 'bar' }));

    const el = mount();

    expect(el.collapsedGroups.size).toBe(0);
  });

  it('falls back to the default when stored entries are not strings', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-real', 7, null, { id: 'g-fake' }]));

    const el = mount();

    expect(el.collapsedGroups.has('g-real')).toBe(true);
    expect(el.collapsedGroups.size).toBe(1);
  });

  it('falls back to the default when localStorage throws (private mode)', () => {
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
    expect(el.collapsedGroups.size).toBe(0);
  });
});

describe('space rail — pruning stale entries', () => {
  it('prunes stale group ids once the current groups are known', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['stale-group', 'live-group']));
    const el = mount();
    el.spaces = [space('p-a', 'Alpha')];
    el.prefs = railPrefs({ 'p-a': [{ id: 'live-group', name: 'Live', threadIds: [] }] });

    el.pruneCollapsedGroups();

    expect(el.collapsedGroups.has('stale-group')).toBe(false);
    expect(el.collapsedGroups.has('live-group')).toBe(true);
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]') as string[];
    expect(stored).not.toContain('stale-group');
    expect(stored).toContain('live-group');
  });

  it('does not prune anything when spaces have not loaded (transient failure)', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['g-a', 'g-b']));
    const el = mount();
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
        return Promise.resolve(
          new Response(JSON.stringify({ threadGroups: JSON.stringify({ 'p-a': [] }) }), {
            status: 200,
          })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

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
  it('toggling a different group afterward does not drop the auto-expanded group from storage', () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
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

  it('a later prune does not drop the auto-expanded group from storage', () => {
    localStorage.setItem(`${STORAGE_KEY}:user-1`, JSON.stringify(['g-sel', 'g-stale']));
    const el = document.createElement('scion-chat-space-rail') as any;
    el.currentUserId = 'user-1';
    document.body.appendChild(el);
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
        return Promise.resolve(
          new Response(
            JSON.stringify({
              threadGroups: JSON.stringify({
                'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }],
              }),
            }),
            { status: 200 }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

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

/** Mocks spaces/threads/prefs so space "p-a" has one collapsed group, `g-sel`,
 * holding `thread-1` — the deep-link target for the R4 tests. */
function serveColdDeepLinkFixture(): void {
  apiFetchMock.mockImplementation((path: string) => {
    if (path === '/api/v1/chat/spaces') {
      return Promise.resolve(
        new Response(JSON.stringify({ spaces: [space('p-a', 'Alpha')] }), { status: 200 })
      );
    }
    if (path.startsWith('/api/v1/chat/spaces/')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            threads: [
              {
                id: 'thread-1',
                name: 'sel-thread',
                isGeneral: false,
                pinned: false,
                hasUnread: false,
                hasUnreadMention: false,
              },
            ],
          }),
          { status: 200 }
        )
      );
    }
    if (path === '/api/v1/chat/user-prefs') {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            threadGroups: JSON.stringify({
              'p-a': [{ id: 'g-sel', name: 'Sel', threadIds: ['thread-1'] }],
            }),
          }),
          { status: 200 }
        )
      );
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
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

  it("restores only the current user's collapsed groups, ignoring another user's stored entries", () => {
    localStorage.setItem(`${STORAGE_KEY}:user-a`, JSON.stringify(['g-a-group']));
    localStorage.setItem(`${STORAGE_KEY}:user-b`, JSON.stringify(['g-b-group']));

    const elA = document.createElement('scion-chat-space-rail') as any;
    elA.currentUserId = 'user-a';
    document.body.appendChild(elA);
    expect(elA.collapsedGroups.has('g-a-group')).toBe(true);
    expect(elA.collapsedGroups.has('g-b-group')).toBe(false);

    const elB = document.createElement('scion-chat-space-rail') as any;
    elB.currentUserId = 'user-b';
    document.body.appendChild(elB);
    expect(elB.collapsedGroups.has('g-b-group')).toBe(true);
    expect(elB.collapsedGroups.has('g-a-group')).toBe(false);
  });
});
