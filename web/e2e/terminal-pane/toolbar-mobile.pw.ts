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

// Narrow-viewport terminal toolbar overflow regression tests (ptone/scion#9406).
import { expect, test, type Page, type WebSocketRoute } from '@playwright/test';
import type {} from './fixture.js';
import { installEventSourceStub } from '../event-source-stub.js';

const agentId = '11111111-1111-4111-8111-111111111111';

interface NarrowPaneControls {
  disconnect(): Promise<void>;
  startReconnect(): Promise<void>;
  completeReconnect(): Promise<void>;
}

async function setupNarrowPane(
  page: Page,
  options: { keyBar: boolean; compactHeader?: boolean; phoneChrome?: boolean; activity?: string }
): Promise<NarrowPaneControls> {
  const {
    keyBar,
    compactHeader = true,
    phoneChrome = false,
    activity = 'waiting_for_input',
  } = options;
  await page.addInitScript((enabled: boolean) => {
    window.__SCION_FEATURES__ = {
      'web.terminal_key_bar': enabled,
      'web.native_chat': true,
    };
  }, keyBar);
  await installEventSourceStub(page);
  await page.route('**/api/v1/**', (route) => {
    const url = route.request().url();
    if (url.endsWith(`/agents/${agentId}`)) {
      return route.fulfill({
        json: {
          id: agentId,
          name: 'very-long-agent-name-for-overflow-check',
          phase: 'running',
          activity,
          projectId: 'fixture-project',
          harnessAuth: 'none',
          resolvedHarness: 'claude',
          exposedPorts: [
            { port: 3000, label: 'Preview' },
            { port: 8080, label: 'API' },
          ],
        },
      });
    }
    if (url.endsWith('/settings/public')) {
      return route.fulfill({ json: { agent_secrets: { user_scope_only: false } } });
    }
    return route.fulfill({ json: {} });
  });
  let activeSocket: WebSocketRoute | null = null;
  let socketSeq = 0;
  let deferHandshake = false;
  await page.routeWebSocket('**/pty?*', (socket) => {
    activeSocket = socket;
    socketSeq++;
    if (!deferHandshake) {
      socket.send(JSON.stringify({ type: 'data', data: Buffer.from('$ ').toString('base64') }));
    }
  });

  await page.goto('/e2e/terminal-pane/fixture.html');
  await expect
    .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
    .toBe('connected');

  await page.evaluate(
    async ({ compact, phone }: { compact: boolean; phone: boolean }) => {
      const pane = window.paneFixture.pane;
      pane.compactHeader = compact;
      pane.phoneChrome = phone;
      pane.userId = 'fixture-user';
      pane.requestUpdate();
      await pane.updateComplete;
    },
    { compact: compactHeader, phone: phoneChrome }
  );
  await page.waitForTimeout(50);

  return {
    async disconnect(): Promise<void> {
      // Close with 1000 ('detached') so TerminalSession transitions cleanly to
      // 'disconnected' without arming an automatic reconnect.
      await activeSocket?.close({ code: 1000 });
      await expect
        .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
        .toBe('disconnected');
      await page.evaluate(() => window.paneFixture.pane.updateComplete);
    },
    async startReconnect(): Promise<void> {
      const prevSeq = socketSeq;
      deferHandshake = true;
      await page.evaluate(() => {
        void window.paneFixture.session.connect();
      });
      await expect
        .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
        .toBe('connecting');
      await expect.poll(() => socketSeq).toBeGreaterThan(prevSeq);
      await page.evaluate(() => window.paneFixture.pane.updateComplete);
    },
    async completeReconnect(): Promise<void> {
      deferHandshake = false;
      activeSocket?.send(
        JSON.stringify({ type: 'data', data: Buffer.from('$ ').toString('base64') })
      );
      await expect
        .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
        .toBe('connected');
      await page.evaluate(() => window.paneFixture.pane.updateComplete);
    },
  };
}

async function assertNoToolbarOverflow(page: Page, viewportWidth: number): Promise<void> {
  const metrics = await page.evaluate(() => {
    const bar = window.paneFixture.pane.shadowRoot!.querySelector<HTMLElement>('.toolbar')!;
    const barRect = bar.getBoundingClientRect();
    const visibleRightEdges: Array<{ tag: string; cls: string; right: number }> = [];
    for (const child of bar.children) {
      const el = child as HTMLElement;
      const style = getComputedStyle(el);
      if (style.display === 'none' || style.visibility === 'hidden') continue;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) continue;
      visibleRightEdges.push({ tag: el.tagName, cls: el.className, right: rect.right });
    }
    return {
      scrollWidth: bar.scrollWidth,
      clientWidth: bar.clientWidth,
      barRight: barRect.right,
      visibleRightEdges,
    };
  });

  expect(metrics.clientWidth).toBeLessThanOrEqual(viewportWidth);
  expect(metrics.scrollWidth).toBeLessThanOrEqual(metrics.clientWidth);
  for (const child of metrics.visibleRightEdges) {
    expect(
      child.right,
      `${child.tag}.${child.cls} should not extend past toolbar right edge`
    ).toBeLessThanOrEqual(metrics.barRight + 1);
  }
}

for (const width of [390, 375, 320]) {
  for (const keyBar of [true, false]) {
    test.describe(`viewport ${width}px (keyBar=${String(keyBar)})`, () => {
      test.use({ viewport: { width, height: 844 }, isMobile: true, hasTouch: true });

      test('toolbar does not overflow and overflow menu exposes secondary actions', async ({
        page,
      }) => {
        await setupNarrowPane(page, { keyBar });
        await assertNoToolbarOverflow(page, width);

        // Key bar visibility matches the experiment flag.
        if (keyBar) {
          await expect(page.getByRole('group', { name: 'Terminal keys' })).toBeVisible();
        } else {
          await expect(page.getByRole('group', { name: 'Terminal keys' })).toHaveCount(0);
        }

        // Primary controls remain directly visible and usable.
        await expect(page.locator('button[title="Agent window"]')).toBeVisible();
        await expect(page.locator('button[title="Shell window"]')).toBeVisible();

        // Overflow menu trigger is visible and opens all secondary actions.
        const moreBtn = page.getByRole('button', { name: 'More actions' });
        await expect(moreBtn).toBeVisible();
        await moreBtn.click();

        const menu = page.locator('.toolbar-overflow sl-menu');
        await expect(menu).toBeVisible();
        await expect(menu.locator('sl-menu-item[value="graph"]')).toBeVisible();
        await expect(menu.locator('sl-menu-item[value="chat"]')).toBeVisible();
        await expect(menu.locator('sl-menu-item[value="port:3000"]')).toBeVisible();
        await expect(menu.locator('sl-menu-item[value="capture-auth"]')).toBeVisible();

        // Selecting Capture Auth from the overflow menu opens the scope dialog.
        await menu.locator('sl-menu-item[value="capture-auth"]').click();
        await expect(page.locator('sl-dialog[label="Capture Auth — Choose Scope"]')).toBeVisible();
      });
    });
  }
}

test('phoneChrome does not overflow in connected, disconnected, or reconnecting states at 320px/375px/390px', async ({
  page,
}) => {
  const controls = await setupNarrowPane(page, { keyBar: true, phoneChrome: true });

  for (const width of [390, 375, 320]) {
    await page.setViewportSize({ width, height: 844 });
    await page.waitForTimeout(30);
    // Connected state
    await assertNoToolbarOverflow(page, width);
    await expect(page.locator('.toolbar-overflow-btn')).toBeVisible();
    await expect(page.locator('.menu-btn')).toBeVisible();

    // Disconnected state (driven through TerminalSession so ResizeObserver
    // sendResize() callbacks cannot race and overwrite pane state)
    await controls.disconnect();
    await assertNoToolbarOverflow(page, width);
    await expect(page.locator('.toolbar-overflow-btn')).toBeVisible();

    // Reconnecting... state
    await controls.startReconnect();
    await assertNoToolbarOverflow(page, width);
    await expect(page.locator('.toolbar-overflow-btn')).toBeVisible();

    // Verify Reconnecting... lives inside .toolbar-overflow
    await page.locator('.toolbar-overflow-btn').click();
    await expect(page.locator('.toolbar-overflow sl-menu-item[value="reconnect"]')).toContainText(
      'Reconnecting...'
    );
    await page.keyboard.press('Escape');
    await expect(page.locator('.toolbar-overflow sl-menu')).toBeHidden();

    // Restore connected state for next width
    await controls.completeReconnect();
  }
});

test('phoneChrome preserves Back-to-Project and Back-to-Agent navigation in error and loading states at 320px/390px without overflow', async ({
  page,
}) => {
  await setupNarrowPane(page, { keyBar: false, phoneChrome: true });

  await page.evaluate(() => {
    const navPaths: string[] = [];
    (window as unknown as { __navPaths: string[] }).__navPaths = navPaths;
    window.addEventListener('nav-click', (e) => {
      const detail = (e as CustomEvent<{ path?: string }>).detail;
      if (detail?.path) navPaths.push(detail.path);
    });
  });

  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 844 });

    // Error state with no terminal instance
    await page.evaluate(() => {
      const pane = window.paneFixture.pane as unknown as {
        loading: boolean;
        terminal: unknown;
        projectId: string;
        session: { state: { error: string | null } };
        requestUpdate: () => void;
      };
      pane.loading = false;
      pane.terminal = null;
      pane.projectId = 'project with space';
      pane.session.state.error = 'Agent is unavailable';
      pane.requestUpdate();
    });
    await page.waitForTimeout(30);
    await assertNoToolbarOverflow(page, width);

    const overflowBtn = page.locator('.toolbar-overflow-btn');
    await expect(overflowBtn).toBeVisible();
    await overflowBtn.click();
    const projectItem = page.locator('.toolbar-overflow sl-menu-item[value="project"]');
    const agentItem = page.locator('.toolbar-overflow sl-menu-item[value="agent"]');
    await expect(projectItem).toBeVisible();
    await expect(agentItem).toBeVisible();
    await projectItem.click();
    await expect
      .poll(() =>
        page.evaluate(() => (window as unknown as { __navPaths: string[] }).__navPaths.at(-1))
      )
      .toBe('/projects/project%20with%20space');

    // Loading state
    await page.evaluate(() => {
      const pane = window.paneFixture.pane as unknown as {
        loading: boolean;
        requestUpdate: () => void;
      };
      pane.loading = true;
      pane.requestUpdate();
    });
    await page.waitForTimeout(30);
    await assertNoToolbarOverflow(page, width);

    await expect(overflowBtn).toBeVisible();
    await overflowBtn.click();
    await expect(projectItem).toBeVisible();
    await expect(agentItem).toBeVisible();
    await agentItem.click();
    await expect
      .poll(() =>
        page.evaluate(() => (window as unknown as { __navPaths: string[] }).__navPaths.at(-1))
      )
      .toBe(`/agents/${encodeURIComponent(agentId)}`);
  }
});

test('desktop viewport keeps inline controls visible and hides overflow menu', async ({ page }) => {
  await setupNarrowPane(page, { keyBar: false, compactHeader: false });
  await expect(page.locator('.toolbar-overflow-btn')).toBeHidden();
  await expect(page.locator('.capture-auth-btn')).toBeVisible();
  await expect(page.locator('a.port-btn').first()).toBeVisible();
  await assertNoToolbarOverflow(page, 1100);
});
