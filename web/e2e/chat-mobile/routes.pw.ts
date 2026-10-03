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
 * App-shell routes on a phone: nothing runs off the side of the page, and
 * the shell's content scroller never scrolls sideways.
 *
 * The document cannot overflow in frame mode, so a too-wide row instead
 * makes the content scroller pan sideways, or is clipped by its own
 * container. Both are checked: the scroller's width, and every visible
 * element's right edge, except inside a container that is meant to scroll
 * sideways (a segmented control or tab strip that scrolls on a narrow
 * screen).
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail } from './fixture.js';
import { PROJECT_A } from './mock-api.js';
import { assertFramePinned } from './helpers.js';

const AGENT = {
  id: 'agent-routes-1',
  name: 'coder-one',
  slug: 'coder-one',
  status: 'running',
  phase: 'running',
  activity: 'idle',
  projectId: PROJECT_A.id,
  harness: 'claude',
  template: 'default',
  createdAt: '2026-09-01T00:00:00Z',
  updatedAt: '2026-09-01T00:00:00Z',
  taskSummary: 'A fairly long task summary that has to wrap or truncate on a narrow screen',
};

async function listMocks(page: Page): Promise<void> {
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) =>
    route.fulfill({ json: { agents: [AGENT] } })
  );
  await page.route(/\/api\/v1\/projects(\?|$)/, (route) =>
    route.fulfill({
      json: {
        projects: [{ id: PROJECT_A.id, name: PROJECT_A.name, slug: PROJECT_A.slug, agentCount: 1 }],
      },
    })
  );
}

/** Elements allowed to clip and scroll their own content sideways. */
const SIDE_SCROLLERS = ['[part~="tabs"]', '[part~="nav"]'];

async function assertRouteFits(page: Page): Promise<void> {
  const result = await page.evaluate((sideScrollers) => {
    const app = document.querySelector('scion-app')!;
    const content = app.shadowRoot!.querySelector('.content') as HTMLElement;
    const offenders: string[] = [];
    const walk = (root: ParentNode): void => {
      const clipped: Element[] = [];
      for (const el of root.querySelectorAll('*')) {
        if (clipped.some((scroller) => scroller.contains(el))) continue;
        const cs = getComputedStyle(el);
        if (cs.display === 'none' || cs.visibility === 'hidden') continue;
        const r = el.getBoundingClientRect();
        if (r.width > 0 && r.height > 0 && r.right > window.innerWidth + 0.5) {
          offenders.push(`${el.tagName.toLowerCase()}.${String(el.className)} right=${r.right}`);
        }
        if (sideScrollers.some((selector) => el.matches(selector))) {
          clipped.push(el);
          continue;
        }
        if (el.shadowRoot) walk(el.shadowRoot);
      }
    };
    walk(app.shadowRoot!);
    return {
      scrollWidth: content.scrollWidth,
      clientWidth: content.clientWidth,
      offenders: offenders.slice(0, 8),
    };
  }, SIDE_SCROLLERS);
  expect(result.scrollWidth, 'the content scroller does not scroll sideways').toBeLessThanOrEqual(
    result.clientWidth
  );
  expect(result.offenders, 'nothing past the right edge').toEqual([]);
}

test.describe('app-shell routes fit a phone', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'phone widths');
  });

  for (const route of [
    { path: '/agents', ready: 'coder-one' },
    { path: '/projects', ready: PROJECT_A.name },
  ]) {
    test(`${route.path} has no horizontal overflow`, async ({ page }) => {
      await openChatRail(page, listMocks);
      await page.goto(route.path, { waitUntil: 'domcontentloaded' });
      await expect(page.getByText(route.ready).first()).toBeVisible({ timeout: 10_000 });
      await assertRouteFits(page);
      await assertFramePinned(page);
    });
  }

  test('the agent detail header keeps every action on screen', async ({ page }) => {
    // A running agent the user may message, attach to, stop and delete:
    // the widest set of header actions.
    const detail = {
      ...AGENT,
      _capabilities: { actions: ['read', 'message', 'lifecycle', 'attach', 'delete'] },
    };
    await openChatRail(page, async (p) => {
      await listMocks(p);
      await p.route(new RegExp(`/api/v1/agents/${AGENT.id}(\\?|$)`), (route) =>
        route.fulfill({ json: detail })
      );
    });
    await page.goto(`/agents/${AGENT.id}`, { waitUntil: 'domcontentloaded' });
    const actions = page.locator('scion-page-agent-detail .header-actions');
    await expect(actions).toBeVisible({ timeout: 10_000 });
    const buttons = await actions.locator('sl-button').evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        return { label: el.textContent?.trim() ?? '', left: r.left, right: r.right };
      })
    );
    expect(buttons.length, 'several actions render').toBeGreaterThanOrEqual(4);
    const width = await page.evaluate(() => window.innerWidth);
    for (const b of buttons) {
      expect(b.right, `${b.label || 'icon'} button inside the screen`).toBeLessThanOrEqual(
        width + 0.5
      );
      expect(b.left, `${b.label || 'icon'} button inside the screen`).toBeGreaterThanOrEqual(0);
    }
    await assertRouteFits(page);
  });

  test('every agent status filter is on screen', async ({ page }) => {
    await openChatRail(page, listMocks);
    await page.goto('/agents', { waitUntil: 'domcontentloaded' });
    const group = page.locator('.filter-bar .scope-toggle');
    await expect(group).toBeVisible({ timeout: 10_000 });
    // Every button is wholly visible: inside the group's box (which clips
    // its corners) and the screen, with nothing hidden behind a scroll.
    const state = await group.evaluate((el) => {
      const box = el.getBoundingClientRect();
      return {
        scrolls: el.scrollWidth > el.clientWidth,
        innerWidth: window.innerWidth,
        box: { left: box.left, right: box.right, top: box.top, bottom: box.bottom },
        buttons: [...el.querySelectorAll('button')].map((b) => {
          const r = b.getBoundingClientRect();
          return {
            label: b.textContent?.trim() ?? '',
            left: r.left,
            right: r.right,
            top: r.top,
            bottom: r.bottom,
          };
        }),
      };
    });
    expect(state.scrolls, 'nothing hidden behind a sideways scroll').toBe(false);
    expect(state.buttons.map((b) => b.label)).toEqual([
      'All',
      'Running',
      'Stopped',
      'Suspended',
      'Error',
    ]);
    for (const b of state.buttons) {
      expect(b.right, `${b.label} inside the group`).toBeLessThanOrEqual(state.box.right + 0.5);
      expect(b.bottom, `${b.label} inside the group`).toBeLessThanOrEqual(state.box.bottom + 0.5);
      expect(b.left, `${b.label} inside the group`).toBeGreaterThanOrEqual(state.box.left - 0.5);
      expect(b.right, `${b.label} inside the screen`).toBeLessThanOrEqual(state.innerWidth);
    }
  });
});

test.describe('app-shell routes on a notched phone in landscape (844x390)', () => {
  test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

  const INSETS = { top: 0, right: 47, bottom: 21, left: 47 };

  test('the sidebar collapse toggle clears the home indicator', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openChatRail(page, listMocks);
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: INSETS });
    await page.goto('/agents', { waitUntil: 'domcontentloaded' });
    const toggle = page.locator('aside.sidebar scion-nav .collapse-toggle');
    await expect(toggle).toBeVisible({ timeout: 10_000 });
    const box = (await toggle.boundingBox())!;
    expect(box.y + box.height, 'above the bottom inset').toBeLessThanOrEqual(390 - INSETS.bottom);
  });
});
