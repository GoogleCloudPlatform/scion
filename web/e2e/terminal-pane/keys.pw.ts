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

const agentId = '11111111-1111-4111-8111-111111111111';

async function setup(page: Page): Promise<{ input: string[]; write(text: string): void }> {
  const input: string[] = [];
  let peer!: WebSocketRoute;
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });
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
    const bar = page.getByRole('toolbar', { name: 'Terminal keys' });
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

  test('the toolbar toggle hides the bar, gives the rows back and is remembered', async ({
    page,
  }) => {
    await setup(page);
    const rows = (): Promise<number> => page.evaluate(() => window.paneFixture.terminal().rows);
    const withBar = await rows();
    await page.getByRole('button', { name: 'Terminal keys' }).tap();
    await expect(page.getByRole('toolbar', { name: 'Terminal keys' })).toBeHidden();
    await expect.poll(rows).toBeGreaterThan(withBar);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Terminal keys' })).toBeVisible();
    await expect(page.getByRole('toolbar', { name: 'Terminal keys' })).toBeHidden();
  });
});

test('desktop pointer shows no key bar and no toggle', async ({ page }) => {
  await setup(page);
  await expect(page.getByRole('button', { name: 'Agent window' })).toBeVisible();
  await expect(page.getByRole('toolbar', { name: 'Terminal keys' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Terminal keys' })).toHaveCount(0);
});
