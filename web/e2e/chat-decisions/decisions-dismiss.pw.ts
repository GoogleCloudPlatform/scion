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
 * Real-hub end-to-end Playwright scenario for user decision dismissal
 * (ptone/scion#8216, ptone/scion#8217, ptone/scion#8218, parent ptone/scion#8194):
 *   1. All scenario prompts (a 3-row set in a topic, a single topic prompt, and
 *      a 2-row DM set) are asked up front at t=0 so the fixed 5-minute
 *      `store.DecisionDismissMinAge` window is traversed once against an
 *      unmodified hub binary (no test clock hook exists in production).
 *   2. Inside the 5-minute grace window: verifies disabled UI state + countdown
 *      tooltip on both surfaces, hub `409 too_recent` with `availableAt`, and
 *      DM dismissal refusal (`404`) for a non-participant admin (acceptance 12).
 *   3. Once `availableAt` arrives: verifies live timer unlock without reload,
 *      dismisses 2 of 3 rows from the chat card `⋯` menu with a reason, verifies
 *      live SSE update in a second open tab (counter drops, compact dismissal
 *      line renders, 3rd row remains answerable and is submitted), verifies the
 *      asking agent receives the `decision-response` (broker dispatch + CLI
 *      `scion decisions list --dismissed` / `show`), verifies Profile
 *      `Waiting on you`, nav badge, and `decisions_waiting` bell notification
 *      drop, bulk-dismisses the remaining 2 waiting cards via multi-select on
 *      `/profile/decisions`, and verifies all dismissals render on `Made` under
 *      `Provenance = Dismissed`.
 */

import { execFileSync } from 'node:child_process';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
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

interface NotificationRow {
  id: string;
  kind?: string;
  status?: string;
  message?: string;
  acknowledged?: boolean;
  resourceId?: string;
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

async function fetchDecisionsBellRow(
  hub: HubState,
  accessToken: string
): Promise<NotificationRow | undefined> {
  const res = await hubApi<
    NotificationRow[] | { notifications?: NotificationRow[]; items?: NotificationRow[] }
  >(hub, accessToken, 'GET', '/api/v1/notifications?acknowledged=true');
  expect(res.status).toBe(200);
  const list = Array.isArray(res.body)
    ? res.body
    : (res.body.notifications ?? res.body.items ?? []);
  return list.find((n) => n.kind === 'decisions_waiting');
}

test.describe('Dismiss decisions on real hub (ptone/scion#8216, ptone/scion#8217, ptone/scion#8218)', () => {
  // Traverses the real 5-minute DecisionDismissMinAge window once for all seeded prompts.
  test.setTimeout(420_000);

  let hub!: HubState;
  let projectId = '';
  let topicId = '';
  let userA!: AuthSession;
  let userOutsider!: AuthSession;
  let agent!: AgentSender;
  let scratchHome = '';

  test.beforeAll(async () => {
    const loadedHub = readHubState(stateFile());
    if (!loadedHub) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loadedHub;
    scratchHome = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-dsm-cli-home-'));

    const stamp = Date.now();
    const projectRes = await hubApi<{ id?: string; project?: { id: string } }>(
      hub,
      hub.devToken,
      'POST',
      '/api/v1/projects',
      { name: `decisions-dismiss-${stamp}` }
    );
    expect([200, 201], JSON.stringify(projectRes.body)).toContain(projectRes.status);
    projectId = projectRes.body.project?.id ?? projectRes.body.id ?? '';
    expect(projectId).toMatch(/^[0-9a-f-]{36}$/);

    userA = await createSession(hub.baseURL, {
      email: `alice-dismiss-${stamp}@e2e.test`,
      role: 'member',
      displayName: 'Alice Dismiss',
    });
    userOutsider = await createSession(hub.baseURL, {
      email: `eve-admin-${stamp}@e2e.test`,
      role: 'admin',
      displayName: 'Eve Admin',
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

    topicId = await createProjectTopic(hub, projectId, 'dismiss-e2e', agent.slug);
  });

  test.afterAll(() => {
    if (scratchHome) {
      fs.rmSync(scratchHome, { recursive: true, force: true });
    }
  });

  test('3-row set subset dismiss in chat, live second-tab update, agent delivery, bell/badge drop, profile multi-select bulk dismiss, and Made Dismissed filter', async ({
    browser,
  }) => {
    const chatRoute = `/chat/space/${projectId}/thread/${topicId}`;
    const chatSubject = `project.${projectId}.chat.>`;

    // 1. Seed all 3 prompts up front at t=0 so they share one 5-minute grace window:
    //    - Prompt 1: 3-row set in #dismiss-e2e (DSM-S1, DSM-S2, DSM-S3)
    //    - Prompt 2: single decision in #dismiss-e2e (DSM-P1)
    //    - Prompt 3: 2-row DM set to Alice (DSM-DM1, DSM-DM2) -> also tests DM-only authz (acceptance 12)
    const setSend = await agent.sendDecisionPrompt({
      conversationRef: '#dismiss-e2e',
      msg: 'Three release calls before tagging:',
      decisions: [
        {
          id: 'DSM-S1',
          question: 'Ship `v0.9` today?',
          choices: [
            { id: 'yes', label: 'Yes' },
            { id: 'no', label: 'No' },
          ],
          default: 'yes',
        },
        {
          id: 'DSM-S2',
          question: 'Which migration rollout?',
          choices: [
            { id: 'staged', label: 'Staged' },
            { id: 'all', label: 'All at once' },
          ],
        },
        {
          id: 'DSM-S3',
          question: 'Notify `#ops` after deploy?',
          choices: [
            { id: 'yes', label: 'Yes' },
            { id: 'no', label: 'No' },
          ],
          default: 'yes',
        },
      ],
    });
    expect(setSend.decisions).toHaveLength(3);
    const [s1, s2, s3] = setSend.decisions!;

    const card2Send = await agent.sendDecisionPrompt({
      conversationRef: '#dismiss-e2e',
      msg: 'Docs follow-up:',
      decisions: [
        {
          id: 'DSM-P1',
          question: 'Archive the old `v0.8` runbook?',
          choices: [
            { id: 'archive', label: 'Archive' },
            { id: 'keep', label: 'Keep' },
          ],
        },
      ],
    });
    expect(card2Send.decisions).toHaveLength(1);

    const card3DmSend = await agent.sendDecisionPrompt({
      recipient: `user:${userA.user.email}`,
      msg: 'Personal deploy credentials check:',
      decisions: [
        {
          id: 'DSM-DM1',
          question: 'Rotate staging deploy key?',
          choices: [
            { id: 'rotate', label: 'Rotate' },
            { id: 'later', label: 'Later' },
          ],
        },
        {
          id: 'DSM-DM2',
          question: 'Prune old local token?',
          choices: [
            { id: 'prune', label: 'Prune' },
            { id: 'keep', label: 'Keep' },
          ],
        },
      ],
    });
    expect(card3DmSend.decisions).toHaveLength(2);

    // 2. Acceptance 12: DM decisions can be dismissed ONLY by the DM's user (non-participant admin gets 404).
    const outsiderDmDismiss = await hubApi(
      hub,
      userOutsider.accessToken,
      'POST',
      `/api/v1/decision-prompts/${encodeURIComponent(card3DmSend.message_id)}/dismiss`,
      { reason: 'Admin intrusion attempt' }
    );
    expect(outsiderDmDismiss.status, JSON.stringify(outsiderDmDismiss.body)).toBe(404);

    // 3. Inside the 5-minute grace window: hub returns 409 too_recent with availableAt.
    const earlyDismiss = await hubApi<{
      error?: { code?: string; details?: { availableAt?: string } };
    }>(
      hub,
      userA.accessToken,
      'POST',
      `/api/v1/decision-prompts/${encodeURIComponent(setSend.message_id)}/dismiss`,
      { reason: 'Too early' }
    );
    expect(earlyDismiss.status, JSON.stringify(earlyDismiss.body)).toBe(409);
    expect(earlyDismiss.body.error?.code).toBe('too_recent');
    const availableAtIso = earlyDismiss.body.error?.details?.availableAt ?? '';
    const availableAtMs = Date.parse(availableAtIso);
    expect(Number.isFinite(availableAtMs)).toBe(true);

    // Also wait until all 3 prompts share an availableAt target (card3DmSend was sent last).
    const card3Early = await hubApi<{
      error?: { code?: string; details?: { availableAt?: string } };
    }>(
      hub,
      userA.accessToken,
      'POST',
      `/api/v1/decision-prompts/${encodeURIComponent(card3DmSend.message_id)}/dismiss`,
      { reason: 'Too early' }
    );
    expect(card3Early.status).toBe(409);
    const lastAvailableAtMs = Date.parse(card3Early.body.error?.details?.availableAt ?? '');
    expect(Number.isFinite(lastAvailableAtMs)).toBe(true);

    const ctx1 = await openUserContext(browser, hub, userA);
    const ctx2 = await openUserContext(browser, hub, userA);
    const ctxProfile = await openUserContext(browser, hub, userA);
    try {
      const tab1 = await ctx1.newPage();
      const tab2 = await ctx2.newPage();
      const profileTab = await ctxProfile.newPage();

      const [bootTab1, bootTab2] = await Promise.all([
        openChatAndAwaitSSE(tab1, chatRoute, chatSubject),
        openChatAndAwaitSSE(tab2, chatRoute, chatSubject),
      ]);

      await profileTab.goto('/profile/decisions?tab=pending&sort=project', {
        waitUntil: 'domcontentloaded',
      });
      const profilePage = profileTab.locator('scion-page-profile-decisions');
      const profileNav = profileTab.locator('scion-profile-nav').first();
      await expect(profilePage.locator('[data-waiting-badge]')).toHaveText('6', {
        timeout: 20_000,
      });
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('6');
      const bootProfile = await awaitSSE(profileTab, ['project.>']);

      // Verify coalesced decisions_waiting bell notification starts at 6.
      await expect
        .poll(
          async () => {
            const row = await fetchDecisionsBellRow(hub, userA.accessToken);
            return row && !row.acknowledged ? row.resourceId : null;
          },
          { timeout: 15_000 }
        )
        .toBe('6');

      const setCardSelector = `scion-chat-message[id="msg-${setSend.message_id}"] scion-chat-decisions`;
      const setCardTab1 = tab1.locator(setCardSelector);
      const setCardTab2 = tab2.locator(setCardSelector);
      await expect(setCardTab1).toBeVisible({ timeout: 15_000 });
      await expect(setCardTab2).toBeVisible({ timeout: 15_000 });
      await expect(setCardTab2.locator('.counter')).toHaveText('3 remaining');

      // Verify pre-5m disabled state on both chat card menu and Profile Waiting on you checkboxes.
      await setCardTab1.locator('[data-card-menu-trigger]').click();
      const menuDsmTab1 = setCardTab1.locator('sl-menu-item[data-dismiss-menu-item]');
      await expect(menuDsmTab1).toHaveAttribute('disabled', '');
      await expect(setCardTab1.locator('sl-tooltip[data-dismiss-tooltip]')).toHaveAttribute(
        'content',
        /Dismiss available in [1-5] min/
      );
      // Close menu before waiting so it re-renders cleanly at boundary.
      await tab1.keyboard.press('Escape');

      await expect(
        profilePage.locator(`sl-checkbox[data-select-prompt="${card2Send.message_id}"]`)
      ).toHaveAttribute('disabled', '');
      await expect(
        profilePage.locator(`sl-checkbox[data-select-prompt="${card3DmSend.message_id}"]`)
      ).toHaveAttribute('disabled', '');

      // 4. Wait out the remaining portion of the single 5-minute grace window.
      const waitMs = Math.max(0, lastAvailableAtMs - Date.now() + 350);
      if (waitMs > 0) {
        await new Promise((resolve) => setTimeout(resolve, waitMs));
      }

      // Boundary timers in both open tabs unlock Dismiss without any reload!
      await expect(menuDsmTab1).not.toHaveAttribute('disabled', '', { timeout: 15_000 });
      await expect(
        profilePage.locator(`sl-checkbox[data-select-prompt="${card2Send.message_id}"]`)
      ).not.toHaveAttribute('disabled', '', { timeout: 15_000 });
      await expect(
        profilePage.locator(`sl-checkbox[data-select-prompt="${card3DmSend.message_id}"]`)
      ).not.toHaveAttribute('disabled', '', { timeout: 15_000 });
      await assertNoPageReload(tab1, bootTab1);
      await assertNoPageReload(profileTab, bootProfile);

      // 5. User dismisses 2 of the 3 rows (DSM-S1, DSM-S2) from Tab 1's chat card with a reason.
      await setCardTab1.locator('[data-card-menu-trigger]').click();
      await expect(menuDsmTab1).toBeVisible();
      await menuDsmTab1.click();

      const dialog = setCardTab1.locator('sl-dialog[data-dismiss-dialog][open]');
      await expect(dialog).toBeVisible();
      await expect(dialog.locator('sl-button[data-dismiss-confirm]')).toContainText(
        'Dismiss 3 decisions'
      );

      // Uncheck DSM-S3 so only DSM-S1 and DSM-S2 are dismissed.
      await dialog.locator(`sl-checkbox[data-dismiss-check="${s3.row_id}"]`).click();
      await expect(dialog.locator('sl-button[data-dismiss-confirm]')).toContainText(
        'Dismiss 2 decisions'
      );

      const chatReason = 'Stale: v0.9 shipped last week.';
      await dialog.locator('sl-textarea[data-dismiss-reason] textarea').fill(chatReason);
      await dialog.locator('sl-button[data-dismiss-confirm]').click();
      await expect(setCardTab1.locator('sl-dialog[data-dismiss-dialog]')).toHaveCount(0, {
        timeout: 15_000,
      });

      // 6. Second open tab (tab2) updates live via SSE without reload; counter drops; 3rd row stays answerable; chat line renders.
      for (const rowId of [s1.row_id, s2.row_id]) {
        const dsmRow = setCardTab2.locator(`[data-row="${rowId}"]`);
        await expect(dsmRow).toHaveAttribute('data-status', 'dismissed', { timeout: 15_000 });
        await expect(dsmRow.locator('.wd')).toContainText('Dismissed by you');
        await expect(dsmRow.locator('.wd-reason')).toHaveText(chatReason);
      }
      await expect(setCardTab2.locator('.counter')).toHaveText('1 remaining · 2 dismissed');
      await expect(tab2.locator('scion-chat-decision-response').first()).toContainText(
        'you dismissed 2 decisions',
        { timeout: 15_000 }
      );
      await expect(tab2.locator('scion-chat-decision-response').first()).toContainText(chatReason);
      await assertNoPageReload(tab2, bootTab2);

      // Answer the remaining 3rd row (DSM-S3) on tab2 to prove it stays answerable.
      await setCardTab2.locator(`[data-row="${s3.row_id}"] button[data-choice="no"]`).click();
      await expect(setCardTab2.locator('.counter')).toHaveText('0 remaining · 2 dismissed');
      await setCardTab2.locator('sl-button.submit').click();
      await expect(setCardTab2.locator(`[data-row="${s3.row_id}"]`)).toHaveAttribute(
        'data-status',
        'decided',
        { timeout: 15_000 }
      );

      // 7. Asking agent received the dismissal decision-response (broker delivery + agent CLI).
      const allResponses = await agent.inspectDecisionResponses(topicId, {
        authToken: userA.accessToken,
      });
      const dismissResp = allResponses.find((r) =>
        r.responseSet?.answers.some((a) => a.outcome === 'dismiss')
      );
      expect(dismissResp, JSON.stringify(allResponses)).toBeTruthy();
      const settled = await agent.waitForDecisionResponseDelivery(topicId, dismissResp!.messageId, {
        authToken: userA.accessToken,
      });
      expect(settled.attemptedAgentDelivery).toBe(true);
      expect(settled.content).toContain('dismissed 2 decisions: DSM-S1, DSM-S2');
      expect(settled.content).toContain(chatReason);
      expect(
        settled.responseSet?.answers.map((a) => `${a.key}:${a.choiceId}:${a.outcome}`)
      ).toEqual(['DSM-S1:$dismissed:dismiss', 'DSM-S2:$dismissed:dismiss']);

      const cliDismissedList = runAgentScionCli(
        hub,
        agent,
        ['decisions', 'list', '--dismissed'],
        scratchHome
      );
      expect(cliDismissedList).toContain('DSM-S1');
      expect(cliDismissedList).toContain('DSM-S2');
      expect(cliDismissedList).toContain(chatReason);

      const cliShowS1 = runAgentScionCli(hub, agent, ['decisions', 'show', 'DSM-S1'], scratchHome);
      expect(cliShowS1).toContain('v1 [dismissed]');
      expect(cliShowS1).toContain(chatReason);

      // 8. Profile Waiting on you, nav badge, and notification bell drop from 6 -> 3 live (2 dismissed + 1 answered).
      await expect(profilePage.locator('[data-waiting-badge]')).toHaveText('3', {
        timeout: 15_000,
      });
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('3');
      await expect(profilePage.locator(`[data-prompt-card="${setSend.message_id}"]`)).toHaveCount(
        0
      );
      await assertNoPageReload(profileTab, bootProfile);

      await expect
        .poll(
          async () => {
            const row = await fetchDecisionsBellRow(hub, userA.accessToken);
            return row && !row.acknowledged ? row.resourceId : null;
          },
          { timeout: 15_000 }
        )
        .toBe('3');

      // 9. Multi-select dismisses the remaining two waiting cards (DSM-P1 + 2-row DM set) on /profile/decisions.
      await profilePage
        .locator(`sl-checkbox[data-select-prompt="${card2Send.message_id}"]`)
        .click();
      await profilePage
        .locator(`sl-checkbox[data-select-prompt="${card3DmSend.message_id}"]`)
        .click();

      const selBar = profilePage.locator('[data-selection-bar]');
      await expect(selBar).toBeVisible();
      await expect(selBar.locator('[data-selected-count]')).toHaveText('2 cards selected');
      await selBar.locator('sl-button[data-selection-dismiss]').click();

      const bulkDialog = profilePage.locator('sl-dialog[data-bulk-dismiss-dialog][open]');
      await expect(bulkDialog).toBeVisible();
      const bulkReason = 'Closing remaining backlog cards.';
      await bulkDialog.locator('sl-textarea[data-bulk-dismiss-reason] textarea').fill(bulkReason);
      await bulkDialog.locator('sl-button[data-bulk-dismiss-confirm]').click();

      await expect(profilePage.locator('[data-empty="pending"]')).toBeVisible({ timeout: 20_000 });
      await expect(profilePage.locator('[data-waiting-badge]')).toHaveCount(0);
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveCount(0);

      // Bell notification clears when waitingOnYou reaches 0.
      await expect
        .poll(
          async () => {
            const row = await fetchDecisionsBellRow(hub, userA.accessToken);
            return !row || Boolean(row.acknowledged);
          },
          { timeout: 15_000 }
        )
        .toBe(true);

      // 10. Switch to Made tab and filter by Provenance = Dismissed: shows all 5 dismissed rows and excludes answered DSM-S3.
      await profileTab.goto('/profile/decisions?tab=made&sort=recent&provenance=dismissed', {
        waitUntil: 'domcontentloaded',
      });
      await expect(
        profilePage.locator('scion-decisions-filter-panel [data-filter-tag="provenance"]')
      ).toContainText('Provenance: dismissed', { timeout: 15_000 });

      const madeTable = profilePage.locator('table.log');
      await expect(madeTable).toBeVisible();
      for (const key of ['DSM-S1', 'DSM-S2', 'DSM-P1', 'DSM-DM1', 'DSM-DM2']) {
        await expect(madeTable).toContainText(key);
      }
      await expect(madeTable).toContainText(chatReason);
      await expect(madeTable).toContainText(bulkReason);
      await expect(madeTable).not.toContainText('DSM-S3');
    } finally {
      await Promise.all([ctx1.close(), ctx2.close(), ctxProfile.close()]);
    }
  });
});
