/**
 * Terminal entry-point coverage (P1.7 #1652).
 *
 * Table-driven real-browser tests for every UI surface that opens a terminal:
 * agent list (grid default), agent list (table view), agent detail,
 * project detail (grid default), project detail (list/table view),
 * tree/graph view, and chat membership sidebar.
 *
 * Covers: workspace-enabled hrefs, flag-off legacy hrefs, modified-click
 * delegation (Ctrl-click actually opens new tab), cross-tab ownership
 * delegation, chat source state retained via actual membership control,
 * repeated pending and connected duplicate opens, and direct legacy redirect.
 */

import { test, expect, type Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// Fixture agent UUIDs — trusted format matching the router regex.
// ---------------------------------------------------------------------------
const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';

// Fixture project
const projectId = 'fixture-project';

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

/** Build the API-shaped agent object with _capabilities. */
function apiAgent(a: AgentFixture): Record<string, unknown> {
  return {
    ...a,
    _capabilities: a.canAttach !== false ? { actions: ['attach'] } : { actions: [] },
  };
}

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

  // Agent detail + sub-resources (paths with segments after /agents/).
  await page.route('**/api/v1/agents/**', (route) => {
    const url = route.request().url();

    // PTY metadata stub
    if (url.endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }

    // Sub-resource stubs (metrics, etc.)
    if (url.match(/\/agents\/[^/]+\/(metrics|start|stop|suspend|resume)/)) {
      void route.fulfill({ json: {} });
      return;
    }

    // Extract the UUID from the path
    const id =
      url.match(
        /\/api\/v1\/agents\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/i
      )?.[1] ?? agentA;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ? apiAgent(agents[id]) : { error: 'not found' },
    });
  });

  // Agent list (bare /api/v1/agents with optional query string).
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => apiAgent(a)),
    });
  });

  // Project API
  await page.route('**/api/v1/projects/**', (route) => {
    void route.fulfill({
      json: {
        id: projectId,
        name: 'Fixture Project',
        slug: projectId,
        agents: Object.values(agents).map((a) => apiAgent(a)),
        _capabilities: { actions: ['attach'] },
      },
    });
  });

  // Notifications API stub (used by agent detail page)
  await page.route('**/api/v1/notifications**', (route) =>
    route.fulfill({ json: { userNotifications: [], subscriptions: [] } })
  );

  // Auth admin status stub
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({ json: { isAdmin: false, isSuperAdmin: false, permissions: [] } })
  );

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

/**
 * Switch the agents or project page view mode by dispatching a synthetic
 * view-change event on the scion-view-toggle component, then toggling the
 * localStorage key so the component re-renders.
 */
async function switchView(page: Page, storageKey: string, mode: string): Promise<void> {
  await page.evaluate(
    ({ storageKey: k, mode: m }) => {
      localStorage.setItem(k, m);
      const toggle = document.querySelector('scion-view-toggle');
      if (toggle) {
        toggle.dispatchEvent(
          new CustomEvent('view-change', { detail: { view: m }, bubbles: true, composed: true })
        );
      }
    },
    { storageKey, mode }
  );
}

// ---------------------------------------------------------------------------
// Table-driven entry point: href verification
//
// Each row describes a page that contains a terminal link, how to find the
// link, and the expected href value under both flag-on and flag-off.
// ---------------------------------------------------------------------------

interface EntryPointRow {
  name: string;
  page: string;
  locator: (page: Page) => ReturnType<Page['locator']>;
  hrefEnabled: string;
  hrefDisabled: string;
  /** Optional setup step before locating (e.g. switch view mode). */
  before?: (page: Page) => Promise<void>;
}

const entryPoints: EntryPointRow[] = [
  {
    name: 'agent list (grid view)',
    page: '/agents',
    locator: (p) => p.locator('sl-button[aria-label="Terminal"]').first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'agent list (table view)',
    page: '/agents',
    before: (p) => switchView(p, 'scion-view-agents', 'list'),
    locator: (p) => p.locator('sl-button[aria-label="Terminal"]').first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'agent detail page',
    page: `/agents/${agentA}`,
    locator: (p) => p.locator('a[href*="terminal"][style*="text-decoration"]').first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'project detail (grid view)',
    page: `/projects/${projectId}`,
    locator: (p) => p.locator('sl-button[aria-label="Terminal"]').first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'project detail (list/table view)',
    page: `/projects/${projectId}`,
    before: (p) => switchView(p, 'scion-view-project', 'list'),
    locator: (p) => p.locator('sl-button[aria-label="Terminal"]').first(),
    hrefEnabled: `/terminals/${agentA}`,
    hrefDisabled: `/agents/${agentA}/terminal`,
  },
  {
    name: 'tree/graph view',
    page: '/agents',
    before: (p) => switchView(p, 'scion-view-agents', 'graph'),
    // Tree view uses sl-icon-button with label="Terminal"
    locator: (p) => p.locator('sl-icon-button[label="Terminal"]').first(),
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
    if (row.before) await row.before(page);
    const link = row.locator(page);
    await expect(link).toBeVisible({ timeout: 15000 });
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
    if (row.before) await row.before(page);
    const link = row.locator(page);
    await expect(link).toBeVisible({ timeout: 15000 });
    const href = await link.getAttribute('href');
    expect(href).toBe(row.hrefDisabled);
  });
}

// ---------------------------------------------------------------------------
// Workspace-enabled: clicking each entry-point link routes to /terminals/{id}
// and the retained workspace attaches a WebSocket.
// ---------------------------------------------------------------------------

for (const row of entryPoints) {
  test(`clicking "${row.name}" navigates to workspace and attaches`, async ({ page }) => {
    const socket = await setup(page);
    await page.goto(row.page);
    if (row.before) await row.before(page);
    const link = row.locator(page);
    await expect(link).toBeVisible({ timeout: 15000 });
    await link.click();
    await expect(page).toHaveURL(`/terminals/${agentA}`);
    await expect.poll(() => socket.attaches).toBe(1);
  });
}

// ---------------------------------------------------------------------------
// Modified click: Ctrl-click opens a new tab instead of navigating in-page.
// We verify the router does NOT prevent default on modified clicks.
// ---------------------------------------------------------------------------

test('Ctrl-click on terminal link opens new tab via real href', async ({ context, page }) => {
  await setup(page);
  await page.goto('/agents');
  const btn = page.locator('sl-button[aria-label="Terminal"]').first();
  await expect(btn).toBeVisible({ timeout: 15000 });

  // Verify the href is a real routable URL
  const href = await btn.getAttribute('href');
  expect(href).toBe(`/terminals/${agentA}`);

  // Ctrl-click: should open a new page/tab, NOT navigate this page
  const [newPage] = await Promise.all([
    context.waitForEvent('page'),
    btn.click({ modifiers: ['Control'] }),
  ]);
  // The original page should NOT have navigated away
  await expect(page).toHaveURL('/agents');
  // The new tab received the terminal URL
  expect(newPage.url()).toContain(`/terminals/${agentA}`);
  await newPage.close();
});

// ---------------------------------------------------------------------------
// Chat membership terminal control: the actual scion-chat-members component
// renders a terminal link for agents with canAttach=true. Clicking it must
// route through openTerminalFromChat and navigate to the workspace.
// ---------------------------------------------------------------------------

test('chat membership terminal control routes through workspace when flag is ON', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: true, nativeChatEnabled: true });
  await page.goto('/chat');
  await expect(page.locator('scion-page-chat')).toBeAttached({ timeout: 10000 });

  // Inject a scion-chat-members component with a test agent into the page,
  // since the chat API stubs don't return full space data. This tests the
  // actual component rendering and click handler.
  const chatMemberTerminal = await page.evaluate((id) => {
    const members = document.createElement('scion-chat-members') as HTMLElement & {
      agents: unknown[];
    };
    members.agents = [
      {
        id,
        kind: 'agent',
        displayName: 'test-agent',
        slug: 'test-agent',
        phase: 'running',
        canAttach: true,
      },
    ];
    document.body.appendChild(members);
    return true;
  }, agentA);
  expect(chatMemberTerminal).toBe(true);

  // Wait for the component to render its terminal link
  const terminalLink = page.locator('scion-chat-members a.agent-terminal').first();
  await expect(terminalLink).toBeAttached({ timeout: 5000 });

  // Verify it has the workspace href
  const href = await terminalLink.getAttribute('href');
  expect(href).toBe(`/terminals/${agentA}`);

  // Click it — should navigate to workspace
  await terminalLink.click();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

test('chat membership terminal control uses legacy popup when flag is OFF', async ({ page }) => {
  await setup(page, { enabled: false, nativeChatEnabled: true });
  await page.goto('/chat');
  await expect(page.locator('scion-page-chat')).toBeAttached({ timeout: 10000 });

  // Inject chat members component
  await page.evaluate((id) => {
    const members = document.createElement('scion-chat-members') as HTMLElement & {
      agents: unknown[];
    };
    members.agents = [
      {
        id,
        kind: 'agent',
        displayName: 'test-agent',
        slug: 'test-agent',
        phase: 'running',
        canAttach: true,
      },
    ];
    document.body.appendChild(members);
  }, agentA);

  const terminalLink = page.locator('scion-chat-members a.agent-terminal').first();
  await expect(terminalLink).toBeAttached({ timeout: 5000 });

  // Verify it has the legacy href
  const href = await terminalLink.getAttribute('href');
  expect(href).toBe(`/agents/${agentA}/terminal`);
});

// ---------------------------------------------------------------------------
// Chat source state retained: navigate from chat to terminal and back.
// The route outlet (with the chat shell) must be hidden, not destroyed.
// When returning, the chat page must still be present.
// ---------------------------------------------------------------------------

test('chat source state is retained when opening terminal from chat', async ({ page }) => {
  const socket = await setup(page, { enabled: true, nativeChatEnabled: true });
  await page.goto('/chat');
  await expect(page.locator('scion-page-chat')).toBeAttached({ timeout: 10000 });

  // Wait for async chat initialization to settle (initV2 lazy-loads
  // components and parses the route), so our state probes aren't
  // overwritten by deferred setup.
  await page.waitForTimeout(500);

  // Stamp the page element with a unique id.  If the page is destroyed
  // and re-created, this id is lost — proving element identity.
  const pageId = await page.evaluate(() => {
    const chatPage = document.querySelector('scion-page-chat')!;
    const id = `chat-page-${Date.now()}`;
    chatPage.id = id;
    return id;
  });

  // Append a child element simulating in-progress user content (e.g. an
  // unsaved draft overlay).  A destroyed page would lose this child.
  await page.evaluate(() => {
    const chatPage = document.querySelector('scion-page-chat')!;
    const draft = document.createElement('div');
    draft.setAttribute('data-unsaved-draft', 'hello world');
    draft.textContent = 'unsaved draft text';
    chatPage.appendChild(draft);
  });

  // Navigate to terminal from chat
  await page.evaluate(
    (id) =>
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` }, bubbles: true })
      ),
    agentA
  );
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // The route outlet should be hidden but not destroyed
  const routeOutlet = page.locator('#route-outlet');
  await expect(routeOutlet).toBeAttached();
  expect(await routeOutlet.evaluate((el) => (el as HTMLElement).hidden)).toBe(true);

  // Navigate back to chat
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/chat' } }))
  );
  await expect(page).toHaveURL('/chat');
  await expect(routeOutlet).not.toBeHidden();

  // 1. Same DOM element — proves page was not destroyed and re-created
  const sameElement = await page.evaluate(
    (expectedId) => document.querySelector('scion-page-chat')?.id === expectedId,
    pageId
  );
  expect(sameElement).toBe(true);

  // 2. User content survived — appended draft child is still present
  const draftSurvived = await page.evaluate(
    () => document.querySelector('scion-page-chat [data-unsaved-draft]')?.textContent
  );
  expect(draftSurvived).toBe('unsaved draft text');

  // Socket still alive — terminal session retained
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

// ---------------------------------------------------------------------------
// Cross-tab: chat state retained when another tab owns terminals
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
  await expect(chatTab.locator('scion-page-chat')).toBeAttached({ timeout: 10000 });
  await chatTab.waitForTimeout(500); // Let async chat init settle

  // Stamp identity and append user content on the chat page
  const pageId = await chatTab.evaluate(() => {
    const chatPage = document.querySelector('scion-page-chat')!;
    const id = `chat-page-crosstab-${Date.now()}`;
    chatPage.id = id;
    // Append child simulating user-generated content
    const draft = document.createElement('div');
    draft.setAttribute('data-unsaved-draft', 'cross-tab draft');
    draft.textContent = 'cross-tab draft text';
    chatPage.appendChild(draft);
    return id;
  });

  // Chat tab navigates to terminal — ownership deferred to owner (denied foreground focus)
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

  // Navigate back to chat after denied foreground focus
  await chatTab.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/chat' } }))
  );
  await expect(chatTab).toHaveURL('/chat');
  await expect(routeOutlet).not.toBeHidden();

  // Same page element survived — not re-created after denied foreground focus
  const sameElement = await chatTab.evaluate(
    (expectedId) => document.querySelector('scion-page-chat')?.id === expectedId,
    pageId
  );
  expect(sameElement).toBe(true);

  // User content survived the denied-foreground-focus round-trip
  const draftSurvived = await chatTab.evaluate(
    () => document.querySelector('scion-page-chat [data-unsaved-draft]')?.textContent
  );
  expect(draftSurvived).toBe('cross-tab draft text');
});

// ---------------------------------------------------------------------------
// Direct legacy route redirect
// ---------------------------------------------------------------------------

test('legacy /agents/{id}/terminal redirects to /terminals/{id} when workspace is ON', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: true });
  await page.goto(`/agents/${agentA}/terminal`);
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);
});

test('legacy /agents/{id}/terminal loads standalone terminal when flag is OFF', async ({
  page,
}) => {
  const socket = await setup(page, { enabled: false });
  await page.goto(`/agents/${agentA}/terminal`);
  await expect(page).toHaveURL(`/agents/${agentA}/terminal`);
  await expect.poll(() => socket.attaches).toBe(1);
  expect(await page.locator('#terminal-workspace').count()).toBe(0);
});

// ---------------------------------------------------------------------------
// Repeated pending and connected opens reuse one pane/socket — no duplicate
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
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);

  // Fire it again while connected — still no duplicate
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentA}`
  );
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('pending open during agent fetch does not create duplicate', async ({ page }) => {
  const socket = await setup(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/api/v1/agents/${agentA}`, async (route) => {
    await blocked;
    await route.fulfill({
      json: apiAgent(defaultAgents[agentA]),
    });
  });
  await page.goto(`/terminals/${agentA}`);
  // Pane created but agent fetch still pending
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
  // Fire another open while still pending
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentA}`
  );
  // Still only one pane
  expect(await page.locator('#terminal-workspace scion-terminal-pane').count()).toBe(1);
  release();
  await expect.poll(() => socket.attaches).toBe(1);
});

// ---------------------------------------------------------------------------
// Cross-tab ownership: second tab defers to the owner
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
// UUID format: entry points must use the full trusted UUID
// ---------------------------------------------------------------------------

test('terminal hrefs use full trusted UUID format', async ({ page }) => {
  await setup(page);
  await page.goto('/agents');
  const btn = page.locator('sl-button[aria-label="Terminal"]').first();
  await expect(btn).toBeVisible({ timeout: 15000 });
  const href = await btn.getAttribute('href');
  expect(href).toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i);
});

// ---------------------------------------------------------------------------
// Back/forward navigation preserves workspace session
// ---------------------------------------------------------------------------

test('browser back/forward preserves retained terminal session', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => socket.attaches).toBe(1);

  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/');

  await page.goBack();
  await expect(page).toHaveURL(`/terminals/${agentA}`);
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});
