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
 * Real-hub end-to-end Playwright suite for Profile > Decisions across projects
 * (ptone/scion#7996, parent ptone/scion#7988).
 *
 * Verifies against a real Go `scion` hub with `web.decisions` enabled:
 *   1. User A is a member of two projects (`projAlpha`, `projBeta`) and NOT a
 *      member of a third project (`projGamma`) owned by User B.
 *   2. Decisions (both pending and answered) exist across all three projects,
 *      including a topic + DM prompt in `projAlpha`, a topic prompt in
 *      `projBeta`, and pending + answered topic prompts in `projGamma`.
 *   3. User A sees `<scion-profile-nav>` `Decisions` badge == 3 (2 from
 *      `projAlpha` + 1 from `projBeta`), and `projGamma` contributes nothing to
 *      counts, Waiting on you groups/cards, Made groups/rows, or filter facets.
 *   4. Switching between `By project` and `Most recent`, answering inline on
 *      `/profile/decisions` (`surface: "decisions_page"`), live nav-badge
 *      decrement, and `"See all in <project>"` drill-down all work end-to-end.
 */

import * as crypto from 'node:crypto';
import { test, expect, type Browser, type BrowserContext } from '@playwright/test';
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
  const projectRes = await hubApi<{ id?: string; project?: { id: string } }>(
    hub,
    token,
    'POST',
    '/api/v1/projects',
    { name }
  );
  expect([200, 201], JSON.stringify(projectRes.body)).toContain(projectRes.status);
  const id = projectRes.body.project?.id ?? projectRes.body.id ?? '';
  expect(id).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

async function addProjectMember(
  hub: HubState,
  adminToken: string,
  projectId: string,
  userId: string
): Promise<void> {
  const rolesRes = await hubApi<{
    items?: Array<{ id: string; name?: string; slug?: string }>;
    roles?: Array<{ id: string; name?: string; slug?: string }>;
  }>(
    hub,
    adminToken,
    'GET',
    `/api/v1/projects/${encodeURIComponent(projectId)}/members/assignable-roles`
  );
  expect(rolesRes.status).toBe(200);
  const roleList = rolesRes.body.items ?? rolesRes.body.roles ?? [];
  const memberRole = roleList.find((r) =>
    (r.slug ?? r.name ?? '').toLowerCase().includes('member')
  );
  expect(memberRole, JSON.stringify(roleList)).toBeTruthy();

  const addMemberRes = await hubApi(
    hub,
    adminToken,
    'POST',
    `/api/v1/projects/${encodeURIComponent(projectId)}/members`,
    {
      roleDefinitionId: memberRole!.id,
      principalType: 'user',
      principalId: userId,
    }
  );
  expect([200, 201], JSON.stringify(addMemberRes.body)).toContain(addMemberRes.status);
}

async function createProjectTopic(
  hub: HubState,
  token: string,
  projectId: string,
  name: string,
  defaultAgent?: string
): Promise<string> {
  const payload = defaultAgent ? { name, defaultAgent } : { name };
  const deadline = Date.now() + 5_000;
  let res = await hubApi<{ id?: string; thread?: { id: string } }>(
    hub,
    token,
    'POST',
    `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
    payload
  );
  while (res.status === 503 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    res = await hubApi<{ id?: string; thread?: { id: string } }>(
      hub,
      token,
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
  }, bootSeed);
  return context;
}

test.describe('Profile Decisions roll-up across projects on real hub (ptone/scion#7996)', () => {
  let hub!: HubState;
  let userA!: AuthSession;
  let userB!: AuthSession;
  let projAlphaId = '';
  let projBetaId = '';
  let projGammaId = '';
  let projAlphaName = '';
  let projBetaName = '';
  let projGammaName = '';
  let alphaBot!: AgentSender;
  let betaBot!: AgentSender;
  let gammaBot!: AgentSender;

  test.beforeAll(async () => {
    const loadedHub = readHubState(stateFile());
    if (!loadedHub) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loadedHub;

    const stamp = Date.now();
    userB = await createSession(hub.baseURL, {
      email: `bob-profile-owner-${stamp}@e2e.test`,
      role: 'member',
      displayName: 'Bob Owner',
    });
    userA = await createSession(hub.baseURL, {
      email: `alice-profile-rollup-${stamp}@e2e.test`,
      role: 'member',
      displayName: 'Alice Rollup',
    });

    projAlphaName = `dux-alpha-${stamp}`;
    projBetaName = `dux-beta-${stamp}`;
    projGammaName = `dux-gamma-nonmember-${stamp}`;

    projAlphaId = await createProject(hub, userB.accessToken, projAlphaName);
    projBetaId = await createProject(hub, userB.accessToken, projBetaName);
    projGammaId = await createProject(hub, userB.accessToken, projGammaName);

    // User A is a member of projAlpha and projBeta ONLY (NOT projGamma).
    await addProjectMember(hub, userB.accessToken, projAlphaId, userA.user.id);
    await addProjectMember(hub, userB.accessToken, projBetaId, userA.user.id);

    alphaBot = await createTestAgent(hub, {
      projectId: projAlphaId,
      authToken: userA.accessToken,
      name: 'alpha-bot',
      messageMode: 'project',
      ancestry: [userA.user.id],
    });
    betaBot = await createTestAgent(hub, {
      projectId: projBetaId,
      authToken: userA.accessToken,
      name: 'beta-bot',
      messageMode: 'project',
      ancestry: [userA.user.id],
    });
    gammaBot = await createTestAgent(hub, {
      projectId: projGammaId,
      authToken: userB.accessToken,
      name: 'gamma-bot',
      messageMode: 'project',
      ancestry: [userB.user.id],
    });

    await createProjectTopic(hub, userB.accessToken, projAlphaId, 'release', alphaBot.slug);
    await createProjectTopic(hub, userB.accessToken, projBetaId, 'landing', betaBot.slug);
    await createProjectTopic(hub, userB.accessToken, projGammaId, 'confidential', gammaBot.slug);
  });

  test('rolls up member projects (projAlpha + projBeta), excludes non-member projGamma completely, and updates nav badge on inline answer', async ({
    browser,
  }) => {
    const alphaRelQuestion = 'Cut alpha release tag v1.2 now?';
    const alphaDmQuestion = 'Rotate Alice alpha deploy token?';
    const betaHeroQuestion = 'Switch beta hero banner to dark mode?';
    const gammaPendingQuestion = 'Confidential acquisition budget cap?';
    const gammaMadeQuestion = 'Approve private gamma vendor contract?';

    // 1. Seed decisions in projAlpha (topic + DM with User A).
    const alphaRelSend = await alphaBot.sendDecisionPrompt({
      conversationRef: '#release',
      msg: 'Alpha release check:',
      decisions: [
        {
          id: 'D-ALPHA-REL',
          question: alphaRelQuestion,
          choices: [
            { id: 'yes', label: 'Cut tag' },
            { id: 'hold', label: 'Hold' },
          ],
          default: 'yes',
        },
      ],
    });
    expect(alphaRelSend.message_id).toBeTruthy();

    const alphaDmSend = await alphaBot.sendDecisionPrompt({
      recipient: `user:${userA.user.email}`,
      msg: 'Alpha DM check:',
      decisions: [
        {
          id: 'D-ALPHA-DM',
          question: alphaDmQuestion,
          choices: [
            { id: 'rotate', label: 'Rotate' },
            { id: 'skip', label: 'Skip' },
          ],
          default: 'rotate',
        },
      ],
    });
    expect(alphaDmSend.message_id).toBeTruthy();

    // Small pause so projBeta has a strictly newer timestamp than projAlpha for deterministic By project ordering.
    await new Promise((resolve) => setTimeout(resolve, 50));

    // 2. Seed decision in projBeta (topic).
    const betaSend = await betaBot.sendDecisionPrompt({
      conversationRef: '#landing',
      msg: 'Beta landing check:',
      decisions: [
        {
          id: 'D-BETA-HERO',
          question: betaHeroQuestion,
          choices: [
            { id: 'dark', label: 'Use dark' },
            { id: 'light', label: 'Keep light' },
          ],
          default: 'dark',
        },
      ],
    });
    expect(betaSend.message_id).toBeTruthy();

    // 3. Seed pending AND answered decisions in non-member projGamma.
    const gammaPendingSend = await gammaBot.sendDecisionPrompt({
      conversationRef: '#confidential',
      msg: 'Gamma confidential pending:',
      decisions: [
        {
          id: 'D-GAMMA-PENDING',
          question: gammaPendingQuestion,
          choices: [
            { id: 'approve', label: 'Approve' },
            { id: 'reject', label: 'Reject' },
          ],
        },
      ],
    });
    expect(gammaPendingSend.message_id).toBeTruthy();

    const gammaMadeSend = await gammaBot.sendDecisionPrompt({
      conversationRef: '#confidential',
      msg: 'Gamma confidential settled:',
      decisions: [
        {
          id: 'D-GAMMA-MADE',
          question: gammaMadeQuestion,
          choices: [
            { id: 'sign', label: 'Sign' },
            { id: 'decline', label: 'Decline' },
          ],
        },
      ],
    });
    expect(gammaMadeSend.message_id).toBeTruthy();

    // User B answers D-GAMMA-MADE so projGamma also has a Made entry in SQLite.
    const gammaListRes = await hubApi<{
      prompts?: Array<{ promptMessageId: string; decisions: Array<{ rowId: string }> }>;
    }>(
      hub,
      userB.accessToken,
      'GET',
      `/api/v1/projects/${encodeURIComponent(projGammaId)}/decisions?tab=pending`
    );
    expect(gammaListRes.status).toBe(200);
    const gammaMadeRowId =
      gammaListRes.body.prompts?.find((p) => p.promptMessageId === gammaMadeSend.message_id)
        ?.decisions[0]?.rowId ?? '';
    expect(gammaMadeRowId).toBeTruthy();
    const gammaAnswerRes = await hubApi(
      hub,
      userB.accessToken,
      'POST',
      `/api/v1/decisions/${encodeURIComponent(gammaMadeRowId)}/answer`,
      { choice_id: 'sign', surface: 'decisions_page' }
    );
    expect(gammaAnswerRes.status).toBe(200);

    // 4. Open User A in Chromium on /profile/env -> verify Profile > Decisions nav entry + badge == 3.
    const ctxA = await openUserContext(browser, hub, userA);
    try {
      const pageA = await ctxA.newPage();
      let limitZeroProbeRequests = 0;
      pageA.on('request', (req) => {
        if (req.method() === 'GET' && req.url().includes('/api/v1/users/me/decisions?limit=0')) {
          limitZeroProbeRequests++;
        }
      });
      await pageA.goto('/profile/env', { waitUntil: 'domcontentloaded' });

      const profileNav = pageA.locator('scion-profile-nav').first();
      const navLink = profileNav.locator('[data-decisions-nav-link]');
      await expect(navLink).toBeVisible({ timeout: 20_000 });
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('3');
      // Dual <scion-profile-nav> instances in <scion-profile-shell> share a single ?limit=0 probe!
      expect(limitZeroProbeRequests).toBe(1);

      // Navigate via the Profile > Decisions nav link -> ?tab=pending&sort=project
      await navLink.click();
      await expect(pageA).toHaveURL(/\/profile\/decisions\?tab=pending&sort=project$/);
      // Route change reuses cached probe rather than re-probing ?limit=0
      expect(limitZeroProbeRequests).toBe(1);

      const decisionsPage = pageA.locator('scion-page-profile-decisions');
      await expect(decisionsPage.locator('[data-waiting-badge]')).toHaveText('3');

      // projBeta (newest prompt) comes before projAlpha; projGamma is completely absent!
      const groupIds = await decisionsPage
        .locator('[data-pending-group]')
        .evaluateAll((els) => els.map((el) => el.getAttribute('data-pending-group')));
      expect(groupIds).toEqual([projBetaId, projAlphaId]);
      expect(groupIds).not.toContain(projGammaId);

      await expect(decisionsPage).toContainText(betaHeroQuestion);
      await expect(decisionsPage).toContainText(alphaRelQuestion);
      await expect(decisionsPage).toContainText(alphaDmQuestion);
      await expect(decisionsPage).not.toContainText(projGammaName);
      await expect(decisionsPage).not.toContainText(gammaPendingQuestion);
      await expect(decisionsPage).not.toContainText('D-GAMMA-PENDING');

      // Filter box suggestions -> verify project: has ONLY projAlpha + projBeta (never projGamma)
      const panel = decisionsPage.locator('scion-decisions-filter-panel');
      const filterInput = panel.locator('scion-filter-box input');
      await filterInput.click();
      await filterInput.fill('project:');
      const projOptionTexts = await panel
        .locator('scion-filter-box [role="option"]')
        .allTextContents();
      expect(projOptionTexts.join(' ')).toContain(projAlphaName);
      expect(projOptionTexts.join(' ')).toContain(projBetaName);
      expect(projOptionTexts.join(' ')).not.toContain(projGammaName);

      // agent: suggestions carry muted project hints from P4's agents[].projectId and exclude gamma-bot
      await filterInput.fill('agent:');
      const agentOptionTexts = await panel
        .locator('scion-filter-box [role="option"]')
        .allTextContents();
      const joinedAgentOptions = agentOptionTexts.join(' | ');
      expect(joinedAgentOptions).toContain('alpha-bot');
      expect(joinedAgentOptions).toContain(projAlphaName);
      expect(joinedAgentOptions).toContain('beta-bot');
      expect(joinedAgentOptions).toContain(projBetaName);
      expect(joinedAgentOptions).not.toContain('gamma-bot');
      expect(joinedAgentOptions).not.toContain(projGammaName);
      await filterInput.fill('');
      await filterInput.press('Escape');

      // Switch sort to Most recent -> single chronological list with project badges on cards
      await decisionsPage.locator('[data-sort-option="recent"]').click();
      await expect(pageA).toHaveURL(/\/profile\/decisions\?tab=pending&sort=recent$/);
      await expect(decisionsPage.locator('[data-card-project]')).toHaveText([
        projBetaName,
        projAlphaName,
        projAlphaName,
      ]);

      // 5. Answer projBeta's decision inline -> records surface="decisions_page" and updates nav badge 3 -> 2
      const answerReqPromise = pageA.waitForRequest(
        (req) => req.method() === 'POST' && /\/api\/v1\/decisions\/[^/]+\/answer$/.test(req.url())
      );
      const answerResPromise = pageA.waitForResponse(
        (res) =>
          res.request().method() === 'POST' &&
          /\/api\/v1\/decisions\/[^/]+\/answer$/.test(res.url())
      );

      const betaCard = decisionsPage.locator(`[data-prompt-card="${betaSend.message_id}"]`);
      await betaCard.locator('button[data-choice="dark"]').click();

      const [answerReq, answerRes] = await Promise.all([answerReqPromise, answerResPromise]);
      expect(answerRes.status()).toBe(200);
      expect(answerReq.postDataJSON()).toMatchObject({
        choice_id: 'dark',
        surface: 'decisions_page',
      });

      await expect(betaCard).toHaveCount(0);
      await expect(decisionsPage.locator('[data-waiting-badge]')).toHaveText('2');
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('2');

      // 6. Switch to Made tab (defaults to Most recent) -> projBeta answer appears with Project column & Decisions page surface; projGamma is absent!
      await decisionsPage.locator('sl-tab[data-tab="made"]').click();
      await expect(pageA).toHaveURL(/\/profile\/decisions\?tab=made&sort=recent$/);
      const madeTable = decisionsPage.locator('table.log[data-made-table]');
      await expect(madeTable).toBeVisible();
      await expect(madeTable).toContainText(projBetaName);
      await expect(madeTable).toContainText(betaHeroQuestion);
      await expect(madeTable).toContainText('Use dark');
      await expect(madeTable).toContainText('Decisions page');
      await expect(madeTable).not.toContainText(projGammaName);
      await expect(madeTable).not.toContainText(gammaMadeQuestion);
      await expect(madeTable).not.toContainText('D-GAMMA-MADE');

      // 7. Switch Made sort to By project -> overview shows projBeta (never projGamma), and "See all in <projBeta>" drills down!
      await decisionsPage.locator('[data-sort-option="project"]').click();
      await expect(pageA).toHaveURL(/\/profile\/decisions\?tab=made&sort=project$/);
      await expect(decisionsPage.locator(`[data-made-group="${projBetaId}"]`)).toBeVisible();
      await expect(decisionsPage.locator(`[data-made-group="${projGammaId}"]`)).toHaveCount(0);

      await decisionsPage.locator(`[data-see-all-project="${projBetaId}"]`).click();
      await expect(pageA).toHaveURL(/\/profile\/decisions\?tab=made&sort=recent&q=project%3A/);
      await expect(panel.locator('scion-filter-box input')).toHaveValue(/^project:/);
    } finally {
      await ctxA.close();
    }
  });
});
