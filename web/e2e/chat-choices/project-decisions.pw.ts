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
 * Real-hub end-to-end Playwright suite for the Project Decisions page
 * (ptone/scion#7800, parent ptone/scion#7753, epic ptone/scion#7514).
 *
 * Implements the Plan §8 G3A browser scenario against a real Go `scion` hub
 * with both `web.chat_choices` and `web.project_decisions` enabled:
 *   1. User B (project owner/admin) and User A (ordinary project member) in one
 *      project with a containerless `release-bot` (`../harness/agent.ts`).
 *   2. Agent posts one choice prompt in `#release` and one in a private DM with User A.
 *   3. User B (project admin) sees `Decisions` header badge == 1 and ONLY the
 *      `#release` decision on `/projects/:id/decisions` (zero DM leakage across
 *      Waiting on you, Waiting on others, and Made).
 *   4. User A opens TWO browser tabs on `/projects/:id/decisions` -> sees badge == 2
 *      and BOTH `#release` and the private DM prompt.
 *   5. User A's DM prompt row is purged directly from the Hub SQLite `messages`
 *      table while leaving `decisions` intact -> reloading Tab 1 still renders
 *      User A's DM card with "Original message no longer available" and no
 *      "Open in chat" permalink.
 *   6. Clicking a choice on the purged DM card in Tab 1 answers with
 *      `surface: "decisions_page"`, removes the card and decrements the badge
 *      from 2 -> 1 in BOTH Tab 1 and Tab 2 live over SSE (without reloading Tab 2),
 *      and records the answer on User A's `Made` tab with surface `Decisions page`
 *      while User B's `Made` tab still never sees User A's DM decision.
 */

import { execFileSync } from 'node:child_process';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { test, expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { createTestAgent, type AgentSender } from '../harness/agent.js';
import { createSession, type AuthSession } from '../harness/auth.js';
import { readHubState, type HubState } from '../harness/hub.js';
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

function findHubDatabasePath(hub: HubState): string {
  const direct = path.join(hub.dbDir, 'e2e.db');
  if (fs.existsSync(direct)) {
    return direct;
  }
  const roots = [hub.dbDir, hub.homeDir].filter((d): d is string => Boolean(d));
  const found: string[] = [];
  const walk = (dir: string): void => {
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(full);
      } else if (entry.isFile() && entry.name.endsWith('.db')) {
        found.push(full);
      }
    }
  };
  for (const root of roots) {
    walk(root);
  }
  const preferred =
    found.find((f) => f.endsWith('hub.db')) ??
    found.find((f) => f.endsWith('e2e.db')) ??
    found[0] ??
    '';
  if (!preferred || !fs.existsSync(preferred)) {
    throw new Error(
      `Could not locate Hub SQLite DB under ${roots.join(', ')} (found: ${found.join(', ')})`
    );
  }
  return preferred;
}

function purgePromptMessagesFromSQLite(
  dbPath: string,
  messageIds: string[]
): { before: number; after: number; survivingDecisions: number } {
  const py = `
import json, sqlite3, sys
db_path = sys.argv[1]
ids = json.loads(sys.argv[2])
con = sqlite3.connect(db_path, timeout=10.0)
cur = con.cursor()
q = ",".join("?" for _ in ids)
before = cur.execute(f"SELECT COUNT(*) FROM messages WHERE id IN ({q})", ids).fetchone()[0]
for tbl, col in [
    ("webchat_message_star", "message_id"),
    ("webchat_mention", "message_id"),
    ("webchat_message_ext", "message_id"),
    ("message_addressees", "message_id"),
]:
    try:
        cur.execute(f"DELETE FROM {tbl} WHERE {col} IN ({q})", ids)
    except Exception:
        pass
cur.execute(f"DELETE FROM messages WHERE id IN ({q})", ids)
con.commit()
after = cur.execute(f"SELECT COUNT(*) FROM messages WHERE id IN ({q})", ids).fetchone()[0]
surviving = cur.execute(f"SELECT COUNT(*) FROM decisions WHERE message_id IN ({q})", ids).fetchone()[0]
con.close()
print(json.dumps({"before": before, "after": after, "survivingDecisions": surviving}))
`;
  const stdout = execFileSync('python3', ['-c', py, dbPath, JSON.stringify(messageIds)], {
    encoding: 'utf8',
  });
  return JSON.parse(stdout.trim()) as {
    before: number;
    after: number;
    survivingDecisions: number;
  };
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

async function awaitDecisionsPageSSE(page: Page, requiredSubjects: string[]): Promise<string> {
  await expect(page.locator('scion-page-project-decisions')).toBeVisible({ timeout: 20_000 });
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
          return (
            decoded.includes('/events') && requiredSubjects.every((subj) => decoded.includes(subj))
          );
        });
      },
      {
        timeout: 20_000,
        message: `Expected an open SSE stream subscribed to ${requiredSubjects.join(', ')}`,
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

test.describe('Project Decisions page on real hub (web.project_decisions + web.chat_choices)', () => {
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

    userB = await createSession(hub.baseURL, {
      email: `bob-decisions-admin-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Bob Admin',
    });
    userA = await createSession(hub.baseURL, {
      email: `alice-decisions-member-${Date.now()}@e2e.test`,
      role: 'member',
      displayName: 'Alice Member',
    });

    // User B creates the project (becoming project owner / project-admin).
    const projectRes = await hubApi<{ id?: string; project?: { id: string } }>(
      hub,
      userB.accessToken,
      'POST',
      '/api/v1/projects',
      { name: `decisions-g3a-${Date.now()}` }
    );
    expect([200, 201], JSON.stringify(projectRes.body)).toContain(projectRes.status);
    projectId = projectRes.body.project?.id ?? projectRes.body.id ?? '';
    expect(projectId).toMatch(/^[0-9a-f-]{36}$/);

    // User B adds User A as an ordinary project-member (NOT project-admin).
    const rolesRes = await hubApi<{
      items?: Array<{ id: string; name?: string; slug?: string }>;
      roles?: Array<{ id: string; name?: string; slug?: string }>;
    }>(
      hub,
      userB.accessToken,
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
      userB.accessToken,
      'POST',
      `/api/v1/projects/${encodeURIComponent(projectId)}/members`,
      {
        roleDefinitionId: memberRole!.id,
        principalType: 'user',
        principalId: userA.user.id,
      }
    );
    expect([200, 201], JSON.stringify(addMemberRes.body)).toContain(addMemberRes.status);

    // User A provisions release-bot so User A is in its delegation ancestry
    // (allowing ordinary member User A to answer it while User B is project-admin).
    agent = await createTestAgent(hub, {
      projectId,
      authToken: userA.accessToken,
      name: 'release-bot',
      messageMode: 'project',
      ancestry: [userA.user.id],
    });

    await createProjectTopic(hub, userB.accessToken, projectId, 'release', agent.slug);
  });

  test('G3A: zero DM leakage to project admin, purged DM prompt durability, inline decisions_page answer, and live multi-tab SSE removal', async ({
    browser,
  }) => {
    const dmSecretQuestion = 'Rotate Alice personal staging deploy key 9482?';
    const releaseQuestion = 'Freeze the v0.9 release branch at 17:00 UTC?';

    const relSend = await agent.sendChoicePrompt({
      conversationRef: '#release',
      msg: 'Release readiness check:',
      choices: [
        {
          id: 'D-REL',
          question: releaseQuestion,
          choices: [
            { id: 'yes', label: 'Yes, freeze' },
            { id: 'delay', label: 'Delay until tomorrow' },
          ],
          default: 'yes',
        },
      ],
    });
    expect(relSend.message_id).toBeTruthy();

    const dmSend = await agent.sendChoicePrompt({
      recipient: `user:${userA.user.email}`,
      msg: 'Private credentials check for Alice:',
      choices: [
        {
          id: 'D-DM-SECRET',
          question: dmSecretQuestion,
          choices: [
            { id: 'rotate', label: 'Rotate key' },
            { id: 'keep', label: 'Keep existing key' },
          ],
          default: 'rotate',
        },
      ],
    });
    expect(dmSend.message_id).toBeTruthy();

    const ctxB = await openUserContext(browser, hub, userB);
    const ctxA = await openUserContext(browser, hub, userA);

    try {
      // 1. User B (project admin): badge == 1 on project-detail and ONLY #release on Decisions page.
      const pageB = await ctxB.newPage();
      await pageB.goto(`/projects/${projectId}`, { waitUntil: 'domcontentloaded' });
      const headerBtnB = pageB.locator('[data-decisions-button]');
      await expect(headerBtnB).toBeVisible({ timeout: 20_000 });
      await expect(headerBtnB.locator('.decisions-badge')).toHaveText('1');

      await headerBtnB.click();
      await expect(pageB).toHaveURL(new RegExp(`/projects/${projectId}/decisions$`));
      const decisionsB = pageB.locator('scion-page-project-decisions');
      await expect(decisionsB.locator('[data-waiting-badge]')).toHaveText('1');
      await expect(decisionsB.locator(`[data-prompt-card="${relSend.message_id}"]`)).toBeVisible();
      await expect(decisionsB).toContainText(releaseQuestion);
      await expect(decisionsB).not.toContainText('D-DM-SECRET');
      await expect(decisionsB).not.toContainText(dmSecretQuestion);
      await expect(decisionsB.locator(`[data-prompt-card="${dmSend.message_id}"]`)).toHaveCount(0);

      // 2. User A (member): badge == 2 on project-detail, TWO open tabs on Decisions page seeing BOTH prompts.
      const tab1 = await ctxA.newPage();
      await tab1.goto(`/projects/${projectId}`, { waitUntil: 'domcontentloaded' });
      const headerBtnA = tab1.locator('[data-decisions-button]');
      await expect(headerBtnA).toBeVisible({ timeout: 20_000 });
      await expect(headerBtnA.locator('.decisions-badge')).toHaveText('2');

      await headerBtnA.click();
      await expect(tab1).toHaveURL(new RegExp(`/projects/${projectId}/decisions$`));
      await awaitDecisionsPageSSE(tab1, [`project.${projectId}.>`, `user.${userA.user.id}.chat.>`]);
      const decisionsTab1 = tab1.locator('scion-page-project-decisions');
      await expect(decisionsTab1.locator('[data-waiting-badge]')).toHaveText('2');
      await expect(
        decisionsTab1.locator(`[data-prompt-card="${relSend.message_id}"]`)
      ).toBeVisible();
      const dmCardTab1Before = decisionsTab1.locator(`[data-prompt-card="${dmSend.message_id}"]`);
      await expect(dmCardTab1Before).toBeVisible();
      await expect(dmCardTab1Before.locator('[data-conv-link]')).toHaveText('your DM');
      await expect(dmCardTab1Before.locator('[data-open-in-chat]')).toBeVisible();

      // Open Tab 2 for User A and await live SSE stream readiness.
      const tab2 = await ctxA.newPage();
      await tab2.goto(`/projects/${projectId}/decisions`, { waitUntil: 'domcontentloaded' });
      const tab2BootId = await awaitDecisionsPageSSE(tab2, [
        `project.${projectId}.>`,
        `user.${userA.user.id}.chat.>`,
      ]);
      const decisionsTab2 = tab2.locator('scion-page-project-decisions');
      await expect(decisionsTab2.locator('[data-waiting-badge]')).toHaveText('2');
      await expect(
        decisionsTab2.locator(`[data-prompt-card="${dmSend.message_id}"]`)
      ).toBeVisible();

      // 3. Purge User A's DM prompt message from Hub SQLite `messages` table while leaving `decisions` intact.
      const dbPath = findHubDatabasePath(hub);
      const purgeResult = purgePromptMessagesFromSQLite(dbPath, [dmSend.message_id]);
      expect(purgeResult).toEqual({ before: 1, after: 0, survivingDecisions: 1 });

      // Reload Tab 1 -> purged DM prompt STILL appears with "Original message no longer available" and no permalink!
      await tab1.reload({ waitUntil: 'domcontentloaded' });
      await awaitDecisionsPageSSE(tab1, [`project.${projectId}.>`, `user.${userA.user.id}.chat.>`]);
      const dmCardTab1Purged = decisionsTab1.locator(`[data-prompt-card="${dmSend.message_id}"]`);
      await expect(dmCardTab1Purged).toBeVisible();
      await expect(decisionsTab1.locator('[data-waiting-badge]')).toHaveText('2');
      await expect(dmCardTab1Purged.locator('[data-purged-note]')).toContainText(
        'Original message no longer available'
      );
      await expect(dmCardTab1Purged.locator('[data-open-in-chat]')).toHaveCount(0);
      await assertNoPageReload(tab2, tab2BootId);

      // 4. Answer the purged DM card inline in Tab 1 -> assert 200 OK with surface="decisions_page",
      //    and live SSE removal + badge 2 -> 1 in BOTH Tab 1 and Tab 2 without reloading Tab 2!
      const answerReqPromise = tab1.waitForRequest(
        (req) => req.method() === 'POST' && /\/api\/v1\/decisions\/[^/]+\/answer$/.test(req.url())
      );
      const answerResPromise = tab1.waitForResponse(
        (res) =>
          res.request().method() === 'POST' &&
          /\/api\/v1\/decisions\/[^/]+\/answer$/.test(res.url())
      );

      await dmCardTab1Purged.locator('sl-button[data-choice="rotate"]').click();

      const [answerReq, answerRes] = await Promise.all([answerReqPromise, answerResPromise]);
      expect(answerRes.status()).toBe(200);
      expect(answerReq.postDataJSON()).toMatchObject({
        choice_id: 'rotate',
        surface: 'decisions_page',
      });

      // Tab 1 updates immediately.
      await expect(decisionsTab1.locator(`[data-prompt-card="${dmSend.message_id}"]`)).toHaveCount(
        0
      );
      await expect(decisionsTab1.locator('[data-waiting-badge]')).toHaveText('1');

      // Tab 2 updates live over SSE WITHOUT reloading!
      await expect(decisionsTab2.locator(`[data-prompt-card="${dmSend.message_id}"]`)).toHaveCount(
        0,
        { timeout: 15_000 }
      );
      await expect(decisionsTab2.locator('[data-waiting-badge]')).toHaveText('1');
      await assertNoPageReload(tab2, tab2BootId);

      // 5. Switch User A's Tab 1 to Made -> DM answer appears with surface "Decisions page".
      await decisionsTab1.locator('sl-tab[panel="made"]').click();
      await expect(tab1).toHaveURL(new RegExp(`/projects/${projectId}/decisions\\?tab=made$`));
      const madeTableA = decisionsTab1.locator('table.log[data-made-table]');
      await expect(madeTableA).toBeVisible();
      await expect(madeTableA).toContainText('D-DM-SECRET');
      await expect(madeTableA).toContainText(dmSecretQuestion);
      await expect(madeTableA).toContainText('Rotate key');
      await expect(madeTableA).toContainText('Decisions page');
      await expect(madeTableA).toContainText('your DM');

      // 6. User B's Made tab MUST still never see User A's DM decision!
      await pageB.goto(`/projects/${projectId}/decisions?tab=made`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(decisionsB.locator('[data-empty="made"]')).toBeVisible({ timeout: 15_000 });
      await expect(decisionsB).not.toContainText('D-DM-SECRET');
      await expect(decisionsB).not.toContainText(dmSecretQuestion);
    } finally {
      await ctxA.close();
      await ctxB.close();
    }
  });
});
