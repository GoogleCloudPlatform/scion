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
 * Inbox sub-navigation tab strip (`Overview | Decisions`) uses the same
 * 404-probe visibility rule for Decisions as `<scion-profile-nav>`.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'lit';

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

import { renderInboxTabStrip } from './inbox-tabs.js';
import '../pages/profile-inbox.js';
import type { ScionPageProfileInbox } from '../pages/profile-inbox.js';
import { inboxSummaryStore } from '../../client/inbox-summary.js';
import {
  dispatchProfileDecisionsCountChanged,
  dispatchProfileDecisionsNotFound,
  resetProfileDecisionProbeCacheForTests,
} from '../../client/profile-decisions.js';
import {
  DECISIONS_FLAG,
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

describe('inbox-tabs mixed-version Decisions visibility', () => {
  beforeEach(() => {
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    inboxSummaryStore.resetForTests();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    inboxSummaryStore.resetForTests();
    resetProfileDecisionProbeCacheForTests();
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('hides Decisions tab when probe availability is unknown or 404, and shows it once available', () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    setFeatureFlag(DECISIONS_FLAG, true);

    const host = document.createElement('div');
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab="overview"]')).not.toBeNull();
    expect(host.querySelector('[data-inbox-tab="decisions"]')).toBeNull();

    dispatchProfileDecisionsNotFound();
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab="decisions"]')).toBeNull();

    dispatchProfileDecisionsCountChanged(2);
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab="decisions"]')).not.toBeNull();
  });

  it('on <scion-page-profile-inbox>, hides Decisions tab on 404 hub and shows it on 200 hub', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    setFeatureFlag(DECISIONS_FLAG, true);

    let decisionsStatus = 404;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes('/api/v1/users/me/decisions')) {
          return Promise.resolve(
            new Response(
              decisionsStatus === 200 ? JSON.stringify({ counts: { waitingOnYou: 1 } }) : '{}',
              { status: decisionsStatus, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        if (url.includes('/api/v1/users/me/inbox/summary')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                generatedAt: '2026-11-24T12:00:00Z',
                total: 0,
                totalCapped: false,
                categories: {
                  decisions: {
                    state: 'disabled',
                    count: 0,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/profile/decisions',
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
                    href: '/artifacts',
                    previews: [],
                  },
                  pending_secrets: {
                    state: 'ok',
                    count: 0,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/profile/secrets',
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
                },
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(new Response('{}', { status: 404 }));
      })
    );

    const page404 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page404);
    await flush();
    await page404.updateComplete;
    expect(page404.shadowRoot!.querySelector('[data-inbox-tab="overview"]')).not.toBeNull();
    expect(page404.shadowRoot!.querySelector('[data-inbox-tab="decisions"]')).toBeNull();
    page404.remove();

    // Reset cache and simulate a hub where /api/v1/users/me/decisions returns 200.
    resetProfileDecisionProbeCacheForTests();
    decisionsStatus = 200;
    const page200 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page200);
    await flush();
    await page200.updateComplete;
    expect(page200.shadowRoot!.querySelector('[data-inbox-tab="overview"]')).not.toBeNull();
    expect(page200.shadowRoot!.querySelector('[data-inbox-tab="decisions"]')).not.toBeNull();
  });
});
