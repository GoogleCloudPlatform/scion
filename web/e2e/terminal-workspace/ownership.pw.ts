/**
 * P3.3 (#1660): Playwright browser tests for owner exit detection,
 * frozen-tab handling, and fresh ownership semantics.
 *
 * Note on headed/headless limitations:
 * - Tab freeze/suspend via Page Lifecycle API cannot be reliably triggered
 *   in headless Chrome. The Web Lock is held by the browser while the page's
 *   JavaScript is suspended, so freeze behavior can only be fully verified
 *   with headed Chrome and manual backgrounding.
 * - Owner tab CLOSE is testable: closing the page releases the Web Lock,
 *   and we verify the new tab acquires ownership.
 * - The "frozen owner holds lock" invariant is tested via the unit test
 *   suite (vitest), which uses a mock lock manager to simulate freeze.
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
      name: 'ownership-agent',
      phase: 'running',
      projectId: 'fixture-project',
    },
  }
): Promise<{
  readonly attaches: number;
  readonly closes: number;
  sent: string[];
}> {
  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];
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
    sent,
  };
}

// ---------------------------------------------------------------------------
// Owner tab close → new tab acquires ownership
// ---------------------------------------------------------------------------
test('owner tab close releases lock — new tab becomes owner on next open', async ({ context }) => {
  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setup(owner);
  const otherSocket = await setup(other);

  // Owner opens terminal
  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Non-owner tab sees the owning-tab message (cannot open locally)
  await other.goto(`/terminals/${agent}`);
  await expect(other.locator('#terminal-workspace')).toContainText('owning tab');
  expect(otherSocket.attaches).toBe(0);

  // Close the owner tab — releases the Web Lock
  await owner.close();

  // The non-owner tab must be able to open after the owner exits.
  // Navigate away and back to trigger a new open() call.
  await other.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(other).toHaveURL('/');

  // Navigate to a terminal — this tab should now acquire ownership
  await other.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );

  // The other tab should now be the owner and have a WebSocket connection
  await expect.poll(() => otherSocket.attaches).toBe(1);
  await expect(other.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
});

// ---------------------------------------------------------------------------
// Non-owner tab cannot steal lock (no duplicate attach)
// ---------------------------------------------------------------------------
test('non-owner tab does not create a local pane or attach when owner holds lock', async ({
  context,
}) => {
  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setup(owner);
  const otherSocket = await setup(other);

  // Owner opens terminal first
  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Non-owner tries to open
  await other.goto(`/terminals/${agent}`);

  // Non-owner should show the "owning tab" message
  await expect(other.locator('#terminal-workspace')).toContainText('owning tab');

  // Non-owner should NOT have any WebSocket connections
  expect(otherSocket.attaches).toBe(0);
  expect(await other.locator('#terminal-workspace scion-terminal-pane').count()).toBe(0);

  // Owner's session count should still be 1 (no duplicate)
  expect(ownerSocket.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Fresh ownership starts clean — no session restore (AC4/AC6)
// ---------------------------------------------------------------------------
test('fresh owner after previous exit starts with no sessions', async ({ context }) => {
  const owner = await context.newPage();
  const successor = await context.newPage();
  const ownerSocket = await setup(owner);
  const successorSocket = await setup(successor);

  // Owner opens terminal
  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Close owner
  await owner.close();

  // Successor opens — should get a fresh start
  await successor.goto(`/terminals/${agent}`);
  await expect.poll(() => successorSocket.attaches).toBe(1);

  // Successor should have exactly one pane (the one it just opened)
  await expect(successor.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);

  // The attach is a new connection, not a restored one
  expect(successorSocket.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Unsupported coordination shows limitation (AC5)
// ---------------------------------------------------------------------------
test('unsupported coordination capability shows clear limitation message', async ({ page }) => {
  // Set up the page with locks disabled to simulate unsupported environment
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    // Remove Web Lock API to simulate unsupported environment
    Object.defineProperty(navigator, 'locks', { value: undefined });
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
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/agents/**', (route) =>
    route.fulfill({
      json: { id: agent, name: 'ownership-agent', phase: 'running', projectId: 'proj' },
    })
  );
  let attaches = 0;
  await page.routeWebSocket('**/pty?*', () => {
    attaches++;
  });

  await page.goto(`/terminals/${agent}`);

  // Should show an unsupported/unavailable message, not a silent failure
  await expect(page.locator('#terminal-workspace')).toContainText('unavailable');

  // No WebSocket connections should have been made
  expect(attaches).toBe(0);

  // No terminal panes should exist
  expect(await page.locator('#terminal-workspace scion-terminal-pane').count()).toBe(0);
});

// ---------------------------------------------------------------------------
// Multi-page concurrent opens
// ---------------------------------------------------------------------------
test('multiple tabs competing for ownership — only one succeeds, no split', async ({ context }) => {
  const page1 = await context.newPage();
  const page2 = await context.newPage();
  const socket1 = await setup(page1);
  const socket2 = await setup(page2);

  // Both navigate to the same terminal URL concurrently
  await Promise.all([page1.goto(`/terminals/${agent}`), page2.goto(`/terminals/${agent}`)]);

  // Wait for at least one to have a WebSocket attach
  await expect.poll(() => socket1.attaches + socket2.attaches).toBeGreaterThanOrEqual(1);

  // Exactly one tab should be the owner with an attach
  // The other should show "owning tab" message
  const page1HasPanes =
    (await page1.locator('#terminal-workspace scion-terminal-pane').count()) > 0;
  const page2HasPanes =
    (await page2.locator('#terminal-workspace scion-terminal-pane').count()) > 0;

  // At least one tab must have a pane
  expect(page1HasPanes || page2HasPanes).toBe(true);

  // Total attaches should be exactly 1 (no duplicate)
  expect(socket1.attaches + socket2.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Headed-only observation notes (cannot be tested headlessly)
// ---------------------------------------------------------------------------
/**
 * HEADED CHROME OBSERVATIONS (manual verification):
 *
 * 1. Tab freeze/suspend: When the owner tab is backgrounded or frozen via
 *    Page Lifecycle API, its Web Lock is retained by the browser. The non-owner
 *    tab cannot acquire the lock. The frozen tab's BroadcastChannel messages
 *    queue and are delivered when the tab resumes. Verified with:
 *    - chrome://discards → Freeze tab
 *    - Background tab for >5 minutes (Chrome may freeze it)
 *
 * 2. Tab crash: When the owner tab crashes (chrome://crash), the Web Lock
 *    is released immediately by the browser. The queued lock request in the
 *    non-owner tab fires, and the next explicit open succeeds.
 *
 * 3. Navigation away: When the owner tab navigates to a different origin,
 *    pagehide fires → stop() → lock released → non-owner acquires.
 *
 * These observations cannot be automated in headless CI because:
 * - Page Lifecycle events (freeze/resume) are not exposed via CDP
 * - chrome://crash is blocked in headless mode
 * - Tab backgrounding has no effect in headless mode
 */
