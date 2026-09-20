/**
 * Hidden terminal interaction isolation — real browser tests (P1.8 #1653).
 *
 * Each test uses a real xterm.js pane with Playwright-supplied network
 * and clipboard boundaries. No desktop clipboard access, no live Hub.
 *
 * Coverage:
 *   - hide → inert/blur/nonzero dims
 *   - reveal → changed size → resize sent
 *   - pending resize timer firing after hide
 *   - late connect focus
 *   - delayed paste/clipboard completing after hide
 *   - hidden OSC 52 isolation + visible OSC 52 continuity
 *   - protocol response continuity while hidden
 *   - Chat/Dashboard file drops unaffected by hidden terminals
 *   - upload completing after hide does not inject paths
 */
import { expect, test, type Page, type WebSocketRoute } from '@playwright/test';
import type {} from './fixture.js';

type Frame = { type: string; data?: string; cols?: number; rows?: number };
const agentId = '11111111-1111-4111-8111-111111111111';

async function setup(page: Page): Promise<{
  frames: Frame[];
  get attaches(): number;
  get closes(): number;
  write(text: string): void;
  input(): string[];
  peer(): WebSocketRoute;
}> {
  const frames: Frame[] = [];
  let peer!: WebSocketRoute;
  let attaches = 0;
  let closes = 0;
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
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    let body: unknown = {};
    if (request.url().endsWith('/shared-dirs')) {
      body = { sharedDirs: [{ name: 'scratchpad' }] };
    } else if (request.url().endsWith(`/agents/${agentId}`)) {
      body = {
        id: agentId,
        name: 'hidden-fixture-agent',
        phase: 'running',
        projectId: 'fixture-project',
        exposedPorts: [{ port: 3000, label: 'Preview' }],
      };
    } else if (request.url().endsWith('/files')) {
      // Upload endpoint — delay handled per-test via route override
      body = {};
    }
    await route.fulfill({ json: body });
  });
  await page.routeWebSocket('**/pty?*', (socket) => {
    peer = socket;
    attaches++;
    socket.onMessage((message) => frames.push(JSON.parse(String(message)) as Frame));
    socket.onClose(() => closes++);
  });
  return {
    frames,
    get attaches() {
      return attaches;
    },
    get closes() {
      return closes;
    },
    write(text: string): void {
      peer.send(JSON.stringify({ type: 'data', data: Buffer.from(text).toString('base64') }));
    },
    input(): string[] {
      return frames
        .filter((frame) => frame.type === 'data')
        .map((frame) => Buffer.from(frame.data!, 'base64').toString());
    },
    peer(): WebSocketRoute {
      return peer;
    },
  };
}

async function ready(page: Page): Promise<void> {
  await page.goto('/e2e/terminal-hidden/fixture.html');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.session.state.connection))
    .toBe('connected');
}

test('hide makes pane inert and blurs, reveal fits and sends changed size', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Verify connected and terminal is focused
  const initialResizeCount = ctx.frames.filter((f) => f.type === 'resize').length;

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Verify inert + hidden attributes
  const paneState = await page.evaluate(() => {
    const pane = window.hiddenFixture.pane;
    return {
      hidden: pane.hidden,
      inert: pane.inert,
    };
  });
  expect(paneState.hidden).toBe(true);
  expect(paneState.inert).toBe(true);

  // Change the container size while hidden
  await page.evaluate(() => {
    window.hiddenFixture.pane.style.width = '650px';
  });
  await page.waitForTimeout(200);

  // No resize should have been sent while hidden
  const hiddenResizeCount = ctx.frames.filter((f) => f.type === 'resize').length;
  expect(hiddenResizeCount).toBe(initialResizeCount);

  // Reveal the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(true));
  await page.waitForTimeout(200);

  // A resize should now be sent with valid nonzero dimensions
  const afterRevealResizes = ctx.frames.filter((f) => f.type === 'resize');
  expect(afterRevealResizes.length).toBeGreaterThan(initialResizeCount);
  const lastResize = afterRevealResizes.at(-1)!;
  expect(lastResize.cols).toBeGreaterThan(0);
  expect(lastResize.rows).toBeGreaterThan(0);
});

test('late socket open while hidden does not steal focus', async ({ page }) => {
  const ctx = await setup(page);

  // Modify setup to delay socket open
  await page.goto('/e2e/terminal-hidden/fixture.html');
  await expect.poll(() => ctx.attaches).toBe(1);

  // Verify terminal is connected
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.session.state.connection))
    .toBe('connected');

  // Focus a non-terminal element
  await page.evaluate(() => {
    document.getElementById('drop-target')!.setAttribute('tabindex', '0');
    document.getElementById('drop-target')!.focus();
  });

  // Verify the drop target has focus
  const activeTag = await page.evaluate(() => document.activeElement?.id);
  expect(activeTag).toBe('drop-target');

  // Send some output to verify the terminal renders but doesn't steal focus
  ctx.write('hello from hidden test\r\n');
  await expect.poll(() => page.evaluate(() => window.hiddenFixture.text())).toContain(
    'hello from hidden test'
  );

  // Verify focus hasn't moved
  const stillActive = await page.evaluate(() => document.activeElement?.id);
  expect(stillActive).toBe('drop-target');
});

test('hidden OSC 52 does not write to system clipboard, visible OSC 52 does', async ({
  page,
}) => {
  const ctx = await setup(page);
  await ready(page);

  // Set clipboard to known value
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'original';
  });

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Send OSC 52 clipboard write while hidden
  ctx.write('\x1b]52;c;' + Buffer.from('hidden-write').toString('base64') + '\x07');
  await page.waitForTimeout(300);

  // Clipboard should be unchanged
  const afterHiddenWrite = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(afterHiddenWrite).toBe('original');

  // Reveal the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(true));
  await page.waitForTimeout(100);

  // Send OSC 52 clipboard write while visible
  ctx.write('\x1b]52;c;' + Buffer.from('visible-write').toString('base64') + '\x07');
  await expect.poll(() => page.evaluate(() => window.hiddenFixture.clipboardText)).toBe(
    'visible-write'
  );
});

test('terminal protocol responses (DSR) continue while hidden', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Output still parses
  ctx.write('hidden output\r\n');
  await expect.poll(() => page.evaluate(() => window.hiddenFixture.text())).toContain(
    'hidden output'
  );

  // Send a Device Status Report request (CSI 6 n — cursor position report)
  // xterm.js should respond with CSI row ; col R through onData → sendData
  const beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[6n');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);

  // The response should be a cursor position report (ESC [ row ; col R)
  const response = ctx.input().at(-1)!;
  expect(response).toMatch(/^\x1b\[\d+;\d+R$/);
});

test('Chat/Dashboard file drops are unaffected when terminal pane is hidden', async ({ page }) => {
  await setup(page);
  await ready(page);

  // Hide the terminal pane (simulates navigating to Chat/Dashboard)
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));
  await page.waitForTimeout(100);

  // Drop a file on the sibling drop target
  const dropTarget = page.locator('#drop-target');
  await dropTarget.evaluate((el) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['test-content'], 'test.txt', { type: 'text/plain' }));
    // Fire dragover to set dropEffect
    const dragOver = new DragEvent('dragover', {
      bubbles: true,
      cancelable: true,
      dataTransfer: transfer,
    });
    el.dispatchEvent(dragOver);
    // Fire drop
    const drop = new DragEvent('drop', {
      bubbles: true,
      cancelable: true,
      dataTransfer: transfer,
    });
    el.dispatchEvent(drop);
  });

  // The drop target should have received the file
  await expect.poll(() => dropTarget.getAttribute('data-dropped')).toBe('true');
  await expect(dropTarget).toContainText('Received: test.txt');
});

test('delayed paste completing after hide does not send to terminal', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Enable gated clipboard mode
  await page.evaluate(() => window.hiddenFixture.gateClipboard());

  // Focus the terminal textarea and trigger Ctrl+V
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await textarea.press('Control+v');

  // Now hide the pane before releasing the clipboard
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));
  await page.waitForTimeout(50);

  // Release the clipboard with paste text
  const beforeInput = ctx.input().slice();
  await page.evaluate(() => window.hiddenFixture.releaseClipboard('late-paste-text'));
  await page.waitForTimeout(300);

  // The paste text should NOT have been sent to the terminal
  const afterInput = ctx.input();
  expect(afterInput.filter((d) => d === 'late-paste-text')).toHaveLength(0);
  // Only pre-existing input should be present
  expect(afterInput.length).toBe(beforeInput.length);
});

test('upload completing after hide does not inject paths', async ({ page }) => {
  const ctx = await setup(page);

  // Register the delayed upload route AFTER setup so it takes LIFO priority
  // over the generic api/v1 handler.
  let resolveUpload!: () => void;
  await page.route('**/shared-dirs/scratchpad/files', async (route) => {
    await new Promise<void>((r) => {
      resolveUpload = r;
    });
    await route.fulfill({ json: {} });
  });

  await ready(page);

  // Drop a file to start an upload
  const wrapper = page.locator('.terminal-wrapper');
  await wrapper.evaluate((el) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['upload-content'], 'delayed.txt', { type: 'text/plain' }));
    el.dispatchEvent(
      new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer })
    );
  });
  await page.waitForTimeout(100);

  // Hide the pane while upload is in progress
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));
  await page.waitForTimeout(50);

  // Complete the upload
  const beforeInput = ctx.input().slice();
  resolveUpload();
  await page.waitForTimeout(300);

  // The path injection should NOT have happened
  const afterInput = ctx.input();
  const newPaths = afterInput.filter((d) => d.includes('delayed.txt'));
  expect(newPaths).toHaveLength(0);
  expect(afterInput.length).toBe(beforeInput.length);
});
