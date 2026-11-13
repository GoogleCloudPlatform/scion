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
 * Project Decisions page and project-detail header button/badge (ptone/scion#7800),
 * covering approved mocks P3-1..P3-6 in light and dark themes on desktop (1440x900)
 * and mobile (390x844).
 *
 * Screenshots are written only to Playwright's per-test artifact output directory
 * (`testInfo.outputPath(...)`); nothing is written into tracked repository paths.
 */

import { test, expect, type Page, type Route, type TestInfo } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

type Theme = 'light' | 'dark';

const PROJECTS = new Set(['desktop-1440', 'chromium-390']);
const PROJECT_ID = 'p-1';
const SELF_ID = 'fixture-user';

interface RecordedAnswerCall {
  rowId: string;
  body: Record<string, unknown>;
}

interface MockDecisionsController {
  decisionsRequests: string[];
  answerCalls: RecordedAnswerCall[];
}

interface MockScenarioOptions {
  empty?: boolean;
}

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 640;

async function shot(page: Page, testInfo: TestInfo, theme: Theme, name: string): Promise<void> {
  await page.waitForTimeout(200);
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  const file = testInfo.outputPath(`project-decisions-${project}-${theme}-${name}.png`);
  await page.screenshot({ path: file, fullPage: false });
  await testInfo.attach(name, { path: file, contentType: 'image/png' });
}

async function setupProjectDecisionsMocks(
  page: Page,
  theme: Theme,
  options: MockScenarioOptions = {}
): Promise<MockDecisionsController> {
  const decisionsRequests: string[] = [];
  const answerCalls: RecordedAnswerCall[] = [];
  let purgedAnswered = false;

  await installEventSourceStub(page);
  await page.addInitScript((t: Theme) => {
    localStorage.setItem('scion-theme', t);
    window.__SCION_FEATURES__ = {
      ...window.__SCION_FEATURES__,
      'web.native_chat': true,
      'web.chat_choices': true,
      'web.project_decisions': true,
    };
  }, theme);

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: { id: SELF_ID, name: 'Alice', email: 'alice@example.test' },
    })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/users**', (route) => route.fulfill({ json: { users: [] } }));
  await page.route(/\/api\/v1\/chat\/spaces$/, (route) =>
    route.fulfill({
      json: {
        spaces: [
          {
            projectId: PROJECT_ID,
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
      void route.fulfill({ json: { peerIds: [] } });
      return;
    }
    void route.fulfill({ json: { dms: [] } });
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
        projects: [{ id: PROJECT_ID, name: 'scion', slug: 'scion' }],
        _capabilities: { actions: ['read'] },
      },
    })
  );
  await page.route(`/api/v1/projects/${PROJECT_ID}`, (route) =>
    route.fulfill({
      json: {
        id: PROJECT_ID,
        name: 'scion',
        slug: 'scion',
        _capabilities: { actions: ['read', 'update'] },
      },
    })
  );
  await page.route(/\/api\/v1\/projects\/p-1\/agents(\?|$)/, (route) =>
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

  const livePrompt = {
    promptMessageId: 'm-live',
    promptAvailable: true,
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: 'a-rel',
    askerAgentName: 'release-bot',
    createdAt: '2026-11-13T15:40:00Z',
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [
      {
        rowId: 'r-live',
        messageId: 'm-live',
        key: 'D234',
        ref: 'D234',
        version: 1,
        status: 'pending',
        askerAgentId: 'a-rel',
        askerAgentName: 'release-bot',
        question: 'Ship `v0.9` today?',
        defaultChoiceId: 'yes',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
          { id: 'wait', label: 'Wait for CI' },
        ],
      },
    ],
  };

  const purgedDmPrompt = {
    promptMessageId: 'm-purged',
    promptAvailable: false,
    chatKey: `dm:agent:a-rel:user:${SELF_ID}`,
    conversationLabel: 'DM with release-bot',
    conversationAvailable: true,
    askerAgentId: 'a-rel',
    askerAgentName: 'release-bot',
    createdAt: '2026-11-13T12:00:00Z',
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [
      {
        rowId: 'r-purged',
        messageId: 'm-purged',
        key: 'D198',
        ref: 'D198',
        version: 1,
        status: 'pending',
        askerAgentId: 'a-rel',
        askerAgentName: 'release-bot',
        question: 'Rotate your personal staging deploy key now?',
        defaultChoiceId: 'rotate',
        choices: [
          { id: 'rotate', label: 'Rotate key' },
          { id: 'keep', label: 'Keep existing key' },
        ],
      },
    ],
  };

  const othersPrompts = [
    {
      promptMessageId: 'm-other-confirm',
      promptAvailable: true,
      chatKey: 'topic-release',
      conversationLabel: '#release',
      conversationAvailable: true,
      askerAgentId: 'a-rel',
      askerAgentName: 'release-bot',
      createdAt: '2026-11-13T14:20:00Z',
      answerMode: 'single',
      waitingOnYou: false,
      canAnswer: false,
      waitingReasons: { 'r-other-confirm': 'other_user_confirm' },
      decisions: [
        {
          rowId: 'r-other-confirm',
          messageId: 'm-other-confirm',
          key: 'D210',
          ref: 'D210',
          version: 1,
          status: 'decided',
          askerAgentId: 'a-rel',
          askerAgentName: 'release-bot',
          question: 'Freeze the v0.9 release branch at 17:00 UTC?',
          choices: [
            { id: 'yes', label: 'Yes' },
            { id: 'no', label: 'No' },
          ],
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-bob',
            answeredByName: 'Bob',
            provenance: 'agent_interpreted',
            outcome: 'answer',
            surface: 'native',
            at: '2026-11-13T14:22:00Z',
          },
        },
      ],
    },
    {
      promptMessageId: 'm-read-only',
      promptAvailable: true,
      chatKey: 'topic-ops',
      conversationLabel: '#ops',
      conversationAvailable: true,
      askerAgentId: 'a-ops',
      askerAgentName: 'ops-bot',
      createdAt: '2026-11-13T13:10:00Z',
      answerMode: 'single',
      waitingOnYou: false,
      canAnswer: false,
      waitingReasons: { 'r-read-only': 'read_only' },
      decisions: [
        {
          rowId: 'r-read-only',
          messageId: 'm-read-only',
          key: 'D175',
          ref: 'D175',
          version: 1,
          status: 'pending',
          askerAgentId: 'a-ops',
          askerAgentName: 'ops-bot',
          question: 'Enable prod audit drain?',
          choices: [
            { id: 'yes', label: 'Yes' },
            { id: 'no', label: 'No' },
          ],
        },
      ],
    },
  ];

  const madePage1Entries = [
    {
      answerId: 'ans-override',
      decision: {
        rowId: 'r-d9',
        promptMessageId: 'm-d9',
        promptAvailable: true,
        key: 'D9',
        ref: 'D9',
        version: 1,
        question: 'Cut the RC from main?',
        askerAgentId: 'a-rel',
        askerAgentName: 'release-bot',
      },
      choiceId: 'no',
      label: 'No',
      matchedDefault: false,
      previousChoiceLabel: 'Yes',
      answeredByUserId: SELF_ID,
      answeredByName: 'Alice',
      provenance: 'direct',
      outcome: 'override',
      surface: 'decisions_page',
      at: '2026-11-13T15:52:00Z',
      chatKey: 'topic-release',
      conversationLabel: '#release',
      conversationAvailable: true,
      sourceMessageAvailable: false,
    },
    {
      answerId: 'ans-interp',
      decision: {
        rowId: 'r-d9',
        promptMessageId: 'm-d9',
        promptAvailable: true,
        key: 'D9',
        ref: 'D9',
        version: 1,
        question: 'Cut the RC from main?',
        askerAgentId: 'a-rel',
        askerAgentName: 'release-bot',
      },
      choiceId: 'yes',
      label: 'Yes',
      matchedDefault: true,
      answeredByUserId: 'u-bob',
      answeredByName: 'Bob',
      provenance: 'agent_interpreted',
      outcome: 'answer',
      surface: 'web',
      at: '2026-11-13T15:48:00Z',
      chatKey: 'topic-release',
      conversationLabel: '#release',
      conversationAvailable: true,
      sourceMessageId: 'msg-bob-reply',
      sourceMessageAvailable: true,
    },
    {
      answerId: 'ans-confirmed-deleted-user',
      decision: {
        rowId: 'r-d7',
        promptMessageId: 'm-d7',
        promptAvailable: false,
        key: 'D7',
        ref: 'D7',
        version: 2,
        question: 'Archive legacy staging logs?',
        askerAgentId: 'a-ops',
        askerAgentName: 'ops-bot',
      },
      choiceId: 'archive',
      label: 'Archive',
      matchedDefault: true,
      answeredByName: 'Former Member',
      provenance: 'direct',
      outcome: 'confirm',
      surface: 'slack',
      at: '2026-11-13T14:05:00Z',
      chatKey: 'topic-deleted',
      conversationLabel: '#retired-channel',
      conversationAvailable: false,
      sourceMessageId: 'msg-purged-reply',
      sourceMessageAvailable: false,
    },
  ];

  const madePage2Entries = [
    {
      answerId: 'ans-page2-dm',
      decision: {
        rowId: 'r-dm-old',
        promptMessageId: 'm-dm-old',
        promptAvailable: true,
        key: 'D3',
        ref: 'D3',
        version: 1,
        question: 'Page you during canary window?',
        askerAgentId: 'a-rel',
        askerAgentName: 'release-bot',
      },
      choiceId: 'slack',
      label: 'Slack only',
      matchedDefault: true,
      answeredByUserId: SELF_ID,
      answeredByName: 'Alice',
      provenance: 'direct',
      outcome: 'answer',
      surface: 'decisions_page',
      at: '2026-11-12T09:30:00Z',
      chatKey: `dm:agent:a-rel:user:${SELF_ID}`,
      conversationLabel: 'DM with release-bot',
      conversationAvailable: true,
      sourceMessageAvailable: false,
    },
  ];

  await page.route(/\/api\/v1\/projects\/p-1\/decisions(\?|$)/, (route: Route) => {
    const url = new URL(route.request().url());
    decisionsRequests.push(url.pathname + url.search);
    const tab = url.searchParams.get('tab') ?? 'pending';
    const limit = url.searchParams.get('limit');
    const waitingOnYou = options.empty ? 0 : purgedAnswered ? 1 : 2;
    const waitingOnOthers = options.empty ? 0 : 2;
    const facets = {
      agents: [
        { id: 'a-rel', name: 'release-bot' },
        { id: 'a-ops', name: 'ops-bot' },
      ],
      answerers: [
        { id: SELF_ID, name: 'Alice' },
        { id: 'u-bob', name: 'Bob' },
      ],
    };

    if (tab === 'made') {
      if (options.empty) {
        void route.fulfill({
          json: {
            counts: { waitingOnYou: 0, waitingOnOthers: 0 },
            entries: [],
            agents: [],
            answerers: [],
          },
        });
        return;
      }
      const provenance = url.searchParams.get('provenance');
      const cursor = url.searchParams.get('cursor');
      if (provenance === 'confirmed') {
        void route.fulfill({
          json: {
            counts: { waitingOnYou, waitingOnOthers },
            entries: [madePage1Entries[2]],
            ...facets,
          },
        });
        return;
      }
      if (cursor === 'cur-page-2') {
        void route.fulfill({
          json: {
            counts: { waitingOnYou, waitingOnOthers },
            entries: madePage2Entries,
            ...facets,
          },
        });
        return;
      }
      void route.fulfill({
        json: {
          counts: { waitingOnYou, waitingOnOthers },
          entries: madePage1Entries,
          nextCursor: 'cur-page-2',
          ...facets,
        },
      });
      return;
    }

    if (limit === '0') {
      void route.fulfill({
        json: {
          counts: { waitingOnYou, waitingOnOthers },
          prompts: [],
          agents: [],
          answerers: [],
        },
      });
      return;
    }

    if (options.empty) {
      void route.fulfill({
        json: {
          counts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          agents: [],
          answerers: [],
        },
      });
      return;
    }

    const prompts = purgedAnswered
      ? [livePrompt, ...othersPrompts]
      : [livePrompt, purgedDmPrompt, ...othersPrompts];

    void route.fulfill({
      json: {
        counts: { waitingOnYou, waitingOnOthers },
        prompts,
        ...facets,
      },
    });
  });

  await page.route(/\/api\/v1\/decisions\/([^/]+)\/answer$/, (route: Route) => {
    const match = route
      .request()
      .url()
      .match(/\/decisions\/([^/]+)\/answer$/);
    const rowId = match?.[1] ? decodeURIComponent(match[1]) : '';
    const body = (route.request().postDataJSON() ?? {}) as Record<string, unknown>;
    answerCalls.push({ rowId, body });
    if (rowId === 'r-purged') {
      purgedAnswered = true;
    }
    const choiceId = typeof body.choice_id === 'string' ? body.choice_id : 'rotate';
    void route.fulfill({
      json: {
        decision: {
          rowId,
          messageId: 'm-purged',
          key: 'D198',
          ref: 'D198',
          version: 1,
          status: 'decided',
          askerAgentId: 'a-rel',
          askerAgentName: 'release-bot',
          question: 'Rotate your personal staging deploy key now?',
          defaultChoiceId: 'rotate',
          choices: [
            { id: 'rotate', label: 'Rotate key' },
            { id: 'keep', label: 'Keep existing key' },
          ],
          decision: {
            choiceId,
            label: choiceId === 'rotate' ? 'Rotate key' : 'Keep existing key',
            answeredByUserId: SELF_ID,
            answeredByName: 'Alice',
            provenance: 'direct',
            outcome: 'answer',
            surface: 'decisions_page',
            at: '2026-11-13T16:00:00Z',
          },
        },
        responseMessageId: 'resp-purged-1',
      },
    });
  });

  return { decisionsRequests, answerCalls };
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`project decisions (${theme})`, () => {
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('P3-1 & P3-2: project-detail Decisions button + badge, Waiting on you live & purged cards, inline answer surface=decisions_page, and Waiting on others', async ({
      page,
    }, testInfo) => {
      const ctrl = await setupProjectDecisionsMocks(page, theme);

      await page.goto(`/projects/${PROJECT_ID}`, { waitUntil: 'domcontentloaded' });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

      const headerBtn = page.locator('[data-decisions-button]');
      await expect(headerBtn).toBeVisible({ timeout: 15_000 });
      await expect(headerBtn.locator('.decisions-badge')).toHaveText('2');
      await shot(page, testInfo, theme, '01-project-detail-header-badge');

      await headerBtn.click();
      await expect(page).toHaveURL(new RegExp(`/projects/${PROJECT_ID}/decisions$`));

      const pageEl = page.locator('scion-page-project-decisions');
      await expect(pageEl).toBeVisible();
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('2');

      // Live #release card has "Open in chat" permalink and no purged note.
      const liveCard = pageEl.locator('[data-prompt-card="m-live"]');
      await expect(liveCard).toBeVisible();
      await expect(liveCard.locator('[data-conv-link]')).toHaveText('#release');
      await expect(liveCard.locator('[data-open-in-chat]')).toHaveAttribute(
        'href',
        '/chat/space/p-1/thread/topic-release#msg-m-live'
      );
      await expect(liveCard.locator('[data-purged-note]')).toHaveCount(0);

      // Purged DM card shows "in your DM" + "Original message no longer available" and NO "Open in chat".
      const purgedCard = pageEl.locator('[data-prompt-card="m-purged"]');
      await expect(purgedCard).toBeVisible();
      await expect(purgedCard.locator('[data-conv-link]')).toHaveText('your DM');
      await expect(purgedCard.locator('[data-purged-note]')).toContainText(
        'Original message no longer available'
      );
      await expect(purgedCard.locator('[data-open-in-chat]')).toHaveCount(0);

      // Waiting on others is collapsed by default, then expands to show reason badges.
      const othersDetails = pageEl.locator('sl-details[data-waiting-others]');
      await expect(othersDetails).toBeVisible();
      await expect(othersDetails).not.toHaveAttribute('open', '');
      await shot(page, testInfo, theme, '02-waiting-on-you');

      await othersDetails.locator('summary').click();
      await expect(othersDetails).toHaveAttribute('open', '');
      await expect(othersDetails.locator('[data-others-row="r-other-confirm"]')).toContainText(
        'Bob to confirm'
      );
      await expect(othersDetails.locator('[data-others-row="r-read-only"]')).toContainText(
        'read-only for you'
      );

      // Answer the purged DM card inline and verify surface="decisions_page" + badge decrement.
      const rotateBtn = purgedCard.locator('sl-button[data-choice="rotate"]');
      await rotateBtn.scrollIntoViewIfNeeded();
      await rotateBtn.click();

      await expect.poll(() => ctrl.answerCalls.length).toBe(1);
      expect(ctrl.answerCalls[0]).toMatchObject({
        rowId: 'r-purged',
        body: { choice_id: 'rotate', surface: 'decisions_page' },
      });
      await expect(purgedCard).toHaveCount(0);
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('1');
    });

    test('P3-3 & P3-5: Made log desktop table vs mobile stacked list, Filters toggle, filter reload, and Load more pagination', async ({
      page,
    }, testInfo) => {
      const ctrl = await setupProjectDecisionsMocks(page, theme);

      await page.goto(`/projects/${PROJECT_ID}/decisions?tab=made`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

      const pageEl = page.locator('scion-page-project-decisions');
      const desktopTable = pageEl.locator('table.log[data-made-table]');
      const mobileList = pageEl.locator('[data-made-mobile-list]');
      const mobileFiltersBtn = pageEl.locator('[data-mobile-filters-toggle]');
      const filterControls = pageEl.locator('.filter-controls');

      if (isPhone(page)) {
        await expect(mobileList).toBeVisible({ timeout: 15_000 });
        await expect(desktopTable).toBeHidden();
        await expect(mobileFiltersBtn).toBeVisible();
        await expect(filterControls).toBeHidden();

        await expect(mobileList.locator('[data-mobile-answer]')).toHaveCount(3);
        await expect(mobileList).toContainText('Decisions page');
        await expect(mobileList).toContainText('(deleted user)');
        await shot(page, testInfo, theme, '03-made-log');

        // Toggle mobile filters open.
        await mobileFiltersBtn.click();
        await expect(filterControls).toBeVisible();
      } else {
        await expect(desktopTable).toBeVisible({ timeout: 15_000 });
        await expect(mobileList).toBeHidden();
        await expect(mobileFiltersBtn).toBeHidden();
        await expect(filterControls).toBeVisible();

        const overrideRow = desktopTable.locator('tr[data-answer-row="ans-override"]');
        await expect(overrideRow).toHaveClass(/follow/);
        await expect(overrideRow.locator('[data-answered-by]')).toHaveText('you');
        await expect(overrideRow.locator('[data-provenance]')).toContainText('changed');
        await expect(overrideRow.locator('[data-provenance]')).toContainText('from Yes');
        await expect(overrideRow.locator('[data-surface]')).toContainText('Decisions page');

        const interpRow = desktopTable.locator('tr[data-answer-row="ans-interp"]');
        await expect(interpRow.locator('[data-reply-link]')).toHaveAttribute(
          'href',
          '/chat/space/p-1/thread/topic-release#msg-msg-bob-reply'
        );

        const deletedRow = desktopTable.locator('tr[data-answer-row="ans-confirmed-deleted-user"]');
        await expect(deletedRow.locator('[data-answered-by]')).toContainText('(deleted user)');
        await expect(deletedRow.locator('[data-conv-unavailable]')).toHaveText('#retired-channel');
        await expect(deletedRow.locator('[data-reply-link]')).toHaveCount(0);

        await shot(page, testInfo, theme, '03-made-log');
      }

      // Load more appends cursor page 2.
      const loadMoreBtn = pageEl.locator('[data-load-more="made"]');
      await loadMoreBtn.scrollIntoViewIfNeeded();
      await loadMoreBtn.click();
      await expect
        .poll(() =>
          ctrl.decisionsRequests.some(
            (u) => u.includes('tab=made') && u.includes('cursor=cur-page-2')
          )
        )
        .toBe(true);
      if (isPhone(page)) {
        await expect(mobileList.locator('[data-mobile-answer]')).toHaveCount(4);
      } else {
        await expect(desktopTable.locator('tbody tr')).toHaveCount(4);
      }

      // Filter by Provenance = Confirmed reloads page 1 with provenance=confirmed.
      const provSelect = pageEl.locator('sl-select[data-filter="provenance"]');
      await provSelect.scrollIntoViewIfNeeded();
      await provSelect.evaluate((el) => {
        (el as HTMLElement & { value: string }).value = 'confirmed';
        el.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
      });

      await expect
        .poll(() =>
          ctrl.decisionsRequests.some(
            (u) => u.includes('tab=made') && u.includes('provenance=confirmed')
          )
        )
        .toBe(true);
      if (isPhone(page)) {
        await expect(mobileList.locator('[data-mobile-answer]')).toHaveCount(1);
      } else {
        await expect(desktopTable.locator('tbody tr')).toHaveCount(1);
      }
    });

    test('P3-6: empty Waiting on you state and See decisions made switcher to empty Made state', async ({
      page,
    }, testInfo) => {
      await setupProjectDecisionsMocks(page, theme, { empty: true });

      await page.goto(`/projects/${PROJECT_ID}/decisions`, { waitUntil: 'domcontentloaded' });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

      const pageEl = page.locator('scion-page-project-decisions');
      const pendingEmpty = pageEl.locator('[data-empty="pending"]');
      await expect(pendingEmpty).toBeVisible({ timeout: 15_000 });
      await expect(pendingEmpty).toContainText('Nothing is waiting on you');
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveCount(0);
      await shot(page, testInfo, theme, '04-empty-pending');

      await pageEl.locator('[data-see-made]').click();
      await expect(page).toHaveURL(new RegExp(`/projects/${PROJECT_ID}/decisions\\?tab=made$`));
      const madeEmpty = pageEl.locator('[data-empty="made"]');
      await expect(madeEmpty).toBeVisible();
      await expect(madeEmpty).toContainText('No decisions recorded yet');
      await shot(page, testInfo, theme, '05-empty-made');
    });
  });
}
