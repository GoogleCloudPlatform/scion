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
 * Tests for `inbox-summary-loader.ts` and `InboxSummaryStore` user-switching
 * and chunk-import failure recovery.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  ensureInboxSummaryStarted,
  formatInboxSummaryBadge,
  getInboxSummaryTotal,
  setInboxSummaryImporterForTests,
} from './inbox-summary-loader.js';
import { inboxSummaryStore, type InboxSummaryResponse } from './inbox-summary.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../utils/feature-flags.js';

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
    generatedAt: '2026-11-24T14:00:00Z',
    total,
    totalCapped,
    categories: {
      decisions: disabled,
      waiting: disabled,
      reviews: disabled,
      approvals: {
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

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

describe('inbox-summary-loader & store user switching / import recovery', () => {
  beforeEach(() => {
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    inboxSummaryStore.resetForTests();
  });

  afterEach(() => {
    inboxSummaryStore.resetForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('recovers from a transient dynamic import rejection on the next call', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify(makeSummary(6)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    let attempts = 0;
    setInboxSummaryImporterForTests(() => {
      attempts += 1;
      if (attempts === 1) {
        return Promise.reject(new Error('chunk network error'));
      }
      return import('./inbox-summary.js');
    });

    ensureInboxSummaryStarted('user-1');
    await flush();
    expect(attempts).toBe(1);
    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(getInboxSummaryTotal()).toBe(0);

    // Subsequent call retries instead of staying stuck on the rejected promise.
    ensureInboxSummaryStarted('user-1');
    await flush();
    expect(attempts).toBe(2);
    expect(getInboxSummaryTotal()).toBe(6);
    expect(formatInboxSummaryBadge()).toBe('6');
  });

  it('clears the previous user badge and refetches when switching users without reload', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    let currentUserSummary = makeSummary(8);
    let resolveSecondFetch: ((res: Response) => void) | null = null;
    let fetchCount = 0;

    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        fetchCount += 1;
        if (fetchCount === 1) {
          return Promise.resolve(
            new Response(JSON.stringify(currentUserSummary), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return new Promise<Response>((resolve) => {
          resolveSecondFetch = resolve;
        });
      })
    );

    ensureInboxSummaryStarted('user-1');
    await flush();
    expect(getInboxSummaryTotal()).toBe(8);
    expect(formatInboxSummaryBadge()).toBe('8');

    // Switch to user-2: previous user's count clears immediately while user-2 loads.
    currentUserSummary = makeSummary(2);
    ensureInboxSummaryStarted('user-2');
    await flush();
    expect(getInboxSummaryTotal()).toBe(0);
    expect(formatInboxSummaryBadge()).toBeNull();
    expect(fetchCount).toBe(2);

    resolveSecondFetch!(
      new Response(JSON.stringify(currentUserSummary), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await flush();
    expect(getInboxSummaryTotal()).toBe(2);
    expect(formatInboxSummaryBadge()).toBe('2');
  });
});
