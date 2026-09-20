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
    const esInstances: EventTarget[] = [];
    (window as unknown as { __sseInstances__: EventTarget[] }).__sseInstances__ = esInstances;
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        esInstances.push(this);
        queueMicrotask(() => {
          // Simulate connected handshake so SSEClient marks the connection open
          const connectedEvent = new MessageEvent('connected', {
            data: JSON.stringify({ connectionId: 'test', subjects: [] }),
          });
          this.dispatchEvent(connectedEvent);
          this.onopen?.();
        });
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

  // Make the agent metadata fetch slow so the reconnect stays pending
  // while we click multiple times. Use a holder object so TypeScript does
  // not narrow the property to null at the call site (TS2349).
  const slowFetch = { resolve: null as (() => void) | null };
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    // Delay the response to keep reconnect in pending state
    slowFetch.resolve = () => {
      void route.fulfill({
        json: {
          id: agent,
          name: 'reconnect-agent',
          phase: 'running',
          projectId: 'fixture-project',
        },
      });
    };
  });

  socket.disconnectAll();
  await expect(page.locator('scion-terminal-pane .disconnected-overlay')).toBeVisible();

  // Click Reconnect button multiple times rapidly via visible UI
  const reconnectBtn = page.locator('scion-terminal-pane .overlay-reconnect');
  await reconnectBtn.click();

  // Button should show "Reconnecting..." and be disabled after first click
  await expect(reconnectBtn).toContainText('Reconnecting...');
  await expect(reconnectBtn).toBeDisabled();

  // Additional clicks are prevented by the disabled state — try force-clicking
  // to verify no additional attempts are created
  await reconnectBtn.click({ force: true });
  await reconnectBtn.click({ force: true });

  // Resolve the slow fetch to complete the reconnect
  slowFetch.resolve?.();
  await expect.poll(() => socket.attaches).toBe(2);

  // Despite multiple clicks, only one additional WebSocket connection was made
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

  // Write terminal content to the buffer before navigation
  await page.evaluate(() => {
    const pane = document.querySelector('scion-terminal-pane');
    const container = pane?.shadowRoot?.querySelector('.terminal-container');
    if (container) {
      // Inject visible content into the terminal container to simulate buffer output
      const marker = document.createElement('div');
      marker.className = 'scrollback-marker';
      marker.textContent = 'SCROLLBACK_CONTENT_BEFORE_NAV';
      container.appendChild(marker);
    }
  });

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

  // No new socket connection — session was preserved
  expect(socket.attaches).toBe(initialAttaches);
  expect(socket.closes).toBe(0);

  // Buffer content should still be present after round-trip navigation
  const markerPresent = await page.evaluate(() => {
    const pane = document.querySelector('scion-terminal-pane');
    const marker = pane?.shadowRoot?.querySelector('.scrollback-marker');
    return marker?.textContent ?? null;
  });
  expect(markerPresent).toBe('SCROLLBACK_CONTENT_BEFORE_NAV');
});

test('unavailable agent shows correct state in rail and pane', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Deliver an SSE agent-stopped event through the mocked EventSource.
  // The production SSE→metadata→session bridge in the workspace root should
  // call markUnavailable automatically when the metadata reports phase=stopped.
  await page.evaluate((agentId) => {
    const instances = (window as unknown as { __sseInstances__: EventTarget[] }).__sseInstances__;
    for (const es of instances) {
      const event = new MessageEvent('update', {
        data: JSON.stringify({
          subject: `agent.${agentId}.status`,
          data: { phase: 'stopped', activity: 'offline' },
        }),
      });
      es.dispatchEvent(event);
    }
  }, agent);

  // Rail should show unavailable state, not disconnected
  await expect(page.locator('#terminal-workspace')).toContainText('Unavailable');

  // Overlay should show AGENT UNAVAILABLE title
  const overlay = page.locator('scion-terminal-pane .disconnected-overlay');
  await expect(overlay).toBeVisible();
  await expect(overlay.locator('.overlay-title')).toContainText('AGENT UNAVAILABLE');
});

test('toolbar reconnect button disabled during active reconnect attempt', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  socket.disconnectAll();
  const toolbarBtn = page.locator('scion-terminal-pane .reconnect-btn');
  await expect(toolbarBtn).toBeVisible();

  // Click toolbar reconnect
  await toolbarBtn.click();

  // The toolbar button should show "Reconnecting..." and be disabled
  await expect(toolbarBtn).toContainText('Reconnecting...');
  await expect(toolbarBtn).toBeDisabled();

  // Wait for reconnect to complete
  await expect.poll(() => socket.attaches).toBe(2);

  // Also verify: for terminal-permanent disconnect reasons (agent-deleted),
  // the reconnect button should be disabled.
  // Deliver SSE deleted event — the production bridge calls markUnavailable.
  await page.evaluate((agentId) => {
    const instances = (window as unknown as { __sseInstances__: EventTarget[] }).__sseInstances__;
    for (const es of instances) {
      es.dispatchEvent(
        new MessageEvent('update', {
          data: JSON.stringify({ subject: `agent.${agentId}.deleted`, data: {} }),
        })
      );
    }
  }, agent);

  // The reconnect button should now be visible but disabled for agent-deleted reason
  await expect(toolbarBtn).toBeVisible();
  await expect(toolbarBtn).toBeDisabled();
});

test('disconnected session transitions to unavailable when agent stops', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Step 1: Disconnect via network — session enters "disconnected" state
  socket.disconnectAll();
  const overlay = page.locator('scion-terminal-pane .disconnected-overlay');
  await expect(overlay).toBeVisible();
  await expect(overlay.locator('.overlay-title')).toContainText('DISCONNECTED');

  // Reconnect button should be enabled (disconnected, not unavailable)
  const reconnectBtn = page.locator('scion-terminal-pane .overlay-reconnect');
  await expect(reconnectBtn).toBeEnabled();

  // Step 2: Deliver SSE agent-stopped event while session is disconnected
  await page.evaluate((agentId) => {
    const instances = (window as unknown as { __sseInstances__: EventTarget[] }).__sseInstances__;
    for (const es of instances) {
      es.dispatchEvent(
        new MessageEvent('update', {
          data: JSON.stringify({
            subject: `agent.${agentId}.status`,
            data: { phase: 'stopped', activity: 'offline' },
          }),
        })
      );
    }
  }, agent);

  // Step 3: Session should transition from disconnected → unavailable
  await expect(overlay.locator('.overlay-title')).toContainText('AGENT UNAVAILABLE');
  // Overlay should have the 'unavailable' class applied (drives CSS styling)
  await expect(overlay).toHaveClass(/unavailable/);
});
