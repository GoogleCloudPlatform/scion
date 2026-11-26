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
 * The main sidebar shows Inbox in the Overview section after Dashboard only
 * while `web.inbox_dashboard` is on, with a live badge from InboxSummaryStore
 * (`N` / `N+`, hidden at 0). Design §6.1 item 3, §9 AC9 & AC13.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../client/inbox-summary-loader.js', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../client/inbox-summary-loader.js')>();
  return {
    ...mod,
    ensureInboxSummaryStarted: vi.fn(mod.ensureInboxSummaryStarted),
  };
});

import type { ScionNav } from './nav.js';
import { ensureInboxSummaryStarted } from '../../client/inbox-summary-loader.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  inboxSummaryStore,
  type InboxSummaryResponse,
} from '../../client/inbox-summary.js';
import {
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

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

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 15));
}

function overviewLinks(el: ScionNav): string[] {
  const sections = Array.from(el.shadowRoot!.querySelectorAll('.nav-section'));
  const overview = sections.find(
    (s) => s.querySelector('.nav-section-title')?.textContent === 'Overview'
  )!;
  return Array.from(overview.querySelectorAll('a.nav-link')).map((a) => a.getAttribute('href')!);
}

describe('sidebar Inbox item', () => {
  let summaryResponse: InboxSummaryResponse = makeSummary(0);
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeAll(async () => {
    await import('./nav.js');
  }, 30_000);

  beforeEach(() => {
    summaryResponse = makeSummary(0);
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
      return Promise.resolve(new Response('{}', { status: 404 }));
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    inboxSummaryStore.resetForTests();
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('is absent and never loads or fetches inbox summary when web.inbox_dashboard is off or unset', async () => {
    const elDefault = document.createElement('scion-nav') as ScionNav;
    elDefault.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(elDefault);
    await flush();
    await elDefault.updateComplete;
    expect(overviewLinks(elDefault)).toEqual(['/']);
    expect(ensureInboxSummaryStarted).not.toHaveBeenCalled();
    elDefault.remove();

    setFeatureFlag(INBOX_DASHBOARD_FLAG, false);
    const elOff = document.createElement('scion-nav') as ScionNav;
    elOff.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(elOff);
    await flush();
    await elOff.updateComplete;
    expect(overviewLinks(elOff)).toEqual(['/']);
    expect(ensureInboxSummaryStarted).not.toHaveBeenCalled();
    const inboxFetches = fetchMock.mock.calls.filter(([input]) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      return url.includes('/inbox/summary');
    });
    expect(inboxFetches).toHaveLength(0);
  });

  it('appears in Overview right after Dashboard with inbox icon and active state when flag is on', async () => {
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const el = document.createElement('scion-nav') as ScionNav;
    el.currentPath = '/profile/inbox';
    el.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(el);
    await flush();
    await el.updateComplete;

    const links = overviewLinks(el);
    expect(links).toEqual(['/', '/profile/inbox']);

    const link = el.shadowRoot!.querySelector('a[href="/profile/inbox"]')!;
    expect(link.classList.contains('active')).toBe(true);
    expect(link.querySelector('sl-icon')!.getAttribute('name')).toBe('inbox');
    expect(link.querySelector('.nav-link-text')!.textContent).toBe('Inbox');
    // Badge is hidden when total is 0.
    expect(link.querySelector('[data-nav-badge]')).toBeNull();
  });

  it('renders N and N+ badge from InboxSummaryStore and re-renders on store change event', async () => {
    summaryResponse = makeSummary(7, false);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const el = document.createElement('scion-nav') as ScionNav;
    el.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(el);
    await flush();
    await el.updateComplete;

    const badge = el.shadowRoot!.querySelector('a[href="/profile/inbox"] [data-nav-badge]');
    expect(badge?.textContent?.trim()).toBe('7');

    // Update store response to capped count and trigger refresh.
    summaryResponse = makeSummary(25, true);
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await el.updateComplete;

    const cappedBadge = el.shadowRoot!.querySelector('a[href="/profile/inbox"] [data-nav-badge]');
    expect(cappedBadge?.textContent?.trim()).toBe('25+');

    // Re-renders when store change event fires with 0 total (badge disappears).
    summaryResponse = makeSummary(0, false);
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('a[href="/profile/inbox"] [data-nav-badge]')).toBeNull();
  });

  it('re-renders when web.inbox_dashboard toggles on at runtime and on summary change events', async () => {
    summaryResponse = makeSummary(3, false);
    const el = document.createElement('scion-nav') as ScionNav;
    el.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(el);
    await el.updateComplete;
    expect(overviewLinks(el)).toEqual(['/']);

    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    await flush();
    await el.updateComplete;

    expect(overviewLinks(el)).toEqual(['/', '/profile/inbox']);
    expect(
      el.shadowRoot!.querySelector('a[href="/profile/inbox"] [data-nav-badge]')?.textContent?.trim()
    ).toBe('3');

    // Verify manual INBOX_SUMMARY_CHANGED_EVENT triggers update without error.
    window.dispatchEvent(new CustomEvent(INBOX_SUMMARY_CHANGED_EVENT));
    await el.updateComplete;
    expect(overviewLinks(el)).toEqual(['/', '/profile/inbox']);
  });

  it('shows count badge, Inbox (N)/(N+) tooltip, and accessible link label when collapsed, and hides at 0', async () => {
    summaryResponse = makeSummary(7, false);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);
    const el = document.createElement('scion-nav') as ScionNav;
    el.collapsed = true;
    el.user = { id: 'u-1', email: 'u@example.com', name: 'User', role: 'member' };
    document.body.appendChild(el);
    await flush();
    await el.updateComplete;

    const inboxLink = el.shadowRoot!.querySelector('a[href="/profile/inbox"]')!;
    const tooltip = inboxLink.closest('sl-tooltip')!;
    const badge = inboxLink.querySelector('[data-nav-badge]')!;

    expect(tooltip.getAttribute('content')).toBe('Inbox (7)');
    expect(tooltip.hasAttribute('disabled')).toBe(false);
    expect(inboxLink.getAttribute('aria-label')).toBe('Inbox (7 waiting)');
    expect(badge.textContent?.trim()).toBe('7');
    expect(badge.getAttribute('aria-hidden')).toBe('true');

    // Capped count renders Inbox (25+) in tooltip and aria-label.
    summaryResponse = makeSummary(25, true);
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await el.updateComplete;

    expect(tooltip.getAttribute('content')).toBe('Inbox (25+)');
    expect(inboxLink.getAttribute('aria-label')).toBe('Inbox (25+ waiting)');
    expect(inboxLink.querySelector('[data-nav-badge]')?.textContent?.trim()).toBe('25+');

    // At 0, badge is omitted, tooltip reverts to "Inbox", and aria-label is omitted.
    summaryResponse = makeSummary(0, false);
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    await flush();
    await el.updateComplete;

    expect(tooltip.getAttribute('content')).toBe('Inbox');
    expect(inboxLink.getAttribute('aria-label')).toBeNull();
    expect(inboxLink.querySelector('[data-nav-badge]')).toBeNull();
  });
});
