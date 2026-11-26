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
 * Data-driven Inbox sub-navigation tab strip (`Overview | Decisions`) and
 * `INBOX_CHILD_ROUTES` registry tests (ptone/scion#9291, design §6.4, AC-5).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'lit';

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

import { INBOX_CHILD_ROUTES, INBOX_TABS, renderInboxTabStrip } from './inbox-tabs.js';
import '../pages/profile-inbox.js';
import type { ScionPageProfileInbox } from '../pages/profile-inbox.js';
import '../pages/profile-decisions.js';
import type { ScionPageProfileDecisions } from '../pages/profile-decisions.js';
import { inboxSummaryStore } from '../../client/inbox-summary.js';
import { stateManager } from '../../client/state.js';
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

describe('inbox-tabs registry and visibility (ptone/scion#9291)', () => {
  beforeEach(() => {
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    inboxSummaryStore.resetForTests();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    stateManager.disconnect();
    inboxSummaryStore.resetForTests();
    resetProfileDecisionProbeCacheForTests();
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('derives INBOX_CHILD_ROUTES from INBOX_TABS in canonical order', () => {
    expect(INBOX_TABS.map((t) => t.id)).toEqual(['overview', 'decisions']);
    expect(INBOX_CHILD_ROUTES).toEqual(INBOX_TABS.map((t) => t.href));
    expect(INBOX_CHILD_ROUTES).toEqual(['/profile/inbox', '/profile/decisions']);
  });

  it('omits the tab strip when web.inbox_dashboard is off', () => {
    setFeatureFlag(DECISIONS_FLAG, true);
    dispatchProfileDecisionsCountChanged(2);

    const host = document.createElement('div');
    render(renderInboxTabStrip('overview', { decisionsVisible: true }), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).toBeNull();
  });

  it('omits the tab strip when fewer than two tabs are visible, and renders both once Decisions is available', () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    setFeatureFlag(DECISIONS_FLAG, true);

    const host = document.createElement('div');
    // Unknown probe availability -> only Overview visible (< 2) -> strip omitted
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).toBeNull();

    // 404 probe -> only Overview visible (< 2) -> strip omitted
    dispatchProfileDecisionsNotFound();
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).toBeNull();

    // Explicit decisionsVisible: false override -> strip omitted
    dispatchProfileDecisionsCountChanged(2);
    render(renderInboxTabStrip('overview', { decisionsVisible: false }), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).toBeNull();

    // Decisions available -> 2 tabs visible -> strip renders Overview | Decisions
    render(renderInboxTabStrip('overview'), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).not.toBeNull();
    const tabs = Array.from(host.querySelectorAll<HTMLAnchorElement>('[data-inbox-tab]'));
    expect(tabs.map((a) => a.dataset.inboxTab)).toEqual(['overview', 'decisions']);
    expect(tabs[0].classList.contains('active')).toBe(true);
    expect(tabs[0].getAttribute('aria-current')).toBe('page');
    expect(tabs[1].classList.contains('active')).toBe(false);
    expect(tabs[1].getAttribute('aria-current')).toBe('false');

    // Active tab switches to decisions
    render(renderInboxTabStrip('decisions'), host);
    const overviewTab = host.querySelector<HTMLAnchorElement>('[data-inbox-tab="overview"]')!;
    const decisionsTab = host.querySelector<HTMLAnchorElement>('[data-inbox-tab="decisions"]')!;
    expect(overviewTab.classList.contains('active')).toBe(false);
    expect(overviewTab.getAttribute('aria-current')).toBe('false');
    expect(decisionsTab.classList.contains('active')).toBe(true);
    expect(decisionsTab.getAttribute('aria-current')).toBe('page');

    // Decisions flag off -> only Overview visible -> strip omitted even with override
    setFeatureFlag(DECISIONS_FLAG, false);
    render(renderInboxTabStrip('overview', { decisionsVisible: true }), host);
    expect(host.querySelector('[data-inbox-tab-strip]')).toBeNull();
  });

  it('on <scion-page-profile-inbox>, omits strip on 404 hub and renders Overview (active) | Decisions on 200 hub', async () => {
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
    expect(page404.shadowRoot!.querySelector('[data-inbox-tab-strip]')).toBeNull();
    page404.remove();

    // Reset cache and simulate a hub where /api/v1/users/me/decisions returns 200.
    resetProfileDecisionProbeCacheForTests();
    decisionsStatus = 200;
    const page200 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page200);
    await flush();
    await page200.updateComplete;
    const overview = page200.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="overview"]'
    );
    const decisions = page200.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="decisions"]'
    );
    expect(overview).not.toBeNull();
    expect(overview?.classList.contains('active')).toBe(true);
    expect(decisions).not.toBeNull();
    expect(decisions?.classList.contains('active')).toBe(false);
  });

  it('on <scion-page-profile-decisions>, renders Overview | Decisions (Decisions active) when Inbox is on, and omits strip when Inbox is off', async () => {
    class StubEventSource extends EventTarget {
      readyState = 0;
      onopen: ((ev: Event) => void) | null = null;
      onerror: ((ev: Event) => void) | null = null;
      close(): void {
        this.readyState = 2;
      }
    }
    vi.stubGlobal('EventSource', StubEventSource);
    setFeatureFlag(DECISIONS_FLAG, true);

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes('/api/v1/users/me/decisions')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                counts: { waitingOnYou: 0, waitingOnOthers: 0 },
                filteredCounts: { waitingOnYou: 0, waitingOnOthers: 0 },
                groups: [],
                facets: { agents: [], projects: [] },
                truncatedProjects: false,
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(new Response('{}', { status: 404 }));
      })
    );

    // Inbox off -> no tab strip
    const pageOff = document.createElement(
      'scion-page-profile-decisions'
    ) as ScionPageProfileDecisions;
    document.body.appendChild(pageOff);
    await flush();
    await pageOff.updateComplete;
    expect(pageOff.shadowRoot!.querySelector('[data-inbox-tab-strip]')).toBeNull();
    pageOff.remove();

    // Inbox on -> strip renders with Decisions active
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const pageOn = document.createElement(
      'scion-page-profile-decisions'
    ) as ScionPageProfileDecisions;
    document.body.appendChild(pageOn);
    await flush();
    await pageOn.updateComplete;
    const overview = pageOn.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="overview"]'
    );
    const decisions = pageOn.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="decisions"]'
    );
    expect(overview).not.toBeNull();
    expect(overview?.classList.contains('active')).toBe(false);
    expect(decisions).not.toBeNull();
    expect(decisions?.classList.contains('active')).toBe(true);
    expect(decisions?.getAttribute('aria-current')).toBe('page');
  });
});
