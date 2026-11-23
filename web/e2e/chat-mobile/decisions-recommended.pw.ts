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
 * Recorded decisions (ptone/scion#8477, design for ptone/scion#8443 section
 * 4.3): decided chat cards keep a muted recommended marker, and Made entries
 * on the project and profile Decisions pages open a detail panel with every
 * offered choice. Light and dark themes on desktop (1440 px) and mobile
 * (390 px).
 *
 * Screenshots are written to Playwright's per-test output directory and
 * mirrored to `REC_EVIDENCE_DIR` when set.
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
  SELF_ID,
  answer,
  at,
  decision,
  routeDecisionThread,
  row,
  withdrawnInfo,
  type DecisionThread,
} from './decisions-mock.js';

type Theme = 'light' | 'dark';
type Json = Record<string, unknown>;

const PROJECTS = new Set(['desktop-1440', 'chromium-390']);
const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 768;

const REGION_CHOICES = [
  { id: 'eu', label: 'Europe', description: 'Lowest latency for most current users.' },
  { id: 'us', label: 'United States', description: 'Closest to the build farm; cheapest egress.' },
  { id: 'ap', label: 'Asia Pacific', description: 'Fastest for the new Tokyo team.' },
];
const YES_NO = [
  { id: 'yes', label: 'Yes' },
  { id: 'no', label: 'No' },
];
const OTHER = { label: 'Somewhere else', description: 'Name a region.' };

const message = (page: Page, id: string): Locator => page.locator(`[id="msg-${id}"]`);
const card = (page: Page, id: string): Locator => message(page, id).locator('scion-chat-decisions');

function mirrorEvidence(testInfo: TestInfo, outFile: string, filename: string): void {
  const evidenceDir = process.env.REC_EVIDENCE_DIR;
  if (evidenceDir && PROJECTS.has(testInfo.project.name)) {
    fs.mkdirSync(evidenceDir, { recursive: true });
    fs.copyFileSync(outFile, path.join(evidenceDir, filename));
  }
}

function shotName(testInfo: TestInfo, theme: Theme, name: string): string {
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  return `rec-${project}-${theme}-${name}.png`;
}

async function shotLocator(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string,
  target: Locator
): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(250);
  const filename = shotName(testInfo, theme, name);
  const outFile = testInfo.outputPath(filename);
  await target.screenshot({ path: outFile });
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });
  mirrorEvidence(testInfo, outFile, filename);
}

async function shotViewport(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string
): Promise<void> {
  // The app shell scrolls inside its own container, so grow the viewport to
  // the page content instead of relying on a full-page capture.
  const size = page.viewportSize()!;
  const contentHeight = await page.evaluate(() => {
    let max = document.documentElement.scrollHeight;
    for (const el of Array.from(document.querySelectorAll('*'))) {
      max = Math.max(max, el.scrollHeight);
      if (el.shadowRoot) {
        for (const inner of Array.from(el.shadowRoot.querySelectorAll('*'))) {
          max = Math.max(max, inner.scrollHeight);
        }
      }
    }
    return max;
  });
  await page.setViewportSize({ width: size.width, height: Math.min(contentHeight + 200, 4000) });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(400);
  const filename = shotName(testInfo, theme, name);
  const outFile = testInfo.outputPath(filename);
  await page.screenshot({ path: outFile });
  await page.setViewportSize(size);
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });
  mirrorEvidence(testInfo, outFile, filename);
}

async function assertNoHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth
  );
  expect(overflow).toBeLessThanOrEqual(1);
}

// ---------------------------------------------------------------------------
// Chat cards
// ---------------------------------------------------------------------------

function prompt(id: string, minutes: number, decisions: Json[], mode: 'single' | 'set'): Json {
  return { id, minutes, decisions, mode };
}

function cardsThread(): DecisionThread {
  const dismissed = {
    at: at(9),
    byUserId: SELF_ID,
    byName: 'alice',
    reason: 'Superseded by the migration plan.',
    surface: 'web',
    responseMessageId: 'resp-dsm',
  };
  const prompts = [
    prompt(
      'p-region',
      0,
      [
        decision({
          messageId: 'p-region',
          rowId: 'r-region',
          key: 'region',
          question: 'Which region for the new cluster?',
          defaultChoiceId: 'us',
          choices: REGION_CHOICES,
          status: 'decided',
          decision: answer('eu', 'Europe', SELF_ID, 'alice', 2),
        }),
      ],
      'single'
    ),
    prompt(
      'p-ship',
      3,
      [
        decision({
          messageId: 'p-ship',
          rowId: 'r-ship',
          key: 'ship',
          question: 'Ship v0.9 today?',
          defaultChoiceId: 'yes',
          choices: YES_NO,
          status: 'decided',
          decision: answer('yes', 'Yes', SELF_ID, 'alice', 4, { matchedDefault: true }),
        }),
      ],
      'single'
    ),
    prompt(
      'p-set',
      5,
      [
        decision({
          messageId: 'p-set',
          rowId: 'r-tag',
          key: 'tag',
          version: 1,
          question: 'Tag the release candidate?',
          defaultChoiceId: 'yes',
          choices: YES_NO,
          status: 'replaced',
          replacedBy: { rowId: 'r-tag-2', messageId: 'p-tag-2', version: 2, at: at(7) },
        }),
        decision({
          messageId: 'p-set',
          rowId: 'r-notes',
          key: 'notes',
          question: 'Publish release notes?',
          defaultChoiceId: 'no',
          choices: YES_NO,
          status: 'withdrawn',
          withdrawn: withdrawnInfo(8, 'Notes moved to the docs site.'),
        }),
        {
          ...decision({
            messageId: 'p-set',
            rowId: 'r-freeze',
            key: 'freeze',
            question: 'Freeze main during the cut?',
            defaultChoiceId: 'yes',
            choices: YES_NO,
          }),
          status: 'dismissed',
          dismissed,
        },
      ],
      'set'
    ),
  ];
  const thread: DecisionThread = {
    messages: [],
    messageDecisions: {},
    messageDecisionIntros: {},
    messageDecisionAnswerModes: {},
  };
  for (const p of prompts) {
    const id = p.id as string;
    thread.messages.push(
      row(id, p.minutes as number, 'decision-prompt', 'Decision prompt (fallback)', 'asker')
    );
    thread.messageDecisions![id] = p.decisions as Json[];
    thread.messageDecisionIntros![id] = 'A call before I continue.';
    thread.messageDecisionAnswerModes![id] = p.mode as 'single' | 'set';
  }
  return thread;
}

async function openCardsChat(page: Page, theme: Theme, thread: DecisionThread): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.addInitScript((t) => {
      localStorage.setItem('scion-theme', t);
      window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, 'web.decisions': true };
    }, theme);
    await routeDecisionThread(p, thread);
  });
  if (isPhone(page)) {
    await openGeneralThread(page);
  }
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
  await expect(message(page, thread.messages[0].id as string)).toBeVisible({ timeout: 15_000 });
}

// ---------------------------------------------------------------------------
// Decisions pages
// ---------------------------------------------------------------------------

const PROJ_REF = { id: PROJECT_A.id, name: 'scion', slug: 'scion' };

function summary(o: {
  rowId: string;
  key: string;
  question: string;
  choices: Json[];
  defaultChoiceId?: string;
  status?: string;
  version?: number;
  other?: Json;
  replacedBy?: Json;
  promptAvailable?: boolean;
}): Json {
  return {
    rowId: o.rowId,
    promptMessageId: `m-${o.rowId}`,
    promptAvailable: o.promptAvailable ?? true,
    key: o.key,
    ref: o.key,
    version: o.version ?? 1,
    status: o.status ?? 'decided',
    question: o.question,
    choices: o.choices,
    ...(o.defaultChoiceId ? { defaultChoiceId: o.defaultChoiceId } : {}),
    ...(o.other ? { other: o.other } : {}),
    ...(o.replacedBy ? { replacedBy: o.replacedBy } : {}),
    askerAgentId: ASKER_ID,
    askerAgentName: ASKER_NAME,
  };
}

function madeEntry(answerId: string, minutes: number, decisionSummary: Json, extra: Json): Json {
  return {
    answerId,
    at: at(minutes),
    provenance: 'direct',
    outcome: 'answer',
    surface: 'decisions_page',
    matchedDefault: false,
    answeredByUserId: SELF_ID,
    answeredByName: 'alice',
    chatKey: GENERAL_THREAD_ID,
    conversationLabel: '#release',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    project: PROJ_REF,
    decision: decisionSummary,
    ...extra,
  };
}

function madeEntries(): Json[] {
  const interpretedEu = {
    choiceId: 'eu',
    label: 'Europe',
    provenance: 'agent_interpreted',
    interpretedByAgentId: ASKER_ID,
    surface: 'chat',
    responseMessageId: '',
    at: at(1),
  };
  return [
    madeEntry(
      'ans-region',
      2,
      summary({
        rowId: 'region',
        key: 'region',
        question: 'Which region for the new cluster?',
        choices: REGION_CHOICES,
        defaultChoiceId: 'us',
        other: OTHER,
      }),
      {
        choiceId: 'ap',
        label: 'Asia Pacific',
        outcome: 'override',
        previousChoiceLabel: 'Europe',
        interpretedAnswer: interpretedEu,
      }
    ),
    madeEntry(
      'ans-ship',
      4,
      summary({
        rowId: 'ship',
        key: 'ship',
        question: 'Ship v0.9 today?',
        choices: YES_NO,
        defaultChoiceId: 'yes',
      }),
      { choiceId: 'yes', label: 'Yes', matchedDefault: true }
    ),
    madeEntry(
      'ans-host',
      6,
      summary({
        rowId: 'host',
        key: 'host',
        question: 'Where should the docs live?',
        choices: [
          { id: 'site', label: 'Docs site' },
          { id: 'wiki', label: 'Wiki' },
        ],
        defaultChoiceId: 'site',
        other: OTHER,
      }),
      { choiceId: '$other', label: 'Somewhere else', otherText: 'The repository README' }
    ),
    madeEntry(
      'ans-freeze',
      9,
      summary({
        rowId: 'freeze',
        key: 'freeze',
        question: 'Freeze main during the cut?',
        choices: YES_NO,
        defaultChoiceId: 'yes',
        status: 'dismissed',
      }),
      {
        choiceId: '$dismissed',
        label: 'Dismissed',
        outcome: 'dismiss',
        dismissReason: 'Superseded by the migration plan.',
        interpretedAnswer: { ...interpretedEu, choiceId: 'no', label: 'No' },
      }
    ),
    madeEntry(
      'ans-tag',
      10,
      summary({
        rowId: 'tag',
        key: 'tag',
        question: 'Tag the release candidate?',
        choices: YES_NO,
        defaultChoiceId: 'yes',
        status: 'replaced',
        replacedBy: { rowId: 'tag-2', version: 2 },
        promptAvailable: false,
      }),
      { choiceId: 'no', label: 'No' }
    ),
    madeEntry(
      'ans-backfill',
      11,
      summary({
        rowId: 'backfill',
        key: 'backfill',
        question: 'Run the backfill on staging first?',
        choices: YES_NO,
        defaultChoiceId: 'yes',
      }),
      {
        choiceId: 'no',
        label: 'No',
        provenance: 'agent_interpreted',
        interpretedByAgentId: ASKER_ID,
        interpretedByAgentName: ASKER_NAME,
        awaitingConfirmation: true,
        surface: 'chat',
      }
    ),
  ];
}

async function setupPageShellRoutes(page: Page, theme: Theme): Promise<void> {
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
      json: { projects: [PROJ_REF], _capabilities: { actions: ['read'] } },
    })
  );
  await page.route(`/api/v1/projects/${PROJECT_A.id}`, (route) =>
    route.fulfill({
      json: { ...PROJ_REF, _capabilities: { actions: ['read', 'update'] } },
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
  await page.route(new RegExp(`/api/v1/agents/${ASKER_ID}(\\?|$)`), (route) =>
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

const MADE_FACETS = {
  agents: [{ id: ASKER_ID, name: ASKER_NAME, projectId: PROJECT_A.id }],
  answerers: [{ id: SELF_ID, name: 'alice' }],
  conversations: [{ key: GENERAL_THREAD_ID, label: '#release', projectId: PROJECT_A.id }],
};

/** The visible Made entry of `answerId`: the table row on desktop, the item on phones. */
function entryOf(scope: Locator, page: Page, answerId: string): Locator {
  return isPhone(page)
    ? scope.locator(`[data-mobile-answer="${answerId}"]`)
    : scope.locator(`tr[data-answer-row="${answerId}"]`);
}

/** The visible detail panel of `answerId`. */
function panelOf(scope: Locator, page: Page, answerId: string): Locator {
  return isPhone(page)
    ? scope.locator(`[data-mobile-answer="${answerId}"] [data-made-detail]`)
    : scope.locator(`tr[data-detail-row="${answerId}"] [data-made-detail]`);
}

async function assertMadePanels(scope: Locator, page: Page): Promise<void> {
  // Collapsed markers on every outcome except the dismissal.
  await expect(entryOf(scope, page, 'ans-region').locator('[data-rec-marker]')).toHaveText(
    'recommended: United States'
  );
  await expect(entryOf(scope, page, 'ans-ship').locator('[data-rec-marker]')).toHaveText(
    'recommended'
  );
  await expect(entryOf(scope, page, 'ans-host').locator('[data-rec-marker]')).toHaveText(
    'recommended: Docs site'
  );
  await expect(entryOf(scope, page, 'ans-freeze').locator('[data-rec-marker]')).toHaveCount(0);
  await expect(
    entryOf(scope, page, 'ans-backfill').locator('[data-awaiting-confirmation]')
  ).toHaveText('awaiting confirmation');

  // Keyboard: the disclosure is a real button operated with Enter and Space.
  const regionToggle = entryOf(scope, page, 'ans-region').getByRole('button', {
    name: 'Show choices for region v1',
  });
  await regionToggle.focus();
  await page.keyboard.press('Enter');
  await expect(
    entryOf(scope, page, 'ans-region').getByRole('button', {
      name: 'Hide choices for region v1',
      expanded: true,
    })
  ).toBeVisible();
  const freezeToggle = entryOf(scope, page, 'ans-freeze').getByRole('button', {
    name: 'Show choices for freeze v1',
  });
  await freezeToggle.focus();
  await page.keyboard.press('Space');
  await entryOf(scope, page, 'ans-host').locator('[data-made-toggle]').click();
  await entryOf(scope, page, 'ans-tag').locator('[data-made-toggle]').click();

  const region = panelOf(scope, page, 'ans-region');
  await expect(region.locator('[data-detail-choice]')).toHaveCount(4);
  await expect(region.locator('[data-detail-choice][data-picked]')).toHaveAttribute(
    'data-detail-choice',
    'ap'
  );
  await expect(region.locator('[data-detail-choice="us"] [data-detail-recommended]')).toHaveText(
    'recommended'
  );
  await expect(region.locator('[data-detail-choice="eu"] [data-detail-desc]')).toHaveText(
    'Lowest latency for most current users.'
  );
  await expect(region.locator('[data-detail-interpreted]')).toHaveText('Was interpreted as Europe');
  await expect(region.locator('[data-detail-open-in-chat]')).toHaveAttribute(
    'href',
    /#msg-m-region$/
  );

  const freeze = panelOf(scope, page, 'ans-freeze');
  await expect(freeze.locator('[data-detail-choice][data-picked]')).toHaveCount(0);
  await expect(freeze.locator('[data-detail-dismissed]')).toHaveText(
    'Dismissed: Superseded by the migration plan.'
  );
  await expect(freeze.locator('[data-detail-interpreted]')).toHaveText('Was interpreted as No');

  const host = panelOf(scope, page, 'ans-host');
  await expect(host.locator('[data-detail-choice="$other"]')).toHaveAttribute('data-picked', '');
  await expect(host.locator('[data-detail-other-text]')).toHaveText('“The repository README”');

  const tag = panelOf(scope, page, 'ans-tag');
  await expect(tag.locator('[data-detail-superseded]')).toHaveText('Superseded by v2');
  await expect(tag.locator('[data-detail-open-in-chat]')).toHaveCount(0);
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`recorded decisions (${theme})`, () => {
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('R1: decided, replaced, withdrawn and dismissed cards keep the recommended marker', async ({
      page,
    }, testInfo) => {
      const thread = cardsThread();
      await openCardsChat(page, theme, thread);

      const region = card(page, 'p-region');
      await expect(region.locator('[data-choice="us"] [data-rec-mark]')).toHaveText('recommended');
      await expect(region.locator('[data-choice="eu"].chosen')).toBeVisible();
      await expect(region.locator('[data-choice="us"]')).not.toHaveClass(/recd/);

      const ship = card(page, 'p-ship');
      await expect(ship.locator('[data-choice="yes"] [data-rec-mark]')).toHaveText('recommended');
      await expect(ship.locator('[data-choice="yes"]')).toHaveAttribute('variant', 'primary');

      const set = card(page, 'p-set');
      for (const [rowId, choice] of [
        ['r-tag', 'yes'],
        ['r-notes', 'no'],
        ['r-freeze', 'yes'],
      ] as const) {
        const c = set.locator(`.row[data-row="${rowId}"] [data-choice="${choice}"]`);
        await expect(c.locator('[data-rec-mark]')).toHaveText('recommended');
        await expect(c).not.toHaveAttribute('data-look', 'outline');
      }
      // Screen readers hear the recommendation on every terminal card.
      await expect(
        page.getByRole('button', { name: 'United States (recommended)', exact: false })
      ).toHaveCount(1);

      await shotLocator(page, testInfo, theme, 'r1-cards-region', message(page, 'p-region'));
      await shotLocator(page, testInfo, theme, 'r1-cards-ship', message(page, 'p-ship'));
      await shotLocator(page, testInfo, theme, 'r1-cards-set', message(page, 'p-set'));
    });

    test('R2: project Made detail panels', async ({ page }, testInfo) => {
      await setupPageShellRoutes(page, theme);
      await page.route(new RegExp(`/api/v1/projects/${PROJECT_A.id}/decisions(\\?|$)`), (route) =>
        route.fulfill({
          json: {
            counts: { waitingOnYou: 0, waitingOnOthers: 0 },
            entries: madeEntries(),
            ...MADE_FACETS,
          },
        })
      );
      await page.goto(`/projects/${PROJECT_A.id}/decisions?tab=made`, {
        waitUntil: 'domcontentloaded',
      });
      const pageEl = page.locator('scion-page-project-decisions');
      await expect(entryOf(pageEl, page, 'ans-region')).toBeVisible({ timeout: 15_000 });

      await assertMadePanels(pageEl, page);
      await assertNoHorizontalOverflow(page);
      await shotViewport(page, testInfo, theme, 'r2-project-made-panels');
    });

    test('R3: profile Made detail panels', async ({ page }, testInfo) => {
      await setupPageShellRoutes(page, theme);
      await page.route('**/api/v1/users/me/decisions**', (route) => {
        const url = new URL(route.request().url());
        if (url.searchParams.get('tab') !== 'made') {
          return route.fulfill({
            json: {
              sort: 'recent',
              counts: { waitingOnYou: 0, waitingOnOthers: 0 },
              prompts: [],
              ...MADE_FACETS,
              projects: [PROJ_REF],
              truncatedProjects: false,
            },
          });
        }
        return route.fulfill({
          json: {
            sort: 'recent',
            counts: { waitingOnYou: 0, waitingOnOthers: 0 },
            entries: madeEntries(),
            ...MADE_FACETS,
            projects: [PROJ_REF],
            truncatedProjects: false,
          },
        });
      });
      await page.goto('/profile/decisions?tab=made&sort=recent', {
        waitUntil: 'domcontentloaded',
      });
      const pageEl = page.locator('scion-page-profile-decisions');
      await expect(entryOf(pageEl, page, 'ans-region')).toBeVisible({ timeout: 15_000 });

      await assertMadePanels(pageEl, page);
      await assertNoHorizontalOverflow(page);
      await shotViewport(page, testInfo, theme, 'r3-profile-made-panels');
    });
  });
}
