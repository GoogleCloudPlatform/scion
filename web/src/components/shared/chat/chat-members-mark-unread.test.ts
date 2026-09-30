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
 * already unread.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const AGENT = { id: 'agent-1', kind: 'agent' as const, displayName: 'Coder', slug: 'coder' };
const HUMAN = { id: 'user-2', kind: 'user' as const, displayName: 'Bob' };

function createMembers(overrides: Record<string, unknown> = {}): any {
  const el = document.createElement('scion-chat-members') as any;
  el.humans = [HUMAN];
  el.agents = [AGENT];
  el.currentUserId = 'user-1';
  el.unreadFromIds = [];
  el.dmKeyByPeerId = { [AGENT.id]: 'dm:agent:agent-1:user:user-1', [HUMAN.id]: 'dm:user:user-1:user:user-2' };
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
  it('allows a member with an existing DM that is not already unread', () => {
    const el = createMembers();
    expect(el.canMarkUnread(AGENT.id)).toBe(true);
  });

  it('hides for the caller themselves', () => {
    const el = createMembers();
    expect(el.canMarkUnread('user-1')).toBe(false);
  });

  it('hides for a member with no DM', () => {
    const el = createMembers({ dmKeyByPeerId: {} });
    expect(el.canMarkUnread(AGENT.id)).toBe(false);
  });

  it('hides once the DM is already unread', () => {
    const el = createMembers({ unreadFromIds: [AGENT.id] });
    expect(el.canMarkUnread(AGENT.id)).toBe(false);
  });
});

describe('members sidebar — mark unread action', () => {
  it('POSTs the unread endpoint for the member’s DM key', async () => {
    const el = createMembers();

    await el.handleMarkUnread(AGENT.id);

    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/' + encodeURIComponent('dm:agent:agent-1:user:user-1') + '/unread',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('dispatches member-marked-unread on success', async () => {
    const el = createMembers();
    document.body.appendChild(el);
    const handler = vi.fn();
    el.addEventListener('member-marked-unread', handler);

    await el.handleMarkUnread(AGENT.id);

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0].detail).toEqual({ peerId: AGENT.id });
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
    const el = createMembers({ dmKeyByPeerId: {} });

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
    const el = createMembers({ dmKeyByPeerId: {} });
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
