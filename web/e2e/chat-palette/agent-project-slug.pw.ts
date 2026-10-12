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
 * Chromium: same-named agents in different projects are told apart by the
 * second line of their Agents row, which shows the project slug once the
 * space rail has supplied it, and the project name until then.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks, SPACE_ALPHA, SPACE_BETA } from './mock-api.js';

const COORDINATOR_ALPHA = {
  id: 'agent-coordinator-alpha',
  name: 'coordinator',
  slug: 'coordinator',
  projectId: SPACE_ALPHA.projectId,
  project: SPACE_ALPHA.projectName,
};
const COORDINATOR_BETA = {
  id: 'agent-coordinator-beta',
  name: 'coordinator',
  slug: 'coordinator',
  projectId: SPACE_BETA.projectId,
  project: SPACE_BETA.projectName,
};

function paletteInput(page: Page) {
  return page.locator('scion-quick-palette #palette-query-input');
}

function agentSecondaryLines(page: Page) {
  return page.locator('scion-quick-palette .palette-option .palette-secondary');
}

async function openPaletteWithQuery(page: Page, query: string) {
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await paletteInput(page).fill(query);
}

test('same-named agents in different projects show their project slugs', async ({ page }) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA, SPACE_BETA],
    agents: [COORDINATOR_ALPHA, COORDINATOR_BETA],
  });
  await openPaletteWithQuery(page, 'coordinator');

  await expect(agentSecondaryLines(page)).toHaveText(
    [SPACE_ALPHA.projectSlug, SPACE_BETA.projectSlug].sort()
  );
});

test('a row shows the project name until the slug is known, then the slug', async ({ page }) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA, SPACE_BETA],
    agents: [COORDINATOR_ALPHA, COORDINATOR_BETA],
  });
  // Hold the space rail's list, the page's source of project slugs, until
  // the palette shows its rows.
  let releaseSpaces!: () => void;
  const spacesReleased = new Promise<void>((resolve) => (releaseSpaces = resolve));
  await page.route('**/api/v1/chat/spaces', async (route) => {
    await spacesReleased;
    await route.fallback();
  });
  await openPaletteWithQuery(page, 'coordinator');

  await expect(agentSecondaryLines(page)).toHaveText([
    SPACE_ALPHA.projectName,
    SPACE_BETA.projectName,
  ]);

  releaseSpaces();
  await expect(agentSecondaryLines(page)).toHaveText([
    SPACE_ALPHA.projectSlug,
    SPACE_BETA.projectSlug,
  ]);
});

test("rows show the agent row's project slug before the space rail loads, with no project request", async ({
  page,
}) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA, SPACE_BETA],
    agents: [
      { ...COORDINATOR_ALPHA, projectSlug: SPACE_ALPHA.projectSlug },
      { ...COORDINATOR_BETA, projectSlug: SPACE_BETA.projectSlug },
    ],
  });
  // Hold the space rail's list for the whole test, so the slugs can only
  // come from the agent rows.
  await page.route('**/api/v1/chat/spaces', () => new Promise<void>(() => {}));
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  // A project list or single-project read from palette open on.
  const projectRequests: string[] = [];
  page.on('request', (request) => {
    if (/^\/api\/v1\/projects(\/[^/]+)?$/.test(new URL(request.url()).pathname))
      projectRequests.push(request.url());
  });
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await paletteInput(page).fill('coordinator');

  await expect(agentSecondaryLines(page)).toHaveText([
    SPACE_ALPHA.projectSlug,
    SPACE_BETA.projectSlug,
  ]);
  await paletteInput(page).fill(SPACE_ALPHA.projectName);
  await expect(agentSecondaryLines(page)).toHaveText([SPACE_ALPHA.projectSlug]);
  expect(projectRequests).toEqual([]);
});

test('the project slug is matchable by the query', async ({ page }) => {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA, SPACE_BETA],
    agents: [COORDINATOR_ALPHA, COORDINATOR_BETA],
  });
  await openPaletteWithQuery(page, SPACE_BETA.projectSlug);

  const row = page.locator('scion-quick-palette .palette-option', { hasText: 'coordinator' });
  await expect(row).toHaveCount(1);
  await expect(row.locator('.palette-secondary')).toHaveText(SPACE_BETA.projectSlug);
});
