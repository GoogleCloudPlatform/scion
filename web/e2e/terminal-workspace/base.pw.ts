import { test, expect } from '@playwright/test';

const agent = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';

test('direct legacy load and history retain the deployment base', async ({ page }) => {
  let attaches = 0;
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
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/agents/**', (route) =>
    route.fulfill({
      json: route.request().url().endsWith('/pty')
        ? {}
        : { id: agent, name: 'isolated-agent', phase: 'running' },
    })
  );
  await page.routeWebSocket('**/pty?*', () => {
    attaches++;
  });
  await page.goto(`/tw/agents/${agent}/terminal`);
  await expect(page).toHaveURL(`/tw/terminals/${agent}`);
  await expect.poll(() => attaches).toBe(1);
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: '/' } }))
  );
  await expect(page).toHaveURL('/tw/');
  await page.goBack();
  await expect(page).toHaveURL(`/tw/terminals/${agent}`);
  expect(attaches).toBe(1);
});

// The restore's onRestoredSelection replaceState must keep the deployment
// base path, the same as the legacy rewrite case above.
test('bare /terminals restore keeps the deployment base in the URL', async ({ page }) => {
  let attaches = 0;
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
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  const fixtures: Record<string, { id: string; name: string; phase: string }> = {
    [agent]: { id: agent, name: 'isolated-agent', phase: 'running' },
    [agentB]: { id: agentB, name: 'second-agent', phase: 'running' },
  };
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id = route
      .request()
      .url()
      .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1];
    void route.fulfill({
      status: id && fixtures[id] ? 200 : 404,
      json: (id && fixtures[id]) ?? { error: 'not found' },
    });
  });
  await page.route('**/api/v1/users/me/terminal-workspace', (route) => {
    if (route.request().method() !== 'GET') {
      void route.fulfill({ status: 405, json: {} });
      return;
    }
    void route.fulfill({
      json: {
        agentIds: [agent, agentB],
        frontmostAgentId: agentB,
        revision: 3,
        updatedAt: new Date().toISOString(),
        pruned: 0,
      },
    });
  });
  await page.routeWebSocket('**/pty?*', () => {
    attaches++;
  });

  await page.goto('/tw/terminals');
  await expect(page).toHaveURL(`/tw/terminals/${agentB}`);
  await expect.poll(() => attaches).toBe(1);
});
