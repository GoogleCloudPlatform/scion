/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { Page } from '@playwright/test';

export const PROJ_PAYMENTS = '11111111-1111-1111-1111-111111111111';
export const PROJ_SCRATCH = '22222222-2222-2222-2222-222222222222';
export const PROJ_WEB = '33333333-3333-3333-3333-333333333333';

const PROJECTS = [
  { id: PROJ_PAYMENTS, name: 'payments-api', slug: 'payments-api' },
  { id: PROJ_SCRATCH, name: 'scratch-experiments', slug: 'scratch-experiments' },
  { id: PROJ_WEB, name: 'web-console', slug: 'web-console' },
];

export interface DecisionRetentionMockOptions {
  retentionFlag?: boolean;
  decisionsFlag?: boolean;
  theme?: 'light' | 'dark';
  projectRetention?: { active: boolean; days: number } | null;
  profileRetention?: Record<string, { active: boolean; days: number }> | null;
}

const SAMPLE_MADE_ENTRY = {
  answerId: 'ans-1',
  decision: {
    rowId: 'dec-1',
    promptMessageId: 'msg-1',
    promptAvailable: true,
    key: 'db-choice',
    ref: '#1',
    version: 1,
    status: 'decided',
    question: 'Which database should staging use?',
    choices: [
      { id: 'pg', label: 'Postgres' },
      { id: 'sqlite', label: 'SQLite' },
    ],
    defaultChoiceId: 'pg',
    askerAgentId: 'agent-1',
    askerAgentName: 'architect',
  },
  choiceId: 'pg',
  label: 'Postgres',
  matchedDefault: true,
  answeredByUserId: 'user-admin',
  answeredByName: 'Preston Holmes',
  provenance: 'direct',
  outcome: 'answered',
  surface: 'web',
  sourceMessageAvailable: true,
  at: '2026-11-25T12:00:00Z',
  chatKey: 'conv:c-1',
  conversationLabel: 'General',
  conversationAvailable: true,
  project: { id: PROJ_WEB, name: 'web-console', slug: 'web-console' },
};

export async function setupDecisionRetentionHub(
  page: Page,
  opts: DecisionRetentionMockOptions = {}
): Promise<void> {
  const retentionFlag = opts.retentionFlag ?? true;
  const decisionsFlag = opts.decisionsFlag ?? true;
  const theme = opts.theme ?? 'light';
  const projectRetention =
    opts.projectRetention === undefined ? { active: true, days: 180 } : opts.projectRetention;
  const profileRetention =
    opts.profileRetention === undefined
      ? { [PROJ_WEB]: { active: true, days: 180 }, [PROJ_SCRATCH]: { active: true, days: 30 } }
      : opts.profileRetention;

  let settingsDoc = {
    enabled: true,
    retention_days: 180,
    project_overrides: {
      [PROJ_PAYMENTS]: { mode: 'keep_forever' },
      [PROJ_SCRATCH]: { mode: 'days', days: 30 },
    },
    sweep: { page_size: 200, tick_budget_seconds: 30, tick_interval_minutes: 60 },
    revision: 4,
    runs: [
      {
        id: 'run-preview',
        trigger: 'preview',
        dry_run: true,
        started_at: '2026-11-25T14:05:00Z',
        finished_at: '2026-11-25T14:05:02Z',
        status: 'ok',
        projects: 2,
        totals: { decisions: 2310, answers: 2100 },
        retention_days: 180,
      },
      {
        id: 'run-sweep',
        trigger: 'sweep',
        dry_run: false,
        started_at: '2026-11-25T14:00:00Z',
        finished_at: '2026-11-25T14:00:12Z',
        status: 'ok',
        projects: 6,
        totals: { decisions: 1284, answers: 1190 },
        retention_days: 180,
      },
    ],
  };

  await page.addInitScript(
    ({ selectedTheme, flags }) => {
      window.localStorage.setItem('scion-theme', selectedTheme);
      window.__SCION_FEATURES__ = flags;
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        onerror: (() => void) | null = null;
        readyState = 1;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    {
      selectedTheme: theme,
      flags: {
        'hub.decision_retention': retentionFlag,
        'web.decisions': decisionsFlag,
      },
    }
  );

  await page.route('**/api/**', (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/projects')) return route.fulfill({ json: { projects: PROJECTS } });
    if (path.endsWith('/agents')) return route.fulfill({ json: { agents: [] } });
    return route.fulfill({ json: {} });
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: {
        id: 'user-admin',
        email: 'admin@example.test',
        displayName: 'Preston Holmes',
        role: 'admin',
      },
    })
  );
  await page.route('**/api/v1/settings/public', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/experiments', (route) =>
    route.fulfill({
      json: {
        experiments: {
          'hub.decision_retention': retentionFlag,
          'web.decisions': decisionsFlag,
        },
      },
    })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({
      json: {
        isAdmin: true,
        isSuperAdmin: true,
        permissions: ['hub.config.read', 'hub.config.update', 'hub.maintenance.execute'],
      },
    })
  );
  await page.route('**/api/v1/admin/server-config', (route) =>
    route.fulfill({
      json: {
        settings: {},
        envValues: {},
        sections: [],
      },
    })
  );
  await page.route('**/api/v1/admin/decision-retention', async (route) => {
    if (!retentionFlag) {
      return route.fulfill({
        status: 404,
        json: { error: { code: 'not_found', message: 'Route not found' } },
      });
    }
    if (route.request().method() === 'PUT') {
      const body = JSON.parse(route.request().postData() ?? '{}') as Record<string, unknown>;
      settingsDoc = {
        ...settingsDoc,
        enabled: Boolean(body.enabled),
        retention_days: (body.retention_days as number | null) ?? null,
        project_overrides:
          (body.project_overrides as typeof settingsDoc.project_overrides) ??
          settingsDoc.project_overrides,
        revision: settingsDoc.revision + 1,
      };
      return route.fulfill({ json: settingsDoc });
    }
    return route.fulfill({ json: settingsDoc });
  });

  await page.route(`**/api/v1/projects/${PROJ_WEB}`, (route) =>
    route.fulfill({
      json: {
        id: PROJ_WEB,
        name: 'web-console',
        slug: 'web-console',
        _capabilities: { actions: ['read'] },
      },
    })
  );

  await page.route(`**/api/v1/projects/${PROJ_WEB}/decisions**`, (route) => {
    const url = new URL(route.request().url());
    const isMade = url.searchParams.get('tab') === 'made';
    return route.fulfill({
      json: {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        ...(isMade ? { entries: [SAMPLE_MADE_ENTRY] } : { prompts: [] }),
        agents: [],
        answerers: [],
        conversations: [],
        ...(projectRetention ? { retention: projectRetention } : {}),
      },
    });
  });

  await page.route('**/api/v1/users/me/decisions**', (route) => {
    const url = new URL(route.request().url());
    const isMade = url.searchParams.get('tab') === 'made';
    return route.fulfill({
      json: {
        sort: url.searchParams.get('sort') === 'project' ? 'project' : 'recent',
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        ...(isMade ? { entries: [SAMPLE_MADE_ENTRY] } : { prompts: [] }),
        agents: [],
        answerers: [],
        conversations: [],
        projects: PROJECTS,
        truncatedProjects: false,
        ...(profileRetention ? { retention: profileRetention } : {}),
      },
    });
  });
}
