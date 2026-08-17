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
 * Tests for custom space ordering in the rail (#1031).
 *
 * The rail talks to /api/v1/chat/user-prefs, where spaceOrder is a JSON array
 * string — the shape the wire uses is easy to get wrong in one direction only,
 * so both the read and the write are asserted. Reordering by drag and by the
 * keyboard-reachable Move up / Move down must land on the same persisted
 * order, and either one switches the rail to custom sort.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

function space(id: string, name: string): any {
  return {
    projectId: id,
    projectName: name,
    projectSlug: name.toLowerCase(),
    unreadCount: 0,
    hasUnreadMention: false,
  };
}

/** A rail holding three spaces, in the order the server listed them. */
function createRail(): any {
  const el = document.createElement('scion-chat-space-rail') as any;
  el.spaces = [space('p-a', 'Alpha'), space('p-b', 'Bravo'), space('p-c', 'Charlie')];
  el.threadsBySpace = new Map();
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  return el;
}

/**
 * Answer user-prefs the way the server does: echo the PUT back with blank
 * modes defaulted, so the rail's reconcile step has something real to read.
 */
function serveUserPrefs(stored: Record<string, string> = {}): void {
  apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
    if (!path.startsWith('/api/v1/chat/user-prefs')) {
      return Promise.resolve(new Response('{}', { status: 200 }));
    }
    if (init?.method === 'PUT') {
      const body = JSON.parse(String(init.body)) as Record<string, string>;
      stored = {
        spaceSortMode: body.spaceSortMode || 'activity',
        threadSortMode: body.threadSortMode || 'activity',
        spaceOrder: body.spaceOrder ?? '',
      };
    }
    return Promise.resolve(new Response(JSON.stringify(stored), { status: 200 }));
  });
}

/** The body of the last PUT to user-prefs. */
function lastPrefsPut(): Record<string, string> {
  const calls = apiFetchMock.mock.calls.filter(
    (c: any[]) => c[0] === '/api/v1/chat/user-prefs' && c[1]?.method === 'PUT'
  );
  const last = calls[calls.length - 1] as any[];
  return JSON.parse(String(last[1].body)) as Record<string, string>;
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  serveUserPrefs();
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('space rail — prefs round trip', () => {
  it('reads sort mode and order from the user-prefs endpoint', async () => {
    const el = createRail();
    serveUserPrefs({
      spaceSortMode: 'custom',
      threadSortMode: 'alpha',
      spaceOrder: JSON.stringify(['p-c', 'p-a', 'p-b']),
    });

    await el.loadPrefs();

    expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/chat/user-prefs');
    expect(el.prefs.spaceSortMode).toBe('custom');
    expect(el.prefs.threadSortMode).toBe('alpha');
    expect(el.prefs.spaceOrder).toEqual(['p-c', 'p-a', 'p-b']);
  });

  it('accepts an order the server already decoded into an array', async () => {
    const el = createRail();
    serveUserPrefs({ spaceSortMode: 'custom', spaceOrder: ['p-c', 'p-a'] as unknown as string });

    await el.loadPrefs();

    expect(el.prefs.spaceOrder).toEqual(['p-c', 'p-a']);
  });

  it('drops non-string entries from a stored order', async () => {
    const el = createRail();
    serveUserPrefs({ spaceSortMode: 'custom', spaceOrder: JSON.stringify(['p-a', 7, null]) });

    await el.loadPrefs();

    expect(el.prefs.spaceOrder).toEqual(['p-a']);
  });

  it('falls back to defaults when the stored order is not parseable', async () => {
    const el = createRail();
    serveUserPrefs({ spaceSortMode: 'custom', spaceOrder: 'not json' });

    await el.loadPrefs();

    expect(el.prefs.spaceSortMode).toBe('custom');
    expect(el.prefs.spaceOrder).toBeUndefined();
  });

  it('writes the order as a JSON array string', async () => {
    const el = createRail();

    await el.savePrefs({ spaceSortMode: 'custom', spaceOrder: ['p-b', 'p-a'] });

    expect(lastPrefsPut()).toEqual({
      spaceSortMode: 'custom',
      threadSortMode: 'activity',
      spaceOrder: JSON.stringify(['p-b', 'p-a']),
    });
    expect(el.prefs.spaceOrder).toEqual(['p-b', 'p-a']);
  });

  it('reverts the local prefs when the save is refused', async () => {
    const el = createRail();
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));

    await el.savePrefs({ spaceSortMode: 'alpha' });

    expect(el.prefs.spaceSortMode).toBe('activity');
  });

  it('reconciles with what the server stored, not what was requested', async () => {
    const el = createRail();
    apiFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ spaceSortMode: 'activity', spaceOrder: '' }), { status: 200 })
    );

    await el.savePrefs({ spaceSortMode: 'custom', spaceOrder: ['p-b'] });

    expect(el.prefs.spaceSortMode).toBe('activity');
    expect(el.prefs.spaceOrder).toBeUndefined();
  });
});

describe('space rail — sort mode selection', () => {
  function selectSort(el: any, value: string): void {
    const item = document.createElement('div');
    item.setAttribute('value', value);
    el.handleSortSelect(new CustomEvent('sl-select', { detail: { item } }));
  }

  it('persists the alphabetical mode', async () => {
    const el = createRail();

    selectSort(el, 'alpha');
    await Promise.resolve();

    expect(lastPrefsPut().spaceSortMode).toBe('alpha');
  });

  it('freezes the visible order when custom is chosen with nothing stored', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'alpha', threadSortMode: 'activity', spaceOrder: undefined };

    selectSort(el, 'custom');
    await Promise.resolve();

    expect(lastPrefsPut()).toMatchObject({
      spaceSortMode: 'custom',
      spaceOrder: JSON.stringify(['p-a', 'p-b', 'p-c']),
    });
  });

  it('freezes the visible order when custom is chosen with an empty order stored', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'alpha', threadSortMode: 'activity', spaceOrder: [] };

    selectSort(el, 'custom');
    await Promise.resolve();

    expect(lastPrefsPut()).toMatchObject({
      spaceSortMode: 'custom',
      spaceOrder: JSON.stringify(['p-a', 'p-b', 'p-c']),
    });
  });

  it('keeps an existing custom order when custom is re-selected', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'activity', threadSortMode: 'activity', spaceOrder: ['p-c', 'p-a'] };

    selectSort(el, 'custom');
    await Promise.resolve();

    expect(lastPrefsPut().spaceOrder).toBe(JSON.stringify(['p-c', 'p-a']));
  });

  it('sorts spaces by the custom order, unknown spaces last', () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: ['p-c', 'p-b'] };

    expect(el.getSortedSpaces().map((s: any) => s.projectId)).toEqual(['p-c', 'p-b', 'p-a']);
  });
});

describe('space rail — reordering', () => {
  it('moves a space up and persists the new order', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };

    await el.moveSpace('p-c', -1);

    expect(lastPrefsPut().spaceOrder).toBe(JSON.stringify(['p-a', 'p-c', 'p-b']));
  });

  it('moves a space down and persists the new order', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };

    await el.moveSpace('p-a', 1);

    expect(lastPrefsPut().spaceOrder).toBe(JSON.stringify(['p-b', 'p-a', 'p-c']));
  });

  it('switches to custom sort when a space is nudged in activity mode', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'activity', threadSortMode: 'activity', spaceOrder: undefined };

    await el.moveSpace('p-b', -1);

    expect(lastPrefsPut().spaceSortMode).toBe('custom');
    expect(el.prefs.spaceSortMode).toBe('custom');
  });

  it('does nothing at the ends of the list', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };

    await el.moveSpace('p-a', -1);
    await el.moveSpace('p-c', 1);

    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('refuses to move a space while the unread filter is on', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };
    el.spaceFilter = 'unread';

    await el.moveSpace('p-b', -1);

    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('disables both reorder items for every space while filtering', () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };
    el.spaceFilter = 'unread';

    expect(el.isSpaceAtEdge('p-b', 'first')).toBe(true);
    expect(el.isSpaceAtEdge('p-b', 'last')).toBe(true);
  });

  it('reports which spaces sit at the edges, for disabling the menu items', () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };

    expect(el.isSpaceAtEdge('p-a', 'first')).toBe(true);
    expect(el.isSpaceAtEdge('p-a', 'last')).toBe(false);
    expect(el.isSpaceAtEdge('p-c', 'last')).toBe(true);
  });

  it('drops a dragged space onto the position of its target', async () => {
    const el = createRail();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };
    const dt = { effectAllowed: '', dropEffect: '', setData: vi.fn() };

    el.handleSpaceDragStart({ dataTransfer: dt } as any, 'p-c');
    el.handleSpaceDragOver({ preventDefault: vi.fn(), dataTransfer: dt } as any, 'p-a');
    await el.handleSpaceDrop({ preventDefault: vi.fn() } as any, 'p-a');

    expect(dt.setData).toHaveBeenCalledWith('text/plain', 'p-c');
    expect(lastPrefsPut().spaceOrder).toBe(JSON.stringify(['p-c', 'p-a', 'p-b']));
    expect(el.draggingSpaceId).toBeNull();
    expect(el.dragOverSpaceId).toBeNull();
  });

  it('ignores a drop on the space being dragged', async () => {
    const el = createRail();

    el.handleSpaceDragStart({ dataTransfer: { setData: vi.fn() } } as any, 'p-b');
    await el.handleSpaceDrop({ preventDefault: vi.fn() } as any, 'p-b');

    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('ignores dragover when no drag started here', () => {
    const el = createRail();
    const preventDefault = vi.fn();

    el.handleSpaceDragOver({ preventDefault } as any, 'p-a');

    expect(preventDefault).not.toHaveBeenCalled();
    expect(el.dragOverSpaceId).toBeNull();
  });
});

describe('space rail — sort menu and space menu rendering', () => {
  async function mount(): Promise<any> {
    const el = createRail();
    document.body.appendChild(el);
    await new Promise((resolve) => setTimeout(resolve, 0));
    el.spaces = [space('p-a', 'Alpha'), space('p-b', 'Bravo'), space('p-c', 'Charlie')];
    el.threadsBySpace = new Map();
    el.collapsedSpaces = new Set<string>();
    el.loading = false;
    await el.updateComplete;
    return el;
  }

  it('offers Custom alongside the other sort modes, checking the active one', async () => {
    const el = await mount();
    el.prefs = { spaceSortMode: 'custom', threadSortMode: 'activity', spaceOrder: undefined };
    await el.updateComplete;

    const items = [...el.shadowRoot.querySelectorAll('.rail-toolbar sl-menu-item')];
    expect(items.map((i: any) => i.getAttribute('value'))).toEqual([
      'activity',
      'alpha',
      'custom',
    ]);
    const checked = items.filter((i: any) => i.hasAttribute('checked'));
    expect(checked).toHaveLength(1);
    expect(checked[0]?.getAttribute('value')).toBe('custom');
  });

  it('offers keyboard move items on each space, disabled at the ends', async () => {
    const el = await mount();

    const upItems = [...el.shadowRoot.querySelectorAll('.move-up')];
    const downItems = [...el.shadowRoot.querySelectorAll('.move-down')];
    expect(upItems).toHaveLength(3);
    expect(downItems).toHaveLength(3);
    expect(upItems[0]?.hasAttribute('disabled')).toBe(true);
    expect(upItems[1]?.hasAttribute('disabled')).toBe(false);
    expect(downItems[2]?.hasAttribute('disabled')).toBe(true);
  });

  it('greys out the move items while the unread filter hides part of the list', async () => {
    const el = await mount();
    el.spaces = [{ ...space('p-a', 'Alpha'), unreadCount: 1 }, space('p-b', 'Bravo')];
    el.spaceFilter = 'unread';
    await el.updateComplete;

    const upItems = [...el.shadowRoot.querySelectorAll('.move-up')];
    const downItems = [...el.shadowRoot.querySelectorAll('.move-down')];
    expect(upItems).toHaveLength(1);
    expect(upItems[0]?.hasAttribute('disabled')).toBe(true);
    expect(downItems[0]?.hasAttribute('disabled')).toBe(true);
  });

  it('marks space headers as draggable', async () => {
    const el = await mount();

    const headers = [...el.shadowRoot.querySelectorAll('.space-header')];
    expect(headers).toHaveLength(3);
    expect(headers.every((h: any) => h.getAttribute('draggable') === 'true')).toBe(true);
  });
});
