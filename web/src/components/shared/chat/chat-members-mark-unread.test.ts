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
 * Tests for the members sidebar's "Mark unread" context-menu action.
 *
 * The item only applies to a member with an existing, non-empty DM — hidden
 * for the caller themselves, for a member with no DM, and once the DM is
 * already unread (checked via the DM's own hasUnread, not the mute-filtered
 * unreadFromIds dot list: a muted-but-unread DM must stay hidden too, not
 * look eligible again because its dot is suppressed).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

// The module under test imports from the app entry. Loading the real
// main.ts registers every Shoelace component and runs the app bootstrap,
// so happy-dom tries to fetch from 127.0.0.1:3000 and logs ECONNREFUSED on
// stderr. These tests only need a stub; it matches the one in
// chat-members.test.ts (ptone/scion#3392).
vi.mock('../../../client/main.js', () => ({
  navigateTo: vi.fn(),
  pushRoute: vi.fn((path: string) => {
    window.history.pushState({}, '', path);
    return Promise.resolve();
  }),
  replaceRoute: vi.fn((path: string) => {
    window.history.replaceState(
      window.history.state,
      '',
      path + window.location.search + window.location.hash
    );
    return Promise.resolve();
  }),
  stateManager: new EventTarget(),
}));

import { apiFetch } from '../../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const AGENT = { id: 'agent-1', kind: 'agent' as const, displayName: 'Coder', slug: 'coder' };
const HUMAN = { id: 'user-2', kind: 'user' as const, displayName: 'Bob' };
const AGENT_DM_KEY = 'dm:agent:agent-1:user:user-1';
const HUMAN_DM_KEY = 'dm:user:user-1:user:user-2';

function createMembers(overrides: Record<string, unknown> = {}): any {
  const el = document.createElement('scion-chat-members') as any;
  el.humans = [HUMAN];
  el.agents = [AGENT];
  el.currentUserId = 'user-1';
  el.unreadFromIds = [];
  el.dmInfoByPeerId = {
    [AGENT.id]: { key: AGENT_DM_KEY, muted: false, hasUnread: false },
    [HUMAN.id]: { key: HUMAN_DM_KEY, muted: false, hasUnread: false },
  };
  Object.assign(el, overrides);
  return el;
}

beforeAll(async () => {
  await import('./chat-members.js');
});

beforeEach(() => {
  apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('members sidebar — mark unread eligibility', () => {
  it('allows a member with an existing, read DM', () => {
    const el = createMembers();
    expect(el.canMarkUnread(AGENT.id)).toBe(true);
  });

  it('hides for the caller themselves', () => {
    const el = createMembers();
    expect(el.canMarkUnread('user-1')).toBe(false);
  });

  it('hides for a member with no DM', () => {
    const el = createMembers({ dmInfoByPeerId: {} });
    expect(el.canMarkUnread(AGENT.id)).toBe(false);
  });

  it('hides once the DM is already unread', () => {
    const el = createMembers({
      dmInfoByPeerId: { [AGENT.id]: { key: AGENT_DM_KEY, muted: false, hasUnread: true } },
    });
    expect(el.canMarkUnread(AGENT.id)).toBe(false);
  });

  it('hides a muted DM that is already unread — the dot being suppressed must not make it look eligible', () => {
    const el = createMembers({
      dmInfoByPeerId: { [AGENT.id]: { key: AGENT_DM_KEY, muted: true, hasUnread: true } },
    });
    // Sanity: unreadFromIds (the dot list) has nothing for this peer, the
    // way loadUnreadDMPeers would leave it for a muted-but-unread DM.
    expect(el.unreadFromIds.includes(AGENT.id)).toBe(false);
    expect(el.canMarkUnread(AGENT.id)).toBe(false);
  });

  it('allows a muted DM that is currently read — muting does not block the action, only the dot', () => {
    const el = createMembers({
      dmInfoByPeerId: { [AGENT.id]: { key: AGENT_DM_KEY, muted: true, hasUnread: false } },
    });
    expect(el.canMarkUnread(AGENT.id)).toBe(true);
  });
});

describe('members sidebar — mark unread action', () => {
  it('POSTs the unread endpoint for the member’s DM key', async () => {
    const el = createMembers();

    await el.handleMarkUnread(AGENT.id);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/' + encodeURIComponent(AGENT_DM_KEY) + '/unread',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('dispatches member-marked-unread with the peerId and conversationKey on success', async () => {
    const el = createMembers();
    document.body.appendChild(el);
    const handler = vi.fn();
    el.addEventListener('member-marked-unread', handler);

    await el.handleMarkUnread(AGENT.id);

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0].detail).toEqual({
      peerId: AGENT.id,
      conversationKey: AGENT_DM_KEY,
    });
  });

  it('does not dispatch when the server refuses', async () => {
    const el = createMembers();
    document.body.appendChild(el);
    apiFetchMock.mockResolvedValue(new Response('{}', { status: 500 }));
    const handler = vi.fn();
    el.addEventListener('member-marked-unread', handler);

    await el.handleMarkUnread(AGENT.id);

    expect(handler).not.toHaveBeenCalled();
  });

  it('is a no-op for a member with no DM key resolvable', async () => {
    const el = createMembers({ dmInfoByPeerId: {} });

    await el.handleMarkUnread(AGENT.id);

    expect(apiFetchMock).not.toHaveBeenCalled();
  });
});

describe('members sidebar — context menu gating', () => {
  function fakeEvent(): any {
    return { preventDefault: vi.fn(), stopPropagation: vi.fn(), clientX: 10, clientY: 20 };
  }

  it('opens the menu for an eligible member', () => {
    const el = createMembers();
    const e = fakeEvent();

    el.handleContextMenu(e, AGENT.id);

    expect(e.preventDefault).toHaveBeenCalled();
    expect(el.contextMenuTarget).toEqual({ peerId: AGENT.id });
  });

  it('does not open the menu for an ineligible member (no DM)', () => {
    const el = createMembers({ dmInfoByPeerId: {} });
    const e = fakeEvent();

    el.handleContextMenu(e, AGENT.id);

    expect(e.preventDefault).not.toHaveBeenCalled();
    expect(el.contextMenuTarget).toBeNull();
  });

  it('does not open the menu for a muted, already-unread member', () => {
    const el = createMembers({
      dmInfoByPeerId: { [AGENT.id]: { key: AGENT_DM_KEY, muted: true, hasUnread: true } },
    });
    const e = fakeEvent();

    el.handleContextMenu(e, AGENT.id);

    expect(e.preventDefault).not.toHaveBeenCalled();
    expect(el.contextMenuTarget).toBeNull();
  });

  it('renders the Mark unread item once the menu is open', async () => {
    const el = createMembers();
    document.body.appendChild(el);
    await el.updateComplete;
    el.contextMenuTarget = { peerId: AGENT.id };
    await el.updateComplete;

    const item = el.shadowRoot.querySelector('.context-menu-item');
    expect(item?.textContent?.trim()).toBe('Mark unread');
  });
});
