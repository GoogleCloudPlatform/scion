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
 * and corrupt/unavailable storage must degrade to that same default rather
 * than throwing.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

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

function mount(): any {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  return el;
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

afterEach(() => {
  vi.clearAllMocks();
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

  it('prunes stale group ids once the current groups are known', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['stale-group', 'live-group']));
    const el = mount();
    el.spaces = [space('p-a', 'Alpha')];
    el.prefs = {
      spaceSortMode: 'activity',
      threadSortMode: 'activity',
      spaceOrder: undefined,
      threadOrder: undefined,
      threadGroups: { 'p-a': [{ id: 'live-group', name: 'Live', threadIds: [] }] },
    };

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

  it('falls back to the default when localStorage throws (private mode)', () => {
    const original = window.localStorage.getItem.bind(window.localStorage);
    vi.spyOn(window.localStorage, 'getItem').mockImplementation((key: string) => {
      if (key === STORAGE_KEY) throw new Error('SecurityError');
      return original(key);
    });

    let el: any;
    expect(() => {
      el = mount();
    }).not.toThrow();
    expect(el.collapsedGroups.size).toBe(0);
  });
});
