import { expect, test, type Page } from '@playwright/test';
import type {} from './fixture.js';
import { installEventSourceStub } from '../event-source-stub.js';

const agentId = '11111111-1111-4111-8111-111111111111';

// A phone: touch input, no wheel.
test.use({ hasTouch: true, viewport: { width: 390, height: 664 } });

/** Mounts the production pane; returns the peer's writer and the input it received. */
async function setup(page: Page): Promise<{ write(text: string): void; input(): string[] }> {
  const input: string[] = [];
  let write!: (text: string) => void;
  await installEventSourceStub(page);
  await page.route('**/api/v1/**', (route) =>
    route.fulfill({
      json: route.request().url().endsWith(`/agents/${agentId}`)
        ? { id: agentId, name: 'fixture-agent', phase: 'running', projectId: 'fixture-project' }
        : {},
    })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    write = (text): void => {
      socket.send(JSON.stringify({ type: 'data', data: Buffer.from(text).toString('base64') }));
    };
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type: string; data?: string };
      if (frame.type === 'data') input.push(Buffer.from(frame.data!, 'base64').toString());
    });
    // The client is connected on its first inbound data frame.
    write('');
  });
  await page.goto('/e2e/terminal-pane/fixture.html');
  await expect
    .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
    .toBe('connected');
  return { write: (text) => write(text), input: () => input };
}

/** Drags one finger from (x, fromY) to (toX, toY) with real (trusted) touch events. */
async function drag(page: Page, x: number, fromY: number, toY: number, toX = x): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.dispatchTouchEvent', {
    type: 'touchStart',
    touchPoints: [{ x, y: fromY }],
  });
  for (let step = 1; step <= 10; step++) {
    const point = { x: x + ((toX - x) * step) / 10, y: fromY + ((toY - fromY) * step) / 10 };
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [point] });
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await cdp.detach();
}

test('a one-finger drag scrolls an application that reports the mouse, at the cell touched', async ({
  page,
}) => {
  const peer = await setup(page);
  // What tmux sends with `mouse on`: button tracking, SGR encoding.
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
  // Column 5. The finger moves down a little over four rows, from row 3 to row 7.
  const x = cell.left + 4.5 * cell.w;
  const y = (row: number): number => cell.top + (row - 0.5) * cell.h;
  await drag(page, x, y(3), y(7.3));

  // Down is towards earlier output: one wheel-up (64) for each row travelled,
  // reported at the cell under the finger.
  await expect.poll(() => peer.input().length).toBe(4);
  expect(peer.input().every((report) => report.startsWith('\x1b[<64;5;'))).toBe(true);
  expect(peer.input().at(-1)).toBe('\x1b[<64;5;7M');

  // And back up two rows: wheel-down (65).
  await drag(page, x, y(7.3), y(5));
  await expect.poll(() => peer.input().length).toBe(6);
  expect(peer.input().slice(4)).toEqual(['\x1b[<65;5;6M', '\x1b[<65;5;5M']);

  // A sideways drag is not a scroll, even when it drifts a row down: nothing
  // more is sent.
  await drag(page, x, y(5), y(6.2), x + 20 * cell.w);
  await drag(page, x, y(5), y(4));
  await expect.poll(() => peer.input().length).toBe(7);
  expect(peer.input().slice(6)).toEqual(['\x1b[<65;5;4M']);
});

test('a drag sends nothing to an application that does not report the mouse', async ({ page }) => {
  const peer = await setup(page);
  for (let line = 0; line < 100; line++) peer.write(`line-${line}\r\n`);
  await expect.poll(() => page.evaluate(() => window.paneFixture.text())).toContain('line-99');
  const top = (): Promise<number> =>
    page.evaluate(() => window.paneFixture.terminal().buffer.active.viewportY);
  const before = await top();

  const box = (await page.locator('.xterm-screen').boundingBox())!;
  await drag(page, box.x + 40, box.y + 40, box.y + 200);

  // xterm.js scrolls its own viewport back; no wheel reports are sent.
  await expect.poll(top).toBeLessThan(before);
  expect(peer.input()).toEqual([]);
});
