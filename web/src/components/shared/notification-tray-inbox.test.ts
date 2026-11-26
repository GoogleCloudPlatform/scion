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
 * Bell notification tray Inbox discovery link and empty-state link
 * (design §6.1 item 2, §9 AC9 & AC13).
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../client/inbox-summary-loader.js', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../client/inbox-summary-loader.js')>();
  return {
    ...mod,
    ensureInboxSummaryStarted: vi.fn(mod.ensureInboxSummaryStarted),
  };
});

import { NOTIFICATION_ACKED_EVENT, type ScionNotificationTray } from './notification-tray.js';
import { ensureInboxSummaryStarted } from '../../client/inbox-summary-loader.js';
import { inboxSummaryStore, type InboxSummaryResponse } from '../../client/inbox-summary.js';
import { stateManager } from '../../client/state.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import type { Notification } from '../../shared/types.js';

function makeSummary(total: number, totalCapped = false): InboxSummaryResponse {
  const disabled = {
    state: 'disabled' as const,
    count: 0,
    capped: false,
    countsTowardTotal: true,
    href: '/',
    previews: [],
  };
  return {
    generatedAt: '2026-11-24T12:00:00Z',
    total,
    totalCapped,
    categories: {
      decisions: disabled,
      waiting: disabled,
      reviews: disabled,
      pending_secrets: {
        state: 'ok',
        count: total,
        capped: totalCapped,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      chat: disabled,
    },
  };
}

function makeNotification(id: string): Notification {
  return {
    id,
    subscriptionId: 'sub-1',
    agentId: 'agent-1',
    projectId: 'proj-1',
    subscriberType: 'user',
    subscriberId: 'u-1',
    status: 'COMPLETED',
    message: `Agent finished ${id}`,
    dispatched: true,
    acknowledged: false,
    createdAt: '2026-11-24T12:00:00Z',
  };
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

describe('notification tray: Inbox link and empty state', () => {
  let summaryResponse: InboxSummaryResponse = makeSummary(0);
  let notificationsResponse: Notification[] = [];
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeAll(async () => {
    await import('./notification-tray.js');
  }, 30_000);

  beforeEach(() => {
    summaryResponse = makeSummary(0);
    notificationsResponse = [];
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    inboxSummaryStore.resetForTests();
    vi.mocked(ensureInboxSummaryStarted).mockClear();
    fetchMock = vi.fn<typeof fetch>((input: RequestInfo | URL) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(JSON.stringify(summaryResponse), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.includes('/api/v1/notifications')) {
        return Promise.resolve(
          new Response(JSON.stringify(notificationsResponse), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(new Response('{}', { status: 404 }));
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    inboxSummaryStore.resetForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  async function mountTray(openPanel = true): Promise<ScionNotificationTray> {
    const tray = document.createElement('scion-notification-tray') as ScionNotificationTray;
    tray.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(tray);
    await flush();
    if (openPanel) {
      const bell = tray.shadowRoot!.querySelector<HTMLButtonElement>('.bell-btn')!;
      bell.click();
      await flush();
    }
    await tray.updateComplete;
    return tray;
  }

  it('renders no Inbox header link, standard empty state, and never loads or fetches inbox summary when flag is off', async () => {
    summaryResponse = makeSummary(5);
    const tray = await mountTray();

    expect(tray.shadowRoot!.querySelector('[data-tray-inbox-link]')).toBeNull();
    expect(tray.shadowRoot!.querySelector('[data-tray-empty-inbox-link]')).toBeNull();
    expect(tray.shadowRoot!.querySelector('.empty-state')?.textContent?.trim()).toBe(
      'No notifications'
    );
    expect(ensureInboxSummaryStarted).not.toHaveBeenCalled();
    const inboxFetches = fetchMock.mock.calls.filter(([input]) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      return url.includes('/inbox/summary');
    });
    expect(inboxFetches).toHaveLength(0);
  });

  it('renders "Inbox · N →" header link and "No new notifications. Open Inbox" when flag is on and total > 0', async () => {
    summaryResponse = makeSummary(9);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const tray = await mountTray();

    const headerLink = tray.shadowRoot!.querySelector<HTMLAnchorElement>('[data-tray-inbox-link]')!;
    expect(headerLink).not.toBeNull();
    expect(headerLink.getAttribute('href')).toBe('/profile/inbox');
    expect(headerLink.textContent?.trim()).toBe('Inbox \u00b7 9 \u2192');

    // Empty tray with total > 0 shows "No new notifications. Open Inbox"
    const emptyText = tray
      .shadowRoot!.querySelector('.empty-state span')
      ?.textContent?.replace(/\s+/g, ' ')
      .trim();
    expect(emptyText).toBe('No new notifications. Open Inbox');
    const emptyLink = tray.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-tray-empty-inbox-link]'
    )!;
    expect(emptyLink.getAttribute('href')).toBe('/profile/inbox');

    // Bell badge stays unchanged (no notifications -> no bell badge).
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')).toBeNull();
  });

  it('renders capped "Inbox · N+ →" and hides empty-state Inbox prompt when total is 0', async () => {
    summaryResponse = makeSummary(0);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const tray = await mountTray();

    const headerLink = tray.shadowRoot!.querySelector<HTMLAnchorElement>('[data-tray-inbox-link]')!;
    expect(headerLink.textContent?.trim()).toBe('Inbox \u2192');
    expect(tray.shadowRoot!.querySelector('[data-tray-empty-inbox-link]')).toBeNull();
    expect(tray.shadowRoot!.querySelector('.empty-state')?.textContent?.trim()).toBe(
      'No notifications'
    );

    // Update to capped total -> header link updates live.
    summaryResponse = makeSummary(12, true);
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await tray.updateComplete;

    expect(tray.shadowRoot!.querySelector('[data-tray-inbox-link]')?.textContent?.trim()).toBe(
      'Inbox \u00b7 12+ \u2192'
    );
    expect(tray.shadowRoot!.querySelector('[data-tray-empty-inbox-link]')).not.toBeNull();
  });

  it('navigates to /profile/inbox and closes the tray when clicking the header link, keeping bell badge and rows unchanged', async () => {
    summaryResponse = makeSummary(4);
    notificationsResponse = [makeNotification('n-1'), makeNotification('n-2')];
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const tray = await mountTray();

    // Bell badge still reflects notification count (2), not inbox total (4).
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('2');
    expect(tray.shadowRoot!.querySelectorAll('.notif-item')).toHaveLength(2);
    expect(tray.shadowRoot!.querySelector('.mark-all-btn')).not.toBeNull();

    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    const removeListener = vi.spyOn(document, 'removeEventListener');
    try {
      const link = tray.shadowRoot!.querySelector<HTMLAnchorElement>('[data-tray-inbox-link]')!;
      const click = new MouseEvent('click', { bubbles: true, cancelable: true });
      link.dispatchEvent(click);
      expect(click.defaultPrevented).toBe(true);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
    await tray.updateComplete;

    expect(paths).toEqual(['/profile/inbox']);
    expect(tray.shadowRoot!.querySelector('.panel')).toBeNull();
    expect(removeListener).toHaveBeenCalledWith('click', expect.any(Function), true);
  });

  it('navigates to /profile/inbox and closes the tray when clicking the empty-state Open Inbox link', async () => {
    summaryResponse = makeSummary(3);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const tray = await mountTray();

    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      const link = tray.shadowRoot!.querySelector<HTMLAnchorElement>(
        '[data-tray-empty-inbox-link]'
      )!;
      const click = new MouseEvent('click', { bubbles: true, cancelable: true });
      link.dispatchEvent(click);
      expect(click.defaultPrevented).toBe(true);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
    await tray.updateComplete;

    expect(paths).toEqual(['/profile/inbox']);
    expect(tray.shadowRoot!.querySelector('.panel')).toBeNull();
  });

  it('syncs bell tray on NOTIFICATION_ACKED_EVENT and inbox-changed notifications hint', async () => {
    notificationsResponse = [makeNotification('n-1'), makeNotification('n-2')];
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const tray = await mountTray(false);

    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('2');

    // Same-tab ack event drops n-1 immediately.
    window.dispatchEvent(new CustomEvent(NOTIFICATION_ACKED_EVENT, { detail: { id: 'n-1' } }));
    await tray.updateComplete;
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('1');

    // Cross-tab SSE inbox-changed { category: 'notifications' } refetches and drops n-2.
    notificationsResponse = [];
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'notifications' } })
    );
    await flush();
    await tray.updateComplete;
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')).toBeNull();
  });

  it('keeps an acked notification absent when an in-flight tray fetch resolves with the stale row', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    notificationsResponse = [makeNotification('n-1'), makeNotification('n-2')];
    const tray = await mountTray(false);
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('2');

    // Start an in-flight tray fetch whose response still includes n-1.
    let resolveStaleFetch: ((res: Response) => void) | null = null;
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes('/api/v1/notifications')) {
        return new Promise<Response>((resolve) => {
          resolveStaleFetch = resolve;
        });
      }
      return Promise.resolve(
        new Response(JSON.stringify(summaryResponse), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });

    // Opening the tray starts a fresh notifications fetch.
    tray.shadowRoot!.querySelector<HTMLButtonElement>('.bell-btn')!.click();
    await tray.updateComplete;
    expect(resolveStaleFetch).not.toBeNull();

    // While the fetch is in flight, Inbox acks n-1.
    window.dispatchEvent(new CustomEvent(NOTIFICATION_ACKED_EVENT, { detail: { id: 'n-1' } }));
    await tray.updateComplete;
    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('1');

    // Stale fetch resolves still containing n-1 -> n-1 must stay absent.
    resolveStaleFetch!(
      new Response(JSON.stringify([makeNotification('n-1'), makeNotification('n-2')]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await flush();
    await tray.updateComplete;

    expect(tray.shadowRoot!.querySelector('.bell-btn .badge')?.textContent?.trim()).toBe('1');
    const items = Array.from(tray.shadowRoot!.querySelectorAll('.notif-item'));
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toContain('Agent finished n-2');
    expect(items[0]?.textContent).not.toContain('Agent finished n-1');
  });
});
