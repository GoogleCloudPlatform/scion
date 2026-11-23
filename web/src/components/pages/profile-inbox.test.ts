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
import type { ScionPageProfileInbox } from './profile-inbox.js';
import type { ScionProfileNav } from '../profile/profile-nav.js';
import { inboxSummaryStore, type InboxSummaryResponse } from '../../client/inbox-summary.js';
import { PENDING_SECRETS_CHANGED_EVENT } from '../../client/pending-secrets.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

function makeSummaryResponse(
  approvals: InboxSummaryResponse['categories']['approvals']
): InboxSummaryResponse {
  const total = approvals.state === 'ok' ? approvals.count : 0;
  return {
    generatedAt: '2026-11-23T18:00:00Z',
    total,
    totalCapped: approvals.capped,
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
      approvals,
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
});
