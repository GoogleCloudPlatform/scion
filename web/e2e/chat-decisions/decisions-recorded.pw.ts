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
 * Real-hub scenario for recorded decisions (ptone/scion#8477, design for
 * ptone/scion#8443 section 4.3): an agent asks decisions with descriptions
 * and a recommendation; the user answers one matching and one not matching
 * the recommendation, confirms an interpreted answer, dismisses one (with
 * the test-only 30 s dismiss minimum age from ptone/scion#8582) and answers
 * one with Other. The Made tab on the project and profile Decisions pages
 * then shows one entry per decision with the right collapsed marker, and
 * each detail panel lists the offered choices, the pick, the dismissal, the
 * folded interpreted answer and the Other text.
 *
 * Screenshots are written to Playwright's per-test output directory and
 * mirrored to `REC_EVIDENCE_DIR` when set.
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { test, expect, type Locator, type Page, type TestInfo } from '@playwright/test';
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

async function createProjectTopic(
  hub: HubState,
  token: string,
  projectId: string,
  name: string,
  defaultAgent: string
): Promise<string> {
  const deadline = Date.now() + 5_000;
  const post = (): Promise<HubApiResponse<{ id?: string; thread?: { id: string } }>> =>
    hubApi(hub, token, 'POST', `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`, {
      name,
      defaultAgent,
    });
  let res = await post();
  while (res.status === 503 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    res = await post();
  }
  expect(res.status, JSON.stringify(res.body)).toBeLessThan(300);
  const id = res.body.thread?.id ?? res.body.id ?? '';
  expect(id).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

function mirrorEvidence(testInfo: TestInfo, outFile: string, filename: string): void {
  const evidenceDir = process.env.REC_EVIDENCE_DIR;
  if (evidenceDir) {
    fs.mkdirSync(evidenceDir, { recursive: true });
    fs.copyFileSync(outFile, path.join(evidenceDir, filename));
  }
}

async function shot(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const size = page.viewportSize() ?? { width: 1440, height: 900 };
  await page.setViewportSize({ width: size.width, height: 2000 });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(400);
  const filename = `rec-realhub-${name}.png`;
  const outFile = testInfo.outputPath(filename);
  await page.screenshot({ path: outFile });
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });
  mirrorEvidence(testInfo, outFile, filename);
  await page.setViewportSize(size);
}

const REGIONS = [
  { id: 'eu', label: 'Europe', description: 'Lowest latency for most current users.' },
  { id: 'us', label: 'United States', description: 'Closest to the build farm.' },
  { id: 'ap', label: 'Asia Pacific', description: 'Fastest for the Tokyo team.' },
];

/** Made entry by decision key on the desktop table. */
function rowOf(scope: Locator, key: string): Locator {
  return scope.locator('table.log[data-made-table] tr[data-answer-row]', {
    has: scope.page().locator(`.key:text-is("${key}")`),
  });
}

async function expandAndAssert(
  scope: Locator,
  page: Page,
  testInfo: TestInfo,
  name: string
): Promise<void> {
  const match = rowOf(scope, 'REC-MATCH');
  const miss = rowOf(scope, 'REC-MISS');
  const interp = rowOf(scope, 'REC-INTERP');
  const dismissed = rowOf(scope, 'REC-DSM');
  const other = rowOf(scope, 'REC-OTHER');
  await expect(match).toHaveCount(1, { timeout: 15_000 });
  // D2: one entry per decision version (the interpreted answer folds into the confirm).
  await expect(interp).toHaveCount(1);

  await expect(match.locator('[data-rec-marker]')).toHaveText('recommended');
  await expect(miss.locator('[data-rec-marker]')).toHaveText('recommended: United States');
  await expect(interp.locator('[data-rec-marker]')).toHaveText('recommended: Canary');
  await expect(dismissed.locator('[data-rec-marker]')).toHaveCount(0);
  await expect(other.locator('[data-rec-marker]')).toHaveText('recommended: Docs site');

  for (const r of [match, miss, interp, dismissed, other]) {
    const toggle = r.getByRole('button', { name: /^Show choices for / });
    await toggle.click();
    await expect(
      r.getByRole('button', { name: /^Hide choices for /, expanded: true })
    ).toBeVisible();
  }

  const panel = async (r: Locator): Promise<Locator> => {
    const answerId = (await r.getAttribute('data-answer-row')) ?? '';
    expect(answerId).not.toBe('');
    return scope.locator(`tr[data-detail-row="${answerId}"] [data-made-detail]`);
  };

  const missPanel = await panel(miss);
  await expect(missPanel.locator('[data-detail-choice]')).toHaveCount(3);
  await expect(missPanel.locator('[data-detail-choice][data-picked]')).toHaveAttribute(
    'data-detail-choice',
    'eu'
  );
  await expect(
    missPanel.locator('[data-detail-choice="us"] [data-detail-recommended]')
  ).toBeVisible();
  await expect(missPanel.locator('[data-detail-choice="eu"] [data-detail-desc]')).toHaveText(
    'Lowest latency for most current users.'
  );
  await expect(missPanel.locator('[data-detail-open-in-chat]')).toHaveAttribute('href', /#msg-/);

  const interpPanel = await panel(interp);
  await expect(interpPanel.locator('[data-detail-choice][data-picked]')).toHaveAttribute(
    'data-detail-choice',
    'b'
  );
  await expect(interpPanel.locator('[data-detail-interpreted]')).toHaveText(
    'Was interpreted as Blue-green'
  );

  const dsmPanel = await panel(dismissed);
  await expect(dsmPanel.locator('[data-detail-choice][data-picked]')).toHaveCount(0);
  await expect(dsmPanel.locator('[data-detail-dismissed]')).toHaveText(
    'Dismissed: Covered by the migration plan.'
  );

  const otherPanel = await panel(other);
  await expect(otherPanel.locator('[data-detail-choice="$other"]')).toHaveAttribute(
    'data-picked',
    ''
  );
  await expect(otherPanel.locator('[data-detail-other-text]')).toHaveText(
    '“The repository README”'
  );

  await shot(page, testInfo, name);
}

test.describe('Recorded decisions on real hub (web.decisions)', () => {
  let hub!: HubState;
  let projectId = '';
  let topicId = '';
  let userA!: AuthSession;
  let agent!: AgentSender;

  test.beforeAll(async () => {
    const loadedHub = readHubState(stateFile());
    if (!loadedHub) {
      throw new Error('Hub state file was empty; did global-setup succeed?');
    }
    hub = loadedHub;
    const stamp = Date.now();
    userA = await createSession(hub.baseURL, {
      email: `alice-recorded-${stamp}@e2e.test`,
      role: 'member',
      displayName: 'Alice Recorded',
    });
    const projectRes = await hubApi<{ id?: string; project?: { id: string } }>(
      hub,
      userA.accessToken,
      'POST',
      '/api/v1/projects',
      { name: `decisions-recorded-${stamp}` }
    );
    expect([200, 201], JSON.stringify(projectRes.body)).toContain(projectRes.status);
    projectId = projectRes.body.project?.id ?? projectRes.body.id ?? '';
    expect(projectId).toMatch(/^[0-9a-f-]{36}$/);
    agent = await createTestAgent(hub, {
      projectId,
      authToken: userA.accessToken,
      name: 'infra-bot',
      messageMode: 'project',
      ancestry: [userA.user.id],
    });
    topicId = await createProjectTopic(hub, userA.accessToken, projectId, 'infra', agent.slug);
  });

  test('REC: Made entries show offered choices, recommendation and final answer on project and profile', async ({
    browser,
  }, testInfo) => {
    test.setTimeout(180_000);

    // Asked first so its dismiss minimum age (30 s here) runs down meanwhile.
    const dsmSend = await agent.sendDecisionPrompt({
      conversationRef: '#infra',
      msg: 'Cut-over check:',
      decisions: [
        {
          id: 'REC-DSM',
          question: 'Freeze main during the cut?',
          choices: [
            { id: 'yes', label: 'Yes', description: 'No merges for an hour.' },
            { id: 'no', label: 'No' },
          ],
          default: 'yes',
        },
      ],
    });
    const matchSend = await agent.sendDecisionPrompt({
      conversationRef: '#infra',
      msg: 'Build cache placement:',
      decisions: [
        {
          id: 'REC-MATCH',
          question: 'Region for the build cache?',
          choices: REGIONS,
          default: 'us',
        },
      ],
    });
    const missSend = await agent.sendDecisionPrompt({
      conversationRef: '#infra',
      msg: 'Cluster placement:',
      decisions: [
        {
          id: 'REC-MISS',
          question: 'Region for the new cluster?',
          choices: REGIONS,
          default: 'us',
        },
      ],
    });
    const interpSend = await agent.sendDecisionPrompt({
      conversationRef: '#infra',
      msg: 'Rollout style:',
      decisions: [
        {
          id: 'REC-INTERP',
          question: 'How should the rollout run?',
          choices: [
            { id: 'a', label: 'Canary', description: '5% first, then everyone.' },
            { id: 'b', label: 'Blue-green', description: 'Switch all traffic at once.' },
          ],
          default: 'a',
        },
      ],
    });
    const otherSend = await agent.sendDecisionPrompt({
      conversationRef: '#infra',
      msg: 'Docs home:',
      decisions: [
        {
          id: 'REC-OTHER',
          question: 'Where should the runbook live?',
          choices: [
            { id: 'site', label: 'Docs site' },
            { id: 'wiki', label: 'Wiki' },
          ],
          default: 'site',
          allow_other: { label: 'Somewhere else', description: 'Name a place.' },
        },
      ],
    });

    const rowId = (
      send: { decisions?: { key: string; row_id: string }[] },
      key: string
    ): string => {
      const id = send.decisions?.find((d) => d.key === key)?.row_id ?? '';
      expect(id, `${key} row id`).not.toBe('');
      return id;
    };
    const answer = async (row: string, body: Record<string, unknown>): Promise<void> => {
      const res = await hubApi(hub, userA.accessToken, 'POST', `/api/v1/decisions/${row}/answer`, {
        idempotency_key: crypto.randomUUID(),
        ...body,
      });
      expect(res.status, JSON.stringify(res.body)).toBe(200);
    };

    // Plain answers: one matching the recommendation, one not.
    await answer(rowId(matchSend, 'REC-MATCH'), { choice_id: 'us' });
    await answer(rowId(missSend, 'REC-MISS'), { choice_id: 'eu' });

    // Interpreted from a free-text reply, then confirmed by the user.
    const reply = await hubApi<{ id: string }>(
      hub,
      userA.accessToken,
      'POST',
      `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`,
      { content: 'Go blue-green on this one.' }
    );
    expect(reply.status, JSON.stringify(reply.body)).toBeLessThan(300);
    const interpRes = await agent.request('/api/v1/agent/decisions/interpreted', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        source_message_id: reply.body.id,
        answers: [{ key: 'REC-INTERP', choice_id: 'b' }],
      }),
    });
    expect(interpRes.status, interpRes.text).toBe(200);
    await answer(rowId(interpSend, 'REC-INTERP'), { choice_id: 'b' });

    // Other with typed text.
    await answer(rowId(otherSend, 'REC-OTHER'), {
      choice_id: '$other',
      other_text: 'The repository README',
    });

    // Dismissal once the minimum age has passed.
    await expect
      .poll(
        async () =>
          (
            await hubApi(
              hub,
              userA.accessToken,
              'POST',
              `/api/v1/decision-prompts/${encodeURIComponent(dsmSend.message_id)}/dismiss`,
              { reason: 'Covered by the migration plan.' }
            )
          ).status,
        { timeout: 60_000, intervals: [2_000] }
      )
      .toBe(200);

    const context = await browser.newContext({
      baseURL: hub.baseURL,
      storageState: userA.storageStatePath,
      viewport: { width: 1440, height: 900 },
    });
    try {
      const page = await context.newPage();
      await page.goto(`/projects/${projectId}/decisions?tab=made`, {
        waitUntil: 'domcontentloaded',
      });
      const projectPage = page.locator('scion-page-project-decisions');
      await expandAndAssert(projectPage, page, testInfo, 'project-made');

      await page.goto('/profile/decisions?tab=made&sort=recent', {
        waitUntil: 'domcontentloaded',
      });
      const profilePage = page.locator('scion-page-profile-decisions');
      await expandAndAssert(profilePage, page, testInfo, 'profile-made');
    } finally {
      await context.close();
    }
  });
});
