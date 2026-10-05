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
 * Chromium: the palette's Agents group against a real, deliberately slow,
 * multi-page `/api/v1/agents`, with a busy hub's per-agent status traffic
 * arriving on the agent feed while the walk is in flight and after it.
 * The group must resolve to its real rows in one walk and then follow the
 * feed's changes — never restart its fetch once per event.
 */

import { test, expect, type Page } from '@playwright/test';
import { emitAgentEvent, setupApiMocks } from './mock-api.js';
import { routeAgentPages } from './route-agent-pages.js';

const PAGE_ONE_AGENT = { id: 'agent-page-one', name: 'Page One Agent', slug: 'page-one' };
const PAGE_TWO_AGENT = { id: 'agent-page-two', name: 'Page Two Agent', slug: 'page-two' };
const CREATED_AGENT = { id: 'agent-created', name: 'Created Agent', slug: 'created' };

function paletteOptions(page: Page) {
  return page.locator('scion-quick-palette .palette-option');
}

function agentsLoading(page: Page) {
  return page.locator('scion-quick-palette [data-palette-group="agents"] .palette-loading');
}

test('agent status events during and after a slow multi-page walk are applied without another agent-list request', async ({
  page,
}) => {
  await setupApiMocks(page);
  // Registered before `goto`, so every request to the endpoint is counted.
  const { fulfilledCount, callCount } = routeAgentPages(
    page,
    [{ agents: [PAGE_ONE_AGENT], nextCursor: 'page-2' }, { agents: [PAGE_TWO_AGENT] }],
    { '': 100, 'page-2': 2000 }
  );
  // The hub's single-agent read, which carries the capabilities and
  // messageability its `created` event leaves out.
  let createdFetches = 0;
  await page.route(`**/api/v1/agents/${CREATED_AGENT.id}`, async (route) => {
    createdFetches++;
    await route.fulfill({
      json: {
        ...CREATED_AGENT,
        projectId: 'p1',
        phase: 'running',
        _capabilities: { actions: ['attach', 'message'] },
        _messageability: { canMessage: true },
      },
    });
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  // The page's members sidebar starts the agent store's hub walk on mount;
  // the palette, opened while it reads its slow second page, joins it. Every
  // request to the endpoint counts.
  await expect.poll(callCount).toBeGreaterThan(0);
  const started = callCount;
  const fulfilled = fulfilledCount;

  await page.keyboard.press('Control+k');
  // Joined mid-walk: the rows read so far, while the slow second page is
  // still on its way.
  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible();
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toHaveCount(0);

  // Status traffic while the walk is still reading pages.
  for (let i = 0; i < 4; i++) {
    await page.waitForTimeout(300);
    await emitAgentEvent(page, 'p1', 'status', { agentId: PAGE_ONE_AGENT.id, activity: 'working' });
  }

  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible({
    timeout: 10_000,
  });
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toBeVisible();
  await expect(agentsLoading(page)).toHaveCount(0);
  expect(fulfilled()).toBe(2);

  // After the walk: one agent stops being messageable and another is
  // created. The open group follows both, from the feed alone.
  await emitAgentEvent(page, 'p1', 'status', {
    agentId: PAGE_TWO_AGENT.id,
    _messageability: { canMessage: false },
  });
  await emitAgentEvent(page, 'p1', 'created', {
    agentId: CREATED_AGENT.id,
    projectId: 'p1',
    name: CREATED_AGENT.name,
    slug: CREATED_AGENT.slug,
    phase: 'running',
  });
  await expect(paletteOptions(page).filter({ hasText: PAGE_TWO_AGENT.name })).toHaveCount(0);
  await expect(paletteOptions(page).filter({ hasText: CREATED_AGENT.name })).toBeVisible();

  expect(createdFetches).toBe(1);

  // No further agent-list request within 1.5 s: still the one walk.
  await page.waitForTimeout(1_500);
  expect(started()).toBe(2);
  expect(fulfilled()).toBe(2);
  expect(createdFetches).toBe(1);

  // Reopening answers from the store, without another walk.
  await page.keyboard.press('Escape');
  await page.waitForFunction(
    () =>
      !(document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
  // Let the close animation finish so the shortcut opens rather than queues.
  await page.waitForTimeout(500);
  await page.keyboard.press('Control+k');
  await expect(paletteOptions(page).filter({ hasText: CREATED_AGENT.name })).toBeVisible();
  await expect(paletteOptions(page).filter({ hasText: PAGE_ONE_AGENT.name })).toBeVisible();
  await page.waitForTimeout(500);
  expect(started()).toBe(2);
});
