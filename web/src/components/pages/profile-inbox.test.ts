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

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

import './profile-inbox.js';
import '../profile/profile-nav.js';
import '../shared/notification-tray.js';
import type { ScionPageProfileInbox } from './profile-inbox.js';
import type { ScionProfileNav } from '../profile/profile-nav.js';
import {
  NOTIFICATION_ACKED_EVENT,
  type ScionNotificationTray,
} from '../shared/notification-tray.js';
import { inboxSummaryStore, type InboxSummaryResponse } from '../../client/inbox-summary.js';
import { stateManager } from '../../client/state.js';
import { PENDING_SECRETS_CHANGED_EVENT } from '../../client/pending-secrets.js';
import {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  resetProfileDecisionProbeCacheForTests,
} from '../../client/profile-decisions.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { showToast } from '../../utils/toast.js';

function makeSummaryResponse(
  approvals: InboxSummaryResponse['categories']['approvals'],
  overrides: Partial<InboxSummaryResponse['categories']> = {}
): InboxSummaryResponse {
  const categories: InboxSummaryResponse['categories'] = {
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
    approvals,
    chat: {
      state: 'disabled',
      count: 0,
      capped: false,
      countsTowardTotal: true,
      href: '/chat',
      previews: [],
    },
    ...overrides,
  };
  let total = 0;
  let totalCapped = false;
  for (const c of Object.values(categories)) {
    if (c.state === 'ok' && c.countsTowardTotal) {
      total += c.count;
      if (c.capped) totalCapped = true;
    }
  }
  return {
    generatedAt: '2026-11-23T18:00:00Z',
    total,
    totalCapped,
    categories,
  };
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

function urlOf(input: RequestInfo | URL): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

describe('ScionPageProfileInbox & ProfileNav Inbox section', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    inboxSummaryStore.resetForTests();
    window.history.replaceState({}, '', '/profile/inbox');
    vi.mocked(showToast).mockClear();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    inboxSummaryStore.resetForTests();
    resetProfileDecisionProbeCacheForTests();
    vi.unstubAllGlobals();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.replaceState({}, '', '/profile/inbox');
  });

  it('renders 404 when web.inbox_dashboard is off', async () => {
    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders Approvals card, hides disabled cards, and supports inline approve and reject', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const preview = {
      id: 'ps-101',
      kind: 'pending_secret',
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/profile/secrets',
      key: 'DEPLOY_TOKEN',
      agentSlug: 'deploy-bot',
      injectionMode: 'as_needed',
      replacesExisting: true,
      allowProgeny: true,
      expiresAt: new Date(Date.now() + 3600_000).toISOString(),
    };

    const summaryWithOne = makeSummaryResponse({
      state: 'ok',
      count: 1,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/secrets',
      previews: [preview],
    });
    const summaryEmpty = makeSummaryResponse({
      state: 'ok',
      count: 0,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/secrets',
      previews: [],
    });

    let approved = false;
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(JSON.stringify(approved ? summaryEmpty : summaryWithOne), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.endsWith('/api/v1/pending-secrets/ps-101/approve') && init?.method === 'POST') {
        approved = true;
        return Promise.resolve(
          new Response(
            JSON.stringify({ id: 'ps-101', key: 'DEPLOY_TOKEN', scope: 'user', state: 'approved' }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const secretsChanged = vi.fn();
    window.addEventListener(PENDING_SECRETS_CHANGED_EVENT, secretsChanged);

    try {
      const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
      page.pageData = {
        path: '/profile/inbox',
        title: 'Inbox',
        user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
      };
      document.body.appendChild(page);
      await flush();
      await page.updateComplete;

      // Only the approvals card is rendered; disabled cards are hidden.
      const cards = page.shadowRoot?.querySelectorAll('[data-inbox-card]');
      expect(cards?.length).toBe(1);
      expect(cards?.[0]?.getAttribute('data-inbox-card')).toBe('approvals');
      expect(page.shadowRoot?.querySelector('[data-approval-row="ps-101"]')).not.toBeNull();

      // Click Approve -> opens confirm dialog, then confirm -> POST approve with application/json.
      const approveBtn = page.shadowRoot?.querySelector<HTMLElement>('[data-approve-btn="ps-101"]');
      expect(approveBtn).not.toBeNull();
      approveBtn!.click();
      await page.updateComplete;

      const confirmBtn = page.shadowRoot?.querySelector<HTMLElement>('[data-confirm-approve-btn]');
      expect(confirmBtn).not.toBeNull();
      const allowProgenyEl = page.shadowRoot?.querySelector('[data-confirm-allow-progeny]');
      expect(allowProgenyEl?.textContent?.trim()).toBe('Yes');
      confirmBtn!.click();
      await flush();
      await page.updateComplete;

      const approveCall = fetchMock.mock.calls.find((c) =>
        urlOf(c[0]).endsWith('/api/v1/pending-secrets/ps-101/approve')
      );
      expect(approveCall).toBeTruthy();
      expect((approveCall?.[1] as RequestInit | undefined)?.method).toBe('POST');
      expect(secretsChanged).toHaveBeenCalled();

      // After approval, the card shows empty state and caught-up banner.
      expect(page.shadowRoot?.querySelector('[data-inbox-caught-up]')).not.toBeNull();
      expect(page.shadowRoot?.querySelector('[data-card-empty]')).not.toBeNull();
    } finally {
      window.removeEventListener(PENDING_SECRETS_CHANGED_EVENT, secretsChanged);
    }
  });

  it('renders Inbox section with badge in profile nav when flag is on, and hides it when off', async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse({
                state: 'ok',
                count: 4,
                capped: false,
                countsTowardTotal: true,
                href: '/profile/secrets',
                previews: [],
              })
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ pendingSecrets: [], configured: false }), { status: 200 })
      );
    });

    // Flag off: no Inbox nav section.
    const navOff = document.createElement('scion-profile-nav') as ScionProfileNav;
    navOff.currentPath = '/profile/secrets';
    document.body.appendChild(navOff);
    await flush();
    await navOff.updateComplete;
    expect(navOff.shadowRoot?.querySelector('[data-inbox-nav-section]')).toBeNull();
    navOff.remove();

    // Flag on: Inbox section appears with badge 4.
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const navOn = document.createElement('scion-profile-nav') as ScionProfileNav;
    navOn.currentPath = '/profile/inbox';
    navOn.user = { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(navOn);
    await flush();
    await navOn.updateComplete;

    expect(navOn.shadowRoot?.querySelector('[data-inbox-nav-section]')).not.toBeNull();
    const badge = navOn.shadowRoot?.querySelector('[data-inbox-nav-badge]');
    expect(badge?.textContent?.trim()).toBe('4');
  });

  it('rounds sub-minute expiry up to 1m and prevents closing confirm dialog while busy', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const preview = {
      id: 'ps-202',
      kind: 'pending_secret',
      projectId: 'proj-1',
      projectName: 'Alpha',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/profile/secrets',
      key: 'SHORT_LIVED_KEY',
      agentSlug: 'deploy-bot',
      injectionMode: 'as_needed',
      replacesExisting: false,
      allowProgeny: false,
      expiresAt: new Date(Date.now() + 15_000).toISOString(),
    };

    let resolveApprove: ((res: Response) => void) | null = null;
    const summaryWithOne = makeSummaryResponse({
      state: 'ok',
      count: 1,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/secrets',
      previews: [preview],
    });
    const summaryEmpty = makeSummaryResponse({
      state: 'ok',
      count: 0,
      capped: false,
      countsTowardTotal: true,
      href: '/profile/secrets',
      previews: [],
    });

    let approved = false;
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(JSON.stringify(approved ? summaryEmpty : summaryWithOne), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.endsWith('/api/v1/pending-secrets/ps-202/approve') && init?.method === 'POST') {
        return new Promise<Response>((resolve) => {
          resolveApprove = resolve;
        });
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    // 15 seconds remaining should render "expires in 1m", not "expires in 0m".
    const row = page.shadowRoot?.querySelector('[data-approval-row="ps-202"]');
    expect(row?.textContent).toContain('expires in 1m');
    expect(row?.textContent).not.toContain('expires in 0m');

    // Open confirm dialog -> shows "No" for allowProgeny=false.
    page.shadowRoot?.querySelector<HTMLElement>('[data-approve-btn="ps-202"]')?.click();
    await page.updateComplete;
    expect(
      page.shadowRoot?.querySelector('[data-confirm-allow-progeny]')?.textContent?.trim()
    ).toBe('No');

    // Start approval so dialog is busy; sl-request-close must be prevented.
    page.shadowRoot?.querySelector<HTMLElement>('[data-confirm-approve-btn]')?.click();
    await page.updateComplete;

    const dialog = page.shadowRoot?.querySelector<HTMLElement>('[data-approve-confirm-dialog]');
    expect(dialog).not.toBeNull();
    const busyClose = new Event('sl-request-close', { cancelable: true });
    dialog!.dispatchEvent(busyClose);
    expect(busyClose.defaultPrevented).toBe(true);
    expect(dialog!.hasAttribute('open')).toBe(true);

    // Resolve the in-flight approval so the test cleans up cleanly.
    approved = true;
    resolveApprove!(
      new Response(
        JSON.stringify({ id: 'ps-202', key: 'SHORT_LIVED_KEY', scope: 'user', state: 'approved' }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await flush();
    await page.updateComplete;
    expect(dialog!.hasAttribute('open')).toBe(false);
  });

  it('renders all five cards in fixed order: Decisions, Agents waiting, Approvals, Reviews, Chat', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const summary = makeSummaryResponse(
      {
        state: 'ok',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        decisions: {
          state: 'ok',
          count: 2,
          capped: true,
          countsTowardTotal: true,
          href: '/profile/decisions?tab=pending',
          previews: [
            {
              id: 'm-1',
              projectId: 'p-1',
              projectName: 'Core API',
              createdAt: '2026-11-23T17:00:00Z',
              href: '/chat/space/p-1/thread/t-1#msg-m-1',
              promptMessageId: 'm-1',
              conversationLabel: '#deploys',
              askerAgentName: 'deploy-bot',
              question: 'Pick migration strategy?',
              decisionCount: 2,
              answerMode: 'set',
              canAnswer: true,
            },
          ],
        },
        waiting: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/agents',
          agentIds: ['agent-1'],
          previews: [
            {
              id: 'agent-1',
              projectId: 'p-1',
              projectName: 'Web',
              createdAt: '2026-11-23T17:00:00Z',
              href: '/agents/agent-1',
              agentId: 'agent-1',
              agentName: 'Frontend Dev',
              agentSlug: 'fe-dev',
              question: 'Need staging creds?',
              since: '2026-11-23T17:46:00Z',
            },
          ],
        },
        reviews: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/artifacts?review_pending=1&owner=me',
          breakdown: { toAddress: 1, requested: 0 },
          previews: [
            {
              id: 'art-1',
              kind: 'to_address',
              projectId: 'p-1',
              projectName: 'Web',
              createdAt: '2026-11-23T17:30:00Z',
              href: '/projects/p-1/artifacts/art-1',
              artifactId: 'art-1',
              title: 'RFC: Inbox Cards',
              scopeRef: 'p-1',
            },
          ],
        },
        chat: {
          state: 'error',
          count: 0,
          capped: false,
          countsTowardTotal: true,
          href: '/chat',
          errorCode: 'timeout',
          previews: [],
        },
      }
    );

    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(summary), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const cards = Array.from(page.shadowRoot?.querySelectorAll('[data-inbox-card]') ?? []).map(
      (el) => el.getAttribute('data-inbox-card')
    );
    expect(cards).toEqual(['decisions', 'waiting', 'approvals', 'reviews', 'chat']);

    // Capped decisions card shows "2+" and "View all" links to /profile/decisions?tab=pending.
    const decCard = page.shadowRoot?.querySelector('[data-inbox-card="decisions"]');
    expect(decCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('2+');
    expect(decCard?.querySelector('[data-card-view-all]')?.getAttribute('href')).toBe(
      '/profile/decisions?tab=pending'
    );
    expect(decCard?.querySelector('[data-decision-link]')?.getAttribute('href')).toBe(
      '/chat/space/p-1/thread/t-1#msg-m-1'
    );
    expect(decCard?.querySelector('[data-decision-inline]')).toBeNull();

    // Approvals card with 0 count renders compact "Nothing waiting".
    const appCard = page.shadowRoot?.querySelector('[data-inbox-card="approvals"]');
    expect(appCard?.classList.contains('inbox-card--compact')).toBe(true);
    expect(appCard?.querySelector('[data-card-empty]')?.textContent?.trim()).toBe(
      'Nothing waiting'
    );

    // Error chat card shows "Couldn't load" and Retry button triggers a refetch.
    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    const chatErr = chatCard?.querySelector('[data-card-error]');
    expect(chatErr?.textContent).toContain("Couldn't load");
    const callsBeforeRetry = fetchMock.mock.calls.length;
    chatErr?.querySelector<HTMLElement>('sl-button')?.click();
    await flush();
    expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBeforeRetry);
  });

  it('answers single-decision previews inline with surface:"inbox" and handles 409 batch_required + allowsOther', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    let answered = false;

    const decisionSingle = {
      id: 'msg-single',
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/chat/space/p-1/thread/t-1#msg-msg-single',
      promptMessageId: 'msg-single',
      conversationLabel: '#release',
      askerAgentName: 'release-bot',
      question: 'Deploy v2?',
      decisionCount: 1,
      answerMode: 'single',
      canAnswer: true,
      inline: {
        decisionRowId: 'row-single',
        choices: [
          { id: 'yes', label: 'Yes, deploy' },
          { id: 'no', label: 'Hold' },
        ],
        allowsOther: true,
      },
    };
    const decisionBatch = {
      id: 'msg-batch',
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:05:00Z',
      href: '/chat/space/p-1/thread/t-2#msg-msg-batch',
      promptMessageId: 'msg-batch',
      conversationLabel: '#ops',
      askerAgentName: 'ops-bot',
      question: 'Run backfill?',
      decisionCount: 1,
      answerMode: 'single',
      canAnswer: true,
      inline: {
        decisionRowId: 'row-batch',
        choices: [{ id: 'run', label: 'Run now' }],
        allowsOther: false,
      },
    };

    const summaryBefore = makeSummaryResponse(
      {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        decisions: {
          state: 'ok',
          count: 2,
          capped: false,
          countsTowardTotal: true,
          href: '/profile/decisions?tab=pending',
          previews: [decisionSingle, decisionBatch],
        },
      }
    );
    const summaryAfter = makeSummaryResponse(
      {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        decisions: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/profile/decisions?tab=pending',
          previews: [decisionBatch],
        },
      }
    );

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(JSON.stringify(answered ? summaryAfter : summaryBefore), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.endsWith('/api/v1/decisions/row-single/answer') && init?.method === 'POST') {
        answered = true;
        return Promise.resolve(
          new Response(
            JSON.stringify({
              decision: {
                rowId: 'row-single',
                status: 'decided',
              },
              responseMessageId: 'resp-1',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.endsWith('/api/v1/decisions/row-batch/answer') && init?.method === 'POST') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: {
                code: 'batch_required',
                message: 'must answer as a batch',
              },
            }),
            { status: 409, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const decisionsCountChanged = vi.fn();
    const navClicks: string[] = [];
    const onNavClick = (e: Event): void => {
      const detail = (e as CustomEvent<{ path?: string }>).detail;
      if (detail?.path) navClicks.push(detail.path);
    };
    window.addEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, decisionsCountChanged);
    document.addEventListener('nav-click', onNavClick);

    try {
      const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
      page.pageData = {
        path: '/profile/inbox',
        title: 'Inbox',
        user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
      };
      document.body.appendChild(page);
      await flush();
      await page.updateComplete;

      // allowsOther renders an "Other…" drill-in link to the preview href, and choice group is labelled for a11y.
      const inlineGroup = page.shadowRoot?.querySelector('[data-decision-inline="msg-single"]');
      expect(inlineGroup?.getAttribute('role')).toBe('group');
      expect(inlineGroup?.getAttribute('aria-label')).toBe('Deploy v2?');
      const otherLink = page.shadowRoot?.querySelector('[data-decision-other-link="msg-single"]');
      expect(otherLink?.getAttribute('href')).toBe('/chat/space/p-1/thread/t-1#msg-msg-single');

      // Click choice "yes" -> POST /api/v1/decisions/row-single/answer with surface: "inbox".
      const yesBtn = page.shadowRoot?.querySelector<HTMLElement>(
        '[data-decision-row="msg-single"] [data-decision-choice-btn="yes"]'
      );
      expect(yesBtn).not.toBeNull();
      yesBtn!.click();
      await flush();
      await page.updateComplete;

      const answerCall = fetchMock.mock.calls.find((c) =>
        urlOf(c[0]).endsWith('/api/v1/decisions/row-single/answer')
      );
      expect(answerCall).toBeTruthy();
      const answerInit = answerCall?.[1] as RequestInit | undefined;
      expect(answerInit?.method).toBe('POST');
      expect((answerInit?.headers as Record<string, string>)?.['Content-Type']).toBe(
        'application/json'
      );
      const answerBody = JSON.parse(answerInit?.body as string) as Record<string, unknown>;
      expect(answerBody.choice_id).toBe('yes');
      expect(answerBody.surface).toBe('inbox');
      expect(typeof answerBody.idempotency_key).toBe('string');

      // Optimistically removed and synced to /profile/decisions count listeners.
      expect(page.shadowRoot?.querySelector('[data-decision-row="msg-single"]')).toBeNull();
      expect(decisionsCountChanged).toHaveBeenCalled();

      // 409 batch_required falls back to drill-in navigation via item.href.
      const batchBtn = page.shadowRoot?.querySelector<HTMLElement>(
        '[data-decision-row="msg-batch"] [data-decision-choice-btn="run"]'
      );
      expect(batchBtn).not.toBeNull();
      batchBtn!.click();
      await flush();
      await page.updateComplete;

      expect(navClicks).toContain('/chat/space/p-1/thread/t-2#msg-msg-batch');
    } finally {
      window.removeEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, decisionsCountChanged);
      document.removeEventListener('nav-click', onNavClick);
    }
  });

  it('renders Chat card with breakdown chips, ?chat= URL persistence, and expand to 10 previews', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const dmPreview = {
      id: 'dm:u1:a1',
      kind: 'dm' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:50:00Z',
      href: '/chat/dm/dm%3Au1%3Aa1',
      conversationKey: 'dm:u1:a1',
      label: 'deploy-bot',
      lastMessageAt: '2026-11-23T17:50:00Z',
    };
    const mentionPreview = {
      id: 'topic-1',
      kind: 'mention' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/chat/core/topic-1',
      conversationKey: 'topic-1',
      label: '#incidents',
      lastMessageAt: '2026-11-23T17:45:00Z',
    };
    const threadPreview = {
      id: 'topic-2',
      kind: 'thread' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:40:00Z',
      href: '/chat/core/topic-2',
      conversationKey: 'topic-2',
      label: '#general',
      lastMessageAt: '2026-11-23T17:40:00Z',
    };
    const breakdown = { dms: 2, mentions: 1, unreadConversations: 5 };

    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        const parsed = new URL(url, 'http://localhost');
        const chatParam = parsed.searchParams.get('chat') ?? 'all';
        const previewsParam = Number(parsed.searchParams.get('previews') ?? '3');
        const previews =
          chatParam === 'dms'
            ? [dmPreview]
            : chatParam === 'mentions'
              ? [mentionPreview]
              : previewsParam >= 10
                ? [
                    dmPreview,
                    mentionPreview,
                    threadPreview,
                    { ...threadPreview, id: 'topic-3', conversationKey: 'topic-3', label: '#ops' },
                  ]
                : [dmPreview, mentionPreview, threadPreview];
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse(
                {
                  state: 'disabled',
                  count: 0,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/profile/secrets',
                  previews: [],
                },
                {
                  chat: {
                    state: 'ok',
                    count: 5,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/chat',
                    breakdown,
                    previews,
                  },
                }
              )
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    expect(chatCard).not.toBeNull();
    expect(chatCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('5 unread');
    expect(chatCard?.querySelector('[data-card-view-all]')?.getAttribute('href')).toBe('/chat');

    // Chips reflect breakdown counts.
    const allChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="all"]');
    const dmsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]');
    const mentionsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="mentions"]');
    expect(allChip?.textContent?.trim()).toBe('All (5)');
    expect(dmsChip?.textContent?.trim()).toBe('DMs (2)');
    expect(mentionsChip?.textContent?.trim()).toBe('Mentions (1)');
    expect(allChip?.getAttribute('aria-pressed')).toBe('true');

    // DM and thread links use preview hrefs.
    expect(
      chatCard?.querySelector('[data-chat-row="dm:u1:a1"] [data-chat-link]')?.getAttribute('href')
    ).toBe('/chat/dm/dm%3Au1%3Aa1');
    expect(
      chatCard?.querySelector('[data-chat-row="topic-1"] [data-chat-link]')?.getAttribute('href')
    ).toBe('/chat/core/topic-1');

    // Clicking DMs chip updates ?chat=dms and fetches chat=dms.
    dmsChip!.click();
    await flush();
    await page.updateComplete;

    expect(window.location.search).toBe('?chat=dms');
    expect(dmsChip?.getAttribute('aria-pressed')).toBe('true');
    expect(fetchMock.mock.calls.some((c) => urlOf(c[0]).includes('previews=3&chat=dms'))).toBe(
      true
    );
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(1);
    expect(chatCard?.querySelector('[data-chat-row="dm:u1:a1"]')).not.toBeNull();

    // Clicking All chip clears ?chat= from URL and shows all 3 previews again.
    allChip!.click();
    await flush();
    await page.updateComplete;

    expect(window.location.search).toBe('');
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(3);

    // Expanding requests previews=10.
    const expandBtn = chatCard?.querySelector<HTMLElement>('[data-chat-expand-btn]');
    expect(expandBtn).not.toBeNull();
    expandBtn!.click();
    await flush();
    await page.updateComplete;

    expect(fetchMock.mock.calls.some((c) => urlOf(c[0]).includes('previews=10&chat=all'))).toBe(
      true
    );
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(4);
    expect(chatCard?.querySelector('[data-chat-collapse-btn]')).not.toBeNull();
  });

  it('hydrates ?chat=mentions from URL on mount', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    window.history.replaceState({}, '', '/profile/inbox?chat=mentions');

    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(
          JSON.stringify(
            makeSummaryResponse(
              {
                state: 'disabled',
                count: 0,
                capped: false,
                countsTowardTotal: true,
                href: '/profile/secrets',
                previews: [],
              },
              {
                chat: {
                  state: 'ok',
                  count: 3,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/chat',
                  breakdown: { dms: 1, mentions: 1, unreadConversations: 3 },
                  previews: [
                    {
                      id: 'topic-m',
                      kind: 'mention',
                      projectId: 'p-1',
                      projectName: 'Core',
                      createdAt: '2026-11-23T17:45:00Z',
                      href: '/chat/core/topic-m',
                      conversationKey: 'topic-m',
                      label: '#alerts',
                      lastMessageAt: '2026-11-23T17:45:00Z',
                    },
                  ],
                },
              }
            )
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    expect(fetchMock.mock.calls.some((c) => urlOf(c[0]).includes('previews=3&chat=mentions'))).toBe(
      true
    );
    const mentionsChip = page.shadowRoot?.querySelector('[data-chat-chip="mentions"]');
    expect(mentionsChip?.getAttribute('aria-pressed')).toBe('true');
  });

  it('aborts superseded chat filter fetches and drops out-of-order responses without showing stale rows', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const dmPreview = {
      id: 'dm:u1:a1',
      kind: 'dm' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:50:00Z',
      href: '/chat/dm/dm%3Au1%3Aa1',
      conversationKey: 'dm:u1:a1',
      label: 'deploy-bot',
      lastMessageAt: '2026-11-23T17:50:00Z',
    };
    const mentionPreview = {
      id: 'topic-m',
      kind: 'mention' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/chat/core/topic-m',
      conversationKey: 'topic-m',
      label: '#alerts',
      lastMessageAt: '2026-11-23T17:45:00Z',
    };
    const breakdown = { dms: 1, mentions: 1, unreadConversations: 2 };
    const chatResponse = (previews: (typeof dmPreview | typeof mentionPreview)[]): Response =>
      new Response(
        JSON.stringify(
          makeSummaryResponse(
            {
              state: 'disabled',
              count: 0,
              capped: false,
              countsTowardTotal: true,
              href: '/profile/secrets',
              previews: [],
            },
            {
              chat: {
                state: 'ok',
                count: 2,
                capped: false,
                countsTowardTotal: true,
                href: '/chat',
                breakdown,
                previews,
              },
            }
          )
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      );

    let dmsSignal: AbortSignal | undefined;
    let resolveDms: ((r: Response) => void) | null = null;
    let resolveMentions: ((r: Response) => void) | null = null;

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('chat=dms')) {
        dmsSignal = init?.signal ?? undefined;
        return new Promise<Response>((resolve) => {
          resolveDms = resolve;
        });
      }
      if (url.includes('chat=mentions')) {
        return new Promise<Response>((resolve) => {
          resolveMentions = resolve;
        });
      }
      return Promise.resolve(chatResponse([dmPreview, mentionPreview]));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    const dmsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]');
    const mentionsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="mentions"]');

    // Click DMs -> loading placeholder shown, no stale rows from All.
    dmsChip!.click();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-chat-loading]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();
    expect(dmsSignal).toBeDefined();
    expect(dmsSignal?.aborted).toBe(false);

    // Click Mentions before DMs resolves -> DMs request is aborted, loading shown with no DM rows.
    mentionsChip!.click();
    await page.updateComplete;
    expect(dmsSignal?.aborted).toBe(true);
    expect(chatCard?.querySelector('[data-chat-loading]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();

    // Resolve Mentions first, then resolve the stale DMs response second.
    resolveMentions!(chatResponse([mentionPreview]));
    await flush();
    await page.updateComplete;
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(1);
    expect(chatCard?.querySelector('[data-chat-row="topic-m"]')).not.toBeNull();

    resolveDms!(chatResponse([dmPreview]));
    await flush();
    await page.updateComplete;
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(1);
    expect(chatCard?.querySelector('[data-chat-row="topic-m"]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();
  });

  it('does not render stale DM rows under Mentions while Mentions fetch is pending (guarded by chatOverrideMeta)', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const dmPreview = {
      id: 'dm:u1:a1',
      kind: 'dm' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:50:00Z',
      href: '/chat/dm/dm%3Au1%3Aa1',
      conversationKey: 'dm:u1:a1',
      label: 'deploy-bot',
      lastMessageAt: '2026-11-23T17:50:00Z',
    };
    const mentionPreview = {
      id: 'topic-m',
      kind: 'mention' as const,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/chat/core/topic-m',
      conversationKey: 'topic-m',
      label: '#alerts',
      lastMessageAt: '2026-11-23T17:45:00Z',
    };
    const breakdown = { dms: 1, mentions: 1, unreadConversations: 2 };
    const makeChatCat = (previews: (typeof dmPreview | typeof mentionPreview)[]) => ({
      state: 'ok' as const,
      count: 2,
      capped: false,
      countsTowardTotal: true,
      href: '/chat',
      breakdown,
      previews,
    });
    const chatResponse = (previews: (typeof dmPreview | typeof mentionPreview)[]): Response =>
      new Response(
        JSON.stringify(
          makeSummaryResponse(
            {
              state: 'disabled',
              count: 0,
              capped: false,
              countsTowardTotal: true,
              href: '/profile/secrets',
              previews: [],
            },
            { chat: makeChatCat(previews) }
          )
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      );

    let resolveMentions: ((r: Response) => void) | null = null;
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.includes('chat=dms')) {
        return Promise.resolve(chatResponse([dmPreview]));
      }
      if (url.includes('chat=mentions')) {
        return new Promise<Response>((resolve) => {
          resolveMentions = resolve;
        });
      }
      return Promise.resolve(chatResponse([dmPreview, mentionPreview]));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    const dmsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]');
    const mentionsChip = chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="mentions"]');

    // Step 1: Switch to DMs and let it resolve.
    dmsChip!.click();
    await flush();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-chat-row="dm:u1:a1"]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).not.toBeNull();

    // Step 2: Switch to Mentions while the Mentions fetch stays pending.
    mentionsChip!.click();
    await page.updateComplete;
    expect(mentionsChip?.getAttribute('aria-pressed')).toBe('true');
    expect(chatCard?.querySelector('[data-chat-loading]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();
    expect(chatCard?.querySelector('[data-chat-row="dm:u1:a1"]')).toBeNull();

    // Step 3: Exercise the chatOverrideMeta guard directly — even if chatOverride
    // holds the prior DMs payload tagged with filter:'dms', renderChatCard must
    // reject it while chatFilter is 'mentions' and render no DM rows.
    const internal = page as unknown as {
      chatOverride: ReturnType<typeof makeChatCat> | null;
      chatOverrideMeta: { filter: 'all' | 'dms' | 'mentions'; previews: number } | null;
    };
    internal.chatOverride = makeChatCat([dmPreview]);
    internal.chatOverrideMeta = { filter: 'dms', previews: 3 };
    page.requestUpdate();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-chat-loading]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();
    expect(chatCard?.querySelector('[data-chat-row="dm:u1:a1"]')).toBeNull();

    // Step 4: Resolve Mentions -> mention row renders, still no DM rows.
    resolveMentions!(chatResponse([mentionPreview]));
    await flush();
    await page.updateComplete;
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(1);
    expect(chatCard?.querySelector('[data-chat-row="topic-m"]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-kind="dm"]')).toBeNull();
  });

  it('aborts an in-flight chat fetch on disconnect and ignores late responses', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    let dmsSignal: AbortSignal | undefined;
    let resolveDms: ((r: Response) => void) | null = null;
    const baseSummary = makeSummaryResponse(
      {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        chat: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/chat',
          breakdown: { dms: 1, mentions: 0, unreadConversations: 1 },
          previews: [
            {
              id: 'dm:u1:a1',
              kind: 'dm',
              projectId: 'p-1',
              projectName: 'Core',
              createdAt: '2026-11-23T17:50:00Z',
              href: '/chat/dm/dm%3Au1%3Aa1',
              conversationKey: 'dm:u1:a1',
              label: 'deploy-bot',
              lastMessageAt: '2026-11-23T17:50:00Z',
            },
          ],
        },
      }
    );

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('chat=dms')) {
        dmsSignal = init?.signal ?? undefined;
        return new Promise<Response>((resolve) => {
          resolveDms = resolve;
        });
      }
      return Promise.resolve(
        new Response(JSON.stringify(baseSummary), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    page.shadowRoot?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]')?.click();
    await page.updateComplete;
    expect(dmsSignal?.aborted).toBe(false);

    // Disconnect mid-fetch -> signal is aborted; late resolution does not trigger state updates.
    const requestUpdateSpy = vi.spyOn(page, 'requestUpdate');
    page.remove();
    expect(dmsSignal?.aborted).toBe(true);

    resolveDms!(
      new Response(JSON.stringify(baseSummary), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await flush();
    expect(requestUpdateSpy).not.toHaveBeenCalled();
  });

  it('refetches filtered chat view when store summary updates, keeps chips on error, and always allows collapsing', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const makePreview = (
      id: string,
      kind: 'dm' | 'mention' | 'thread',
      label: string
    ): InboxSummaryResponse['categories']['chat']['previews'][number] => ({
      id,
      kind,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:50:00Z',
      href: kind === 'dm' ? `/chat/dm/${id}` : `/chat/core/${id}`,
      conversationKey: id,
      label,
      lastMessageAt: '2026-11-23T17:50:00Z',
    });

    let unreadCount = 5;
    let failMentions = false;
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.includes('chat=mentions') && failMentions) {
        return Promise.resolve(new Response('boom', { status: 500 }));
      }
      const previews =
        unreadCount <= 2
          ? [makePreview('dm:1', 'dm', 'bot-1'), makePreview('t:1', 'thread', '#general')]
          : [
              makePreview('dm:1', 'dm', 'bot-1'),
              makePreview('t:1', 'mention', '#alerts'),
              makePreview('t:2', 'thread', '#general'),
            ];
      return Promise.resolve(
        new Response(
          JSON.stringify(
            makeSummaryResponse(
              {
                state: 'disabled',
                count: 0,
                capped: false,
                countsTowardTotal: true,
                href: '/profile/secrets',
                previews: [],
              },
              {
                chat: {
                  state: 'ok',
                  count: unreadCount,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/chat',
                  breakdown: { dms: 1, mentions: 1, unreadConversations: unreadCount },
                  previews,
                },
              }
            )
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');

    // Item 3: a failed filtered fetch keeps chips visible so the user can switch back to All.
    failMentions = true;
    chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="mentions"]')?.click();
    await flush();
    await page.updateComplete;

    expect(chatCard?.querySelector('[data-card-error]')).not.toBeNull();
    expect(chatCard?.querySelector('[data-chat-chips]')).not.toBeNull();

    // Switching back to All recovers cleanly from the error state.
    chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="all"]')?.click();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-card-error]')).toBeNull();
    expect(chatCard?.querySelectorAll('[data-chat-row]').length).toBe(3);

    // Item 1: store-change refetch while filtered.
    chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]')?.click();
    await flush();
    await page.updateComplete;
    const dmsCallsBefore = fetchMock.mock.calls.filter((c) =>
      urlOf(c[0]).includes('chat=dms')
    ).length;
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;
    const dmsCallsAfter = fetchMock.mock.calls.filter((c) =>
      urlOf(c[0]).includes('chat=dms')
    ).length;
    expect(dmsCallsAfter).toBeGreaterThan(dmsCallsBefore);

    // Item 4: expand while count > 3, then drop count to 2 -> "Show fewer" stays visible.
    chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="all"]')?.click();
    await page.updateComplete;
    chatCard?.querySelector<HTMLElement>('[data-chat-expand-btn]')?.click();
    await flush();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-chat-collapse-btn]')).not.toBeNull();

    unreadCount = 2;
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;
    const collapseBtn = chatCard?.querySelector<HTMLElement>('[data-chat-collapse-btn]');
    expect(collapseBtn).not.toBeNull();

    collapseBtn!.click();
    await page.updateComplete;
    expect(chatCard?.querySelector('[data-chat-collapse-btn]')).toBeNull();
  });

  it('guards inline decision answers against double-clicks and shows error toasts for 0, 403, and 409', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const decision = {
      id: 'msg-err',
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/chat/space/p-1/thread/t-1#msg-msg-err',
      promptMessageId: 'msg-err',
      conversationLabel: '#release',
      askerAgentName: 'release-bot',
      question: 'Proceed?',
      decisionCount: 1,
      answerMode: 'single',
      canAnswer: true,
      inline: {
        decisionRowId: 'row-err',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
        allowsOther: false,
      },
    };
    const summary = makeSummaryResponse(
      {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        decisions: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/profile/decisions?tab=pending',
          previews: [decision],
        },
      }
    );

    let answerMode: 'defer' | 'network' | '403' | 'already_answered' | 'replaced' = 'defer';
    let resolveDeferredAnswer: ((r: Response) => void) | null = null;
    let answerPosts = 0;

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(JSON.stringify(summary), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.endsWith('/api/v1/decisions/row-err/answer') && init?.method === 'POST') {
        answerPosts += 1;
        if (answerMode === 'defer') {
          return new Promise<Response>((resolve) => {
            resolveDeferredAnswer = resolve;
          });
        }
        if (answerMode === 'network') {
          return Promise.reject(new TypeError('offline'));
        }
        if (answerMode === '403') {
          return Promise.resolve(
            new Response(
              JSON.stringify({ error: { code: 'message_denied', message: 'Not allowed' } }),
              { status: 403, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: { code: answerMode, message: `Decision was ${answerMode}` },
            }),
            { status: 409, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const yesBtn = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-decision-row="msg-err"] [data-decision-choice-btn="yes"]'
    );
    const noBtn = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-decision-row="msg-err"] [data-decision-choice-btn="no"]'
    );

    // Double-click guard: clicking twice (and another choice) while in flight sends only 1 POST.
    yesBtn!.click();
    yesBtn!.click();
    noBtn!.click();
    await page.updateComplete;
    expect(answerPosts).toBe(1);

    resolveDeferredAnswer!(
      new Response(
        JSON.stringify({ error: { code: 'already_answered', message: 'Already done' } }),
        {
          status: 409,
          headers: { 'Content-Type': 'application/json' },
        }
      )
    );
    await flush();
    expect(showToast).toHaveBeenLastCalledWith('Already done', 'neutral');

    // Status 0 (network error) -> danger toast.
    answerMode = 'network';
    yesBtn!.click();
    await flush();
    expect(showToast).toHaveBeenLastCalledWith(expect.stringContaining('Network error'), 'danger');

    // 403 -> danger toast.
    answerMode = '403';
    yesBtn!.click();
    await flush();
    expect(showToast).toHaveBeenLastCalledWith('Not allowed', 'danger');

    // 409 replaced -> neutral toast and store refresh.
    const summaryCallsBefore = fetchMock.mock.calls.filter((c) =>
      urlOf(c[0]).includes('/api/v1/users/me/inbox/summary')
    ).length;
    answerMode = 'replaced';
    yesBtn!.click();
    await flush();
    expect(showToast).toHaveBeenLastCalledWith('Decision was replaced', 'neutral');
    const summaryCallsAfter = fetchMock.mock.calls.filter((c) =>
      urlOf(c[0]).includes('/api/v1/users/me/inbox/summary')
    ).length;
    expect(summaryCallsAfter).toBeGreaterThan(summaryCallsBefore);
  });

  it('tracks inline decision loading spinners independently across concurrent rows', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const makeDecision = (
      id: string,
      rowId: string,
      question: string
    ): InboxSummaryResponse['categories']['decisions']['previews'][number] => ({
      id,
      projectId: 'p-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:00:00Z',
      href: `/chat/space/p-1/thread/t-1#msg-${id}`,
      promptMessageId: id,
      conversationLabel: '#release',
      askerAgentName: 'release-bot',
      question,
      decisionCount: 1,
      answerMode: 'single',
      canAnswer: true,
      inline: {
        decisionRowId: rowId,
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
        allowsOther: false,
      },
    });

    const d1 = makeDecision('msg-1', 'row-1', 'Ship A?');
    const d2 = makeDecision('msg-2', 'row-2', 'Ship B?');
    let remaining = [d1, d2];

    let resolveRow1: ((r: Response) => void) | null = null;
    let resolveRow2: ((r: Response) => void) | null = null;

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse(
                {
                  state: 'disabled',
                  count: 0,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/profile/secrets',
                  previews: [],
                },
                {
                  decisions: {
                    state: 'ok',
                    count: remaining.length,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/profile/decisions?tab=pending',
                    previews: remaining,
                  },
                }
              )
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.endsWith('/api/v1/decisions/row-1/answer') && init?.method === 'POST') {
        return new Promise<Response>((resolve) => {
          resolveRow1 = resolve;
        });
      }
      if (url.endsWith('/api/v1/decisions/row-2/answer') && init?.method === 'POST') {
        return new Promise<Response>((resolve) => {
          resolveRow2 = resolve;
        });
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const btn1 = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-decision-row="msg-1"] [data-decision-choice-btn="yes"]'
    );
    const btn2 = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-decision-row="msg-2"] [data-decision-choice-btn="no"]'
    );
    expect(btn1).not.toBeNull();
    expect(btn2).not.toBeNull();

    // Answer both rows concurrently -> both choice buttons show loading.
    btn1!.click();
    btn2!.click();
    await page.updateComplete;

    expect(btn1!.hasAttribute('loading')).toBe(true);
    expect(btn2!.hasAttribute('loading')).toBe(true);

    // Resolve row 1 -> row 1 completes, while row 2 keeps its loading spinner.
    remaining = [d2];
    resolveRow1!(
      new Response(
        JSON.stringify({
          decision: { rowId: 'row-1', status: 'decided' },
          responseMessageId: 'r1',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await flush();
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('[data-decision-row="msg-1"]')).toBeNull();
    const btn2StillLoading = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-decision-row="msg-2"] [data-decision-choice-btn="no"]'
    );
    expect(btn2StillLoading?.hasAttribute('loading')).toBe(true);

    // Resolve row 2 -> clears cleanly.
    remaining = [];
    resolveRow2!(
      new Response(
        JSON.stringify({
          decision: { rowId: 'row-2', status: 'decided' },
          responseMessageId: 'r2',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await flush();
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('[data-decision-row="msg-2"]')).toBeNull();
  });

  it('ignores AbortError rejections without relying on global DOMException', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const baseSummary = makeSummaryResponse(
      {
        state: 'disabled',
        count: 0,
        capped: false,
        countsTowardTotal: true,
        href: '/profile/secrets',
        previews: [],
      },
      {
        chat: {
          state: 'ok',
          count: 1,
          capped: false,
          countsTowardTotal: true,
          href: '/chat',
          breakdown: { dms: 1, mentions: 0, unreadConversations: 1 },
          previews: [
            {
              id: 'dm:u1:a1',
              kind: 'dm',
              projectId: 'p-1',
              projectName: 'Core',
              createdAt: '2026-11-23T17:50:00Z',
              href: '/chat/dm/dm%3Au1%3Aa1',
              conversationKey: 'dm:u1:a1',
              label: 'deploy-bot',
              lastMessageAt: '2026-11-23T17:50:00Z',
            },
          ],
        },
      }
    );

    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.includes('chat=dms')) {
        const abortErr = new Error('aborted');
        abortErr.name = 'AbortError';
        return Promise.reject(abortErr);
      }
      return Promise.resolve(
        new Response(JSON.stringify(baseSummary), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const chatCard = page.shadowRoot?.querySelector('[data-inbox-card="chat"]');
    chatCard?.querySelector<HTMLButtonElement>('[data-chat-chip="dms"]')?.click();
    await flush();
    await page.updateComplete;

    expect(chatCard?.querySelector('[data-card-error]')).toBeNull();
  });

  it('renders Agents waiting card with agentName/agentSlug, question, since, href, no inline action, and updates on waiting SSE hints', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const waitingItem = {
      id: 'agent-42',
      projectId: 'proj-web',
      projectName: 'Web',
      createdAt: '2026-11-23T17:00:00Z',
      href: '/agents/agent-42',
      agentId: 'agent-42',
      agentName: 'Frontend Dev',
      agentSlug: 'fe-dev',
      question: 'Need staging credentials before running migration?',
      since: new Date(Date.now() - 14 * 60_000).toISOString(),
    };

    let currentWaiting = [waitingItem];
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(
          JSON.stringify(
            makeSummaryResponse(
              {
                state: 'disabled',
                count: 0,
                capped: false,
                countsTowardTotal: true,
                href: '/profile/secrets',
                previews: [],
              },
              {
                waiting: {
                  state: 'ok',
                  count: currentWaiting.length,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/agents',
                  agentIds: currentWaiting.map((w) => w.agentId),
                  previews: currentWaiting,
                },
              }
            )
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const waitingCard = page.shadowRoot?.querySelector('[data-inbox-card="waiting"]');
    expect(waitingCard).not.toBeNull();
    expect(waitingCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('1');
    expect(waitingCard?.querySelector('[data-card-view-all]')?.getAttribute('href')).toBe(
      '/agents'
    );

    const row = waitingCard?.querySelector('[data-waiting-row="agent-42"]');
    expect(row).not.toBeNull();
    const link = row?.querySelector('[data-waiting-link]');
    expect(link?.getAttribute('href')).toBe('/agents/agent-42');
    expect(link?.textContent?.trim()).toBe('Frontend Dev');
    expect(row?.querySelector('[data-waiting-slug]')?.textContent?.trim()).toBe('fe-dev');
    expect(row?.querySelector('[data-waiting-question]')?.textContent?.trim()).toBe(
      'Need staging credentials before running migration?'
    );
    expect(row?.querySelector('[data-waiting-since]')?.textContent?.trim()).toBeTruthy();
    // No inline action buttons on the waiting card.
    expect(row?.querySelector('sl-button')).toBeNull();

    // Resolving the waiting agent via SSE hint clears the row and shows compact empty state.
    currentWaiting = [];
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', {
        detail: { category: 'waiting', agentId: 'agent-42', activity: 'thinking' },
      })
    );
    await new Promise((r) => setTimeout(r, 850));
    await page.updateComplete;

    expect(waitingCard?.querySelector('[data-waiting-row="agent-42"]')).toBeNull();
    expect(waitingCard?.querySelector('[data-card-empty]')?.textContent?.trim()).toBe(
      'Nothing waiting'
    );
  });

  it('renders Reviews card with separate to_address and requested groups, hides when disabled, and dismisses requested rows optimistically with bell sync', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const toAddressItem = {
      id: 'art-1',
      kind: 'to_address' as const,
      projectId: 'proj-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:30:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'API Spec v2',
      scopeRef: 'proj-1',
    };
    const requestedItem = {
      id: 'notif-99',
      kind: 'requested' as const,
      projectId: 'proj-2',
      projectName: 'Web',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/projects/proj-2/artifacts/art-2',
      artifactId: 'art-2',
      title: 'Launch Checklist',
      scopeRef: 'proj-2',
      requestedBy: 'alice',
      notificationId: 'notif-99',
    };

    let dismissed = false;
    let resolveAck: ((res: Response) => void) | null = null;

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        const previews = dismissed ? [toAddressItem] : [requestedItem, toAddressItem];
        const breakdown = dismissed
          ? { toAddress: 1, requested: 0 }
          : { toAddress: 1, requested: 1 };
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse(
                {
                  state: 'disabled',
                  count: 0,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/profile/secrets',
                  previews: [],
                },
                {
                  reviews: {
                    state: 'ok',
                    count: previews.length,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/artifacts?review_pending=1&owner=me',
                    breakdown,
                    previews,
                  },
                }
              )
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.endsWith('/api/v1/notifications/notif-99/ack') && init?.method === 'POST') {
        return new Promise<Response>((resolve) => {
          resolveAck = resolve;
        });
      }
      if (url.includes('/api/v1/notifications?acknowledged=false')) {
        const list = dismissed
          ? []
          : [
              {
                id: 'notif-99',
                subscriptionId: 'sub-1',
                agentId: '00000000-0000-0000-0000-000000000000',
                projectId: 'proj-2',
                subscriberType: 'user',
                subscriberId: 'user-1',
                status: 'ARTIFACT_REVIEW_REQUESTED',
                message: 'alice requested your review on Launch Checklist',
                acknowledged: false,
                createdAt: '2026-11-23T17:45:00Z',
              },
            ];
        return Promise.resolve(
          new Response(JSON.stringify(list), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const tray = document.createElement('scion-notification-tray') as ScionNotificationTray;
    tray.user = { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(tray);

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;
    await tray.updateComplete;

    const reviewsCard = page.shadowRoot?.querySelector('[data-inbox-card="reviews"]');
    expect(reviewsCard).not.toBeNull();
    expect(reviewsCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('2');
    expect(reviewsCard?.querySelector('[data-card-view-all]')?.getAttribute('href')).toBe(
      '/artifacts?review_pending=1&owner=me'
    );

    // Two visually separate groups with breakdown counts.
    const toAddressGroup = reviewsCard?.querySelector('[data-reviews-group="to_address"]');
    const requestedGroup = reviewsCard?.querySelector('[data-reviews-group="requested"]');
    expect(toAddressGroup).not.toBeNull();
    expect(requestedGroup).not.toBeNull();
    expect(
      toAddressGroup?.querySelector('[data-reviews-group-count="to_address"]')?.textContent?.trim()
    ).toBe('1');
    expect(
      requestedGroup?.querySelector('[data-reviews-group-count="requested"]')?.textContent?.trim()
    ).toBe('1');

    // to_address row shows title, scopeRef, href, and NO dismiss button.
    const toAddressRow = toAddressGroup?.querySelector('[data-review-row="art-1"]');
    expect(toAddressRow?.querySelector('[data-review-link]')?.getAttribute('href')).toBe(
      '/projects/proj-1/artifacts/art-1'
    );
    expect(toAddressRow?.querySelector('[data-review-link]')?.textContent?.trim()).toBe(
      'API Spec v2'
    );
    expect(toAddressRow?.querySelector('[data-review-scope]')?.textContent?.trim()).toBe('proj-1');
    expect(toAddressRow?.querySelector('[data-review-dismiss-btn]')).toBeNull();

    // requested row shows title, scopeRef, requestedBy, href, and Dismiss button with row context.
    const requestedRow = requestedGroup?.querySelector('[data-review-row="notif-99"]');
    expect(requestedRow?.querySelector('[data-review-link]')?.getAttribute('href')).toBe(
      '/projects/proj-2/artifacts/art-2'
    );
    expect(requestedRow?.querySelector('[data-review-scope]')?.textContent?.trim()).toBe('proj-2');
    expect(requestedRow?.querySelector('[data-review-requested-by]')?.textContent?.trim()).toBe(
      'requested by alice'
    );
    const dismissBtn = requestedRow?.querySelector<HTMLElement>(
      '[data-review-dismiss-btn="notif-99"]'
    );
    expect(dismissBtn).not.toBeNull();
    expect(dismissBtn?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Dismiss review request for Launch Checklist'
    );

    const ackedEvents: string[] = [];
    const onAcked = (e: Event): void => {
      const id = (e as CustomEvent<{ id?: string }>).detail?.id;
      if (id) ackedEvents.push(id);
    };
    window.addEventListener(NOTIFICATION_ACKED_EVENT, onAcked);

    try {
      // Click Dismiss -> row is removed optimistically BEFORE POST resolves.
      dismissBtn!.click();
      await page.updateComplete;

      expect(reviewsCard?.querySelector('[data-review-row="notif-99"]')).toBeNull();
      expect(reviewsCard?.querySelector('[data-card-count]')?.textContent?.trim()).toBe('1');

      // POST /api/v1/notifications/notif-99/ack sent with Content-Type: application/json.
      const ackCall = fetchMock.mock.calls.find((c) =>
        urlOf(c[0]).endsWith('/api/v1/notifications/notif-99/ack')
      );
      expect(ackCall).toBeTruthy();
      const ackInit = ackCall?.[1] as RequestInit | undefined;
      expect(ackInit?.method).toBe('POST');
      expect((ackInit?.headers as Record<string, string>)?.['Content-Type']).toBe(
        'application/json'
      );

      // Resolve ack -> triggers a forced summary refresh (with no SSE hint) and bell tray drops the notification.
      const summaryCallsBeforeAck = fetchMock.mock.calls.filter((c) =>
        urlOf(c[0]).includes('/api/v1/users/me/inbox/summary')
      ).length;
      dismissed = true;
      resolveAck!(
        new Response(JSON.stringify({ status: 'ok' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      await flush();
      await page.updateComplete;
      await tray.updateComplete;

      const summaryCallsAfterAck = fetchMock.mock.calls.filter((c) =>
        urlOf(c[0]).includes('/api/v1/users/me/inbox/summary')
      ).length;
      expect(summaryCallsAfterAck).toBeGreaterThan(summaryCallsBeforeAck);
      expect(ackedEvents).toEqual(['notif-99']);
      expect(tray.shadowRoot?.querySelector('.badge')).toBeNull();
      expect(showToast).toHaveBeenLastCalledWith('Dismissed review request', 'neutral');
    } finally {
      window.removeEventListener(NOTIFICATION_ACKED_EVENT, onAcked);
    }
  });

  it('guards review Dismiss against double-submit, rolls back on error with toast, and tracks concurrent rows independently', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const rev1 = {
      id: 'notif-1',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'Doc One',
      scopeRef: 'proj-1',
      requestedBy: 'alice',
      notificationId: 'notif-1',
    };
    const rev2 = {
      id: 'notif-2',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:40:00Z',
      href: '/projects/proj-1/artifacts/art-2',
      artifactId: 'art-2',
      title: 'Doc Two',
      scopeRef: 'proj-1',
      requestedBy: 'bob',
      notificationId: 'notif-2',
    };

    let ack1Calls = 0;
    let ack2Calls = 0;
    let resolveAck1: ((res: Response) => void) | null = null;
    let resolveAck2: ((res: Response) => void) | null = null;
    let serverItems = [rev1, rev2];

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse(
                {
                  state: 'disabled',
                  count: 0,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/profile/secrets',
                  previews: [],
                },
                {
                  reviews: {
                    state: 'ok',
                    count: serverItems.length,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/artifacts?review_pending=1&owner=me',
                    breakdown: { toAddress: 0, requested: serverItems.length },
                    previews: serverItems,
                  },
                }
              )
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.endsWith('/api/v1/notifications/notif-1/ack') && init?.method === 'POST') {
        ack1Calls += 1;
        return new Promise<Response>((resolve) => {
          resolveAck1 = resolve;
        });
      }
      if (url.endsWith('/api/v1/notifications/notif-2/ack') && init?.method === 'POST') {
        ack2Calls += 1;
        return new Promise<Response>((resolve) => {
          resolveAck2 = resolve;
        });
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const btn1 = page.shadowRoot?.querySelector<HTMLElement>('[data-review-dismiss-btn="notif-1"]');
    const btn2 = page.shadowRoot?.querySelector<HTMLElement>('[data-review-dismiss-btn="notif-2"]');
    expect(btn1).not.toBeNull();
    expect(btn2).not.toBeNull();

    // Double-submit guard on row 1: two synchronous clicks send only 1 POST.
    // Concurrent row 2 dismiss is NOT blocked by row 1's busy state.
    btn1!.click();
    btn1!.click();
    btn2!.click();
    await page.updateComplete;

    expect(ack1Calls).toBe(1);
    expect(ack2Calls).toBe(1);
    expect(page.shadowRoot?.querySelector('[data-review-row="notif-1"]')).toBeNull();
    expect(page.shadowRoot?.querySelector('[data-review-row="notif-2"]')).toBeNull();

    // Row 1 fails with 500 -> rolls back row 1 with danger toast and requests a non-forced refresh.
    const refreshSpy = vi.spyOn(inboxSummaryStore, 'requestRefresh');
    try {
      resolveAck1!(
        new Response(JSON.stringify({ error: { message: 'Ack failed' } }), {
          status: 500,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      await flush();
      await page.updateComplete;

      expect(showToast).toHaveBeenCalledWith('Ack failed', 'danger');
      expect(refreshSpy).toHaveBeenCalledWith();
      expect(page.shadowRoot?.querySelector('[data-review-row="notif-1"]')).not.toBeNull();
      expect(page.shadowRoot?.querySelector('[data-review-row="notif-2"]')).toBeNull();
    } finally {
      refreshSpy.mockRestore();
    }

    serverItems = [rev1];
    resolveAck2!(
      new Response(JSON.stringify({ status: 'ok' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await flush();
    await page.updateComplete;

    expect(page.shadowRoot?.querySelector('[data-review-row="notif-1"]')).not.toBeNull();
    expect(page.shadowRoot?.querySelector('[data-review-row="notif-2"]')).toBeNull();
    expect(
      page.shadowRoot
        ?.querySelector('[data-inbox-card="reviews"] [data-card-count]')
        ?.textContent?.trim()
    ).toBe('1');
  });

  it('aborts in-flight review Dismiss fetch on disconnect, rolls back store state, and skips toast', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const rev = {
      id: 'notif-abort',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'Abort Doc',
      scopeRef: 'proj-1',
      requestedBy: 'alice',
      notificationId: 'notif-abort',
    };

    let dismissSignal: AbortSignal | undefined;
    let rejectAck: ((err: unknown) => void) | null = null;

    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      if (url.includes('/api/v1/users/me/inbox/summary')) {
        return Promise.resolve(
          new Response(
            JSON.stringify(
              makeSummaryResponse(
                {
                  state: 'disabled',
                  count: 0,
                  capped: false,
                  countsTowardTotal: true,
                  href: '/profile/secrets',
                  previews: [],
                },
                {
                  reviews: {
                    state: 'ok',
                    count: 1,
                    capped: false,
                    countsTowardTotal: true,
                    href: '/artifacts?review_pending=1&owner=me',
                    breakdown: { toAddress: 0, requested: 1 },
                    previews: [rev],
                  },
                }
              )
            ),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.endsWith('/api/v1/notifications/notif-abort/ack') && init?.method === 'POST') {
        dismissSignal = init?.signal ?? undefined;
        return new Promise<Response>((_resolve, reject) => {
          rejectAck = reject;
        });
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const dismissBtn = page.shadowRoot?.querySelector<HTMLElement>(
      '[data-review-dismiss-btn="notif-abort"]'
    );
    expect(dismissBtn).not.toBeNull();
    dismissBtn!.click();
    await page.updateComplete;

    expect(dismissSignal).toBeDefined();
    expect(dismissSignal?.aborted).toBe(false);
    expect(inboxSummaryStore.summary?.categories.reviews.count).toBe(0);

    // Disconnect mid-flight -> aborts signal; settling with AbortError rolls back store, requests a non-forced refresh, and skips toast.
    const refreshSpy = vi.spyOn(inboxSummaryStore, 'requestRefresh');
    try {
      page.remove();
      expect(dismissSignal?.aborted).toBe(true);

      const abortErr = new Error('aborted');
      abortErr.name = 'AbortError';
      rejectAck!(abortErr);
      await flush();

      expect(inboxSummaryStore.summary?.categories.reviews.count).toBe(1);
      expect(refreshSpy).toHaveBeenCalledWith();
      expect(showToast).not.toHaveBeenCalled();
    } finally {
      refreshSpy.mockRestore();
    }
  });

  it('shows overflow hint in Reviews group when breakdown count exceeds previews and hides Reviews card when disabled', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const requestedOnly = {
      id: 'notif-only',
      kind: 'requested' as const,
      projectId: 'proj-1',
      projectName: 'Core',
      createdAt: '2026-11-23T17:45:00Z',
      href: '/projects/proj-1/artifacts/art-1',
      artifactId: 'art-1',
      title: 'Review Me',
      scopeRef: 'proj-1',
      requestedBy: 'alice',
      notificationId: 'notif-only',
    };

    let reviewsDisabled = false;
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(
          JSON.stringify(
            makeSummaryResponse(
              {
                state: 'ok',
                count: 0,
                capped: false,
                countsTowardTotal: true,
                href: '/profile/secrets',
                previews: [],
              },
              {
                reviews: reviewsDisabled
                  ? {
                      state: 'disabled',
                      count: 0,
                      capped: false,
                      countsTowardTotal: true,
                      href: '/artifacts?review_pending=1&owner=me',
                      previews: [],
                    }
                  : {
                      state: 'ok',
                      count: 3,
                      capped: false,
                      countsTowardTotal: true,
                      href: '/artifacts?review_pending=1&owner=me',
                      breakdown: { toAddress: 2, requested: 1 },
                      previews: [requestedOnly],
                    },
              }
            )
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );

    const page = document.createElement('scion-page-profile-inbox') as ScionPageProfileInbox;
    page.pageData = {
      path: '/profile/inbox',
      title: 'Inbox',
      user: { id: 'user-1', email: 'u@example.com', name: 'User', role: 'member' },
    };
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const reviewsCard = page.shadowRoot?.querySelector('[data-inbox-card="reviews"]');
    expect(reviewsCard).not.toBeNull();
    expect(
      reviewsCard?.querySelector('[data-reviews-group-count="to_address"]')?.textContent?.trim()
    ).toBe('2');
    expect(
      reviewsCard?.querySelector('[data-reviews-group-overflow="to_address"]')?.textContent?.trim()
    ).toBe('2 more · View all');
    expect(
      reviewsCard?.querySelector('[data-reviews-group="requested"] [data-review-row="notif-only"]')
    ).not.toBeNull();

    // When hub.artifacts is off, reviews is state:"disabled" -> card is hidden.
    reviewsDisabled = true;
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await page.updateComplete;
    expect(page.shadowRoot?.querySelector('[data-inbox-card="reviews"]')).toBeNull();
  });
});
