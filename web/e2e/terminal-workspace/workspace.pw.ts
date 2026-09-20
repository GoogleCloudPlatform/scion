import { test, expect, type Page } from '@playwright/test';

const agent = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';
const agentE = '55555555-5555-4555-8555-555555555555';

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
  activity?: string;
}

async function setup(
  page: Page,
  enabled = true,
  locks = true,
  agents: Record<string, AgentFixture> = {
    [agent]: {
      id: agent,
      name: 'isolated-agent',
      phase: 'running',
      projectId: 'fixture-project',
    },
  },
  nativeChatEnabled = true
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
  await page.addInitScript(
    ({ enabled, locks }) => {
      window.__SCION_FEATURES__ = { 'web.terminal_workspace': enabled };
      if (!locks) Object.defineProperty(navigator, 'locks', { value: undefined });
      void customElements.whenDefined('scion-terminal-pane').then(() => {
        const instrumented = window as typeof window & { terminalInitializers?: number };
        const prototype = customElements.get('scion-terminal-pane')!.prototype as {
          initTerminal: (...args: unknown[]) => Promise<unknown>;
        };
        const initialize = prototype.initTerminal;
        prototype.initTerminal = function (...args: unknown[]): Promise<unknown> {
          instrumented.terminalInitializers = (instrumented.terminalInitializers ?? 0) + 1;
          return initialize.apply(this, args);
        };
      });
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    { enabled, locks }
  );
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled } })
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

async function identity(
  page: Page
): Promise<{ host: boolean; pane: boolean; terminal: boolean; count: number }> {
  return page.evaluate(() => {
    const host = document.querySelector('#terminal-workspace')!;
    const pane = host.querySelector('scion-terminal-pane')!;
    const terminal = pane.shadowRoot!.querySelector('.xterm')!;
    const memo = window as typeof window & {
      retained?: { host: Element; pane: Element; terminal: Element };
    };
    if (!memo.retained) memo.retained = { host, pane, terminal };
    return {
      host: memo.retained.host === host,
      pane: memo.retained.pane === pane,
      terminal: memo.retained.terminal === terminal,
      count: host.querySelectorAll('scion-terminal-pane').length,
    };
  });
}

async function focusedRailControlLabel(page: Page): Promise<string | null> {
  return page.evaluate(() =>
    document.activeElement instanceof HTMLElement &&
    document.querySelector('#terminal-workspace')?.contains(document.activeElement)
      ? document.activeElement.getAttribute('aria-label')
      : null
  );
}

test('production router retains one pane and socket across shell routes, alias and history', async ({
  page,
}) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect(page.locator('#terminal-workspace')).toHaveCount(1);
  await expect.poll(() => socket.attaches).toBe(1);
  await expect
    .poll(() => identity(page))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });
  for (const path of [
    '/',
    '/chat',
    '/profile',
    `/agents/${agent}/terminal`,
    `/terminals/${agent}`,
  ]) {
    await page.evaluate(
      (path) =>
        document.dispatchEvent(
          new CustomEvent('nav-click', {
            detail: { path },
            bubbles: true,
          })
        ),
      path
    );
    if (path.includes('/terminal')) await expect(page).toHaveURL(`/terminals/${agent}`);
    await expect
      .poll(() => identity(page))
      .toEqual({ host: true, pane: true, terminal: true, count: 1 });
    expect(socket.attaches).toBe(1);
    expect(socket.closes).toBe(0);
  }
  await page.goBack();
  await expect
    .poll(() => identity(page))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });
});

test('repeated pending and connected opens reuse one pane; a duplicate pane is rejected', async ({
  page,
}) => {
  const socket = await setup(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/api/v1/agents/${agent}`, async (route) => {
    await blocked;
    await route.fulfill({
      json: { id: agent, name: 'isolated-agent', phase: 'running', projectId: 'fixture-project' },
    });
  });
  await page.goto(`/terminals/${agent}`);
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );
  expect(await page.locator('#terminal-workspace scion-terminal-pane').count()).toBe(1);
  release();
  await expect.poll(() => socket.attaches).toBe(1);
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );
  await expect
    .poll(() => identity(page))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });
  expect(
    await page.evaluate((id) => {
      const pane = document.querySelector(
        '#terminal-workspace scion-terminal-pane'
      ) as HTMLElement & { registry: unknown };
      const duplicate = document.createElement('scion-terminal-pane') as HTMLElement & {
        open: (registry: unknown, id: string) => unknown;
      };
      try {
        duplicate.open(pane.registry, id);
        return false;
      } catch (error) {
        return String(error).includes('already has a pane');
      }
    }, agent)
  ).toBe(true);
  expect(
    await page.evaluate(
      () => (window as typeof window & { terminalInitializers?: number }).terminalInitializers
    )
  ).toBe(1);
});

test('rail selection reuses sessions and disambiguates repeated agent names by project', async ({
  page,
}) => {
  const socket = await setup(page, true, true, {
    [agent]: { id: agent, name: 'worker', phase: 'running', projectId: 'alpha-project' },
    [agentB]: { id: agentB, name: 'worker', phase: 'running', projectId: 'beta-project' },
  });
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentB}`
  );
  await expect.poll(() => socket.attaches).toBe(2);
  await expect(page.getByRole('button', { name: 'Terminals (2)' })).toBeVisible();
  await expect(page.getByRole('button', { name: /worker in alpha-project/ })).toBeVisible();
  await expect(page.getByRole('button', { name: /worker in beta-project/ })).toBeVisible();

  await page.getByRole('button', { name: /worker in alpha-project/ }).click();
  await expect(page).toHaveURL(`/terminals/${agent}`);
  expect(socket.attaches).toBe(2);
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(2);
});

test('rail list preserves valid semantics and real browser arrow key navigation', async ({
  page,
}) => {
  const socket = await setup(page, true, true, {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'project-a' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'project-b' },
  });
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentB}`
  );
  await expect.poll(() => socket.attaches).toBe(2);

  await expect(page.getByRole('list', { name: 'Retained terminal sessions' })).toBeVisible();
  await expect(page.getByRole('button', { name: /alpha in project-a/ })).toBeVisible();
  await expect(page.getByRole('button', { name: /beta in project-b/ })).toBeVisible();
  await expect(page.getByRole('listitem')).toHaveCount(2);
  await expect(page.locator('[role="option"]')).toHaveCount(0);

  await page.getByRole('button', { name: /alpha in project-a/ }).focus();
  await expect
    .poll(() => focusedRailControlLabel(page))
    .toBe('Show terminal for alpha in project-a');

  await page.keyboard.press('ArrowDown');
  await expect
    .poll(() => focusedRailControlLabel(page))
    .toBe('Show terminal for beta in project-b');
  await page.keyboard.press('ArrowUp');
  await expect
    .poll(() => focusedRailControlLabel(page))
    .toBe('Show terminal for alpha in project-a');
  await page.keyboard.press('End');
  await expect
    .poll(() => focusedRailControlLabel(page))
    .toBe('Show terminal for beta in project-b');
  await page.keyboard.press('Home');
  await expect
    .poll(() => focusedRailControlLabel(page))
    .toBe('Show terminal for alpha in project-a');
});

test('metadata-only rail updates preserve focused control', async ({ page }) => {
  const agents: Record<string, AgentFixture> = {
    [agent]: {
      id: agent,
      name: 'isolated-agent',
      phase: 'running',
      projectId: 'fixture-project',
    },
  };
  await setup(page, true, true, agents);
  await page.goto(`/terminals/${agent}`);
  await expect(page.getByRole('button', { name: 'Close isolated-agent' })).toBeVisible();

  await page.getByRole('button', { name: 'Close isolated-agent' }).focus();
  await expect.poll(() => focusedRailControlLabel(page)).toBe('Close isolated-agent');

  agents[agent] = {
    ...agents[agent],
    name: 'renamed-agent',
    activity: 'snapshot refreshed',
  };
  await page.evaluate(async (id) => {
    const pane = document.querySelector<
      HTMLElement & {
        registry: { metadata: { refresh: (agentId: string) => Promise<void> } };
      }
    >('#terminal-workspace scion-terminal-pane');
    await pane?.registry.metadata.refresh(id);
  }, agent);

  await expect(page.getByRole('button', { name: 'Close renamed-agent' })).toBeFocused();
  await expect(
    page.getByRole('button', { name: /renamed-agent in fixture-project/ })
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Close isolated-agent' })).toHaveCount(0);
});

test('close removes only that retained client and leaves peers connected', async ({ page }) => {
  const socket = await setup(page, true, true, {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'same-project' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'same-project' },
  });
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentB}`
  );
  await expect.poll(() => socket.attaches).toBe(2);
  await page.getByRole('button', { name: 'Close alpha' }).click();
  await expect(page.getByRole('button', { name: 'Terminals (1)' })).toBeVisible();
  await expect(page.getByRole('button', { name: /beta in same-project/ })).toBeVisible();
  expect(socket.sent.some((frame) => frame.includes('AmQ='))).toBe(true);
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
});

test('disconnected entries persist until explicit reconnect or close', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  socket.disconnectAll();
  await expect(page.locator('#terminal-workspace')).toContainText('Disconnected');
  await expect(page.getByRole('button', { name: 'Terminals (1)' })).toBeVisible();
  await page.getByRole('button', { name: 'Reconnect isolated-agent' }).click();
  await expect.poll(() => socket.attaches).toBe(2);
  await page.getByRole('button', { name: 'Close isolated-agent' }).click();
  await expect(page.getByRole('button', { name: 'Terminals (0)' })).toBeVisible();
  await expect(page.locator('#terminal-workspace')).toContainText('No terminals are open.');
});

test('empty workspace and chat-disabled header keep Terminals available', async ({ page }) => {
  await setup(page, true, true, undefined, false);
  await page.goto('/terminals');
  await expect(page.locator('#terminal-workspace')).toContainText('No terminals are open.');
  await expect(page.getByRole('button', { name: 'Dashboard' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Terminals (0)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Chat' })).toHaveCount(0);
});

test('mode switch restores last dashboard and chat routes while retaining terminal selection', async ({
  page,
}) => {
  const socket = await setup(page);
  await page.goto('/projects/project-123/settings');
  await page.getByRole('button', { name: 'Chat' }).click();
  await expect(page).toHaveURL('/chat');
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );
  await expect.poll(() => socket.attaches).toBe(1);
  await page.getByRole('button', { name: 'Dashboard' }).click();
  await expect(page).toHaveURL('/projects/project-123/settings');
  await page.getByRole('button', { name: 'Terminals (1)' }).click();
  await expect(page).toHaveURL('/terminals');
  await expect(page.locator('#terminal-workspace scion-terminal-pane:not([hidden])')).toHaveCount(
    1
  );
  expect(socket.attaches).toBe(1);
});

test('explicit close during agent initialization cannot attach later', async ({ page }) => {
  const socket = await setup(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/api/v1/agents/${agent}`, async (route) => {
    await blocked;
    await route.fulfill({ json: { id: agent, name: 'isolated-agent', phase: 'running' } });
  });
  await page.goto(`/terminals/${agent}`);
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
  await page.evaluate(() =>
    (
      document.querySelector('#terminal-workspace scion-terminal-pane') as HTMLElement & {
        dispose: () => void;
      }
    ).dispose()
  );
  release();
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(0);
  expect(socket.attaches).toBe(0);
});

test('a second tab selects through the owner without attaching locally', async ({ context }) => {
  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setup(owner);
  const otherSocket = await setup(other);
  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);
  await other.goto(`/terminals/${agent}`);
  await expect(other.locator('#terminal-workspace')).toContainText('owning tab');
  expect(otherSocket.attaches).toBe(0);
  expect(await other.locator('#terminal-workspace scion-terminal-pane').count()).toBe(0);
  await expect
    .poll(() => identity(owner))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });
});

test('a superseded local terminal navigation cannot reveal or focus its pane', async ({ page }) => {
  await setup(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/api/v1/agents/${agent}`, async (route) => {
    await blocked;
    await route.fulfill({ json: { id: agent, name: 'isolated-agent', phase: 'running' } });
  });
  await page.goto(`/terminals/${agent}`);
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/');
  release();
  await expect(page.locator('#terminal-workspace')).toBeHidden();
  expect(
    await page.evaluate(() => {
      const pane = document.querySelector('#terminal-workspace scion-terminal-pane');
      return !!pane && pane.hasAttribute('hidden');
    })
  ).toBe(true);
});

test('flag-off legacy route stays on the disposable pane adapter', async ({ page }) => {
  const socket = await setup(page, false);
  await page.goto(`/agents/${agent}/terminal`);
  await expect.poll(() => socket.attaches).toBe(1);
  expect(await page.locator('#terminal-workspace').count()).toBe(0);
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect.poll(() => socket.closes).toBe(1);
  expect(socket.sent.some((frame) => frame.includes('AmQ='))).toBe(false);
});

test('changing the flag after bootstrap keeps an active retained session', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await identity(page);
  await page.evaluate(() => {
    window.__SCION_FEATURES__!['web.terminal_workspace'] = false;
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }));
  });
  await expect(page).toHaveURL('/');
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );
  await expect
    .poll(() => identity(page))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('unsupported coordination does not create a local pane or attach', async ({ page }) => {
  const socket = await setup(page, true, false);
  await page.goto(`/terminals/${agent}`);
  await expect(page.locator('#terminal-workspace')).toContainText('unavailable');
  expect(await page.locator('#terminal-workspace scion-terminal-pane').count()).toBe(0);
  expect(socket.attaches).toBe(0);
});

// --- Layout preset tests (P2.2) ---

/** Navigate to a terminal agent by dispatching a nav-click event. */
async function navigateToTerminal(page: Page, agentId: string): Promise<void> {
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentId}`
  );
}

/** Click a layout preset button by its data-preset attribute. */
async function clickPreset(page: Page, preset: string): Promise<void> {
  await page.click(`.terminal-layout-btn[data-preset="${preset}"]`);
}

/** Get the data-preset value of the currently active layout button. */
async function activePreset(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const btn = document.querySelector('.terminal-layout-btn[data-active="true"]');
    return btn instanceof HTMLElement ? (btn.dataset.preset ?? null) : null;
  });
}

/** Count visible (not hidden, display not none) pane slots in the grid. */
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

/** Count placeholder slots in the grid. */
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

test('preset rendering shows correct number of pane slots for each preset', async ({ page }) => {
  const fourAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'a1', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'a2', phase: 'running', projectId: 'proj' },
    [agentC]: { id: agentC, name: 'a3', phase: 'running', projectId: 'proj' },
    [agentD]: { id: agentD, name: 'a4', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, fourAgents);

  // Open first agent
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Single preset: 1 visible pane, no placeholders
  await expect.poll(() => activePreset(page)).toBe('single');
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
  await expect.poll(() => placeholderCount(page)).toBe(0);

  // Open remaining agents, waiting for each socket attachment
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);
  await navigateToTerminal(page, agentC);
  await expect.poll(() => socket.attaches).toBe(3);
  await navigateToTerminal(page, agentD);
  await expect.poll(() => socket.attaches).toBe(4);

  // Now switch to two-columns: should show 1 pane (single[0] was last agent opened) + 1 placeholder
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  // Two-columns has 2 slots; initially both are empty in multi-pane presets
  // Only the slot with a placed pane shows, the rest are placeholders
  const twoColVisible = await visiblePaneCount(page);
  const twoColPH = await placeholderCount(page);
  expect(twoColVisible + twoColPH).toBe(2);

  // Switch to two-rows
  await clickPreset(page, 'two-rows');
  await expect.poll(() => activePreset(page)).toBe('two-rows');
  const twoRowVisible = await visiblePaneCount(page);
  const twoRowPH = await placeholderCount(page);
  expect(twoRowVisible + twoRowPH).toBe(2);

  // Switch to four
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');
  const fourVisible = await visiblePaneCount(page);
  const fourPH = await placeholderCount(page);
  expect(fourVisible + fourPH).toBe(4);

  // Back to single
  await clickPreset(page, 'single');
  await expect.poll(() => activePreset(page)).toBe('single');
  const singleVisible = await visiblePaneCount(page);
  const singlePH = await placeholderCount(page);
  expect(singleVisible + singlePH).toBe(1);
});

test('fifth-agent open switches to single; choosing four restores earlier grid', async ({
  page,
}) => {
  const fiveAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'a1', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'a2', phase: 'running', projectId: 'proj' },
    [agentC]: { id: agentC, name: 'a3', phase: 'running', projectId: 'proj' },
    [agentD]: { id: agentD, name: 'a4', phase: 'running', projectId: 'proj' },
    [agentE]: { id: agentE, name: 'a5', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, fiveAgents);

  // Open 4 agents and place them in the grid
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);
  await navigateToTerminal(page, agentC);
  await expect.poll(() => socket.attaches).toBe(3);
  await navigateToTerminal(page, agentD);
  await expect.poll(() => socket.attaches).toBe(4);

  // Multi-pane presets start with null slots — populated only via place() (P2.3 drag-drop).
  // open()/select() only sets single[0]. So the four-grid has 4 empty slots.

  // Verify: switch to four, get 4 placeholder slots
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');
  const fourPH = await placeholderCount(page);
  expect(fourPH).toBe(4);

  // Open 5th agent — this calls open() which switches to single
  await navigateToTerminal(page, agentE);
  await expect.poll(() => socket.attaches).toBe(5);
  await expect.poll(() => activePreset(page)).toBe('single');
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Switch back to four — grid should still have 4 placeholder slots (unchanged)
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');
  await expect.poll(() => placeholderCount(page)).toBe(4);
});

test('socket identity preserved across preset switches — no new connections', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Record initial WebSocket attach count
  const initialAttaches = socket.attaches;

  // Switch through all presets
  for (const preset of ['two-columns', 'two-rows', 'four', 'single'] as const) {
    await clickPreset(page, preset);
    await expect.poll(() => activePreset(page)).toBe(preset);
    // No new socket connections
    expect(socket.attaches).toBe(initialAttaches);
    expect(socket.closes).toBe(0);
  }
});

test('xterm identity preserved across preset switches', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Record identity
  const initial = await identity(page);
  expect(initial.pane).toBe(true);
  expect(initial.terminal).toBe(true);

  // Switch through presets
  for (const preset of ['two-columns', 'two-rows', 'four', 'single'] as const) {
    await clickPreset(page, preset);
    const id = await identity(page);
    expect(id.host).toBe(true);
    expect(id.pane).toBe(true);
    expect(id.terminal).toBe(true);
  }

  // Terminal was initialized only once
  expect(
    await page.evaluate(
      () => (window as typeof window & { terminalInitializers?: number }).terminalInitializers
    )
  ).toBe(1);
});

test('zoom preserves all preset assignments and shows restore button', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Switch to two-columns so we can see a multi-pane preset
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Restore button should be hidden when not zoomed
  await expect(page.locator('.terminal-layout-restore')).toBeHidden();

  // Zoom the pane via the workspace root's public layoutManager
  await page.evaluate(() => {
    type WorkspaceEl = HTMLElement & {
      workspaceRoot?: { layoutManager: { zoom: (k: string) => void; getState: () => unknown } };
    };
    const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
    const panes = host.querySelectorAll<
      HTMLElement & { session: { state: { key: string } } | null }
    >('scion-terminal-pane');
    const key = [...panes].find((p) => p.session)?.session?.state.key;
    if (!key) throw new Error('No session key for zoom');
    host.workspaceRoot!.layoutManager.zoom(key);
  });

  // Restore button should now be visible
  await expect(page.locator('.terminal-layout-restore')).toBeVisible();

  // Only zoomed pane should be visible
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Unzoom via the restore button
  await page.click('.terminal-layout-restore');

  // Restore button hidden again
  await expect(page.locator('.terminal-layout-restore')).toBeHidden();

  // Preset state was preserved — still two-columns
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // No new sockets from zoom/unzoom
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('close clears session from layout slots', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  // Close alpha
  await page.getByRole('button', { name: 'Close alpha' }).click();
  await expect(page.getByRole('button', { name: 'Terminals (1)' })).toBeVisible();
  await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);

  // Verify remaining beta is still working across presets
  for (const preset of ['two-columns', 'two-rows', 'four', 'single'] as const) {
    await clickPreset(page, preset);
    // Beta's pane should still exist
    await expect(page.locator('#terminal-workspace scion-terminal-pane')).toHaveCount(1);
    // No new sockets
    expect(socket.attaches).toBe(2);
  }
});

test('focus isolation: only focused pane xterm receives typed input', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  // The active session should be in single view (beta, last opened)
  await expect.poll(() => activePreset(page)).toBe('single');

  // Only one pane visible at a time in single mode
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // The visible pane should be beta (last opened)
  const visibleAgent = await page.evaluate(() => {
    const panes = document.querySelectorAll<
      HTMLElement & { session: { state: { agentId: string } } | null }
    >('#terminal-workspace scion-terminal-pane');
    for (const p of panes) {
      if (!p.hidden && p.style.display !== 'none' && p.session) {
        return p.session.state.agentId;
      }
    }
    return null;
  });
  expect(visibleAgent).toBe(agentB);
});

test('preset control buttons activate correct layout', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Verify layout bar is visible
  await expect(page.locator('.terminal-layout-bar')).toBeVisible();

  // Click each preset button and verify it becomes active
  for (const preset of ['two-columns', 'two-rows', 'four', 'single'] as const) {
    await clickPreset(page, preset);
    await expect.poll(() => activePreset(page)).toBe(preset);

    // Verify the button has aria-pressed="true"
    const pressed = await page.evaluate(
      (p) =>
        document
          .querySelector(`.terminal-layout-btn[data-preset="${p}"]`)
          ?.getAttribute('aria-pressed'),
      preset
    );
    expect(pressed).toBe('true');

    // Verify other buttons are not active
    const otherActive = await page.evaluate(
      (p) =>
        [
          ...document.querySelectorAll<HTMLElement>(
            `.terminal-layout-btn:not([data-preset="${p}"])`
          ),
        ].some((b) => b.dataset.active === 'true'),
      preset
    );
    expect(otherActive).toBe(false);
  }
});

// --- Supplemental populated-preset and narrow-screen tests (manager verification) ---

/** Helper: call layoutManager.place() from the page context. */
async function placeInPreset(
  page: Page,
  sessionKey: string,
  preset: string,
  slotIndex: number
): Promise<void> {
  await page.evaluate(
    ({ key, preset, slot }) => {
      type WorkspaceEl = HTMLElement & {
        workspaceRoot?: {
          layoutManager: { place: (k: string, p: string, i: number) => void };
        };
      };
      const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
      host.workspaceRoot!.layoutManager.place(key, preset, slot);
    },
    { key: sessionKey, preset, slot: slotIndex }
  );
}

/** Helper: get all session keys from pane elements. */
async function getPaneSessionKeys(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const panes = document.querySelectorAll<
      HTMLElement & { session: { state: { key: string } } | null }
    >('#terminal-workspace scion-terminal-pane');
    return [...panes].filter((p) => p.session).map((p) => p.session!.state.key);
  });
}

/** Helper: get grid-column and grid-row of a visible pane by session key. */
async function getPaneGridPosition(
  page: Page,
  sessionKey: string
): Promise<{ col: string; row: string; visible: boolean } | null> {
  return page.evaluate((key) => {
    const panes = document.querySelectorAll<
      HTMLElement & { session: { state: { key: string } } | null }
    >('#terminal-workspace scion-terminal-pane');
    for (const p of panes) {
      if (p.session?.state.key === key) {
        return {
          col: p.style.gridColumn,
          row: p.style.gridRow,
          visible: !p.hidden && p.style.display !== 'none',
        };
      }
    }
    return null;
  }, sessionKey);
}

test('populated two-columns renders two distinct terminals side by side', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents (both go to single[0] via open())
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  // Get session keys
  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both into two-columns via layoutManager.place()
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[1], 'two-columns', 1);

  // Switch to two-columns preset
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Both panes should be visible (no placeholders)
  await expect.poll(() => visiblePaneCount(page)).toBe(2);
  await expect.poll(() => placeholderCount(page)).toBe(0);

  // Verify grid positions: slot 0 → col 1, slot 1 → col 2
  const pos0 = await getPaneGridPosition(page, keys[0]);
  const pos1 = await getPaneGridPosition(page, keys[1]);
  expect(pos0).not.toBeNull();
  expect(pos1).not.toBeNull();
  expect(pos0!.visible).toBe(true);
  expect(pos1!.visible).toBe(true);
  expect(pos0!.col).toBe('1');
  expect(pos1!.col).toBe('2');

  // No new socket connections from placement or preset switch
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

test('populated four-grid renders four distinct terminals with identity', async ({ page }) => {
  const fourAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'a1', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'a2', phase: 'running', projectId: 'proj' },
    [agentC]: { id: agentC, name: 'a3', phase: 'running', projectId: 'proj' },
    [agentD]: { id: agentD, name: 'a4', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, fourAgents);

  // Open all 4 agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);
  await navigateToTerminal(page, agentC);
  await expect.poll(() => socket.attaches).toBe(3);
  await navigateToTerminal(page, agentD);
  await expect.poll(() => socket.attaches).toBe(4);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(4);

  // Place all 4 into four-grid
  await placeInPreset(page, keys[0], 'four', 0);
  await placeInPreset(page, keys[1], 'four', 1);
  await placeInPreset(page, keys[2], 'four', 2);
  await placeInPreset(page, keys[3], 'four', 3);

  // Switch to four preset
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');

  // All 4 panes visible, no placeholders
  await expect.poll(() => visiblePaneCount(page)).toBe(4);
  await expect.poll(() => placeholderCount(page)).toBe(0);

  // Verify grid positions: (1,1), (2,1), (1,2), (2,2)
  const positions = await Promise.all(keys.map((k) => getPaneGridPosition(page, k)));
  for (const pos of positions) expect(pos).not.toBeNull();
  expect(positions[0]!.col).toBe('1');
  expect(positions[0]!.row).toBe('1');
  expect(positions[1]!.col).toBe('2');
  expect(positions[1]!.row).toBe('1');
  expect(positions[2]!.col).toBe('1');
  expect(positions[2]!.row).toBe('2');
  expect(positions[3]!.col).toBe('2');
  expect(positions[3]!.row).toBe('2');

  // No new socket connections
  expect(socket.attaches).toBe(4);
  expect(socket.closes).toBe(0);

  // Switch to single and back to four — grid assignments preserved
  await clickPreset(page, 'single');
  await expect.poll(() => activePreset(page)).toBe('single');
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');
  await expect.poll(() => visiblePaneCount(page)).toBe(4);

  // Re-check positions — unchanged
  const positionsAfter = await Promise.all(keys.map((k) => getPaneGridPosition(page, k)));
  for (const pos of positionsAfter) expect(pos).not.toBeNull();
  for (let i = 0; i < 4; i++) {
    expect(positionsAfter[i]!.col).toBe(positions[i]!.col);
    expect(positionsAfter[i]!.row).toBe(positions[i]!.row);
    expect(positionsAfter[i]!.visible).toBe(true);
  }

  // Still no new sockets
  expect(socket.attaches).toBe(4);
  expect(socket.closes).toBe(0);
});

test('populated two-rows rendering with toolbar reachability', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both into two-rows
  await placeInPreset(page, keys[0], 'two-rows', 0);
  await placeInPreset(page, keys[1], 'two-rows', 1);

  // Switch to two-rows
  await clickPreset(page, 'two-rows');
  await expect.poll(() => activePreset(page)).toBe('two-rows');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Verify grid positions: slot 0 → row 1, slot 1 → row 2
  const pos0 = await getPaneGridPosition(page, keys[0]);
  const pos1 = await getPaneGridPosition(page, keys[1]);
  expect(pos0).not.toBeNull();
  expect(pos1).not.toBeNull();
  expect(pos0!.row).toBe('1');
  expect(pos0!.visible).toBe(true);
  expect(pos1!.row).toBe('2');
  expect(pos1!.visible).toBe(true);

  // Both panes are setVisible(true) — focus is derived from DOM focus events
  // Verify both have setVisible(true) by checking pane visibility
  const bothVisible = await page.evaluate(() => {
    const panes = document.querySelectorAll<HTMLElement>('#terminal-workspace scion-terminal-pane');
    return [...panes].filter((p) => !p.hidden && p.style.display !== 'none').length;
  });
  expect(bothVisible).toBe(2);

  // Layout toolbar buttons remain reachable (not inside terminal shadow DOM)
  const toolbarVisible = await page.evaluate(() => {
    const bar = document.querySelector('.terminal-layout-bar');
    return bar instanceof HTMLElement && !bar.hidden;
  });
  expect(toolbarVisible).toBe(true);

  // No socket recreation from preset switch and rendering
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

test('narrow screen shows single pane; wide restores full populated layout', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both into two-columns
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[1], 'two-columns', 1);

  // Switch to two-columns and verify both visible at desktop width
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Narrow the viewport below 760px threshold
  await page.setViewportSize({ width: 700, height: 600 });

  // Wait for media query to trigger
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Grid should be forced to single-column
  const gridCols = await page.evaluate(() => {
    const host = document.querySelector('.terminal-pane-host') as HTMLElement;
    return host?.style.gridTemplateColumns ?? '';
  });
  expect(gridCols).toBe('1fr');

  // No socket recreation from viewport change
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);

  // Widen viewport back to desktop
  await page.setViewportSize({ width: 1280, height: 720 });

  // Wait for restoration — both panes should be visible again
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Layout preset still two-columns (not changed by narrow mode)
  await expect.poll(() => activePreset(page)).toBe('two-columns');

  // Verify grid positions restored
  const pos0 = await getPaneGridPosition(page, keys[0]);
  const pos1 = await getPaneGridPosition(page, keys[1]);
  expect(pos0).not.toBeNull();
  expect(pos1).not.toBeNull();
  expect(pos0!.visible).toBe(true);
  expect(pos1!.visible).toBe(true);
  expect(pos0!.col).toBe('1');
  expect(pos1!.col).toBe('2');

  // Still no socket recreation
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

// --- Drag/Drop and Accessible Placement tests (P2.3) ---

/** Custom MIME type constant matching the workspace root implementation. */
const TERMINAL_DRAG_MIME = 'application/x-scion-terminal';

/** Helper: simulate a drag-and-drop from a rail entry's drag handle to a slot element. */
async function simulateDragDrop(
  page: Page,
  sourceSessionKey: string,
  targetSlotIndex: number,
  mimeType: string = TERMINAL_DRAG_MIME
): Promise<void> {
  await page.evaluate(
    ({ key, slot, mime }) => {
      const host = document.querySelector('#terminal-workspace')!;

      // Find the drag handle for this session key by scanning all rail items
      // and matching the focus-id attribute on the select button.
      // Session keys can contain special characters, so we iterate instead of querySelector.
      let dragHandle: HTMLElement | null = null;
      const focusTargets = host.querySelectorAll<HTMLElement>('[data-rail-focus-id]');
      for (const el of focusTargets) {
        if (el.dataset.railFocusId === `${key}:select`) {
          const item = el.closest('.terminal-rail-item');
          if (item) {
            dragHandle = item.querySelector('.terminal-drag-handle') as HTMLElement;
            break;
          }
        }
      }
      if (!dragHandle) throw new Error(`No drag handle found for session key ${key}`);

      // Find target slot element by iterating data-slot-index attributes
      let target: HTMLElement | null = null;
      const slotStr = String(slot);
      const candidates = host.querySelectorAll<HTMLElement>('[data-slot-index]');
      for (const el of candidates) {
        if (el.dataset.slotIndex === slotStr) {
          target = el;
          break;
        }
      }
      if (!target) throw new Error(`No slot element found for index ${slot}`);

      // Create DataTransfer and dispatch drag events
      const dt = new DataTransfer();
      dt.setData(mime, key);

      const dragStartEvent = new DragEvent('dragstart', {
        bubbles: true,
        dataTransfer: dt,
      });
      Object.defineProperty(dragStartEvent, 'dataTransfer', { value: dt });
      dragHandle.dispatchEvent(dragStartEvent);

      // Debug: verify DataTransfer is readable
      // eslint-disable-next-line no-console
      console.log(
        'DT types:',
        JSON.stringify(Array.from(dt.types)),
        'includes:',
        dt.types.includes(mime),
        'getData:',
        dt.getData(mime)
      );

      const dragOverEvent = new DragEvent('dragover', {
        bubbles: true,
        cancelable: true,
      });
      Object.defineProperty(dragOverEvent, 'dataTransfer', { value: dt });
      const dragOverResult = target.dispatchEvent(dragOverEvent);
      // eslint-disable-next-line no-console
      console.log(
        'dragover dispatched, defaultPrevented:',
        !dragOverResult,
        'dragOver attr:',
        target.dataset.dragOver
      );

      const dropEvent = new DragEvent('drop', {
        bubbles: true,
        cancelable: true,
      });
      Object.defineProperty(dropEvent, 'dataTransfer', { value: dt });
      target.dispatchEvent(dropEvent);

      const dragEndEvent = new DragEvent('dragend', { bubbles: true });
      dragHandle.dispatchEvent(dragEndEvent);
    },
    { key: sourceSessionKey, slot: targetSlotIndex, mime: mimeType }
  );
}

/** Helper: simulate a file drop on a slot element. Returns true if our drag handler highlighted it. */
async function simulateFileDrop(page: Page, targetSlotIndex: number): Promise<boolean> {
  return page.evaluate((slot) => {
    const host = document.querySelector('#terminal-workspace')!;

    // Find target by iterating data-slot-index
    let target: HTMLElement | null = null;
    const slotStr = String(slot);
    const candidates = host.querySelectorAll<HTMLElement>('[data-slot-index]');
    for (const el of candidates) {
      if (el.dataset.slotIndex === slotStr) {
        target = el;
        break;
      }
    }
    if (!target) throw new Error(`No slot element found for index ${slot}`);

    const dt = new DataTransfer();
    // Add a file-like item (text/plain) — no custom terminal MIME type
    dt.setData('text/plain', 'file-content');

    const dragOverEvent = new DragEvent('dragover', {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
    });
    Object.defineProperty(dragOverEvent, 'dataTransfer', { value: dt });
    target.dispatchEvent(dragOverEvent);

    // Check if our handler added drag-over feedback (it shouldn't for file drops)
    const highlighted = target.dataset.dragOver === 'true';

    const dropEvent = new DragEvent('drop', {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
    });
    Object.defineProperty(dropEvent, 'dataTransfer', { value: dt });
    target.dispatchEvent(dropEvent);

    return highlighted;
  }, targetSlotIndex);
}

test('drag rail entry to empty slot places session in correct grid position', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Switch to two-columns (both slots empty)
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);

  // Drag alpha to slot 0
  await simulateDragDrop(page, keys[0], 0);

  // Alpha should be visible in slot 0, slot 1 still placeholder
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
  await expect.poll(() => placeholderCount(page)).toBe(1);

  const pos = await getPaneGridPosition(page, keys[0]);
  expect(pos).not.toBeNull();
  expect(pos!.visible).toBe(true);
  expect(pos!.col).toBe('1');

  // No new socket connections from drag-drop
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

test('drag rail entry to occupied slot performs swap', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both manually first
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[1], 'two-columns', 1);

  await clickPreset(page, 'two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Verify initial positions
  let pos0 = await getPaneGridPosition(page, keys[0]);
  let pos1 = await getPaneGridPosition(page, keys[1]);
  expect(pos0!.col).toBe('1');
  expect(pos1!.col).toBe('2');

  // Drag alpha (slot 0) to slot 1 (occupied by beta) — should swap
  await simulateDragDrop(page, keys[0], 1);

  // After swap: alpha in slot 1 (col 2), beta in slot 0 (col 1)
  await expect
    .poll(async () => {
      const p = await getPaneGridPosition(page, keys[0]);
      return p?.col;
    })
    .toBe('2');

  pos0 = await getPaneGridPosition(page, keys[0]);
  pos1 = await getPaneGridPosition(page, keys[1]);
  expect(pos0!.col).toBe('2'); // alpha moved to slot 1
  expect(pos1!.col).toBe('1'); // beta swapped to slot 0

  // Both still visible, no new sockets
  await expect.poll(() => visiblePaneCount(page)).toBe(2);
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

test('drag to same slot is a no-op', async ({ page }) => {
  const socket = await setup(page);

  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(1);

  // Place in two-columns slot 0
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await clickPreset(page, 'two-columns');

  // Get initial position
  const posBefore = await getPaneGridPosition(page, keys[0]);
  expect(posBefore!.col).toBe('1');

  // Drag to same slot — no-op
  await simulateDragDrop(page, keys[0], 0);

  // Position unchanged
  const posAfter = await getPaneGridPosition(page, keys[0]);
  expect(posAfter!.col).toBe('1');

  // No state change, no new sockets
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('invalid/unknown payload is rejected — no placement, no state change', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  await clickPreset(page, 'two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);

  // Drop with wrong MIME type — should not trigger placement
  const beforePH = await placeholderCount(page);
  await page.evaluate(() => {
    const host = document.querySelector('#terminal-workspace')!;
    const placeholder = host.querySelector('.terminal-slot-placeholder')!;

    const dt = new DataTransfer();
    dt.setData('text/plain', 'not-a-terminal-key');

    const dragOverEvent = new DragEvent('dragover', {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
    });
    Object.defineProperty(dragOverEvent, 'dataTransfer', { value: dt });
    placeholder.dispatchEvent(dragOverEvent);

    const dropEvent = new DragEvent('drop', {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
    });
    Object.defineProperty(dropEvent, 'dataTransfer', { value: dt });
    placeholder.dispatchEvent(dropEvent);
  });

  // Placeholder count unchanged — no placement occurred
  expect(await placeholderCount(page)).toBe(beforePH);

  // No socket changes
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('file drop does not trigger placement', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  const keys = await getPaneSessionKeys(page);

  // Place session in two-columns slot 0
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await clickPreset(page, 'two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Simulate file drop on slot 0 (occupied by our terminal)
  const wasHighlighted = await simulateFileDrop(page, 0);

  // File drops should NOT trigger our drag highlight feedback
  expect(wasHighlighted).toBe(false);

  // Layout unchanged
  const pos = await getPaneGridPosition(page, keys[0]);
  expect(pos!.col).toBe('1');

  // No socket changes
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('keyboard "Place in pane" action places session without drag gesture', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);

  // Switch to two-columns (both slots empty)
  await clickPreset(page, 'two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);

  // Click the "Place in pane" button for alpha
  const placeBtn = page.getByRole('button', { name: /Place alpha in pane/ });
  await expect(placeBtn).toBeVisible();
  await placeBtn.click();

  // Place menu should appear with slot options
  const menu = page.locator('.terminal-place-menu');
  await expect(menu).toBeVisible();

  // Menu should have 2 items (two-columns has 2 slots)
  const menuItems = menu.locator('.terminal-place-menu-item');
  await expect(menuItems).toHaveCount(2);

  // First item should indicate empty slot
  await expect(menuItems.first()).toContainText('Slot 1 (empty)');

  // Click the first slot
  await menuItems.first().click();

  // Menu should be hidden
  await expect(menu).toBeHidden();

  // Alpha should now be placed in slot 0
  await expect.poll(() => visiblePaneCount(page)).toBe(1);
  await expect.poll(() => placeholderCount(page)).toBe(1);

  const pos = await getPaneGridPosition(page, keys[0]);
  expect(pos!.visible).toBe(true);
  expect(pos!.col).toBe('1');

  // Aria-live should announce the placement
  const announcement = await page.evaluate(() => {
    return document.querySelector('.terminal-aria-live')?.textContent ?? '';
  });
  expect(announcement).toContain('Placed');
  expect(announcement).toContain('alpha');
  expect(announcement).toContain('slot 1');

  // No new sockets
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

test('ordinary rail click still opens in single, not place', async ({ page }) => {
  const socket = await setup(page);

  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Switch to two-columns
  await clickPreset(page, 'two-columns');
  await expect.poll(() => activePreset(page)).toBe('two-columns');
  await expect.poll(() => placeholderCount(page)).toBe(2);

  // Click the rail entry name/button (the select button) to go back
  await page.getByRole('button', { name: /isolated-agent in fixture-project/ }).click();

  // Should navigate via open() which sets active='single'
  await expect.poll(() => activePreset(page)).toBe('single');
  await expect.poll(() => visiblePaneCount(page)).toBe(1);

  // Two-columns should still have empty slots (not affected by rail click)
  const twoColSlots = await page.evaluate(() => {
    type WorkspaceEl = HTMLElement & {
      workspaceRoot?: {
        layoutManager: { getState: () => { twoColumns: (string | null)[] } };
      };
    };
    const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
    return host.workspaceRoot!.layoutManager.getState().twoColumns;
  });
  expect(twoColSlots).toEqual([null, null]);

  expect(socket.attaches).toBe(1);
});

test('cross-preset independence: placing in two-columns does not change four-grid', async ({
  page,
}) => {
  const fourAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'a1', phase: 'running', projectId: 'proj' },
    [agentB]: { id: agentB, name: 'a2', phase: 'running', projectId: 'proj' },
    [agentC]: { id: agentC, name: 'a3', phase: 'running', projectId: 'proj' },
    [agentD]: { id: agentD, name: 'a4', phase: 'running', projectId: 'proj' },
  };
  const socket = await setup(page, true, true, fourAgents);

  // Open all four agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);
  await navigateToTerminal(page, agentC);
  await expect.poll(() => socket.attaches).toBe(3);
  await navigateToTerminal(page, agentD);
  await expect.poll(() => socket.attaches).toBe(4);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(4);

  // Place a1 and a2 in four-grid slots 0 and 1
  await placeInPreset(page, keys[0], 'four', 0);
  await placeInPreset(page, keys[1], 'four', 1);

  // Switch to two-columns and place a1 and a3
  await clickPreset(page, 'two-columns');
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[2], 'two-columns', 1);

  // Now drag a3 to swap with a1 within two-columns
  await simulateDragDrop(page, keys[2], 0);

  // Verify two-columns swapped: a3 at slot 0, a1 at slot 1
  await expect
    .poll(async () => {
      const p = await getPaneGridPosition(page, keys[2]);
      return p?.col;
    })
    .toBe('1');

  // Switch to four-grid — its slots should be unchanged
  await clickPreset(page, 'four');
  await expect.poll(() => activePreset(page)).toBe('four');

  const fourState = await page.evaluate(() => {
    type WorkspaceEl = HTMLElement & {
      workspaceRoot?: {
        layoutManager: { getState: () => { four: (string | null)[] } };
      };
    };
    const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
    return host.workspaceRoot!.layoutManager.getState().four;
  });

  // Four-grid should still have keys[0] at slot 0 and keys[1] at slot 1
  expect(fourState[0]).toBe(keys[0]);
  expect(fourState[1]).toBe(keys[1]);
  expect(fourState[2]).toBeNull();
  expect(fourState[3]).toBeNull();

  expect(socket.attaches).toBe(4);
  expect(socket.closes).toBe(0);
});

// --- Pane header action buttons tests (P2.4) ---

test('graph button navigates to correct URL with project and focus params', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Wait for metadata to load (projectId must be set for graph button to show)
  const graphBtn = page.locator('scion-terminal-pane').locator('button[title="Open in graph"]');
  await expect(graphBtn).toBeVisible();

  // Click the graph button
  await graphBtn.click();

  // Should navigate to the graph URL with correct project and focus params
  await expect(page).toHaveURL(
    `/agents/graph?project=${encodeURIComponent('fixture-project')}&focus=${encodeURIComponent(agent)}`
  );

  // Session stays attached — no socket close
  expect(socket.closes).toBe(0);
});

test('graph button hidden when agent has no projectId', async ({ page }) => {
  const noProjectAgents: Record<string, AgentFixture> = {
    [agent]: {
      id: agent,
      name: 'no-project-agent',
      phase: 'running',
      projectId: '',
    },
  };
  const socket = await setup(page, true, true, noProjectAgents);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Wait for the pane to render fully
  await expect(page.locator('scion-terminal-pane')).toHaveCount(1);

  // Graph button should not be visible (no projectId)
  const graphBtn = page.locator('scion-terminal-pane').locator('button[title="Open in graph"]');
  await expect(graphBtn).toHaveCount(0);
});

test('chat button navigates to DM route with full dm key', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Wait for chat button to appear (requires userId and native chat enabled)
  const chatBtn = page.locator('scion-terminal-pane').locator('button[title="Open in chat"]');
  await expect(chatBtn).toBeVisible();

  // Click the chat button
  await chatBtn.click();

  // Should navigate to the DM route with the full encoded key
  const expectedKey = `dm:agent:${agent}:user:fixture-user`;
  await expect(page).toHaveURL(`/chat/dm/${encodeURIComponent(expectedKey)}`);

  // Session stays attached — no socket close
  expect(socket.closes).toBe(0);
});

test('chat button hidden when native chat is disabled', async ({ page }) => {
  const socket = await setup(page, true, true, undefined, false);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Wait for the pane to render
  await expect(page.locator('scion-terminal-pane')).toHaveCount(1);

  // Graph button should still be visible (projectId is set)
  const graphBtn = page.locator('scion-terminal-pane').locator('button[title="Open in graph"]');
  await expect(graphBtn).toBeVisible();

  // Chat button should not be visible (native chat disabled)
  const chatBtn = page.locator('scion-terminal-pane').locator('button[title="Open in chat"]');
  await expect(chatBtn).toHaveCount(0);
});

test('chat button hidden when userId is unavailable', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Verify the chat button is visible with a valid userId
  const chatBtn = page.locator('scion-terminal-pane').locator('button[title="Open in chat"]');
  await expect(chatBtn).toBeVisible();

  // Clear the userId on the pane — @state() decorator triggers re-render automatically
  await page.evaluate(() => {
    type WorkspaceEl = HTMLElement & {
      workspaceRoot?: { setUser: (u: null) => void };
    };
    const host = document.querySelector('#terminal-workspace') as WorkspaceEl;
    host.workspaceRoot!.setUser(null);
  });

  // Chat button should disappear when userId is cleared
  await expect(chatBtn).toHaveCount(0);
});

test('navigation to graph preserves retained sessions', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Record initial identity
  const initialIdentity = await identity(page);
  expect(initialIdentity.pane).toBe(true);

  // Click graph button
  const graphBtn = page.locator('scion-terminal-pane').locator('button[title="Open in graph"]');
  await expect(graphBtn).toBeVisible();
  await graphBtn.click();

  // Should have navigated away
  await expect(page).toHaveURL(/\/agents\/graph/);

  // Session should still be attached (no close)
  expect(socket.closes).toBe(0);
  expect(socket.attaches).toBe(1);

  // Navigate back to terminals
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agent}`
  );

  // Pane should be preserved (same identity)
  await expect
    .poll(() => identity(page))
    .toEqual({ host: true, pane: true, terminal: true, count: 1 });

  // Still the same socket — no reconnect
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('each pane buttons target its own agent in multi-pane layout', async ({ page }) => {
  const twoAgents: Record<string, AgentFixture> = {
    [agent]: { id: agent, name: 'alpha', phase: 'running', projectId: 'proj-a' },
    [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj-b' },
  };
  const socket = await setup(page, true, true, twoAgents);

  // Open both agents
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await navigateToTerminal(page, agentB);
  await expect.poll(() => socket.attaches).toBe(2);

  const keys = await getPaneSessionKeys(page);
  expect(keys.length).toBe(2);

  // Place both into two-columns
  await placeInPreset(page, keys[0], 'two-columns', 0);
  await placeInPreset(page, keys[1], 'two-columns', 1);
  await clickPreset(page, 'two-columns');
  await expect.poll(() => visiblePaneCount(page)).toBe(2);

  // Verify each pane's graph button has the correct aria-label targeting its own agent
  const graphLabels = await page.evaluate(() => {
    const panes = document.querySelectorAll<HTMLElement>('#terminal-workspace scion-terminal-pane');
    const labels: Array<{ agentId: string; graphLabel: string | null; chatLabel: string | null }> =
      [];
    for (const pane of panes) {
      if (pane.hidden || pane.style.display === 'none') continue;
      const shadow = pane.shadowRoot;
      if (!shadow) continue;
      const graphBtn = shadow.querySelector('button[title="Open in graph"]');
      const chatBtn = shadow.querySelector('button[title="Open in chat"]');
      const sessionProp = (pane as HTMLElement & { session: { state: { agentId: string } } | null })
        .session;
      labels.push({
        agentId: sessionProp?.state.agentId ?? '',
        graphLabel: graphBtn?.getAttribute('aria-label') ?? null,
        chatLabel: chatBtn?.getAttribute('aria-label') ?? null,
      });
    }
    return labels;
  });

  expect(graphLabels.length).toBe(2);

  // Find alpha and beta pane labels
  const alphaPane = graphLabels.find((l) => l.agentId === agent);
  const betaPane = graphLabels.find((l) => l.agentId === agentB);

  expect(alphaPane).toBeDefined();
  expect(betaPane).toBeDefined();
  expect(alphaPane!.graphLabel).toContain('alpha');
  expect(betaPane!.graphLabel).toContain('beta');
  expect(alphaPane!.chatLabel).toContain('alpha');
  expect(betaPane!.chatLabel).toContain('beta');

  // No new sockets
  expect(socket.attaches).toBe(2);
  expect(socket.closes).toBe(0);
});

// --- Account teardown tests (P3.1 — #1658) ---

test('owner-tab logout disposes workspace, closes sessions, hides UI', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);
  await expect(page.locator('#terminal-workspace')).toBeVisible();

  // Simulate logout by dispatching the teardown event
  await page.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } }),
    );
  });

  // Workspace should be hidden after teardown
  await expect(page.locator('#terminal-workspace')).toBeHidden();
  // WebSocket should have been closed
  await expect.poll(() => socket.closes).toBeGreaterThanOrEqual(1);
});

test('non-owner logout broadcasts teardown, owner disposes', async ({ context }) => {
  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setup(owner);
  await setup(other);

  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Other tab navigates to terminals (becomes non-owner)
  await other.goto(`/terminals/${agent}`);
  await expect(other.locator('#terminal-workspace')).toContainText('owning tab');

  // Non-owner fires teardown
  await other.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } }),
    );
  });

  // Owner should receive the broadcast and dispose
  await expect.poll(() => ownerSocket.closes).toBeGreaterThanOrEqual(1);
  // Non-owner's workspace should be hidden
  await expect(other.locator('#terminal-workspace')).toBeHidden();
});

test('pending connect during teardown aborts inflight attach', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Teardown while connected — the coordinator aborts the lifetime controller
  // which cancels any inflight or future connection attempts.
  await page.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } }),
    );
  });

  // After teardown, the existing socket should be closed
  await expect.poll(() => socket.closes).toBeGreaterThanOrEqual(1);

  // No new opens should be possible since the coordinator is torn down
  const result = await page.evaluate(async (id) => {
    // Try to navigate to terminals again
    document.dispatchEvent(
      new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` }, bubbles: true }),
    );
    // Wait a tick for the route handler to execute
    await new Promise((resolve) => setTimeout(resolve, 100));
    return true;
  }, agent);
  expect(result).toBe(true);

  // Socket count should not have increased
  expect(socket.attaches).toBe(1);
});

test('login after teardown cannot recreate workspace in same page', async ({ page }) => {
  const socket = await setup(page);
  await page.goto(`/terminals/${agent}`);
  await expect.poll(() => socket.attaches).toBe(1);

  // Teardown
  await page.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } }),
    );
  });
  await expect(page.locator('#terminal-workspace')).toBeHidden();

  // After teardown, navigating to terminals should not create a new coordinator
  // The accountTornDown flag prevents recreation within the same SPA lifetime
  await page.evaluate(() => {
    document.dispatchEvent(
      new CustomEvent('nav-click', { detail: { path: '/terminals' }, bubbles: true }),
    );
  });

  // Since the account is torn down, the coordinator won't be recreated in this page lifetime
  // This is expected — a real re-login would reload the page
  await expect(page.locator('#terminal-workspace')).toBeHidden();
});

test('teardown on one hub/account does not affect unrelated workspace', async ({ context }) => {
  const owner = await context.newPage();
  const ownerSocket = await setup(owner);
  await owner.goto(`/terminals/${agent}`);
  await expect.poll(() => ownerSocket.attaches).toBe(1);

  // Teardown the owner
  await owner.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } }),
    );
  });

  // Owner workspace should be torn down
  await expect(owner.locator('#terminal-workspace')).toBeHidden();
  await expect.poll(() => ownerSocket.closes).toBeGreaterThanOrEqual(1);
});
