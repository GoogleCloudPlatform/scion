// @vitest-environment happy-dom
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScionTerminalPane } from './terminal-pane.js';
import { TerminalSessionRegistry } from '../../client/terminal-sessions.js';

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
    blur = vi.fn();
    refresh = vi.fn();
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
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  constructor(readonly url: string) {
    super();
    FakeEventSource.instances.push(this);
  }
  close = vi.fn();
}
const agentId = '11111111-1111-4111-8111-111111111111';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
let frames: FrameRequestCallback[];
let fetcher: ReturnType<typeof vi.fn<typeof fetch>>;
let page: ScionTerminalPane;
let registry: TerminalSessionRegistry;

beforeAll(async () => {
  await import('./terminal-pane.js');
});
beforeEach(() => {
  terminal.instances.length = 0;
  FakeSocket.instances = [];
  FakeEventSource.instances = [];
  frames = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
    frames.push(callback);
    return frames.length;
  });
  fetcher = vi.fn<typeof fetch>();
  fetcher.mockImplementation(() =>
    Promise.resolve(
      json({
        id: agentId,
        name: 'test',
        phase: 'running',
        exposedPorts: [3000, 3001, 3002, 3003].map((port) => ({ port })),
      })
    )
  );
  vi.stubGlobal('fetch', fetcher);
  page = document.createElement('scion-terminal-pane');
  registry = new TerminalSessionRegistry({
    hubUrl: window.location.origin,
    accountId: 'account-1',
  });
  page.open(registry, agentId);
  vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(500);
});
afterEach(() => {
  page.dispose();
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

describe('retained terminal pane', () => {
  it('keeps the same host and attach across hide, route changes and DOM remount', async () => {
    await mountConnected();
    const host = page.shadowRoot?.querySelector('.terminal-container');
    const session = page.session;
    const socket = FakeSocket.instances[0];
    socket.send.mockClear();
    page.setVisible(false);
    history.replaceState(null, '', '/dashboard');
    page.remove();
    expect(socket.close).not.toHaveBeenCalled();
    expect(terminal.instances[0].dispose).not.toHaveBeenCalled();
    document.body.append(page);
    page.setVisible(true);
    await page.updateComplete;
    expect(page.session).toBe(session);
    expect(page.shadowRoot?.querySelector('.terminal-container')).toBe(host);
    expect(FakeSocket.instances).toHaveLength(1);
    expect(socket.send).not.toHaveBeenCalled();
  });

  it('explicit close during initialization disposes once and cannot attach later', async () => {
    await mountToFrame();
    page.dispose();
    page.dispose();
    frames.shift()?.(0);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(FakeSocket.instances).toHaveLength(0);
    expect(terminal.instances[0].dispose).toHaveBeenCalledTimes(1);
    expect(page.session?.state.connection).toBe('closed');
  });

  it('external session close releases resources and explicit close detaches once', async () => {
    await mountConnected();
    const socket = FakeSocket.instances[0];
    socket.send.mockClear();
    page.session?.close();
    page.dispose();
    expect(socket.send).toHaveBeenCalledExactlyOnceWith(
      JSON.stringify({ type: 'data', data: btoa('\x02d') })
    );
    expect(socket.close).toHaveBeenCalledTimes(1);
    expect(terminal.instances[0].dispose).toHaveBeenCalledTimes(1);
  });

  it('disposal removes an open ports dropdown document listener', async () => {
    await mountConnected();
    const added = vi.spyOn(document, 'addEventListener');
    const removed = vi.spyOn(document, 'removeEventListener');
    page.shadowRoot?.querySelector<HTMLButtonElement>('.port-dropdown-trigger')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    const close = added.mock.calls.find(([event]) => event === 'click')?.[1];
    expect(close).toBeDefined();
    page.dispose();
    expect(removed).toHaveBeenCalledWith('click', close);
  });

  it('requires explicit identity and prevents rebinding a session to another pane', () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test',
    });
    const first = document.createElement('scion-terminal-pane');
    const second = document.createElement('scion-terminal-pane');
    const session = first.open(registry, agentId);
    expect(first.open(registry, agentId)).toBe(session);
    expect(() => second.open(registry, agentId)).toThrow('already has a pane');
    expect(second.agentId).toBe('');
    second.dispose();
    expect(session.state.connection).not.toBe('closed');
    expect(() => first.open(registry, '22222222-2222-4222-8222-222222222222')).toThrow(
      'cannot be rebound'
    );
    first.dispose();
    expect(() => first.open(registry, agentId)).toThrow('disposed');
  });
});

it('two panes share registry SSE and preserve metadata across transport notifications', async () => {
  await mountConnected();
  const otherId = '22222222-2222-4222-8222-222222222222';
  const other = document.createElement('scion-terminal-pane');
  fetcher.mockImplementation((url) =>
    Promise.resolve(
      json({
        id: String(url).includes(otherId) ? otherId : agentId,
        name: 'test',
        phase: 'running',
      })
    )
  );
  other.open(registry, otherId);
  document.body.append(other);
  try {
    await vi.waitFor(() => {
      frames.splice(0).forEach((frame) => frame(0));
      expect(FakeSocket.instances).toHaveLength(2);
    });
    expect(terminal.instances).toHaveLength(2);
    FakeSocket.instances.forEach((socket) => socket.open());
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0].close).toHaveBeenCalledTimes(1);
    const source = FakeEventSource.instances[1];
    source.onopen?.();
    await vi.waitFor(() => expect(registry.metadata.get(agentId)?.availability).toBe('ready'));
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({
          subject: `agent.${agentId}.status`,
          data: { phase: 'stopped' },
        }),
      })
    );
    page.session?.resize(90, 30);
    await page.updateComplete;
    expect(page.shadowRoot?.querySelector('scion-status-badge')?.getAttribute('status')).toBe(
      'stopped'
    );
    expect(other.shadowRoot?.querySelector('scion-status-badge')?.getAttribute('status')).toBe(
      'running'
    );
    page.setVisible(false);
    page.remove();
    expect(source.close).not.toHaveBeenCalled();
    other.dispose();
    page.dispose();
    expect(source.close).toHaveBeenCalledTimes(1);
    expect(registry.list()).toEqual([]);
  } finally {
    other.dispose();
    other.remove();
  }
});

it('a failed metadata snapshot does not remove the independently authorized terminal host', async () => {
  page.dispose();
  FakeEventSource.instances = [];
  page = document.createElement('scion-terminal-pane');
  registry = new TerminalSessionRegistry({
    hubUrl: window.location.origin,
    accountId: 'account-1',
  });
  let resolve!: (response: Response) => void;
  const gate = new Promise<Response>((r) => {
    resolve = r;
  });
  fetcher.mockReturnValueOnce(gate).mockResolvedValueOnce(json({}, 503));
  page.open(registry, agentId);
  document.body.append(page);
  await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
  FakeEventSource.instances[0].onopen?.();
  await vi.waitFor(() => expect(registry.metadata.get(agentId)?.availability).toBe('unavailable'));
  resolve(json({ id: agentId, name: 'test', phase: 'running' }));
  await vi.waitFor(() => {
    frames.splice(0).forEach((frame) => frame(0));
    expect(FakeSocket.instances).toHaveLength(1);
  });
  expect(page.shadowRoot?.querySelector('.terminal-container')).not.toBeNull();
  expect(page.shadowRoot?.textContent).toContain('metadata unavailable');
});
