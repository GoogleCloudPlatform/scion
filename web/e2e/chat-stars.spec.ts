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
 * Starred messages, phase 1 (web.chat_starred_messages), end to end against
 * the real hub started by global-setup, with mock content only.
 *
 * A user stars a project-thread message outside the latest history page,
 * reloads, opens its entry from the conversation header and lands on it,
 * then unstars it. A second tab reconciles through the SSE invalidation,
 * and a second user (a project member) has no star.
 *
 * State hygiene: the spec turns the experiment on through the admin API,
 * asserts it had no stored override first, and always resets it to null.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { createSession } from './harness/auth.js';
import { getE2EEnv } from './harness/env.js';

const EXPERIMENT = 'web.chat_starred_messages';
// More than one history page (HISTORY_PAGE_SIZE is 50 in chat-thread.ts), so the target is outside
// the latest page and the jump has to load it.
const FILLER_COUNT = 60;
const TARGET_TEXT = 'Mock starred target: the rollout starts after the migration.';

interface ApiResult {
  status: number;
  body: unknown;
  /** Retry-After in seconds, when the hub sent one (a 429). */
  retryAfter: number | null;
}

async function api(
  token: string,
  method: string,
  path: string,
  body?: unknown
): Promise<ApiResult> {
  const env = getE2EEnv();
  const init: RequestInit = {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
  };
  if (body !== undefined) init.body = JSON.stringify(body);
  const res = await fetch(`${env.baseURL}${path}`, init);
  const text = await res.text();
  let parsed: unknown = text;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    // keep text
  }
  const retryAfter = Number(res.headers.get('Retry-After'));
  return {
    status: res.status,
    body: parsed,
    retryAfter: Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : null,
  };
}

async function setExperiment(token: string, value: boolean | null): Promise<void> {
  const cur = await api(token, 'GET', '/api/v1/admin/experiments');
  expect(cur.status).toBe(200);
  const revision = (cur.body as { revision: number }).revision;
  const res = await api(token, 'PUT', '/api/v1/admin/experiments', {
    overrides: { [EXPERIMENT]: value },
    expected_revision: revision,
  });
  expect(res.status, JSON.stringify(res.body)).toBe(200);
}

/** The message row: the id is on the <scion-chat-message> element itself. */
function msgLocator(page: Page, id: string): Locator {
  return page.locator(`#msg-${id}`);
}

/**
 * The width at or under which the chat opens a message's menu as the bottom action sheet
 * instead of the popup (MENU_SHEET_MAX_WIDTH_PX in components/shared/chat/context-menu.ts).
 */
const MENU_SHEET_MAX_WIDTH_PX = 768;

/** A message-menu item: a row of the action sheet on a phone-width page, else of the popup. */
function menuItem(page: Page, label: string): Locator {
  const width = page.viewportSize()?.width ?? Number.MAX_SAFE_INTEGER;
  return width <= MENU_SHEET_MAX_WIDTH_PX
    ? page.locator('scion-chat-thread scion-action-sheet .item', { hasText: label })
    : page.locator('.context-menu-item', { hasText: label });
}

function indicator(page: Page, id: string): Locator {
  return msgLocator(page, id).locator('[aria-label="Starred by you"]');
}

async function openMenuItem(page: Page, id: string, label: string): Promise<void> {
  await msgLocator(page, id).click({ button: 'right' });
  const item = menuItem(page, label);
  await expect(item).toBeVisible();
  await item.click();
}

test.describe('Starred messages (phase 1)', () => {
  const env = getE2EEnv();
  let projectId = '';
  let topicId = '';
  let targetId = '';
  let memberToken = '';

  test.beforeAll(async () => {
    // Seeding pauses for the hub's send rate limit (below), about a minute per run.
    test.setTimeout(240_000);
    const before = await api(env.devToken, 'GET', '/api/v1/admin/experiments');
    const entry = (
      before.body as { experiments: Array<{ name: string; override: boolean | null }> }
    ).experiments.find((e) => e.name === EXPERIMENT);
    expect(entry, 'the experiment is registered').toBeTruthy();
    expect(entry?.override ?? null, 'no leftover override').toBeNull();
    await setExperiment(env.devToken, true);

    const project = await api(env.devToken, 'POST', '/api/v1/projects', {
      name: `stars-e2e-${Date.now()}`,
    });
    expect(project.status, JSON.stringify(project.body)).toBe(201);
    projectId = (project.body as { id: string }).id;
    const topic = await api(env.devToken, 'POST', `/api/v1/chat/spaces/${projectId}/threads`, {
      name: 'stars-e2e',
    });
    expect(topic.status, JSON.stringify(topic.body)).toBeLessThan(300);
    topicId = (topic.body as { id: string }).id;

    // The hub allows a human sender 30 sends a minute (a bucket of 30 that refills one every 2 s)
    // and answers 429 with Retry-After when it is empty. The seed waits as told and sends again,
    // so the history can be longer than the burst without any test-only bypass.
    const send = async (content: string): Promise<string> => {
      let res: ApiResult;
      for (let attempt = 0; ; attempt++) {
        res = await api(env.devToken, 'POST', `/api/v1/chat/conversations/${topicId}/messages`, {
          content,
        });
        if (res.status !== 429 || attempt >= 20) break;
        await new Promise((resolve) => setTimeout(resolve, (res.retryAfter ?? 2) * 1000 + 100));
      }
      expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
      const body = res.body as { id?: string; message?: { id: string } };
      return body.id ?? body.message?.id ?? '';
    };
    targetId = await send(TARGET_TEXT);
    for (let i = 0; i < FILLER_COUNT; i++) await send(`Mock filler message ${i}`);
    if (!targetId) {
      const hist = await api(
        env.devToken,
        'GET',
        `/api/v1/chat/conversations/${topicId}/messages?limit=200`
      );
      const msgs = (hist.body as { messages: Array<{ id: string; msg: string }> }).messages;
      targetId = msgs.find((m) => m.msg === TARGET_TEXT)?.id ?? '';
    }
    expect(targetId).toMatch(/^[0-9a-f-]{36}$/);

    // A second user, a member of the project.
    const member = await createSession(env.baseURL, {
      email: 'stars-member@e2e.test',
      role: 'member',
      displayName: 'Stars Member',
    });
    memberToken = member.accessToken;
    const roles = await api(
      env.devToken,
      'GET',
      `/api/v1/projects/${projectId}/members/assignable-roles`
    );
    const role = ((roles.body as { items?: Array<{ id: string; name: string }> }).items ?? []).find(
      (r) => r.name === 'project-member'
    );
    expect(role).toBeTruthy();
    const added = await api(env.devToken, 'POST', `/api/v1/projects/${projectId}/members`, {
      roleDefinitionId: role!.id,
      principalType: 'user',
      principalId: 'stars-member@e2e.test',
    });
    expect([200, 201]).toContain(added.status);
  });

  test.afterAll(async () => {
    await setExperiment(env.devToken, null);
  });

  test.use({ storageState: env.adminStorageState });

  test('star outside the latest page, reload, jump from the header, unstar; second tab reconciles', async ({
    page,
    context,
  }) => {
    const threadPath = `/chat/space/${projectId}/thread/${topicId}`;

    // The target is outside the latest page: open it through its permalink
    // (the existing around-message jump).
    await page.goto(`${threadPath}#msg-${targetId}`, { waitUntil: 'domcontentloaded' });
    await expect(msgLocator(page, targetId)).toBeVisible({ timeout: 20_000 });
    await openMenuItem(page, targetId, 'Star message');
    await expect(indicator(page, targetId)).toBeVisible();

    // Repeating PUT keeps one association and its time.
    const first = await api(env.devToken, 'GET', `/api/v1/chat/stars?conversationKey=${topicId}`);
    const items = (first.body as { items: Array<{ messageId: string; starredAt: string }> }).items;
    expect(items.map((i) => i.messageId)).toEqual([targetId]);
    const again = await api(env.devToken, 'PUT', `/api/v1/chat/stars/${targetId}`, {
      conversationKey: topicId,
    });
    expect((again.body as { starredAt: string }).starredAt).toBe(items[0].starredAt);

    // The second user has no star.
    const memberState = await api(
      memberToken,
      'GET',
      `/api/v1/chat/stars/state?messageId=${targetId}`
    );
    expect(memberState.status).toBe(200);
    expect((memberState.body as { starredMessageIds: string[] }).starredMessageIds).toEqual([]);

    // Reload on the latest page: the target is not loaded.
    await page.goto(threadPath, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('scion-chat-message').first()).toBeVisible({ timeout: 20_000 });
    await expect(msgLocator(page, targetId)).toHaveCount(0);

    // Open the header list and jump to the entry.
    await page.getByRole('button', { name: 'Starred messages in this conversation' }).click();
    const entry = page.locator('scion-chat-starred-list a.entry', {
      hasText: 'Mock starred target',
    });
    await expect(entry).toBeVisible();
    await entry.click();
    await expect(msgLocator(page, targetId)).toBeInViewport({ timeout: 20_000 });
    await expect(indicator(page, targetId)).toBeVisible();

    // A second tab shows the star, then reconciles when the first unstars.
    const second = await context.newPage();
    await second.goto(`${threadPath}#msg-${targetId}`, { waitUntil: 'domcontentloaded' });
    await expect(indicator(second, targetId)).toBeVisible({ timeout: 20_000 });

    await openMenuItem(page, targetId, 'Unstar message');
    await expect(indicator(page, targetId)).toHaveCount(0);
    await expect(indicator(second, targetId)).toHaveCount(0, { timeout: 15_000 });

    // Repeating DELETE stays 204.
    const del = await api(env.devToken, 'DELETE', `/api/v1/chat/stars/${targetId}`);
    expect(del.status).toBe(204);
    const after = await api(env.devToken, 'GET', `/api/v1/chat/stars?conversationKey=${topicId}`);
    expect((after.body as { items: unknown[] }).items).toEqual([]);
    await second.close();
  });

  test('flag off: routes answer 404 and the UI shows no star surfaces', async ({ page }) => {
    await setExperiment(env.devToken, false);
    try {
      const res = await api(env.devToken, 'GET', `/api/v1/chat/stars?conversationKey=${topicId}`);
      expect(res.status).toBe(404);
      await page.goto(`/chat/space/${projectId}/thread/${topicId}`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(page.locator('scion-chat-message').first()).toBeVisible({ timeout: 20_000 });
      await expect(page.locator('scion-chat-starred-list')).toHaveCount(0);
      await page.locator('scion-chat-message').last().click({ button: 'right' });
      await expect(menuItem(page, 'Copy link')).toBeVisible();
      await expect(menuItem(page, 'Star message')).toHaveCount(0);
    } finally {
      await setExperiment(env.devToken, true);
    }
  });
});
