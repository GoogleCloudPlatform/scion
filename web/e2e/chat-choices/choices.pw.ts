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
  const deadline = Date.now() + 5_000;
  let res = await hubApi<{ id: string }>(
    hub,
    hub.devToken,
    'POST',
    `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
    { name }
  );
  // On fast CI cold boots, /healthz can return 200 ~100ms before the chat
  // subsystem finishes initializing (503 SERVICE_UNAVAILABLE).
  while (res.status === 503 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    res = await hubApi<{ id: string }>(
      hub,
      hub.devToken,
      'POST',
      `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
      { name }
    );
  }
  expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
  expect(res.body.id).toMatch(/^[0-9a-f-]{36}$/);
  return res.body.id;
}

async function sendUserTopicMessage(
  hub: HubState,
  accessToken: string,
  topicId: string,
  content: string
): Promise<string> {
  const res = await hubApi<{ id: string }>(
    hub,
    accessToken,
    'POST',
    `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`,
    { content }
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

  test('multi-decision set card selects rows and submits both answers together via Submit answers', async ({
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

  test('Phase 2 set card: interpreted answers flip User A to will confirm and User B to read-only live over SSE, then Submit answers converges', async ({
    browser,
  }) => {
    const topicName = 'choices-p2-set';
    const topicId = await createProjectTopic(hub, projectId, topicName);
    const routePath = `/chat/space/${projectId}/thread/${topicId}`;
    const projectSubject = `project.${projectId}.chat.>`;

    const setAgent = await createTestAgent(hub, {
      projectId,
      name: 'choices-set-p2',
      messageMode: 'project',
    });

    const sessionA = await openUserContext(browser, hub, userA);
    const sessionB = await openUserContext(browser, hub, userB);
    try {
      const [bootIdA, bootIdB] = await Promise.all([
        openChatAndAwaitSSE(sessionA.page, routePath, projectSubject),
        openChatAndAwaitSSE(sessionB.page, routePath, projectSubject),
      ]);

      const sendRes = await setAgent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Release checklist decisions:',
        choices: [
          {
            id: 'D234',
            question: 'Ship v0.9 today?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
            // Default is 'no' so interpreted 'yes' proves interpreted beats default.
            default: 'no',
          },
          {
            id: 'D17',
            question: 'Which approach?',
            choices: [
              { id: 'a', label: 'Extend input-needed' },
              { id: 'b', label: 'New waiting state' },
            ],
          },
          {
            id: 'ship-now',
            question: 'Deploy after tagging?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
          },
        ],
      });
      expect(sendRes.decisions).toHaveLength(3);
      const d234RowId = sendRes.decisions![0].row_id;
      const q17RowId = sendRes.decisions![1].row_id;
      const shipRowId = sendRes.decisions![2].row_id;

      const cardA = sessionA.page.locator(
        `scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices .card.set`
      );
      const cardB = sessionB.page.locator(
        `scion-chat-message[id="msg-${sendRes.message_id}"] scion-chat-choices .card.set`
      );
      await expect(cardA).toBeVisible({ timeout: 15_000 });
      await expect(cardB).toBeVisible({ timeout: 15_000 });

      // User A posts a free-text reply; agent records interpreted answers for D234 and 17.
      const sourceMessageId = await sendUserTopicMessage(
        hub,
        userA.accessToken,
        topicId,
        'D234: yes, 17: a'
      );

      const interpRes = await setAgent.request<{
        response_message_id: string;
        batch_id: string;
      }>('/api/v1/agent/decisions/interpreted', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          source_message_id: sourceMessageId,
          answers: [
            { key: 'D234', choice_id: 'yes' },
            { key: 'D17', choice_id: 'a' },
          ],
        }),
      });
      expect(interpRes.status, interpRes.text).toBe(200);

      // User B sees Alice's interpreted rows flip to read-only live over SSE,
      // while ship-now stays pending for Bob.
      const rowB234 = cardB.locator(`[data-row="${d234RowId}"]`);
      const rowB17 = cardB.locator(`[data-row="${q17RowId}"]`);
      const rowBShip = cardB.locator(`[data-row="${shipRowId}"]`);
      await expect(rowB234).toHaveClass(/readonly/, { timeout: 15_000 });
      await expect(rowB17).toHaveClass(/readonly/, { timeout: 15_000 });
      await expect(rowB234.locator('.chip.interp')).toHaveText("Alice Choices's answer");
      await expect(rowB234.locator('sl-tooltip')).toHaveAttribute(
        'content',
        'Only Alice Choices can confirm or change this answer'
      );
      await expect(rowB234.locator('button[data-choice="yes"]')).toBeDisabled();
      await expect(rowB234.locator('.row-foot.interpreted')).toContainText(
        "Interpreted by choices-set-p2 from Alice Choices's reply"
      );
      await expect(rowBShip).toHaveAttribute('data-status', 'pending');
      await expect(cardB.locator('.card-foot .counter')).toHaveText(
        '1 remaining · 2 answered by Alice Choices'
      );

      // Live SSE interpreted set response line attributes Alice Choices (not agent sender).
      const interpSetLineB = sessionB.page
        .locator(
          `scion-chat-message[id="msg-${interpRes.body.response_message_id}"] scion-chat-choice-response details[data-kind="set"] summary`
        )
        .first();
      await expect(interpSetLineB).toContainText(
        "choices-set-p2 recorded Alice Choices's answers to 2 decisions from their reply",
        { timeout: 15_000 }
      );

      // User A sees D234 and 17 flip live over SSE to interactive pre-selected 'will confirm'.
      const rowA234 = cardA.locator(`[data-row="${d234RowId}"]`);
      const rowA17 = cardA.locator(`[data-row="${q17RowId}"]`);
      const rowAShip = cardA.locator(`[data-row="${shipRowId}"]`);
      await expect(rowA234.locator('.chip.confirm')).toHaveText('will confirm', {
        timeout: 15_000,
      });
      await expect(rowA234.locator('button[data-choice="yes"]')).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(rowA17.locator('.chip.confirm')).toHaveText('will confirm');
      await expect(rowA17.locator('button[data-choice="a"]')).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(cardA.locator('.card-foot .counter')).toHaveText('1 remaining');

      // User A changes 17 to 'b' (will change) and picks 'yes' on pending ship-now.
      await rowA17.locator('button[data-choice="b"]').click();
      await rowAShip.locator('button[data-choice="yes"]').click();
      await expect(rowA17.locator('.chip.change')).toHaveText('will change');
      await expect(cardA.locator('.card-foot .counter')).toHaveText(
        '3 of 3 ready · 1 confirm · 1 change'
      );

      await cardA.locator('sl-button.submit').click();

      // Both User A and User B converge live over SSE to direct decided states.
      await expect(rowA234.locator('.row-foot')).toContainText('Answered by you (confirmed)', {
        timeout: 15_000,
      });
      await expect(rowA17.locator('.row-foot')).toContainText(
        'changed from the interpreted answer Extend input-needed'
      );
      await expect(rowAShip.locator('.row-foot')).toContainText('Answered by you');
      await expect(cardA.locator('.card-foot .counter')).toContainText('Submitted by you');

      await expect(rowB234.locator('.row-foot')).toContainText(
        'Answered by Alice Choices (confirmed)',
        { timeout: 15_000 }
      );
      await expect(rowB17.locator('.row-foot')).toContainText(
        'changed from the interpreted answer Extend input-needed'
      );
      await expect(rowBShip.locator('.row-foot')).toContainText('Answered by Alice Choices');

      await assertNoPageReload(sessionA.page, bootIdA);
      await assertNoPageReload(sessionB.page, bootIdB);

      const delivery = await setAgent.waitForChoiceResponseDelivery(
        topicId,
        { promptMessageId: sendRes.message_id },
        { authToken: userA.accessToken }
      );
      expect(delivery.deliveryClassification).toBe('attempted_delivery');
      expect(delivery.attemptedAgentDelivery).toBe(true);
    } finally {
      await Promise.all([sessionA.context.close(), sessionB.context.close()]);
    }
  });

  test('Phase 2 single cards: confirm stays logged_without_delivery while override attempts delivery and preserves previousChoiceLabel on reload', async ({
    browser,
  }) => {
    const topicName = 'choices-p2-single';
    const topicId = await createProjectTopic(hub, projectId, topicName);
    const routePath = `/chat/space/${projectId}/thread/${topicId}`;
    const projectSubject = `project.${projectId}.chat.>`;

    const singleAgent = await createTestAgent(hub, {
      projectId,
      name: 'choices-single-p2',
      messageMode: 'project',
    });

    const sessionA = await openUserContext(browser, hub, userA);
    const sessionB = await openUserContext(browser, hub, userB);
    try {
      const [bootIdA, bootIdB] = await Promise.all([
        openChatAndAwaitSSE(sessionA.page, routePath, projectSubject),
        openChatAndAwaitSSE(sessionB.page, routePath, projectSubject),
      ]);

      const promptConf = await singleAgent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Staging rollout confirmation:',
        choices: [
          {
            id: 'S-CONF',
            question: 'Confirm staging deploy?',
            choices: [
              { id: 'yes', label: 'Yes' },
              { id: 'no', label: 'No' },
            ],
          },
        ],
      });
      const promptOver = await singleAgent.sendChoicePrompt({
        conversationRef: `#${topicName}`,
        msg: 'Canary duration check:',
        choices: [
          {
            id: 'S-OVER',
            question: 'Keep canary window at 5m?',
            choices: [
              { id: '5m', label: '5 minutes' },
              { id: '30m', label: '30 minutes' },
            ],
          },
        ],
      });

      const cardConfA = sessionA.page.locator(
        `scion-chat-message[id="msg-${promptConf.message_id}"] scion-chat-choices`
      );
      const cardOverA = sessionA.page.locator(
        `scion-chat-message[id="msg-${promptOver.message_id}"] scion-chat-choices`
      );
      const cardConfB = sessionB.page.locator(
        `scion-chat-message[id="msg-${promptConf.message_id}"] scion-chat-choices`
      );
      const cardOverB = sessionB.page.locator(
        `scion-chat-message[id="msg-${promptOver.message_id}"] scion-chat-choices`
      );
      await expect(cardConfA).toBeVisible({ timeout: 15_000 });
      await expect(cardOverA).toBeVisible({ timeout: 15_000 });

      const sourceMessageId = await sendUserTopicMessage(
        hub,
        userA.accessToken,
        topicId,
        'Yes on staging, 5m on canary.'
      );
      const interpRes = await singleAgent.request('/api/v1/agent/decisions/interpreted', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          source_message_id: sourceMessageId,
          answers: [
            { key: 'S-CONF', choice_id: 'yes' },
            { key: 'S-OVER', choice_id: '5m' },
          ],
        }),
      });
      expect(interpRes.status, interpRes.text).toBe(200);

      await expect(cardConfA.locator('.card-head .badge.interp')).toContainText('interpreted', {
        timeout: 15_000,
      });
      await expect(cardOverA.locator('.card-head .badge.interp')).toContainText('interpreted', {
        timeout: 15_000,
      });
      await expect(cardConfB.locator('sl-tooltip')).toHaveAttribute(
        'content',
        'Only Alice Choices can confirm or change this answer',
        { timeout: 15_000 }
      );

      // 1. User A confirms S-CONF (response 1: logged without waking the agent).
      await cardConfA.locator('.confirm-row sl-button[data-action="confirm"]').click();
      await expect(cardConfA.locator('.card-head .badge.conf')).toContainText('confirmed', {
        timeout: 15_000,
      });
      await expect(cardConfA.locator('.row-foot')).toContainText('Answered by you (confirmed)');
      await expect(cardConfB.locator('.card-head .badge.conf')).toContainText('confirmed', {
        timeout: 15_000,
      });

      const confRespLine = sessionA.page
        .locator('scion-chat-choice-response [data-kind="single"]')
        .last();
      await expect(confRespLine).toContainText('you confirmed Yes for', { timeout: 15_000 });
      await expect(confRespLine.locator('[data-state="not-sent"]')).toHaveText(
        'not sent to the agent'
      );

      // Space >2s past the co-located broker's 2s per-agent debounce window
      // before triggering the override on the same agent.
      await new Promise((resolve) => setTimeout(resolve, 2_500));

      // 2. User A overrides S-OVER via the inline change bar (response 2: delivered to agent).
      await cardOverA.locator('sl-button[data-choice="30m"]').click();
      await expect(cardOverA.locator('.confirmbar')).toContainText(
        'Change your answer from 5 minutes to 30 minutes? choices-single-p2 will be told.'
      );
      await cardOverA.locator('.confirmbar sl-button[data-action="confirm-change"]').click();

      await expect(cardOverA.locator('.card-head .badge.over')).toContainText('changed', {
        timeout: 15_000,
      });
      await expect(cardOverA.locator('.row-foot')).toContainText(
        'changed from the interpreted answer 5 minutes'
      );
      await expect(cardOverB.locator('.card-head .badge.over')).toContainText('changed', {
        timeout: 15_000,
      });
      await expect(cardOverB.locator('.row-foot')).toContainText(
        'changed from the interpreted answer 5 minutes'
      );

      await assertNoPageReload(sessionA.page, bootIdA);
      await assertNoPageReload(sessionB.page, bootIdB);

      // Wait for response 2 (override) to attempt broker delivery, then verify
      // response 1 (confirm) remained logged_without_delivery after 500ms settle.
      const overDelivery = await singleAgent.waitForChoiceResponseDelivery(
        topicId,
        { promptMessageId: promptOver.message_id },
        { authToken: userA.accessToken }
      );
      expect(overDelivery.deliveryClassification).toBe('attempted_delivery');
      expect(overDelivery.attemptedAgentDelivery).toBe(true);
      expect(overDelivery.response?.outcome).toBe('override');

      await new Promise((resolve) => setTimeout(resolve, 500));
      const inspected = await singleAgent.inspectChoiceResponses(topicId, {
        authToken: userA.accessToken,
      });
      const confirmInspected = inspected.find(
        (r) =>
          r.response?.promptMessageId === promptConf.message_id &&
          r.response?.provenance === 'direct'
      );
      expect(confirmInspected).toBeTruthy();
      expect(confirmInspected!.response?.outcome).toBe('confirm');
      expect(confirmInspected!.deliveryClassification).toBe('logged_without_delivery');
      expect(confirmInspected!.attemptedAgentDelivery).toBe(false);

      // 3. Reload parity: reload User B's page so history comes from GET /messages
      // and verify AnswerView.previousChoiceLabel still renders on the card.
      await sessionB.page.reload({ waitUntil: 'domcontentloaded' });
      const reloadedOverCard = sessionB.page.locator(
        `scion-chat-message[id="msg-${promptOver.message_id}"] scion-chat-choices`
      );
      await expect(reloadedOverCard.locator('.card-head .badge.over')).toContainText('changed', {
        timeout: 20_000,
      });
      await expect(reloadedOverCard.locator('.row-foot')).toContainText(
        'changed from the interpreted answer 5 minutes'
      );
    } finally {
      await Promise.all([sessionA.context.close(), sessionB.context.close()]);
    }
  });
});
