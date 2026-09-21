import { expect, test, type Page, type WebSocketRoute } from '@playwright/test';
import type {} from './fixture.js';

type Frame = { type: string; data?: string; cols?: number; rows?: number };
const agentId = '11111111-1111-4111-8111-111111111111';

async function setup(page: Page) {
  const frames: Frame[] = [];
  const requests: Array<{ url: string; body: string | null }> = [];
  let peer!: WebSocketRoute;
  let attaches = 0;
  let closes = 0;
  let execs = 0;
  await page.addInitScript(() => {
    // Real pane/SSE adapter, fake isolated event source and system clipboard.
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
    window.clipboardText = '';
    Object.defineProperty(navigator, 'clipboard', {
      value: {
        writeText: (text: string) => {
          window.clipboardText = text;
          return Promise.resolve();
        },
        readText: () => Promise.resolve(window.clipboardText),
      },
    });
  });
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    requests.push({ url: request.url(), body: request.postData() });
    let body: unknown = {};
    if (request.url().endsWith('/exec')) {
      execs++;
      body =
        execs === 1
          ? { exitCode: 1, output: 'secret "TOKEN" already exists' }
          : { exitCode: 0, output: '' };
    } else if (request.url().endsWith('/shared-dirs')) {
      body = { sharedDirs: [{ name: 'scratchpad' }] };
    } else if (request.url().endsWith(`/agents/${agentId}`)) {
      body = {
        id: agentId,
        name: 'fixture-agent',
        phase: 'running',
        projectId: 'fixture-project',
        harnessAuth: 'none',
        resolvedHarness: 'claude',
        exposedPorts: [{ port: 3000, label: 'Preview' }],
      };
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
    requests,
    get attaches() {
      return attaches;
    },
    get closes() {
      return closes;
    },
    write(text: string): void {
      peer.send(JSON.stringify({ type: 'data', data: Buffer.from(text).toString('base64') }));
    },
    bytewise(text: string): void {
      for (const byte of Buffer.from(text))
        peer.send(JSON.stringify({ type: 'data', data: Buffer.from([byte]).toString('base64') }));
    },
    input(): string[] {
      return frames
        .filter((frame) => frame.type === 'data')
        .map((frame) => Buffer.from(frame.data!, 'base64').toString());
    },
  };
}

async function ready(page: Page): Promise<void> {
  await page.goto('/e2e/terminal-pane/fixture.html');
  await expect
    .poll(() => page.evaluate(() => window.paneFixture.session.state.connection))
    .toBe('connected');
  await page.evaluate(() => window.paneFixture.remember());
}

test('production pane retains real xterm, hidden byte parsing and scrollback across route/layout/remount', async ({
  page,
}) => {
  const peer = await setup(page);
  await ready(page);
  peer.write('before\r\n');
  await expect.poll(() => page.evaluate(() => window.paneFixture.text())).toContain('before');
  await page.evaluate(() => {
    window.paneFixture.pane.setVisible(false);
    history.pushState(null, '', '/dashboard');
  });
  await page.waitForTimeout(150);
  const count = peer.frames.length;
  peer.bytewise('hidden café 世界 🙂\r\n\x1b[31mRED\x1b[0m\r\n');
  await expect
    .poll(() => page.evaluate(() => window.paneFixture.text()))
    .toContain('hidden café 世界 🙂\nRED');
  expect(
    await page.evaluate(() =>
      window.paneFixture.terminal().buffer.active.getLine(2)?.getCell(0)?.getFgColor()
    )
  ).toBe(1);
  for (let index = 0; index < 200; index++) peer.write(`line-${index}\r\n`);
  await expect.poll(() => page.evaluate(() => window.paneFixture.text())).toContain('line-199');
  await page.waitForTimeout(150);
  expect(peer.frames).toHaveLength(count);
  await page.evaluate(() => {
    const pane = window.paneFixture.pane;
    pane.remove();
    document.body.append(pane);
    pane.style.width = '650px';
    pane.style.flex = 'none';
    pane.setVisible(true);
  });
  await expect
    .poll(() => peer.frames.filter((frame) => frame.type === 'resize').length)
    .toBeGreaterThan(0);
  await expect(page.locator('.xterm-rows')).toContainText('line-199');
  expect(await page.evaluate(() => window.paneFixture.sameIdentity())).toBe(true);
  expect(await page.evaluate(() => window.paneFixture.text())).toContain(
    Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n')
  );
  expect(peer.attaches).toBe(1);
  expect(peer.closes).toBe(0);
  expect(peer.input()).toEqual([]);
  expect(
    peer.frames.every((frame) => frame.type !== 'resize' || (frame.cols! > 0 && frame.rows! > 0))
  ).toBe(true);
  await page.evaluate(() => {
    window.paneFixture.pane.dispose();
    window.paneFixture.pane.dispose();
  });
  await expect.poll(() => peer.closes).toBe(1);
  expect(peer.input()).toEqual(['\x02d']);
  expect(await page.evaluate(() => window.paneFixture.disposals)).toBe(1);
  await expect(page.locator('.xterm')).toHaveCount(0);
});

test('hidden initial mount waits for layout and close cannot resurrect it', async ({ page }) => {
  const peer = await setup(page);
  await page.goto('/e2e/terminal-pane/fixture.html?hidden');
  await expect.poll(() => page.evaluate(() => !!window.paneFixture.terminal()?.element)).toBe(true);
  expect(peer.attaches).toBe(0);
  await page.evaluate(() => window.paneFixture.pane.setVisible(true));
  await expect.poll(() => peer.attaches).toBe(1);
  await page.evaluate(() => window.paneFixture.pane.dispose());
  await page.goto('/e2e/terminal-pane/fixture.html?hidden');
  await expect.poll(() => page.evaluate(() => !!window.paneFixture.terminal()?.element)).toBe(true);
  await page.evaluate(() => {
    window.paneFixture.remember();
    window.paneFixture.pane.dispose();
    window.paneFixture.pane.setVisible(true);
  });
  await page.waitForTimeout(200);
  expect(peer.attaches).toBe(1);
  expect(await page.evaluate(() => window.paneFixture.disposals)).toBe(1);
});

test('real xterm keyboard, OSC window, selection, clipboard, links and toolbar parity', async ({
  page,
  context,
}) => {
  const peer = await setup(page);
  await ready(page);
  peer.write('\x1b]7337;tmuxwindow=shell\x07COPY ME\r\nhttps://example.test/link\r\n');
  await expect(page.locator('button[title="Shell window"]')).toHaveClass('active');
  await page.locator('button[title="Agent window"]').click();
  await page.locator('button[title="Shell window"]').click();
  expect(peer.input()).toEqual(['\x02A', '\x02S']);
  const textarea = page.locator('.xterm-helper-textarea');
  await textarea.press('Shift+Enter');
  expect(peer.input()).toEqual(['\x02A', '\x02S', '\x1b\r']);
  await page.evaluate(() => window.paneFixture.terminal().select(0, 0, 7));
  await textarea.press('Control+c');
  expect(await page.evaluate(() => window.clipboardText)).toBe('COPY ME');
  await textarea.press('Control+v');
  await expect.poll(() => peer.input().at(-1)).toBe('COPY ME');
  peer.write('\x1b]52;c;' + Buffer.from('OSC clipboard').toString('base64') + '\x07');
  await expect.poll(() => page.evaluate(() => window.clipboardText)).toBe('OSC clipboard');
  await expect(page.locator('a.port-btn')).toHaveAttribute(
    'href',
    `/api/v1/agents/${agentId}/ports/3000/proxy/`
  );
  // WebLinksAddon detects and activates links through real pointer interaction.
  const line = page.locator('.xterm-rows > div').nth(1);
  await line.hover({ position: { x: 30, y: 8 } });
  await context.route('https://example.test/link', (route) =>
    route.fulfill({ body: 'Link target fixture' })
  );
  const popupPromise = context.waitForEvent('page');
  await line.click({ position: { x: 30, y: 8 } });
  const popup = await popupPromise;
  await expect.poll(() => popup.url()).toBe('https://example.test/link');
  await popup.close();
});

test('OSC 0 title update switches toolbar active window and overrides OSC 7337 (F1 fix)', async ({
  page,
}) => {
  const peer = await setup(page);
  await ready(page);
  // OSC 7337 sets initial state to "shell"
  peer.write('\x1b]7337;tmuxwindow=shell\x07');
  await expect(page.locator('button[title="Shell window"]')).toHaveClass('active');
  // OSC 0 title update overrides to "agent"
  peer.write('\x1b]0;agent\x07');
  await expect(page.locator('button[title="Agent window"]')).toHaveClass('active');
  // OSC 0 with "shell" switches back
  peer.write('\x1b]0;shell\x07');
  await expect(page.locator('button[title="Shell window"]')).toHaveClass('active');
  // Unknown OSC 0 value does not change toolbar state
  peer.write('\x1b]0;bash\x07');
  await expect(page.locator('button[title="Shell window"]')).toHaveClass('active');
});

test('capture-auth scope/conflict retry and upload use the pane identity after route change', async ({
  page,
}) => {
  const peer = await setup(page);
  await ready(page);
  await page.evaluate(() => history.pushState(null, '', '/agents/not-this-agent/terminal'));
  await page.locator('.capture-auth-btn').click();
  await page.locator('sl-radio[value="user"]').click();
  await page.locator('sl-button[variant="primary"]').click();
  await expect(page.locator('sl-dialog')).toContainText('TOKEN');
  await page.locator('sl-button[variant="warning"]').click();
  await expect
    .poll(() => peer.requests.filter((request) => request.url.endsWith('/exec')).length)
    .toBe(2);
  const execs = peer.requests.filter((request) => request.url.endsWith('/exec'));
  expect(execs.every((request) => request.url.endsWith(`/agents/${agentId}/exec`))).toBe(true);
  expect((JSON.parse(execs[0].body!) as { command: string[] }).command).toEqual([
    'python3',
    '/home/scion/.scion/harness/capture_auth.py',
    '--scope',
    'user',
  ]);
  expect((JSON.parse(execs[1].body!) as { command: string[] }).command.at(-1)).toBe('--force');
  await page.locator('.terminal-wrapper').evaluate((element) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['example'], 'upload file.txt', { type: 'text/plain' }));
    element.dispatchEvent(
      new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer })
    );
  });
  await expect
    .poll(() => peer.input().at(-1))
    .toMatch(/^'\/scion-volumes\/scratchpad\/\.attachments\/_web\/[^/]+\/upload file.txt' $/);
  const upload = peer.requests.find((request) => request.url.endsWith('/files'));
  expect(upload?.url).toContain('/projects/fixture-project/shared-dirs/scratchpad/files');
  expect(upload?.body).toContain('example');
});
