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

import { createHash } from 'node:crypto';
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

      const dropdownTrigger = rail.locator('[data-unread-dropdown-trigger]');
      const unreadMain = rail.locator('[data-unread-filter-main]');
      await expect(dropdownTrigger).toBeVisible();
      await expect(unreadMain).toBeVisible();
      await expect(unreadMain).toContainText('Mentions');
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'true');
      await expect(unreadMain.locator('sl-icon')).toHaveAttribute('name', 'at');
      await expect(dropdownTrigger).toHaveAttribute('aria-haspopup', 'menu');
      await expect(rail.locator('[data-mentions-chip]')).toHaveCount(0);
      await expect(rail.locator('.rail-header .sort-btn')).toBeVisible();
      await expect(rail.locator('.rail-toolbar .sort-btn')).toHaveCount(0);

      // Expand the shown space and confirm only the mentioned thread appears (plain unread thread and Unread DMs hidden),
      // matching the Inbox Chat card's "1 with mentions" count.
      const spaceHeader = rail.locator('.space-header').first();
      await expect(spaceHeader).toBeVisible({ timeout: 10_000 });
      await spaceHeader.click();
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'mentions-' })).toBeVisible(
        { timeout: 10_000 }
      );
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'plain-' })).toHaveCount(0);
      await expect(rail.locator('.dm-section')).toHaveCount(0);

      // Clicking the main part of the split button does NOT open the dropdown.
      await unreadMain.click();
      await expect(dropdownTrigger).toHaveAttribute('aria-expanded', 'false');

      // Open the dropdown via the chevron trigger and switch Mentions -> Unread -> Mentions.
      await dropdownTrigger.click();
      await expect(dropdownTrigger).toHaveAttribute('aria-expanded', 'true');
      const unreadMenuItem = rail.locator('[data-unread-filter-menu] sl-menu-item[value="unread"]');
      const mentionsMenuItem = rail.locator(
        '[data-unread-filter-menu] sl-menu-item[value="mentions"]'
      );
      await expect(mentionsMenuItem).toHaveAttribute('checked', '');
      await expect(unreadMenuItem).toBeVisible();
      await unreadMenuItem.click();
      await expect(unreadMenuItem).toBeHidden();
      await expect(unreadMain).toContainText('Unread');
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'true');
      await expect(unreadMain.locator('sl-icon')).toHaveAttribute('name', 'envelope');
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'plain-' })).toBeVisible({
        timeout: 10_000,
      });
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'mentions-' })).toBeVisible(
        { timeout: 10_000 }
      );
      expect(await page.evaluate(() => localStorage.getItem('scion-chat-space-filter'))).toBe(
        'unread'
      );

      // Re-selecting the already-active Unread item preserves its checked state cleanly.
      await dropdownTrigger.click();
      await expect(unreadMenuItem).toHaveAttribute('checked', '');
      await unreadMenuItem.click();
      await expect(unreadMenuItem).toBeHidden();
      await dropdownTrigger.click();
      await expect(unreadMenuItem).toHaveAttribute('checked', '');

      // Switch back to Mentions via the dropdown.
      await expect(mentionsMenuItem).toBeVisible();
      await mentionsMenuItem.click();
      await expect(mentionsMenuItem).toBeHidden();
      await expect(unreadMain).toContainText('Mentions');
      await expect(unreadMain.locator('sl-icon')).toHaveAttribute('name', 'at');
      await expect(rail.locator('.thread-item .thread-name', { hasText: 'plain-' })).toHaveCount(0);

      // Switching to All and clicking the main split button re-selects Mentions without opening the menu.
      await rail.locator('.filter-toggle button', { hasText: 'All' }).click();
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'false');
      await unreadMain.click();
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'true');
      await expect(dropdownTrigger).toHaveAttribute('aria-expanded', 'false');

      // AC-8 extended: toolbar, split button, icons, and labels (including Mentions + chevron)
      // do not overflow or clip across 200-260px rail widths.
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
          const caret = railEl?.shadowRoot?.querySelector<HTMLElement>('.filter-caret');
          const mainIcon = railEl?.shadowRoot?.querySelector<HTMLElement>(
            '[data-unread-filter-main] sl-icon'
          );
          const buttons = Array.from(
            railEl?.shadowRoot?.querySelectorAll<HTMLElement>('.filter-toggle button') ?? []
          );
          const buttonOverflows = buttons.map((b) => ({
            label: b.textContent?.trim() ?? b.getAttribute('aria-label') ?? '',
            scrollWidth: b.scrollWidth,
            clientWidth: b.clientWidth,
            clipped: b.scrollWidth > b.clientWidth + 1,
          }));
          return {
            width: w,
            railWidth: railEl?.getBoundingClientRect().width ?? 0,
            toolbarOverflow: toolbar ? toolbar.scrollWidth > toolbar.clientWidth + 1 : true,
            caretVisible: caret ? getComputedStyle(caret).display !== 'none' : false,
            mainIconVisible: mainIcon ? getComputedStyle(mainIcon).display !== 'none' : false,
            buttonOverflows,
          };
        }, width);
        expect(probe.railWidth, `width=${width}`).toBeLessThanOrEqual(width + 1);
        expect(probe.toolbarOverflow, `width=${width}`).toBe(false);
        expect(probe.caretVisible, `width=${width}`).toBe(true);
        expect(probe.mainIconVisible, `width=${width}`).toBe(true);
        expect(probe.buttonOverflows.length, `width=${width}`).toBeGreaterThanOrEqual(4);
        expect(
          probe.buttonOverflows.every((b) => !b.clipped),
          `width=${width}: ${JSON.stringify(probe.buttonOverflows)}`
        ).toBe(true);
      }

      // Browser back returns from the opened space thread to /chat, and a second back returns to /profile/inbox.
      await page.goBack({ waitUntil: 'domcontentloaded' });
      await page.goBack({ waitUntil: 'domcontentloaded' });
      await expect(chatCard).toBeVisible({ timeout: 15_000 });
      await chatCard.locator('a[data-card-prose-link="chat-unread"]').click();
      await expect(rail).toBeVisible({ timeout: 15_000 });
      await expect
        .poll(() => new URL(page.url()).searchParams.has('filter'), { timeout: 5_000 })
        .toBe(false);
      await expect(unreadMain).toContainText('Unread');
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'true');
      await expect(unreadMain.locator('sl-icon')).toHaveAttribute('name', 'envelope');

      // Single-step browser back/forward works cleanly after ?filter=unread hydration.
      await page.goBack({ waitUntil: 'domcontentloaded' });
      await expect(chatCard).toBeVisible({ timeout: 15_000 });
      await page.goForward({ waitUntil: 'domcontentloaded' });
      await expect(rail).toBeVisible({ timeout: 15_000 });
      await expect(unreadMain).toContainText('Unread');
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'true');

      // Hydrate ?filter=starred directly -> Starred active, middle button inactive, param stripped.
      await page.goto('/chat?filter=starred', { waitUntil: 'domcontentloaded' });
      await expect(rail).toBeVisible({ timeout: 15_000 });
      await expect
        .poll(() => new URL(page.url()).searchParams.has('filter'), { timeout: 5_000 })
        .toBe(false);
      await expect(rail.locator('.filter-toggle button', { hasText: 'Starred' })).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(unreadMain).toContainText('Unread');
      await expect(unreadMain).toHaveAttribute('aria-pressed', 'false');

      expect(pageErrors).toEqual([]);
    } finally {
      await context.close();
    }
  });

  test('Artifact Reviews card renders to-address, requested and drafts links, live-updates on draft create/delete without changing badge total, and hydrates /artifacts filters (Phase 4 AC-9, AC-10)', async ({
    browser,
  }) => {
    const context = await browser.newContext({
      baseURL: hub.baseURL,
      storageState: user.storageStatePath,
    });
    const page = await context.newPage();
    const pageErrors: string[] = [];
    page.on('pageerror', (err) => pageErrors.push(err.message));

    const publishArtifact = async (
      token: string,
      opts: {
        artifactId?: string;
        title?: string;
        kind?: 'publish' | 'review';
        bodyText: string;
      }
    ): Promise<{ id: string; seq: number }> => {
      const data = Buffer.from(opts.bodyText, 'utf-8');
      const sha256 = createHash('sha256').update(data).digest('hex');
      const req: Record<string, unknown> = {
        entry: 'doc.md',
        files: [{ path: 'doc.md', size: data.length, sha256 }],
      };
      if (opts.title) req.title = opts.title;
      if (opts.kind) req.kind = opts.kind;
      if (!opts.artifactId) req.scope = projectId;

      const created = await hubApi<{
        artifact: { id: string };
        version: { seq: number };
        upload: { required?: string[] };
      }>(
        hub,
        token,
        'POST',
        opts.artifactId
          ? `/api/v1/artifacts/${encodeURIComponent(opts.artifactId)}/versions`
          : '/api/v1/artifacts',
        req
      );
      expect(created.status, JSON.stringify(created.body)).toBeLessThan(300);
      const { id } = created.body.artifact;
      const { seq } = created.body.version;
      for (const path of created.body.upload.required ?? []) {
        const upRes = await fetch(
          `${hub.baseURL}/api/v1/artifacts/${encodeURIComponent(id)}/versions/${seq}/files/${encodeURIComponent(path)}`,
          {
            method: 'PUT',
            headers: {
              Authorization: `Bearer ${token}`,
              'Content-Type': 'application/octet-stream',
              'X-Content-SHA256': sha256,
            },
            body: data,
            signal: AbortSignal.timeout(15_000),
          }
        );
        expect(upRes.status, await upRes.text()).toBeLessThan(300);
      }
      const fin = await hubApi(
        hub,
        token,
        'POST',
        `/api/v1/artifacts/${encodeURIComponent(id)}/versions/${seq}/finalize`,
        opts.kind === 'review' ? { base: 1 } : undefined
      );
      expect(fin.status, JSON.stringify(fin.body)).toBeLessThan(300);
      return { id, seq };
    };

    try {
      const stamp = Date.now();
      const ownedTitle = `Owned Reviewed Doc ${stamp}`;
      const requestedTitle = `Requested Review Doc ${stamp}`;

      // 1. User owns an artifact with a submitted review on latest -> toAddress = 1.
      const owned = await publishArtifact(user.accessToken, {
        title: ownedTitle,
        bodyText: '# Owned doc\nInitial version.\n',
      });
      await publishArtifact(user.accessToken, {
        artifactId: owned.id,
        kind: 'review',
        bodyText: '# Owned doc\nInitial version.{>>Looks good with minor edits<<}\n',
      });

      // 2. Dev admin publishes an artifact in the project and requests review from user -> requested = 1.
      const reqArt = await publishArtifact(hub.devToken, {
        title: requestedTitle,
        bodyText: '# Requested doc\nPlease review.\n',
      });
      const reqRes = await hubApi(
        hub,
        hub.devToken,
        'POST',
        `/api/v1/artifacts/${encodeURIComponent(reqArt.id)}/review-requests`,
        { reviewers: [`user:${user.user.id}`], note: 'Please check' }
      );
      expect(reqRes.status, JSON.stringify(reqRes.body)).toBe(200);

      // Open /profile/inbox and verify Artifact Reviews card with toAddress=1, requested=1 (drafts=0 dropped).
      await page.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
      const reviewsCard = page.locator('[data-inbox-card="reviews"]');
      await expect(reviewsCard).toBeVisible({ timeout: 15_000 });
      await expect(reviewsCard.locator('.card-title')).toHaveText('Artifact Reviews');
      await expect(reviewsCard.locator('[data-card-count]')).toHaveText('2', { timeout: 10_000 });
      await expect(reviewsCard.locator('[data-card-prose]')).toHaveText(
        '1 review to address, 1 review requested',
        { timeout: 10_000 }
      );
      await expect(
        reviewsCard.locator('a[data-card-prose-link="reviews-to-address"]')
      ).toHaveAttribute('href', '/artifacts?review_on_latest=1&owner=me');
      await expect(
        reviewsCard.locator('a[data-card-prose-link="reviews-requested"]')
      ).toHaveAttribute('href', '/artifacts?requested=1');
      await expect(reviewsCard.locator('a[data-card-prose-link="reviews-drafts"]')).toHaveCount(0);

      // Plant sentinel to verify draft create/delete live-updates via SSE without page reload.
      await page.evaluate(() => {
        (window as unknown as { __reviewsSentinel?: string }).__reviewsSentinel = 'alive';
      });

      // 3. Save a review draft -> live update adds "1 unfinished draft" without changing badge count 2 (AC-10).
      const putDraftRes = await hubApi(
        hub,
        user.accessToken,
        'PUT',
        `/api/v1/artifacts/${encodeURIComponent(reqArt.id)}/review-draft`,
        {
          rev: 0,
          base: 1,
          entry: 'doc.md',
          body: '# Requested doc\nPlease review.{>>WIP note<<}\n',
        }
      );
      expect(putDraftRes.status, JSON.stringify(putDraftRes.body)).toBe(200);

      await expect(reviewsCard.locator('[data-card-prose]')).toHaveText(
        '1 review to address, 1 review requested, 1 unfinished draft',
        { timeout: 7_000 }
      );
      await expect(reviewsCard.locator('a[data-card-prose-link="reviews-drafts"]')).toHaveAttribute(
        'href',
        '/artifacts?drafts=1'
      );
      await expect(reviewsCard.locator('[data-card-count]')).toHaveText('2');

      const sentinel = await page.evaluate(
        () => (window as unknown as { __reviewsSentinel?: string }).__reviewsSentinel
      );
      expect(sentinel).toBe('alive');

      // 4. Verify each of the three links hydrates the /artifacts list with row count matching the card's number (AC-9).
      const checkArtifactsLink = async (
        linkKey: string,
        expectedCount: number,
        expectedTitle: string,
        unexpectedTitle: string
      ): Promise<void> => {
        await page.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
        const link = page.locator(
          `[data-inbox-card="reviews"] a[data-card-prose-link="${linkKey}"]`
        );
        await expect(link).toBeVisible({ timeout: 15_000 });
        await link.click();
        const artifactsPage = page.locator('scion-page-artifacts');
        await expect(artifactsPage).toBeVisible({ timeout: 15_000 });
        await expect(artifactsPage.locator('tbody tr')).toHaveCount(expectedCount, {
          timeout: 15_000,
        });
        await expect(artifactsPage).toContainText(expectedTitle, { timeout: 15_000 });
        await expect(artifactsPage).not.toContainText(unexpectedTitle);
      };

      await checkArtifactsLink('reviews-to-address', 1, ownedTitle, requestedTitle);
      await checkArtifactsLink('reviews-requested', 1, requestedTitle, ownedTitle);
      await checkArtifactsLink('reviews-drafts', 1, requestedTitle, ownedTitle);

      // 5. Delete the draft -> live update removes the drafts clause without reload.
      await page.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
      await expect(reviewsCard.locator('[data-card-prose]')).toHaveText(
        '1 review to address, 1 review requested, 1 unfinished draft',
        { timeout: 10_000 }
      );
      const delDraftRes = await hubApi(
        hub,
        user.accessToken,
        'DELETE',
        `/api/v1/artifacts/${encodeURIComponent(reqArt.id)}/review-draft`
      );
      expect(delDraftRes.status, JSON.stringify(delDraftRes.body)).toBe(204);
      await expect(reviewsCard.locator('[data-card-prose]')).toHaveText(
        '1 review to address, 1 review requested',
        { timeout: 7_000 }
      );
      await expect(reviewsCard.locator('a[data-card-prose-link="reviews-drafts"]')).toHaveCount(0);

      expect(pageErrors).toEqual([]);
    } finally {
      await context.close();
    }
  });
});
