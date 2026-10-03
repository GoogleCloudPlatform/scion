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
 * Tests for which rail rows can be dragged.
 *
 * With a mouse or trackpad, every thread row except #general is draggable in
 * every sort mode, and so is the space header. On a touch-primary device no
 * row is draggable, because a press-and-hold opens the row's menu instead and
 * a native drag would swallow it.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ChatSpace, ChatSpaceThread } from './chat-space-rail.js';
import { TOUCH_PRIMARY_QUERY } from '../../../utils/input-modality.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const SPACE: ChatSpace = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 0,
  hasUnreadMention: false,
};

function thread(overrides: Partial<ChatSpaceThread>): ChatSpaceThread {
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

const THREADS: ChatSpaceThread[] = [
  thread({ id: 'general', name: 'general', isGeneral: true }),
  thread({ id: 'topic-b', name: 'bravo' }),
  thread({ id: 'topic-a', name: 'alpha' }),
];

/** Report the touch-primary query as matching (or not) to every caller. */
function stubTouchPrimary(isTouch: boolean): void {
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query: string) =>
      ({
        matches: query === TOUCH_PRIMARY_QUERY ? isTouch : false,
        media: query,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      }) as unknown as MediaQueryList
  );
}

async function mount(): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await new Promise((resolve) => setTimeout(resolve, 0));
  el.spaces = [SPACE];
  el.threadsBySpace = new Map([[SPACE.projectId, THREADS]]);
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  await el.updateComplete;
  return el;
}

/** Each rendered thread row's name and its draggable attribute (null when absent). */
function rowDraggable(el: any): Array<[string, string | null]> {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-item')).map((row: any) => [
    row.querySelector('.thread-name')?.textContent?.trim() ?? '',
    row.getAttribute('draggable'),
  ]);
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

describe('rail drag gate', () => {
  for (const mode of ['activity', 'alpha'] as const) {
    it(`with a mouse, every thread but #general is draggable (${mode} sort)`, async () => {
      stubTouchPrimary(false);
      const el = await mount();
      el.prefs = { ...el.prefs, threadSortMode: mode };
      await el.updateComplete;

      const rows = rowDraggable(el);
      expect(rows).toHaveLength(3);
      for (const [name, draggable] of rows) {
        expect(draggable, name).toBe(name === 'general' ? null : 'true');
      }
      expect(el.shadowRoot.querySelector('.space-header')?.getAttribute('draggable')).toBe('true');
    });
  }

  it('on a touch-primary device, no thread row and no space header is draggable', async () => {
    stubTouchPrimary(true);
    const el = await mount();

    const rows = rowDraggable(el);
    expect(rows).toHaveLength(3);
    for (const [name, draggable] of rows) {
      expect(draggable, name).toBeNull();
    }
    const header = el.shadowRoot.querySelector('.space-header');
    expect(header).not.toBeNull();
    expect(header.hasAttribute('draggable')).toBe(false);
  });
});
