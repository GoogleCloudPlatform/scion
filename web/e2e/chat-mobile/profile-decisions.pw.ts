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
 * Profile > Decisions page and profile nav badge (ptone/scion#7996),
 * covering approved mocks R-1..R-4 in light and dark themes on desktop
 * (1440x900) and mobile (320/375/390 widths).
 *
 * Screenshots are written to Playwright's per-test output directory
 * (`testInfo.outputPath(...)`), and optionally mirrored to `DUX_EVIDENCE_DIR`
 * when capturing design-review evidence outside `/workspace`.
 */

import * as fs from 'node:fs';
import * as path from 'node:path';
import { test, expect, type Page, type Route, type TestInfo } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

type Theme = 'light' | 'dark';

const SELF_ID = 'u-alice';
const PROJ_SCION = { id: 'proj-1', name: 'scion', slug: 'scion' };
const PROJ_WEB = { id: 'proj-2', name: 'web-site', slug: 'web-site' };

const AGENT_REL = '11111111-1111-4111-8111-111111111111';
const AGENT_NCX = '22222222-2222-4222-8222-222222222222';
const AGENT_DES = '33333333-3333-4333-8333-333333333333';

interface RecordedAnswerCall {
  rowId: string;
  body: Record<string, unknown>;
}

interface MockProfileController {
  decisionsRequests: string[];
  answerCalls: RecordedAnswerCall[];
}

interface MockProfileOptions {
  hubNotFound?: boolean;
  truncatedProjects?: boolean;
}

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 640;

async function shot(page: Page, testInfo: TestInfo, theme: Theme, name: string): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  const projectTag =
    testInfo.project.name === 'desktop-1440' ? 'desktop-1440' : testInfo.project.name;
  const filename = `${name}-${projectTag}-${theme}.png`;
  const outFile = testInfo.outputPath(filename);
  await page.screenshot({ path: outFile, fullPage: false });
  await testInfo.attach(filename, { path: outFile, contentType: 'image/png' });

  const evidenceDir = process.env.DUX_EVIDENCE_DIR;
  if (
    evidenceDir &&
    (testInfo.project.name === 'desktop-1440' || testInfo.project.name === 'chromium-390')
  ) {
    fs.mkdirSync(evidenceDir, { recursive: true });
    fs.copyFileSync(outFile, path.join(evidenceDir, filename));
  }
}

async function assertZeroHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(() => {
    const doc = document.documentElement;
    return doc.scrollWidth - window.innerWidth;
  });
  expect(overflow).toBeLessThanOrEqual(1);
}

async function setupProfileDecisionsMocks(
  page: Page,
  theme: Theme,
  options: MockProfileOptions = {}
): Promise<MockProfileController> {
  const decisionsRequests: string[] = [];
  const answerCalls: RecordedAnswerCall[] = [];
  let shipAnswered = false;

  const nowMs = Date.now();
  const isoAgo = (minutesAgo: number): string =>
    new Date(nowMs - minutesAgo * 60_000).toISOString();

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
      json: { id: SELF_ID, name: 'Alice', email: 'alice@example.test', role: 'member' },
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
  await page.route('**/api/v1/projects**', (route) =>
    route.fulfill({
      json: {
        projects: [PROJ_SCION, PROJ_WEB],
        _capabilities: { actions: ['read'] },
      },
    })
  );

  const shipPrompt = {
    project: PROJ_SCION,
    promptMessageId: 'm-ship',
    promptAvailable: true,
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: AGENT_REL,
    askerAgentName: 'release-bot',
    createdAt: isoAgo(12),
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [
      {
        rowId: 'r-ship',
        messageId: 'm-ship',
        key: 'ship-now',
        ref: 'ship-now',
        version: 1,
        status: 'pending',
        askerAgentId: AGENT_REL,
        askerAgentName: 'release-bot',
        question: 'Deploy to prod after tagging `v0.9`?',
        defaultChoiceId: 'yes',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'wait', label: 'Wait for QA' },
        ],
      },
    ],
  };

  const dmSetPrompt = {
    project: PROJ_SCION,
    promptMessageId: 'm-dm-set',
    promptAvailable: true,
    chatKey: `dm:agent:${AGENT_NCX}:user:${SELF_ID}`,
    conversationLabel: 'DM with nc-x',
    conversationAvailable: true,
    askerAgentId: AGENT_NCX,
    askerAgentName: 'nc-x',
    createdAt: isoAgo(120),
    answerMode: 'set',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [
      {
        rowId: 'r-mig',
        messageId: 'm-dm-set',
        key: 'migrate',
        ref: 'migrate',
        version: 1,
        status: 'pending',
        askerAgentId: AGENT_NCX,
        askerAgentName: 'nc-x',
        question: 'Rotate the staging DB password?',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
      },
      {
        rowId: 'r-d12',
        messageId: 'm-dm-set',
        key: 'D12',
        ref: 'D12',
        version: 1,
        status: 'pending',
        askerAgentId: AGENT_NCX,
        askerAgentName: 'nc-x',
        question: 'Keep the old replica for a week?',
        choices: [
          { id: 'keep', label: 'Keep' },
          { id: 'drop', label: 'Drop now' },
        ],
      },
    ],
  };

  const heroPrompt = {
    project: PROJ_WEB,
    promptMessageId: 'm-hero',
    promptAvailable: true,
    chatKey: 'topic-landing',
    conversationLabel: '#landing',
    conversationAvailable: true,
    askerAgentId: AGENT_DES,
    askerAgentName: 'design-bot',
    createdAt: isoAgo(24 * 60),
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [
      {
        rowId: 'r-hero',
        messageId: 'm-hero',
        key: 'hero-img',
        ref: 'hero-img',
        version: 1,
        status: 'pending',
        askerAgentId: AGENT_DES,
        askerAgentName: 'design-bot',
        question: 'Use the dark hero image?',
        choices: [
          { id: 'dark', label: 'Dark' },
          { id: 'light', label: 'Light' },
        ],
      },
    ],
  };

  const othersPrompts = Array.from({ length: 5 }, (_, idx) => ({
    project: idx < 3 ? PROJ_SCION : PROJ_WEB,
    promptMessageId: `m-other-${idx + 1}`,
    promptAvailable: true,
    chatKey: idx < 3 ? 'topic-release' : 'topic-landing',
    conversationLabel: idx < 3 ? '#release' : '#landing',
    conversationAvailable: true,
    askerAgentId: idx < 3 ? AGENT_REL : AGENT_DES,
    askerAgentName: idx < 3 ? 'release-bot' : 'design-bot',
    createdAt: isoAgo(180 + idx * 10),
    answerMode: 'single',
    waitingOnYou: false,
    canAnswer: false,
    waitingReasons: { [`r-other-${idx + 1}`]: idx === 0 ? 'other_user_confirm' : 'read_only' },
    decisions: [
      {
        rowId: `r-other-${idx + 1}`,
        messageId: `m-other-${idx + 1}`,
        key: `O${idx + 1}`,
        ref: `O${idx + 1}`,
        version: 1,
        status: idx === 0 ? 'decided' : 'pending',
        askerAgentId: idx < 3 ? AGENT_REL : AGENT_DES,
        askerAgentName: idx < 3 ? 'release-bot' : 'design-bot',
        question: `Follow-up check #${idx + 1}?`,
        ...(idx === 0
          ? {
              decision: {
                choiceId: 'yes',
                label: 'Yes',
                answeredByUserId: 'u-bob',
                answeredByName: 'Bob',
                provenance: 'agent_interpreted',
                outcome: 'answer',
                surface: 'native',
                responseMessageId: 'resp-bob',
                at: isoAgo(170),
              },
            }
          : {}),
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
      },
    ],
  }));

  const madeScionEntries = [
    {
      project: PROJ_SCION,
      answerId: 'ans-d9',
      decision: {
        rowId: 'r-d9',
        promptMessageId: 'm-d9',
        promptAvailable: true,
        key: 'D9',
        ref: 'D9',
        version: 1,
        question: 'Cut the RC from main?',
        askerAgentId: AGENT_REL,
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
      at: isoAgo(45),
      chatKey: 'topic-release',
      conversationLabel: '#release',
      conversationAvailable: true,
      sourceMessageAvailable: false,
    },
    {
      project: PROJ_SCION,
      answerId: 'ans-17',
      decision: {
        rowId: 'r-17',
        promptMessageId: 'm-17',
        promptAvailable: true,
        key: '17',
        ref: '17',
        version: 1,
        question: 'Which approach?',
        askerAgentId: AGENT_NCX,
        askerAgentName: 'nc-x',
      },
      choiceId: 'a',
      label: 'a · Extend input-needed',
      matchedDefault: false,
      answeredByUserId: 'u-bob',
      answeredByName: 'Bob',
      provenance: 'direct',
      outcome: 'answer',
      surface: 'native',
      at: isoAgo(75),
      chatKey: 'topic-design',
      conversationLabel: '#design',
      conversationAvailable: true,
      sourceMessageAvailable: false,
    },
  ];

  const madeWebEntries = [
    {
      project: PROJ_WEB,
      answerId: 'ans-font',
      decision: {
        rowId: 'r-font',
        promptMessageId: 'm-font',
        promptAvailable: true,
        key: 'font',
        ref: 'font',
        version: 1,
        question: 'Which body font?',
        askerAgentId: AGENT_DES,
        askerAgentName: 'design-bot',
      },
      choiceId: 'inter',
      label: 'Inter',
      matchedDefault: true,
      answeredByUserId: SELF_ID,
      answeredByName: 'Alice',
      provenance: 'direct',
      outcome: 'confirm',
      surface: 'native',
      at: isoAgo(5 * 24 * 60),
      chatKey: 'topic-landing',
      conversationLabel: '#landing',
      conversationAvailable: true,
      sourceMessageAvailable: false,
    },
  ];

  const facets = {
    agents: [
      { id: AGENT_DES, name: 'design-bot', projectId: PROJ_WEB.id },
      { id: AGENT_NCX, name: 'nc-x', projectId: PROJ_SCION.id },
      { id: AGENT_REL, name: 'release-bot', projectId: PROJ_SCION.id },
    ],
    answerers: [
      { id: SELF_ID, name: 'Alice' },
      { id: 'u-bob', name: 'Bob' },
    ],
    conversations: [
      { key: 'topic-design', label: '#design', projectId: PROJ_SCION.id },
      { key: 'topic-landing', label: '#landing', projectId: PROJ_WEB.id },
      { key: 'topic-release', label: '#release', projectId: PROJ_SCION.id },
      {
        key: `dm:agent:${AGENT_NCX}:user:${SELF_ID}`,
        label: 'DM with nc-x',
        projectId: PROJ_SCION.id,
      },
    ],
    projects: [PROJ_SCION, PROJ_WEB],
    truncatedProjects: Boolean(options.truncatedProjects),
  };

  await page.route(/\/api\/v1\/users\/me\/decisions(\?|$)/, async (route: Route) => {
    const url = new URL(route.request().url());
    decisionsRequests.push(url.pathname + url.search);

    if (options.hubNotFound) {
      await route.fulfill({
        status: 404,
        json: { error: { code: 'not_found', message: 'not found' } },
      });
      return;
    }

    const limitParam = url.searchParams.get('limit');
    const tab = url.searchParams.get('tab') || 'pending';
    const sort = url.searchParams.get('sort') || (tab === 'made' ? 'recent' : 'project');
    const rawQ = url.searchParams.get('q') || '';
    const projectTokenMatch = rawQ.match(/(?:^|\s)project:([^\s]+)/i);
    const projectFromQ = projectTokenMatch
      ? projectTokenMatch[1].toLowerCase() === 'scion'
        ? PROJ_SCION.id
        : projectTokenMatch[1].toLowerCase() === 'web-site'
          ? PROJ_WEB.id
          : projectTokenMatch[1]
      : '';
    const projectFilter = url.searchParams.get('project') || projectFromQ;
    const q = rawQ
      .replace(/(?:^|\s)project:[^\s]+/gi, '')
      .trim()
      .toLowerCase();
    const convFilter = url.searchParams.get('conversation') || '';
    const agentFilter = url.searchParams.get('agent') || '';
    const cursor = url.searchParams.get('cursor') || '';

    const activeWaitingPrompts = [...(shipAnswered ? [] : [shipPrompt]), dmSetPrompt, heroPrompt];
    const totalWaitingOnYou = activeWaitingPrompts.reduce((sum, p) => sum + p.decisions.length, 0);
    const counts = { waitingOnYou: totalWaitingOnYou, waitingOnOthers: othersPrompts.length };

    if (limitParam === '0') {
      await route.fulfill({
        json: {
          counts,
          prompts: [],
          ...facets,
        },
      });
      return;
    }

    if (tab === 'pending') {
      let allPrompts = [...activeWaitingPrompts, ...othersPrompts];
      const isFiltered = Boolean(projectFilter || q || convFilter || agentFilter);
      if (projectFilter) {
        allPrompts = allPrompts.filter((p) => p.project.id === projectFilter);
      }
      if (convFilter) {
        allPrompts = allPrompts.filter((p) => p.chatKey === convFilter);
      }
      if (agentFilter) {
        allPrompts = allPrompts.filter((p) => p.askerAgentId === agentFilter);
      }
      if (q) {
        allPrompts = allPrompts.filter((p) =>
          p.decisions.some(
            (d) => d.question.toLowerCase().includes(q) || d.key.toLowerCase().includes(q)
          )
        );
      }

      const filteredWaiting = allPrompts
        .filter((p) => p.waitingOnYou)
        .reduce((sum, p) => sum + p.decisions.length, 0);
      const filteredOthers = allPrompts.filter((p) => !p.waitingOnYou).length;

      await route.fulfill({
        json: {
          counts,
          ...(isFiltered
            ? {
                filteredCounts: {
                  waitingOnYou: filteredWaiting,
                  waitingOnOthers: filteredOthers,
                },
              }
            : {}),
          prompts: allPrompts,
          ...facets,
        },
      });
      return;
    }

    if (sort === 'project') {
      const groups = [
        { project: PROJ_SCION, entries: madeScionEntries, more: true },
        { project: PROJ_WEB, entries: madeWebEntries, more: false },
      ].filter((g) => !projectFilter || g.project.id === projectFilter);

      await route.fulfill({
        json: {
          counts,
          groups,
          ...facets,
        },
      });
      return;
    }

    let entries = [...madeScionEntries, ...madeWebEntries];
    if (projectFilter) {
      entries = entries.filter((e) => e.project.id === projectFilter);
    }
    if (cursor === 'made-page-2') {
      entries = [
        {
          ...madeScionEntries[0],
          answerId: 'ans-older',
          decision: {
            ...madeScionEntries[0].decision,
            rowId: 'r-older',
            key: 'D1',
            ref: 'D1',
            question: 'Enable canary metrics export?',
          },
          label: 'Yes',
          outcome: 'answer',
        },
      ];
    }

    await route.fulfill({
      json: {
        counts,
        entries,
        ...facets,
        ...(cursor ? {} : { nextCursor: 'made-page-2' }),
      },
    });
  });

  await page.route(/\/api\/v1\/decisions\/([^/]+)\/answer$/, async (route: Route) => {
    const match = /\/api\/v1\/decisions\/([^/]+)\/answer$/.exec(route.request().url());
    const rowId = match?.[1] ?? 'r-ship';
    const raw = route.request().postData() || '{}';
    const body = JSON.parse(raw) as Record<string, unknown>;
    answerCalls.push({ rowId, body });
    if (rowId === 'r-ship') {
      shipAnswered = true;
    }
    await route.fulfill({
      json: {
        decision: {
          ...shipPrompt.decisions[0],
          rowId,
          status: 'decided',
          decision: {
            choiceId: typeof body.choice_id === 'string' ? body.choice_id : 'yes',
            label: 'Yes',
            answeredByUserId: SELF_ID,
            answeredByName: 'Alice',
            provenance: 'direct',
            outcome: 'answer',
            surface: typeof body.surface === 'string' ? body.surface : 'decisions_page',
            responseMessageId: 'resp-ship',
            at: new Date().toISOString(),
          },
        },
      },
    });
  });

  return { decisionsRequests, answerCalls };
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`Profile Decisions page (${theme}) @static`, () => {
    test('R-1 & R-2: Profile nav badge, Waiting on you By project vs Most recent, D5 project facet hints, and inline answer updating nav badge', async ({
      page,
    }, testInfo) => {
      const ctrl = await setupProfileDecisionsMocks(page, theme);
      const phone = isPhone(page);

      await page.goto('/profile/decisions', { waitUntil: 'domcontentloaded' });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      await expect(page).toHaveURL(/\/profile\/decisions\?tab=pending&sort=project$/);

      const pageEl = page.locator('scion-page-profile-decisions');
      await expect(pageEl).toBeVisible({ timeout: 15_000 });
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('4');
      await expect(pageEl.getByRole('radiogroup', { name: 'Sort decisions' })).toBeVisible();

      // Profile left nav shows Decisions at the top with badge "4" on desktop (and inside drawer on mobile).
      const profileNav = page.locator('scion-profile-nav').first();
      await expect(profileNav.locator('[data-decisions-nav-link]')).toHaveAttribute(
        'href',
        '/profile/decisions'
      );
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('4');

      // R-1: Grouped By project -> scion (3) and web-site (1) with Open project Decisions links
      const scionGroup = pageEl.locator('[data-pending-group="proj-1"]');
      const webGroup = pageEl.locator('[data-pending-group="proj-2"]');
      await expect(scionGroup.locator('h3')).toHaveText('scion');
      await expect(scionGroup.locator('.groupsec sl-badge')).toHaveText('3');
      await expect(scionGroup.locator('[data-open-project-decisions]')).toHaveAttribute(
        'href',
        '/projects/proj-1/decisions'
      );
      await expect(webGroup.locator('h3')).toHaveText('web-site');
      await expect(webGroup.locator('.groupsec sl-badge')).toHaveText('1');
      await expect(pageEl.locator('[data-card-project]')).toHaveCount(0);
      await expect(pageEl.locator('sl-details[data-waiting-others]')).toHaveAttribute(
        'summary',
        'Waiting on others (5)'
      );
      await assertZeroHorizontalOverflow(page);

      if (!phone) {
        await shot(page, testInfo, theme, 'r1-waiting-by-project');
      }

      // Switch sort to Most recent (R-2): project badge joins each card header
      await pageEl.locator('[data-sort-option="recent"]').click();
      await expect(page).toHaveURL(/\/profile\/decisions\?tab=pending&sort=recent$/);
      await expect(pageEl.locator('[data-pending-group]')).toHaveCount(0);
      await expect(pageEl.locator('[data-card-project]')).toHaveText([
        'scion',
        'scion',
        'web-site',
      ]);
      await assertZeroHorizontalOverflow(page);

      if (!phone) {
        await shot(page, testInfo, theme, 'r2-waiting-most-recent');
      }

      // Answer ship-now inline -> records surface=decisions_page and decrements both Waiting badge and Profile nav badge (4 -> 3)
      const shipCard = pageEl.locator('[data-prompt-card="m-ship"]');
      const yesBtn = shipCard.locator('sl-button[data-choice="yes"]');
      await yesBtn.scrollIntoViewIfNeeded();
      await yesBtn.click();

      await expect.poll(() => ctrl.answerCalls.length).toBe(1);
      expect(ctrl.answerCalls[0]).toMatchObject({
        rowId: 'r-ship',
        body: { choice_id: 'yes', surface: 'decisions_page' },
      });
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('3');
      await expect(profileNav.locator('[data-decisions-nav-badge]')).toHaveText('3');
    });

    test('R-3 & R-4: Made By project overview + See all drill-down, Most recent Project column, and mobile filter tags/comboboxes', async ({
      page,
    }, testInfo) => {
      await setupProfileDecisionsMocks(page, theme);
      const phone = isPhone(page);

      if (phone) {
        // R-4: Mobile (320 / 375 / 390) filtered by project:scion with filter box and zero horizontal overflow
        await page.goto('/profile/decisions?tab=pending&sort=project&project=proj-1', {
          waitUntil: 'domcontentloaded',
        });
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
        const pageEl = page.locator('scion-page-profile-decisions');
        const panel = pageEl.locator('scion-decisions-filter-panel');
        const filterInput = panel.locator('scion-filter-box input');
        await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('3 of 4', {
          timeout: 15_000,
        });
        await expect(filterInput).toHaveValue('project:scion');
        await assertZeroHorizontalOverflow(page);
        await shot(page, testInfo, theme, 'r4-mobile-waiting-filtered');

        // Conversation autocomplete suggestions carry project hints on mobile
        await filterInput.click();
        await filterInput.fill('conversation:');
        await expect(panel.locator('scion-filter-box [role="option"]').first()).toContainText(
          'scion'
        );
        await assertZeroHorizontalOverflow(page);
        await shot(page, testInfo, theme, 'r4b-mobile-filters-expanded');
        return;
      }

      // Desktop R-3a: Made · By project overview (5 newest per project + "See all in <project>")
      await page.goto('/profile/decisions?tab=made&sort=project', {
        waitUntil: 'domcontentloaded',
      });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      const pageEl = page.locator('scion-page-profile-decisions');
      await expect(pageEl.locator('[data-made-group="proj-1"]')).toBeVisible({ timeout: 15_000 });
      await expect(pageEl.getByRole('radiogroup', { name: 'Sort decisions' })).toBeVisible();
      await expect(pageEl.locator('[data-made-group="proj-2"]')).toBeVisible();
      await expect(pageEl.locator('[data-see-all-project="proj-1"]')).toHaveText(
        'See all in scion'
      );
      await expect(pageEl.locator('[data-load-more="made"]')).toHaveCount(0);
      await shot(page, testInfo, theme, 'r3a-made-by-project');

      // Desktop R-3b: Click "See all in scion" -> switches to Most recent filtered to project:scion; then Clear -> full Most recent table with Project column
      await pageEl.locator('[data-see-all-project="proj-1"]').click();
      await expect(page).toHaveURL(/\/profile\/decisions\?tab=made&sort=recent&q=project%3Ascion$/);
      const panel = pageEl.locator('scion-decisions-filter-panel');
      await expect(panel.locator('scion-filter-box input')).toHaveValue('project:scion');

      await panel.locator('scion-filter-box button[aria-label="Clear filter"]').click();
      await expect(page).toHaveURL(/\/profile\/decisions\?tab=made&sort=recent$/);
      const table = pageEl.locator('table.log[data-made-table]');
      await expect(table).toBeVisible();
      await expect(table.locator('thead th').nth(1)).toHaveText('Project');
      await expect(table.locator('tbody tr')).toHaveCount(3);
      await expect(pageEl.locator('[data-load-more="made"]')).toBeVisible();
      await shot(page, testInfo, theme, 'r3b-made-most-recent');
    });

    test('R3 truncatedProjects banner and 404-hidden profile nav fallback', async ({ page }) => {
      await setupProfileDecisionsMocks(page, theme, { truncatedProjects: true });
      await page.goto('/profile/decisions', { waitUntil: 'domcontentloaded' });

      const pageEl = page.locator('scion-page-profile-decisions');
      await expect(pageEl.locator('[data-truncated-projects]')).toContainText(
        'Showing your 200 most recently active projects; only those 200 projects are searched.'
      );

      // Older hub returning 404 on /api/v1/users/me/decisions hides the Profile > Decisions nav entry
      await setupProfileDecisionsMocks(page, theme, { hubNotFound: true });
      await page.goto('/profile/env', { waitUntil: 'domcontentloaded' });
      const profileNav = page.locator('scion-profile-nav').first();
      await expect(profileNav.locator('a[href="/profile/env"]')).toHaveCount(1);
      await expect(profileNav.locator('[data-decisions-nav-link]')).toHaveCount(0);
    });
  });
}
