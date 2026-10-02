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

/**
 * Chromium, real xterm, under the `?shell=1` fixture mode (the real
 * scion-chat-shell + header mounted around the page): re-proves the
 * terminal-passthrough guard that terminal-and-modal.pw.ts already covers
 * against the default (non-shell) fixture, this time with the header's own
 * palette button present in the DOM. The header button must not change
 * whether a terminal keystroke reaches the PTY or opens the palette — this
 * is a regression check for the header's addition, not new behaviour, so it
 * intentionally mirrors terminal-and-modal.pw.ts's own setup rather than
 * reusing it directly (that file is a sibling spec, not shared
 * infrastructure, and is left unmodified).
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';

type Frame = { type: string; data?: string };

/** Real xterm/PTY wiring: a routed WebSocket standing in for the Hub's PTY endpoint. */
async function setupTerminal(page: Page) {
  const frames: Frame[] = [];
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
  await page.routeWebSocket('**/pty?*', (socket) => {
    socket.onMessage((message) => frames.push(JSON.parse(String(message)) as Frame));
    socket.send(JSON.stringify({ type: 'data', data: Buffer.from('').toString('base64') }));
  });
  return {
    frames,
    input(): string[] {
      return frames
        .filter((f) => f.type === 'data')
        .map((f) => Buffer.from(f.data!, 'base64').toString());
    },
  };
}

async function gotoHiddenChatWithTerminalUnderShell(page: Page) {
  const requests = await setupApiMocks(page);
  const terminal = await setupTerminal(page);
  await page.goto('/e2e/chat-palette/fixture.html?shell=1', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.evaluate(() => window.chatPaletteFixture.hideChatShowTerminal());
  const helperTextarea = page.locator('.xterm-helper-textarea');
  await helperTextarea.waitFor({ state: 'attached' });
  await expect
    .poll(() => page.evaluate(() => window.chatPaletteFixture.terminalConnection()))
    .toBe('connected');
  return { requests, terminal, helperTextarea };
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

test('under ?shell=1, Ctrl+K in the terminal still reaches the PTY and makes zero palette state/fetch changes', async ({
  page,
}) => {
  const { requests, terminal, helperTextarea } = await gotoHiddenChatWithTerminalUnderShell(page);
  const requestCountBefore = requests.length;

  await helperTextarea.press('Control+k');
  await page.waitForTimeout(150);

  expect(terminal.input().join('')).toContain('\x0b');
  expect(requests.length).toBe(requestCountBefore);
  const paletteOpen = await page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
  expect(paletteOpen).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();
});

test('under ?shell=1, Meta+K in the terminal does not reach the PTY and does not open the chat palette', async ({
  page,
}) => {
  const { terminal, helperTextarea } = await gotoHiddenChatWithTerminalUnderShell(page);

  await helperTextarea.press('Meta+k');
  await page.waitForTimeout(150);

  // xterm does not forward Meta/Cmd-chord input to the PTY as data at all.
  expect(terminal.input().join('')).toBe('');
  await expect(paletteDialog(page)).toBeHidden();
});
