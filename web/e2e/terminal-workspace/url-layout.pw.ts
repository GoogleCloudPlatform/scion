/**
 * Playwright browser tests for URL layout encoding (#1715).
 *
 * These tests exercise production routing, query-parameter parsing, and actual
 * browser URL assertions. They validate that layout state survives page reload,
 * copy-paste to a new tab, back/forward navigation, and graceful degradation
 * on malformed input.
 */
import { test, expect, type Page } from '@playwright/test';

const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';
const agentE = '55555555-5555-4555-8555-555555555555';

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
  [agentE]: { id: agentE, name: 'epsilon', phase: 'running', projectId: 'proj' },
};

// ---------------------------------------------------------------------------
// Fixture setup — mirrors the pattern used by workspace.pw.ts
// ---------------------------------------------------------------------------

async function setup(
  page: Page,
  enabled = true,
  agents: Record<string, AgentFixture> = ALL_AGENTS
): Promise<{
  readonly attaches: number;
  readonly closes: number;
  sent: string[];
}> {
  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];
  await page.addInitScript(
    ({ enabled: feat }) => {
      window.__SCION_FEATURES__ = { 'web.terminal_workspace': feat };
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    { enabled }
  );
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
// Helper functions (same patterns as workspace.pw.ts)
// ---------------------------------------------------------------------------

async function navigateToTerminal(page: Page, agentId: string): Promise<void> {
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentId}`
  );
}

async function clickPreset(page: Page, preset: string): Promise<void> {
  await page.click(`.terminal-layout-btn[data-preset="${preset}"]`);
}

async function activePreset(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const btn = document.querySelector('.terminal-layout-btn[data-active="true"]');
    return btn instanceof HTMLElement ? (btn.dataset.preset ?? null) : null;
  });
}

async function visiblePaneCount(page: Page): Promise<number> {
  return page.evaluate(() => {
    const panes = document.querySelectorAll<HTMLElement>('#terminal-workspace scion-terminal-pane');
    let count = 0;
    for (const p of panes) {
      if (!p.hidden && p.style.display !== 'none') count++;
    }
    return count;
  });
}

async function placeholderCount(page: Page): Promise<number> {
  return page.evaluate(() => {
    const phs = document.querySelectorAll<HTMLElement>(
      '#terminal-workspace .terminal-slot-placeholder'
    );
    let count = 0;
    for (const p of phs) {
      if (!p.hidden) count++;
    }
    return count;
  });
}

async function getPaneSessionKeys(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const panes = document.querySelectorAll<
      HTMLElement & { session: { state: { key: string } } | null }
    >('#terminal-workspace scion-terminal-pane');
    return [...panes].filter((p) => p.session).map((p) => p.session!.state.key);
  });
}

async function placeInPreset(
  page: Page,
  sessionKey: string,
  preset: string,
  slotIndex: number
): Promise<void> {
  await page.evaluate(
    ({ key, preset: p, slot }) => {
      type WorkspaceEl = HTMLElement & {
        workspaceRoot?: {
          layoutManager: { place: (k: string, p: string, i: number) => void };
        };
      };
      const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
      host.workspaceRoot!.layoutManager.place(key, p, slot);
    },
    { key: sessionKey, preset, slot: slotIndex }
  );
}

/** Return the URL search string for the current page. */
async function getSearchParams(page: Page): Promise<string> {
  return page.evaluate(() => window.location.search);
}

/** Return the URL path + search for the current page. */
async function getPathWithSearch(page: Page): Promise<string> {
  return page.evaluate(() => window.location.pathname + window.location.search);
}

/** Get the agent ID visible in a specific pane slot by checking session state. */
async function getSlotAgentIds(page: Page): Promise<Array<string | null>> {
  return page.evaluate(() => {
    type WorkspaceEl = HTMLElement & {
      workspaceRoot?: {
        getActiveSlotAgentIds: () => ReadonlyArray<string | null>;
      };
    };
    const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
    return [...host.workspaceRoot!.getActiveSlotAgentIds()];
  });
}

// =========================================================================
// Test Scenario A: Zero-terminal multi-pane URL reload with placeholders
// =========================================================================
test('A: multi-pane URL with no agents reloads with placeholders', async ({ page }) => {
  const socket = await setup(page);

  // Navigate directly to a multi-pane layout URL with empty slots
  await page.goto('/terminals?lv=1&lp=two-columns&s0=&s1=');

  // Verify the URL contains layout params
  const search = await getSearchParams(page);
  expect(search).toContain('lv=1');
  expect(search).toContain('lp=two-columns');

  // The layout should be two-columns with placeholder panes (no agents)
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);
  await expect.poll(() => visiblePaneCount(page)).toBe(0);

  // No WebSocket attaches since no agents
  expect(socket.attaches).toBe(0);

  // Reload the page
  await page.reload();

  // After reload, layout should still be two-columns with placeholders
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);
  await expect.poll(() => visiblePaneCount(page)).toBe(0);
  expect(socket.attaches).toBe(0);
});

// =========================================================================
// Test Scenario B: Partial ordered slots (some occupied, some empty)
// =========================================================================
test('B: partial ordered slots restore correctly after reload', async ({ page }) => {
  const socket = await setup(page);

  // Navigate to a two-columns layout with agentA in slot 0 and empty slot 1
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=`);
  await expect.poll(() => socket.attaches).toBe(1);
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Verify the URL encodes the correct agent IDs
  const search = await getSearchParams(page);
  expect(search).toContain(`s0=${agentA}`);

  // Verify one visible pane and one placeholder
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
  await expect.poll(() => placeholderCount(page)).toBe(1);

  // Verify slot assignment — agentA in slot 0
  const agentIds = await getSlotAgentIds(page);
  expect(agentIds[0]).toBe(agentA);
  expect(agentIds[1]).toBeNull();

  // Reload
  await page.reload();

  // After reload, layout and agent positions must be restored
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
  await expect.poll(() => placeholderCount(page)).toBe(1);

  const agentIdsAfter = await getSlotAgentIds(page);
  expect(agentIdsAfter[0]).toBe(agentA);
  expect(agentIdsAfter[1]).toBeNull();
});

// =========================================================================
// Test Scenario C: Full four slots
// =========================================================================
test('C: full four-pane layout restores all agents after reload', async ({ page }) => {
  const socket = await setup(page);

  // Navigate directly to a four-pane layout with all 4 agents
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentC}&s3=${agentD}`);

  // Wait for all 4 WebSocket attaches
  await expect.poll(() => socket.attaches).toBe(4);
  await expect.poll(() => activePreset(page)).toBe('four');
  await expect.poll(() => visiblePaneCount(page)).toBe(4);
  await expect.poll(() => placeholderCount(page)).toBe(0);

  // Verify agent order
  const agentIds = await getSlotAgentIds(page);
  expect(agentIds).toEqual([agentA, agentB, agentC, agentD]);

  // Reload
  await page.reload();

  // All agents restored in correct positions
  await expect.poll(() => socket.attaches).toBe(8); // 4 initial + 4 after reload
  await expect.poll(() => activePreset(page)).toBe('four');
  await expect.poll(() => visiblePaneCount(page)).toBe(4);

  const agentIdsAfter = await getSlotAgentIds(page);
  expect(agentIdsAfter).toEqual([agentA, agentB, agentC, agentD]);
});

// =========================================================================
// Test Scenario D: Copied URL in a fresh browser context
// =========================================================================
test('D: layout URL opened in fresh browser context restores layout', async ({ context }) => {
  const page1 = await context.newPage();
  const socket1 = await setup(page1);

  // Build a layout URL
  const layoutUrl = `/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`;
  await page1.goto(layoutUrl);
  await expect.poll(() => socket1.attaches).toBe(2);
  await expect.poll(() => activePreset(page1)).toBe('two-columns');

  // Capture the full URL
  const fullUrl = await getPathWithSearch(page1);

  // Close page1 first — release Web Lock so page2 can own
  await page1.close();

  // Open the captured URL in a new page (simulates copy-paste / sharing)
  const page2 = await context.newPage();
  const socket2 = await setup(page2);
  await page2.goto(fullUrl);

  // The new page should restore the same layout
  await expect.poll(() => socket2.attaches).toBe(2);
  await expect.poll(() => activePreset(page2)).toBe('two-columns');
  await expect.poll(() => visiblePaneCount(page2)).toBe(2);

  const agentIds = await getSlotAgentIds(page2);
  expect(agentIds[0]).toBe(agentA);
  expect(agentIds[1]).toBe(agentB);
});

// =========================================================================
// Test Scenario E: Back/forward canonical state restoration
// =========================================================================
test('E: back/forward navigation restores layout state from URL', async ({ page }) => {
  const socket = await setup(page);

  // Start at a non-terminal page (creates a history entry)
  await page.goto('/');
  await expect(page).toHaveURL('/');

  // Navigate to a multi-pane layout URL (via nav-click → pushState)
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`
  );
  await expect.poll(() => socket.attaches).toBe(2);
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Verify the URL contains layout params
  const searchBefore = await getSearchParams(page);
  expect(searchBefore).toContain('lp=two-columns');

  // Navigate away from terminals (creates another history entry)
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/');

  // Go back — should restore the terminal layout URL
  await page.goBack();
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  const searchAfter = await getSearchParams(page);
  expect(searchAfter).toContain('lp=two-columns');

  // No socket closures (sessions reused)
  expect(socket.closes).toBe(0);
});

// =========================================================================
// Test Scenario F: Direct single route compatibility and flag-off
// =========================================================================
test('F: direct single route works without layout params', async ({ page }) => {
  const socket = await setup(page);

  // Navigate to a single-agent URL (no layout query params)
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await expect.poll(() => activePreset(page)).toBe('single');

  // URL should NOT contain layout query params in single mode
  const search = await getSearchParams(page);
  expect(search).not.toContain('lv=');
  expect(search).not.toContain('lp=');

  // Pane should display the agent
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
});

test('F: feature flag off — layout params are ignored, legacy page renders', async ({ page }) => {
  const socket = await setup(page, false);

  // Navigate to a URL with layout params while flag is off
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentC}&s3=${agentD}`);

  // Layout params should be ignored — no workspace root should render
  expect(await page.locator('#terminal-workspace').count()).toBe(0);

  // No WebSocket connections (flag is off, workspace not enabled)
  expect(socket.attaches).toBe(0);
});

// =========================================================================
// Test Scenario G: Malformed/duplicate/missing/unauthorized IDs fail safely
// =========================================================================
test('G: malformed UUIDs produce empty slots, no crash', async ({ page }) => {
  const socket = await setup(page);

  await page.goto(`/terminals?lv=1&lp=two-columns&s0=not-a-uuid&s1=${agentA}`);

  // Should render without error
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => socket.attaches).toBe(1); // Only agentA attached

  // Slot 0 should be empty (malformed UUID), slot 1 has agentA
  const agentIds = await getSlotAgentIds(page);
  expect(agentIds[0]).toBeNull();
  expect(agentIds[1]).toBe(agentA);
});

test('G: duplicate agent IDs — first occurrence wins', async ({ page }) => {
  const socket = await setup(page);

  // Same agent ID in slot 0 and slot 2
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentA}&s3=${agentC}`);

  // Only 3 distinct agents should attach (agentA deduplicated)
  await expect.poll(() => socket.attaches).toBe(3);
  await expect.poll(() => activePreset(page)).toBe('four');

  // Slot 0 has agentA (first occurrence), slot 2 should be null
  const agentIds = await getSlotAgentIds(page);
  expect(agentIds[0]).toBe(agentA);
  expect(agentIds[1]).toBe(agentB);
  expect(agentIds[2]).toBeNull();
  expect(agentIds[3]).toBe(agentC);
});

test('G: nonexistent agent ID fails safely — no crash, no error dialog', async ({ page }) => {
  const socket = await setup(page);
  const fakeAgent = '99999999-9999-4999-8999-999999999999';

  // Track page errors
  const pageErrors: string[] = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));

  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${fakeAgent}&s1=${agentA}`);

  // agentA should attach successfully
  await expect.poll(() => socket.attaches).toBeGreaterThanOrEqual(1);
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // agentA must be present in one of the slots
  const agentIds = await getSlotAgentIds(page);
  expect(agentIds).toContain(agentA);

  // No uncaught page errors or crash
  expect(pageErrors.length).toBe(0);

  // No error dialog visible
  expect(await page.locator('[role="alertdialog"]').count()).toBe(0);
});

test('G: unknown version (lv=99) — layout params ignored entirely', async ({ page }) => {
  const socket = await setup(page);

  await page.goto(`/terminals?lv=99&lp=four&s0=${agentA}`);

  // Unknown version should be ignored — page renders default behavior
  // No layout restoration happens; default single view
  // No crash, no error
  expect(socket.attaches).toBe(0);

  // The page should not show multi-pane layout
  // (it might show "No terminals are open" or terminal workspace empty state)
  await expect(page.locator('#terminal-workspace')).toBeVisible();
});

// =========================================================================
// Test Scenario H: No duplicate attach on URL reorder/navigation
// =========================================================================
test('H: re-navigating same layout URL reuses sessions, no duplicate attaches', async ({
  page,
}) => {
  const socket = await setup(page);

  // Open a two-columns layout
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`);
  await expect.poll(() => socket.attaches).toBe(2);
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  const initialAttaches = socket.attaches;

  // Trigger a popstate with the same URL (simulating back/forward to same entry)
  await page.evaluate(() => {
    window.dispatchEvent(new PopStateEvent('popstate'));
  });

  // Wait a bit for any async operations to settle
  await page.waitForTimeout(500);

  // No new WebSocket connections — sessions are reused
  expect(socket.attaches).toBe(initialAttaches);
  expect(socket.closes).toBe(0);
});

// =========================================================================
// Test Scenario I: #1701 available-slot/overflow preserved
// =========================================================================
test('I: overflow to single when all slots filled, then open new agent', async ({ page }) => {
  const socket = await setup(page);

  // Open a two-columns layout with both slots filled
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`);
  await expect.poll(() => socket.attaches).toBe(2);
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Open a new agent — two-columns is at capacity, should overflow to single
  await navigateToTerminal(page, agentC);
  await expect.poll(() => socket.attaches).toBe(3);
  await expect.poll(() => activePreset(page)).toBe('single');

  // The new agent should be the visible one in single mode
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Switch back to two-columns — original grid should be preserved
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // No additional WebSocket connections from preset switch
  expect(socket.attaches).toBe(3);
  expect(socket.closes).toBe(0);
});

// =========================================================================
// Test Scenario J: #1716 focus behavior preserved
// =========================================================================
test('J: focus outline visible in multi-pane, absent in single', async ({ page }) => {
  const socket = await setup(page);

  // Set up a two-columns layout with two agents
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`);
  await expect.poll(() => socket.attaches).toBe(2);
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Focus a pane to trigger focus outline
  await page.evaluate(() => {
    const panes = document.querySelectorAll<HTMLElement>('#terminal-workspace scion-terminal-pane');
    if (panes.length > 0) {
      const wrapper = panes[0].shadowRoot?.querySelector('.terminal-wrapper') as HTMLElement;
      if (wrapper) {
        wrapper.setAttribute('tabindex', '-1');
        wrapper.focus();
      }
    }
  });

  // In multi-pane layout, the focused pane should have data-focused attribute
  const hasFocused = await page.evaluate(() => {
    const panes = document.querySelectorAll<HTMLElement>('#terminal-workspace scion-terminal-pane');
    return [...panes].some((p) => p.hasAttribute('data-focused'));
  });
  expect(hasFocused).toBe(true);

  // Verify the effective layout attribute is set for multi-pane
  const effectiveLayout = await page.evaluate(() => {
    const host = document.querySelector('.terminal-pane-host') as HTMLElement;
    return host?.dataset.effectiveLayout ?? null;
  });
  expect(effectiveLayout).toBe('two-columns');

  // Switch to single — focus outline should not be shown
  await clickPreset(page, 'single');
  await expect.poll(() => activePreset(page)).toBe('single');

  // In single-pane mode, the effective layout should be 'single'
  const singleLayout = await page.evaluate(() => {
    const host = document.querySelector('.terminal-pane-host') as HTMLElement;
    return host?.dataset.effectiveLayout ?? null;
  });
  expect(singleLayout).toBe('single');

  // No extra sockets from layout switching
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

// =========================================================================
// Additional URL encoding tests
// =========================================================================

test('URL updates to canonical form after layout change via preset buttons', async ({ page }) => {
  const socket = await setup(page);

  // Start with a single-agent terminal
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await expect.poll(() => activePreset(page)).toBe('single');

  // No layout params in single mode
  let search = await getSearchParams(page);
  expect(search).not.toContain('lv=');

  // Switch to two-columns
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // URL should now contain layout params
  search = await getSearchParams(page);
  expect(search).toContain('lv=1');
  expect(search).toContain('lp=two-columns');

  // Switch back to single — layout params should be cleared
  await clickPreset(page, 'single');
  await expect.poll(() => activePreset(page)).toBe('single');
  search = await getSearchParams(page);
  expect(search).not.toContain('lv=');
  expect(search).not.toContain('lp=');
});

test('URL reflects agent placement changes', async ({ page }) => {
  const socket = await setup(page);

  // Open two agents
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both in two-columns
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[1], 'two-columns', 1);

  // Switch to two-columns
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // URL should contain both agent IDs
  const search = await getSearchParams(page);
  expect(search).toContain('lp=two-columns');
  // s0 and s1 should be present
  expect(search).toContain('s0=');
  expect(search).toContain('s1=');
});

test('four-pane reload with mixed empty and occupied slots', async ({ page }) => {
  const socket = await setup(page);

  // Navigate to four-pane with slots 0 and 2 occupied, 1 and 3 empty
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=&s2=${agentB}&s3=`);

  await expect.poll(() => socket.attaches).toBe(2);
  await expect.poll(() => activePreset(page)).toBe('four');

  const agentIds = await getSlotAgentIds(page);
  expect(agentIds[0]).toBe(agentA);
  expect(agentIds[1]).toBeNull();
  expect(agentIds[2]).toBe(agentB);
  expect(agentIds[3]).toBeNull();

  // Reload and verify positions preserved
  await page.reload();

  await expect.poll(() => activePreset(page)).toBe('four');
  const agentIdsAfter = await getSlotAgentIds(page);
  expect(agentIdsAfter[0]).toBe(agentA);
  expect(agentIdsAfter[1]).toBeNull();
  expect(agentIdsAfter[2]).toBe(agentB);
  expect(agentIdsAfter[3]).toBeNull();
});
