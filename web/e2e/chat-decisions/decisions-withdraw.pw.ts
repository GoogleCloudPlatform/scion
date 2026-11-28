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
 * Real-hub end-to-end Playwright suite for decision withdrawal
 * (ptone/scion#8193, parent ptone/scion#8145, design-8145 r3 §10):
 *   1. Agent asks a 3-row set; user selects choices; agent withdraws one row
 *      with a reason via the real `scion decisions withdraw` CLI in agent mode.
 *   2. Card updates live over SSE in a second open tab (without reload), drops
 *      the withdrawn row's selection, decrements the counter, posts no chat
 *      message (D4), refuses a stale set submit with 409 `answer_set_changed`
 *      (`withdrawn`), and `Submit answers` covers the remaining two rows.
 *   3. Agent asks a single decision and withdraws it; it leaves both the
 *      project and profile `Waiting on you` pages and their badges live without
 *      reload; a forced answer from a stale card receives 409 `withdrawn`,
 *      updates to the withdrawn state, shows the warning toast, and records
 *      nothing.
 */

import { execFileSync } from 'node:child_process';
import * as crypto from 'node:crypto';
import * as os from 'node:os';
import * as path from 'node:path';
import * as fs from 'node:fs';
import { test, expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { createTestAgent, type AgentSender } from '../harness/agent.js';
import { createSession, type AuthSession } from '../harness/auth.js';
import { hubBinaryPath, readHubState, type HubState } from '../harness/hub.js';
import { stateFile } from './state.js';

interface TrackedEventSourceInfo {
  url: string;
  readyState: number;
}

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

async function createProjectTopic(
  hub: HubState,
  projectId: string,
  name: string,
  defaultAgent?: string
): Promise<string> {
  const payload = defaultAgent ? { name, defaultAgent } : { name };
  const deadline = Date.now() + 5_000;
  let res = await hubApi<{ id?: string; thread?: { id: string } }>(
    hub,
    hub.devToken,
    'POST',
    `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
    payload
  );
  while (res.status === 503 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    res = await hubApi<{ id?: string; thread?: { id: string } }>(
      hub,
      hub.devToken,
      'POST',
      `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
      payload
    );
  }
  expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
  const id = res.body.thread?.id ?? res.body.id ?? '';
  expect(id).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

function runAgentScionCli(
  hub: HubState,
  agent: AgentSender,
  args: string[],
  scratchHome: string
): string {
  const cleanEnv: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (!k.startsWith('SCION_')) {
      cleanEnv[k] = v;
    }
  }
  cleanEnv.HOME = scratchHome;
  cleanEnv.SCION_CLI_MODE = 'agent';
  cleanEnv.SCION_HUB_ENDPOINT = hub.baseURL;
  cleanEnv.SCION_AUTH_TOKEN = agent.token;
  cleanEnv.SCION_PROJECT_ID = agent.projectId;
  return execFileSync(hubBinaryPath(), args, {
    env: cleanEnv,
    encoding: 'utf8',
    timeout: 15_000,
  });
}

async function openUserContext(
  browser: Browser,
  hub: HubState,
  session: AuthSession
): Promise<BrowserContext> {
  const bootSeed = crypto.randomUUID();
  const context = await browser.newContext({
    baseURL: hub.baseURL,
    storageState: session.storageStatePath,
  });
  await context.addInitScript((seed: string) => {
    window.__e2eBootId = `${seed}:${performance.timeOrigin}`;
    const tracked: EventSource[] = [];
    window.__e2eEventSources = tracked;
    const NativeEventSource = window.EventSource;
    class ObservedEventSource extends NativeEventSource {
      constructor(url: string | URL, eventSourceInitDict?: EventSourceInit) {
        super(url, eventSourceInitDict);
        tracked.push(this);
      }
    }
    window.EventSource = ObservedEventSource;
  }, bootSeed);
  return context;
}

async function awaitSSE(page: Page, requiredSubjects: string[]): Promise<string> {
  await expect
    .poll(
      async () => {
        const sources = await page.evaluate((): TrackedEventSourceInfo[] => {
          return (window.__e2eEventSources ?? []).map((es) => ({
            url: es.url,
            readyState: es.readyState,
          }));
        });
        return requiredSubjects.every((subj) =>
          sources.some((s) => {
            if (s.readyState !== 1) return false;
            const decoded = decodeURIComponent(s.url);
            return decoded.includes('/events') && decoded.includes(subj);
          })
        );
      },
      {
        timeout: 20_000,
        message: `Expected open SSE stream(s) subscribed to ${requiredSubjects.join(', ')}`,
      }
    )
    .toBe(true);

  const bootId = await page.evaluate(() => window.__e2eBootId ?? '');
  expect(bootId).not.toBe('');
  return bootId;
}

async function openChatAndAwaitSSE(
  page: Page,
  routePath: string,
  requiredSubject: string
): Promise<string> {
  await page.goto(routePath, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('scion-chat-thread')).toBeVisible({ timeout: 20_000 });
  return awaitSSE(page, [requiredSubject]);
}

async function assertNoPageReload(page: Page, expectedBootId: string): Promise<void> {
  const current = await page.evaluate(() => window.__e2eBootId ?? '');
  expect(current).toBe(expectedBootId);
}

test.describe('Withdraw decisions on real hub (ptone/scion#8193)', () => {
  let hub!: HubState;
  let projectId = '';
  let topicId = '';
  let userA!: AuthSession;
  let agent!: AgentSender;
  let scratchHome = '';

  test.beforeAll(async () => {
    const loadedHub = readHubState(stateFile());
    if (!loadedHub) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loadedHub;
    scratchHome = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-wd-cli-home-'));

    const projectRes = await hubApi<{ id?: string; project?: { id: string } }>(
      hub,
      hub.devToken,
      'POST',
      '/api/v1/projects',
      { name: `decisions-withdraw-${Date.now()}` }
    );
    expect([200, 201], JSON.stringify(projectRes.body)).toContain(projectRes.status);
    projectId = projectRes.body.project?.id ?? projectRes.body.id ?? '';
    expect(projectId).toMatch(/^[0-9a-f-]{36}$/);

    userA = await createSession(hub.baseURL, {
      email: `alice-withdraw-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Alice Withdraw',
    });

    const rolesRes = await hubApi<{ items?: Array<{ id: string; name: string }> }>(
      hub,
      hub.devToken,
      'GET',
      `/api/v1/projects/${encodeURIComponent(projectId)}/members/assignable-roles`
    );
    expect(rolesRes.status).toBe(200);
    const adminRole = (rolesRes.body.items ?? []).find((r) => r.name === 'project-admin');
    expect(adminRole).toBeTruthy();

    const addRes = await hubApi(
      hub,
      hub.devToken,
      'POST',
      `/api/v1/projects/${encodeURIComponent(projectId)}/members`,
      {
        roleDefinitionId: adminRole!.id,
        principalType: 'user',
        principalId: userA.user.email,
      }
    );
    expect([200, 201], JSON.stringify(addRes.body)).toContain(addRes.status);

    agent = await createTestAgent(hub, {
      projectId,
      name: 'release-bot',
      messageMode: 'project',
    });

    topicId = await createProjectTopic(hub, projectId, 'withdraw-e2e', agent.slug);
  });

  test.afterAll(() => {
    if (scratchHome) {
      fs.rmSync(scratchHome, { recursive: true, force: true });
    }
  });

  test('3-row set live withdrawal across tabs, single withdrawal leaving project & profile Waiting on you, and forced 409 states', async ({
    browser,
  }) => {
    const chatRoute = `/chat/space/${projectId}/thread/${topicId}`;
    const chatSubject = `project.${projectId}.chat.>`;

    const ctx = await openUserContext(browser, hub, userA);
    try {
      const tab1 = await ctx.newPage();
      const tab2 = await ctx.newPage();
      const [bootTab1, bootTab2] = await Promise.all([
        openChatAndAwaitSSE(tab1, chatRoute, chatSubject),
        openChatAndAwaitSSE(tab2, chatRoute, chatSubject),
      ]);

      // 1. Agent asks a 3-row decision set.
      const setSend = await agent.sendDecisionPrompt({
        conversationRef: '#withdraw-e2e',
        msg: 'Three calls before the release:',
        decisions: [
          {
            id: 'WD-S1',
            question: 'Ship `v0.9` today?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
            default: 'yes',
          },
          {
            id: 'WD-S2',
            question: 'Which approach?',
            choices: [
              { id: 'a', label: 'Extend input-needed' },
              { id: 'b', label: 'New waiting state' },
            ],
          },
          {
            id: 'WD-S3',
            question: 'Deploy to prod after tagging?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
            default: 'no',
          },
        ],
      });
      expect(setSend.decisions).toHaveLength(3);
      const [d1, d2, d3] = setSend.decisions!;

      const cardSelector = `scion-chat-message[id="msg-${setSend.message_id}"] scion-chat-decisions`;
      const setCardTab1 = tab1.locator(cardSelector);
      const setCardTab2 = tab2.locator(cardSelector);
      await expect(setCardTab1).toBeVisible({ timeout: 15_000 });
      await expect(setCardTab2).toBeVisible({ timeout: 15_000 });
      await expect(setCardTab2.locator('.counter')).toHaveText('3 remaining');

      // 2. User selects choices on WD-S1 and WD-S2 in Tab 2.
      await setCardTab2.locator(`[data-row="${d1.row_id}"] button[data-choice="yes"]`).click();
      await setCardTab2.locator(`[data-row="${d2.row_id}"] button[data-choice="a"]`).click();
      await expect(setCardTab2.locator('.counter')).toHaveText('1 remaining');

      const beforeWithdrawHist = await hubApi<{ messages?: Array<{ id: string }> }>(
        hub,
        userA.accessToken,
        'GET',
        `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`
      );
      expect(beforeWithdrawHist.status).toBe(200);
      const msgCountBeforeWithdraw = (beforeWithdrawHist.body.messages ?? []).length;

      // 3. Agent withdraws WD-S2 with a reason using the real `scion decisions withdraw` CLI.
      const cliOut1 = runAgentScionCli(
        hub,
        agent,
        ['decisions', 'withdraw', 'WD-S2', '--reason', 'Settled in architecture review.'],
        scratchHome
      );
      expect(cliOut1.trim()).toBe('WD-S2 v1 withdrawn');

      // Repeating is idempotent and preserves the first reason.
      const cliOut2 = runAgentScionCli(
        hub,
        agent,
        ['decisions', 'withdraw', 'WD-S2', '--reason', 'Different reason'],
        scratchHome
      );
      expect(cliOut2.trim()).toBe('WD-S2 v1 already withdrawn');

      const cliShow = runAgentScionCli(hub, agent, ['decisions', 'show', 'WD-S2'], scratchHome);
      expect(cliShow).toContain('withdrawn');
      expect(cliShow).toContain('Settled in architecture review.');

      // 4. Card updates live in Tab 2 (and Tab 1) without reload; counter drops; D4 posts no chat line.
      for (const c of [setCardTab2, setCardTab1]) {
        const wdRow = c.locator(`[data-row="${d2.row_id}"]`);
        await expect(wdRow).toHaveAttribute('data-status', 'withdrawn', { timeout: 15_000 });
        await expect(wdRow).toHaveClass(/withdrawn/);
        await expect(wdRow.locator('.wd')).toContainText(
          'No longer needed · withdrawn by release-bot'
        );
        await expect(wdRow.locator('.wd-reason')).toHaveText('Settled in architecture review.');
      }
      await expect(setCardTab2.locator('.counter')).toHaveText('1 remaining · 1 no longer needed');
      await assertNoPageReload(tab1, bootTab1);
      await assertNoPageReload(tab2, bootTab2);

      const afterWithdrawHist = await hubApi<{ messages?: Array<{ id: string }> }>(
        hub,
        userA.accessToken,
        'GET',
        `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`
      );
      expect(afterWithdrawHist.status).toBe(200);
      expect((afterWithdrawHist.body.messages ?? []).length).toBe(msgCountBeforeWithdraw);

      // Verify a stale set submit including the withdrawn row receives 409 answer_set_changed (`withdrawn`).
      const forcedSet409 = await tab2.evaluate(
        async ({ msgId, r1, r2, r3 }) => {
          const r = await fetch(`/api/v1/decision-prompts/${encodeURIComponent(msgId)}/answers`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              idempotency_key: crypto.randomUUID(),
              answers: [
                { decision_row_id: r1, choice_id: 'yes' },
                { decision_row_id: r2, choice_id: 'a' },
                { decision_row_id: r3, choice_id: 'no' },
              ],
            }),
          });
          return { status: r.status, body: (await r.json()) as Record<string, unknown> };
        },
        { msgId: setSend.message_id, r1: d1.row_id, r2: d2.row_id, r3: d3.row_id }
      );
      expect(forcedSet409.status).toBe(409);
      const setErr = forcedSet409.body.error as {
        code?: string;
        details?: { reasons?: Record<string, string> };
      };
      expect(setErr.code).toBe('answer_set_changed');
      expect(setErr.details?.reasons?.[d2.row_id]).toBe('withdrawn');

      // 5. User selects WD-S3 in Tab 2 (`0 remaining · 1 no longer needed`) and submits the remaining 2 rows.
      await setCardTab2.locator(`[data-row="${d3.row_id}"] button[data-choice="no"]`).click();
      await expect(setCardTab2.locator('.counter')).toHaveText('0 remaining · 1 no longer needed');
      await setCardTab2.locator('sl-button.submit').click();

      for (const c of [setCardTab2, setCardTab1]) {
        await expect(c.locator(`[data-row="${d1.row_id}"]`)).toHaveAttribute(
          'data-status',
          'decided',
          { timeout: 15_000 }
        );
        await expect(c.locator(`[data-row="${d2.row_id}"]`)).toHaveAttribute(
          'data-status',
          'withdrawn'
        );
        await expect(c.locator(`[data-row="${d3.row_id}"]`)).toHaveAttribute(
          'data-status',
          'decided'
        );
      }
      await tab2.close();

      // 6. Agent asks a single decision; open Project Decisions, Profile Decisions, and a stale chat tab.
      const singleSend = await agent.sendDecisionPrompt({
        conversationRef: '#withdraw-e2e',
        msg: 'One more check before cutting hotfix:',
        decisions: [
          {
            id: 'WD-SINGLE',
            question: 'Ship the hotfix tag right now?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
            default: 'yes',
          },
        ],
      });
      const singleRowId = singleSend.decisions![0].row_id;

      const projPage = await ctx.newPage();
      await projPage.goto(`/projects/${projectId}/decisions`, { waitUntil: 'domcontentloaded' });
      const bootProj = await awaitSSE(projPage, [`project.${projectId}.>`]);
      const projDecisions = projPage.locator('scion-page-project-decisions');
      await expect(projDecisions.locator('[data-waiting-badge]')).toHaveText('1', {
        timeout: 15_000,
      });
      await expect(
        projDecisions.locator(`[data-prompt-card="${singleSend.message_id}"]`)
      ).toBeVisible();

      const profPage = await ctx.newPage();
      await profPage.goto('/profile/decisions', { waitUntil: 'domcontentloaded' });
      const bootProf = await awaitSSE(profPage, ['project.>', `user.${userA.user.id}.chat.>`]);
      const profDecisions = profPage.locator('scion-page-profile-decisions');
      await expect(profDecisions.locator('[data-waiting-badge]')).toHaveText('1', {
        timeout: 15_000,
      });
      await expect(
        profDecisions.locator(`[data-prompt-card="${singleSend.message_id}"]`)
      ).toBeVisible();
      const profileNav = profPage.locator('scion-profile-nav').first();
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('1');

      // Open a stale chat tab and close its SSE streams so clicking Yes after withdrawal hits the 409 path.
      const staleChatTab = await ctx.newPage();
      await openChatAndAwaitSSE(staleChatTab, chatRoute, chatSubject);
      const staleSingleCard = staleChatTab.locator(
        `scion-chat-message[id="msg-${singleSend.message_id}"] scion-chat-decisions`
      );
      await expect(staleSingleCard.locator(`[data-row="${singleRowId}"]`)).toHaveAttribute(
        'data-status',
        'pending',
        { timeout: 15_000 }
      );
      await staleChatTab.evaluate(() => {
        for (const es of window.__e2eEventSources ?? []) {
          es.close();
        }
      });

      // 7. Agent withdraws WD-SINGLE; verify it leaves project and profile Waiting on you and badges live.
      const cliOutSingle = runAgentScionCli(
        hub,
        agent,
        ['decisions', 'withdraw', 'WD-SINGLE', '--reason', 'Release postponed to Monday.'],
        scratchHome
      );
      expect(cliOutSingle.trim()).toBe('WD-SINGLE v1 withdrawn');

      await expect(
        projDecisions.locator(`[data-prompt-card="${singleSend.message_id}"]`)
      ).toHaveCount(0, { timeout: 15_000 });
      await expect(projDecisions.locator('[data-waiting-badge]')).toHaveCount(0);
      await expect(projDecisions.locator('[data-waiting-others]')).toHaveCount(0);
      await expect(projDecisions.locator('[data-empty="pending"]')).toBeVisible();
      await assertNoPageReload(projPage, bootProj);

      await expect(
        profDecisions.locator(`[data-prompt-card="${singleSend.message_id}"]`)
      ).toHaveCount(0, { timeout: 15_000 });
      await expect(profDecisions.locator('[data-waiting-badge]')).toHaveCount(0);
      await expect(profDecisions.locator('[data-waiting-others]')).toHaveCount(0);
      await expect(profDecisions.locator('[data-empty="pending"]')).toBeVisible();
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveCount(0);
      await assertNoPageReload(profPage, bootProf);

      await expect(
        tab1.locator(
          `scion-chat-message[id="msg-${singleSend.message_id}"] scion-chat-decisions [data-row="${singleRowId}"]`
        )
      ).toHaveAttribute('data-status', 'withdrawn', { timeout: 15_000 });

      // 8. Forced answer in staleChatTab receives HTTP 409 `withdrawn`, updates card, and shows toast.
      await staleSingleCard.locator('button[data-choice="yes"]').click();
      await expect(staleSingleCard.locator(`[data-row="${singleRowId}"]`)).toHaveAttribute(
        'data-status',
        'withdrawn',
        { timeout: 15_000 }
      );
      await expect(staleSingleCard.locator('.wd')).toContainText(
        'No longer needed · withdrawn by release-bot'
      );
      await expect(staleSingleCard.locator('.wd-reason')).toHaveText(
        'Release postponed to Monday.'
      );
      await expect(
        staleChatTab.locator('sl-alert[open]:not([data-testid="test-hub-banner"])')
      ).toContainText('No longer needed: release-bot withdrew this decision');

      // Verify nothing was recorded for WD-SINGLE or WD-S2 in Made.
      const madeRes = await hubApi<{ entries?: Array<{ decision?: { key?: string } }> }>(
        hub,
        userA.accessToken,
        'GET',
        `/api/v1/projects/${encodeURIComponent(projectId)}/decisions?tab=made`
      );
      expect(madeRes.status).toBe(200);
      const madeKeys = (madeRes.body.entries ?? []).map((e) => e.decision?.key);
      expect(madeKeys).toContain('WD-S1');
      expect(madeKeys).toContain('WD-S3');
      expect(madeKeys).not.toContain('WD-S2');
      expect(madeKeys).not.toContain('WD-SINGLE');
    } finally {
      await ctx.close();
    }
  });
});
