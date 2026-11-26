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
      // Page 1: /profile/inbox starts with Pending Secrets empty ("Nothing Waiting",
      // count badge hidden, no inline links or footer actions).
      await page1.goto('/profile/inbox', { waitUntil: 'domcontentloaded' });
      const pendingSecretsCard = page1.locator('[data-inbox-card="pending_secrets"]');
      const profileNavBadge = page1
        .locator('scion-profile-nav')
        .first()
        .locator('[data-inbox-nav-badge]');
      await expect(pendingSecretsCard).toBeVisible({ timeout: 15_000 });
      await expect(pendingSecretsCard.locator('[data-card-count]')).toHaveCount(0);
      await expect(pendingSecretsCard.locator('[data-card-empty]')).toHaveText('Nothing Waiting', {
        timeout: 10_000,
      });
      await expect(pendingSecretsCard.locator('a.card-prose-link')).toHaveCount(0);
      await expect(pendingSecretsCard.locator('.card-actions')).toHaveCount(0);
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
      await expect(pendingSecretsCard.locator('[data-card-count]')).toHaveText('1', {
        timeout: 7_000,
      });
      await expect(pendingSecretsCard.locator('[data-card-prose]')).toHaveText(
        '1 pending secret from 1 agent in 1 project',
        { timeout: 7_000 }
      );
      const inlineLink = pendingSecretsCard.locator('[data-card-prose] a.card-prose-link');
      await expect(inlineLink).toHaveCount(1);
      await expect(inlineLink).toHaveText('1 pending secret');
      await expect(inlineLink).toHaveAttribute('href', '/profile/secrets');
      await expect(pendingSecretsCard.locator('.card-actions')).toHaveCount(0);
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
      await expect(pendingSecretsCard.locator('[data-card-count]')).toHaveCount(0, {
        timeout: 7_000,
      });
      await expect(pendingSecretsCard.locator('[data-card-empty]')).toHaveText('Nothing Waiting', {
        timeout: 7_000,
      });
      await expect(pendingSecretsCard.locator('a.card-prose-link')).toHaveCount(0, {
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
});
