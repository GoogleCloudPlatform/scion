// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Real-hub end-to-end Playwright suite for Inbox Dashboard live updates
 * (ptone/scion#8962, design §9 AC10; updated for Phase 1b key-noun inline
 * links in ptone/scion#9441).
 *
 * Verifies against a real Go `scion` hub with `web.inbox_dashboard` enabled:
 *   1. Page 1 opens `/profile/inbox` (Pending Secrets card shows "Nothing
 *      Waiting", count badge hidden, no inline links or footer actions) and
 *      Page 2 opens `/` in the same browser context (mounting the main
 *      sidebar and bell tray).
 *   2. An agent creates a pending user-scope secret; within ~5 s and without
 *      reloading, Page 1's Pending Secrets card prose/count/inline key-noun
 *      link + Profile nav badge update and Page 2's main sidebar badge + bell
 *      tray Inbox link converge.
 *   3. Resolving the pending secret via the Hub API (`POST /api/v1/pending-secrets/{id}/reject`)
 *      clears the card back to "Nothing Waiting" and clears badges on both
 *      Page 1 and Page 2 without reloading.
 */

import { test, expect } from '@playwright/test';
import { createTestAgent, type AgentSender } from '../harness/agent.js';
import { createSession, type AuthSession } from '../harness/auth.js';
import { readHubState, type HubState } from '../harness/hub.js';
import { stateFile } from './state.js';

interface HubApiResponse<T = unknown> {
  status: number;
  body: T;
}

async function hubApi<T = unknown>(
  hub: HubState,
  token: string,
  method: string,
  apiPath: string,
  body?: unknown
): Promise<HubApiResponse<T>> {
  const res = await fetch(`${hub.baseURL}${apiPath}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    signal: AbortSignal.timeout(15_000),
  });
  const text = await res.text();
  let parsed: unknown = text;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      // keep raw text
    }
  }
  return { status: res.status, body: parsed as T };
}

async function createProject(hub: HubState, token: string, name: string): Promise<string> {
  const res = await hubApi<{ id?: string; project?: { id: string } }>(
    hub,
    token,
    'POST',
    '/api/v1/projects',
    { name }
  );
  expect([200, 201], JSON.stringify(res.body)).toContain(res.status);
  const id = res.body.project?.id ?? res.body.id ?? '';
  expect(id).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

test.describe('Inbox Dashboard live updates on real hub (ptone/scion#8962 AC10)', () => {
  let hub!: HubState;
  let user!: AuthSession;
  let projectId = '';
  let agent!: AgentSender;

  test.beforeAll(async () => {
    const loaded = readHubState(stateFile());
    if (!loaded) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loaded;

    const stamp = Date.now();
    user = await createSession(hub.baseURL, {
      email: `alice-inbox-live-${stamp}@e2e.test`,
      role: 'member',
      displayName: 'Alice Inbox',
    });

    projectId = await createProject(hub, user.accessToken, `inbox-live-${stamp}`);

    agent = await createTestAgent(hub, {
      projectId,
      authToken: user.accessToken,
      name: 'inbox-bot',
      role: 'full',
      messageMode: 'project',
      ancestry: [user.user.id],
    });
  });

  test('live-updates /profile/inbox, sidebar/bell badges, and a second tab when a pending secret is created and resolved', async ({
    browser,
  }) => {
    const context = await browser.newContext({
      baseURL: hub.baseURL,
      storageState: user.storageStatePath,
    });

    const pageErrors: string[] = [];
    const page1 = await context.newPage();
    const page2 = await context.newPage();
    page1.on('pageerror', (err) => pageErrors.push(`page1: ${err.message}`));
    page2.on('pageerror', (err) => pageErrors.push(`page2: ${err.message}`));

    try {
      // Page 1: /profile/inbox starts with Secrets empty ("Nothing Waiting",
      // count badge hidden, no inline links or footer actions).
      await page1.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
      const secretsCard = page1.locator('[data-inbox-card="secrets"]');
      const profileNavBadge = page1
        .locator('scion-profile-nav')
        .first()
        .locator('[data-inbox-nav-badge]');
      await expect(secretsCard).toBeVisible({ timeout: 15_000 });
      await expect(secretsCard.locator('[data-card-count]')).toHaveCount(0);
      await expect(secretsCard.locator('[data-card-empty]')).toHaveText('Nothing Waiting', {
        timeout: 10_000,
      });
      await expect(secretsCard.locator('a.card-prose-link')).toHaveCount(0);
      await expect(secretsCard.locator('.card-actions')).toHaveCount(0);
      await expect(profileNavBadge).toHaveCount(0);

      // Page 2: main app shell on `/` mounts the sidebar Inbox link and bell tray.
      await page2.goto('/', { waitUntil: 'domcontentloaded' });
      const sidebarInboxLink = page2
        .locator('scion-nav')
        .first()
        .locator('a[href="/profile/inbox"]');
      await expect(sidebarInboxLink).toBeVisible({ timeout: 15_000 });
      await expect(sidebarInboxLink.locator('[data-nav-badge]')).toHaveCount(0);

      // Plant a per-page sentinel so we can assert neither tab reloaded.
      await page1.evaluate(() => {
        (window as unknown as { __inboxLiveSentinel?: string }).__inboxLiveSentinel = 'alive';
      });
      await page2.evaluate(() => {
        (window as unknown as { __inboxLiveSentinel?: string }).__inboxLiveSentinel = 'alive';
      });

      // Trigger a pending user-scope secret from the agent (202 Accepted).
      const secretKey = `INBOX_LIVE_SECRET_${Date.now()}`;
      const createRes = await agent.request<{ pendingId?: string; state?: string }>(
        `/api/v1/agents/${encodeURIComponent(agent.id)}/secrets/${encodeURIComponent(secretKey)}`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            value: 'e2e-live-secret-value',
            encoding: 'raw',
            scope: 'user',
          }),
        }
      );
      expect(createRes.status, createRes.text).toBe(202);
      const pendingId = createRes.body.pendingId ?? '';
      expect(pendingId).toBeTruthy();

      // Within ~5 s and without reloading, Page 1's summary card prose, count badge,
      // inline key-noun link, and profile nav badge update.
      await expect(secretsCard.locator('[data-card-count]')).toHaveText('1', {
        timeout: 7_000,
      });
      await expect(secretsCard.locator('[data-card-prose]')).toHaveText(
        '1 pending secret to approve, across 1 project',
        { timeout: 7_000 }
      );
      const inlineLink = secretsCard.locator('[data-card-prose] a.card-prose-link');
      await expect(inlineLink).toHaveCount(1);
      await expect(inlineLink).toHaveText('1 pending secret');
      await expect(inlineLink).toHaveAttribute('href', '/profile/secrets');
      await expect(inlineLink).toHaveCSS('text-decoration-line', 'underline');
      await expect(secretsCard.locator('.card-actions')).toHaveCount(0);
      await expect(profileNavBadge).toHaveText('1', { timeout: 7_000 });

      // Page 2 (second tab in the same context) converges without reload:
      // sidebar badge and bell tray Inbox link both reflect count 1.
      await expect(sidebarInboxLink.locator('[data-nav-badge]')).toHaveText('1', {
        timeout: 7_000,
      });
      await page2.getByRole('button', { name: 'Notifications' }).click({ timeout: 10_000 });
      await expect(page2.locator('scion-notification-tray [data-tray-inbox-link]')).toContainText(
        'Inbox · 1',
        { timeout: 7_000 }
      );

      // Resolve the pending secret via the Hub API (reject publishes the SSE hint).
      const rejectRes = await hubApi<{ state?: string }>(
        hub,
        user.accessToken,
        'POST',
        `/api/v1/pending-secrets/${encodeURIComponent(pendingId)}/reject`
      );
      expect(rejectRes.status, JSON.stringify(rejectRes.body)).toBe(200);
      expect(rejectRes.body.state).toBe('rejected');

      // Page 1 hides the count badge & inline link, restores "Nothing Waiting", and clears profile nav badge.
      await expect(secretsCard.locator('[data-card-count]')).toHaveCount(0, {
        timeout: 7_000,
      });
      await expect(secretsCard.locator('[data-card-empty]')).toHaveText('Nothing Waiting', {
        timeout: 7_000,
      });
      await expect(secretsCard.locator('a.card-prose-link')).toHaveCount(0, {
        timeout: 7_000,
      });
      await expect(profileNavBadge).toHaveCount(0, { timeout: 7_000 });

      // Page 2 converges on resolution too without reload.
      await expect(sidebarInboxLink.locator('[data-nav-badge]')).toHaveCount(0, {
        timeout: 7_000,
      });
      await expect(page2.locator('scion-notification-tray [data-tray-inbox-link]')).toHaveText(
        'Inbox →',
        { timeout: 7_000 }
      );

      // Confirm neither tab reloaded during the create/resolve lifecycle.
      const sentinel1 = await page1.evaluate(
        () => (window as unknown as { __inboxLiveSentinel?: string }).__inboxLiveSentinel
      );
      const sentinel2 = await page2.evaluate(
        () => (window as unknown as { __inboxLiveSentinel?: string }).__inboxLiveSentinel
      );
      expect(sentinel1).toBe('alive');
      expect(sentinel2).toBe('alive');

      expect(pageErrors).toEqual([]);
    } finally {
      await context.close();
    }
  });

  test('chat card shows starred/projects summary, live-updates on star/unstar without changing total, and hydrates ?filter=mentions|starred with 200px rail fit (Phase 3 AC-8, AC-10)', async ({
    browser,
  }) => {
    const context = await browser.newContext({
      baseURL: hub.baseURL,
      storageState: user.storageStatePath,
    });
    const page = await context.newPage();
    const pageErrors: string[] = [];
    page.on('pageerror', (err) => pageErrors.push(err.message));

    try {
      const createTopic = async (name: string): Promise<string> => {
        const res = await hubApi<{ id: string }>(
          hub,
          hub.devToken,
          'POST',
          `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
          { name }
        );
        expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
        return res.body.id;
      };

      const postAsDev = async (topicId: string, content: string): Promise<string> => {
        const res = await hubApi<{ id: string }>(
          hub,
          hub.devToken,
          'POST',
          `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`,
          { content }
        );
        expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
        return res.body.id;
      };

      const plainTopic = await createTopic(`plain-${Date.now()}`);
      const mentionTopic = await createTopic(`mentions-${Date.now()}`);
      await postAsDev(plainTopic, 'Routine update without mention');
      const mentionMsgId = await postAsDev(
        mentionTopic,
        `Attention @${user.user.email} please review`
      );

      await page.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
      const chatCard = page.locator('[data-inbox-card="chat"]');
      await expect(chatCard).toBeVisible({ timeout: 15_000 });
      await expect(chatCard.locator('[data-card-count]')).toHaveText('2', { timeout: 10_000 });
      await expect(chatCard.locator('[data-card-prose]')).toHaveText(
        '2 unread conversations, 1 with mentions across 1 project',
        { timeout: 10_000 }
      );
      await expect(chatCard.locator('a[data-card-prose-link="chat-unread"]')).toHaveAttribute(
        'href',
        '/chat?filter=unread'
      );
      await expect(chatCard.locator('a[data-card-prose-link="chat-mentions"]')).toHaveAttribute(
        'href',
        '/chat?filter=mentions'
      );
      await expect(chatCard.locator('a[data-card-prose-link="chat-starred"]')).toHaveCount(0);

      // Plant sentinel to prove star/unstar updates happen without page reload.
      await page.evaluate(() => {
        (window as unknown as { __chatStarSentinel?: string }).__chatStarSentinel = 'alive';
      });

      // Star a message via the session's browser fetch (cookie auth).
      const starStatus = await page.evaluate(
        async ({ msgId, convKey }) => {
          const res = await fetch(`/api/v1/chat/stars/${encodeURIComponent(msgId)}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ conversationKey: convKey }),
          });
          return res.status;
        },
        { msgId: mentionMsgId, convKey: mentionTopic }
      );
      expect(starStatus).toBeLessThan(300);

      // Live update adds starred clause & link; badge total stays 2 (AC-10).
      await expect(chatCard.locator('[data-card-prose]')).toHaveText(
        '2 unread conversations, 1 with mentions, 1 starred message across 1 project',
        { timeout: 7_000 }
      );
      await expect(chatCard.locator('a[data-card-prose-link="chat-starred"]')).toHaveAttribute(
        'href',
        '/chat?filter=starred'
      );
      await expect(chatCard.locator('[data-card-count]')).toHaveText('2');

      // Unstar the message; starred clause & link disappear without reload.
      const unstarStatus = await page.evaluate(async (msgId) => {
        const res = await fetch(`/api/v1/chat/stars/${encodeURIComponent(msgId)}`, {
          method: 'DELETE',
        });
        return res.status;
      }, mentionMsgId);
      expect(unstarStatus).toBe(204);

      await expect(chatCard.locator('[data-card-prose]')).toHaveText(
        '2 unread conversations, 1 with mentions across 1 project',
        { timeout: 7_000 }
      );
      await expect(chatCard.locator('a[data-card-prose-link="chat-starred"]')).toHaveCount(0);

      const sentinel = await page.evaluate(
        () => (window as unknown as { __chatStarSentinel?: string }).__chatStarSentinel
      );
      expect(sentinel).toBe('alive');

      // Click the "1 with mentions" link -> /chat?filter=mentions hydrates, strips param, persists.
      await chatCard.locator('a[data-card-prose-link="chat-mentions"]').click();
      const rail = page.locator('scion-chat-space-rail');
      await expect(rail).toBeVisible({ timeout: 15_000 });
      await expect
        .poll(() => new URL(page.url()).searchParams.has('filter'), { timeout: 5_000 })
        .toBe(false);
      const storedFilter = await page.evaluate(() =>
        localStorage.getItem('scion-chat-space-filter')
      );
      expect(storedFilter).toBe('mentions');

      const unreadBtn = rail.locator('.filter-toggle button', { hasText: 'Unread' });
      const mentionsChip = rail.locator('[data-mentions-chip]');
      await expect(unreadBtn).toHaveAttribute('aria-pressed', 'true');
      await expect(mentionsChip).toBeVisible();
      await expect(mentionsChip).toHaveAttribute('aria-pressed', 'true');

      // Expand the shown space and confirm only the mentioned thread appears (plain unread thread and Unread DMs hidden).
      const spaceHeader = rail.locator('.space-header').first();
      await expect(spaceHeader).toBeVisible({ timeout: 10_000 });
      await spaceHeader.click();
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'mentions-' })).toBeVisible(
        { timeout: 10_000 }
      );
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'plain-' })).toHaveCount(0);
      await expect(rail.locator('.dm-section')).toHaveCount(0);

      // AC-8 extended: toolbar and individual filter button labels do not overflow or clip across 200-260px rail widths.
      for (const width of [200, 230, 240, 260]) {
        const probe = await page.evaluate((w) => {
          const chatPage = document.querySelector('scion-page-chat');
          const railWrap = chatPage?.shadowRoot?.querySelector<HTMLElement>('.v2-rail');
          if (railWrap) {
            railWrap.style.width = `${w}px`;
            railWrap.style.minWidth = `${w}px`;
            railWrap.style.maxWidth = `${w}px`;
          }
          const railEl = chatPage?.shadowRoot?.querySelector('scion-chat-space-rail');
          const toolbar = railEl?.shadowRoot?.querySelector<HTMLElement>('.rail-toolbar');
          const subfilter = railEl?.shadowRoot?.querySelector<HTMLElement>('.rail-subfilter');
          const buttons = Array.from(
            railEl?.shadowRoot?.querySelectorAll<HTMLElement>('.filter-toggle button') ?? []
          );
          const buttonOverflows = buttons.map((b) => ({
            label: b.textContent?.trim() ?? '',
            scrollWidth: b.scrollWidth,
            clientWidth: b.clientWidth,
            clipped: b.scrollWidth > b.clientWidth + 1,
          }));
          return {
            width: w,
            railWidth: railEl?.getBoundingClientRect().width ?? 0,
            toolbarOverflow: toolbar ? toolbar.scrollWidth > toolbar.clientWidth + 1 : true,
            subfilterOverflow: subfilter ? subfilter.scrollWidth > subfilter.clientWidth + 1 : true,
            buttonOverflows,
          };
        }, width);
        expect(probe.railWidth, `width=${width}`).toBeLessThanOrEqual(width + 1);
        expect(probe.toolbarOverflow, `width=${width}`).toBe(false);
        expect(probe.subfilterOverflow, `width=${width}`).toBe(false);
        expect(probe.buttonOverflows.length, `width=${width}`).toBeGreaterThanOrEqual(3);
        expect(
          probe.buttonOverflows.every((b) => !b.clipped),
          `width=${width}: ${JSON.stringify(probe.buttonOverflows)}`
        ).toBe(true);
      }

      // Hydrate ?filter=starred directly -> Starred active, chip hidden, param stripped.
      await page.goto('/chat?filter=starred', { waitUntil: 'domcontentloaded' });
      await expect(rail).toBeVisible({ timeout: 15_000 });
      await expect
        .poll(() => new URL(page.url()).searchParams.has('filter'), { timeout: 5_000 })
        .toBe(false);
      await expect(rail.locator('.filter-toggle button', { hasText: 'Starred' })).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(rail.locator('[data-mentions-chip]')).toHaveCount(0);

      expect(pageErrors).toEqual([]);
    } finally {
      await context.close();
    }
  });
});
