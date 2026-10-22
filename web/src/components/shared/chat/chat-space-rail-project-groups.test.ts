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
 * Tests for project thread groups in the space rail (ptone/scion#4424,
 * design section 5 and section 8 phase 4). They are always on
 * (ptone/scion#5546).
 *
 * A small in-memory hub answers the rail's requests: the spaces and thread
 * lists, per-user preferences, and the `/api/v1/thread-groups` resource
 * (list, create, rename, delete, add and remove a thread). Each phase 4
 * acceptance item has its own test:
 *   - a group created by another user or an agent shows without a reload
 *     (the `chat.thread_group` event);
 *   - drag-into-group, rename and delete persist on the hub;
 *   - one user's reorder does not change another user's order;
 *   - migrated groups keep their top-level positions (5a rule 6);
 *   - the older personal groups are hidden and never changed.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { chatSpacesLoad } from '../../../client/chat-list-cache.js';
import { requestBodyText } from '../../../client/__fixtures__/request-url.js';
import { showToast } from '../../../utils/toast.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

const apiFetchMock = vi.mocked(apiFetch);
const showToastMock = vi.mocked(showToast);

const P = 'p-a';

interface HubThread {
  conversationId: string;
  threadId: string;
  name: string;
}

interface HubGroup {
  id: string;
  projectId: string;
  name: string;
  createdBy: string;
  createdByKind: 'user' | 'agent';
  threads: HubThread[];
}

/** The fake hub's state. `user` is whose preferences the next request reads. */
interface FakeHub {
  user: string;
  prefs: Record<string, Record<string, string>>;
  groups: HubGroup[];
  nextId: number;
  /** Answer the next request matching `method path-prefix` with this status. */
  failNext: { method: string; prefix: string; status: number } | null;
  /** Hold the answer to the next request matching `method path-prefix` until `release()`. */
  holdNext: { method: string; prefix: string } | null;
  /** Fail every user-prefs GET (a 500). */
  prefsDown: boolean;
  /** Also list a second space, Q (collapsed; its threads and groups are not loaded). */
  secondSpace: boolean;
}

const Q = 'p-b';

/** Releases the request held by `hub.holdNext` (answered with the hub's state at release). */
let release: () => void = () => {};

let hub: FakeHub;

const THREADS = [
  { id: 't1', name: 'alpha' },
  { id: 't2', name: 'bravo' },
  { id: 't3', name: 'charlie' },
  { id: 't4', name: 'delta' },
].map((t) => ({
  ...t,
  isGeneral: false,
  pinned: false,
  muted: false,
  hasUnread: false,
  hasUnreadMention: false,
}));

function hubThread(threadId: string): HubThread {
  const t = THREADS.find((x) => x.id === threadId)!;
  return { conversationId: `conv-${threadId}`, threadId, name: t.name };
}

function hubGroup(
  id: string,
  name: string,
  threadIds: string[],
  kind: 'user' | 'agent' = 'user'
): HubGroup {
  return {
    id,
    projectId: P,
    name,
    createdBy: kind === 'user' ? 'user-b' : 'agent-1',
    createdByKind: kind,
    threads: threadIds.map(hubThread),
  };
}

/** The wire shape of a group (what the API and the SSE event carry). */
function wire(g: HubGroup): Record<string, unknown> {
  return {
    ...g,
    createdAt: '2026-10-13T00:00:00Z',
    updatedAt: '2026-10-13T00:00:00Z',
    threads: g.threads.map((t) => ({ ...t, addedBy: 'user-b', addedAt: '2026-10-13T00:00:00Z' })),
  };
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(new Response(JSON.stringify(body), { status }));
}

function serveHub(): void {
  apiFetchMock.mockImplementation((path: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const hold = hub.holdNext;
    if (hold && hold.method === method && path.startsWith(hold.prefix)) {
      hub.holdNext = null;
      return new Promise<Response>((resolve) => {
        release = (): void => resolve(answer(path, init));
      });
    }
    return answer(path, init);
  });
}

function answer(path: string, init?: RequestInit): Promise<Response> {
  const method = init?.method ?? 'GET';
  const fail = hub.failNext;
  if (fail && fail.method === method && path.startsWith(fail.prefix)) {
    hub.failNext = null;
    return json({ error: { message: 'refused' } }, fail.status);
  }
  if (path === '/api/v1/chat/spaces') {
    return json({
      spaces: [
        {
          projectId: P,
          projectName: 'Alpha',
          projectSlug: 'alpha',
          unreadCount: 0,
          hasUnreadMention: false,
        },
        ...(hub.secondSpace
          ? [
              {
                projectId: Q,
                projectName: 'Bravo',
                projectSlug: 'bravo',
                unreadCount: 0,
                hasUnreadMention: false,
              },
            ]
          : []),
      ],
    });
  }
  if (path.startsWith('/api/v1/chat/spaces/')) return json({ threads: THREADS });
  if (path === '/api/v1/chat/user-prefs') {
    if (method === 'GET' && hub.prefsDown) return json({}, 500);
    if (method === 'PUT') {
      const body = JSON.parse(requestBodyText(init!.body)) as Record<string, string>;
      hub.prefs[hub.user] = body;
    }
    return json(hub.prefs[hub.user] ?? {});
  }
  if (path === `/api/v1/thread-groups?projectId=${P}`) {
    return json({ threadGroups: hub.groups.map(wire) });
  }
  if (path === '/api/v1/thread-groups' && method === 'POST') {
    const body = JSON.parse(requestBodyText(init!.body)) as { projectId: string; name: string };
    const key = body.name.trim().toLowerCase();
    if (hub.groups.some((g) => g.name.trim().toLowerCase() === key)) {
      return json({ error: { message: 'conflict' } }, 409);
    }
    const g = hubGroup(`srv-${hub.nextId++}`, body.name, []);
    hub.groups.push(g);
    return json(wire(g), 201);
  }
  const threadsMatch = /^\/api\/v1\/thread-groups\/([^/]+)\/threads(?:\/([^/]+))?$/.exec(path);
  if (threadsMatch) {
    const g = hub.groups.find((x) => x.id === decodeURIComponent(threadsMatch[1]!));
    if (!g) return json({ error: { message: 'not found' } }, 404);
    if (method === 'POST') {
      const { threadId } = JSON.parse(requestBodyText(init!.body)) as { threadId: string };
      for (const other of hub.groups) {
        other.threads = other.threads.filter((t) => t.threadId !== threadId);
      }
      g.threads.push(hubThread(threadId));
      return json(wire(g));
    }
    if (method === 'DELETE') {
      const conv = decodeURIComponent(threadsMatch[2] ?? '');
      g.threads = g.threads.filter((t) => t.conversationId !== conv);
      return Promise.resolve(new Response(null, { status: 204 }));
    }
  }
  const groupMatch = /^\/api\/v1\/thread-groups\/([^/?]+)$/.exec(path);
  if (groupMatch) {
    const id = decodeURIComponent(groupMatch[1]!);
    const g = hub.groups.find((x) => x.id === id);
    if (!g) return json({ error: { message: 'not found' } }, 404);
    if (method === 'PATCH') {
      const { name } = JSON.parse(requestBodyText(init!.body)) as { name: string };
      const key = name.trim().toLowerCase();
      if (hub.groups.some((x) => x.id !== id && x.name.trim().toLowerCase() === key)) {
        return json({ error: { message: 'conflict' } }, 409);
      }
      g.name = name;
      return json(wire(g));
    }
    if (method === 'DELETE') {
      hub.groups = hub.groups.filter((x) => x.id !== id);
      return Promise.resolve(new Response(null, { status: 204 }));
    }
  }
  return json({});
}

/** Requests the rail made, as "METHOD path". */
function requests(): string[] {
  return apiFetchMock.mock.calls.map(
    (c: any[]) => `${(c[1] as RequestInit | undefined)?.method ?? 'GET'} ${c[0] as string}`
  );
}

function threadGroupRequests(): string[] {
  return requests().filter((r) => r.includes('/api/v1/thread-groups'));
}

/** The bodies of every user-prefs PUT so far. */
function prefsPuts(): Record<string, string>[] {
  return apiFetchMock.mock.calls
    .filter((c: any[]) => c[0] === '/api/v1/chat/user-prefs' && c[1]?.method === 'PUT')
    .map((c: any[]) => JSON.parse(requestBodyText(c[1].body)) as Record<string, string>);
}

/** Let pending fetches and renders finish. */
async function settle(el: any): Promise<void> {
  for (let i = 0; i < 8; i++) {
    await new Promise((resolve) => setTimeout(resolve, 0));
    await el.updateComplete;
  }
}

/** Mount a rail as `user` with space p-a open, and wait for its threads (and groups). */
async function mountAs(user: string): Promise<any> {
  hub.user = user;
  chatSpacesLoad.invalidate();
  const el = document.createElement('scion-chat-space-rail') as any;
  el.currentUserId = user;
  el.selectedProjectId = P;
  document.body.appendChild(el);
  await settle(el);
  expect(el.threadsBySpace.has(P)).toBe(true);
  return el;
}

/** Group names in the order rendered. */
function renderedGroupNames(el: any): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('.thread-group-header .group-name')).map(
    (n) => (n as HTMLElement).textContent?.trim() ?? ''
  );
}

/** The space's groups as {name: threadIds}. */
function groupsOf(el: any): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const g of el.getGroups(P)) out[g.name] = [...g.threadIds];
  return out;
}

/** The space's top-level display order, with group IDs shown by name. */
function topLevel(el: any): string[] {
  const names = new Map<string, string>(el.getGroups(P).map((g: any) => [g.id, `[${g.name}]`]));
  return el.currentTopLevelOrder(P).map((id: string) => names.get(id) ?? id);
}

function personalPrefs(
  threadGroups: unknown,
  threadOrder?: Record<string, string[]>
): Record<string, string> {
  return {
    spaceSortMode: 'alpha',
    threadSortMode: 'alpha',
    spaceOrder: '',
    threadOrder: JSON.stringify(threadOrder ?? {}),
    threadGroups: JSON.stringify(threadGroups),
  };
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  hub = {
    user: 'user-a',
    prefs: {},
    groups: [],
    nextId: 1,
    failNext: null,
    holdNext: null,
    prefsDown: false,
    secondSpace: false,
  };
  serveHub();
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
  localStorage.clear();
});

describe('project thread groups', () => {
  it('loads the project groups with the space and hides personal groups', async () => {
    const personal = { [P]: [{ id: 'g-personal', name: 'Mine', threadIds: ['t1'] }] };
    hub.prefs['user-a'] = personalPrefs(personal);
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2', 't3'])];

    const el = await mountAs('user-a');

    expect(requests()).toContain(`GET /api/v1/thread-groups?projectId=${P}`);
    expect(renderedGroupNames(el)).toEqual(['Shared']);
    expect(groupsOf(el)).toEqual({ Shared: ['t2', 't3'] });
    // Personal groups are hidden, not deleted.
    expect(el.prefs.threadGroups).toEqual(personal);
  });

  it('shows a group that another user or an agent creates, without a reload', async () => {
    const el = await mountAs('user-a');
    const before = requests().length;

    el.applyThreadGroupEvent(P, 'created', wire(hubGroup('srv-9', 'From B', ['t1'])));
    el.applyThreadGroupEvent(P, 'created', wire(hubGroup('srv-10', 'From agent', ['t2'], 'agent')));
    await el.updateComplete;

    expect(renderedGroupNames(el).sort()).toEqual(['From B', 'From agent']);
    expect(groupsOf(el)).toEqual({ 'From B': ['t1'], 'From agent': ['t2'] });
    // Applied from the event payload: no refetch.
    expect(requests().length).toBe(before);
  });

  it('reconciles updates and deletes from events, one group per thread', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1', 't2']), hubGroup('srv-2', 'Two', [])];
    const el = await mountAs('user-a');

    // Another user moved t1 into Two: Two's event arrives before One's.
    el.applyThreadGroupEvent(P, 'updated', wire(hubGroup('srv-2', 'Two', ['t1'])));
    await el.updateComplete;
    expect(groupsOf(el)).toEqual({ One: ['t2'], Two: ['t1'] });

    el.applyThreadGroupEvent(P, 'updated', wire(hubGroup('srv-1', 'Renamed', ['t2'])));
    el.applyThreadGroupEvent(P, 'deleted', wire(hubGroup('srv-2', 'Two', ['t1'])));
    // A late update for the deleted group does not bring it back.
    el.applyThreadGroupEvent(P, 'updated', wire(hubGroup('srv-2', 'Two', ['t1'])));
    await el.updateComplete;
    expect(groupsOf(el)).toEqual({ Renamed: ['t2'] });
    expect(renderedGroupNames(el)).toEqual(['Renamed']);
  });

  it('ignores an event whose group belongs to another project', async () => {
    const el = await mountAs('user-a');
    el.applyThreadGroupEvent(P, 'created', {
      ...wire(hubGroup('srv-1', 'X', ['t1'])),
      projectId: 'p-z',
    });
    await el.updateComplete;
    expect(groupsOf(el)).toEqual({});
  });

  it('drag-into-group persists on the hub, not in personal prefs', async () => {
    const personalDoc = JSON.stringify({
      [P]: [{ id: 'g-personal', name: 'Mine', threadIds: [] }],
    });
    hub.prefs['user-a'] = { ...personalPrefs({}), threadGroups: personalDoc };
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    const el = await mountAs('user-a');

    // Drop t3 onto the group header.
    el.draggingThreadId = 't3';
    await el.handleGroupDrop({ preventDefault: vi.fn() }, 'srv-1', P);
    await settle(el);

    expect(threadGroupRequests()).toContain('POST /api/v1/thread-groups/srv-1/threads');
    expect(hub.groups[0]!.threads.map((t) => t.threadId)).toEqual(['t1', 't3']);
    expect(groupsOf(el)).toEqual({ Shared: ['t1', 't3'] });

    // Drop t4 onto a member row: filed on the hub, and the drop position is
    // this user's order, saved in their prefs. The personal groups document
    // goes back byte for byte (the hub's migration marker stays set).
    el.draggingThreadId = 't4';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't1', P);
    await settle(el);
    expect(hub.groups[0]!.threads.map((t) => t.threadId)).toEqual(['t1', 't3', 't4']);
    const put = prefsPuts().at(-1)!;
    expect(put.threadGroups).toBe(personalDoc);
    const order = JSON.parse(put.threadOrder)[P] as string[];
    // Members follow the group's ID, t4 where it was dropped (before t1).
    expect(order.slice(order.indexOf('srv-1'), order.indexOf('srv-1') + 4)).toEqual([
      'srv-1',
      't4',
      't1',
      't3',
    ]);
  });

  it('removing a thread from a group calls the hub with its conversation ID', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1', 't2'])];
    const el = await mountAs('user-a');

    await el.removeThreadFromGroup('t1', P);
    await settle(el);

    expect(threadGroupRequests()).toContain('DELETE /api/v1/thread-groups/srv-1/threads/conv-t1');
    expect(hub.groups[0]!.threads.map((t) => t.threadId)).toEqual(['t2']);
    expect(groupsOf(el)).toEqual({ Shared: ['t2'] });
  });

  it('creates a group on the hub and files the thread into it', async () => {
    const el = await mountAs('user-a');

    await el.createGroup(P, 'Release', 't2');
    await settle(el);

    expect(threadGroupRequests()).toEqual(
      expect.arrayContaining([
        'POST /api/v1/thread-groups',
        'POST /api/v1/thread-groups/srv-1/threads',
      ])
    );
    expect(hub.groups.map((g) => [g.name, g.threads.map((t) => t.threadId)])).toEqual([
      ['Release', ['t2']],
    ]);
    expect(groupsOf(el)).toEqual({ Release: ['t2'] });
    // No personal group was written.
    for (const put of prefsPuts()) expect(put.threadGroups).not.toContain('Release');
  });

  it('reports a duplicate name on create and creates nothing', async () => {
    hub.groups = [hubGroup('srv-1', 'Release', [])];
    const el = await mountAs('user-a');

    await el.createGroup(P, 'release');
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith(
      'A group with that name already exists in this space',
      'warning'
    );
    expect(Object.keys(groupsOf(el))).toEqual(['Release']);
  });

  it('rename persists on the hub; a conflict is reported and undone', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1']), hubGroup('srv-2', 'Two', [])];
    const el = await mountAs('user-a');

    await el.renameGroup('srv-1', 'Uno', P);
    await settle(el);
    expect(threadGroupRequests()).toContain('PATCH /api/v1/thread-groups/srv-1');
    expect(hub.groups[0]!.name).toBe('Uno');
    expect(renderedGroupNames(el).sort()).toEqual(['Two', 'Uno']);

    await el.renameGroup('srv-1', 'two', P);
    await settle(el);
    expect(showToastMock).toHaveBeenCalledWith(
      'A group with that name already exists in this space',
      'warning'
    );
    // The optimistic rename is undone by the reload of the hub's groups.
    expect(hub.groups[0]!.name).toBe('Uno');
    expect(renderedGroupNames(el).sort()).toEqual(['Two', 'Uno']);
  });

  it('delete persists on the hub; the threads stay, ungrouped, where the group was', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2', 't3'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    expect(topLevel(el)).toEqual(['t1', '[Shared]', 't4']);

    await el.deleteGroup('srv-1', P);
    await settle(el);

    expect(threadGroupRequests()).toContain('DELETE /api/v1/thread-groups/srv-1');
    expect(hub.groups).toEqual([]);
    expect(groupsOf(el)).toEqual({});
    expect(topLevel(el)).toEqual(['t1', 't2', 't3', 't4']);
  });

  it('a failed edit is reported and the hub state comes back', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    const el = await mountAs('user-a');

    hub.failNext = { method: 'DELETE', prefix: '/api/v1/thread-groups/srv-1', status: 500 };
    await el.deleteGroup('srv-1', P);
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith('Couldn’t delete the group', 'warning');
    expect(groupsOf(el)).toEqual({ Shared: ['t1'] });
    // And a later event for it applies again.
    el.applyThreadGroupEvent(P, 'updated', wire(hubGroup('srv-1', 'Shared', ['t1', 't2'])));
    expect(groupsOf(el)).toEqual({ Shared: ['t1', 't2'] });
  });

  it('a groups list read before an edit landed does not undo it', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    const el = await mountAs('user-a');

    // A stale list (from before the edit) arrives after the edit started.
    const stale = { ok: true, seq: el._groupsMutationSeq.get(P) ?? 0, groups: [] };
    const editing = el.renameGroup('srv-1', 'Renamed', P);
    el.commitProjectGroups(P, stale);
    await editing;
    await settle(el);

    expect(groupsOf(el)).toEqual({ Renamed: ['t1'] });
  });

  it("one user's reorder does not change another user's order", async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1', 't2', 't3'])];
    hub.prefs['user-b'] = personalPrefs({});
    const railB = await mountAs('user-b');
    const orderB = railB.currentGroupThreadOrder(P, 'srv-1');
    const topB = topLevel(railB);
    expect(orderB).toEqual(['t1', 't2', 't3']); // alpha

    const railA = await mountAs('user-a');
    // A drags t3 above t1 inside the group, and moves t4 below the group.
    railA.draggingThreadId = 't3';
    await railA.handleThreadDrop({ preventDefault: vi.fn() }, 't1', P);
    await settle(railA);
    await railA.moveThread('t4', P, 1);
    await settle(railA);
    expect(railA.currentGroupThreadOrder(P, 'srv-1')).toEqual(['t3', 't1', 't2']);
    expect(topLevel(railA)).toEqual(['[Shared]', 't4']);

    // The order went only to A's own prefs; the hub groups are untouched.
    expect(threadGroupRequests().filter((r) => !r.startsWith('GET '))).toEqual([]);
    expect(hub.prefs['user-b']!.threadOrder).toBe('{}');
    // B's rail, reloading, still shows B's order.
    hub.user = 'user-b';
    await railB.reload();
    await settle(railB);
    expect(railB.currentGroupThreadOrder(P, 'srv-1')).toEqual(orderB);
    expect(topLevel(railB)).toEqual(topB);

    // A's in-group order survives A's own reload: the hub lists the members
    // in the order they were added, and A's order comes from A's prefs.
    hub.user = 'user-a';
    await railA.reload();
    await settle(railA);
    expect(groupsOf(railA)).toEqual({ Shared: ['t1', 't2', 't3'] });
    expect(railA.currentGroupThreadOrder(P, 'srv-1')).toEqual(['t3', 't1', 't2']);
    expect(topLevel(railA)).toEqual(['[Shared]', 't4']);
  });

  it('migrated groups keep their top-level positions (5a rule 6)', async () => {
    // The migration put the project group ID right after the personal one.
    const personal = { [P]: [{ id: 'g-personal', name: 'Release', threadIds: ['t2'] }] };
    hub.prefs['user-a'] = personalPrefs(personal, {
      [P]: ['t3', 'g-personal', 'srv-1', 't1', 't4'],
    });
    hub.groups = [hubGroup('srv-1', 'Release', ['t2'])];
    const el = await mountAs('user-a');

    expect(topLevel(el)).toEqual(['t3', '[Release]', 't1', 't4']);

    // A reorder keeps the hidden personal ID where it was.
    el.draggingThreadId = 't4';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't3', P);
    await settle(el);
    expect(topLevel(el)).toEqual(['t4', 't3', '[Release]', 't1']);
    const order = JSON.parse(prefsPuts().at(-1)!.threadOrder)[P] as string[];
    expect(order).toEqual(['t4', 't3', 'g-personal', 'srv-1', 't2', 't1']);
  });

  it('edits and reorders never change the personal groups document', async () => {
    const personal = { [P]: [{ id: 'g-personal', name: 'Mine', threadIds: ['t1', 't4'] }] };
    const personalDoc = JSON.stringify(personal);
    hub.prefs['user-a'] = {
      ...personalPrefs({}, { [P]: ['t3', 'g-personal', 'srv-1', 't2'] }),
      threadGroups: personalDoc,
    };
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    const el = await mountAs('user-a');
    expect(renderedGroupNames(el)).toEqual(['Shared']);

    await el.moveThreadToGroup('t1', 'srv-1', P);
    await el.moveThread('t3', P, 1);
    await settle(el);
    expect(prefsPuts().length).toBeGreaterThan(0);
    for (const put of prefsPuts()) expect(put.threadGroups).toBe(personalDoc);
    expect(el.prefs.threadGroups).toEqual(personal);
    expect(renderedGroupNames(el)).toEqual(['Shared']);
  });

  it('a failed groups load shows the threads ungrouped and retries on the next reload', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    hub.failNext = { method: 'GET', prefix: '/api/v1/thread-groups', status: 500 };
    const el = await mountAs('user-a');
    expect(renderedGroupNames(el)).toEqual([]);
    expect(threadGroupRequests()).toHaveLength(1);

    await el.reload();
    await settle(el);
    expect(renderedGroupNames(el)).toEqual(['Shared']);
  });

  it('a failed membership drag is undone, with the thread and its order where they were', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1', 't3'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t4', 'srv-1', 't1', 't3', 't2'] });
    const el = await mountAs('user-a');
    const putsBefore = prefsPuts().length;

    hub.failNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads', status: 422 };
    el.draggingThreadId = 't4';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't1', P);
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith(
      'This group has reached its thread limit',
      'warning'
    );
    expect(hub.groups[0]!.threads.map((t) => t.threadId)).toEqual(['t1', 't3']);
    expect(groupsOf(el)).toEqual({ Shared: ['t1', 't3'] });
    // No order was saved for the refused move: t4 is still first.
    expect(prefsPuts().length).toBe(putsBefore);
    expect(topLevel(el)).toEqual(['t4', '[Shared]', 't2']);
  });

  it('a thread filed from the menu and refused keeps its place in the order', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    const putsBefore = prefsPuts().length;

    hub.failNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads', status: 403 };
    await el.moveThreadToGroup('t1', 'srv-1', P);
    await settle(el);

    expect(prefsPuts().length).toBe(putsBefore);
    expect(groupsOf(el)).toEqual({ Shared: ['t2'] });
    expect(topLevel(el)).toEqual(['t1', '[Shared]', 't3', 't4']);
  });

  it('a drag out of a group onto an ungrouped row removes it on the hub', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1', 't2'])];
    const el = await mountAs('user-a');

    el.draggingThreadId = 't1';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't4', P);
    await settle(el);

    expect(threadGroupRequests()).toContain('DELETE /api/v1/thread-groups/srv-1/threads/conv-t1');
    expect(hub.groups[0]!.threads.map((t) => t.threadId)).toEqual(['t2']);
    expect(groupsOf(el)).toEqual({ Shared: ['t2'] });
    // Dropped just above t4, at the top level.
    const order = topLevel(el);
    expect(order.indexOf('t1')).toBe(order.indexOf('t4') - 1);
  });

  it('an event during an optimistic edit is applied, and the edit answer wins', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    const el = await mountAs('user-a');

    hub.holdNext = { method: 'PATCH', prefix: '/api/v1/thread-groups/srv-1' };
    const renaming = el.renameGroup('srv-1', 'Renamed', P);
    await settle(el);
    expect(groupsOf(el)).toEqual({ Renamed: ['t1'] });

    // Meanwhile another user files t2 into the group.
    hub.groups[0]!.threads.push(hubThread('t2'));
    el.applyThreadGroupEvent(P, 'updated', wire(hub.groups[0]!));
    expect(groupsOf(el)).toEqual({ Shared: ['t1', 't2'] });

    release();
    await renaming;
    await settle(el);
    expect(groupsOf(el)).toEqual({ Renamed: ['t1', 't2'] });
  });

  it('a failed edit is rolled back even while another edit in the space is in flight', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1']), hubGroup('srv-2', 'Two', ['t2'])];
    const el = await mountAs('user-a');

    hub.holdNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads' };
    const filing = el.moveThreadToGroup('t3', 'srv-1', P);
    hub.failNext = { method: 'DELETE', prefix: '/api/v1/thread-groups/srv-2', status: 403 };
    await el.deleteGroup('srv-2', P);
    await settle(el);
    expect(showToastMock).toHaveBeenCalledWith(
      'You don’t have permission to change this space’s groups',
      'warning'
    );

    release();
    await filing;
    await settle(el);

    expect(groupsOf(el)).toEqual({ One: ['t1', 't3'], Two: ['t2'] });
  });

  it('a failed delete leaves the group where it was in the user’s order', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    const putsBefore = prefsPuts().length;

    hub.failNext = { method: 'DELETE', prefix: '/api/v1/thread-groups/srv-1', status: 403 };
    await el.deleteGroup('srv-1', P);
    await settle(el);

    expect(prefsPuts().length).toBe(putsBefore);
    expect(topLevel(el)).toEqual(['t1', '[Shared]', 't3', 't4']);
  });

  it('shows the hub’s message for a rejected group name', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', [])];
    const el = await mountAs('user-a');

    hub.failNext = { method: 'PATCH', prefix: '/api/v1/thread-groups/srv-1', status: 400 };
    await el.renameGroup('srv-1', 'x'.repeat(101), P);
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith('refused', 'warning');
    expect(renderedGroupNames(el)).toEqual(['Shared']);
  });

  it('a thread filed from the menu joins the end of the group in an ordered space', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');

    await el.moveThreadToGroup('t1', 'srv-1', P);
    await settle(el);

    expect(el.currentGroupThreadOrder(P, 'srv-1')).toEqual(['t2', 't1']);
    const order = JSON.parse(prefsPuts().at(-1)!.threadOrder)[P] as string[];
    expect(order).toEqual(['srv-1', 't2', 't1', 't3', 't4']);
  });

  it('removing a member with no known conversation ID says so and sends nothing', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    hub.groups[0]!.threads[0]!.conversationId = '';
    const el = await mountAs('user-a');

    await el.removeThreadFromGroup('t1', P);
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith(
      'Couldn’t remove the thread from the group',
      'warning'
    );
    expect(threadGroupRequests().filter((r) => r.startsWith('DELETE'))).toEqual([]);
    expect(groupsOf(el)).toEqual({ Shared: ['t1'] });
  });

  it('never saves preferences before they have loaded (personal groups survive)', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    hub.prefsDown = true;
    const el = await mountAs('user-a');
    const prefsGets = (): number =>
      requests().filter((r) => r === 'GET /api/v1/chat/user-prefs').length;
    const getsBefore = prefsGets();

    await el.moveThread('t2', P, 1);
    el.draggingThreadId = 't3';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't1', P);
    await settle(el);

    expect(prefsPuts()).toEqual([]);
    expect(showToastMock).toHaveBeenCalledWith(
      'Couldn’t save your sidebar order. Try again in a moment.',
      'warning'
    );
    // The preferences are asked for again.
    expect(prefsGets()).toBeGreaterThan(getsBefore);

    // Once they load, saving works again and the stored personal groups
    // document goes back byte for byte.
    const personalDoc = JSON.stringify({
      [P]: [{ id: 'g-personal', name: 'Mine', threadIds: [] }],
    });
    hub.prefs['user-a'] = { ...personalPrefs({}), threadGroups: personalDoc };
    hub.prefsDown = false;
    await el.reload();
    await settle(el);
    await el.moveThread('t2', P, 1);
    await settle(el);
    expect(prefsPuts()).toHaveLength(1);
    expect(prefsPuts()[0]!.threadGroups).toBe(personalDoc);
  });

  it('writes back an absent personal groups document as empty, not "{}"', async () => {
    hub.prefs['user-a'] = { spaceSortMode: 'alpha', threadSortMode: 'alpha', threadOrder: '{}' };
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1'])];
    const el = await mountAs('user-a');

    await el.moveThread('t2', P, 1);
    await settle(el);

    expect(prefsPuts().at(-1)!.threadGroups).toBe('');
  });

  it('reads the preferences again before saving an order after a new group arrived', async () => {
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;

    // The migration writes its group's position into the stored order and
    // publishes the group.
    hub.groups = [hubGroup('srv-9', 'Migrated', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-9', 't2', 't3', 't4'] });
    el.applyThreadGroupEvent(P, 'created', wire(hub.groups[0]!));
    await el.updateComplete;

    await el.moveThread('t4', P, -1);
    await settle(el);

    const order = JSON.parse(prefsPuts().at(-1)!.threadOrder)[P] as string[];
    expect(order).toEqual(['t1', 'srv-9', 't2', 't4', 't3']);
  });

  it('a rollback read dropped by a later edit is read again', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1']), hubGroup('srv-2', 'Two', ['t2'])];
    const el = await mountAs('user-a');

    // The rename is refused (409); hold the rollback read it owes.
    hub.holdNext = { method: 'GET', prefix: '/api/v1/thread-groups?projectId=' };
    await el.renameGroup('srv-1', 'Two', P);
    await settle(el);
    // The refused name still shows while the rollback read is held.
    expect(el.getGroups(P).map((g: any) => g.name)).toEqual(['Two', 'Two']);

    // Another edit starts and ends before the held read answers: that read
    // is dropped, and the rollback must be read again.
    await el.renameGroup('srv-2', 'Deux', P);
    release();
    await settle(el);

    expect(groupsOf(el)).toEqual({ One: ['t1'], Deux: ['t2'] });
  });

  it('a refused edit is rolled back when a create with a thread follows it', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1']), hubGroup('srv-2', 'Two', ['t2'])];
    hub.nextId = 3;
    const el = await mountAs('user-a');

    hub.holdNext = { method: 'PATCH', prefix: '/api/v1/thread-groups/srv-1' };
    const renaming = el.renameGroup('srv-1', 'Two', P); // will be refused (409)
    await settle(el);
    const creating = el.createGroup(P, 'X', 't3');
    release();
    await Promise.all([renaming, creating]);
    await settle(el);

    expect(showToastMock).toHaveBeenCalledWith(
      'A group with that name already exists in this space',
      'warning'
    );
    expect(groupsOf(el)).toEqual({ One: ['t1'], Two: ['t2'], X: ['t3'] });
  });

  it('interleaved edits with a failure end at the hub’s state', async () => {
    hub.groups = [hubGroup('srv-1', 'One', ['t1']), hubGroup('srv-2', 'Two', ['t2'])];
    const el = await mountAs('user-a');

    hub.holdNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads' };
    const a = el.moveThreadToGroup('t3', 'srv-1', P); // held
    const b = el.renameGroup('srv-2', 'one', P); // refused at once (409)
    const c = el.moveThreadToGroup('t4', 'srv-2', P); // accepted
    await Promise.all([b, c]);
    await settle(el);
    release();
    await a;
    await settle(el);

    expect(groupsOf(el)).toEqual({ One: ['t1', 't3'], Two: ['t2', 't4'] });
    expect(groupsOf(el)).toEqual(
      Object.fromEntries(hub.groups.map((g) => [g.name, g.threads.map((t) => t.threadId)]))
    );
  });

  it('a new group for a space whose groups are not loaded makes the next save read first', async () => {
    hub.secondSpace = true;
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2', 't3', 't4'], [Q]: ['t1', 't2'] });
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;
    expect(el.projectGroups.has(Q)).toBe(false);

    // The migration writes Q's group position and publishes the group.
    const migratedQ = ['t1', 'srv-q', 't2'];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2', 't3', 't4'], [Q]: migratedQ });
    el.applyThreadGroupEvent(Q, 'created', {
      ...wire(hubGroup('srv-q', 'Q group', [])),
      projectId: Q,
    });

    await el.moveThread('t4', P, -1);
    await settle(el);

    expect(JSON.parse(prefsPuts().at(-1)!.threadOrder)[Q]).toEqual(migratedQ);
  });

  it('a space drag after a stale mark reads the preferences first', async () => {
    hub.secondSpace = true;
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2'], [Q]: ['t1'] });
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;

    const migratedQ = ['t1', 'srv-q'];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2'], [Q]: migratedQ });
    el.applyThreadGroupEvent(Q, 'created', {
      ...wire(hubGroup('srv-q', 'Q group', [])),
      projectId: Q,
    });

    await el.applySpaceOrder([Q, P]);
    await settle(el);

    const put = prefsPuts().at(-1)!;
    expect(JSON.parse(put.threadOrder)[Q]).toEqual(migratedQ);
    expect(JSON.parse(put.spaceOrder)).toEqual([Q, P]);
  });

  it('an update for a shown group the stored order does not record marks prefs stale', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;
    expect(el._prefsStale).toBe(false);

    // The migration reused the group another user created (rule 2) and
    // wrote it into this user's order (rule 6).
    el.applyThreadGroupEvent(P, 'updated', wire(hubGroup('srv-1', 'Shared', ['t2', 't3'])));

    expect(el._prefsStale).toBe(true);
  });

  it('a reorder made while a row drop awaits the hub is kept', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 'srv-1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');

    // Drop t4 onto t2 (into the group, above t2); hold the hub's answer.
    hub.holdNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads' };
    el.draggingThreadId = 't4';
    const dropping = el.handleThreadDrop({ preventDefault: vi.fn() }, 't2', P);
    await settle(el);
    // Meanwhile the user moves t3 above the group.
    await el.moveThread('t3', P, -1);
    await settle(el);
    expect(topLevel(el)).toEqual(['t1', 't3', '[Shared]']);

    release();
    await dropping;
    await settle(el);

    // Both the nudge and the drop position are kept.
    expect(topLevel(el)).toEqual(['t1', 't3', '[Shared]']);
    expect(el.currentGroupThreadOrder(P, 'srv-1')).toEqual(['t4', 't2']);
    const order = JSON.parse(prefsPuts().at(-1)!.threadOrder)[P] as string[];
    expect(order).toEqual(['t1', 't3', 'srv-1', 't4', 't2']);
  });

  it('a row drop accepted after a stale mark saves on fresh preferences', async () => {
    hub.secondSpace = true;
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs(
      {},
      { [P]: ['t1', 'srv-1', 't2', 't3', 't4'], [Q]: ['t1'] }
    );
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;

    hub.holdNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads' };
    el.draggingThreadId = 't4';
    const dropping = el.handleThreadDrop({ preventDefault: vi.fn() }, 't2', P);
    await settle(el);
    // During the round trip the migration writes Q's order and publishes.
    const migratedQ = ['t1', 'srv-q'];
    hub.prefs['user-a'] = personalPrefs(
      {},
      { [P]: ['t1', 'srv-1', 't2', 't3', 't4'], [Q]: migratedQ }
    );
    el.applyThreadGroupEvent(Q, 'created', {
      ...wire(hubGroup('srv-q', 'Q group', [])),
      projectId: Q,
    });

    release();
    await dropping;
    await settle(el);

    const put = JSON.parse(prefsPuts().at(-1)!.threadOrder) as Record<string, string[]>;
    expect(put[Q]).toEqual(migratedQ);
    expect(put[P]).toEqual(['t1', 'srv-1', 't4', 't2', 't3']);
  });

  it('saves nothing while stale preferences cannot be read', async () => {
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t1', 't2', 't3', 't4'] });
    const el = await mountAs('user-a');
    el._prefsRefreshDelayMs = 60_000;

    el.applyThreadGroupEvent(P, 'created', wire(hubGroup('srv-9', 'New', [])));
    hub.prefsDown = true;
    await el.moveThread('t4', P, -1);
    await el.applySpaceOrder([P]);
    await settle(el);

    expect(prefsPuts()).toEqual([]);
    expect(showToastMock).toHaveBeenCalledWith(
      'Couldn’t save your sidebar order. Try again in a moment.',
      'warning'
    );
  });

  it('a thread filed from the menu joins after the members the group has when the hub answers', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t2'])];
    hub.prefs['user-a'] = personalPrefs({}, { [P]: ['t4', 't1', 'srv-1', 't2', 't3'] });
    const el = await mountAs('user-a');

    hub.holdNext = { method: 'POST', prefix: '/api/v1/thread-groups/srv-1/threads' };
    const filing = el.moveThreadToGroup('t4', 'srv-1', P);
    await settle(el);
    // During the round trip another user files t3 into the group.
    hub.groups[0]!.threads.push(hubThread('t3'));
    el.applyThreadGroupEvent(P, 'updated', wire(hub.groups[0]!));
    release();
    await filing;
    await settle(el);

    // t4 goes after the group's last member as the hub answered (t3), not
    // after the last member it had when the file started (t2).
    const order = JSON.parse(prefsPuts().at(-1)!.threadOrder)[P] as string[];
    expect(order).toEqual(['t1', 'srv-1', 't2', 't3', 't4']);
    expect(el.currentGroupThreadOrder(P, 'srv-1')).toEqual(['t2', 't3', 't4']);
  });

  it('a remove that had to look up the conversation ID applies to the groups it read', async () => {
    hub.groups = [hubGroup('srv-1', 'Shared', ['t1', 't2']), hubGroup('srv-2', 'Two', [])];
    hub.groups[0]!.threads[0]!.conversationId = '';
    const el = await mountAs('user-a');

    // The hub now knows t1's conversation, and has changed since the rail read it.
    hub.groups[0]!.threads[0]!.conversationId = 'conv-t1';
    hub.groups[0]!.threads.push(hubThread('t3'));
    hub.groups[1]!.name = 'Zwei';

    el.draggingThreadId = 't1';
    await el.handleThreadDrop({ preventDefault: vi.fn() }, 't4', P);
    await settle(el);

    expect(threadGroupRequests()).toContain('DELETE /api/v1/thread-groups/srv-1/threads/conv-t1');
    // The fresh state minus t1, not the drag-time snapshot.
    expect(groupsOf(el)).toEqual({ Shared: ['t2', 't3'], Zwei: [] });
  });
});
