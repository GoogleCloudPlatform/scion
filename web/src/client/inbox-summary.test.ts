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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  INBOX_DEBOUNCE_MS,
  INBOX_MAX_WAIT_MS,
  INBOX_POLL_HEALTHY_MS,
  INBOX_POLL_UNHEALTHY_MS,
  INBOX_SHARE_KEY,
  INBOX_SUMMARY_CHANGED_EVENT,
  InboxSummaryStore,
  type InboxSummaryResponse,
} from './inbox-summary.js';
import { PENDING_SECRETS_CHANGED_EVENT } from './pending-secrets.js';
import { stateManager } from './state.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../utils/feature-flags.js';

function makeSummary(
  count: number,
  opts?: {
    nextExpiresAt?: string;
    agentSecretsNextExpiresAt?: string;
    agentExecutionsNextExpiresAt?: string;
  }
): InboxSummaryResponse {
  return {
    generatedAt: '2026-11-23T18:00:00Z',
    total: count,
    totalCapped: false,
    categories: {
      decisions: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/decisions?tab=pending',
        previews: [],
      },
      waiting: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/agents?q=activity%3Awaiting_for_input',
        previews: [],
      },
      pending_secrets: {
        state: 'ok',
        count,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
        breakdown: { agents: count > 0 ? 1 : 0, projects: count > 0 ? 1 : 0 },
        ...(opts?.nextExpiresAt ? { nextExpiresAt: opts.nextExpiresAt } : {}),
      },
      reviews: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/artifacts?review_on_latest=1&owner=me',
        previews: [],
      },
      chat: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/chat',
        previews: [],
      },
      ...(opts?.agentSecretsNextExpiresAt
        ? {
            agent_secrets: {
              state: 'ok' as const,
              count: 1,
              capped: false,
              countsTowardTotal: true,
              href: '/profile/agent-secrets',
              previews: [],
              nextExpiresAt: opts.agentSecretsNextExpiresAt,
            },
          }
        : {}),
      ...(opts?.agentExecutionsNextExpiresAt
        ? {
            agent_executions: {
              state: 'ok' as const,
              count: 1,
              capped: false,
              countsTowardTotal: true,
              href: '/profile/agent-executions',
              previews: [],
              nextExpiresAt: opts.agentExecutionsNextExpiresAt,
            },
          }
        : {}),
    },
  };
}

describe('InboxSummaryStore', () => {
  let store: InboxSummaryStore;
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    store = new InboxSummaryStore();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    store.resetForTests();
    vi.unstubAllGlobals();
    vi.useRealTimers();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('does not fetch when web.inbox_dashboard flag is off', async () => {
    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(100);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('always fetches previews=0 for both badge mode and mounted page', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify(makeSummary(2)), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/users/me/inbox/summary?previews=0');
    expect(store.total).toBe(2);
    expect(store.formatBadge()).toBe('2');

    // Mounting the inbox page reuses the already-loaded summary without upgrading previews.
    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    unmount();
  });

  it('debounces rapid SSE hints (750ms trailing / 4s max-wait)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(makeSummary(1)), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'pending_secrets' } })
    );
    await vi.advanceTimersByTimeAsync(300);
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'reviews' } })
    );
    await vi.advanceTimersByTimeAsync(300);
    window.dispatchEvent(new CustomEvent(PENDING_SECRETS_CHANGED_EVENT, { detail: {} }));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Continuous burst exceeding max-wait (4s) still flushes at max-wait.
    for (let elapsed = 0; elapsed < INBOX_MAX_WAIT_MS + 100; elapsed += 400) {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', { detail: { category: 'pending_secrets' } })
      );
      await vi.advanceTimersByTimeAsync(400);
    }
    expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(3);
  });

  it('broadcasts fetched summary over localStorage and adopts updates from another tab', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify(makeSummary(1)), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const changed = vi.fn();
    window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, changed);
    try {
      store.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);

      const stored = localStorage.getItem(INBOX_SHARE_KEY);
      expect(stored).toBeTruthy();

      const updated = makeSummary(5);
      localStorage.setItem(
        INBOX_SHARE_KEY,
        JSON.stringify({
          userId: 'user-1',
          fetchedAt: Date.now() + 1000,
          summary: updated,
        })
      );
      window.dispatchEvent(new StorageEvent('storage', { key: INBOX_SHARE_KEY }));

      expect(store.total).toBe(5);
      expect(changed).toHaveBeenCalled();
    } finally {
      window.removeEventListener(INBOX_SUMMARY_CHANGED_EVENT, changed);
    }
  });

  it('schedules expiry refresh from earliest nextExpiresAt across pending_secrets and agent_secrets', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const now = Date.now();
    const laterIso = new Date(now + 12_000).toISOString();
    const soonerIso = new Date(now + 4_000).toISOString();

    fetchMock
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify(
            makeSummary(1, { nextExpiresAt: laterIso, agentSecretsNextExpiresAt: soonerIso })
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(makeSummary(0)), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Before the 4s expiry + 500ms buffer: no refresh yet.
    await vi.advanceTimersByTimeAsync(3_500);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Past the sooner expiry + buffer: refreshes immediately.
    await vi.advanceTimersByTimeAsync(1_500);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('schedules expiry refresh from agent_executions.nextExpiresAt via generalized scheduleExpiryTimer (AC-13)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const now = Date.now();
    // Use 10s (strictly below INBOX_POLL_UNHEALTHY_MS = 30s) so the safety poll cannot mask expiry.
    const execExpiresIso = new Date(now + 10_000).toISOString();

    fetchMock
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify(makeSummary(1, { agentExecutionsNextExpiresAt: execExpiresIso })),
          {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }
        )
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(makeSummary(0)), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(store.total).toBe(1);

    // Before 10s expiry (+9s): no refresh yet.
    await vi.advanceTimersByTimeAsync(9_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Past 10s expiry + 500ms buffer (+11s total, well before 30s safety poll): refreshes and drops total to 0.
    await vi.advanceTimersByTimeAsync(2_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(store.total).toBe(0);
  });

  it('marks unavailable404 when the hub returns 404', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(new Response('', { status: 404 }));

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);

    expect(store.unavailable404).toBe(true);
    expect(store.summary).toBeNull();
  });

  describe('waiting-hint filter', () => {
    function makeWaitingSummary(agentIds: string[]): InboxSummaryResponse {
      const s = makeSummary(0);
      s.total = agentIds.length;
      s.categories.waiting = {
        state: 'ok',
        count: agentIds.length,
        capped: false,
        countsTowardTotal: true,
        href: '/agents?q=activity%3Awaiting_for_input',
        previews: [],
        agentIds,
      };
      return s;
    }

    beforeEach(async () => {
      setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeWaitingSummary(['agent-known'])), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      );
      store.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('refetches when an unknown agent enters waiting_for_input', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-new', activity: 'waiting_for_input' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    it('ignores waiting_for_input hint for an already-known agent', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-known', activity: 'waiting_for_input' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_MAX_WAIT_MS + 100);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('refetches when a known agent leaves waiting_for_input', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-known', activity: 'thinking' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    it('ignores non-waiting activity hints for an unknown agent', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-other', activity: 'thinking' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_MAX_WAIT_MS + 100);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('refetches when a waiting hint omits agentId', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', { detail: { category: 'waiting' } })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });
  });

  it('refetches on decision, chat, and notification trigger events', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(makeSummary(1)), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    for (const eventName of [
      'decision-updated',
      'chat-message-received',
      'chat-read-state-updated',
      'notification-created',
    ]) {
      const before = fetchMock.mock.calls.length;
      stateManager.dispatchEvent(new CustomEvent(eventName, { detail: {} }));
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock.mock.calls.length).toBe(before + 1);
    }
  });

  describe('safety poll leader deduplication', () => {
    let secondStore: InboxSummaryStore;

    beforeEach(() => {
      secondStore = new InboxSummaryStore();
    });

    afterEach(() => {
      secondStore.resetForTests();
    });

    it('makes only one fetch per safety-poll interval across two tabs and converges both', async () => {
      setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
      let count = 1;
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeSummary(count++)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      );

      store.ensureStarted('user-1');
      secondStore.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);
      expect(fetchMock).toHaveBeenCalledTimes(2);

      const pollInterval = stateManager.isConnected
        ? INBOX_POLL_HEALTHY_MS
        : INBOX_POLL_UNHEALTHY_MS;
      await vi.advanceTimersByTimeAsync(pollInterval + 50);

      expect(fetchMock).toHaveBeenCalledTimes(3);
      expect(store.total).toBe(secondStore.total);
    });

    it('fetches on safety poll when the shared payload is stale for the active interval', async () => {
      setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeSummary(2)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      );

      store.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);
      expect(fetchMock).toHaveBeenCalledTimes(1);

      const pollInterval = stateManager.isConnected
        ? INBOX_POLL_HEALTHY_MS
        : INBOX_POLL_UNHEALTHY_MS;

      localStorage.setItem(
        INBOX_SHARE_KEY,
        JSON.stringify({
          userId: 'user-1',
          fetchedAt: Date.now() - pollInterval - 10_000,
          summary: makeSummary(99),
        })
      );

      await vi.advanceTimersByTimeAsync(pollInterval + 50);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(store.total).toBe(2);
    });

    it('continues fetching on subsequent safety polls after an event-triggered fetch', async () => {
      setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeSummary(1)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      );

      store.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);
      expect(fetchMock).toHaveBeenCalledTimes(1);

      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', { detail: { category: 'pending_secrets' } })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);

      const pollInterval = stateManager.isConnected
        ? INBOX_POLL_HEALTHY_MS
        : INBOX_POLL_UNHEALTHY_MS;
      await vi.advanceTimersByTimeAsync(pollInterval * 2 + 50);
      expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    it('refreshes on chat-stars-updated SSE event (design §6.5, AC-8)', async () => {
      setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeSummary(1)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      );

      store.ensureStarted('user-1');
      await vi.advanceTimersByTimeAsync(10);
      expect(fetchMock).toHaveBeenCalledTimes(1);

      stateManager.dispatchEvent(
        new CustomEvent('chat-stars-updated', { detail: { action: 'changed' } })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });
  });
});
