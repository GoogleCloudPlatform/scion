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
 * The chat page's wiring of the rail's Unread DMs list and the unread
 * badge:
 * - `loadUnreadDMPeers` feeds `v2DMList` from `/chat/dms`, and an unchanged
 *   answer does not replace it (no rail re-render on a quiet poll).
 * - The rail gets the list as `.dms`, and its `dm-select` opens the DM whose
 *   key is the listed one.
 * - Marking a DM unread (here, from another tab over SSE) puts it back in
 *   the list at once and reloads the list, as the badge refreshes on the
 *   same event.
 * - While the page is up, the badge is the rail's lists' sum.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';
import { apiFetch } from '../../client/api.js';
import { chatDMsLoad, chatSpacesLoad } from '../../client/chat-list-cache.js';
import { chatUnread } from '../../client/chat-unread.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const fakeState = vi.hoisted(() => {
  const t = new EventTarget() as any;
  t.agents = new Map<string, any>();
  t.getAgents = () => Array.from(t.agents.values());
  t.getDeletedAgentIds = () => new Set<string>();
  t.seedAgents = () => {};
  t.removeAgent = () => {};
  t.getAgent = (id: string) => t.agents.get(id);
  t.scopeGeneration = 0;
  t.beginSeedEpoch = () => Symbol('seed-epoch');
  t.endSeedEpoch = () => {};
  return t;
});

vi.mock('../../client/main.js', async () => ({
  ...(await import('../../client/__fixtures__/main-stub.js')),
  stateManager: fakeState,
}));
vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));
// The rail renders as a bare element: these tests read the page's bindings.
vi.mock('../shared/chat/chat-space-rail.js', () => ({}));

const ME = '11111111-1111-4111-8111-111111111111';
const AGENT = '22222222-2222-4222-8222-222222222222';
const GONE = '33333333-3333-4333-8333-333333333333';
const AGENT_KEY = `dm:agent:${AGENT}:user:${ME}`;
const GONE_KEY = `dm:agent:${GONE}:user:${ME}`;

const DMS = [
  {
    conversationKey: AGENT_KEY,
    peerId: AGENT,
    peerKind: 'agent',
    peerName: 'other-space-agent',
    hasUnread: true,
    lastMessageId: 'm1',
  },
  // A hard-deleted agent: the hub has no name for it.
  {
    conversationKey: GONE_KEY,
    peerId: GONE,
    peerKind: 'agent',
    hasUnread: false,
    lastMessageId: 'm2',
  },
];

let dmsBody: unknown = { dms: DMS };
let spacesBody: unknown = { spaces: [] };

beforeAll(async () => {
  await import('./chat.js');
});

beforeEach(() => {
  dmsBody = { dms: DMS };
  spacesBody = { spaces: [] };
  chatDMsLoad.invalidate();
  chatSpacesLoad.invalidate();
  vi.mocked(apiFetch).mockReset();
  vi.mocked(apiFetch).mockImplementation((url: string) => {
    const body = url.startsWith('/api/v1/chat/dms') ? dmsBody : spacesBody;
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
});

function createPage(): any {
  const page = document.createElement('scion-page-chat') as any;
  page.pageData = { path: '/chat', title: 'Chat', user: { id: ME } };
  return page;
}

function dmsCalls(): number {
  return vi
    .mocked(apiFetch)
    .mock.calls.filter(([url]) => String(url).startsWith('/api/v1/chat/dms')).length;
}

describe('chat page Unread DMs wiring', () => {
  it('feeds v2DMList from /chat/dms and keeps it when nothing changed', async () => {
    const page = createPage();
    await page.loadUnreadDMPeers();
    expect(page.v2DMList.map((d: any) => d.conversationKey)).toEqual([AGENT_KEY, GONE_KEY]);
    const first = page.v2DMList;
    await page.loadUnreadDMPeers();
    expect(page.v2DMList).toBe(first);
    dmsBody = { dms: [{ ...DMS[0], hasUnread: false }, DMS[1]] };
    await page.loadUnreadDMPeers();
    expect(page.v2DMList).not.toBe(first);
    expect(page.v2DMList[0].hasUnread).toBe(false);
  });

  it('binds the list to the rail and opens the DM the rail picks', async () => {
    const page = createPage();
    await page.loadUnreadDMPeers();
    page.v2SpaceRailLoaded = true;
    const host = document.createElement('div');
    document.body.appendChild(host);
    render(page.renderV2Rail(), host, { host: page });
    const rail = host.querySelector('scion-chat-space-rail') as any;
    expect(rail).not.toBeNull();
    expect(rail.dms).toBe(page.v2DMList);

    rail.dispatchEvent(
      new CustomEvent('dm-select', {
        detail: {
          conversationKey: GONE_KEY,
          peerId: GONE,
          peerKind: 'agent',
          displayName: 'Unknown agent',
        },
        bubbles: true,
        composed: true,
      })
    );
    expect(page.v2Conversation).toMatchObject({
      conversationKey: GONE_KEY,
      isDM: true,
      peerId: GONE,
      peerKind: 'agent',
      peerName: 'Unknown agent',
    });
  });

  it('puts a DM marked unread elsewhere back in the list at once, then reloads', async () => {
    const page = createPage();
    await page.loadUnreadDMPeers();
    const before = dmsCalls();
    dmsBody = { dms: [DMS[0], { ...DMS[1], hasUnread: true }] };

    page._handleOwnReadStateSSE(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: GONE_KEY, userId: ME, unread: true } },
      })
    );
    expect(page.v2DMList.find((d: any) => d.conversationKey === GONE_KEY).hasUnread).toBe(true);
    expect(dmsCalls()).toBe(before + 1);
    await vi.waitFor(() => expect(page.v2UnreadFromIds).toContain(GONE));
    expect(page.v2DMList.find((d: any) => d.conversationKey === GONE_KEY).hasUnread).toBe(true);
  });

  it('drops a DM read elsewhere from the list on the own read event', async () => {
    dmsBody = { dms: [DMS[0]] };
    const page = createPage();
    await page.loadUnreadDMPeers();
    expect(page.v2DMList[0].hasUnread).toBe(true);
    dmsBody = { dms: [{ ...DMS[0], hasUnread: false }] };

    page._handleOwnReadStateSSE(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: AGENT_KEY, userId: ME } },
      })
    );
    await vi.waitFor(() => expect(page.v2DMList[0].hasUnread).toBe(false));
  });

  it("ignores another user's read and thread reads", async () => {
    const page = createPage();
    await page.loadUnreadDMPeers();
    const before = dmsCalls();
    page._handleOwnReadStateSSE(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: AGENT_KEY, userId: 'someone-else' } },
      })
    );
    page._handleOwnReadStateSSE(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: 'topic-1', userId: ME } },
      })
    );
    expect(dmsCalls()).toBe(before);
  });

  it('counts the badge from the rail lists while up', async () => {
    const page = createPage();
    spacesBody = { spaces: [{ unreadCount: 2 }, { unreadCount: 0 }] };
    dmsBody = {
      dms: [
        DMS[0],
        { ...DMS[1], hasUnread: true, muted: true },
        { ...DMS[1], conversationKey: 'dm:deleted', hasUnread: true, peerDeleted: true },
      ],
    };
    expect(await page._unreadCountSource(-Infinity)).toEqual({
      count: 3,
      startedAt: expect.any(Number),
    });
  });

  it('dates the answer by the older of the requests it joined', async () => {
    const page = createPage();
    // A /chat/dms request already in flight, started before the spaces one.
    const dmsJoined = chatDMsLoad.load();
    const dmsAt = chatDMsLoad.startedAt() as number;
    await new Promise((r) => setTimeout(r, 5));
    const answer = await page._unreadCountSource(dmsAt - 1);
    await dmsJoined;
    expect(answer.startedAt).toBe(dmsAt);
    expect(chatSpacesLoad.startedAt()).toBeGreaterThan(dmsAt);
  });

  it('installs the badge source while connected and removes it on disconnect', () => {
    // connectedCallback starts the page's live updates; happy-dom has no
    // EventSource.
    vi.stubGlobal(
      'EventSource',
      class {
        addEventListener(): void {}
        removeEventListener(): void {}
        close(): void {}
      }
    );
    const page = createPage();
    const setSource = vi.spyOn(chatUnread, 'setSource');
    try {
      page.connectedCallback();
      expect(setSource).toHaveBeenLastCalledWith(page._unreadCountSource);
      page.disconnectedCallback();
      expect(setSource).toHaveBeenLastCalledWith(null);
    } finally {
      setSource.mockRestore();
      vi.unstubAllGlobals();
    }
  });
});
