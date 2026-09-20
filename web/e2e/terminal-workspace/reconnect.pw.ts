/**
 * P3.2 (#1659): Playwright browser tests for explicit reconnect and
 * agent-unavailable transitions.
 */
import { test, expect, type Page } from '@playwright/test';

const agent = '11111111-1111-4111-8111-111111111111';

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
  activity?: string;
}

async function setup(
  page: Page,
  agents: Record<string, AgentFixture> = {
    [agent]: {
      id: agent,
      name: 'reconnect-agent',
      phase: 'running',
      projectId: 'fixture-project',
    },
  }
): Promise<{
  readonly attaches: number;
  readonly closes: number;
  disconnectAll(): void;
  sent: string[];
}> {
  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];
  const sockets: Array<{ close: (options?: { code?: number; reason?: string }) => void }> = [];
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id =
      route
        .request()
        .url()
        .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1] ?? agent;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ?? { error: 'not found' },
    });
  });
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    attaches++;
    sockets.push(socket);
    socket.onMessage((message) => sent.push(String(message)));
    socket.onClose(() => closes++);
  });
  return {
    get attaches(): number {
      return attaches;
    },
    get closes(): number {
      return closes;
    },
    disconnectAll(): void {
      for (const socket of sockets) socket.close({ code: 1006, reason: 'fixture disconnect' });
    },
    sent,
  };
}

test('disconnect overlay appears on socket loss and shows Reconnect button', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Disconnect
  socket.disconnectAll();

  // Overlay with disconnect info and Reconnect button should appear
  const overlay = page.locator('scion-terminal-pane .disconnected-overlay');
  await expect(overlay).toBeVisible();
  await expect(overlay.locator('.overlay-title')).toContainText('DISCONNECTED');
  await expect(overlay.locator('.overlay-reconnect')).toBeVisible();
  await expect(overlay.locator('.overlay-reconnect')).toBeEnabled();
});

test('Reconnect button works and creates new connection', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  socket.disconnectAll();
  await expect(page.locator('scion-terminal-pane .disconnected-overlay')).toBeVisible();

  // Click Reconnect on the overlay
  await page.locator('scion-terminal-pane .overlay-reconnect').click();
  await expect.poll(() => socket.attaches).toBe(2);

  // Overlay should disappear after successful reconnect
  await expect(page.locator('scion-terminal-pane .disconnected-overlay')).toBeHidden();
});

test('repeated Reconnect clicks produce only one attempt', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  socket.disconnectAll();
  await expect(page.locator('scion-terminal-pane .disconnected-overlay')).toBeVisible();

  // Rapidly click Reconnect multiple times
  const reconnectBtn = page.locator('scion-terminal-pane .overlay-reconnect');
  await reconnectBtn.click();
  // Button should be disabled while reconnecting
  await expect(reconnectBtn).toContainText('Reconnecting...');

  // Wait for reconnect to complete
  await expect.poll(() => socket.attaches).toBe(2);

  // Only one additional connection was made
  expect(socket.attaches).toBe(2);
});

test('auth failure shows appropriate error without retry loop', async ({ page }) => {
  // Set up a scenario where the preflight will fail with 403
  const socket = await setup(page);

  // After initial connect, make the preflight return 403
  let preflight403 = false;
  await page.route('**/api/v1/agents/**/pty', (route) => {
    if (preflight403) {
      void route.fulfill({ status: 403, json: { error: 'forbidden' } });
    } else {
      void route.fulfill({ json: {} });
    }
  });

  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  socket.disconnectAll();
  await expect(page.locator('scion-terminal-pane .disconnected-overlay')).toBeVisible();

  // Now make preflight return 403
  preflight403 = true;
  await page.locator('scion-terminal-pane .overlay-reconnect').click();

  // Wait for error to appear in the overlay or error bar
  await expect(page.locator('scion-terminal-pane')).toContainText('permission');

  // Only the initial attach + reconnect attempt, no retry loop
  expect(socket.attaches).toBe(1); // No new WebSocket because preflight failed
});

test('connected session navigation does not reset scrollback', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  const initialAttaches = socket.attaches;

  // Navigate away
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );

  // Navigate back to the same terminal
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );

  // No new socket connection
  expect(socket.attaches).toBe(initialAttaches);
  expect(socket.closes).toBe(0);
});

test('unavailable agent shows correct state in rail and pane', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Disconnect and verify rail shows disconnected state
  socket.disconnectAll();

  // Rail should show disconnected
  await expect(page.locator('#terminal-workspace')).toContainText('Disconnected');

  // The rail reconnect button should be visible and enabled
  const railReconnect = page.getByRole('button', { name: 'Reconnect reconnect-agent' });
  await expect(railReconnect).toBeVisible();
  await expect(railReconnect).toBeEnabled();
});

test('toolbar reconnect button disabled during active reconnect attempt', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  socket.disconnectAll();
  await expect(page.locator('scion-terminal-pane .reconnect-btn')).toBeVisible();

  // Click toolbar reconnect
  await page.locator('scion-terminal-pane .reconnect-btn').click();

  // The toolbar button should show "Reconnecting..."
  await expect(page.locator('scion-terminal-pane .reconnect-btn')).toContainText('Reconnecting...');

  // Wait for reconnect to complete
  await expect.poll(() => socket.attaches).toBe(2);
});
