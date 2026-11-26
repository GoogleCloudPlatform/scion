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
 * The terminal viewer on a phone: the terminal takes the screen, and the
 * terminal list, Jump to agent, the app header and the pane's secondary
 * actions sit behind menus.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

const agent = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';

const fixtures: Record<string, Record<string, unknown>> = {
  [agent]: { id: agent, name: 'first-agent', phase: 'running', projectId: 'p1', project: 'demo' },
  [agentB]: {
    id: agentB,
    name: 'second-agent',
    phase: 'running',
    projectId: 'p1',
    project: 'demo',
  },
};

async function mockHub(page: Page): Promise<void> {
  await installEventSourceStub(page);
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/agents/**', (route) => {
    const id = route
      .request()
      .url()
      .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1];
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      status: id && fixtures[id] ? 200 : 404,
      json: (id && fixtures[id]) ?? { error: 'not found' },
    });
  });
  await page.route('**/api/v1/users/me/terminal-workspace', (route) =>
    route.fulfill({
      json: {
        agentIds: [agent, agentB],
        frontmostAgentId: agent,
        revision: 1,
        updatedAt: new Date().toISOString(),
        pruned: 0,
      },
    })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    socket.send(JSON.stringify({ type: 'data', data: Buffer.from('$ ').toString('base64') }));
  });
}

type KeyboardWindow = Window & { __setTestKeyboard: (frameHeight: number | null) => void };

/**
 * Chromium's visual viewport never falls short of the layout viewport, so,
 * as in e2e/chat-mobile/keyboard-frame.pw.ts, replace it before the app
 * loads with one whose height the test sets, and fire its resize as iOS
 * does when the keyboard opens.
 */
async function installFakeKeyboard(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const vv = Object.assign(new EventTarget(), {
      scale: 1,
      offsetTop: 0,
      offsetLeft: 0,
      pageTop: 0,
      pageLeft: 0,
      width: window.innerWidth,
      height: window.innerHeight,
    });
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => vv });
    (window as unknown as KeyboardWindow).__setTestKeyboard = (frameHeight): void => {
      vv.height = frameHeight ?? window.innerHeight;
      vv.dispatchEvent(new Event('resize'));
    };
  });
}

/** The visible pane's terminal area. */
const terminalArea = (page: Page): Locator =>
  page.locator('scion-terminal-pane:not([hidden]) .terminal-wrapper');
const menuButton = (page: Page): Locator =>
  page.locator('scion-terminal-pane:not([hidden]) .menu-btn');
const rail = (page: Page): Locator => page.locator('.terminal-rail');

async function open(page: Page, id = agent): Promise<void> {
  await mockHub(page);
  await page.goto(`/terminals/${id}`);
  await expect(terminalArea(page)).toBeVisible();
}

async function box(
  page: Page,
  selector: Locator
): Promise<{ x: number; y: number; width: number; height: number }> {
  const b = await selector.boundingBox();
  if (!b) throw new Error('no box');
  return b;
}

// A fresh dev server optimizes the terminal workspace's lazily imported
// dependencies on the first load, which fails that load ("Outdated
// Optimize Dep"); load it once before the tests.
test.beforeAll(async ({ browser }) => {
  const page = await browser.newPage();
  await mockHub(page);
  let loaded = false;
  for (let attempt = 0; attempt < 3 && !loaded; attempt++) {
    await page.goto(`/terminals/${agent}`);
    loaded = await terminalArea(page)
      .waitFor({ timeout: 10_000 })
      .then(() => true)
      .catch(() => false);
  }
  await page.close();
  if (!loaded) throw new Error('the terminal workspace never loaded (warm-up)');
});

test('the terminal takes the screen; the list, header and layout bar are off it', async ({
  page,
}) => {
  await open(page);
  const vh = page.viewportSize()!.height;
  const area = await box(page, terminalArea(page));
  expect(area.height).toBeGreaterThan(vh * 0.85);
  expect(area.y + area.height).toBeLessThanOrEqual(vh + 1);
  await expect(rail(page)).toBeHidden();
  const railBox = await rail(page).boundingBox();
  expect(railBox === null || railBox.x + railBox.width <= 0).toBe(true);
  await expect(page.locator('#terminal-workspace scion-header')).toBeHidden();
  await expect(page.locator('.terminal-layout-bar')).toBeHidden();
  // The pane header is one row.
  const toolbar = await box(page, page.locator('scion-terminal-pane:not([hidden]) .toolbar'));
  expect(toolbar.height).toBeLessThanOrEqual(52);
});

test('the menu button opens the drawer with the list, Jump to agent and the header', async ({
  page,
}) => {
  await open(page);
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();
  await expect(rail(page).locator('scion-header')).toBeVisible();
  await expect(rail(page).getByRole('button', { name: /Jump to agent/ })).toBeVisible();
  await expect(rail(page).getByText('second-agent')).toBeVisible();
  // Choosing a terminal closes the drawer and shows it.
  await rail(page).getByText('second-agent').click();
  await expect(rail(page)).toBeHidden();
  await expect(page).toHaveURL(`/terminals/${agentB}`);
  await expect(page.locator('scion-terminal-pane:not([hidden]) .agent-name')).toHaveText(
    'second-agent'
  );
});

test('the scrim and Escape close the drawer', async ({ page }) => {
  await open(page);
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();
  await page.locator('.terminal-drawer-scrim').click({ position: { x: 380, y: 400 } });
  await expect(rail(page)).toBeHidden();
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(rail(page)).toBeHidden();
  // Focus returns to the menu button.
  await expect(menuButton(page)).toBeFocused();
  await expect(menuButton(page)).toHaveAttribute('aria-expanded', 'false');
});

test('the open drawer is a modal dialog over an inert terminal', async ({ page }) => {
  await open(page);
  await menuButton(page).click();
  await expect(rail(page)).toHaveAttribute('role', 'dialog');
  await expect(rail(page)).toHaveAttribute('aria-modal', 'true');
  await expect(page.locator('.terminal-pane-area')).toHaveJSProperty('inert', true);
  await expect(rail(page).locator('[aria-current="page"]')).toBeFocused();
});

test('turning to a wide viewport with the drawer open puts the list back', async ({ page }) => {
  await open(page);
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(page.locator('#terminal-workspace')).toHaveAttribute('data-phone-drawer', 'off');
  await expect(rail(page)).toBeVisible();
  await expect(rail(page)).toHaveJSProperty('inert', false);
  await expect(page.locator('#terminal-workspace > scion-header')).toBeVisible();
  await expect(page.locator('.terminal-pane-area')).toHaveJSProperty('inert', false);
});

test('the pane actions are in the More menu', async ({ page }) => {
  await open(page);
  const pane = page.locator('scion-terminal-pane:not([hidden])');
  await expect(pane.locator('.back-link')).toHaveCount(0);
  await expect(pane.locator('button.toolbar-overflow-btn')).toBeVisible();
  await pane.locator('button.toolbar-overflow-btn').click();
  const menu = pane.locator('.toolbar-overflow sl-menu');
  await expect(menu.getByRole('menuitem', { name: /Back to Agent/ })).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: /Back to demo/ })).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: /Open in graph/ })).toBeVisible();
  await menu.getByRole('menuitem', { name: /Back to Agent/ }).click();
  await expect(page).toHaveURL(`/agents/${agent}`);
});

test('picking a terminal from the header Terminals dropdown closes the drawer and focuses the pane', async ({
  page,
}) => {
  await open(page);
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();

  const header = rail(page).locator('scion-header');
  const modeTrigger = header.locator('.mode-trigger');
  await modeTrigger.click();
  await header.locator('sl-menu-item', { hasText: 'second-agent' }).first().click();

  await expect(page.locator('#terminal-workspace')).toHaveAttribute('data-phone-drawer', 'closed');
  await expect(page.locator('.terminal-pane-area')).toHaveJSProperty('inert', false);
  await expect(page).toHaveURL(`/terminals/${agentB}`);
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.tagName === 'SCION-TERMINAL-PANE'))
    .toBe(true);

  // Picking the already-shown terminal from the header dropdown also closes the drawer.
  await menuButton(page).click();
  await expect(page.locator('#terminal-workspace')).toHaveAttribute('data-phone-drawer', 'open');
  await modeTrigger.click();
  await header.locator('sl-menu-item', { hasText: 'second-agent' }).first().click();

  await expect(page.locator('#terminal-workspace')).toHaveAttribute('data-phone-drawer', 'closed');
  await expect(page.locator('.terminal-pane-area')).toHaveJSProperty('inert', false);
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.tagName === 'SCION-TERMINAL-PANE'))
    .toBe(true);
});

test('with the keyboard up, the terminal fills what the keyboard leaves and opening the drawer blurs xterm', async ({
  page,
}) => {
  await installFakeKeyboard(page);
  await open(page);
  // Focus xterm's helper textarea inside the pane's shadow root.
  const helper = page.locator('scion-terminal-pane:not([hidden]) .xterm-helper-textarea');
  await expect(helper).toBeAttached();
  await helper.focus();
  await expect
    .poll(() =>
      page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        return el?.className ?? '';
      })
    )
    .toContain('xterm-helper-textarea');

  const frame = 844 - 336; // a 336px iPhone keyboard
  await page.evaluate((h) => (window as unknown as KeyboardWindow).__setTestKeyboard(h), frame);
  await expect
    .poll(async () => {
      const b = await terminalArea(page).boundingBox();
      return b ? Math.round(b.y + b.height) : -1;
    })
    .toBeLessThanOrEqual(frame + 1);
  const area = await box(page, terminalArea(page));
  expect(area.height).toBeGreaterThan(frame * 0.8);
  // The menu still opens the drawer with the keyboard up and blurs xterm's textarea.
  await menuButton(page).click();
  await expect(rail(page)).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        const railEl = document.querySelector('.terminal-rail');
        return {
          inRail: Boolean(el && railEl?.contains(el)),
          isXtermTextarea: Boolean(el?.classList.contains('xterm-helper-textarea')),
        };
      })
    )
    .toEqual({ inRail: true, isXtermTextarea: false });
  await page.keyboard.press('Escape');
  await expect(rail(page)).toBeHidden();
  // Keyboard down: the full screen again.
  await page.evaluate(() => (window as unknown as KeyboardWindow).__setTestKeyboard(null));
  await expect
    .poll(async () => (await terminalArea(page).boundingBox())?.height ?? 0)
    .toBeGreaterThan(844 * 0.85);
});

test('with no terminal open, the list stays on screen', async ({ page }) => {
  await mockHub(page);
  await page.route('**/api/v1/users/me/terminal-workspace', (route) =>
    route.fulfill({
      json: {
        agentIds: [],
        frontmostAgentId: null,
        revision: 1,
        updatedAt: new Date().toISOString(),
        pruned: 0,
      },
    })
  );
  await page.goto('/terminals');
  await expect(rail(page)).toBeVisible();
  await expect(page.locator('#terminal-workspace > scion-header')).toBeVisible();
});
