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
 * Dismissed decision cards, row-picker dialog, multi-select, Made entries,
 * chat response lines, and 5-minute minimum age (ptone/scion#8216,
 * ptone/scion#8217, ptone/scion#8218, parent ptone/scion#8194;
 * scenes X1–X9 and X4b) in light and dark themes on desktop (1440 px) and
 * mobile (390 px).
 *
 * Screenshots are written to Playwright's per-test output directory
 * (`testInfo.outputPath(...)`), and mirrored to `DSM_EVIDENCE_DIR` when set
 * for design-review evidence outside `/workspace`.
 */

import * as fs from 'node:fs';
import * as path from 'node:path';
import { test, expect, type Locator, type Page, type TestInfo } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';
import { openChatRail, openGeneralThread } from './fixture.js';
import { GENERAL_THREAD_ID, PROJECT_A } from './mock-api.js';
import {
  ASKER_ID,
  ASKER_NAME,
  BOB_ID,
  SELF_ID,
  at,
  decision,
  routeDecisionThread,
  row,
  setDecisions,
  setThread,
  singleThread,
  type AskerLookup,
  type DecisionThread,
} from './decisions-mock.js';

type Theme = 'light' | 'dark';
type Json = Record<string, unknown>;

const PROJECTS = new Set(['desktop-1440', 'chromium-390']);
const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 768;

const AGENT_NCX_ID = '22222222-2222-4222-8222-222222222222';
const AGENT_DOCS_ID = '33333333-3333-4333-8333-333333333333';

const YES_NO = [
  { id: 'yes', label: 'Yes' },
  { id: 'no', label: 'No' },
];

const message = (page: Page, id: string): Locator => page.locator(`[id="msg-${id}"]`);
const card = (page: Page, id: string): Locator => message(page, id).locator('scion-chat-decisions');

function dismissedInfo(
  minutes: number,
  opts: {
    reason?: string;
    byUserId?: string;
    byName?: string;
    surface?: string;
    responseMessageId?: string;
  } = {}
): Json {
  return {
    at: at(minutes),
    byUserId: opts.byUserId ?? 'user-alice',
    byName: opts.byName ?? 'alice',
    ...(opts.reason ? { reason: opts.reason } : {}),
    surface: opts.surface ?? 'decisions_page',
    responseMessageId: opts.responseMessageId ?? `resp-dsm-${minutes}`,
  };
}

function withDismissed(base: Json, dismissed: Json, extra: Json = {}): Json {
  return {
    ...base,
    status: 'dismissed',
    dismissed,
    ...extra,
  };
}

function mirrorEvidence(testInfo: TestInfo, outFile: string, filename: string): void {
  const evidenceDir = process.env.DSM_EVIDENCE_DIR;
  if (evidenceDir && PROJECTS.has(testInfo.project.name)) {
    fs.mkdirSync(evidenceDir, { recursive: true });
    fs.copyFileSync(outFile, path.join(evidenceDir, filename));
  }
}

async function shotMessages(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string,
  ids: string[]
): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(250);
  const boxes = [];
  for (const id of ids) {
    const box = await message(page, id).boundingBox();
    if (box) boxes.push(box);
  }
  expect(boxes.length, `messages ${ids.join(', ')} have boxes`).toBe(ids.length);
  const viewport = page.viewportSize()!;
  const pad = 8;
  const x = Math.max(0, Math.min(...boxes.map((b) => b.x)) - pad);
  const y = Math.max(0, Math.min(...boxes.map((b) => b.y)) - pad);
  const right = Math.min(viewport.width, Math.max(...boxes.map((b) => b.x + b.width)) + pad);
  const bottom = Math.min(viewport.height, Math.max(...boxes.map((b) => b.y + b.height)) + pad);
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  const filename = `dismiss-${project}-${theme}-${name}.png`;
  const outFile = testInfo.outputPath(filename);
  await page.screenshot({ path: outFile, clip: { x, y, width: right - x, height: bottom - y } });
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });
  mirrorEvidence(testInfo, outFile, filename);
}

async function shotViewport(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string
): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(250);
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  const filename = `dismiss-${project}-${theme}-${name}.png`;
  const outFile = testInfo.outputPath(filename);
  await page.screenshot({ path: outFile, fullPage: false });
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });
  mirrorEvidence(testInfo, outFile, filename);
}

async function openDecisionsChat(
  page: Page,
  theme: Theme,
  thread: DecisionThread,
  asker: AskerLookup = 'ok',
  extraRoutes?: (p: Page) => Promise<void>,
  threadName?: string
): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.addInitScript((t) => {
      localStorage.setItem('scion-theme', t);
      window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, 'web.decisions': true };
    }, theme);
    await routeDecisionThread(p, thread, asker);
    if (threadName) {
      await p.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/threads/, (route) =>
        route.fulfill({
          json: {
            threads: [
              {
                id: GENERAL_THREAD_ID,
                name: threadName,
                isGeneral: true,
                pinned: false,
                hasUnread: false,
                hasUnreadMention: false,
              },
            ],
          },
        })
      );
      await p.route(new RegExp(`/api/v1/chat/topics/${GENERAL_THREAD_ID}(\\?|$)`), (route) =>
        route.fulfill({ json: { id: GENERAL_THREAD_ID, name: threadName } })
      );
    }
    if (extraRoutes) {
      await extraRoutes(p);
    }
  });
  if (isPhone(page)) {
    if (threadName) {
      const targetRow = page.locator('.thread-item', { hasText: threadName }).first();
      await expect(targetRow).toBeVisible({ timeout: 15_000 });
      await targetRow.click();
      await expect(page.locator('.v2-thread-header')).toBeVisible({ timeout: 10_000 });
      await page.waitForTimeout(400);
    } else {
      await openGeneralThread(page);
    }
  }
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
  const first = thread.messages[0]?.id as string;
  await expect(message(page, first)).toBeVisible({ timeout: 15_000 });
}

async function markdownReady(page: Page, id: string): Promise<void> {
  await expect(card(page, id).locator('.q code').first()).toBeVisible();
}

async function setupDecisionsPageShellRoutes(page: Page, theme: Theme): Promise<void> {
  await installEventSourceStub(page);
  await page.addInitScript((t: Theme) => {
    localStorage.setItem('scion-theme', t);
    localStorage.removeItem('scion:decisions:filters-open');
    window.__SCION_FEATURES__ = {
      ...window.__SCION_FEATURES__,
      'web.native_chat': true,
      'web.decisions': true,
    };
  }, theme);

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: { id: SELF_ID, name: 'alice', email: 'alice@example.test', role: 'member' },
    })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/notifications**', (route) =>
    route.fulfill({ json: { notifications: [], unreadCount: 0 } })
  );
  await page.route('**/api/v1/env**', (route) =>
    route.fulfill({ json: { envVars: [], scope: 'user' } })
  );
  await page.route('**/api/v1/users**', (route) => route.fulfill({ json: { users: [] } }));
  await page.route(/\/api\/v1\/chat\/spaces$/, (route) =>
    route.fulfill({
      json: {
        spaces: [
          {
            projectId: PROJECT_A.id,
            projectSlug: 'scion',
            projectName: 'scion',
            unreadCount: 0,
            hasUnreadMention: false,
          },
        ],
      },
    })
  );
  await page.route('**/api/v1/chat/dms**', (route) => {
    if (route.request().url().includes('/unread')) {
      return route.fulfill({ json: { peerIds: [] } });
    }
    return route.fulfill({ json: { dms: [] } });
  });
  await page.route('**/api/v1/chat/presence', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/chat/user-prefs', (route) =>
    route.fulfill({
      json: {
        spaceSortMode: 'activity',
        threadSortMode: 'activity',
        spaceOrder: '[]',
        threadOrder: '{}',
        threadGroups: '{}',
      },
    })
  );
  await page.route(/\/api\/v1\/projects(\?|$)/, (route) =>
    route.fulfill({
      json: {
        projects: [{ id: PROJECT_A.id, name: 'scion', slug: 'scion' }],
        _capabilities: { actions: ['read'] },
      },
    })
  );
  await page.route(`/api/v1/projects/${PROJECT_A.id}`, (route) =>
    route.fulfill({
      json: {
        id: PROJECT_A.id,
        name: 'scion',
        slug: 'scion',
        _capabilities: { actions: ['read', 'update'] },
      },
    })
  );
  await page.route(new RegExp(`/api/v1/projects/${PROJECT_A.id}/agents(\\?|$)`), (route) =>
    route.fulfill({
      json: {
        agents: [],
        totalCount: 0,
        complete: true,
        stats: { total: 0, running: 0 },
        _capabilities: { actions: ['read'] },
      },
    })
  );
  await page.route(/\/(metrics-summary|session-metrics)(\?|$)/, (route) =>
    route.fulfill({ json: { available: false } })
  );
  await page.route(new RegExp(`/api/v1/agents/${AGENT_NCX_ID}(\\?|$)`), (route) =>
    route.fulfill({
      status: 404,
      json: { error: { code: 'not_found', message: 'agent not found' } },
    })
  );
  await page.route(new RegExp(`/api/v1/agents/(${ASKER_ID}|${AGENT_DOCS_ID})(\\?|$)`), (route) =>
    route.fulfill({
      json: {
        id: ASKER_ID,
        name: ASKER_NAME,
        slug: ASKER_NAME,
        projectId: PROJECT_A.id,
        phase: 'running',
        _messageability: { canMessage: true },
      },
    })
  );
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`dismissed decision scenes (${theme})`, () => {
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('X1: single decision dismissed by a user with a reason', async ({ page }, testInfo) => {
      const base = decision({
        messageId: 'prompt-x1',
        rowId: 'prompt-x1-d234',
        key: 'D234',
        question: 'Ship `v0.9` today?',
        defaultChoiceId: 'yes',
        choices: YES_NO,
      });
      const d = withDismissed(
        base,
        dismissedInfo(11, { reason: 'Stale: v0.9 shipped last week.' })
      );
      await openDecisionsChat(page, theme, singleThread(d, 'One call before I tag.'));
      await markdownReady(page, 'prompt-x1');

      const c = card(page, 'prompt-x1');
      await expect(c.locator('.row.dismissed')).toBeVisible();
      await expect(c.locator('sl-button[data-choice="yes"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await expect(c.locator('sl-button[data-choice="no"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await expect(c.locator('[data-card-menu]')).toHaveCount(0);
      await expect(c.locator('.wd sl-icon[name="x-circle"]')).toHaveCount(1);
      await expect(c.locator('.wd')).toContainText('Dismissed by alice');
      await expect(c.locator('.wd-reason')).toHaveText('Stale: v0.9 shipped last week.');

      await shotMessages(page, testInfo, theme, 'x1-single-dismissed', ['prompt-x1']);
    });

    test('X2: set with one row dismissed and remaining rows still open', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-x2');
      decisions[1] = withDismissed(
        decisions[1],
        dismissedInfo(11, { reason: 'Not my area; ask bob.' })
      );
      await openDecisionsChat(
        page,
        theme,
        setThread('prompt-x2', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-x2');

      const c = card(page, 'prompt-x2');
      await c.locator('[data-row="prompt-x2-d234"] button[data-choice="yes"]').click();

      await expect(c.locator('[data-card-menu]')).toBeVisible();
      const dsmRow = c.locator('[data-row="prompt-x2-17"]');
      await expect(dsmRow).toHaveClass(/dismissed/);
      await expect(dsmRow.locator('.wd')).toContainText('Dismissed by alice');
      await expect(dsmRow.locator('.wd-reason')).toHaveText('Not my area; ask bob.');
      await expect(c.locator('.counter')).toHaveText('1 remaining · 1 dismissed');
      await expect(c.locator('sl-button.submit')).toHaveAttribute('aria-disabled', 'true');

      await shotMessages(page, testInfo, theme, 'x2-set-one-dismissed', ['prompt-x2']);
    });

    test('X3: whole card dismissed in one action collapses to a single card footer', async ({
      page,
    }, testInfo) => {
      const sharedDismissed = dismissedInfo(11, {
        reason: 'Stale: the release already shipped.',
        responseMessageId: 'resp-x3-batch',
      });
      const decisions = setDecisions('prompt-x3').map((d) => withDismissed(d, sharedDismissed));
      await openDecisionsChat(
        page,
        theme,
        setThread('prompt-x3', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-x3');

      const c = card(page, 'prompt-x3');
      await expect(c.locator('.card.dismissed-all')).toBeVisible();
      await expect(c.locator('.card-head .tag-wd')).toHaveText('Dismissed');
      await expect(c.locator('[data-card-menu]')).toHaveCount(0);
      await expect(c.locator('.row .wd')).toHaveCount(0);
      await expect(c.locator('.card-foot .wd')).toContainText('Dismissed by alice');
      await expect(c.locator('.card-foot .wd-reason')).toHaveText(
        'Stale: the release already shipped.'
      );
      await expect(c.locator('sl-button.submit')).toHaveCount(0);

      await shotMessages(page, testInfo, theme, 'x3-set-whole-dismissed', ['prompt-x3']);
    });

    test('X4: card ⋯ menu opens row-picker dismiss dialog for a set with a live agent', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-x4');
      decisions[2] = {
        ...decisions[2],
        status: 'decided',
        decision: {
          choiceId: 'no',
          label: 'No',
          answeredByUserId: BOB_ID,
          answeredByName: 'bob',
          provenance: 'agent_interpreted',
          outcome: 'answer',
          surface: 'web',
          responseMessageId: 'resp-bob-interp',
          at: at(3),
        },
      };
      await openDecisionsChat(
        page,
        theme,
        setThread('prompt-x4', decisions, 'Three calls before the release.'),
        'ok',
        undefined,
        'release'
      );
      await markdownReady(page, 'prompt-x4');

      const c = card(page, 'prompt-x4');
      await c.locator('[data-card-menu-trigger]').click();
      const dsmItem = c.locator('sl-menu-item[data-dismiss-menu-item]');
      await expect(dsmItem).toBeVisible();
      await dsmItem.click();

      const dialog = c.locator('sl-dialog[data-dismiss-dialog][open]');
      await expect(dialog).toBeVisible();
      await expect(dialog.locator('[data-dismiss-check="prompt-x4-d234"]')).toHaveAttribute(
        'checked',
        ''
      );
      await expect(dialog.locator('[data-dismiss-check="prompt-x4-17"]')).toHaveAttribute(
        'checked',
        ''
      );
      await expect(
        dialog.locator('[data-dismiss-row="prompt-x4-ship-now"] sl-checkbox')
      ).toHaveAttribute('disabled', '');
      await expect(dialog.locator('.dismiss-why')).toContainText(
        "Waiting on bob to confirm release-bot's reading"
      );

      const textarea = dialog.locator('sl-textarea[data-dismiss-reason] textarea');
      await textarea.fill('Stale: the release already shipped.');
      await expect(dialog.locator('[data-dismiss-reason-count]')).toHaveText('35/500');
      await expect(dialog.locator('[data-dismiss-tell="live"] span')).toHaveText(
        `${ASKER_NAME} will be told in #release.`
      );
      await expect(dialog.locator('sl-button[data-dismiss-confirm]')).toContainText(
        'Dismiss 2 decisions'
      );

      await shotViewport(page, testInfo, theme, 'x4-dialog-set-live');
    });

    test('X4b: dismiss dialog for a single decision when the asking agent is gone', async ({
      page,
    }, testInfo) => {
      const migrate = {
        ...decision({
          messageId: 'prompt-x4b',
          rowId: 'prompt-x4b-mig',
          key: 'migrate',
          question: 'Run the backfill on staging first?',
          choices: YES_NO,
        }),
        askerAgentId: AGENT_NCX_ID,
        askerAgentName: 'nc-x',
      };
      const thread: DecisionThread = {
        messages: [
          row(
            'prompt-x4b',
            0,
            'decision-prompt',
            'Staging migration check.\n\n(fallback)',
            'asker',
            {
              sender: 'agent:nc-x',
              senderId: AGENT_NCX_ID,
              agentId: AGENT_NCX_ID,
            }
          ),
        ],
        messageDecisions: { 'prompt-x4b': [migrate] },
        messageDecisionIntros: { 'prompt-x4b': 'Staging migration check.' },
        messageDecisionAnswerModes: { 'prompt-x4b': 'single' },
      };

      await openDecisionsChat(page, theme, thread, 'gone', async (p) => {
        await p.route(new RegExp(`/api/v1/agents/${AGENT_NCX_ID}(\\?|$)`), (route) =>
          route.fulfill({
            status: 404,
            json: { error: { code: 'not_found', message: 'agent not found' } },
          })
        );
      });

      const c = card(page, 'prompt-x4b');
      await c.locator('[data-card-menu-trigger]').click();
      await c.locator('sl-menu-item[data-dismiss-menu-item]').click();

      const dialog = c.locator('sl-dialog[data-dismiss-dialog][open]');
      await expect(dialog).toBeVisible();
      await expect(dialog.locator('.dismiss-single-row')).toContainText('migrate');
      await expect(dialog.locator('.dismiss-single-row')).toContainText(
        'Run the backfill on staging first?'
      );
      await expect(dialog.locator('[data-dismiss-tell="gone"]')).toHaveText(
        'nc-x is gone; nothing will be sent.'
      );
      await expect(dialog.locator('sl-button[data-dismiss-confirm]')).toHaveText('Dismiss');

      await shotViewport(page, testInfo, theme, 'x4b-dialog-single-gone');
    });

    test('X5: Profile Decisions Waiting on you multi-select bar and bulk dismiss', async ({
      page,
    }, testInfo) => {
      await setupDecisionsPageShellRoutes(page, theme);
      const nowMs = Date.now();
      const isoAgo = (mins: number): string => new Date(nowMs - mins * 60_000).toISOString();

      const projRef = { id: PROJECT_A.id, name: 'scion', slug: 'scion' };
      const prompts = [
        {
          project: projRef,
          promptMessageId: 'm-ncx',
          promptAvailable: true,
          chatKey: `dm:agent:${AGENT_NCX_ID}:user:${SELF_ID}`,
          conversationLabel: 'DM with nc-x',
          conversationAvailable: true,
          askerAgentId: AGENT_NCX_ID,
          askerAgentName: 'nc-x',
          createdAt: isoAgo(60 * 24 * 9),
          answerMode: 'single',
          waitingOnYou: true,
          canAnswer: true,
          decisions: [
            {
              rowId: 'r-ncx-mig',
              messageId: 'm-ncx',
              key: 'migrate',
              ref: 'migrate',
              version: 1,
              status: 'pending',
              askerAgentId: AGENT_NCX_ID,
              askerAgentName: 'nc-x',
              question: 'Run the backfill on staging first?',
              choices: YES_NO,
            },
          ],
        },
        {
          project: projRef,
          promptMessageId: 'm-rel-set',
          promptAvailable: true,
          chatKey: GENERAL_THREAD_ID,
          conversationLabel: '#release',
          conversationAvailable: true,
          askerAgentId: ASKER_ID,
          askerAgentName: ASKER_NAME,
          createdAt: isoAgo(60 * 24 * 7),
          answerMode: 'set',
          waitingOnYou: true,
          canAnswer: true,
          decisions: setDecisions('m-rel-set').slice(0, 2),
        },
        {
          project: projRef,
          promptMessageId: 'm-docs',
          promptAvailable: true,
          chatKey: 'topic-docs',
          conversationLabel: '#docs',
          conversationAvailable: true,
          askerAgentId: AGENT_DOCS_ID,
          askerAgentName: 'docs-bot',
          createdAt: isoAgo(20),
          answerMode: 'single',
          waitingOnYou: true,
          canAnswer: true,
          decisions: [
            {
              rowId: 'r-docs-toc',
              messageId: 'm-docs',
              key: 'toc',
              ref: 'toc',
              version: 1,
              status: 'pending',
              askerAgentId: AGENT_DOCS_ID,
              askerAgentName: 'docs-bot',
              question: 'Split the CLI reference into one page per command?',
              choices: [
                { id: 'yes', label: 'Yes' },
                { id: 'keep', label: 'Keep one page' },
              ],
            },
          ],
        },
      ];

      await page.route('**/api/v1/users/me/decisions**', (route) =>
        route.fulfill({
          json: {
            counts: { waitingOnYou: 4, waitingOnOthers: 0 },
            prompts,
            agents: [
              { id: AGENT_NCX_ID, name: 'nc-x', hint: '(deleted)', projectId: PROJECT_A.id },
              { id: ASKER_ID, name: ASKER_NAME, projectId: PROJECT_A.id },
              { id: AGENT_DOCS_ID, name: 'docs-bot', projectId: PROJECT_A.id },
            ],
            answerers: [{ id: SELF_ID, name: 'alice' }],
            conversations: [
              { key: GENERAL_THREAD_ID, label: '#release', projectId: PROJECT_A.id },
              { key: 'topic-docs', label: '#docs', projectId: PROJECT_A.id },
            ],
            projects: [projRef],
            truncatedProjects: false,
          },
        })
      );

      await page.goto('/profile/decisions', { waitUntil: 'domcontentloaded' });
      const pageEl = page.locator('scion-page-profile-decisions');
      await expect(pageEl).toBeVisible();
      await expect(pageEl.locator('[data-prompt-card="m-ncx"]')).toBeVisible();

      await pageEl.locator('sl-checkbox[data-select-prompt="m-ncx"]').click();
      await pageEl.locator('sl-checkbox[data-select-prompt="m-rel-set"]').click();

      const selBar = pageEl.locator('[data-selection-bar]');
      await expect(selBar).toBeVisible();
      await expect(selBar.locator('[data-selected-count]')).toHaveText('2 cards selected');
      await expect(pageEl.locator('[data-prompt-card="m-ncx"]')).toHaveClass(/sel/);
      await expect(pageEl.locator('[data-prompt-card="m-rel-set"]')).toHaveClass(/sel/);
      await expect(pageEl.locator('[data-prompt-card="m-ncx"] [data-asker-deleted]')).toHaveText(
        '(deleted)'
      );
      await expect(
        pageEl.locator('[data-prompt-card="m-rel-set"] [data-asker-deleted]')
      ).toHaveCount(0);
      await expect(
        pageEl.locator('[data-prompt-card="m-ncx"] scion-chat-decisions [data-card-menu]')
      ).toBeVisible();

      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth
      );
      expect(overflow).toBeLessThanOrEqual(1);

      await shotViewport(page, testInfo, theme, 'x5-profile-multiselect');
    });

    test('X6: Project Decisions Made tab with Provenance = Dismissed filter', async ({
      page,
    }, testInfo) => {
      await setupDecisionsPageShellRoutes(page, theme);

      const entries = [
        {
          answerId: 'ans-d234',
          at: at(11),
          outcome: 'dismiss',
          provenance: 'direct',
          surface: 'decisions_page',
          choiceId: '$dismissed',
          choiceLabel: 'Dismissed',
          dismissReason: 'Stale: the release already shipped.',
          matchedDefault: false,
          answeredByUserId: SELF_ID,
          answeredByName: 'alice',
          chatKey: GENERAL_THREAD_ID,
          conversationLabel: '#release',
          conversationAvailable: true,
          responseMessageId: 'resp-dsm-set',
          responseAvailable: true,
          decision: {
            rowId: 'r-d234',
            promptMessageId: 'm-rel-set',
            promptAvailable: true,
            key: 'D234',
            ref: 'D234',
            version: 1,
            question: 'Ship v0.9 today?',
            askerAgentId: ASKER_ID,
            askerAgentName: ASKER_NAME,
          },
        },
        {
          answerId: 'ans-17',
          at: at(11),
          outcome: 'dismiss',
          provenance: 'direct',
          surface: 'decisions_page',
          choiceId: '$dismissed',
          choiceLabel: 'Dismissed',
          dismissReason: 'Stale: the release already shipped.',
          matchedDefault: false,
          answeredByUserId: SELF_ID,
          answeredByName: 'alice',
          chatKey: GENERAL_THREAD_ID,
          conversationLabel: '#release',
          conversationAvailable: true,
          responseMessageId: 'resp-dsm-set',
          responseAvailable: true,
          decision: {
            rowId: 'r-17',
            promptMessageId: 'm-rel-set',
            promptAvailable: true,
            key: '17',
            ref: '17',
            version: 1,
            question: 'Which approach?',
            askerAgentId: ASKER_ID,
            askerAgentName: ASKER_NAME,
          },
        },
        {
          answerId: 'ans-mig',
          at: at(9),
          outcome: 'dismiss',
          provenance: 'direct',
          surface: 'decisions_page',
          choiceId: '$dismissed',
          choiceLabel: 'Dismissed',
          matchedDefault: false,
          answeredByUserId: SELF_ID,
          answeredByName: 'alice',
          chatKey: `dm:agent:${AGENT_NCX_ID}:user:${SELF_ID}`,
          conversationLabel: 'DM with nc-x',
          conversationAvailable: true,
          responseMessageId: 'resp-dsm-mig',
          responseAvailable: true,
          decision: {
            rowId: 'r-mig',
            promptMessageId: 'm-ncx',
            promptAvailable: true,
            key: 'migrate',
            ref: 'migrate',
            version: 1,
            question: 'Run the backfill on staging first?',
            askerAgentId: AGENT_NCX_ID,
            askerAgentName: 'nc-x',
          },
        },
      ];

      await page.route(new RegExp(`/api/v1/projects/${PROJECT_A.id}/decisions(\\?|$)`), (route) =>
        route.fulfill({
          json: {
            counts: { waitingOnYou: 1, waitingOnOthers: 0 },
            entries,
            agents: [
              { id: ASKER_ID, name: ASKER_NAME },
              { id: AGENT_NCX_ID, name: 'nc-x', hint: '(deleted)' },
            ],
            answerers: [{ id: SELF_ID, name: 'alice' }],
            conversations: [{ key: GENERAL_THREAD_ID, label: '#release' }],
          },
        })
      );

      await page.goto(`/projects/${PROJECT_A.id}/decisions?tab=made&provenance=dismissed`, {
        waitUntil: 'domcontentloaded',
      });
      const pageEl = page.locator('scion-page-project-decisions');
      await expect(pageEl).toBeVisible();
      await expect(
        pageEl.locator('scion-decisions-filter-panel scion-filter-box input')
      ).toHaveValue('provenance:dismissed');

      if (isPhone(page)) {
        const mobileItem = pageEl.locator('[data-mobile-answer="ans-d234"]');
        await expect(mobileItem).toBeVisible();
        await expect(mobileItem).toContainText('Dismissed');
        await expect(mobileItem.locator('[data-dismiss-reason]')).toHaveText(
          'Stale: the release already shipped.'
        );
      } else {
        const tableRow = pageEl.locator('tr[data-answer-row="ans-d234"]');
        await expect(tableRow).toBeVisible();
        await expect(tableRow).toContainText('Dismissed');
        await expect(tableRow.locator('[data-dismiss-reason]')).toHaveText(
          'Stale: the release already shipped.'
        );
      }

      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth
      );
      expect(overflow).toBeLessThanOrEqual(1);

      await shotViewport(page, testInfo, theme, 'x6-made-dismissed');
    });

    test('X7: conversation dismissal response lines (expanded set + undelivered gone agent)', async ({
      page,
    }, testInfo) => {
      const sharedDismissed = dismissedInfo(11, {
        reason: 'Stale: the release already shipped.',
        responseMessageId: 'resp-x7-set',
      });
      const setPrompt = setDecisions('prompt-x7').map((d) => withDismissed(d, sharedDismissed));
      const thread: DecisionThread = {
        messages: [
          row('prompt-x7', 0, 'decision-prompt', 'Three calls before the release.', 'asker'),
          row(
            'resp-x7-set',
            11,
            'decision-response',
            'alice dismissed 2 decisions: D234, 17 · Stale: the release already shipped.',
            'bob'
          ),
          row('resp-x7-gone', 9, 'decision-response', 'alice dismissed migrate', 'bob', {
            dispatchState: 'failed',
            dispatchFailureReason: 'agent gone',
          }),
        ],
        messageDecisions: { 'prompt-x7': setPrompt },
        messageDecisionIntros: { 'prompt-x7': 'Three calls before the release.' },
        messageDecisionAnswerModes: { 'prompt-x7': 'set' },
        messageDecisionResponseSets: {
          'resp-x7-set': {
            promptMessageId: 'prompt-x7',
            batchId: 'batch-x7-set',
            answers: [
              {
                rowId: 'prompt-x7-d234',
                ref: 'D234',
                key: 'D234',
                version: 1,
                question: 'Ship v0.9 today?',
                choiceId: '$dismissed',
                label: 'Dismissed',
                provenance: 'direct',
                outcome: 'dismiss',
                dismissReason: 'Stale: the release already shipped.',
                answeredByUserId: 'user-alice',
                answeredByName: 'alice',
                promptMessageId: 'prompt-x7',
              },
              {
                rowId: 'prompt-x7-17',
                ref: '17',
                key: '17',
                version: 1,
                question: 'Which approach?',
                choiceId: '$dismissed',
                label: 'Dismissed',
                provenance: 'direct',
                outcome: 'dismiss',
                dismissReason: 'Stale: the release already shipped.',
                answeredByUserId: 'user-alice',
                answeredByName: 'alice',
                promptMessageId: 'prompt-x7',
              },
            ],
          },
          'resp-x7-gone': {
            promptMessageId: 'prompt-x7',
            batchId: 'batch-x7-gone',
            answers: [
              {
                rowId: 'prompt-x7-mig',
                ref: 'migrate',
                key: 'migrate',
                version: 1,
                question: 'Run the backfill on staging first?',
                choiceId: '$dismissed',
                label: 'Dismissed',
                provenance: 'direct',
                outcome: 'dismiss',
                answeredByUserId: 'user-alice',
                answeredByName: 'alice',
                promptMessageId: 'prompt-x7',
              },
            ],
          },
        },
      };

      await openDecisionsChat(page, theme, thread);
      const setResp = message(page, 'resp-x7-set').locator('scion-chat-decision-response');
      await setResp.locator('summary').click();
      await expect(setResp.locator('details[data-outcome="dismiss"]')).toHaveAttribute('open', '');
      await expect(setResp).toContainText('alice dismissed 2 decisions');
      await expect(setResp).toContainText('Stale: the release already shipped.');
      await expect(setResp.locator('.answers li .key')).toHaveCount(2);

      const goneResp = message(page, 'resp-x7-gone').locator('scion-chat-decision-response');
      await expect(goneResp).toContainText('alice dismissed migrate');
      await expect(goneResp.locator('.failed')).toContainText('not delivered (agent gone)');

      await shotMessages(page, testInfo, theme, 'x7-chat-response-line', [
        'resp-x7-set',
        'resp-x7-gone',
      ]);
    });

    test('X8: set with one row dismissed and remaining rows ready at 390 px and desktop, plus 409 dismissed handling', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-x8');
      decisions[1] = withDismissed(
        decisions[1],
        dismissedInfo(11, { reason: 'Not my area; ask bob.' })
      );
      await openDecisionsChat(
        page,
        theme,
        setThread('prompt-x8', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-x8');

      const c = card(page, 'prompt-x8');
      await c.locator('[data-row="prompt-x8-d234"] button[data-choice="yes"]').click();
      await c.locator('[data-row="prompt-x8-ship-now"] button[data-choice="no"]').click();

      await expect(c.locator('[data-row="prompt-x8-17"] .wd-reason')).toHaveText(
        'Not my area; ask bob.'
      );
      await expect(c.locator('.counter')).toHaveText('0 remaining · 1 dismissed');
      await expect(c.locator('sl-button.submit')).not.toHaveAttribute('aria-disabled', 'true');

      const overflow = await c.locator('.card').evaluate((el) => el.scrollWidth - el.clientWidth);
      expect(overflow).toBeLessThanOrEqual(1);
      const titleBox = (await c.locator('.card-head .title').boundingBox())!;
      const menuBox = (await c.locator('.card-head .menu').boundingBox())!;
      expect(titleBox).not.toBeNull();
      expect(menuBox).not.toBeNull();
      expect(Math.abs(menuBox.y - titleBox.y)).toBeLessThan(12);

      await shotMessages(page, testInfo, theme, 'x8-set-dismissed-ready', ['prompt-x8']);

      // Also verify 409 answer_set_changed when another row was dismissed meanwhile
      const fresh = [...decisions];
      fresh[2] = withDismissed(fresh[2], dismissedInfo(12, { byName: 'bob' }));
      await page.route('**/api/v1/decision-prompts/prompt-x8/answers', (route) =>
        route.fulfill({
          status: 409,
          json: {
            error: {
              code: 'answer_set_changed',
              message: 'the decisions to answer changed; refresh and submit again',
              details: {
                decisions: fresh,
                reasons: { 'prompt-x8-ship-now': 'dismissed' },
              },
            },
          },
        })
      );
      await c.locator('sl-button.submit').click();
      await expect(c.locator('sl-alert.changed')).toContainText(
        'This changed while you were choosing: ship-now was dismissed.'
      );
    });

    test('X9: too recent to dismiss (disabled menu item with countdown on chat card and Waiting on you)', async ({
      page,
    }, testInfo) => {
      const freshCreatedAt = new Date(Date.now() - 2 * 60_000 - 15_000).toISOString();
      const tocDecision = decision({
        messageId: 'prompt-x9',
        rowId: 'prompt-x9-toc',
        key: 'toc',
        question: 'Split the CLI reference into one page per command?',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'keep', label: 'Keep one page' },
        ],
      });
      const thread: DecisionThread = {
        messages: [
          row(
            'prompt-x9',
            0,
            'decision-prompt',
            'Quick one on the reference layout.\n\n(fallback)',
            'asker',
            { createdAt: freshCreatedAt }
          ),
        ],
        messageDecisions: { 'prompt-x9': [tocDecision] },
        messageDecisionIntros: { 'prompt-x9': 'Quick one on the reference layout.' },
        messageDecisionAnswerModes: { 'prompt-x9': 'single' },
      };

      await openDecisionsChat(page, theme, thread);
      const c = card(page, 'prompt-x9');
      await c.locator('[data-card-menu-trigger]').click();
      const dsmItem = c.locator('sl-menu-item[data-dismiss-menu-item]');
      await expect(dsmItem).toBeVisible();
      await expect(dsmItem).toHaveAttribute('disabled', '');
      const tooltip = c.locator('sl-tooltip[data-dismiss-tooltip]');
      await expect(tooltip).toHaveAttribute('content', /Dismiss available in [23] min/);

      await shotViewport(page, testInfo, theme, 'x9-too-recent-chat');

      // Also verify Waiting on you card checkbox disabled + skipped by Select all
      await setupDecisionsPageShellRoutes(page, theme);
      await page.route(new RegExp(`/api/v1/projects/${PROJECT_A.id}/decisions(\\?|$)`), (route) =>
        route.fulfill({
          json: {
            counts: { waitingOnYou: 1, waitingOnOthers: 0 },
            prompts: [
              {
                promptMessageId: 'prompt-x9',
                promptAvailable: true,
                chatKey: 'topic-docs',
                conversationLabel: '#docs',
                conversationAvailable: true,
                askerAgentId: AGENT_DOCS_ID,
                askerAgentName: 'docs-bot',
                createdAt: freshCreatedAt,
                answerMode: 'single',
                waitingOnYou: true,
                canAnswer: true,
                decisions: [tocDecision],
              },
            ],
            agents: [{ id: AGENT_DOCS_ID, name: 'docs-bot' }],
            answerers: [],
            conversations: [{ key: 'topic-docs', label: '#docs' }],
          },
        })
      );

      await page.goto(`/projects/${PROJECT_A.id}/decisions`, { waitUntil: 'domcontentloaded' });
      const pageEl = page.locator('scion-page-project-decisions');
      await expect(pageEl.locator('[data-prompt-card="prompt-x9"]')).toBeVisible();
      await expect(pageEl.locator('sl-checkbox[data-select-prompt="prompt-x9"]')).toHaveAttribute(
        'disabled',
        ''
      );
      await expect(
        pageEl.locator('sl-tooltip[data-select-prompt-tooltip="prompt-x9"]')
      ).toHaveAttribute('content', /Dismiss available in [23] min/);
      await expect(pageEl.locator('sl-checkbox[data-select-all-prompts]')).toHaveAttribute(
        'disabled',
        ''
      );
      await expect(
        pageEl.locator('[data-prompt-card="prompt-x9"] [data-dismiss-available-at]')
      ).toHaveText(/· dismiss available at \d{2}:\d{2}/);

      await shotViewport(page, testInfo, theme, 'x9-too-recent-page');
    });
  });
}
