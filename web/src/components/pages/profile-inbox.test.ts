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
  inboxSummaryStore,
  type InboxCategory,
  type InboxSummaryResponse,
} from '../../client/inbox-summary.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import './profile-inbox.js';
import { INBOX_LINK_HREFS, type ScionPageProfileInbox } from './profile-inbox.js';

function makeCat(overrides: Partial<InboxCategory> & { href: string }): InboxCategory {
  return {
    state: 'ok',
    count: 0,
    capped: false,
    countsTowardTotal: true,
    previews: [],
    ...overrides,
  };
}

function makeSummary(
  overrides?: Partial<InboxSummaryResponse['categories']>
): InboxSummaryResponse {
  const categories: InboxSummaryResponse['categories'] = {
    decisions: makeCat({
      href: '/profile/decisions?tab=pending',
      breakdown: { agents: 0, projects: 0 },
    }),
    waiting: makeCat({
      href: '/agents?q=activity%3Awaiting_for_input',
      agentIds: [],
      breakdown: { projects: 0 },
    }),
    pending_secrets: makeCat({
      href: '/profile/secrets',
      breakdown: { agents: 0, projects: 0 },
    }),
    reviews: makeCat({
      href: '/artifacts?review_on_latest=1&owner=me',
      breakdown: { toAddress: 0, requested: 0 },
    }),
    chat: makeCat({
      href: '/chat',
      breakdown: { unreadConversations: 0, dms: 0, mentions: 0 },
    }),
    ...overrides,
  };
  let total = 0;
  let totalCapped = false;
  for (const cat of Object.values(categories)) {
    if (cat && cat.state === 'ok' && cat.countsTowardTotal) {
      total += cat.count;
      if (cat.capped) totalCapped = true;
    }
  }
  return {
    generatedAt: '2026-11-25T18:00:00Z',
    total,
    totalCapped,
    categories,
  };
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

describe('<scion-page-profile-inbox>', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    inboxSummaryStore.resetForTests();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    inboxSummaryStore.resetForTests();
    vi.unstubAllGlobals();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('renders 404 when web.inbox_dashboard flag is off', async () => {
    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders 404 when hub returns 404', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(new Response('', { status: 404 }));

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
  });

  it('renders all five cards in canonical order with descriptors, prose, badges, and key-noun inline links (AC-1, AC-2, AC-4, AC-9, AC-15, AC-16)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const summary = makeSummary({
      decisions: makeCat({
        count: 8,
        href: '/profile/decisions?tab=pending',
        breakdown: { agents: 3, projects: 2 },
      }),
      waiting: makeCat({
        count: 2,
        href: '/agents?q=activity%3Awaiting_for_input',
        agentIds: ['a1', 'a2'],
        breakdown: { projects: 1 },
      }),
      pending_secrets: makeCat({
        count: 50,
        capped: true,
        href: '/profile/secrets',
        breakdown: { agents: 4, projects: 2 },
      }),
      reviews: makeCat({
        count: 3,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 2, requested: 1 },
      }),
      chat: makeCat({
        count: 5,
        href: '/chat',
        breakdown: {
          unreadConversations: 5,
          dms: 1,
          mentions: 2,
          starredMessages: 4,
          projects: 2,
        },
      }),
    });

    fetchMock.mockResolvedValue(
      new Response(JSON.stringify(summary), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    // Fetches previews=0.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/users/me/inbox/summary?previews=0',
      expect.any(Object)
    );

    const cards = Array.from(page.shadowRoot?.querySelectorAll('[data-inbox-card]') ?? []);
    expect(cards.map((c) => c.getAttribute('data-inbox-card'))).toEqual([
      'decisions',
      'waiting',
      'pending_secrets',
      'reviews',
      'chat',
    ]);

    // No card header contains a link, no footer actions row or "Details" / "View all" links exist.
    expect(page.shadowRoot?.querySelector('.card-header a')).toBeNull();
    expect(page.shadowRoot?.querySelector('.card-actions')).toBeNull();
    expect(page.shadowRoot?.querySelector('[data-card-action]')).toBeNull();
    expect(page.shadowRoot?.textContent).not.toContain('View all');
    expect(page.shadowRoot?.textContent).not.toContain('Details');

    // No inline triage controls anywhere on the page.
    expect(page.shadowRoot?.querySelector('sl-dialog')).toBeNull();
    expect(page.shadowRoot?.querySelector('[data-approve-btn]')).toBeNull();
    expect(page.shadowRoot?.querySelector('[data-decision-choice-btn]')).toBeNull();
    expect(page.shadowRoot?.querySelector('[data-review-dismiss-btn]')).toBeNull();
    expect(page.shadowRoot?.querySelector('.chat-chip')).toBeNull();

    const checkCard = (
      key: string,
      title: string,
      badge: string,
      prose: string,
      expectedLinks: Array<{ text: string; href: string }>
    ): void => {
      const card = page.shadowRoot?.querySelector(`[data-inbox-card="${key}"]`);
      expect(card).not.toBeNull();
      expect(card?.querySelector('.card-title')?.textContent?.trim()).toBe(title);
      expect(card?.querySelector('[data-card-descriptor]')?.textContent?.trim()).toBeTruthy();
      expect(card?.querySelector('[data-card-count]')?.textContent?.trim()).toBe(badge);
      expect(card?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(prose);
      expect(card?.querySelector('.card-actions')).toBeNull();

      const links = Array.from(
        card?.querySelectorAll('[data-card-prose] a.card-prose-link') ?? []
      ) as HTMLAnchorElement[];
      expect(
        links.map((a) => ({
          text: a.textContent?.trim(),
          href: a.getAttribute('href'),
        }))
      ).toEqual(expectedLinks);

      // AC-15: visible text is the accessible name (no overriding aria-label).
      for (const a of links) {
        expect(a.hasAttribute('aria-label')).toBe(false);
      }
    };

    checkCard('decisions', 'Decisions', '8', '8 decisions from 3 agents in 2 projects', [
      { text: '8 decisions', href: '/profile/decisions?tab=pending' },
    ]);
    checkCard('waiting', 'Agents Waiting', '2', '2 agents waiting in 1 project', [
      { text: '2 agents waiting', href: '/agents?q=activity%3Awaiting_for_input' },
    ]);
    checkCard(
      'pending_secrets',
      'Pending Secrets',
      '50+',
      '50+ pending secrets from 4 agents in 2 projects',
      [{ text: '50+ pending secrets', href: '/profile/secrets' }]
    );
    checkCard('reviews', 'Reviews', '3', '3 pending reviews', [
      { text: '3 pending reviews', href: '/artifacts?review_on_latest=1&owner=me' },
    ]);
    checkCard(
      'chat',
      'Chat',
      '5',
      '5 unread conversations, 2 with mentions, 4 starred messages across 2 projects',
      [
        { text: '5 unread conversations', href: '/chat?filter=unread' },
        { text: '2 with mentions', href: '/chat?filter=mentions' },
        { text: '4 starred messages', href: '/chat?filter=starred' },
      ]
    );

    // AC-16: Phase 3 chat filter links are active; no rendered link targets Phase 4 unhydrated reviews filter routes.
    const allLinks = Array.from(page.shadowRoot?.querySelectorAll('a[href]') ?? []);
    for (const a of allLinks) {
      const href = a.getAttribute('href') ?? '';
      expect(href).not.toContain('requested=1');
      expect(href).not.toContain('drafts=1');
    }
    expect(Object.keys(INBOX_LINK_HREFS).sort()).toEqual([
      'chat-mentions',
      'chat-starred',
      'chat-unread',
    ]);
    expect(INBOX_LINK_HREFS['reviews-to-address']).toBeUndefined();
    expect(INBOX_LINK_HREFS['reviews-requested']).toBeUndefined();
    expect(INBOX_LINK_HREFS['reviews-drafts']).toBeUndefined();
  });

  it('hides disabled cards, renders unavailable sign-in note, error Retry, and Nothing Waiting with no links when 0', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const summary = makeSummary({
      decisions: makeCat({
        state: 'disabled',
        count: 0,
        countsTowardTotal: false,
        href: '/profile/decisions?tab=pending',
      }),
      waiting: makeCat({
        state: 'ok',
        count: 0,
        href: '/agents?q=activity%3Awaiting_for_input',
        breakdown: { projects: 0 },
      }),
      pending_secrets: makeCat({
        state: 'unavailable',
        count: 0,
        countsTowardTotal: false,
        href: '/profile/secrets',
      }),
      reviews: makeCat({
        state: 'error',
        count: 0,
        countsTowardTotal: false,
        errorCode: 'timeout',
        href: '/artifacts?review_on_latest=1&owner=me',
      }),
      chat: makeCat({
        state: 'ok',
        count: 0,
        href: '/chat',
        breakdown: { unreadConversations: 0, dms: 0, mentions: 0 },
      }),
    });

    fetchMock.mockResolvedValue(
      new Response(JSON.stringify(summary), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    // Disabled card is hidden.
    expect(page.shadowRoot?.querySelector('[data-inbox-card="decisions"]')).toBeNull();

    // Empty ok cards show "Nothing Waiting", no badge, and no links.
    const waitingCard = page.shadowRoot?.querySelector('[data-inbox-card="waiting"]');
    expect(waitingCard?.querySelector('[data-card-empty]')?.textContent?.trim()).toBe(
      'Nothing Waiting'
    );
    expect(waitingCard?.querySelector('[data-card-count]')).toBeNull();
    expect(waitingCard?.querySelector('a')).toBeNull();

    // Unavailable card shows descriptor and sign-in message, no badge, no links.
    const pendingCard = page.shadowRoot?.querySelector('[data-inbox-card="pending_secrets"]');
    expect(pendingCard?.querySelector('[data-card-descriptor]')?.textContent?.trim()).toBeTruthy();
    expect(pendingCard?.querySelector('[data-card-unavailable]')?.textContent?.trim()).toBe(
      'Not available with this sign-in'
    );
    expect(pendingCard?.querySelector('[data-card-count]')).toBeNull();
    expect(pendingCard?.querySelector('a')).toBeNull();

    // Error card shows "Couldn't load" and Retry button, no links.
    const reviewsCard = page.shadowRoot?.querySelector('[data-inbox-card="reviews"]');
    expect(reviewsCard?.querySelector('[data-card-error]')?.textContent).toContain("Couldn't load");
    expect(reviewsCard?.querySelector('a')).toBeNull();
    const retryBtn = reviewsCard?.querySelector(
      '[data-card-error] sl-button'
    ) as HTMLElement | null;
    expect(retryBtn).not.toBeNull();
    retryBtn?.click();
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('shows all-caught-up banner when every enabled card is ok with count 0', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify(makeSummary()), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('[data-inbox-caught-up]')).not.toBeNull();
    expect(page.shadowRoot?.querySelectorAll('.card-prose-link').length).toBe(0);
    expect(page.shadowRoot?.querySelectorAll('.card-actions').length).toBe(0);
  });

  it('renders starred-only chat prose and link without badge when unread is 0 (AC-10) and omits starred link when gated off (AC-13)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const starredOnly = makeSummary({
      chat: makeCat({
        count: 0,
        href: '/chat',
        breakdown: {
          unreadConversations: 0,
          dms: 0,
          mentions: 0,
          starredMessages: 4,
          projects: 2,
        },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(starredOnly), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    expect(chatCard?.querySelector('[data-card-count]')).toBeNull();
    expect(chatCard?.querySelector('[data-card-empty]')).toBeNull();
    expect(chatCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '4 starred messages across 2 projects'
    );
    const links = Array.from(
      chatCard?.querySelectorAll('a.card-prose-link') ?? []
    ) as HTMLAnchorElement[];
    expect(
      links.map((a) => ({ text: a.textContent?.trim(), href: a.getAttribute('href') }))
    ).toEqual([{ text: '4 starred messages', href: '/chat?filter=starred' }]);

    // AC-13: when web.chat_starred_messages is off, hub omits starredMessages -> no starred link.
    const gateOff = makeSummary({
      chat: makeCat({
        count: 2,
        href: '/chat',
        breakdown: { unreadConversations: 2, dms: 0, mentions: 0, projects: 1 },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(gateOff), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;

    expect(chatCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '2 unread conversations across 1 project'
    );
    const gateOffLinks = Array.from(
      chatCard?.querySelectorAll('a.card-prose-link') ?? []
    ) as HTMLAnchorElement[];
    expect(
      gateOffLinks.map((a) => ({ text: a.textContent?.trim(), href: a.getAttribute('href') }))
    ).toEqual([{ text: '2 unread conversations', href: '/chat?filter=unread' }]);
  });
});
