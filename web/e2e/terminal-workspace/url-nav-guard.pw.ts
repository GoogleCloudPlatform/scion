/**
 * Playwright browser tests for navigation guard in URL restoration loop (#1816).
 *
 * Validates that the async multi-agent URL restoration loop correctly aborts
 * when the user navigates away mid-restoration. The `thisNav !== navigationId`
 * guard prevents later-slot agents from being opened after the navigation
 * context has changed.
 */
import { test, expect, type Page } from '@playwright/test';

const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
}

const ALL_AGENTS: Record<string, AgentFixture> = {
  [agentA]: { id: agentA, name: 'alpha', phase: 'running', projectId: 'proj' },
  [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  [agentC]: { id: agentC, name: 'gamma', phase: 'running', projectId: 'proj' },
  [agentD]: { id: agentD, name: 'delta', phase: 'running', projectId: 'proj' },
};

// ---------------------------------------------------------------------------
// Fixture setup — mirrors the pattern from url-layout.pw.ts
// ---------------------------------------------------------------------------

async function setup(
  page: Page,
  agents: Record<string, AgentFixture> = ALL_AGENTS
): Promise<{
  readonly attaches: number;
  readonly closes: number;
  attachedAgentIds: string[];
}> {
  let attaches = 0;
  let closes = 0;
  const attachedAgentIds: string[] = [];
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
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => ({
        ...a,
        _capabilities: { actions: ['attach'] },
      })),
    });
  });
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id =
      route
        .request()
        .url()
        .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1] ?? agentA;
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
    // Extract agent ID from the WebSocket URL query params
    const url = new URL(socket.url());
    const aid = url.searchParams.get('agentId') ?? 'unknown';
    attachedAgentIds.push(aid);
    socket.onClose(() => closes++);
  });
  return {
    get attaches(): number {
      return attaches;
    },
    get closes(): number {
      return closes;
    },
    attachedAgentIds,
  };
}

// =========================================================================
// Test: Navigation away during multi-agent URL restoration aborts later slots
// =========================================================================
test('navigation away during URL restoration aborts later agent opens', async ({ page }) => {
  const socket = await setup(page);

  // First, navigate to / so the SPA router is initialized
  await page.goto('/');
  await expect(page).toHaveURL('/');

  // Record initial attaches
  const initialAttaches = socket.attaches;

  // Dispatch two nav-click events back-to-back in a single evaluate call.
  // The first navigates to a 4-pane URL restoration, which starts the async
  // loop. When the loop hits `await coordinator.open(...)`, it yields.
  // The second nav-click fires synchronously and increments `navigationId`.
  // When the first loop resumes from its await, `thisNav !== navigationId`
  // is true, so the loop returns early — later-slot agents are never opened.
  const terminalUrl = `/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentC}&s3=${agentD}`;
  await page.evaluate((url) => {
    // Navigate to multi-pane terminal URL (starts async restoration loop)
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: url } }));
    // Immediately navigate away — this increments navigationId, which the
    // guard checks in the loop will detect after the awaited open resolves.
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }));
  }, terminalUrl);

  // Wait for async operations to settle
  await page.waitForTimeout(1500);

  // The destination route (/) should be authoritative
  await expect(page).toHaveURL('/');

  // The terminal workspace should not be visible (we navigated away)
  const terminalWorkspaceVisible = await page.evaluate(() => {
    const tw = document.querySelector('#terminal-workspace') as HTMLElement | null;
    return tw ? !tw.hidden : false;
  });
  expect(terminalWorkspaceVisible).toBe(false);

  // The key assertion: NOT all 4 agents should have been opened. The guard
  // should have aborted the loop before reaching all slots. Because the
  // loop yields at `await coordinator.open(...)` and the navigation change
  // happens synchronously between the two nav-clicks, the first open that
  // resumes should see the guard and return.
  const newAttaches = socket.attaches - initialAttaches;
  expect(newAttaches).toBeLessThan(4);
});

// =========================================================================
// Test: Normal URL restoration completes when no navigation interruption
// =========================================================================
test('URL restoration completes all agents without navigation interruption', async ({ page }) => {
  const socket = await setup(page);

  // Navigate to a 4-pane layout URL — all agents should open
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentC}&s3=${agentD}`);
  await expect.poll(() => socket.attaches, { timeout: 10000 }).toBe(4);

  // All 4 agents attached — no guard triggered
  expect(socket.attaches).toBe(4);
});

// =========================================================================
// Test: Navigate away after URL restoration — previously opened sessions ok
// =========================================================================
test('navigate away after URL restoration preserves already-opened sessions', async ({ page }) => {
  const socket = await setup(page);

  // Navigate to a two-column layout with two agents
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`);
  await expect.poll(() => socket.attaches, { timeout: 10000 }).toBe(2);

  // Navigate away
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/');

  // The terminal workspace should not be visible
  const terminalWorkspaceVisible = await page.evaluate(() => {
    const tw = document.querySelector('#terminal-workspace') as HTMLElement | null;
    return tw ? !tw.hidden : false;
  });
  expect(terminalWorkspaceVisible).toBe(false);

  // Sessions were already opened — no additional attaches, no closes
  // (sessions remain valid for reuse)
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});
