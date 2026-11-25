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
  previews: InboxSummaryResponse['categories']['approvals']['previews'] = []
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
        href: '/agents',
        previews: [],
      },
      reviews: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/artifacts?review_pending=1&owner=me',
        previews: [],
      },
      approvals: {
        state: 'ok',
        count,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews,
      },
      chat: {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/chat?filter=unread',
        previews: [],
      },
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

  it('fetches previews=0 for badge-only mode and previews=3 when page is mounted', async () => {
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

    // Mounting the inbox page triggers a fetch with previews=3.
    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe('/api/v1/users/me/inbox/summary?previews=3');
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

    // Fire 3 hints within 500ms -> should coalesce into 1 fetch after 750ms trailing.
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'approvals' } })
    );
    await vi.advanceTimersByTimeAsync(300);
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'approvals' } })
    );
    await vi.advanceTimersByTimeAsync(300);
    window.dispatchEvent(new CustomEvent(PENDING_SECRETS_CHANGED_EVENT, { detail: {} }));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Continuous burst exceeding max-wait (4s) still flushes at max-wait.
    for (let elapsed = 0; elapsed < INBOX_MAX_WAIT_MS + 100; elapsed += 400) {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', { detail: { category: 'approvals' } })
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

      // Simulate another tab writing an updated summary to localStorage.
      const updated = makeSummary(5);
      localStorage.setItem(
        INBOX_SHARE_KEY,
        JSON.stringify({
          userId: 'user-1',
          previews: 3,
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

  it('optimistically removes an approval and refetches', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const preview = {
      id: 'ps-1',
      kind: 'pending_secret',
      projectId: 'p1',
      projectName: 'Proj',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/profile/secrets',
      key: 'MY_KEY',
      agentSlug: 'bot-1',
      replacesExisting: false,
      allowProgeny: true,
      expiresAt: '2026-11-24T17:00:00Z',
    };
    fetchMock
      .mockResolvedValueOnce(
        new Response(JSON.stringify(makeSummary(1, [preview])), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(makeSummary(0, [])), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(store.total).toBe(1);
    expect(store.summary?.categories.approvals.previews).toHaveLength(1);

    store.optimisticRemoveApproval('ps-1');
    expect(store.total).toBe(0);
    expect(store.summary?.categories.approvals.previews).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    unmount();
  });

  it('marks unavailable404 when the hub returns 404', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(new Response('', { status: 404 }));

    store.ensureStarted('user-1');
    await vi.advanceTimersByTimeAsync(10);

    expect(store.unavailable404).toBe(true);
    expect(store.summary).toBeNull();
  });

  describe('waiting-hint filter (§5.2)', () => {
    function makeWaitingSummary(agentIds: string[]): InboxSummaryResponse {
      const s = makeSummary(0);
      s.total = agentIds.length;
      s.categories.waiting = {
        state: 'ok',
        count: agentIds.length,
        capped: false,
        countsTowardTotal: true,
        href: '/agents',
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
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('refetches when a known agent leaves waiting_for_input (non-waiting or cleared activity)', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-known', activity: 'thinking' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);

      // Cleared/omitted activity (e.g. stopped/suspended) also counts as leaving waiting_for_input.
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-known' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    it('ignores non-waiting activity hints for an unknown agent', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-other', activity: 'thinking' },
        })
      );
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting', agentId: 'agent-other', activity: '' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('refetches when a waiting hint omits agentId', async () => {
      stateManager.dispatchEvent(
        new CustomEvent('inbox-changed', {
          detail: { category: 'waiting' },
        })
      );
      await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });
  });

  it('refetches on decisions_waiting notification and decision-updated events', async () => {
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
      new CustomEvent('notification-created', {
        detail: { kind: 'decisions_waiting', status: 'DECISIONS_WAITING', resourceId: '2' },
      })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { decision: { rowId: 'row-1', status: 'decided' } },
      })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('optimistically removes a decision and refetches', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const decisionPreview = {
      id: 'msg-1',
      projectId: 'p1',
      projectName: 'Proj',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/chat/space/p1/thread/t1#msg-msg-1',
      promptMessageId: 'msg-1',
      conversationLabel: '#deploy',
      askerAgentName: 'deploy-bot',
      question: 'Ship to prod?',
      decisionCount: 1,
      answerMode: 'single',
      canAnswer: true,
      inline: {
        decisionRowId: 'row-1',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
        allowsOther: false,
      },
    };
    const withDecision = makeSummary(0);
    withDecision.total = 1;
    withDecision.categories.decisions = {
      state: 'ok',
      count: 1,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/decisions?tab=pending',
      previews: [decisionPreview],
    };
    const withoutDecision = makeSummary(0);
    withoutDecision.categories.decisions = {
      state: 'ok',
      count: 0,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/decisions?tab=pending',
      previews: [],
    };

    fetchMock
      .mockResolvedValueOnce(
        new Response(JSON.stringify(withDecision), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(withoutDecision), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(store.total).toBe(1);
    expect(store.summary?.categories.decisions.previews).toHaveLength(1);

    const nextCount = store.optimisticRemoveDecision('msg-1');
    expect(nextCount).toBe(0);
    expect(store.total).toBe(0);
    expect(store.summary?.categories.decisions.count).toBe(0);
    expect(store.summary?.categories.decisions.previews).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(10);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    unmount();
  });

  it('refetches on chat-message-received and chat-read-state-updated events', async () => {
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

    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    stateManager.dispatchEvent(new CustomEvent('chat-read-state-updated', { detail: {} }));
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('refetches on inbox-changed for reviews and notifications categories', async () => {
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
      new CustomEvent('inbox-changed', { detail: { category: 'reviews' } })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { category: 'notifications' } })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('refetches on ARTIFACT_REVIEW_SUBMITTED and ARTIFACT_REVIEW_REQUESTED notifications', async () => {
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
      new CustomEvent('notification-created', {
        detail: {
          kind: 'artifact_review_summary',
          status: 'ARTIFACT_REVIEW_SUBMITTED',
          resourceType: 'artifact',
          resourceId: 'art-1',
        },
      })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    stateManager.dispatchEvent(
      new CustomEvent('notification-created', {
        detail: {
          kind: 'artifact_grant',
          status: 'ARTIFACT_REVIEW_REQUESTED',
          resourceType: 'artifact',
          resourceId: 'art-2',
        },
      })
    );
    await vi.advanceTimersByTimeAsync(INBOX_DEBOUNCE_MS + 20);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('optimistically removes a review and supports targeted rollback without clobbering concurrent removals', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const rev1 = {
      id: 'notif-1',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:10:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'Design Doc A',
      scopeRef: 'proj-1',
      requestedBy: 'alice',
      notificationId: 'notif-1',
    };
    const rev2 = {
      id: 'notif-2',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:05:00Z',
      href: '/projects/proj-1/artifacts/art-2',
      artifactId: 'art-2',
      title: 'Design Doc B',
      scopeRef: 'proj-1',
      requestedBy: 'bob',
      notificationId: 'notif-2',
    };
    const withReviews = makeSummary(0);
    withReviews.total = 3;
    withReviews.categories.reviews = {
      state: 'ok',
      count: 3,
      capped: false,
      countsTowardTotal: true,
      href: '/artifacts?review_pending=1&owner=me',
      breakdown: { toAddress: 1, requested: 2 },
      previews: [rev1, rev2],
    };

    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(withReviews), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(store.total).toBe(3);
    expect(store.summary?.categories.reviews.previews).toHaveLength(2);

    // Remove both concurrently; no immediate refetch before caller's POST settles.
    const rollback1 = store.optimisticRemoveReview('notif-1');
    const rollback2 = store.optimisticRemoveReview('notif-2');
    expect(rollback1).not.toBeNull();
    expect(rollback2).not.toBeNull();
    expect(store.total).toBe(1);
    expect(store.summary?.categories.reviews.count).toBe(1);
    expect(store.summary?.categories.reviews.breakdown).toEqual({ toAddress: 1, requested: 0 });
    expect(store.summary?.categories.reviews.previews).toHaveLength(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Roll back only rev1 -> rev1 is restored while rev2 stays removed.
    rollback1!();
    expect(store.total).toBe(2);
    expect(store.summary?.categories.reviews.count).toBe(2);
    expect(store.summary?.categories.reviews.breakdown).toEqual({ toAddress: 1, requested: 1 });
    expect(store.summary?.categories.reviews.previews.map((p) => p.id)).toEqual(['notif-1']);

    // Calling rollback1 a second time is a no-op.
    rollback1!();
    expect(store.total).toBe(2);
    expect(store.summary?.categories.reviews.previews).toHaveLength(1);
    unmount();
  });

  it('does not duplicate a review row or inflate count when rollback runs after a refresh that still includes the row', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const rev1 = {
      id: 'notif-1',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:10:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'Design Doc A',
      scopeRef: 'proj-1',
      requestedBy: 'alice',
      notificationId: 'notif-1',
    };
    const rev2 = {
      id: 'notif-2',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:05:00Z',
      href: '/projects/proj-1/artifacts/art-2',
      artifactId: 'art-2',
      title: 'Design Doc B',
      scopeRef: 'proj-1',
      requestedBy: 'bob',
      notificationId: 'notif-2',
    };
    const withReviews = makeSummary(0);
    withReviews.total = 3;
    withReviews.categories.reviews = {
      state: 'ok',
      count: 3,
      capped: false,
      countsTowardTotal: true,
      href: '/artifacts?review_pending=1&owner=me',
      breakdown: { toAddress: 1, requested: 2 },
      previews: [rev1, rev2],
    };

    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(withReviews), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );

    const unmount = store.mountPage('user-1');
    await vi.advanceTimersByTimeAsync(10);
    expect(store.summary?.categories.reviews.previews.map((p) => p.id)).toEqual([
      'notif-1',
      'notif-2',
    ]);

    // Optimistically remove notif-1, then a summary fetch arrives that still contains notif-1.
    const rollback = store.optimisticRemoveReview('notif-1');
    expect(rollback).not.toBeNull();
    expect(store.summary?.categories.reviews.count).toBe(2);

    store.requestRefresh({ immediate: true, force: true });
    await vi.advanceTimersByTimeAsync(10);
    expect(store.summary?.categories.reviews.previews.map((p) => p.id)).toEqual([
      'notif-1',
      'notif-2',
    ]);
    expect(store.summary?.categories.reviews.count).toBe(3);

    // Rolling back after the refresh must be a no-op (no duplicate row, unchanged count/total).
    rollback!();
    expect(store.summary?.categories.reviews.previews.map((p) => p.id)).toEqual([
      'notif-1',
      'notif-2',
    ]);
    expect(store.summary?.categories.reviews.count).toBe(3);
    expect(store.summary?.categories.reviews.breakdown).toEqual({ toAddress: 1, requested: 2 });
    expect(store.total).toBe(3);
    unmount();
  });
});
