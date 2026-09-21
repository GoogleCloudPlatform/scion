import { test, expect, type Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// #1702 — DM header Terminal/Graph buttons & member list Graph icon
// ---------------------------------------------------------------------------

const agentId = '11111111-1111-4111-8111-111111111111';
const userId = 'fixture-user';
const projectId = 'fixture-project';

/** Full DM key for the agent↔user conversation. */
const dmKey = `dm:agent:${agentId}:user:${userId}`;

/** Encoded DM key for use in URLs. */
const encodedDmKey = encodeURIComponent(dmKey);

interface ChatSetupOptions {
  /** Whether the agent has a projectId. Default true. */
  agentHasProject?: boolean;
  /** Whether to navigate to the DM. Default true. */
  navigateToDM?: boolean;
  /** Whether to enable the terminal workspace flag. Default true. */
  terminalWorkspace?: boolean;
}

/**
 * Set up the chat page with mocked APIs for an agent DM conversation.
 *
 * The DM page component (`scion-page-chat`) is freshly created on each
 * navigation.  In production the agent member list is populated before the
 * user clicks into a DM (from the space view or the hub sidebar), so
 * `v2AgentMembers` already has data by the time the header renders.  In an
 * isolated test we navigate directly to a DM URL, so the member list is
 * empty on first render.
 *
 * After the page loads we inject `v2AgentMembers` (and `v2HumanMembers`)
 * into the component, which is the equivalent of the hub member API
 * response that a real session would have received before the DM was
 * opened.  LitElement's `@state()` decorator triggers a re-render when
 * the property is set, exactly as it does in the real application.
 */
async function setupChat(page: Page, opts: ChatSetupOptions = {}): Promise<void> {
  const { agentHasProject = true, navigateToDM = true, terminalWorkspace = true } = opts;

  const agentProjectId = agentHasProject ? projectId : '';

  // Feature flags: both native_chat flags ON (default), terminal workspace
  await page.addInitScript(
    ({ tw }) => {
      window.__SCION_FEATURES__ = {
        'web.native_chat': true,
        'web.native_chat_v2': true,
        'web.terminal_workspace': tw,
      };
      // Suppress EventSource (SSE) — no real server
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    { tw: terminalWorkspace }
  );

  // Auth
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: userId, email: 'fixture@example.test' } })
  );

  // Public settings
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );

  // System status
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  // Agent list (loadHubMembers expects { agents: [...] })
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: {
        agents: [
          {
            id: agentId,
            name: 'test-agent',
            slug: 'test-agent',
            phase: 'running',
            projectId: agentProjectId,
            canAttach: true,
          },
        ],
      },
    });
  });
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      json: { id: agentId, name: 'test-agent', phase: 'running', projectId: agentProjectId },
    });
  });

  // Users (loadHubMembers expects { users: [...] })
  await page.route('**/api/v1/users**', (route) => {
    void route.fulfill({
      json: {
        users: [{ id: userId, displayName: 'Fixture User', email: 'fixture@example.test' }],
      },
    });
  });

  // Chat spaces (space rail)
  await page.route('**/api/v1/chat/spaces', (route) => {
    const url = route.request().url();
    if (/\/spaces\/[^/]+/.test(url.split('/api/v1/chat/')[1] || '')) {
      void route.fallback();
      return;
    }
    void route.fulfill({
      json: {
        spaces: agentProjectId
          ? [
              {
                projectId: agentProjectId,
                projectSlug: 'fixture-proj',
                projectName: 'Fixture Project',
              },
            ]
          : [],
      },
    });
  });

  // Chat DMs
  await page.route('**/api/v1/chat/dms**', (route) => {
    const url = route.request().url();
    if (url.includes('/unread')) {
      void route.fulfill({ json: { peerIds: [] } });
      return;
    }
    void route.fulfill({
      json: {
        dms: [
          {
            conversationKey: dmKey,
            peerId: agentId,
            peerKind: 'agent',
            peerName: 'Test Agent',
            peerSlug: 'test-agent',
          },
        ],
      },
    });
  });

  // Chat space members
  await page.route(/\/api\/v1\/chat\/spaces\/[^/]+\/members/, (route) =>
    route.fulfill({
      json: {
        agents: [
          {
            id: agentId,
            kind: 'agent',
            displayName: 'Test Agent',
            slug: 'test-agent',
            phase: 'running',
            projectId: agentProjectId,
            canAttach: true,
          },
        ],
        humans: [
          { id: userId, kind: 'user', displayName: 'Fixture User', email: 'fixture@example.test' },
        ],
      },
    })
  );

  // Chat threads (empty — DMs don't have threads)
  await page.route(/\/api\/v1\/chat\/spaces\/[^/]+\/threads/, (route) =>
    route.fulfill({ json: { threads: [] } })
  );

  // Other chat endpoints
  await page.route(/\/api\/v1\/chat\/topics\//, (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/chat/presence', (route) => route.fulfill({ json: {} }));
  await page.route(/\/api\/v1\/chat\/conversations\//, (route) => route.fulfill({ json: {} }));
  await page.route(/\/api\/v1\/chat\/messages/, (route) =>
    route.fulfill({ json: { messages: [] } })
  );

  // WebSocket for PTY connections
  await page.routeWebSocket('**/pty?*', () => {
    // No-op — prevent 404 on WebSocket upgrade
  });

  if (navigateToDM) {
    await page.goto(`/chat/dm/${encodedDmKey}`);
  }
}

/**
 * Inject agent and human member data into the chat component.
 *
 * In production, `v2AgentMembers` is populated from the hub member API
 * before the user navigates to a DM.  In a test that loads directly into
 * a DM URL, the component starts with an empty member list.  This helper
 * simulates the real pre-populated state by setting the reactive properties
 * on the LitElement, which triggers a re-render.
 */
async function injectMembers(page: Page, opts: { agentProjectId?: string } = {}): Promise<void> {
  const pid = opts.agentProjectId ?? projectId;
  await page.evaluate(
    ({ aid, pid: projId, uid }) => {
      const chatShell = document.querySelector('scion-chat-shell');
      const chat = chatShell?.querySelector('scion-page-chat') as
        | (HTMLElement & {
            v2AgentMembers: unknown[];
            v2HumanMembers: unknown[];
          })
        | null;
      if (!chat) throw new Error('scion-page-chat not found inside scion-chat-shell');

      chat.v2AgentMembers = [
        {
          id: aid,
          kind: 'agent',
          displayName: 'Test Agent',
          slug: 'test-agent',
          phase: 'running',
          projectId: projId,
          canAttach: true,
        },
      ];
      chat.v2HumanMembers = [
        {
          id: uid,
          kind: 'user',
          displayName: 'Fixture User',
          email: 'fixture@example.test',
        },
      ];
    },
    { aid: agentId, pid, uid: userId }
  );
}

// ---------------------------------------------------------------------------
// DM Header Icon Tests
// ---------------------------------------------------------------------------

test.describe('DM header navigation icons (#1702)', () => {
  test('terminal button present in agent DM header', async ({ page }) => {
    await setupChat(page);

    // The terminal button appears immediately — it only needs the DM
    // peer to be an agent (conv.isDM && conv.peerKind === 'agent').
    const terminalBtn = page.locator('sl-icon-button[name="terminal"][label="Open terminal"]');
    await expect(terminalBtn).toBeVisible({ timeout: 10000 });
  });

  test('graph button present in agent DM header when agent has projectId', async ({ page }) => {
    await setupChat(page);

    // Wait for the page to render — terminal button is the first gate
    const terminalBtn = page.locator('sl-icon-button[name="terminal"][label="Open terminal"]');
    await expect(terminalBtn).toBeVisible({ timeout: 10000 });

    // Inject agent members so getAgentProjectId() can resolve the projectId
    await injectMembers(page);

    const graphBtn = page.locator('sl-icon-button[name="diagram-3"][label="Open in graph"]');
    await expect(graphBtn).toBeVisible({ timeout: 5000 });
  });

  test('graph button href contains correct project and focus params', async ({ page }) => {
    await setupChat(page);
    await expect(
      page.locator('sl-icon-button[name="terminal"][label="Open terminal"]')
    ).toBeVisible({ timeout: 10000 });
    await injectMembers(page);

    const graphBtn = page.locator('sl-icon-button[name="diagram-3"][label="Open in graph"]');
    await expect(graphBtn).toBeVisible({ timeout: 5000 });

    const href = await graphBtn.getAttribute('href');
    expect(href).not.toBeNull();
    expect(href).toContain('/agents/graph?project=');
    expect(href).toContain(`project=${encodeURIComponent(projectId)}`);
    expect(href).toContain(`&focus=${encodeURIComponent(agentId)}`);
  });

  test('terminal button navigates to /terminals/{agentId}', async ({ page }) => {
    await setupChat(page);

    const terminalBtn = page.locator('sl-icon-button[name="terminal"][label="Open terminal"]');
    await expect(terminalBtn).toBeVisible({ timeout: 10000 });

    // The terminal button href should point to the terminal workspace route
    const href = await terminalBtn.getAttribute('href');
    expect(href).toBe(`/terminals/${agentId}`);

    // Click the terminal button — it dispatches a nav-click event which the
    // router handles, navigating to /terminals/{agentId}
    await terminalBtn.click();
    await expect(page).toHaveURL(`/terminals/${agentId}`);
  });

  test('graph button absent when agent has no projectId', async ({ page }) => {
    await setupChat(page, { agentHasProject: false });
    await expect(
      page.locator('sl-icon-button[name="terminal"][label="Open terminal"]')
    ).toBeVisible({ timeout: 10000 });

    // Inject agents with empty projectId
    await injectMembers(page, { agentProjectId: '' });

    // Graph button should NOT be present (no projectId to resolve)
    const graphBtn = page.locator('sl-icon-button[name="diagram-3"][label="Open in graph"]');
    await expect(graphBtn).toHaveCount(0);
  });

  test('DM buttons do not appear in a thread (non-DM) conversation', async ({ page }) => {
    // Navigate to a space view (not a DM)
    await setupChat(page, { navigateToDM: false });
    await page.goto('/chat/fixture-proj');

    // Wait for the page to load
    await expect(page.locator('scion-page-chat')).toBeVisible({ timeout: 10000 });

    // Neither terminal nor graph icon-button should appear in the header
    // (they are gated on conv.isDM && conv.peerKind === 'agent')
    const terminalBtn = page.locator('sl-icon-button[name="terminal"][label="Open terminal"]');
    await expect(terminalBtn).toHaveCount(0);

    const graphBtn = page.locator('sl-icon-button[name="diagram-3"][label="Open in graph"]');
    await expect(graphBtn).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// Member List Graph Icon Tests
// ---------------------------------------------------------------------------

test.describe('Member list graph icon (#1702)', () => {
  test('agent entry shows diagram-3 icon instead of box-arrow-up-right', async ({ page }) => {
    await setupChat(page);
    await expect(
      page.locator('sl-icon-button[name="terminal"][label="Open terminal"]')
    ).toBeVisible({ timeout: 10000 });

    // Inject members so the sidebar displays the agent entry
    await injectMembers(page);

    const membersPanel = page.locator('scion-chat-members');
    await expect(membersPanel).toBeVisible({ timeout: 5000 });

    // The graph icon (diagram-3) should be present in the agent member entry
    const graphIcon = membersPanel.locator('sl-icon[name="diagram-3"]');
    await expect(graphIcon).toBeVisible({ timeout: 5000 });

    // The old pop-out icon class should NOT exist
    const popoutIcon = membersPanel.locator('.agent-popout');
    await expect(popoutIcon).toHaveCount(0);
  });

  test('graph link has correct href with project and focus params', async ({ page }) => {
    await setupChat(page);
    await expect(
      page.locator('sl-icon-button[name="terminal"][label="Open terminal"]')
    ).toBeVisible({ timeout: 10000 });
    await injectMembers(page);

    const membersPanel = page.locator('scion-chat-members');
    await expect(membersPanel).toBeVisible({ timeout: 5000 });

    const graphLink = membersPanel.locator('a.agent-graph');
    await expect(graphLink).toBeVisible({ timeout: 5000 });

    const href = await graphLink.getAttribute('href');
    expect(href).not.toBeNull();
    expect(href).toContain('/agents/graph?project=');
    expect(href).toContain(`project=${encodeURIComponent(projectId)}`);
    expect(href).toContain(`&focus=${encodeURIComponent(agentId)}`);
  });

  test('graph icon absent in member list when agent has no projectId', async ({ page }) => {
    await setupChat(page, { agentHasProject: false });
    await expect(
      page.locator('sl-icon-button[name="terminal"][label="Open terminal"]')
    ).toBeVisible({ timeout: 10000 });

    // Inject agents with empty projectId
    await injectMembers(page, { agentProjectId: '' });

    const membersPanel = page.locator('scion-chat-members');
    await expect(membersPanel).toBeVisible({ timeout: 5000 });

    // The graph link should NOT be present (gated on a.projectId)
    const graphLink = membersPanel.locator('a.agent-graph');
    await expect(graphLink).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// Icon Registration Proof
// ---------------------------------------------------------------------------

test.describe('diagram-3 icon registration (#1702)', () => {
  test('diagram-3 is registered in copy-shoelace-icons.mjs', async () => {
    const fs = await import('node:fs');
    const path = await import('node:path');
    const { fileURLToPath } = await import('node:url');
    const thisDir = path.dirname(fileURLToPath(import.meta.url));

    // Verify the icon is in the USED_ICONS list in copy-shoelace-icons.mjs
    const scriptPath = path.resolve(thisDir, '../../scripts/copy-shoelace-icons.mjs');
    const scriptContent = fs.readFileSync(scriptPath, 'utf-8');
    expect(scriptContent).toContain("'diagram-3'");
  });

  test('diagram-3.svg exists in production build output and matches Shoelace source', async () => {
    const fs = await import('node:fs');
    const path = await import('node:path');
    const { fileURLToPath } = await import('node:url');
    const thisDir = path.dirname(fileURLToPath(import.meta.url));
    const distPath = path.resolve(thisDir, '../../dist/client');

    // Verify the built SVG file exists
    const builtSvg = path.join(distPath, 'shoelace/assets/icons/diagram-3.svg');
    expect(fs.existsSync(builtSvg)).toBe(true);

    // Verify it's a real SVG with content
    const builtContent = fs.readFileSync(builtSvg, 'utf-8');
    expect(builtContent).toContain('<svg');
    expect(builtContent.length).toBeGreaterThan(100);

    // Compare against packaged Shoelace source — byte-equal copy
    const sourceSvg = path.join(
      thisDir,
      '../../node_modules/@shoelace-style/shoelace/dist/assets/icons/diagram-3.svg'
    );
    const sourceContent = fs.readFileSync(sourceSvg, 'utf-8');
    expect(builtContent).toBe(sourceContent);
  });
});
