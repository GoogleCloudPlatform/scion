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

import { expect, test, type WebSocketRoute } from '@playwright/test';
import type {} from './fixture.js';

type ClientFrame = { type: string; cols?: number; rows?: number; data?: string };

test('hidden PTY bytes preserve xterm, transport, screen and nonzero size', async ({ page }) => {
  let peer!: WebSocketRoute;
  const frames: ClientFrame[] = [];
  let upgrades = 0;
  await page.routeWebSocket('**/fixture-pty', (socket) => {
    upgrades++;
    peer = socket;
    socket.onMessage((data) => frames.push(JSON.parse(String(data)) as ClientFrame));
  });
  await page.goto('/e2e/terminal-lifecycle/fixture.html');
  await expect.poll(() => frames.filter((f) => f.type === 'resize').length).toBe(1);
  await page.evaluate(() => window.fixture.remember());
  peer.send(JSON.stringify({ type: 'data', data: Buffer.from('before\r\n').toString('base64') }));
  await expect.poll(() => page.evaluate(() => window.fixture.parsed)).toBe(1);

  await page.evaluate(() => window.fixture.hide());
  const initialFrames = frames.length;
  // Split every UTF-8 byte and every escape sequence across separate PTY frames.
  const bytes = Buffer.from('hidden café 世界 🙂\r\n\x1b[31mRED\x1b[0m\r\n');
  for (const byte of bytes) {
    peer.send(JSON.stringify({ type: 'data', data: Buffer.from([byte]).toString('base64') }));
  }
  await expect.poll(() => page.evaluate(() => window.fixture.parsed)).toBe(bytes.length + 1);
  expect(await page.evaluate(() => window.fixture.text())).toContain('hidden café 世界 🙂');
  // Let both the ResizeObserver and the debounce deadline run while hidden.
  await page.waitForTimeout(200);
  expect(frames).toHaveLength(initialFrames);
  await page.evaluate(() => window.fixture.reveal());
  expect(await page.evaluate(() => window.fixture.sameIdentity())).toBe(true);
  expect(await page.evaluate(() => window.fixture.text())).toContain(
    'before\nhidden café 世界 🙂\nRED'
  );
  expect(
    await page.evaluate(() =>
      window.fixture.terminal.buffer.active.getLine(2)?.getCell(0)?.getFgColor()
    )
  ).toBe(1);
  await expect(page.locator('.xterm-rows')).toContainText('hidden café 世界 🙂');
  expect(upgrades).toBe(1);
  expect(frames.filter((f) => f.type === 'data')).toEqual([]);

  await page.evaluate(async () => {
    window.fixture.hide();
    window.fixture.pane.style.width = '600px';
    await window.fixture.reveal();
  });
  await expect.poll(() => frames.filter((f) => f.type === 'resize').length).toBe(2);
  expect(frames.every((f) => f.type !== 'resize' || (f.cols! > 0 && f.rows! > 0))).toBe(true);
  expect(frames[1].cols).toBeLessThan(frames[0].cols!);
});

test('explicit close detaches and disposes once, including pending resize', async ({ page }) => {
  const frames: ClientFrame[] = [];
  let closes = 0;
  await page.routeWebSocket('**/fixture-pty', (socket) => {
    socket.onMessage((data) => frames.push(JSON.parse(String(data)) as ClientFrame));
    socket.onClose(() => closes++);
  });
  await page.goto('/e2e/terminal-lifecycle/fixture.html');
  await expect.poll(() => frames.length).toBe(1);
  await page.evaluate(() => {
    window.fixture.pane.style.width = '620px';
    window.fixture.scheduleFit();
    window.fixture.close();
    window.fixture.close();
  });
  await expect.poll(() => closes).toBe(1);
  expect(
    frames.filter((f) => f.type === 'data').map((f) => Buffer.from(f.data!, 'base64').toString())
  ).toEqual(['\x02d']);
  expect(await page.evaluate(() => window.fixture.disposals)).toBe(1);
  await expect(page.locator('.xterm')).toHaveCount(0);
  const afterClose = frames.length;
  await page.waitForTimeout(200);
  expect(frames).toHaveLength(afterClose);
});

test('hidden scrollback survives repeated reveals and zero-geometry layout', async ({ page }) => {
  let peer!: WebSocketRoute;
  const frames: ClientFrame[] = [];
  await page.routeWebSocket('**/fixture-pty', (socket) => {
    peer = socket;
    socket.onMessage((data) => frames.push(JSON.parse(String(data)) as ClientFrame));
  });
  await page.goto('/e2e/terminal-lifecycle/fixture.html');
  await expect.poll(() => frames.length).toBe(1);
  const originalSize = await page.evaluate(() => {
    const f = window.fixture;
    f.remember();
    f.scheduleFit();
    f.hide();
    return [f.terminal.cols, f.terminal.rows];
  });
  const lines = Array.from({ length: 200 }, (_, i) => `hidden line ${i}: 世界`);
  peer.send(
    JSON.stringify({ type: 'data', data: Buffer.from(lines.join('\r\n')).toString('base64') })
  );
  await expect.poll(() => page.evaluate(() => window.fixture.parsed)).toBe(1);
  expect(await page.evaluate(() => window.fixture.text())).toBe(lines.join('\n'));
  for (let round = 0; round < 3; round++) {
    await page.evaluate(async () => {
      const f = window.fixture;
      f.pane.style.width = '0px';
      f.pane.style.height = '0px';
      await f.reveal();
    });
    await page.waitForTimeout(150);
    expect(
      await page.evaluate(() => [window.fixture.terminal.cols, window.fixture.terminal.rows])
    ).toEqual(originalSize);
    await page.evaluate(async () => {
      const f = window.fixture;
      f.hide();
      f.pane.style.width = '800px';
      f.pane.style.height = '400px';
      await f.reveal();
    });
  }
  expect(frames).toHaveLength(1);
  expect(await page.evaluate(() => window.fixture.sameIdentity())).toBe(true);
  expect(await page.evaluate(() => window.fixture.text())).toBe(lines.join('\n'));
  await expect(page.locator('.xterm-rows')).toContainText('hidden line 199: 世界');
});
