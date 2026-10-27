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
    ? page.locator('scion-chat-thread scion-action-sheet[open] .item', { hasText: label })
    : page.locator('.context-menu-item', { hasText: label });
}

function indicator(page: Page, id: string): Locator {
  return msgLocator(page, id).locator('[aria-label="Starred by you"]');
}

/**
 * Waits until the thread's message list has kept the same scroll position for `quietMs`. The
 * desktop message menu is dismissed when the thread scrolls (chat-thread.ts), so a menu opened
 * while a permalink jump is still settling can close again before the test reads it.
 */
async function threadScrollIdle(page: Page, quietMs = 300, timeoutMs = 15_000): Promise<void> {
  const list = page.locator('scion-chat-thread .messages-scroll');
  const deadline = Date.now() + timeoutMs;
  let last = Number.NaN;
  let stableSince = Date.now();
  for (;;) {
    const top = await list.evaluate((el) => el.scrollTop);
    const now = Date.now();
    if (top !== last) {
      last = top;
      stableSince = now;
    } else if (now - stableSince >= quietMs) {
      return;
    }
    if (now > deadline) throw new Error(`the thread kept scrolling for ${timeoutMs} ms`);
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

/** The thread list's scrollTop, or -1 when the list is not rendered. */
async function threadScrollTop(page: Page): Promise<number> {
  const list = page.locator('scion-chat-thread .messages-scroll');
  if ((await list.count()) === 0) return -1;
  return list.evaluate((el) => el.scrollTop);
}

/** First line of an error message, without colour codes or quotes, at most 160 characters. */
function shortError(e: unknown): string {
  const text = e instanceof Error ? e.message : String(e);
  const plain = text.replace(new RegExp(`${String.fromCharCode(27)}\\[[0-9;]*m`, 'g'), '');
  return (plain.split('\n').find((l) => l.trim() !== '') ?? '').replace(/"/g, "'").slice(0, 160);
}

/**
 * Opens the message menu and clicks `label`. If the row is not visible within a second (for
 * example a desktop popup dismissed by a late scroll), it tries again, up to 15 s; a row that
 * never shows still fails at that cap. Each attempt first checks the page: if the row is already
 * visible it opens nothing; if a popup or sheet is open without the row it presses Escape (the
 * thread closes its message menu on Escape) before the right-click. Every attempt after the
 * first is recorded as a "menu-retry" annotation and printed: the attempt number, the time since
 * `loadedAt` and the previous attempt's error are recorded first, then the page state (URL, two
 * scrollTop samples 100 ms apart, open popup rows and sheets), so a run shows whether the race
 * happened and why.
 */
async function openMenuItem(
  page: Page,
  id: string,
  label: string,
  loadedAt: number
): Promise<void> {
  const item = menuItem(page, label);
  const popupRows = page.locator('.context-menu-item');
  const openSheets = page.locator('scion-chat-thread scion-action-sheet[open]');
  let attempt = 0;
  let previousError = '';
  await expect(async () => {
    attempt++;
    try {
      if (attempt > 1) {
        const info = test.info();
        const base =
          `attempt=${attempt} label="${label}" test="${info.title}" project=${info.project.name} ` +
          `elapsed_ms=${Date.now() - loadedAt} previous_error="${previousError}"`;
        // Recorded before any sampling, so a failing sample cannot lose the retry.
        const annotation = { type: 'menu-retry', description: base };
        info.annotations.push(annotation);
        let state = '';
        try {
          const rows = await popupRows.count();
          const sheets = await openSheets.count();
          const scrollA = await threadScrollTop(page);
          await new Promise((resolve) => setTimeout(resolve, 100));
          const scrollB = await threadScrollTop(page);
          state =
            ` url=${page.url()} scrollTop=${scrollA},${scrollB}` +
            ` popup_rows=${rows} open_sheets=${sheets}`;
        } catch (e) {
          state = ` state_error="${shortError(e)}"`;
        }
        annotation.description = base + state;
        console.info(`[menu-retry] ${annotation.description}`);
      }
      if (await item.isVisible()) return;
      if ((await popupRows.count()) + (await openSheets.count()) > 0) {
        await page.keyboard.press('Escape');
      }
      await msgLocator(page, id).click({ button: 'right', timeout: 2_000 });
      await expect(item).toBeVisible({ timeout: 1_000 });
      // At phone width the menu is exactly one bottom action sheet, which
      // guards the mirrored MENU_SHEET_MAX_WIDTH_PX threshold above.
      if ((page.viewportSize()?.width ?? Number.MAX_SAFE_INTEGER) <= MENU_SHEET_MAX_WIDTH_PX) {
        await expect(openSheets).toHaveCount(1);
      }
    } catch (e) {
      previousError = shortError(e);
      throw e;
    }
  }).toPass({ timeout: 15_000 });
  await item.click();
}

test.describe('Starred messages', () => {
  const env = getE2EEnv();
  let projectId = '';
  let topicId = '';
  let targetId = '';
  let memberToken = '';
  let memberId = '';

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
    // so the history can be longer than the burst with the hub unchanged.
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
    memberId = member.user.id;
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
    let loadedAt = Date.now();
    await page.goto(`${threadPath}#msg-${targetId}`, { waitUntil: 'domcontentloaded' });
    await expect(msgLocator(page, targetId)).toBeVisible({ timeout: 20_000 });
    // Let the page settle before the first menu: the legacy thread URL is rewritten to the
    // readable one, the jump scrolls the target into view, and the list stops moving.
    await expect(page).toHaveURL(new RegExp(`/chat/(?!space/)[^/]+/${topicId}(?:[?#].*)?$`), {
      timeout: 20_000,
    });
    // ratio 1: the target message is one line of text (TARGET_TEXT) that may wrap to two at 390 px,
    // plus its sender/time header, at most about 150 px tall. That is far shorter than the message
    // list on either project (desktop 1440x900, mobile 390x844), so it can be wholly in view once
    // the jump is done.
    await expect(msgLocator(page, targetId)).toBeInViewport({ ratio: 1, timeout: 20_000 });
    await threadScrollIdle(page);
    await openMenuItem(page, targetId, 'Star message', loadedAt);
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
    loadedAt = Date.now();
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

    await openMenuItem(page, targetId, 'Unstar message', loadedAt);
    await expect(indicator(page, targetId)).toHaveCount(0);
    await expect(indicator(second, targetId)).toHaveCount(0, { timeout: 15_000 });

    // Repeating DELETE stays 204.
    const del = await api(env.devToken, 'DELETE', `/api/v1/chat/stars/${targetId}`);
    expect(del.status).toBe(204);
    const after = await api(env.devToken, 'GET', `/api/v1/chat/stars?conversationKey=${topicId}`);
    expect((after.body as { items: unknown[] }).items).toEqual([]);
    await second.close();
  });

  test('human DM: star, list in the header after reload, jump; the peer has no star', async ({
    page,
  }) => {
    // The admin's user ID is the sender of the seeded thread messages.
    const hist = await api(
      env.devToken,
      'GET',
      `/api/v1/chat/conversations/${topicId}/messages?limit=1`
    );
    expect(hist.status).toBe(200);
    const adminId =
      (hist.body as { messages: Array<{ senderId?: string }> }).messages[0]?.senderId ?? '';
    expect(adminId).toMatch(/^[0-9a-f-]{36}$/);
    expect(memberId).toMatch(/^[0-9a-f-]{36}$/);
    // The canonical DM key orders the two participants' tokens.
    const [a, b] = [`user:${adminId}`, `user:${memberId}`].sort();
    const dmKey = `dm:${a}:${b}`;
    const dmPath = `/api/v1/chat/conversations/${encodeURIComponent(dmKey)}/messages`;

    // The member writes to the admin: a target, then a few later messages.
    const sendAsMember = async (content: string): Promise<string> => {
      const res = await api(memberToken, 'POST', dmPath, { content });
      expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
      const body = res.body as { id?: string; message?: { id: string } };
      return body.id ?? body.message?.id ?? '';
    };
    const dmTargetId = await sendAsMember('Mock DM target: keep the retry budget small.');
    for (let i = 0; i < 3; i++) await sendAsMember(`Mock DM filler ${i}`);
    expect(dmTargetId).toMatch(/^[0-9a-f-]{36}$/);

    const loadedAt = Date.now();
    await page.goto(`/chat/dm/${encodeURIComponent(dmKey)}#msg-${dmTargetId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(msgLocator(page, dmTargetId)).toBeVisible({ timeout: 20_000 });
    await expect(msgLocator(page, dmTargetId)).toBeInViewport({ ratio: 1, timeout: 20_000 });
    await threadScrollIdle(page);
    await openMenuItem(page, dmTargetId, 'Star message', loadedAt);
    await expect(indicator(page, dmTargetId)).toBeVisible();

    // The hub lists it as a DM star without project fields or a raw address.
    const list = await api(
      env.devToken,
      'GET',
      `/api/v1/chat/stars?conversationKey=${encodeURIComponent(dmKey)}`
    );
    expect(list.status).toBe(200);
    const items = (list.body as { items: Array<Record<string, unknown>> }).items;
    expect(items.map((i) => i.messageId)).toEqual([dmTargetId]);
    expect(items[0].conversationKind).toBe('dm');
    expect(items[0].projectId).toBeUndefined();
    expect(items[0].projectName).toBeUndefined();
    expect(items[0].conversationName).toBe('Stars Member');
    expect(items[0].senderName).toBe('Stars Member');

    // The peer of the DM has no star of their own.
    const peerState = await api(
      memberToken,
      'GET',
      `/api/v1/chat/stars/state?messageId=${dmTargetId}`
    );
    expect(peerState.status).toBe(200);
    expect((peerState.body as { starredMessageIds: string[] }).starredMessageIds).toEqual([]);

    // Reload without the fragment, open the header list and jump.
    await page.goto(`/chat/dm/${encodeURIComponent(dmKey)}`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('scion-chat-message').first()).toBeVisible({ timeout: 20_000 });
    await page.getByRole('button', { name: 'Starred messages in this conversation' }).click();
    const entry = page.locator('scion-chat-starred-list a.entry', { hasText: 'Mock DM target' });
    await expect(entry).toBeVisible();
    await entry.click();
    await expect(msgLocator(page, dmTargetId)).toBeInViewport({ timeout: 20_000 });
    await expect(indicator(page, dmTargetId)).toBeVisible();

    const del = await api(env.devToken, 'DELETE', `/api/v1/chat/stars/${dmTargetId}`);
    expect(del.status).toBe(204);
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

  test('starred-only center: only the starred message, jump to it in full context, Back restores', async ({
    page,
  }) => {
    // Phase 3. Star the target (outside the latest page) through the API, so this case
    // does not depend on the order of the others.
    const put = await api(env.devToken, 'PUT', `/api/v1/chat/stars/${targetId}`, {
      conversationKey: topicId,
    });
    expect(put.status, JSON.stringify(put.body)).toBe(200);
    try {
      const filtered = await api(
        env.devToken,
        'GET',
        `/api/v1/chat/conversations/${topicId}/messages?starred=true`
      );
      expect(filtered.status, JSON.stringify(filtered.body)).toBe(200);
      const body = filtered.body as { messages: Array<{ id: string }>; totalCount: number };
      expect(body.messages.map((m) => m.id)).toEqual([targetId]);
      expect(body.totalCount).toBe(1);

      const loadedAt = Date.now();
      await page.goto(`/chat/space/${projectId}/thread/${topicId}?messages=starred`, {
        waitUntil: 'domcontentloaded',
      });
      const banner = page.locator('scion-chat-thread .star-filter-banner');
      await expect(banner).toBeVisible({ timeout: 20_000 });
      await expect(banner).toContainText('Starred messages only');
      // Slug canonicalization keeps the filter.
      await expect(page).toHaveURL(
        new RegExp(`/chat/(?!space/)[^/]+/${topicId}\\?messages=starred$`),
        {
          timeout: 20_000,
        }
      );
      await expect(msgLocator(page, targetId)).toBeVisible({ timeout: 20_000 });
      await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(1);
      await expect(page.getByText('Mock filler message', { exact: false })).toHaveCount(0);
      await expect(page.locator('scion-chat-thread scion-chat-composer')).toHaveCount(0);

      await threadScrollIdle(page);
      await openMenuItem(page, targetId, 'Jump to message in conversation', loadedAt);
      await expect(page).toHaveURL(new RegExp(`/${topicId}#msg-${targetId}$`), { timeout: 20_000 });
      await expect(banner).toHaveCount(0);
      await expect(msgLocator(page, targetId)).toBeInViewport({ timeout: 20_000 });
      // Full context: the unstarred filler is there too.
      await expect(page.getByText('Mock filler message 0', { exact: true })).toBeAttached({
        timeout: 20_000,
      });

      await page.goBack();
      await expect(page).toHaveURL(/\?messages=starred$/, { timeout: 20_000 });
      await expect(banner).toBeVisible({ timeout: 20_000 });
      await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(1);
      await expect(msgLocator(page, targetId)).toBeVisible();
    } finally {
      await api(env.devToken, 'DELETE', `/api/v1/chat/stars/${targetId}`);
    }
  });
});
