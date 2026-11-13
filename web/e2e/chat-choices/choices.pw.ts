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
 * Real-hub end-to-end Playwright suite for interactive chat choices
 * (ptone/scion#7776, epic ptone/scion#7514).
 *
 * Runs against the real Go `scion` hub binary with `web.chat_choices` enabled
 * and a containerless test agent (`../harness/agent.ts`) backed by the
 * co-located runtime broker — no Docker daemon or live agent container needed.
 */

import * as crypto from 'node:crypto';
import { test, expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { canonicalAgentUserDMKey, createTestAgent, type AgentSender } from '../harness/agent.js';
import { createSession, type AuthSession } from '../harness/auth.js';
import { readHubState, type HubState } from '../harness/hub.js';
import { stateFile } from './state.js';

interface TrackedEventSourceInfo {
  url: string;
  readyState: number;
}

declare global {
  interface Window {
    __e2eBootId?: string;
    __e2eEventSources?: EventSource[];
  }
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

async function createProjectTopic(hub: HubState, projectId: string, name: string): Promise<string> {
  const res = await hubApi<{ id: string }>(
    hub,
    hub.devToken,
    'POST',
    `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
    { name }
  );
  expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
  expect(res.body.id).toMatch(/^[0-9a-f-]{36}$/);
  return res.body.id;
}

/**
 * Creates an authenticated browser context with a non-intrusive observer script
 * that stamps a per-navigation boot ID (`window.__e2eBootId`, proving live SSE
 * updates occurred without a page reload) and records `EventSource` instances
 * so tests can await stream readiness without arbitrary sleeps.
 */
async function openUserContext(
  browser: Browser,
  hub: HubState,
  session: AuthSession
): Promise<{ context: BrowserContext; page: Page }> {
  const bootSeed = crypto.randomUUID();
  const context = await browser.newContext({
    baseURL: hub.baseURL,
    storageState: session.storageStatePath,
  });
  await context.addInitScript((seed: string) => {
    let navSeq = 0;
    navSeq += 1;
    window.__e2eBootId = `${seed}:${navSeq}:${performance.timeOrigin}`;
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
  const page = await context.newPage();
  return { context, page };
}

/**
 * Navigates to a chat route (`domcontentloaded` — never `networkidle` because
 * SSE stays open) and waits until `<scion-chat-thread>` is visible and an
 * `EventSource` subscribed to `requiredSubject` is open (`readyState === 1`).
 */
async function openChatAndAwaitSSE(
  page: Page,
  routePath: string,
  requiredSubject: string
): Promise<string> {
  await page.goto(routePath, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('scion-chat-thread')).toBeVisible({ timeout: 20_000 });

  await expect
    .poll(
      async () => {
        const sources = await page.evaluate((): TrackedEventSourceInfo[] => {
          return (window.__e2eEventSources ?? []).map((es) => ({
            url: es.url,
            readyState: es.readyState,
          }));
        });
        return sources.some((s) => {
          if (s.readyState !== 1) return false;
          const decoded = decodeURIComponent(s.url);
          return decoded.includes('/events') && decoded.includes(requiredSubject);
        });
      },
      {
        timeout: 20_000,
        message: `Expected an open SSE stream subscribed to ${requiredSubject}`,
      }
    )
    .toBe(true);

  const bootId = await page.evaluate(() => window.__e2eBootId ?? '');
  expect(bootId).not.toBe('');
  return bootId;
}

async function assertNoPageReload(page: Page, expectedBootId: string): Promise<void> {
  const current = await page.evaluate(() => window.__e2eBootId ?? '');
  expect(current).toBe(expectedBootId);
}

test.describe('Interactive chat choices on real hub (web.chat_choices)', () => {
  let hub!: HubState;
  let projectId = '';
  let userA!: AuthSession;
  let userB!: AuthSession;
  let agent!: AgentSender;

  test.beforeAll(async () => {
    const loadedHub = readHubState(stateFile());
    if (!loadedHub) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loadedHub;

    const projectRes = await hubApi<{ id: string }>(hub, hub.devToken, 'POST', '/api/v1/projects', {
      name: `choices-e2e-${Date.now()}`,
    });
    expect(projectRes.status, JSON.stringify(projectRes.body)).toBe(201);
    projectId = projectRes.body.id;

    userA = await createSession(hub.baseURL, {
      email: 'alice-choices@e2e.test',
      role: 'member',
      displayName: 'Alice Choices',
    });
    userB = await createSession(hub.baseURL, {
      email: 'bob-choices@e2e.test',
      role: 'member',
      displayName: 'Bob Choices',
    });

    const rolesRes = await hubApi<{ items?: Array<{ id: string; name: string }> }>(
      hub,
      hub.devToken,
      'GET',
      `/api/v1/projects/${encodeURIComponent(projectId)}/members/assignable-roles`
    );
    expect(rolesRes.status).toBe(200);
    // project-admin grants agent.message (upstream removed agent.message from project-member).
    const answererRole = (rolesRes.body.items ?? []).find((r) => r.name === 'project-admin');
    expect(answererRole).toBeTruthy();

    for (const user of [userA, userB]) {
      const addRes = await hubApi(
        hub,
        hub.devToken,
        'POST',
        `/api/v1/projects/${encodeURIComponent(projectId)}/members`,
        {
          roleDefinitionId: answererRole!.id,
          principalType: 'user',
          principalId: user.user.email,
        }
      );
      expect([200, 201], JSON.stringify(addRes.body)).toContain(addRes.status);
    }

    agent = await createTestAgent(hub, {
      projectId,
      name: 'choices-asker',
      messageMode: 'project',
    });
  });

  test('containerless agent fixture authenticates on both RouteAgentSelf and RouteAgentToken', async () => {
    const unauthRes = await fetch(`${hub.baseURL}/api/v1/agent/gcp-token`, {
      signal: AbortSignal.timeout(10_000),
    });
    expect(unauthRes.status).toBe(401);

    // /api/v1/agent/gcp-token is classified as RouteAgentToken in route_metadata.go.
    // With a valid X-Scion-Agent-Token, auth middleware admits AgentIdentity and
    // reaches the handler (which refuses with a non-401 status because no GCP
    // service account is assigned to this test agent).
    const agentTokenRouteRes = await agent.request('/api/v1/agent/gcp-token');
    expect(agentTokenRouteRes.status).not.toBe(401);
    expect(agentTokenRouteRes.status).not.toBe(404);

    // /api/v1/agents/{id}/outbound-message is classified as RouteAgentSelf.
    const smokeTopicId = await createProjectTopic(hub, projectId, 'choices-fixture-smoke');
    const msgRes = await agent.sendMessage({
      conversationRef: '#choices-fixture-smoke',
      msg: 'Containerless test agent ready.',
    });
    expect(msgRes.message_id).toMatch(/^[0-9a-f-]{36}$/);
    expect(smokeTopicId).toMatch(/^[0-9a-f-]{36}$/);
  });

  test('single-decision prompt renders live over SSE in DM and transitions to decided on click', async ({
    browser,
  }) => {
    const dmKey = canonicalAgentUserDMKey(agent.id, userA.user.id);
    // Seed initial DM message so the direct conversation exists in User A's rail/thread view.
    await agent.sendMessage({
      recipient: `user:${userA.user.email}`,
      msg: 'Ready to deploy once you confirm the strategy.',
    });

    const { context, page } = await openUserContext(browser, hub, userA);
    try {
      const bootId = await openChatAndAwaitSSE(
        page,
        `/chat/dm/${encodeURIComponent(dmKey)}`,
        `user.${userA.user.id}.chat.>`
      );

      const sendRes = await agent.sendChoicePrompt({
        recipient: `user:${userA.user.email}`,
        msg: 'Choose deployment strategy:',
        choices: [
          {
            id: 'D-SINGLE',
            question: 'Which rollout strategy should we use?',
            choices: [
              { id: 'canary', label: 'Canary 10%' },
              { id: 'bluegreen', label: 'Blue/Green' },
            ],
            default: 'canary',
          },
        ],
      });
      expect(sendRes.decisions).toHaveLength(1);
      const decision = sendRes.decisions![0];
      expect(decision.key).toBe('D-SINGLE');
      expect(decision.version).toBe(1);

      const promptCard = page
        .locator(`scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices`)
        .first();
      await expect(promptCard).toBeVisible({ timeout: 15_000 });
      await assertNoPageReload(page, bootId);

      const row = promptCard.locator(`[data-row="${decision.row_id}"]`);
      await expect(row).toHaveAttribute('data-status', 'pending');
      await expect(row).toContainText('Which rollout strategy should we use?');
      await expect(row).toContainText('Awaiting your answer');

      await row.locator('sl-button[data-choice="bluegreen"]').click();

      await expect(row).toHaveAttribute('data-status', 'decided', { timeout: 15_000 });
      await expect(row.locator('.row-foot')).toContainText('Answered by you');
      await expect(row.locator('sl-button[data-choice="canary"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );

      const responseLine = page.locator('scion-chat-choice-response [data-kind="single"]').last();
      await expect(responseLine).toBeVisible({ timeout: 15_000 });
      await expect(responseLine).toContainText('you chose Blue/Green for');
      await expect(responseLine).toContainText('D-SINGLE');
      await assertNoPageReload(page, bootId);

      const delivery = await agent.waitForChoiceResponseDelivery(
        dmKey,
        { promptMessageId: sendRes.message_id },
        { authToken: userA.accessToken }
      );
      expect(delivery.attemptedAgentDelivery).toBe(true);
      expect(delivery.deliveryClassification).toBe('attempted_delivery');
      expect(delivery.response?.choiceId).toBe('bluegreen');
    } finally {
      await context.close();
    }
  });

  test('multi-user group conversation updates User B live over SSE when User A answers', async ({
    browser,
  }) => {
    const topicName = 'choices-group';
    const topicId = await createProjectTopic(hub, projectId, topicName);
    const routePath = `/chat/space/${projectId}/thread/${topicId}`;
    const projectSubject = `project.${projectId}.chat.>`;

    const sessionA = await openUserContext(browser, hub, userA);
    const sessionB = await openUserContext(browser, hub, userB);
    try {
      const [bootIdA, bootIdB] = await Promise.all([
        openChatAndAwaitSSE(sessionA.page, routePath, projectSubject),
        openChatAndAwaitSSE(sessionB.page, routePath, projectSubject),
      ]);

      const sendRes = await agent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Database engine decision for the team:',
        choices: [
          {
            id: 'D-GROUP',
            question: 'Select the primary database engine',
            choices: [
              { id: 'postgres', label: 'PostgreSQL' },
              { id: 'sqlite', label: 'SQLite' },
            ],
          },
        ],
      });
      const rowId = sendRes.decisions![0].row_id;

      const cardA = sessionA.page.locator(
        `scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices`
      );
      const cardB = sessionB.page.locator(
        `scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices`
      );
      await expect(cardA.locator(`[data-row="${rowId}"]`)).toHaveAttribute(
        'data-status',
        'pending',
        { timeout: 15_000 }
      );
      await expect(cardB.locator(`[data-row="${rowId}"]`)).toHaveAttribute(
        'data-status',
        'pending',
        { timeout: 15_000 }
      );
      await expect(cardB.locator(`[data-row="${rowId}"]`)).toContainText('Awaiting your answer');

      // User A clicks PostgreSQL.
      await cardA.locator(`[data-row="${rowId}"] sl-button[data-choice="postgres"]`).click();

      // User A sees their own decided state.
      await expect(cardA.locator(`[data-row="${rowId}"]`)).toHaveAttribute(
        'data-status',
        'decided',
        { timeout: 15_000 }
      );
      await expect(cardA.locator(`[data-row="${rowId}"] .row-foot`)).toContainText(
        'Answered by you'
      );

      // User B sees the live SSE update without reloading.
      const rowB = cardB.locator(`[data-row="${rowId}"]`);
      await expect(rowB).toHaveAttribute('data-status', 'decided', { timeout: 15_000 });
      await expect(rowB.locator('.row-foot')).toContainText('Answered by Alice Choices');
      await expect(rowB.locator('sl-button[data-choice="sqlite"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );

      const responseLineB = sessionB.page
        .locator('scion-chat-choice-response [data-kind="single"]')
        .last();
      await expect(responseLineB).toBeVisible({ timeout: 15_000 });
      await expect(responseLineB).toContainText('Alice Choices chose PostgreSQL for');
      await expect(responseLineB).toContainText('D-GROUP');

      await assertNoPageReload(sessionA.page, bootIdA);
      await assertNoPageReload(sessionB.page, bootIdB);
    } finally {
      await Promise.all([sessionA.context.close(), sessionB.context.close()]);
    }
  });

  test('multi-decision set card selects rows and submits atomically via Submit answers', async ({
    browser,
  }) => {
    const topicName = 'choices-set';
    const topicId = await createProjectTopic(hub, projectId, topicName);

    const { context, page } = await openUserContext(browser, hub, userA);
    try {
      const bootId = await openChatAndAwaitSSE(
        page,
        `/chat/space/${projectId}/thread/${topicId}`,
        `project.${projectId}.chat.>`
      );

      const sendRes = await agent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Configure release target:',
        choices: [
          {
            id: 'D-REGION',
            question: 'Which target region should receive the canary?',
            choices: [
              { id: 'us', label: 'US Central' },
              { id: 'eu', label: 'EU West' },
            ],
          },
          {
            id: 'D-CACHE',
            question: 'Should edge caching be enabled?',
            choices: [
              { id: 'on', label: 'Enabled' },
              { id: 'off', label: 'Disabled' },
            ],
          },
        ],
      });
      expect(sendRes.decisions).toHaveLength(2);
      const regionRowId = sendRes.decisions![0].row_id;
      const cacheRowId = sendRes.decisions![1].row_id;

      const setCard = page.locator(
        `scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices .card.set`
      );
      await expect(setCard).toBeVisible({ timeout: 15_000 });
      await expect(setCard.locator('.card-head')).toContainText('2 decisions');
      await expect(setCard.locator('.card-foot .counter')).toContainText('2 remaining');
      await expect(setCard.locator('sl-button.submit')).toHaveAttribute('aria-disabled', 'true');

      const regionRow = setCard.locator(`[data-row="${regionRowId}"]`);
      const cacheRow = setCard.locator(`[data-row="${cacheRowId}"]`);

      await regionRow.locator('button[data-choice="eu"]').click();
      await expect(regionRow.locator('button[data-choice="eu"]')).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(setCard.locator('.card-foot .counter')).toContainText('1 remaining');

      await cacheRow.locator('button[data-choice="on"]').click();
      await expect(cacheRow.locator('button[data-choice="on"]')).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(setCard.locator('.card-foot .counter')).toContainText('0 remaining');

      await setCard.locator('sl-button.submit').click();

      await expect(regionRow).toHaveAttribute('data-status', 'decided', { timeout: 15_000 });
      await expect(cacheRow).toHaveAttribute('data-status', 'decided', { timeout: 15_000 });
      await expect(setCard.locator('.card-foot .counter')).toContainText('Submitted by you');

      const setResponse = page
        .locator('scion-chat-choice-response details[data-kind="set"]')
        .last();
      await expect(setResponse).toBeVisible({ timeout: 15_000 });
      await expect(setResponse.locator('summary')).toContainText('you answered 2 decisions');

      await setResponse.locator('summary').click();
      await expect(setResponse.locator(`li[data-row="${regionRowId}"]`)).toContainText('EU West');
      await expect(setResponse.locator(`li[data-row="${cacheRowId}"]`)).toContainText('Enabled');

      await assertNoPageReload(page, bootId);

      const delivery = await agent.waitForChoiceResponseDelivery(
        topicId,
        { promptMessageId: sendRes.message_id },
        { authToken: userA.accessToken }
      );
      expect(delivery.attemptedAgentDelivery).toBe(true);
      expect(delivery.responseSet?.answers).toHaveLength(2);
    } finally {
      await context.close();
    }
  });

  test('replacement with choicesUpdate flips open v1 card live over SSE to Replaced by v2', async ({
    browser,
  }) => {
    const topicName = 'choices-replace';
    const topicId = await createProjectTopic(hub, projectId, topicName);

    const { context, page } = await openUserContext(browser, hub, userA);
    try {
      const bootId = await openChatAndAwaitSSE(
        page,
        `/chat/space/${projectId}/thread/${topicId}`,
        `project.${projectId}.chat.>`
      );

      const v1Res = await agent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Release candidate check (v1):',
        choices: [
          {
            id: 'D-REL',
            question: 'Cut release candidate v1 now?',
            choices: [
              { id: 'ship', label: 'Ship now' },
              { id: 'hold', label: 'Hold' },
            ],
          },
        ],
      });
      expect(v1Res.decisions).toHaveLength(1);
      const v1Decision = v1Res.decisions![0];
      expect(v1Decision.version).toBe(1);

      const v1Card = page.locator(
        `scion-chat-message[id="msg-${v1Res.message_id}"] scion-chat-choices`
      );
      const v1Row = v1Card.locator(`[data-row="${v1Decision.row_id}"]`);
      await expect(v1Row).toHaveAttribute('data-status', 'pending', { timeout: 15_000 });

      // Agent re-sends D-REL with choicesUpdate: true using the resolved conversationId.
      expect(v1Res.conversation_id).toMatch(/^[0-9a-f-]{36}$/);
      const v2Res = await agent.sendChoicePrompt({
        conversationId: v1Res.conversation_id!,
        msg: 'Release candidate check (v2):',
        choicesUpdate: true,
        choices: [
          {
            id: 'D-REL',
            question: 'Cut release candidate v2 after hotfix?',
            choices: [
              { id: 'ship_v2', label: 'Ship v2' },
              { id: 'hold_v2', label: 'Hold v2' },
            ],
          },
        ],
      });
      expect(v2Res.decisions).toHaveLength(1);
      const v2Decision = v2Res.decisions![0];
      expect(v2Decision.version).toBe(2);
      expect(v2Decision.replaced?.version).toBe(1);

      // Open v1 card flips live over SSE to replaced ("Replaced by v2").
      await expect(v1Row).toHaveAttribute('data-status', 'replaced', { timeout: 15_000 });
      await expect(v1Row.locator('.row-foot')).toContainText('Replaced by v2');
      await expect(v1Row.locator('a.jump')).toContainText('Jump to v2');
      await expect(v1Row.locator('sl-button[data-choice="ship"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );

      // New v2 card renders live with pending status and v2 badge.
      const v2Card = page.locator(
        `scion-chat-message[id="msg-${v2Res.message_id}"] scion-chat-choices`
      );
      const v2Row = v2Card.locator(`[data-row="${v2Decision.row_id}"]`);
      await expect(v2Row).toHaveAttribute('data-status', 'pending', { timeout: 15_000 });
      await expect(v2Card.locator('.version')).toContainText('v2');
      await expect(v2Row).toContainText('Cut release candidate v2 after hotfix?');

      await assertNoPageReload(page, bootId);
    } finally {
      await context.close();
    }
  });
});
