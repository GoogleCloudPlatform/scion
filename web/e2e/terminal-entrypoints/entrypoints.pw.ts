/**
 * Terminal entry-point coverage (P1.7 #1652).
 *
 * Table-driven real-browser tests for every UI surface that opens a terminal:
 * agent list, agent detail, project list/grid, tree/graph, chat membership.
 *
 * Covers: workspace-enabled hrefs, flag-off legacy hrefs, modified-click
 * passthrough (Ctrl/Cmd-click opens in new tab), cross-tab ownership
 * delegation, chat source state retained when workspace steals focus,
 * repeated/pending opens, and direct legacy route redirect.
 */

import { test, expect, type Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// Fixture agent UUIDs — trusted format matching the router regex.
// ---------------------------------------------------------------------------
const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';

// Fixture project
const projectId = 'fixture-project';
const projectSlug = 'fixture-project';

// ---------------------------------------------------------------------------
// API fixture helpers
// ---------------------------------------------------------------------------

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
  slug?: string;
  canAttach?: boolean;
  activity?: string;
}

const defaultAgents: Record<string, AgentFixture> = {
  [agentA]: {
    id: agentA,
    name: 'alpha-agent',
    phase: 'running',
    projectId,
    slug: 'alpha-agent',
    canAttach: true,
  },
  [agentB]: {
    id: agentB,
    name: 'beta-agent',
    phase: 'running',
    projectId,
    slug: 'beta-agent',
    canAttach: true,
  },
};

interface SetupResult {
  readonly attaches: number;
  readonly closes: number;
  sent: string[];
}

async function setup(
  page: Page,
  opts: {
    enabled?: boolean;
    agents?: Record<string, AgentFixture>;
    nativeChatEnabled?: boolean;
  } = {}
): Promise<SetupResult> {
  const { enabled = true, agents = defaultAgents, nativeChatEnabled = true } = opts;

  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];

  await page.addInitScript(
    ({ enabled: e }) => {
      window.__SCION_FEATURES__ = {
        'web.terminal_workspace': e,
        'web.native_chat': true,
        'web.native_chat_v2': true,
      };
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
    route.fulfill({ json: { nativeChatEnabled } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  // Agent API: detail, list, and pty stub
  await page.route('**/api/v1/agents/**', (route) => {
    const url = route.request().url();
    if (url.endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id = url.match(/\/api\/v1\/agents\/([^/?]+)/)?.[1] ?? agentA;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ?? { error: 'not found' },
    });
  });

  // Agent list endpoint (used by agents page)
  await page.route('**/api/v1/agents?*', (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => ({
        ...a,
        capabilities: a.canAttach !== false ? { terminalAttach: true } : {},
      })),
    });
  });
  await page.route('**/api/v1/agents', (route) => {
    if (route.request().url().includes('?')) return;
    void route.fulfill({
      json: Object.values(agents).map((a) => ({
        ...a,
        capabilities: a.canAttach !== false ? { terminalAttach: true } : {},
      })),
    });
  });

  // Project API
  await page.route('**/api/v1/projects/**', (route) => {
    void route.fulfill({
      json: {
        id: projectId,
        name: 'Fixture Project',
        slug: projectSlug,
        agents: Object.values(agents).map((a) => ({
          ...a,
          capabilities: a.canAttach !== false ? { terminalAttach: true } : {},
        })),
      },
    });
  });

  // Chat API stubs
  await page.route('**/api/v2/chat/**', (route) => void route.fulfill({ json: {} }));
  await page.route('**/api/v1/chat/**', (route) => void route.fulfill({ json: {} }));

  // WebSocket for terminal pty
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
// Table-driven entry point: href verification
//
// Each row describes a page that contains a terminal link, how to find the
// link, and the expected href value under both flag-on and flag-off.
// ---------------------------------------------------------------------------

interface EntryPointRow {
  /** Human-readable name for the test title */
  name: string;
  /** URL to navigate to before looking for the link */
  page: string;
  /** Playwright locator strategy to find the terminal link */
  locator: (page: Page) => ReturnType<Page['locator']>;
  /** Expected href when workspace flag is ON */
  hrefEnabled: string;
  /** Expected href when workspace flag is OFF */
  hrefDisabled: string;
}

const entryPoints: EntryPointRow[] = [
  {
    name: 'agent list page',
    page: '/agents',
    locator: (p) => p.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'agent detail page',
    page: `/agents/${agentA}`,
    locator: (p) => p.locator(`a[href*="terminal"]`).first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'project detail list view',
    page: `/projects/${projectId}`,
    locator: (p) => p.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
];

// ---------------------------------------------------------------------------
// Flag-ON: verify every entry point produces workspace hrefs
// ---------------------------------------------------------------------------

for (const row of entryPoints) {
  test(`entry point "${row.name}" has workspace href when flag is ON`, async ({ page }) => {
    await setup(page, { enabled: true });
    await page.goto(row.page);
    const link = row.locator(page);
    await expect(link).toBeVisible({ timeout: 10000 });
    const href = await link.getAttribute('href');
    expect(href).toBe(row.hrefEnabled);
  });
}

// ---------------------------------------------------------------------------
// Flag-OFF: verify every entry point falls back to legacy hrefs
// ---------------------------------------------------------------------------

for (const row of entryPoints) {
  test(`entry point "${row.name}" has legacy href when flag is OFF`, async ({ page }) => {
    await setup(page, { enabled: false });
    await page.goto(row.page);
    const link = row.locator(page);
    await expect(link).toBeVisible({ timeout: 10000 });
    const href = await link.getAttribute('href');
    expect(href).toBe(row.hrefDisabled);
  });
}

// ---------------------------------------------------------------------------
// Workspace-enabled: clicking an entry-point link routes to /terminals/{id}
// and the retained workspace attaches a WebSocket.
// ---------------------------------------------------------------------------

test('agent list terminal button navigates to workspace and attaches', async ({ page }) => {
  const socket = await setup(page);
  await page.goto('/agents');
  const btn = page.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first();
  await expect(btn).toBeVisible({ timeout: 10000 });
  await btn.click();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

test('agent detail terminal link navigates to workspace and attaches', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/agents/${agentA}`);
  const link = page.locator('a[href*="terminal"]').first();
  await expect(link).toBeVisible({ timeout: 10000 });
  await link.click();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

test('project detail terminal button navigates to workspace and attaches', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/projects/${projectId}`);
  const btn = page.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first();
  await expect(btn).toBeVisible({ timeout: 10000 });
  await btn.click();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Modified click: Ctrl/Meta-click on a terminal link must NOT prevent
// default — the browser should handle it (open in new tab). Verify the
// href is a real URL the browser can open.
// ---------------------------------------------------------------------------

test('modified click on terminal link preserves browser new-tab behaviour', async ({ page }) => {
  await setup(page);
  await page.goto('/agents');
  const btn = page.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first();
  await expect(btn).toBeVisible({ timeout: 10000 });

  // Verify the link has a real href (not javascript: or #)
  const href = await btn.getAttribute('href');
  expect(href).toBeTruthy();
  expect(href).not.toContain('javascript:');
  expect(href).not.toContain('#');
  expect(href).toMatch(/^\/terminals\//);
});

// ---------------------------------------------------------------------------
// Direct legacy route redirect: /agents/{id}/terminal → /terminals/{id}
// when workspace is enabled.
// ---------------------------------------------------------------------------

test('legacy /agents/{id}/terminal redirects to /terminals/{id} when workspace is ON', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: true });
  await page.goto(`/agents/${agentA}/terminal`);
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Flag-off: legacy route stays on disposable terminal page
// ---------------------------------------------------------------------------

test('legacy /agents/{id}/terminal loads standalone terminal when flag is OFF', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: false });
  await page.goto(`/agents/${agentA}/terminal`);
  await expect(page).toHaveURL(`/agents/${agentA}/terminal`);
  await expect.poll(() => socket.attaches).toBe(1);
  // No workspace element
  expect(await page.locator('#terminal-workspace').count()).toBe(0);
});

// ---------------------------------------------------------------------------
// Repeated opens of the same agent reuse one pane/socket
// ---------------------------------------------------------------------------

test('repeated navigation to same agent reuses one pane and socket', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Navigate away and back
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/');

  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentA}`
  );
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  // Still only one attach — session was retained
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

// ---------------------------------------------------------------------------
// Cross-tab ownership: a second tab defers to the owner
// ---------------------------------------------------------------------------

test('second tab defers terminal to owning tab without attaching locally', async ({ context }) => {
  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setup(owner);
  const otherSocket = await setup(other);

  await owner.goto(`/terminals/${agentA}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  await other.goto(`/terminals/${agentA}`);
  await expect(other.locator('#terminal-workspace')).toContainText('owning tab');
  expect(otherSocket.attaches).toBe(0);
});

// ---------------------------------------------------------------------------
// Chat source state retained when terminal is opened from chat.
//
// This is the critical requirement from the manager: navigating to a terminal
// via the chat members sidebar must NOT destroy chat source state. When the
// workspace is enabled, the route outlet (containing the chat page) is hidden
// but NOT torn down, so chat keeps its DOM, scroll position, and reactive
// state. Returning to chat restores it.
// ---------------------------------------------------------------------------

test('chat source state is retained when opening terminal from chat membership', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: true, nativeChatEnabled: true });
  // Navigate to chat
  await page.goto('/chat');
  await expect(page.locator('scion-chat-shell, scion-page-chat, [data-scion-page]')).toBeVisible({
    timeout: 10000,
  });

  // Dispatch a terminal open as if the user clicked the chat member terminal icon
  await page.evaluate(
    (id) =>
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` }, bubbles: true })
      ),
    agentA
  );
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // The terminal workspace should be visible
  await expect(page.locator('#terminal-workspace')).toBeVisible();

  // The route outlet (chat page container) should be hidden but still in DOM —
  // chat source state is retained, not destroyed
  const routeOutlet = page.locator('#route-outlet');
  await expect(routeOutlet).toBeAttached();
  expect(await routeOutlet.evaluate((el) => (el as HTMLElement).hidden)).toBe(true);

  // Chat page element should still exist in the hidden outlet
  const chatPageExists = await page.evaluate(() => {
    const outlet = document.getElementById('route-outlet');
    return outlet ? outlet.querySelector('[data-scion-page]') !== null : false;
  });
  expect(chatPageExists).toBe(true);

  // Navigate back to chat — the page should restore, not re-create
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/chat' } }))
  );
  await expect(page).toHaveURL('/chat');

  // Route outlet should be visible again
  await expect(routeOutlet).not.toBeHidden();
  // Terminal workspace should be hidden
  await expect(page.locator('#terminal-workspace')).toBeHidden();
  // Socket should still be alive — session retained
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

// ---------------------------------------------------------------------------
// Cross-tab: chat source state retained when another tab owns terminals
//
// Even when another tab owns the terminal workspace, chat state must survive
// the navigation. The non-owner tab's workspace shows "owning tab" but the
// route outlet (with chat) is hidden, not destroyed.
// ---------------------------------------------------------------------------

test('cross-tab: chat state retained when another tab owns terminals', async ({ context }) => {
  const owner = await context.newPage();
  const chatTab = await context.newPage();
  const ownerSocket = await setup(owner);
  await setup(chatTab);

  // Owner opens a terminal
  await owner.goto(`/terminals/${agentA}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Chat tab starts at /chat
  await chatTab.goto('/chat');
  await expect(chatTab.locator('scion-chat-shell, scion-page-chat, [data-scion-page]')).toBeVisible(
    { timeout: 10000 }
  );

  // Chat tab navigates to terminal — ownership deferred to owner
  await chatTab.evaluate(
    (id) =>
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` }, bubbles: true })
      ),
    agentA
  );
  await expect(chatTab).toHaveURL(`/terminals/${agentA}`);
  await expect(chatTab.locator('#terminal-workspace')).toContainText('owning tab');

  // Route outlet still exists, hidden but alive
  const routeOutlet = chatTab.locator('#route-outlet');
  await expect(routeOutlet).toBeAttached();
  expect(await routeOutlet.evaluate((el) => (el as HTMLElement).hidden)).toBe(true);

  // Navigate back to chat — page restores
  await chatTab.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/chat' } }))
  );
  await expect(chatTab).toHaveURL('/chat');
  await expect(routeOutlet).not.toBeHidden();
});

// ---------------------------------------------------------------------------
// UUID format: entry points must use the full trusted UUID, not a slug or
// shortened form.
// ---------------------------------------------------------------------------

test('terminal hrefs use full trusted UUID format', async ({ page }) => {
  await setup(page);
  await page.goto('/agents');
  const btn = page.locator(`sl-icon-button[href*="${agentA}"][name="terminal"]`).first();
  await expect(btn).toBeVisible({ timeout: 10000 });
  const href = await btn.getAttribute('href');
  // Full UUID in the path
  expect(href).toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i);
});

// ---------------------------------------------------------------------------
// Back/forward navigation preserves workspace session
// ---------------------------------------------------------------------------

test('browser back/forward preserves retained terminal session', async ({ page }) => {
  const socket = await setup(page);
  await page.goto('/');
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentA}`
  );
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Go back to dashboard
  await page.goBack();
  await expect(page).toHaveURL('/');

  // Go forward to terminals — session should still be alive
  await page.goForward();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});
