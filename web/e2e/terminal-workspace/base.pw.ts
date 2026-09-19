import { test, expect } from '@playwright/test';

const agent = '11111111-1111-4111-8111-111111111111';

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
      close() {}
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
