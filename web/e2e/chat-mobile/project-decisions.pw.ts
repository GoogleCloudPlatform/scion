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
const REL_AGENT_UUID = '11111111-1111-4111-8111-111111111111';
const REL_DELETED_AGENT_UUID = '3f9c0000-2222-4222-8222-222222222222';
const NCX_AGENT_UUID = '44444444-4444-4444-8444-444444444444';

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
  duxMocks?: boolean;
}

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 640;

async function shot(page: Page, testInfo: TestInfo, theme: Theme, name: string): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
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
    const q = (url.searchParams.get('q') ?? '').trim().toLowerCase();

    if (q === 'timeout-slow') {
      void route.fulfill({
        status: 503,
        json: {
          error: {
            code: 'SEARCH_TIMEOUT',
            message: 'search query timed out',
          },
        },
      });
      return;
    }

    if (options.duxMocks) {
      const duxFacets = {
        agents: [
          { id: REL_AGENT_UUID, name: 'release-bot' },
          { id: REL_DELETED_AGENT_UUID, name: 'release-bot', hint: 'deleted · 3f9c' },
          { id: NCX_AGENT_UUID, name: 'nc-x' },
        ],
        answerers: [
          { id: SELF_ID, name: 'Alice' },
          { id: 'u-bob', name: 'Bob' },
        ],
        conversations: [
          { key: 'topic-release', label: '#release', projectId: PROJECT_ID },
          {
            key: `dm:agent:${REL_AGENT_UUID}:user:${SELF_ID}`,
            label: 'DM with release-bot',
            projectId: PROJECT_ID,
          },
          { key: 'topic-ops', label: '#ops', projectId: PROJECT_ID },
        ],
      };

      const duxPrompts = [
        {
          promptMessageId: 'm-ship-now',
          promptAvailable: true,
          chatKey: 'topic-release',
          conversationLabel: '#release',
          conversationAvailable: true,
          askerAgentId: REL_AGENT_UUID,
          askerAgentName: 'release-bot',
          createdAt: '2026-11-13T15:48:00Z',
          answerMode: 'single',
          waitingOnYou: true,
          canAnswer: true,
          decisions: [
            {
              rowId: 'r-ship-now',
              messageId: 'm-ship-now',
              key: 'ship-now',
              ref: 'ship-now',
              version: 1,
              status: 'pending',
              askerAgentId: REL_AGENT_UUID,
              askerAgentName: 'release-bot',
              question: 'Deploy to prod after tagging `v0.9`?',
              defaultChoiceId: 'yes',
              choices: [
                { id: 'yes', label: 'Yes' },
                { id: 'wait-qa', label: 'Wait for QA' },
              ],
            },
          ],
        },
        {
          promptMessageId: 'm-migrate-1',
          promptAvailable: true,
          chatKey: 'topic-ops',
          conversationLabel: '#ops',
          conversationAvailable: true,
          askerAgentId: NCX_AGENT_UUID,
          askerAgentName: 'nc-x',
          createdAt: '2026-11-13T14:00:00Z',
          answerMode: 'single',
          waitingOnYou: true,
          canAnswer: true,
          decisions: [
            {
              rowId: 'r-migrate-1',
              messageId: 'm-migrate-1',
              key: 'migrate',
              ref: 'migrate',
              version: 3,
              status: 'pending',
              askerAgentId: NCX_AGENT_UUID,
              askerAgentName: 'nc-x',
              question: 'Rotate the staging DB password?',
              choices: [
                { id: 'yes', label: 'Yes' },
                { id: 'no', label: 'No' },
              ],
            },
          ],
        },
        {
          promptMessageId: 'm-migrate-2',
          promptAvailable: true,
          chatKey: 'topic-ops',
          conversationLabel: '#ops',
          conversationAvailable: true,
          askerAgentId: NCX_AGENT_UUID,
          askerAgentName: 'nc-x',
          createdAt: '2026-11-13T13:30:00Z',
          answerMode: 'single',
          waitingOnYou: true,
          canAnswer: true,
          decisions: [
            {
              rowId: 'r-migrate-2',
              messageId: 'm-migrate-2',
              key: 'D12',
              ref: 'D12',
              version: 1,
              status: 'pending',
              askerAgentId: NCX_AGENT_UUID,
              askerAgentName: 'nc-x',
              question: 'Keep the old replica for a week?',
              choices: [
                { id: 'keep', label: 'Keep' },
                { id: 'drop', label: 'Drop now' },
              ],
            },
          ],
        },
        ...othersPrompts.map((p) => ({
          ...p,
          askerAgentId: NCX_AGENT_UUID,
          askerAgentName: 'nc-x',
          decisions: p.decisions.map((d) => ({
            ...d,
            askerAgentId: NCX_AGENT_UUID,
            askerAgentName: 'nc-x',
          })),
        })),
      ];

      const duxMadeEntries = [
        {
          answerId: 'ans-backfill-v2',
          decision: {
            rowId: 'r-mig-v2',
            promptMessageId: 'm-mig-v2',
            promptAvailable: true,
            key: 'migrate',
            ref: 'migrate',
            version: 2,
            question: 'Run the backfill on staging first?',
            askerAgentId: NCX_AGENT_UUID,
            askerAgentName: 'nc-x',
          },
          choiceId: 'yes',
          label: 'Yes',
          matchedDefault: true,
          answeredByUserId: SELF_ID,
          answeredByName: 'Alice',
          provenance: 'direct',
          outcome: 'answer',
          surface: 'decisions_page',
          at: '2026-11-12T10:15:00Z',
          chatKey: 'topic-release',
          conversationLabel: '#release',
          conversationAvailable: true,
          sourceMessageAvailable: false,
        },
        {
          answerId: 'ans-backfill-v1',
          decision: {
            rowId: 'r-mig-v1',
            promptMessageId: 'm-mig-v1',
            promptAvailable: true,
            key: 'migrate',
            ref: 'migrate',
            version: 1,
            question: 'Backfill before the cutover?',
            askerAgentId: NCX_AGENT_UUID,
            askerAgentName: 'nc-x',
          },
          choiceId: 'yes',
          label: 'Yes',
          matchedDefault: true,
          answeredByName: 'Alice',
          provenance: 'direct',
          outcome: 'confirm',
          surface: 'web',
          at: '2026-11-02T09:00:00Z',
          chatKey: 'topic-release',
          conversationLabel: '#release',
          conversationAvailable: true,
          sourceMessageAvailable: false,
        },
        {
          answerId: 'ans-other-ops',
          decision: {
            rowId: 'r-ops-1',
            promptMessageId: 'm-ops-1',
            promptAvailable: true,
            key: 'audit',
            ref: 'audit',
            version: 1,
            question: 'Enable staging audit logs?',
            askerAgentId: REL_AGENT_UUID,
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
          at: '2026-11-01T08:00:00Z',
          chatKey: 'topic-ops',
          conversationLabel: '#ops',
          conversationAvailable: true,
          sourceMessageAvailable: false,
        },
      ];

      const convParam = url.searchParams.get('conversation') ?? '';
      const agentParam = url.searchParams.get('agent') ?? '';
      const userParam = url.searchParams.get('user') ?? '';
      const provParam = url.searchParams.get('provenance') ?? '';
      const unfilteredCounts = { waitingOnYou: 3, waitingOnOthers: 2 };

      if (tab === 'made') {
        const filteredEntries = duxMadeEntries.filter((e) => {
          if (convParam && e.chatKey !== convParam) return false;
          if (agentParam && e.decision.askerAgentId !== agentParam) return false;
          if (userParam && e.answeredByUserId !== userParam) return false;
          if (provParam === 'confirmed' && e.outcome !== 'confirm') return false;
          if (provParam === 'direct' && !(e.provenance === 'direct' && e.outcome === 'answer')) {
            return false;
          }
          if (q) {
            const hay =
              `${e.decision.key} ${e.decision.question} ${e.label} ${e.decision.askerAgentName} ${e.conversationLabel}`.toLowerCase();
            if (!hay.includes(q)) return false;
          }
          return true;
        });
        void route.fulfill({
          json: {
            counts: unfilteredCounts,
            entries: filteredEntries,
            ...duxFacets,
          },
        });
        return;
      }

      if (limit === '0') {
        void route.fulfill({
          json: {
            counts: unfilteredCounts,
            prompts: [],
            agents: [],
            answerers: [],
            conversations: [],
          },
        });
        return;
      }

      const isPendingSharedFiltered = Boolean(q || convParam || agentParam);
      const filteredPrompts = duxPrompts.filter((p) => {
        if (convParam && p.chatKey !== convParam) return false;
        if (agentParam && p.askerAgentId !== agentParam) return false;
        if (q) {
          const hay =
            `${p.askerAgentName} ${p.conversationLabel} ${p.decisions.map((d) => `${d.key} ${d.question}`).join(' ')}`.toLowerCase();
          if (!hay.includes(q)) return false;
        }
        return true;
      });
      const filteredWaitingOnYou = filteredPrompts.filter((p) => p.waitingOnYou).length;
      const filteredWaitingOnOthers = filteredPrompts.filter((p) => !p.waitingOnYou).length;

      void route.fulfill({
        json: {
          counts: unfilteredCounts,
          ...(isPendingSharedFiltered
            ? {
                filteredCounts: {
                  waitingOnYou: filteredWaitingOnYou,
                  waitingOnOthers: filteredWaitingOnOthers,
                },
              }
            : {}),
          prompts: filteredPrompts,
          ...duxFacets,
        },
      });
      return;
    }

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
      conversations: [
        { key: 'topic-release', label: '#release', projectId: PROJECT_ID },
        {
          key: `dm:agent:a-rel:user:${SELF_ID}`,
          label: 'DM with release-bot',
          projectId: PROJECT_ID,
        },
        { key: 'topic-ops', label: '#ops', projectId: PROJECT_ID },
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
            conversations: [],
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
          conversations: [],
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
          conversations: [],
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
      const filterPanel = pageEl.locator('scion-decisions-filter-panel');
      const filterToggle = filterPanel.getByRole('button', {
        name: /Search and filters/i,
        expanded: false,
      });
      const filterBody = filterPanel.locator('#decisions-filter-body');

      if (isPhone(page)) {
        await expect(mobileList).toBeVisible({ timeout: 15_000 });
        await expect(desktopTable).toBeHidden();
        await expect(filterToggle).toBeVisible();
        await expect(filterBody).toBeHidden();

        await expect(mobileList.locator('[data-mobile-answer]')).toHaveCount(3);
        await expect(mobileList).toContainText('Decisions page');
        await expect(mobileList).toContainText('(deleted user)');
        await shot(page, testInfo, theme, '03-made-log');
      } else {
        await expect(desktopTable).toBeVisible({ timeout: 15_000 });
        await expect(mobileList).toBeHidden();
        await expect(filterToggle).toBeVisible();
        await expect(filterBody).toBeHidden();

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

      // Expand the collapsible filter panel and filter by Provenance = Confirmed.
      await filterToggle.click();
      await expect(
        filterPanel.getByRole('button', { name: /Search and filters/i, expanded: true })
      ).toBeVisible();
      await expect(filterBody).toBeVisible();

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

    test('F-1..F-5: Decisions filter panel (collapsed/expanded), fuzzy comboboxes, URL state, n-of-m badge, no-match, and search timeout', async ({
      page,
    }, testInfo) => {
      await setupProjectDecisionsMocks(page, theme, { duxMocks: true });
      const phone = isPhone(page);

      if (phone) {
        // F-3: Mobile (390px) collapsed with wrapping removable tags ("backfill", Conversation: #release, Answered by: you)
        await page.goto(
          `/projects/${PROJECT_ID}/decisions?tab=made&q=backfill&conversation=topic-release&user=${SELF_ID}`,
          { waitUntil: 'domcontentloaded' }
        );
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

        const pageEl = page.locator('scion-page-project-decisions');
        const panel = pageEl.locator('scion-decisions-filter-panel');
        await expect(
          panel.getByRole('button', { name: /Search and filters/i, expanded: false })
        ).toBeVisible({ timeout: 15_000 });
        await expect(panel.locator('[data-filter-tag="q"]')).toContainText('"backfill"');
        await expect(panel.locator('[data-filter-tag="conversation"]')).toContainText(
          'Conversation: #release'
        );
        await expect(panel.locator('[data-filter-tag="user"]')).toContainText('Answered by: you');
        await expect(panel.locator('[data-active-filter-count]')).toHaveText('3 active');
        await expect(pageEl.locator('[data-made-mobile-list] [data-mobile-answer]')).toHaveCount(1);
        await shot(page, testInfo, theme, 'f3-mobile-collapsed');

        // F-4: Mobile expanded (stacked controls, no drawer) + Conversation combobox fuzzy matching "rel"
        await panel.getByRole('button', { name: /Search and filters/i, expanded: false }).click();
        await expect(
          panel.getByRole('button', { name: /Search and filters/i, expanded: true })
        ).toBeVisible();
        await expect(page.locator('sl-drawer[open]')).toHaveCount(0);

        const convCombo = panel.locator('scion-facet-combobox[data-filter="conversation"]');
        await convCombo.locator('[data-combobox-clear]').click();
        await expect(panel.locator('[data-active-filter-count]')).toHaveText('2 active');

        const convInput = convCombo.getByRole('combobox', { name: 'Conversation' });
        await convInput.click();
        await convInput.fill('rel');
        const convOptions = convCombo.getByRole('option');
        await expect(convOptions).toHaveCount(2);
        await expect(convOptions.nth(0)).toContainText('#release');
        await expect(convOptions.nth(1)).toContainText('DM with release-bot');
        await expect(convOptions.nth(0).locator('mark')).toHaveText('rel');
        await shot(page, testInfo, theme, 'f4-mobile-expanded-combobox');
      } else {
        // F-1: Desktop collapsed by default with removable tags ("backfill", Conversation: #release), 2 active, Clear
        await page.goto(
          `/projects/${PROJECT_ID}/decisions?tab=made&q=backfill&conversation=topic-release`,
          { waitUntil: 'domcontentloaded' }
        );
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

        const pageEl = page.locator('scion-page-project-decisions');
        const panel = pageEl.locator('scion-decisions-filter-panel');
        await expect(
          panel.getByRole('button', { name: /Search and filters/i, expanded: false })
        ).toBeVisible({ timeout: 15_000 });
        await expect(panel.locator('[data-filter-tag="q"]')).toContainText('"backfill"');
        await expect(panel.locator('[data-filter-tag="conversation"]')).toContainText(
          'Conversation: #release'
        );
        await expect(panel.locator('[data-active-filter-count]')).toHaveText('2 active');
        await expect(pageEl.locator('table.log[data-made-table] tbody tr')).toHaveCount(2);
        await expect(pageEl).toContainText('End of matching decisions');
        await shot(page, testInfo, theme, 'f1-made-collapsed');

        // F-2 / F-2-dark: Press "/" on page -> expands panel & focuses search; Agent combobox fuzzy matches "rlsb"
        await page.keyboard.press('/');
        await expect(
          panel.getByRole('button', { name: /Search and filters/i, expanded: true })
        ).toBeVisible();

        const agentCombo = panel.locator('scion-facet-combobox[data-filter="agent"]');
        const agentInput = agentCombo.getByRole('combobox', { name: 'Agent' });
        await agentInput.click();
        await agentInput.fill('rlsb');
        const agentOptions = agentCombo.getByRole('option');
        await expect(agentOptions).toHaveCount(2);
        await expect(agentOptions.nth(0)).toContainText('release-bot');
        await expect(agentOptions.nth(1)).toContainText('release-bot');
        await expect(agentOptions.nth(1)).toContainText('deleted · 3f9c');
        await expect(agentOptions.nth(0).locator('mark')).toHaveCount(4);
        await shot(page, testInfo, theme, 'f2-made-expanded-combobox');
      }

      // F-2b: Waiting on you filtered by agent -> "1 of 3" tab badge and "Waiting on others (0 of 2)"
      await page.evaluate(() => localStorage.removeItem('scion:decisions:filters-open'));
      await page.goto(`/projects/${PROJECT_ID}/decisions?agent=${REL_AGENT_UUID}`, {
        waitUntil: 'domcontentloaded',
      });
      const pageEl = page.locator('scion-page-project-decisions');
      const panel = pageEl.locator('scion-decisions-filter-panel');
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('1 of 3', {
        timeout: 15_000,
      });
      await expect(panel.locator('[data-filter-tag="agent"]')).toContainText('Agent: release-bot');
      await expect(panel.locator('[data-active-filter-count]')).toHaveText('1 active');
      await expect(pageEl.locator('sl-details[data-waiting-others]')).toHaveAttribute(
        'summary',
        'Waiting on others (0 of 2)'
      );
      await shot(page, testInfo, theme, 'f2b-waiting-filtered');

      // F-5a: Distinct no-match state with "Clear filters" action
      await panel.getByRole('button', { name: /Search and filters/i, expanded: false }).click();
      const searchInput = panel.locator('sl-input[data-filter="q"] input');
      await searchInput.fill('nomatch');
      const noMatch = pageEl.locator('[data-no-match="pending"]');
      await expect(noMatch).toBeVisible({ timeout: 10_000 });
      await expect(noMatch).toContainText('No decisions match these filters');
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('0 of 3');
      await shot(page, testInfo, theme, 'f5a-no-match');

      // F-5b: Search timeout warning alert ("Search took too long; narrow it...") on 503 SEARCH_TIMEOUT
      await searchInput.fill('timeout-slow');
      const timeoutAlert = pageEl.locator('[data-search-timeout]');
      await expect(timeoutAlert).toBeVisible({ timeout: 10_000 });
      await expect(timeoutAlert).toContainText('Search took too long; narrow it');
      await shot(page, testInfo, theme, 'f5b-search-timeout');

      // Clear filters recovers back to unfiltered Waiting on you ("3" badge) and cleans URL
      await panel.locator('[data-clear-filters]').click();
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('3');
      await expect(page).toHaveURL(new RegExp(`/projects/${PROJECT_ID}/decisions$`));

      // First-load SEARCH_TIMEOUT via direct ?q=timeout-slow link still mounts filter panel, tag, and Clear
      await page.goto(`/projects/${PROJECT_ID}/decisions?q=timeout-slow`, {
        waitUntil: 'domcontentloaded',
      });
      await expect(timeoutAlert).toBeVisible({ timeout: 10_000 });
      await expect(panel.locator('[data-filter-tag="q"]')).toContainText('"timeout-slow"');
      await panel.locator('[data-clear-filters]').click();
      await expect(pageEl.locator('[data-waiting-badge]')).toHaveText('3');
      await expect(page).toHaveURL(new RegExp(`/projects/${PROJECT_ID}/decisions$`));
    });
  });
}
