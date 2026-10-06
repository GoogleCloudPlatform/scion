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
 * Tests for the left-rail "Unread" filter also filtering the thread list
 * inside each shown space, not just which spaces appear.
 *
 * The filter keeps the same unread definition the dots and the space rollup
 * already use (mute always wins), and always keeps the currently open
 * conversation on screen so it doesn't vanish out from under the user while
 * they're reading it — including when auto-advance marks it read.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import type { ChatSpace, ChatSpaceThread } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

// unreadCount > 0 so this space survives the space-level "Unread" filter —
// these tests are about the thread list *inside* an already-shown space.
const SPACE: ChatSpace = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 1,
  hasUnreadMention: false,
};

// A second space, at zero unread, for the selected-space tests — distinct
// projectId/name from SPACE so assertions can't pass by coincidence.
const SPACE_A: ChatSpace = {
  projectId: 'proj-a',
  projectName: 'Alpha',
  projectSlug: 'alpha',
  unreadCount: 0,
  hasUnreadMention: false,
};
const SPACE_B: ChatSpace = {
  projectId: 'proj-b',
  projectName: 'Beta',
  projectSlug: 'beta',
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

/**
 * Mount a rail with the given spaces and their threads. Mirrors the pattern
 * in chat-space-rail-mark-unread.test.ts: let the connectedCallback load
 * settle against the generic `{}` mock, then overwrite with the fixture so
 * there's no race between the (irrelevant) network load and the test's own
 * state.
 */
async function mountSpaces(
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

/** Mount a rail with the given threads in one expanded space. */
async function mount(threads: ChatSpaceThread[]): Promise<any> {
  return mountSpaces([SPACE], new Map([[SPACE.projectId, threads]]));
}

function threadNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-item .thread-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function groupNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-group-header .group-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

function spaceNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.space-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('space rail — unread filter also filters threads within a space', () => {
  it('hides read threads within a shown space, keeps unread ones', async () => {
    const unread = thread({ id: 't-unread', name: 'unread-thread', hasUnread: true });
    const read = thread({ id: 't-read', name: 'read-thread', hasUnread: false });
    const el = await mount([unread, read]);

    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = threadNames(el);
    expect(names).toContain('unread-thread');
    expect(names).not.toContain('read-thread');
  });

  it('keeps the currently open conversation visible even though it is read', async () => {
    const open = thread({ id: 't-open', name: 'open-thread', hasUnread: false });
    const other = thread({ id: 't-other', name: 'other-thread', hasUnread: false });
    const el = await mount([open, other]);

    el.selectedKey = 't-open';
    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = threadNames(el);
    expect(names).toContain('open-thread');
    expect(names).not.toContain('other-thread');
  });

  it('hides a thread group whose members are all read', async () => {
    const groupedRead = thread({ id: 't-grouped', name: 'grouped-thread', hasUnread: false });
    const ungroupedUnread = thread({ id: 't-unread', name: 'unread-thread', hasUnread: true });
    const el = await mount([groupedRead, ungroupedUnread]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-grouped'] }],
      },
    };

    el.spaceFilter = 'unread';
    await el.updateComplete;

    expect(groupNames(el)).not.toContain('My Group');
    expect(threadNames(el)).toContain('unread-thread');
    expect(threadNames(el)).not.toContain('grouped-thread');
  });

  it('treats a muted thread as read for the filter even though hasUnread is true', async () => {
    const muted = thread({ id: 't-muted', name: 'muted-thread', hasUnread: true, muted: true });
    const unread = thread({ id: 't-unread', name: 'unread-thread', hasUnread: true });
    const el = await mount([muted, unread]);

    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = threadNames(el);
    expect(names).toContain('unread-thread');
    expect(names).not.toContain('muted-thread');
  });

  it('shows a thread once it becomes unread while the filter is on (live update)', async () => {
    const t1 = thread({ id: 't-1', name: 'thread-one', hasUnread: false });
    const el = await mount([t1]);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(threadNames(el)).not.toContain('thread-one');

    // Same local update path the SSE handler and "Mark unread" use.
    el.markThreadUnread('t-1');
    await el.updateComplete;

    expect(threadNames(el)).toContain('thread-one');
  });

  it('hides a thread once read while the filter is on, unless it is the open one', async () => {
    const t1 = thread({ id: 't-1', name: 'thread-one', hasUnread: true });
    const t2 = thread({ id: 't-2', name: 'thread-two', hasUnread: true });
    const el = await mount([t1, t2]);
    el.selectedKey = 't-2';
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(threadNames(el)).toEqual(expect.arrayContaining(['thread-one', 'thread-two']));

    // A thread other than the open one gets read (e.g. auto-advance, or the
    // user reading it elsewhere) — it drops out of the filtered list.
    el.markThreadRead('t-1');
    await el.updateComplete;
    expect(threadNames(el)).not.toContain('thread-one');

    // The open conversation itself gets marked read (auto-advance) — it
    // must stay, so it doesn't vanish while the user is looking at it.
    el.markThreadRead('t-2');
    await el.updateComplete;
    expect(threadNames(el)).toContain('thread-two');
  });

  it('filters ungrouped threads and #general independently in a space that also has a group', async () => {
    const general = thread({
      id: 't-general',
      name: 'general-thread',
      isGeneral: true,
      hasUnread: false,
    });
    const ungroupedRead = thread({
      id: 't-ungrouped-read',
      name: 'ungrouped-read-thread',
      hasUnread: false,
    });
    const ungroupedUnread = thread({
      id: 't-ungrouped-unread',
      name: 'ungrouped-unread-thread',
      hasUnread: true,
    });
    const groupedUnread = thread({
      id: 't-grouped-unread',
      name: 'grouped-unread-thread',
      hasUnread: true,
    });
    const el = await mount([general, ungroupedRead, ungroupedUnread, groupedUnread]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-grouped-unread'] }],
      },
    };

    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = threadNames(el);
    expect(names).toContain('ungrouped-unread-thread');
    expect(names).toContain('grouped-unread-thread');
    expect(names).not.toContain('ungrouped-read-thread');
    expect(names).not.toContain('general-thread');
    expect(groupNames(el)).toContain('My Group');
  });

  it('shows a thread with only an unread mention, with no unread flag set', async () => {
    const mentioned = thread({
      id: 't-mention',
      name: 'mention-thread',
      hasUnread: false,
      hasUnreadMention: true,
    });
    const read = thread({ id: 't-read', name: 'read-thread', hasUnread: false });
    const el = await mount([mentioned, read]);

    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = threadNames(el);
    expect(names).toContain('mention-thread');
    expect(names).not.toContain('read-thread');
  });

  it('still shows the header of a collapsed group that has unread members', async () => {
    const unreadMember = thread({ id: 't-1', name: 'thread-one', hasUnread: true });
    const el = await mount([unreadMember]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [{ id: 'g-1', name: 'My Group', threadIds: ['t-1'] }],
      },
    };
    el.collapsedGroups = new Set(['g-1']);

    el.spaceFilter = 'unread';
    await el.updateComplete;

    expect(groupNames(el)).toContain('My Group');
    // Collapsed: the member list itself stays hidden.
    expect(el.shadowRoot.querySelector('.thread-group')).toBeNull();
  });

  it('restores all threads and group headers after toggling the filter back off', async () => {
    const groupedRead = thread({ id: 't-grouped', name: 'grouped-thread', hasUnread: false });
    const ungroupedRead = thread({
      id: 't-ungrouped-read',
      name: 'ungrouped-read-thread',
      hasUnread: false,
    });
    const el = await mount([groupedRead, ungroupedRead]);
    el.prefs = {
      ...el.prefs,
      threadGroups: {
        [SPACE.projectId]: [
          { id: 'g-1', name: 'Read Group', threadIds: ['t-grouped'] },
          // An empty group has no members with the filter off or on — it
          // exists purely as a drag target (see "New thread group"). It
          // must disappear only because the *filter* hides zero-member
          // groups, not unconditionally — so it has to come back once the
          // filter is off even though it is still, in fact, empty.
          { id: 'g-2', name: 'Empty Group', threadIds: [] },
        ],
      },
    };

    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(groupNames(el)).not.toContain('Read Group');
    expect(groupNames(el)).not.toContain('Empty Group');
    expect(threadNames(el)).not.toContain('grouped-thread');
    expect(threadNames(el)).not.toContain('ungrouped-read-thread');

    el.spaceFilter = 'all';
    await el.updateComplete;

    expect(groupNames(el)).toContain('Read Group');
    expect(groupNames(el)).toContain('Empty Group');
    expect(threadNames(el)).toContain('grouped-thread');
    expect(threadNames(el)).toContain('ungrouped-read-thread');
  });
});

describe("space rail — unread filter keeps only the open thread's space at zero unread", () => {
  it('keeps the space holding the open thread and drops the other, both at zero unread', async () => {
    const threadA = thread({ id: 't-a', name: 'a-thread', hasUnread: false });
    const threadB = thread({ id: 't-b', name: 'b-thread', hasUnread: false });
    const el = await mountSpaces(
      [SPACE_A, SPACE_B],
      new Map([
        [SPACE_A.projectId, [threadA]],
        [SPACE_B.projectId, [threadB]],
      ])
    );

    el.selectedKey = 't-b';
    el.spaceFilter = 'unread';
    await el.updateComplete;

    const names = spaceNames(el);
    expect(names).toContain('Beta');
    expect(names).not.toContain('Alpha');
  });

  it('hides every space when selectedKey matches no thread in any space (e.g. a DM)', async () => {
    const threadA = thread({ id: 't-a', name: 'a-thread', hasUnread: false });
    const threadB = thread({ id: 't-b', name: 'b-thread', hasUnread: false });
    const el = await mountSpaces(
      [SPACE_A, SPACE_B],
      new Map([
        [SPACE_A.projectId, [threadA]],
        [SPACE_B.projectId, [threadB]],
      ])
    );

    el.selectedKey = 'dm:user:someone';
    el.spaceFilter = 'unread';
    await el.updateComplete;

    expect(spaceNames(el)).toEqual([]);
  });
});
