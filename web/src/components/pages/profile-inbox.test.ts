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
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  ARTIFACTS_REVIEW_REQUESTS_FLAG,
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
    setFeatureFlag(ARTIFACTS_REVIEW_REQUESTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_REVIEW_DRAFTS_FLAG, true);
    const summary = makeSummary({
      decisions: makeCat({
        count: 8,
        href: '/profile/decisions?tab=pending',
        breakdown: { agents: 3, projects: 2 },
      }),
      agent_secrets: makeCat({
        count: 5,
        href: '/profile/agent-secrets',
        breakdown: { agents: 2, projects: 1, secretsProjects: 2 },
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
        breakdown: { toAddress: 2, requested: 1, drafts: 4 },
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
      'secrets',
      'waiting',
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
    checkCard(
      'secrets',
      'Secrets',
      '55+',
      '5 secret requests from 2 agents, 50+ pending secrets to approve, across 2 projects',
      [
        { text: '5 secret requests', href: '/profile/agent-secrets' },
        { text: '50+ pending secrets', href: '/profile/secrets' },
      ]
    );
    checkCard('waiting', 'Agents Waiting', '2', '2 agents waiting in 1 project', [
      { text: '2 agents waiting', href: '/agents?q=activity%3Awaiting_for_input' },
    ]);
    checkCard(
      'reviews',
      'Artifact Reviews',
      '3',
      '2 reviews to address, 1 review requested, 4 unfinished drafts',
      [
        { text: '2 reviews to address', href: '/artifacts?review_on_latest=1&owner=me' },
        { text: '1 review requested', href: '/artifacts?requested=1' },
        { text: '4 unfinished drafts', href: '/artifacts?drafts=1' },
      ]
    );
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

    // AC-16: Phase 3 chat and Phase 4 reviews filter links are active; reviews-to-address resolves via hub primary href.
    expect(Object.keys(INBOX_LINK_HREFS).sort()).toEqual([
      'chat-mentions',
      'chat-starred',
      'chat-unread',
      'reviews-drafts',
      'reviews-requested',
    ]);
    expect(INBOX_LINK_HREFS['reviews-to-address']).toBeUndefined();
    expect(INBOX_LINK_HREFS['reviews-requested']).toBe('/artifacts?requested=1');
    expect(INBOX_LINK_HREFS['reviews-drafts']).toBe('/artifacts?drafts=1');
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
    const secretsCard = page.shadowRoot?.querySelector('[data-inbox-card="secrets"]');
    expect(secretsCard?.querySelector('[data-card-descriptor]')?.textContent?.trim()).toBeTruthy();
    expect(secretsCard?.querySelector('[data-card-unavailable]')?.textContent?.trim()).toBe(
      'Not available with this sign-in'
    );
    expect(secretsCard?.querySelector('[data-card-count]')).toBeNull();
    expect(secretsCard?.querySelector('a')).toBeNull();

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

    // When hub omits starredMessages (e.g. starred scan degradation), no starred link is rendered.
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

    // AC-10: starredMessagesCapped renders N+ on starred clause only, never on badge or unread count.
    const cappedStarred = makeSummary({
      chat: makeCat({
        count: 2,
        capped: false,
        href: '/chat',
        breakdown: {
          unreadConversations: 2,
          dms: 0,
          mentions: 0,
          starredMessages: 4,
          starredMessagesCapped: 1,
          projects: 2,
        },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(cappedStarred), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;

    expect(chatCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('2');
    expect(chatCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '2 unread conversations, 4+ starred messages across 2 projects'
    );
  });

  it('merged Secrets card handles disabled+ok, error+ok, and both disabled states', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    // 1. agent_secrets disabled, pending_secrets ok -> renders only pending_secrets clause using pending_secrets.breakdown.projects
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify(
          makeSummary({
            agent_secrets: makeCat({
              state: 'disabled',
              count: 0,
              countsTowardTotal: false,
              href: '/profile/agent-secrets',
            }),
            pending_secrets: makeCat({
              state: 'ok',
              count: 2,
              href: '/profile/secrets',
              breakdown: { agents: 1, projects: 2 },
            }),
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    const page1 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page1);
    await flush();
    await page1.updateComplete;

    const card1 = page1.shadowRoot?.querySelector('[data-inbox-card="secrets"]');
    expect(card1?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('2');
    expect(card1?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '2 pending secrets to approve, across 2 projects'
    );
    const links1 = Array.from(card1?.querySelectorAll('a.card-prose-link') ?? []);
    expect(links1.map((a) => [a.textContent?.trim(), a.getAttribute('href')])).toEqual([
      ['2 pending secrets', '/profile/secrets'],
    ]);
    page1.remove();
    inboxSummaryStore.resetForTests();

    // 2. agent_secrets error, pending_secrets ok -> shows Retry error state
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify(
          makeSummary({
            agent_secrets: makeCat({
              state: 'error',
              errorCode: 'timeout',
              count: 0,
              countsTowardTotal: false,
              href: '/profile/agent-secrets',
            }),
            pending_secrets: makeCat({
              state: 'ok',
              count: 1,
              href: '/profile/secrets',
              breakdown: { agents: 1, projects: 1 },
            }),
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    const page2 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page2);
    await flush();
    await page2.updateComplete;

    const card2 = page2.shadowRoot?.querySelector('[data-inbox-card="secrets"]');
    expect(card2?.querySelector('[data-card-error]')?.textContent).toContain("Couldn't load");
    page2.remove();
    inboxSummaryStore.resetForTests();

    // 3. both disabled -> Secrets card is hidden
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify(
          makeSummary({
            agent_secrets: makeCat({
              state: 'disabled',
              count: 0,
              countsTowardTotal: false,
              href: '/profile/agent-secrets',
            }),
            pending_secrets: makeCat({
              state: 'disabled',
              count: 0,
              countsTowardTotal: false,
              href: '/profile/secrets',
            }),
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    const page3 = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page3);
    await flush();
    await page3.updateComplete;

    expect(page3.shadowRoot?.querySelector('[data-inbox-card="secrets"]')).toBeNull();
  });

  it('renders Artifact Reviews drafts-only without badge (AC-10), hides drafts when gated off (AC-13), and applies per-clause caps', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    setFeatureFlag(ARTIFACTS_REVIEW_REQUESTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_REVIEW_DRAFTS_FLAG, true);
    const draftsOnly = makeSummary({
      reviews: makeCat({
        count: 0,
        capped: false,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 0, requested: 0, drafts: 3 },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(draftsOnly), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const reviewsCard = page.shadowRoot?.querySelector('[data-inbox-card="reviews"]');
    expect(reviewsCard?.querySelector('.card-title')?.textContent?.trim()).toBe('Artifact Reviews');
    expect(reviewsCard?.querySelector('[data-card-descriptor]')?.textContent?.trim()).toBe(
      'Reviews on your artifacts, review requests for you, and your unfinished drafts'
    );
    expect(reviewsCard?.querySelector('[data-card-count]')).toBeNull();
    expect(reviewsCard?.querySelector('[data-card-empty]')).toBeNull();
    expect(reviewsCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '3 unfinished drafts'
    );
    const draftsOnlyLinks = Array.from(
      reviewsCard?.querySelectorAll('a.card-prose-link') ?? []
    ) as HTMLAnchorElement[];
    expect(
      draftsOnlyLinks.map((a) => ({ text: a.textContent?.trim(), href: a.getAttribute('href') }))
    ).toEqual([{ text: '3 unfinished drafts', href: '/artifacts?drafts=1' }]);

    // AC-13: when drafts key is omitted (hub.artifacts_review_drafts off), no drafts number or link renders.
    const draftsGateOff = makeSummary({
      reviews: makeCat({
        count: 3,
        capped: false,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 2, requested: 1 },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(draftsGateOff), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;

    expect(reviewsCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('3');
    expect(reviewsCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '2 reviews to address, 1 review requested'
    );
    const gateOffLinks = Array.from(
      reviewsCard?.querySelectorAll('a.card-prose-link') ?? []
    ) as HTMLAnchorElement[];
    expect(
      gateOffLinks.map((a) => ({ text: a.textContent?.trim(), href: a.getAttribute('href') }))
    ).toEqual([
      { text: '2 reviews to address', href: '/artifacts?review_on_latest=1&owner=me' },
      { text: '1 review requested', href: '/artifacts?requested=1' },
    ]);

    // AC-10: draftsCapped renders N+ on drafts clause only, never on badge or other clauses.
    const cappedDrafts = makeSummary({
      reviews: makeCat({
        count: 2,
        capped: false,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 1, requested: 1, drafts: 500, draftsCapped: 1 },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(cappedDrafts), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;

    expect(reviewsCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('2');
    expect(reviewsCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '1 review to address, 1 review requested, 500+ unfinished drafts'
    );

    // Per-clause cap on toAddress only leaves requested exact.
    const cappedToAddressOnly = makeSummary({
      reviews: makeCat({
        count: 502,
        capped: true,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 500, toAddressCapped: 1, requested: 2 },
      }),
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(cappedToAddressOnly), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;

    expect(reviewsCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('502+');
    expect(reviewsCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '500+ reviews to address, 2 reviews requested'
    );
  });

  it('leaves reviews-requested and reviews-drafts unlinked while their browser flags are off (AC-9, AC-13)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const summary = makeSummary({
      reviews: makeCat({
        count: 3,
        capped: false,
        href: '/artifacts?review_on_latest=1&owner=me',
        breakdown: { toAddress: 1, requested: 2, drafts: 3 },
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

    const reviewsCard = page.shadowRoot?.querySelector('[data-inbox-card="reviews"]');
    expect(reviewsCard?.querySelector('[data-card-prose]')?.textContent?.trim()).toBe(
      '1 review to address, 2 reviews requested, 3 unfinished drafts'
    );
    const getLinks = (): Array<{ text: string | undefined; href: string | null }> =>
      Array.from(reviewsCard?.querySelectorAll('a.card-prose-link') ?? []).map((a) => ({
        text: a.textContent?.trim(),
        href: a.getAttribute('href'),
      }));

    // Both flags off: only to-address is linked; requested and drafts stay plain text.
    expect(getLinks()).toEqual([
      { text: '1 review to address', href: '/artifacts?review_on_latest=1&owner=me' },
    ]);

    // Enabling hub.artifacts_review_requests links requested while drafts stays plain text.
    setFeatureFlag(ARTIFACTS_REVIEW_REQUESTS_FLAG, true);
    await page.updateComplete;
    expect(getLinks()).toEqual([
      { text: '1 review to address', href: '/artifacts?review_on_latest=1&owner=me' },
      { text: '2 reviews requested', href: '/artifacts?requested=1' },
    ]);

    // Enabling hub.artifacts_review_drafts links drafts too.
    setFeatureFlag(ARTIFACTS_REVIEW_DRAFTS_FLAG, true);
    await page.updateComplete;
    expect(getLinks()).toEqual([
      { text: '1 review to address', href: '/artifacts?review_on_latest=1&owner=me' },
      { text: '2 reviews requested', href: '/artifacts?requested=1' },
      { text: '3 unfinished drafts', href: '/artifacts?drafts=1' },
    ]);
  });
});
