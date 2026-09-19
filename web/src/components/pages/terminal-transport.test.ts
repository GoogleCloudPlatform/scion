// @vitest-environment happy-dom
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScionPageTerminal } from './terminal.js';

const terminal = vi.hoisted(() => ({
  instances: [] as Array<{ dispose: ReturnType<typeof vi.fn>; reset: ReturnType<typeof vi.fn> }>,
}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    dispose = vi.fn();
    reset = vi.fn();
    write = vi.fn();
    focus = vi.fn();
    parser = { registerOscHandler: vi.fn() };
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    attachCustomKeyEventHandler = vi.fn();
    constructor() {
      terminal.instances.push(this);
    }
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/addon-clipboard', () => ({ ClipboardAddon: class {} }));

class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  send = vi.fn();
  close = vi.fn();
  constructor() {
    FakeSocket.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
}
class FakeEventSource extends EventTarget {
  close = vi.fn();
}
const agentId = '11111111-1111-4111-8111-111111111111';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
let frames: FrameRequestCallback[];
let fetcher: ReturnType<typeof vi.fn<typeof fetch>>;
let page: ScionPageTerminal;

beforeAll(async () => {
  await import('./terminal.js');
});
beforeEach(() => {
  terminal.instances.length = 0;
  FakeSocket.instances = [];
  frames = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
    frames.push(callback);
    return frames.length;
  });
  fetcher = vi
    .fn<typeof fetch>()
    .mockImplementation(() =>
      Promise.resolve(json({ id: agentId, name: 'test', phase: 'running' }))
    );
  vi.stubGlobal('fetch', fetcher);
  page = document.createElement('scion-page-terminal');
  page.agentId = agentId;
  page.pageData = {
    path: `/agents/${agentId}/terminal`,
    title: 'Terminal',
    user: { id: 'account-1', email: 'test@example.com', name: 'Test' },
  };
});
afterEach(() => {
  page.remove();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function mountToFrame() {
  document.body.append(page);
  await vi.waitFor(() => expect(frames).toHaveLength(1));
}
async function mountConnected() {
  await mountToFrame();
  frames.shift()?.(0);
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
  FakeSocket.instances[0].open();
  await page.updateComplete;
}

describe('legacy terminal transport adapter', () => {
  it('requires authenticated bootstrap identity before fetching or attaching', async () => {
    page.pageData = null;
    document.body.append(page);
    await page.updateComplete;
    expect(page.shadowRoot?.textContent).toContain('Authentication required');
    expect(fetcher).not.toHaveBeenCalled();
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it('navigation during animation-frame initialization cannot attach later', async () => {
    await mountToFrame();
    page.remove();
    frames.shift()?.(0);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(FakeSocket.instances).toHaveLength(0);
    expect(terminal.instances[0].dispose).toHaveBeenCalledTimes(1);
  });

  it('closes navigation transport without a detach frame and disposes xterm once', async () => {
    await mountConnected();
    const socket = FakeSocket.instances[0];
    socket.send.mockClear();
    page.remove();
    expect(socket.send).not.toHaveBeenCalled();
    expect(socket.close).toHaveBeenCalledTimes(1);
    expect(terminal.instances[0].dispose).toHaveBeenCalledTimes(1);
  });

  it('reconnect keeps the old terminal on denial and resets only after a new socket opens', async () => {
    await mountConnected();
    const socket = FakeSocket.instances[0];
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });
    await page.updateComplete;
    fetcher
      .mockResolvedValueOnce(json({ id: agentId, name: 'test', phase: 'running' }))
      .mockResolvedValueOnce(json({}, 403));
    page.shadowRoot?.querySelector<HTMLButtonElement>('.reconnect-btn')?.click();
    await vi.waitFor(() => expect(page.shadowRoot?.textContent).toContain('permission'));
    expect(terminal.instances[0].dispose).not.toHaveBeenCalled();
    expect(terminal.instances[0].reset).not.toHaveBeenCalled();
    page.shadowRoot?.querySelector<HTMLButtonElement>('.reconnect-btn')?.click();
    await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2));
    expect(terminal.instances[0].reset).not.toHaveBeenCalled();
    FakeSocket.instances[1].open();
    expect(terminal.instances[0].reset).toHaveBeenCalledTimes(1);
    expect(terminal.instances).toHaveLength(1);
  });
});

it('connection notifications do not overwrite metadata refreshed by page controls', async () => {
  await mountConnected();
  const controls = page as unknown as { refreshAgentData(): Promise<void>; sendResize(): void };
  fetcher.mockResolvedValueOnce(json({ id: agentId, name: 'test', phase: 'stopped' }));
  await controls.refreshAgentData();
  controls.sendResize();
  await page.updateComplete;
  expect(page.shadowRoot?.querySelector('scion-status-badge')?.getAttribute('status')).toBe(
    'stopped'
  );
});
