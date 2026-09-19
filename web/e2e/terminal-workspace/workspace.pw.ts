import { test, expect, type Page } from '@playwright/test';

const agent = '11111111-1111-4111-8111-111111111111';

async function setup(
  page: Page,
  enabled = true,
  locks = true
): Promise<{ readonly attaches: number; readonly closes: number; sent: string[] }> {
  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];
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
  await page.route('**/api/v1/agents/**', (route) =>
    route.fulfill({
      json: route.request().url().endsWith('/pty')
        ? {}
        : {
            id: agent,
            name: 'isolated-agent',
            phase: 'running',
            projectId: 'fixture-project',
          },
    })
  );
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
