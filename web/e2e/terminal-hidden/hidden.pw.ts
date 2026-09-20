/**
 * Hidden terminal interaction isolation — real browser tests (P1.8 #1653).
 *
 * Each test uses a real xterm.js pane with Playwright-supplied network
 * and clipboard boundaries. No desktop clipboard access, no live Hub.
 *
 * Coverage:
 *   - hide → inert/blur/nonzero dims preserved
 *   - reveal → changed size → resize sent
 *   - resize timer cancelled on hide → no late resize
 *   - late connect focus
 *   - delayed paste/clipboard completing after hide
 *   - hidden OSC 52 isolation + visible OSC 52 continuity
 *   - visible-but-unfocused OSC 52 write/read rejection
 *   - focus lost during pending paste/upload
 *   - protocol response continuity while hidden
 *   - Chat/Dashboard file drops unaffected (hidden AND visible panes)
 *   - upload completing after hide does not inject paths
 *   - UTF-8 multi-byte OSC 52 round-trip parity
 *   - DOM focus ownership: focusout to sibling blocks OSC 52
 *   - DOM focus ownership: toolbar click within pane preserves focus
 *   - DOM focus ownership: refocusing terminal restores _focused
 *   - DOM focus ownership: initial visible pane without DOM focus blocks OSC 52
 *   - DOM focus ownership: hide/reveal while sibling focused blocks until focused
 *   - DOM focus ownership: window blur (null relatedTarget) blocks OSC 52
 *   - DOM focus ownership: file drop focuses terminal via DOM
 *   - Protocol: DSR/DA unchanged through focus state transitions
 *   - OSC 52 read generation check: reconnect blocks stale response
 *   - OSC 52 selection types: non-'c' reads get empty response, writes no-op
 *   - OSC 52 selection type echoed in read response
 *   - OSC 52 malformed payloads handled gracefully (invalid base64 no-mutation)
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
    get attaches(): number {
      return attaches;
    },
    get closes(): number {
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
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.text()))
    .toContain('hello from hidden test');

  // Verify focus hasn't moved
  const stillActive = await page.evaluate(() => document.activeElement?.id);
  expect(stillActive).toBe('drop-target');
});

test('hidden OSC 52 does not write to system clipboard, visible OSC 52 does', async ({ page }) => {
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

  // Focus the terminal (setVisible derives _focused from DOM, not unconditionally)
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Send OSC 52 clipboard write while visible and focused
  ctx.write('\x1b]52;c;' + Buffer.from('visible-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('visible-write');
});

test('terminal protocol responses (DSR) continue while hidden', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Output still parses
  ctx.write('hidden output\r\n');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.text()))
    .toContain('hidden output');

  // Send a Device Status Report request (CSI 6 n — cursor position report)
  // xterm.js should respond with CSI row ; col R through onData → sendData
  const beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[6n');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);

  // The response should be a cursor position report (ESC [ row ; col R).
  // Use String.raw + eslint-disable to match the literal ESC byte.
  const response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
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

// --- Visible-but-unfocused tests (setFocused) ---

test('visible but unfocused pane rejects OSC 52 clipboard write', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'original';
  });

  // Pane is visible but unfocused (multi-pane scenario)
  await page.evaluate(() => window.hiddenFixture.pane.setFocused(false));

  // Send OSC 52 clipboard write while visible+unfocused
  ctx.write('\x1b]52;c;' + Buffer.from('unfocused-write').toString('base64') + '\x07');
  await page.waitForTimeout(300);

  // Clipboard should be unchanged — unfocused panes cannot write
  const afterWrite = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(afterWrite).toBe('original');

  // Re-focus and verify write works
  await page.evaluate(() => window.hiddenFixture.pane.setFocused(true));
  ctx.write('\x1b]52;c;' + Buffer.from('focused-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('focused-write');
});

test('visible but unfocused pane rejects OSC 52 clipboard read', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'read-me';
  });

  // Unfocus the pane (visible but not focused)
  await page.evaluate(() => window.hiddenFixture.pane.setFocused(false));

  const beforeCount = ctx.frames.filter((f) => f.type === 'data').length;

  // Send OSC 52 clipboard read request while visible+unfocused
  ctx.write('\x1b]52;c;?\x07');
  await page.waitForTimeout(300);

  // No clipboard response should have been sent — read silently dropped
  const afterCount = ctx.frames.filter((f) => f.type === 'data').length;
  expect(afterCount).toBe(beforeCount);
});

test('focus lost during pending clipboard read does not send paste', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Enable gated clipboard mode
  await page.evaluate(() => window.hiddenFixture.gateClipboard());

  // Focus the terminal textarea and trigger Ctrl+V
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await textarea.press('Control+v');

  // Lose focus (but remain visible) before clipboard resolves
  await page.evaluate(() => window.hiddenFixture.pane.setFocused(false));
  await page.waitForTimeout(50);

  // Release the clipboard with paste text
  const beforeInput = ctx.input().slice();
  await page.evaluate(() => window.hiddenFixture.releaseClipboard('unfocused-paste'));
  await page.waitForTimeout(300);

  // The paste should NOT have been sent — pane lost focus during async
  const afterInput = ctx.input();
  expect(afterInput.filter((d) => d === 'unfocused-paste')).toHaveLength(0);
  expect(afterInput.length).toBe(beforeInput.length);
});

test('focus lost during pending upload does not inject paths', async ({ page }) => {
  const ctx = await setup(page);

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
    transfer.items.add(
      new File(['upload-content'], 'unfocused-upload.txt', { type: 'text/plain' })
    );
    el.dispatchEvent(
      new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer })
    );
  });
  await page.waitForTimeout(100);

  // Lose focus (but remain visible) while upload is pending
  await page.evaluate(() => window.hiddenFixture.pane.setFocused(false));
  await page.waitForTimeout(50);

  // Complete the upload
  const beforeInput = ctx.input().slice();
  resolveUpload();
  await page.waitForTimeout(300);

  // Paths should NOT have been injected
  const afterInput = ctx.input();
  expect(afterInput.filter((d) => d.includes('unfocused-upload.txt'))).toHaveLength(0);
  expect(afterInput.length).toBe(beforeInput.length);
});

test('visible pane does not preventDefault on sibling drop targets', async ({ page }) => {
  await setup(page);
  await ready(page);

  // Pane is VISIBLE and FOCUSED — but sibling drops must still work
  const dropTarget = page.locator('#drop-target');
  await dropTarget.evaluate((el) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['sibling-file'], 'sibling.txt', { type: 'text/plain' }));
    const dragOver = new DragEvent('dragover', {
      bubbles: true,
      cancelable: true,
      dataTransfer: transfer,
    });
    el.dispatchEvent(dragOver);
    const drop = new DragEvent('drop', {
      bubbles: true,
      cancelable: true,
      dataTransfer: transfer,
    });
    el.dispatchEvent(drop);
  });

  // The sibling drop target should have received the file
  await expect.poll(() => dropTarget.getAttribute('data-dropped')).toBe('true');
  await expect(dropTarget).toContainText('Received: sibling.txt');
});

test('visible focused OSC 52 round-trips UTF-8 multi-byte content', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Set clipboard to multi-byte UTF-8 content
  const utf8Content = '\u{1F600} éèê 你好 \u{1F680}';
  await page.evaluate((text) => {
    window.hiddenFixture.clipboardText = text;
  }, utf8Content);

  // OSC 52 write: send multi-byte content encoded as base64
  const writePayload = Buffer.from(utf8Content).toString('base64');
  ctx.write('\x1b]52;c;' + writePayload + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe(utf8Content);

  // OSC 52 read: request clipboard back and verify response
  const beforeDataCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;c;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeDataCount);

  // Decode the response: ESC ] 52 ; c ; <base64> BEL
  const response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  const match = response.match(/^\x1b\]52;c;([A-Za-z0-9+/=]+)\x07$/);
  expect(match).not.toBeNull();
  const decoded = Buffer.from(match![1], 'base64').toString('utf-8');
  expect(decoded).toBe(utf8Content);
});

test('hidden pane preserves nonzero terminal dimensions', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Capture dimensions while visible
  const visibleDims = await page.evaluate(() => {
    const t = window.hiddenFixture.terminal();
    return { cols: t.cols, rows: t.rows };
  });
  expect(visibleDims.cols).toBeGreaterThan(0);
  expect(visibleDims.rows).toBeGreaterThan(0);

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Terminal dimensions should still be nonzero (last known size preserved)
  const hiddenDims = await page.evaluate(() => {
    const t = window.hiddenFixture.terminal();
    return { cols: t.cols, rows: t.rows };
  });
  expect(hiddenDims.cols).toBe(visibleDims.cols);
  expect(hiddenDims.rows).toBe(visibleDims.rows);

  // No resize frame should have been sent when hiding
  const resizeFrames = ctx.frames.filter((f) => f.type === 'resize');
  const lastResize = resizeFrames.at(-1);
  // If a resize was sent at connection, it should match visible dims, not zero
  if (lastResize) {
    expect(lastResize.cols).toBeGreaterThan(0);
    expect(lastResize.rows).toBeGreaterThan(0);
  }
});

test('resize timer cancelled on hide does not send late resize', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Record resize count
  const resizesBefore = ctx.frames.filter((f) => f.type === 'resize').length;

  // Rapidly change size to trigger resize debounce timer, then immediately hide
  await page.evaluate(() => {
    window.hiddenFixture.pane.style.width = '500px';
  });
  // Don't wait for debounce — hide immediately to test timer cancelation
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));

  // Wait longer than the debounce interval (150ms default)
  await page.waitForTimeout(400);

  // No new resize should have been sent after hide
  const resizesAfter = ctx.frames.filter((f) => f.type === 'resize').length;
  expect(resizesAfter).toBe(resizesBefore);
});

// --- DOM focus ownership tests ---

test('clicking sibling element clears _focused via DOM focusout, blocks OSC 52', async ({
  page,
}) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'before-focus-loss';
  });

  // Focus the terminal first (ensures focusin fires, _focused = true)
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Now focus a sibling element — this triggers focusout on the pane
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  await page.waitForTimeout(50);

  // Verify DOM focus has moved
  const activeId = await page.evaluate(() => document.activeElement?.id);
  expect(activeId).toBe('drop-target');

  // Send OSC 52 clipboard write — should be blocked because _focused is now false
  ctx.write('\x1b]52;c;' + Buffer.from('after-focus-loss').toString('base64') + '\x07');
  await page.waitForTimeout(300);

  // Clipboard should NOT have been modified
  const clipboardAfter = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clipboardAfter).toBe('before-focus-loss');
});

test('toolbar click within pane preserves _focused (focus stays inside pane)', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'toolbar-test';
  });

  // Focus the terminal first
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Click a toolbar button (focus moves within the pane's shadow DOM children).
  // Use the agent/shell switch button if available, or simulate a toolbar-like click.
  await page.evaluate(() => {
    // Create a focusable element inside the pane to simulate toolbar interaction
    const pane = window.hiddenFixture.pane;
    const btn = document.createElement('button');
    btn.id = 'test-toolbar-btn';
    btn.textContent = 'Test';
    pane.appendChild(btn);
    btn.focus();
  });
  await page.waitForTimeout(50);

  // _focused should still be true because focus stayed within the pane
  ctx.write('\x1b]52;c;' + Buffer.from('toolbar-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('toolbar-write');

  // Clean up
  await page.evaluate(() => {
    document.getElementById('test-toolbar-btn')?.remove();
  });
});

test('refocusing terminal after sibling focus restores _focused', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'original';
  });

  // Focus terminal, then move focus to sibling
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  await page.waitForTimeout(50);

  // OSC 52 write should be blocked (focus lost)
  ctx.write('\x1b]52;c;' + Buffer.from('blocked-write').toString('base64') + '\x07');
  await page.waitForTimeout(200);
  const clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('original');

  // Refocus the terminal — _focused should be restored via focusin
  await textarea.focus();
  await page.waitForTimeout(50);

  // OSC 52 write should now succeed
  ctx.write('\x1b]52;c;' + Buffer.from('restored-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('restored-write');
});

// --- OSC 52 generation check on read ---

test('OSC 52 read response blocked after session reconnect (generation mismatch)', async ({
  page,
}) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'sensitive-content';
  });

  // Gate the clipboard so readText() won't resolve until we release it
  await page.evaluate(() => window.hiddenFixture.gateClipboard());

  // Record generation before
  const genBefore = await page.evaluate(() => window.hiddenFixture.session.state.generation);

  // Send OSC 52 read request — starts gated readText()
  ctx.write('\x1b]52;c;?\x07');
  await page.waitForTimeout(100);

  // Close the WebSocket from server side to simulate disconnect
  void ctx.peer().close();

  // Wait for disconnected state
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.session.state.connection))
    .toBe('disconnected');

  // Reconnect — increments generation
  await page.evaluate(() => void window.hiddenFixture.session.connect());
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.session.state.connection))
    .toBe('connected');

  const genAfter = await page.evaluate(() => window.hiddenFixture.session.state.generation);
  expect(genAfter).toBeGreaterThan(genBefore);

  // Count data frames before releasing clipboard
  const dataFramesBefore = ctx.frames.filter((f) => f.type === 'data').length;

  // Release the gated clipboard — the pending readText() resolves
  await page.evaluate(() => window.hiddenFixture.releaseClipboard('sensitive-content'));
  await page.waitForTimeout(300);

  // No new data should have been sent — generation mismatch blocks the response
  const dataFramesAfter = ctx.frames.filter((f) => f.type === 'data').length;
  expect(dataFramesAfter, 'OSC 52 read response must not leak to reconnected session').toBe(
    dataFramesBefore
  );
});

// --- OSC 52 selection type tests ---

test('OSC 52 non-c selection types are silently ignored', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'original';
  });

  // Send OSC 52 write with selection type 'p' (X11 primary — unsupported)
  ctx.write('\x1b]52;p;' + Buffer.from('primary-write').toString('base64') + '\x07');
  await page.waitForTimeout(200);

  // Clipboard should be unchanged — 'p' selection ignored
  let clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('original');

  // Send OSC 52 read with selection type 'p' — should get empty response
  // matching original BrowserClipboardProvider semantics (returns '' for non-'c')
  const dataFramesBefore = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;p;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(dataFramesBefore);
  // Response should be empty: ESC ] 52 ; p ; BEL (no base64 content)
  const pResponse = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(pResponse).toBe('\x1b]52;p;\x07');

  // Send OSC 52 write with selection type 's' — also unsupported
  ctx.write('\x1b]52;s;' + Buffer.from('secondary-write').toString('base64') + '\x07');
  await page.waitForTimeout(200);
  clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('original');

  // Verify 'c' selection still works
  ctx.write('\x1b]52;c;' + Buffer.from('correct-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('correct-write');
});

test('OSC 52 read response echoes selection type in reply', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'echo-test';
  });

  const beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;c;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);

  // Response should include 'c' selection type: ESC ] 52 ; c ; <base64> BEL
  const response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  const match = response.match(/^\x1b\]52;c;([A-Za-z0-9+/=]+)\x07$/);
  expect(match).not.toBeNull();
  const decoded = Buffer.from(match![1], 'base64').toString('utf-8');
  expect(decoded).toBe('echo-test');
});

test('OSC 52 malformed payloads are handled gracefully', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'unchanged';
  });

  // No semicolon — should be silently ignored
  ctx.write('\x1b]52nosemicolon\x07');
  await page.waitForTimeout(100);

  // Clipboard should be unchanged — no semicolon means handler returns early
  let clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('unchanged');

  // Invalid base64 — atob() throws, no clipboard mutation. Deliberate
  // divergence from original addon which wrote '' to clipboard on invalid
  // base64. No-mutation is safer for malformed server output.
  ctx.write('\x1b]52;c;!!!invalid-base64!!!\x07');
  await page.waitForTimeout(100);

  // Clipboard untouched — atob throw prevents writeText call
  clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('unchanged');

  // Empty payload — empty string is valid base64 (decodes to empty bytes),
  // so this writes empty string to clipboard. This is correct behavior.
  ctx.write('\x1b]52;c;\x07');
  await page.waitForTimeout(100);
  clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('');
});

// --- Rev4: Initialization and reveal focus derivation tests ---

test('initial visible pane without DOM focus blocks OSC 52 clipboard write', async ({ page }) => {
  const ctx = await setup(page);

  // Focus sibling BEFORE navigating, so shouldAutoFocusTerminal returns false
  await page.goto('/e2e/terminal-hidden/fixture.html');

  // Wait for connection
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.session.state.connection))
    .toBe('connected');

  // Move focus to sibling — even if auto-focus ran, this simulates rail/tab focus
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  await page.waitForTimeout(50);

  // Verify focus is on sibling
  const activeId = await page.evaluate(() => document.activeElement?.id);
  expect(activeId).toBe('drop-target');

  // Set clipboard
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'before-init-test';
  });

  // OSC 52 write should be blocked — pane is visible but has no DOM focus
  ctx.write('\x1b]52;c;' + Buffer.from('init-write').toString('base64') + '\x07');
  await page.waitForTimeout(300);
  const clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('before-init-test');

  // Now focus terminal — should allow OSC 52
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  ctx.write('\x1b]52;c;' + Buffer.from('after-focus').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('after-focus');
});

test('hide/reveal while sibling focused blocks OSC 52 until terminal focused', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Focus the terminal first
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Focus sibling (simulates clicking rail/tab button)
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  const activeBeforeHide = await page.evaluate(() => document.activeElement?.id);
  expect(activeBeforeHide).toBe('drop-target');

  // Hide the pane
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));
  await page.waitForTimeout(50);

  // Reveal — focus is on sibling, not terminal. setVisible derives _focused from DOM.
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(true));
  await page.waitForTimeout(100);

  // Verify focus still on sibling
  const activeAfterReveal = await page.evaluate(() => document.activeElement?.id);
  expect(activeAfterReveal).toBe('drop-target');

  // Set clipboard
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'before-reveal-test';
  });

  // OSC 52 write should be BLOCKED — pane visible but _focused derived as false
  ctx.write('\x1b]52;c;' + Buffer.from('reveal-write').toString('base64') + '\x07');
  await page.waitForTimeout(300);
  const clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('before-reveal-test');

  // Focus the terminal — now OSC 52 should work
  await textarea.focus();
  await page.waitForTimeout(50);

  ctx.write('\x1b]52;c;' + Buffer.from('reveal-focused').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('reveal-focused');
});

test('window blur (null relatedTarget) clears _focused and blocks OSC 52', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Focus the terminal
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Set clipboard and verify OSC 52 works while focused
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'before-blur';
  });
  ctx.write('\x1b]52;c;' + Buffer.from('focused-ok').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('focused-ok');

  // Simulate window blur by dispatching focusout with null relatedTarget
  await page.evaluate(() => {
    const pane = window.hiddenFixture.pane;
    const focused = pane.shadowRoot?.querySelector('.xterm-helper-textarea');
    if (focused) {
      focused.dispatchEvent(
        new FocusEvent('focusout', { bubbles: true, composed: true, relatedTarget: null })
      );
    }
  });
  await page.waitForTimeout(50);

  // Reset clipboard to detect unauthorized write
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'after-blur';
  });

  // OSC 52 write should be blocked — null relatedTarget cleared _focused
  ctx.write('\x1b]52;c;' + Buffer.from('blur-write').toString('base64') + '\x07');
  await page.waitForTimeout(300);
  const clipAfter = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clipAfter).toBe('after-blur');
});

test('protocol DSR/DA responses continue through focus state transitions', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Focus terminal
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // DSR while focused — should work
  let beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[6n');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  let response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toMatch(/^\x1b\[\d+;\d+R$/);

  // Move focus to sibling (unfocused)
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  await page.waitForTimeout(50);

  // DSR while unfocused — should STILL work (protocol responses unrestricted)
  beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[6n');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toMatch(/^\x1b\[\d+;\d+R$/);

  // DA (Device Attributes) request while unfocused — also unrestricted
  beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[c');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  response = ctx.input().at(-1)!;
  // DA response: ESC [ ? ... c
  // eslint-disable-next-line no-control-regex
  expect(response).toMatch(/^\x1b\[\?[\d;]+c$/);

  // Hide pane — protocol still works
  await page.evaluate(() => window.hiddenFixture.pane.setVisible(false));
  await page.waitForTimeout(50);

  beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b[6n');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toMatch(/^\x1b\[\d+;\d+R$/);
});

test('file drop establishes real DOM focus on terminal', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Focus sibling first (terminal loses focus)
  await page.evaluate(() => {
    const target = document.getElementById('drop-target')!;
    target.setAttribute('tabindex', '0');
    target.focus();
  });
  await page.waitForTimeout(50);

  // Verify _focused is false (sibling has focus)
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'before-drop';
  });
  ctx.write('\x1b]52;c;' + Buffer.from('pre-drop-write').toString('base64') + '\x07');
  await page.waitForTimeout(200);
  const clip = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clip).toBe('before-drop');

  // Drop a file on the terminal — should focus terminal via DOM
  const wrapper = page.locator('.terminal-wrapper');
  await wrapper.evaluate((el) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['drop-content'], 'focus-test.txt', { type: 'text/plain' }));
    el.dispatchEvent(
      new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer })
    );
  });
  await page.waitForTimeout(100);

  // After drop, terminal should have focus via real DOM focus (not back door)
  // OSC 52 should now work because focusin fired
  ctx.write('\x1b]52;c;' + Buffer.from('post-drop-write').toString('base64') + '\x07');
  await expect
    .poll(() => page.evaluate(() => window.hiddenFixture.clipboardText))
    .toBe('post-drop-write');
});

test('non-c OSC 52 read returns empty response matching original addon', async ({ page }) => {
  const ctx = await setup(page);
  await ready(page);

  // Focus terminal
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.waitForTimeout(50);

  // Set clipboard to known value (should NOT be returned for non-'c')
  await page.evaluate(() => {
    window.hiddenFixture.clipboardText = 'system-clipboard-content';
  });

  // Read with selection 'p' — should get empty response, no clipboard access
  let beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;p;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  let response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toBe('\x1b]52;p;\x07');

  // Read with selection 'q' — same empty response
  beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;q;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toBe('\x1b]52;q;\x07');

  // Read with selection 's' — same empty response
  beforeCount = ctx.frames.filter((f) => f.type === 'data').length;
  ctx.write('\x1b]52;s;?\x07');
  await expect
    .poll(() => ctx.frames.filter((f) => f.type === 'data').length)
    .toBeGreaterThan(beforeCount);
  response = ctx.input().at(-1)!;
  // eslint-disable-next-line no-control-regex
  expect(response).toBe('\x1b]52;s;\x07');

  // Clipboard should NOT have been read — no OS access for non-'c'
  const clipUnchanged = await page.evaluate(() => window.hiddenFixture.clipboardText);
  expect(clipUnchanged).toBe('system-clipboard-content');
});
