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
 * Coalesced Decisions bell notification (`decisions_waiting`, ptone/scion#7997):
 * pinning at top of tray, link to `/profile/decisions?tab=pending`, live SSE
 * count updates and `acknowledged: true` clearing, browser push on new/increased
 * count only, B6 reconcile probe on login + tray open, and `projectDecisionsEnabled()`
 * feature gating.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';

import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { setPushOptIn } from '../../client/push-preference.js';
import {
  CHAT_CHOICES_FLAG,
  PROJECT_DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import type { Notification, User } from '../../shared/types.js';
import {
  DECISIONS_WAITING_KIND,
  DECISIONS_WAITING_STATUS,
  PROFILE_DECISIONS_PENDING_PATH,
  ScionNotificationTray,
  acknowledgedNotificationFromEvent,
  decisionsWaitingLink,
} from './notification-tray.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(),
}));

const mockedFetch = vi.mocked(apiFetch);

const NIL_UUID = '00000000-0000-0000-0000-000000000000';
const AGENT_ID = '6a2b3c4d-0000-4000-8000-000000000001';

const alice: User = {
  id: 'user-alice',
  email: 'alice@example.test',
  name: 'Alice',
  role: 'member',
};

function decisionsRow(
  count: number,
  projects = 1,
  overrides: Partial<Notification> = {}
): Notification {
  const dWord = count === 1 ? '1 decision is' : `${count} decisions are`;
  const pWord = projects === 1 ? '1 project' : `${projects} projects`;
  return {
    id: 'notif-decisions-alice',
    subscriptionId: NIL_UUID,
    agentId: NIL_UUID,
    projectId: NIL_UUID,
    subscriberType: 'user',
    subscriberId: alice.id,
    status: DECISIONS_WAITING_STATUS,
    message: `${dWord} waiting on you in ${pWord}`,
    dispatched: false,
    acknowledged: false,
    createdAt: '2026-11-15T12:00:00Z',
    kind: DECISIONS_WAITING_KIND,
    resourceType: 'decisions',
    resourceId: String(count),
    ...overrides,
  };
}

function agentRow(id: string, createdAt = '2026-11-15T12:05:00Z'): Notification {
  return {
    id,
    subscriptionId: 'sub-1',
    agentId: AGENT_ID,
    projectId: 'proj-1',
    subscriberType: 'user',
    subscriberId: alice.id,
    status: 'COMPLETED',
    message: `Agent finished ${id}`,
    dispatched: true,
    acknowledged: false,
    createdAt,
  };
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function emitUserNotification(data: Record<string, unknown>): void {
  stateManager.dispatchEvent(
    new CustomEvent('notification-created', { detail: { state: {}, data } })
  );
}

const flush = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

function enableDecisionsFlags(choices = true, projectDecisions = true): void {
  window.__SCION_FEATURES__ = {
    [CHAT_CHOICES_FLAG]: choices,
    [PROJECT_DECISIONS_FLAG]: projectDecisions,
  };
}

describe('notification tray: decisions_waiting', () => {
  const createdPushCalls: Array<{ title: string; options: NotificationOptions | undefined }> = [];
  const origNotification = window.Notification;

  beforeEach(() => {
    mockedFetch.mockReset();
    createdPushCalls.length = 0;
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();

    class StubNotification {
      static permission: NotificationPermission = 'granted';
      static requestPermission = vi.fn(
        (): Promise<NotificationPermission> => Promise.resolve('granted')
      );
      constructor(title: string, options?: NotificationOptions) {
        createdPushCalls.push({ title, options });
      }
    }
    Object.defineProperty(window, 'Notification', {
      configurable: true,
      writable: true,
      value: StubNotification,
    });
    setPushOptIn('agent', true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    Object.defineProperty(window, 'Notification', {
      configurable: true,
      writable: true,
      value: origNotification,
    });
  });

  it('exports the hub kind, status, pending decisions link, and acknowledged SSE parser', () => {
    expect(DECISIONS_WAITING_KIND).toBe('decisions_waiting');
    expect(DECISIONS_WAITING_STATUS).toBe('DECISIONS_WAITING');
    expect(PROFILE_DECISIONS_PENDING_PATH).toBe('/profile/decisions?tab=pending');

    expect(decisionsWaitingLink(decisionsRow(3))).toBe('/profile/decisions?tab=pending');
    const { kind: _omitKind, ...withoutKind } = decisionsRow(1, 1);
    void _omitKind;
    expect(decisionsWaitingLink(withoutKind)).toBe('/profile/decisions?tab=pending');
    expect(decisionsWaitingLink(agentRow('n-1'))).toBeNull();

    expect(
      acknowledgedNotificationFromEvent(
        {
          id: 'notif-decisions-alice',
          subscriberId: alice.id,
          kind: DECISIONS_WAITING_KIND,
          status: DECISIONS_WAITING_STATUS,
          acknowledged: true,
        },
        alice.id
      )
    ).toEqual({ id: 'notif-decisions-alice', isDecisions: true });
    expect(
      acknowledgedNotificationFromEvent(
        { id: 'notif-decisions-alice', subscriberId: 'other-user', acknowledged: true },
        alice.id
      )
    ).toBeNull();
    expect(
      acknowledgedNotificationFromEvent(
        { id: 'notif-decisions-alice', subscriberId: alice.id, acknowledged: false },
        alice.id
      )
    ).toBeNull();
  });

  it('renders a pinned row with signpost-split icon, Decisions waiting title, Open Decisions link, and no View agent link', () => {
    const host = document.createElement('div');
    const tray: any = document.createElement('scion-notification-tray');
    tray.open = true;
    render(tray.renderItem(decisionsRow(3, 2)), host);

    const item = host.querySelector('.notif-item');
    expect(item?.classList.contains('pinned')).toBe(true);
    expect(host.querySelector('.notif-icon')?.classList.contains('status-primary')).toBe(true);
    expect(host.querySelector('.notif-icon sl-icon')?.getAttribute('name')).toBe('signpost-split');
    expect(host.querySelector('.notif-title')?.textContent?.trim()).toBe('Decisions waiting');
    expect(host.querySelector('.notif-message')?.textContent?.trim()).toBe(
      '3 decisions are waiting on you in 2 projects'
    );

    const links = [...host.querySelectorAll('a')].map((a) => [
      a.getAttribute('href'),
      a.textContent?.trim(),
    ]);
    expect(links).toEqual([['/profile/decisions?tab=pending', 'Open Decisions']]);

    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      const openLink = host.querySelector<HTMLAnchorElement>('a.decisions-open-link');
      expect(openLink).not.toBeNull();
      openLink?.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
    expect(paths).toEqual(['/profile/decisions?tab=pending']);
    expect(tray.open).toBe(false);
  });

  it('pins decisions_waiting at index 0 above newer agent notifications on load and SSE arrivals', async () => {
    enableDecisionsFlags(true, true);
    mockedFetch.mockImplementation((input) => {
      if (String(input).startsWith('/api/v1/notifications')) {
        return Promise.resolve(jsonResponse([agentRow('newer-agent'), decisionsRow(2, 1)]));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const tray = new ScionNotificationTray();
    tray.user = alice;
    document.body.appendChild(tray);
    await flush();
    await tray.updateComplete;

    expect((tray as any).notifications.map((n: Notification) => n.id)).toEqual([
      'notif-decisions-alice',
      'newer-agent',
    ]);

    // A brand-new agent notification arrives via SSE -> decisions_waiting remains at index 0.
    emitUserNotification({
      ...agentRow('newest-agent', '2026-11-15T12:10:00Z'),
      acknowledged: false,
    });
    await tray.updateComplete;

    expect((tray as any).notifications.map((n: Notification) => n.id)).toEqual([
      'notif-decisions-alice',
      'newest-agent',
      'newer-agent',
    ]);
  });

  it('updates live on SSE count changes, clears immediately on acknowledged: true without refetching, and follows B7 push rules', async () => {
    enableDecisionsFlags(true, true);
    mockedFetch.mockImplementation(() => Promise.resolve(jsonResponse([])));

    const tray = new ScionNotificationTray();
    tray.user = alice;
    document.body.appendChild(tray);
    await flush();
    await tray.updateComplete;

    expect(createdPushCalls).toHaveLength(0);
    mockedFetch.mockClear();

    // 1. First prompt arrives (0 -> 2): pushes once with tag decisions_waiting.
    emitUserNotification({ ...decisionsRow(2, 1), acknowledged: false });
    await tray.updateComplete;
    expect((tray as any).notifications).toHaveLength(1);
    expect(createdPushCalls).toHaveLength(1);
    expect(createdPushCalls[0]).toEqual({
      title: 'Decisions waiting',
      options: {
        body: '2 decisions are waiting on you in 1 project',
        tag: 'decisions_waiting',
        icon: '/scion-notification-icon.png',
      },
    });

    // 2. Count increases (2 -> 3): pushes again with same tag so OS replaces in place.
    emitUserNotification({ ...decisionsRow(3, 1), acknowledged: false });
    await tray.updateComplete;
    expect((tray as any).notifications[0].message).toBe(
      '3 decisions are waiting on you in 1 project'
    );
    expect(createdPushCalls).toHaveLength(2);
    expect(createdPushCalls[1]?.options?.body).toBe('3 decisions are waiting on you in 1 project');

    // 3. Count decreases (3 -> 2): updates message silently WITHOUT browser push.
    emitUserNotification({ ...decisionsRow(2, 1), acknowledged: false });
    await tray.updateComplete;
    expect((tray as any).notifications[0].message).toBe(
      '2 decisions are waiting on you in 1 project'
    );
    expect(createdPushCalls).toHaveLength(2);

    // 4. All answered -> SSE acknowledged: true removes row immediately with zero HTTP fetches and no push.
    emitUserNotification({
      ...decisionsRow(0, 0, {
        message: 'No decisions are waiting on you',
        resourceId: '0',
      }),
      acknowledged: true,
    });
    await tray.updateComplete;
    expect((tray as any).notifications).toEqual([]);
    expect(mockedFetch).not.toHaveBeenCalled();
    expect(createdPushCalls).toHaveLength(2);
  });

  it('suppresses push on initial load and fires push again when resurfacing after Mark read', async () => {
    enableDecisionsFlags(true, true);
    mockedFetch.mockImplementation((input) => {
      const url = String(input);
      if (url.startsWith('/api/v1/notifications?acknowledged=false')) {
        return Promise.resolve(jsonResponse([decisionsRow(3, 1)]));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const tray = new ScionNotificationTray();
    tray.user = alice;
    document.body.appendChild(tray);
    await flush();
    await tray.updateComplete;

    // Initial load never fires browser push.
    expect(createdPushCalls).toHaveLength(0);

    // User clicks Mark read -> hides row.
    await (tray as any).ackOne('notif-decisions-alice');
    await tray.updateComplete;
    expect((tray as any).notifications).toEqual([]);

    // Resurfaces with 3 decisions -> pushes because row was hidden.
    emitUserNotification({ ...decisionsRow(3, 1), acknowledged: false });
    await tray.updateComplete;
    expect((tray as any).notifications).toHaveLength(1);
    expect(createdPushCalls).toHaveLength(1);
  });

  it('probes /api/v1/users/me/decisions?tab=pending&limit=0 on login and tray open only when projectDecisionsEnabled() is true', async () => {
    // Flags off initially -> no reconcile probe, and any stale decisions_waiting row is hidden.
    enableDecisionsFlags(true, false);
    mockedFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse([decisionsRow(1, 1), agentRow('a-1')]))
    );

    const tray = new ScionNotificationTray();
    tray.user = alice;
    document.body.appendChild(tray);
    await flush();
    await tray.updateComplete;

    expect(mockedFetch.mock.calls.map((c) => c[0])).toEqual([
      '/api/v1/notifications?acknowledged=false',
    ]);
    expect((tray as any).notifications.map((n: Notification) => n.id)).toEqual(['a-1']);

    // Server flags arrive enabling web.project_decisions -> triggers reconcile probe + refresh!
    mockedFetch.mockClear();
    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    await flush();
    await tray.updateComplete;

    expect(mockedFetch.mock.calls.map((c) => c[0])).toContain(
      '/api/v1/users/me/decisions?tab=pending&limit=0'
    );
    expect((tray as any).notifications.map((n: Notification) => n.id)).toEqual([
      'notif-decisions-alice',
      'a-1',
    ]);

    // Opening the tray fires the reconcile probe again alongside the list refresh.
    mockedFetch.mockClear();
    tray.shadowRoot?.querySelector<HTMLButtonElement>('.bell-btn')?.click();
    await flush();
    expect(mockedFetch.mock.calls.map((c) => c[0])).toEqual([
      '/api/v1/notifications?acknowledged=false',
      '/api/v1/users/me/decisions?tab=pending&limit=0',
    ]);
  });

  it('preserves newer SSE in-place updates and acknowledgements when an older fetchNotifications resolves late', async () => {
    enableDecisionsFlags(true, true);
    let resolveInFlight!: (res: Response) => void;
    let callCount = 0;
    mockedFetch.mockImplementation((input) => {
      const url = String(input);
      if (url.startsWith('/api/v1/notifications?acknowledged=false')) {
        callCount += 1;
        if (callCount === 1) {
          return Promise.resolve(jsonResponse([decisionsRow(2, 1)]));
        }
        return new Promise<Response>((resolve) => {
          resolveInFlight = resolve;
        });
      }
      return Promise.resolve(jsonResponse({}));
    });

    const tray = new ScionNotificationTray();
    tray.user = alice;
    document.body.appendChild(tray);
    await flush();
    await tray.updateComplete;
    expect((tray as any).notifications[0].message).toBe(
      '2 decisions are waiting on you in 1 project'
    );

    // 1. Opening the tray starts fetch #2 (which reads stale count=2 on the server)
    //    while reconcile pushes count=3 over SSE before fetch #2 resolves.
    tray.shadowRoot?.querySelector<HTMLButtonElement>('.bell-btn')?.click();
    emitUserNotification({ ...decisionsRow(3, 1), acknowledged: false });
    await tray.updateComplete;
    expect((tray as any).notifications[0].message).toBe(
      '3 decisions are waiting on you in 1 project'
    );

    resolveInFlight(jsonResponse([decisionsRow(2, 1)]));
    await flush();
    await tray.updateComplete;
    expect((tray as any).notifications[0].message).toBe(
      '3 decisions are waiting on you in 1 project'
    );

    // 2. Start fetch #3 while unacknowledged, then SSE acknowledges the row
    //    before fetch #3 resolves -> stale fetch #3 must not resurrect it.
    void (tray as any).fetchNotifications();
    emitUserNotification({
      ...decisionsRow(0, 0, {
        message: 'No decisions are waiting on you',
        resourceId: '0',
      }),
      acknowledged: true,
    });
    await tray.updateComplete;
    expect((tray as any).notifications).toEqual([]);

    resolveInFlight(jsonResponse([decisionsRow(3, 1)]));
    await flush();
    await tray.updateComplete;
    expect((tray as any).notifications).toEqual([]);
  });
});
