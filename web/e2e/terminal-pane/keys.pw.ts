// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Touch key bar on the production pane with real xterm, in phone emulation.
import { expect, test, type Page, type WebSocketRoute } from '@playwright/test';
import type {} from './fixture.js';
import { installEventSourceStub } from '../event-source-stub.js';

const agentId = '11111111-1111-4111-8111-111111111111';

async function setup(
  page: Page,
  options: { keyBar?: boolean } = {}
): Promise<{ input: string[]; write(text: string): void }> {
  const { keyBar = true } = options;
  const input: string[] = [];
  let peer!: WebSocketRoute;
  await page.addInitScript((enabled: boolean) => {
    window.__SCION_FEATURES__ = { 'web.terminal_key_bar': enabled };
  }, keyBar);
  await installEventSourceStub(page);
  await page.route('**/api/v1/**', (route) =>
    route.fulfill({
      json: route.request().url().endsWith(`/agents/${agentId}`)
        ? { id: agentId, name: 'fixture-agent', phase: 'running', projectId: 'fixture-project' }
        : {},
    })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    peer = socket;
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type: string; data?: string };
      if (frame.type === 'data') input.push(Buffer.from(frame.data!, 'base64').toString());
    });
    // The session counts as connected on the first data frame (tmux's redraw).
    socket.send(JSON.stringify({ type: 'data', data: Buffer.from('$ ').toString('base64') }));
  });
  await page.goto('/e2e/terminal-pane/fixture.html');
  await expect
    .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
    .toBe('connected');
  return {
    input,
    write(text: string): void {
      peer.send(JSON.stringify({ type: 'data', data: Buffer.from(text).toString('base64') }));
    },
  };
}

/** The deepest focused element's class, through the pane's shadow root. */
function focusedClass(page: Page): Promise<string> {
  return page.evaluate(() => {
    let active = document.activeElement;
    while (active?.shadowRoot?.activeElement) active = active.shadowRoot.activeElement;
    return active?.className ?? '';
  });
}

test.describe('phone', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test('key bar shows at the pane bottom with 44px targets and does not take focus', async ({
    page,
  }) => {
    const peer = await setup(page);
    const bar = page.getByRole('group', { name: 'Terminal keys' });
    await expect(bar).toBeVisible();
    const barBox = (await bar.boundingBox())!;
    expect(barBox.y + barBox.height).toBeLessThanOrEqual(844);
    expect(barBox.y + barBox.height).toBeGreaterThan(800);
    for (const name of ['Esc (Escape)', 'Tab', 'Ctrl (Control)', 'Up arrow']) {
      const box = (await bar.getByRole('button', { name, exact: true }).boundingBox())!;
      expect(box.width).toBeGreaterThanOrEqual(44);
      expect(box.height).toBeGreaterThanOrEqual(44);
    }

    await page.locator('.xterm-helper-textarea').focus();
    expect(await focusedClass(page)).toContain('xterm-helper-textarea');
    await bar.getByRole('button', { name: 'Esc (Escape)' }).tap();
    await bar.getByRole('button', { name: 'Ctrl (Control)', exact: true }).tap();
    expect(await focusedClass(page)).toContain('xterm-helper-textarea');
    await page.keyboard.type('c');
    await expect.poll(() => peer.input).toEqual(['\x1b', '\x03']);
    expect(await focusedClass(page)).toContain('xterm-helper-textarea');

    // Nothing focused (the user closed the keyboard): a tap sends the key
    // and leaves focus alone, so the keyboard stays closed.
    await page.evaluate(() => {
      const pane = window.paneFixture.pane;
      (pane.shadowRoot?.activeElement as HTMLElement | null)?.blur();
    });
    expect(await focusedClass(page)).not.toContain('xterm-helper-textarea');
    await bar.getByRole('button', { name: 'Tab', exact: true }).tap();
    expect(await focusedClass(page)).not.toContain('xterm-helper-textarea');

    // Focus in another element (another pane, an input): a tap brings it here.
    await page.evaluate(() => {
      const input = document.createElement('input');
      input.className = 'outside';
      document.body.prepend(input);
      input.focus();
    });
    expect(await focusedClass(page)).toBe('outside');
    await bar.getByRole('button', { name: 'Tab', exact: true }).tap();
    expect(await focusedClass(page)).toContain('xterm-helper-textarea');
    await expect.poll(() => peer.input).toEqual(['\x1b', '\x03', '\t', '\t']);
  });

  test('a multi-character input after Ctrl uses it up instead of hitting the next key', async ({
    page,
  }) => {
    const peer = await setup(page);
    await page.locator('.xterm-helper-textarea').focus();
    await page.getByRole('button', { name: 'Ctrl (Control)', exact: true }).tap();
    await page.keyboard.insertText('ls');
    await page.keyboard.type(' ');
    await expect.poll(() => peer.input).toEqual(['ls', ' ']);
  });

  test('a bracketed paste after Ctrl uses it up too', async ({ page }) => {
    const peer = await setup(page);
    peer.write('\x1b[?2004h');
    await expect
      .poll(() => page.evaluate(() => window.paneFixture.terminal().modes.bracketedPasteMode))
      .toBe(true);
    await page.locator('.xterm-helper-textarea').focus();
    await page.getByRole('button', { name: 'Ctrl (Control)', exact: true }).tap();
    await page.evaluate(() => window.paneFixture.terminal().paste('ls'));
    await page.keyboard.type(' ');
    await expect.poll(() => peer.input).toEqual(['\x1b[200~ls\x1b[201~', ' ']);
  });

  test('arrows follow the application cursor mode set by the remote program', async ({ page }) => {
    const peer = await setup(page);
    const up = page.getByRole('button', { name: 'Up arrow' });
    await up.tap();
    peer.write('\x1b[?1h');
    await expect
      .poll(() =>
        page.evaluate(() => window.paneFixture.terminal().modes.applicationCursorKeysMode)
      )
      .toBe(true);
    await up.tap();
    peer.write('\x1b[?1l');
    await expect
      .poll(() =>
        page.evaluate(() => window.paneFixture.terminal().modes.applicationCursorKeysMode)
      )
      .toBe(false);
    await up.tap();
    await expect.poll(() => peer.input).toEqual(['\x1b[A', '\x1bOA', '\x1b[A']);
  });

  test('the in-bar toggle collapses the keys into a 44px chip and is remembered', async ({
    page,
  }) => {
    await setup(page);
    await expect(page.locator('.toolbar .key-bar-toggle')).toHaveCount(0);
    const hideBtn = page.getByRole('button', { name: 'Hide terminal keys' });
    await expect(hideBtn).toBeVisible();
    await hideBtn.tap();
    await expect(page.getByRole('group', { name: 'Terminal keys' })).toHaveCount(0);
    const showBtn = page.getByRole('button', { name: 'Show terminal keys' });
    await expect(showBtn).toBeVisible();
    const chipBox = (await showBtn.boundingBox())!;
    expect(chipBox.width).toBeGreaterThanOrEqual(44);
    expect(chipBox.height).toBeGreaterThanOrEqual(44);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Show terminal keys' })).toBeVisible();
    await expect(page.getByRole('group', { name: 'Terminal keys' })).toHaveCount(0);
    await page.getByRole('button', { name: 'Show terminal keys' }).tap();
    await expect(page.getByRole('group', { name: 'Terminal keys' })).toBeVisible();
  });

  test('is absent when the web.terminal_key_bar experiment is off', async ({ page }) => {
    await setup(page, { keyBar: false });
    await expect(page.getByRole('group', { name: 'Terminal keys' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: /terminal keys/i })).toHaveCount(0);
  });

  test('composes with touch scroll: vertical drag scrolls with bar present and Esc exits copy-mode', async ({
    page,
  }) => {
    const peer = await setup(page);
    const bar = page.getByRole('group', { name: 'Terminal keys' });
    await expect(bar).toBeVisible();

    // Enable tmux-style SGR mouse reporting.
    peer.write('\x1b[?1000h\x1b[?1006h');
    await expect
      .poll(() => page.evaluate(() => window.paneFixture.terminal().modes.mouseTrackingMode))
      .toBe('vt200');

    const cell = await page.evaluate(() => {
      const term = window.paneFixture.terminal();
      const screen = term.element!.querySelector('.xterm-screen')!.getBoundingClientRect();
      return {
        left: screen.left,
        top: screen.top,
        w: screen.width / term.cols,
        h: screen.height / term.rows,
      };
    });
    const x = cell.left + 4.5 * cell.w;
    const fromY = cell.top + 2.5 * cell.h;
    const toY = cell.top + 6.8 * cell.h;

    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Input.dispatchTouchEvent', {
      type: 'touchStart',
      touchPoints: [{ x, y: fromY }],
    });
    for (let step = 1; step <= 10; step++) {
      await cdp.send('Input.dispatchTouchEvent', {
        type: 'touchMove',
        touchPoints: [{ x, y: fromY + ((toY - fromY) * step) / 10 }],
      });
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await cdp.detach();

    // Vertical drag produced SGR wheel-up reports while the key bar is mounted.
    await expect.poll(() => peer.input.length).toBeGreaterThanOrEqual(3);
    expect(peer.input.every((r) => r.startsWith('\x1b[<64;'))).toBe(true);

    // Tapping Esc on the key bar sends ESC to return tmux copy-mode to live.
    const beforeEsc = peer.input.length;
    await bar.getByRole('button', { name: 'Esc (Escape)' }).tap();
    await expect.poll(() => peer.input.slice(beforeEsc)).toEqual(['\x1b']);
  });
});

test('desktop pointer shows no key bar and no toggle', async ({ page }) => {
  await setup(page);
  await expect(page.getByRole('button', { name: 'Agent window' })).toBeVisible();
  await expect(page.getByRole('group', { name: 'Terminal keys' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /terminal keys/i })).toHaveCount(0);
});
