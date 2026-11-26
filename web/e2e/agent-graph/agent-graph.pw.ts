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

/**
 * Offline Playwright E2E regression spec for agent-graph node opacity.
 *
 * Verifies in real Chromium that the `.node` entrance animation (`node-in`)
 * does not pin `opacity: 1` or `transform` in the CSS Animations cascade
 * origin after settling, so `.node.context` ancestors render dimmed at
 * `--scion-graph-context-opacity` (0.45), brighten to 1 on hover, and
 * `.node.dim` unrelated nodes dim to 0.3 on hover.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

const MOCK_AGENTS = [
  {
    id: 'agent-parent',
    slug: 'parent-coord',
    name: 'Parent Coord',
    projectId: 'proj-1',
    projectSlug: 'alpha',
    phase: 'stopped',
    status: 'stopped',
    harnessConfig: 'claude',
    ancestry: ['user:u1'],
    updated: '2026-11-25T12:00:00Z',
  },
  {
    id: 'agent-child',
    slug: 'child-dev',
    name: 'Child Dev',
    projectId: 'proj-1',
    projectSlug: 'alpha',
    phase: 'running',
    status: 'running',
    harnessConfig: 'antigravity',
    ancestry: ['user:u1', 'agent-parent'],
    updated: '2026-11-25T12:01:00Z',
  },
  {
    id: 'agent-sibling',
    slug: 'sibling-dev',
    name: 'Sibling Dev',
    projectId: 'proj-1',
    projectSlug: 'alpha',
    phase: 'running',
    status: 'running',
    harnessConfig: 'gemini-cli',
    ancestry: ['user:u1'],
    updated: '2026-11-25T12:02:00Z',
  },
  {
    id: 'agent-unrelated',
    slug: 'unrelated-dev',
    name: 'Unrelated Dev',
    projectId: 'proj-1',
    projectSlug: 'alpha',
    phase: 'stopped',
    status: 'stopped',
    harnessConfig: 'codex',
    ancestry: ['user:u1'],
    updated: '2026-11-25T12:03:00Z',
  },
];

async function installMockGraphHub(page: Page): Promise<void> {
  await installEventSourceStub(page);

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'u1',
        email: 'dev@example.test',
        name: 'Test Dev',
        role: 'admin',
      }),
    })
  );

  await page.route('**/api/**', (route) => {
    const url = new URL(route.request().url());
    const p = url.pathname;

    if (p === '/api/v1/system/status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: '{"complete":true}',
      });
    }
    if (p === '/api/v1/settings/public') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ appName: 'Scion', onboardingComplete: true }),
      });
    }
    if (p === '/api/v1/experiments') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ experiments: {} }),
      });
    }
    if (p === '/api/v1/projects') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          projects: [{ id: 'proj-1', slug: 'alpha', name: 'Alpha' }],
        }),
      });
    }
    if (p === '/api/v1/projects/proj-1') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'proj-1',
          slug: 'alpha',
          name: 'Alpha',
          _capabilities: { actions: ['read'] },
        }),
      });
    }
    if (p === '/api/v1/agents' || p === '/api/v1/projects/proj-1/agents') {
      const idsParam = url.searchParams.get('ids');
      const q = url.searchParams.get('q') ?? '';
      let rows = MOCK_AGENTS;
      if (idsParam) {
        const wanted = new Set(idsParam.split(',').filter(Boolean));
        rows = MOCK_AGENTS.filter((a) => wanted.has(a.id));
      } else if (q.includes('harness:antigravity')) {
        // Simulate server-side ?q= narrowing (non-matching ancestors omitted).
        rows = MOCK_AGENTS.filter((a) => a.harnessConfig === 'antigravity');
      }
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          agents: rows,
          complete: true,
          totalCount: rows.length,
        }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
}

async function waitForAnimationsToFinish(locator: Locator): Promise<void> {
  await locator.evaluate(async (el) => {
    const anims = el.getAnimations();
    await Promise.all(anims.map((a) => a.finished.catch(() => undefined)));
  });
}

test('dims non-matching context ancestors after node-in settles and brightens on hover', async ({
  page,
}) => {
  await installMockGraphHub(page);

  await page.goto('/agents/graph?q=phase:running', { waitUntil: 'domcontentloaded' });

  const parentNode = page.locator('a.node[data-agent-id="agent-parent"]');
  const childNode = page.locator('a.node[data-agent-id="agent-child"]');

  await expect(parentNode).toBeVisible();
  await expect(childNode).toBeVisible();
  await expect(parentNode).toHaveClass(/\bcontext\b/);
  await expect(parentNode).toHaveAttribute(
    'aria-description',
    'shown as context; does not match filter'
  );
  await expect(childNode).not.toHaveClass(/\bcontext\b/);

  // Wait for the 0.35s node-in entrance animation to finish.
  await waitForAnimationsToFinish(parentNode);
  await waitForAnimationsToFinish(childNode);

  // Context ancestor must settle at --scion-graph-context-opacity (0.45), not 1.
  await expect
    .poll(async () => parentNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(0.45, 2);

  // Matching child must settle at full opacity (1).
  await expect
    .poll(async () => childNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(1, 2);

  // Hovering the context ancestor brightens it to opacity 1 and applies the hover scale(1.02) transform.
  await parentNode.hover();
  await expect
    .poll(async () => parentNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(1, 2);
  await expect
    .poll(async () =>
      parentNode.evaluate((el) => {
        const m = /^matrix\(([^,]+),/.exec(getComputedStyle(el).transform);
        return m ? Number.parseFloat(m[1]) : 0;
      })
    )
    .toBeCloseTo(1.02, 2);
});

test('dims unrelated nodes (.node.dim) when hovering a lineage branch', async ({ page }) => {
  await installMockGraphHub(page);

  await page.goto('/agents/graph?q=phase:running', { waitUntil: 'domcontentloaded' });

  const childNode = page.locator('a.node[data-agent-id="agent-child"]');
  const siblingNode = page.locator('a.node[data-agent-id="agent-sibling"]');

  await expect(childNode).toBeVisible();
  await expect(siblingNode).toBeVisible();

  await waitForAnimationsToFinish(childNode);
  await waitForAnimationsToFinish(siblingNode);

  await childNode.hover();
  await expect(siblingNode).toHaveClass(/\bdim\b/);

  await expect
    .poll(async () => siblingNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(0.3, 2);
});

async function assertInlineGraphContextAncestors(page: Page, url: string): Promise<void> {
  await page.goto(url, { waitUntil: 'domcontentloaded' });

  const parentNode = page.locator('a.node[data-agent-id="agent-parent"]');
  const childNode = page.locator('a.node[data-agent-id="agent-child"]');
  const unrelatedNode = page.locator('a.node[data-agent-id="agent-unrelated"]');

  await expect(parentNode).toBeVisible();
  await expect(childNode).toBeVisible();
  await expect(unrelatedNode).toHaveCount(0);

  await expect(parentNode).toHaveClass(/\bcontext\b/);
  await expect(parentNode).toHaveAttribute(
    'aria-description',
    'shown as context; does not match filter'
  );
  await expect(childNode).not.toHaveClass(/\bcontext\b/);

  await waitForAnimationsToFinish(parentNode);
  await waitForAnimationsToFinish(childNode);

  await expect
    .poll(async () => parentNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(0.45, 2);

  await expect
    .poll(async () => childNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(1, 2);

  await parentNode.hover();
  await expect
    .poll(async () => parentNode.evaluate((el) => Number.parseFloat(getComputedStyle(el).opacity)))
    .toBeCloseTo(1, 2);
}

test('renders dimmed context ancestors in /agents inline Graph view on cold load with server-narrowed q', async ({
  page,
}) => {
  await page.addInitScript(() => {
    localStorage.setItem('scion-view-agents', 'graph');
  });
  await installMockGraphHub(page);

  await assertInlineGraphContextAncestors(page, '/agents?q=harness:antigravity');
});

test('renders dimmed context ancestors in /projects/:id inline Graph view on cold load with server-narrowed q', async ({
  page,
}) => {
  await page.addInitScript(() => {
    localStorage.setItem('scion-view-project-agents', 'graph');
  });
  await installMockGraphHub(page);

  await assertInlineGraphContextAncestors(page, '/projects/proj-1?q=harness:antigravity');
});
