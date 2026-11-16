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
 * Coalesced Decisions bell notification (`decisions_waiting`, ptone/scion#7997),
 * covering approved mocks B-1..B-3 in light and dark themes on desktop
 * (1440x900) and mobile (390x844 bottom sheet).
 *
 * Screenshots are written only to Playwright's per-test artifact output
 * directory (`testInfo.outputPath(...)`); nothing is written into tracked
 * repository paths.
 */

import { test, expect, type Page, type TestInfo } from '@playwright/test';
import { emitEventSourceUpdate, installEventSourceStub } from '../event-source-stub.js';

type Theme = 'light' | 'dark';

const PROJECTS = new Set(['desktop-1440', 'chromium-390']);
const PROJECT_ID = 'p-1';
const SELF_ID = 'fixture-user';
const NIL_UUID = '00000000-0000-0000-0000-000000000000';
const DECISIONS_NOTIF_ID = 'd9999999-0000-5000-8000-000000000001';
const AGENT_NOTIF_ID = 'a1111111-0000-4000-8000-000000000001';
const AGENT_ID = '6a2b3c4d-0000-4000-8000-000000000001';

interface MockBellController {
  reconcileProbes: string[];
  ackedIds: string[];
  setServerDecisionsCount: (count: number, projects: number, acknowledged: boolean) => void;
}

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 640;

async function shot(page: Page, testInfo: TestInfo, theme: Theme, name: string): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  const file = testInfo.outputPath(`decisions-bell-${project}-${theme}-${name}.png`);
  await page.screenshot({ path: file, fullPage: false });
  await testInfo.attach(name, { path: file, contentType: 'image/png' });
}

function buildDecisionsNotif(count: number, projects: number, acknowledged = false) {
  const dWord = count === 1 ? '1 decision is' : `${count} decisions are`;
  const pWord = projects === 1 ? '1 project' : `${projects} projects`;
  return {
    id: DECISIONS_NOTIF_ID,
    subscriptionId: NIL_UUID,
    agentId: NIL_UUID,
    projectId: NIL_UUID,
    subscriberType: 'user',
    subscriberId: SELF_ID,
    status: 'DECISIONS_WAITING',
    message: count > 0 ? `${dWord} waiting on you in ${pWord}` : 'No decisions are waiting on you',
    dispatched: false,
    acknowledged,
    createdAt: '2026-11-15T12:00:00Z',
    kind: 'decisions_waiting',
    resourceType: 'decisions',
    resourceId: String(count),
  };
}

async function openTray(page: Page): Promise<void> {
  const header = page.locator('scion-header');
  if (isPhone(page)) {
    const accountTrigger = header.locator('button.user-trigger[aria-label="Account menu"]');
    await expect(accountTrigger).toBeVisible();
    await accountTrigger.click();
    const notifItem = header.locator('sl-menu-item[value="notifications"]');
    await expect(notifItem).toBeVisible();
    await notifItem.click();
  } else {
    const bellBtn = header.locator('.header-actions sl-icon-button[name="bell"]');
    await expect(bellBtn).toBeVisible();
    await bellBtn.click();
  }
  await expect(page.locator('scion-notification-tray .panel')).toBeVisible();
}

async function setupBellMocks(page: Page, theme: Theme): Promise<MockBellController> {
  const reconcileProbes: string[] = [];
  const ackedIds: string[] = [];
  let currentDecisions = buildDecisionsNotif(3, 2, false);

  const agentCompleted = {
    id: AGENT_NOTIF_ID,
    subscriptionId: 'sub-1',
    agentId: AGENT_ID,
    projectId: PROJECT_ID,
    subscriberType: 'user',
    subscriberId: SELF_ID,
    status: 'COMPLETED',
    message: 'Agent release-bot completed deployment verification',
    dispatched: true,
    acknowledged: false,
    createdAt: '2026-11-15T12:05:00Z',
  };

  await installEventSourceStub(page);
  await page.addInitScript((t: Theme) => {
    localStorage.setItem('scion-theme', t);
    window.__SCION_FEATURES__ = {
      ...window.__SCION_FEATURES__,
      'web.native_chat': true,
      'web.decisions': true,
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
  await page.route('**/api/v1/users**', (route) => {
    const url = route.request().url();
    if (url.includes('/api/v1/users/me/decisions')) {
      reconcileProbes.push(url);
      void route.fulfill({
        json: {
          counts: {
            waitingOnYou: currentDecisions.acknowledged ? 0 : Number(currentDecisions.resourceId),
            waitingOnOthers: 0,
          },
          prompts: [],
          agents: [],
          answerers: [],
          conversations: [],
          projects: [],
          truncatedProjects: false,
        },
      });
      return;
    }
    void route.fulfill({ json: { users: [] } });
  });
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
  await page.route(`/api/v1/projects/${PROJECT_ID}/decisions**`, (route) =>
    route.fulfill({
      json: {
        counts: { waitingOnYou: 2, waitingOnOthers: 0 },
        prompts: [],
        agents: [],
        answerers: [],
        conversations: [],
      },
    })
  );
  await page.route(/\/(metrics-summary|session-metrics)(\?|$)/, (route) =>
    route.fulfill({ json: { available: false } })
  );

  await page.route('**/api/v1/notifications**', (route) => {
    const url = route.request().url();
    const ackMatch = url.match(/\/api\/v1\/notifications\/([^/]+)\/ack$/);
    if (ackMatch && route.request().method() === 'POST') {
      const id = decodeURIComponent(ackMatch[1] ?? '');
      ackedIds.push(id);
      if (id === DECISIONS_NOTIF_ID) {
        currentDecisions = { ...currentDecisions, acknowledged: true };
      }
      void route.fulfill({ json: { ok: true } });
      return;
    }
    // Deliberately return the newer agent row FIRST so the tray proves client-side pinning.
    const list = currentDecisions.acknowledged
      ? [agentCompleted]
      : [agentCompleted, currentDecisions];
    void route.fulfill({ json: list });
  });

  return {
    reconcileProbes,
    ackedIds,
    setServerDecisionsCount: (count: number, projects: number, acknowledged: boolean) => {
      currentDecisions = buildDecisionsNotif(count, projects, acknowledged);
    },
  };
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`decisions bell (${theme})`, () => {
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('B-1..B-3: pinned Decisions item, live SSE count updates, Mark read + resurface, acknowledged clear, and Open Decisions navigation', async ({
      page,
    }, testInfo) => {
      const ctrl = await setupBellMocks(page, theme);

      await page.goto(`/projects/${PROJECT_ID}`, { waitUntil: 'domcontentloaded' });
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

      // Login B6 reconcile probe runs with limit=0.
      await expect
        .poll(() => ctrl.reconcileProbes.some((u) => u.includes('tab=pending&limit=0')))
        .toBe(true);

      if (!isPhone(page)) {
        await expect(page.locator('scion-header .header-actions .trigger-badge')).toHaveText('2');
      }

      const probesBeforeOpen = ctrl.reconcileProbes.length;
      await openTray(page);
      await expect.poll(() => ctrl.reconcileProbes.length).toBeGreaterThan(probesBeforeOpen);

      const tray = page.locator('scion-notification-tray');
      const items = tray.locator('.notif-item');
      await expect(items).toHaveCount(2);

      // Pinned item is always index 0, even though API returned agentCompleted first.
      const pinned = items.nth(0);
      await expect(pinned).toHaveClass(/pinned/);
      await expect(pinned.locator('.notif-icon sl-icon')).toHaveAttribute('name', 'signpost-split');
      await expect(pinned.locator('.notif-title')).toHaveText('Decisions waiting');
      await expect(pinned.locator('.notif-message')).toHaveText(
        '3 decisions are waiting on you in 2 projects'
      );
      await expect(pinned.locator('a.decisions-open-link')).toHaveAttribute(
        'href',
        '/profile/decisions?tab=pending'
      );
      await shot(page, testInfo, theme, '01-bell-pinned-open');

      // Live SSE update (3 -> 2 in 1 project) updates message in place while keeping pinning above a new agent alert.
      ctrl.setServerDecisionsCount(2, 1, false);
      await emitEventSourceUpdate(page, `user.${SELF_ID}.notification`, {
        id: 'a2222222-0000-4000-8000-000000000002',
        subscriptionId: 'sub-2',
        agentId: AGENT_ID,
        projectId: PROJECT_ID,
        subscriberId: SELF_ID,
        status: 'WAITING_FOR_INPUT',
        message: 'Agent qa-bot is waiting on parent input',
        acknowledged: false,
        createdAt: '2026-11-15T12:08:00Z',
      });
      await emitEventSourceUpdate(
        page,
        `user.${SELF_ID}.notification`,
        buildDecisionsNotif(2, 1, false)
      );

      await expect(items).toHaveCount(3);
      await expect(items.nth(0)).toHaveClass(/pinned/);
      await expect(items.nth(0).locator('.notif-message')).toHaveText(
        '2 decisions are waiting on you in 1 project'
      );

      // Mark read hides the Decisions item while leaving agent alerts intact.
      await items.nth(0).locator('button.mark-read-link').click();
      await expect.poll(() => ctrl.ackedIds).toContain(DECISIONS_NOTIF_ID);
      await expect(tray.locator('.notif-item.pinned')).toHaveCount(0);
      await expect(items).toHaveCount(2);
      await shot(page, testInfo, theme, '02-bell-after-mark-read');

      // New prompt increases count (2 -> 3) -> resurfaces live at index 0!
      ctrl.setServerDecisionsCount(3, 1, false);
      await emitEventSourceUpdate(
        page,
        `user.${SELF_ID}.notification`,
        buildDecisionsNotif(3, 1, false)
      );
      await expect(tray.locator('.notif-item.pinned')).toHaveCount(1);
      await expect(items.nth(0).locator('.notif-message')).toHaveText(
        '3 decisions are waiting on you in 1 project'
      );

      // Answering all clears the pinned Decisions row live on acknowledged: true.
      ctrl.setServerDecisionsCount(0, 0, true);
      await emitEventSourceUpdate(
        page,
        `user.${SELF_ID}.notification`,
        buildDecisionsNotif(0, 0, true)
      );
      await expect(tray.locator('.notif-item.pinned')).toHaveCount(0);
      await shot(page, testInfo, theme, '03-bell-cleared');

      // Re-surface once more and verify "Open Decisions" navigates to /profile/decisions?tab=pending and closes the tray.
      ctrl.setServerDecisionsCount(1, 1, false);
      await emitEventSourceUpdate(
        page,
        `user.${SELF_ID}.notification`,
        buildDecisionsNotif(1, 1, false)
      );
      await expect(items.nth(0).locator('.notif-message')).toHaveText(
        '1 decision is waiting on you in 1 project'
      );
      await items.nth(0).locator('a.decisions-open-link').click();
      await expect(page).toHaveURL(/\/profile\/decisions\?tab=pending$/);
      await expect(tray.locator('.panel')).toHaveCount(0);
    });
  });
}
