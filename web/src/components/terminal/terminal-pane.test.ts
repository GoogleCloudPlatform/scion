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
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  send = vi.fn();
  close = vi.fn();
  constructor() {
    FakeSocket.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
  /**
   * A data frame is what actually confirms the stream is live, not bare
   * onopen. Simulates tmux's redraw on attach.
   */
  data(payload = '') {
    this.onmessage?.({ data: JSON.stringify({ type: 'data', data: btoa(payload) }) });
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
  // Wait for initTerminal to complete through its RAF push.
  // terminal.instances tracks mocked Terminal constructors; once length is 1,
  // initTerminal has set this.terminal and all synchronous operations through
  // the RAF push have completed (no async gaps between constructor and RAF).
  // Use >= 1 because reveal() may also push a RAF if it wins the race.
  await vi.waitFor(() => {
    expect(terminal.instances).toHaveLength(1);
    expect(frames.length).toBeGreaterThanOrEqual(1);
  });
}
async function mountConnected() {
  await mountToFrame();
  frames.shift()?.(0);
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
  FakeSocket.instances[0].open();
  FakeSocket.instances[0].data(); // confirms the stream live
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

describe('hidden pane interaction isolation (P1.8)', () => {
  it('setVisible(false) blurs terminal, cancels pending resize and removes window drag prevention', async () => {
    await mountConnected();
    const xt = terminal.instances[0];
    expect(xt.blur).not.toHaveBeenCalled();
    // Verify window drag handlers are installed when visible
    const addSpy = vi.spyOn(window, 'addEventListener');
    const removeSpy = vi.spyOn(window, 'removeEventListener');
    page.setVisible(false);
    expect(xt.blur).toHaveBeenCalled();
    // Window drag prevention removed when hidden
    expect(removeSpy).toHaveBeenCalledWith('dragover', expect.any(Function));
    expect(removeSpy).toHaveBeenCalledWith('drop', expect.any(Function));
    // setVisible(true) reinstalls them
    page.setVisible(true);
    expect(addSpy).toHaveBeenCalledWith('dragover', expect.any(Function));
    expect(addSpy).toHaveBeenCalledWith('drop', expect.any(Function));
  });

  it('hidden pane does not auto-focus on late socket connect', async () => {
    await mountToFrame();
    frames.shift()?.(0);
    await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    // Hide before socket opens
    page.setVisible(false);
    const xt = terminal.instances[0];
    xt.focus.mockClear();
    FakeSocket.instances[0].open();
    FakeSocket.instances[0].data();
    await page.updateComplete;
    // Terminal should NOT have been focused since pane is hidden
    expect(xt.focus).not.toHaveBeenCalled();
  });

  it('window drag prevention is not installed when pane starts hidden', () => {
    const pane2 = document.createElement('scion-terminal-pane');
    const reg2 = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test-hidden',
    });
    pane2.setVisible(false);
    const addSpy = vi.spyOn(window, 'addEventListener');
    document.body.append(pane2);
    const dragOverCalls = addSpy.mock.calls.filter(([event]) => event === 'dragover');
    expect(dragOverCalls).toHaveLength(0);
    pane2.open(reg2, '33333333-3333-4333-8333-333333333333');
    pane2.dispose();
    pane2.remove();
  });
});

describe('OSC 0 window-state tracking (F1 fix)', () => {
  // Retrieve an OSC handler registered on the mock terminal by OSC number.
  function getOscHandler(oscId: number): ((data: string) => boolean) | undefined {
    const xt = terminal.instances[0] as unknown as {
      parser: { registerOscHandler: ReturnType<typeof vi.fn> };
    };
    const call = xt.parser.registerOscHandler.mock.calls.find((c: unknown[]) => c[0] === oscId);
    return call?.[1] as ((data: string) => boolean) | undefined;
  }

  it('OSC 0 "agent" sets activeWindow to agent', async () => {
    await mountConnected();
    const handler = getOscHandler(0);
    expect(handler).toBeDefined();
    handler!('agent');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('agent');
  });

  it('OSC 0 "shell" sets activeWindow to shell', async () => {
    await mountConnected();
    const handler = getOscHandler(0);
    expect(handler).toBeDefined();
    handler!('shell');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
  });

  it('OSC 0 with unknown value does not change activeWindow', async () => {
    await mountConnected();
    const handler = getOscHandler(0);
    expect(handler).toBeDefined();
    // Set a known baseline via OSC 7337
    const osc7337 = getOscHandler(7337)!;
    osc7337('tmuxwindow=shell');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
    // Unknown values should be ignored
    handler!('bash');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
    handler!('');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
  });

  it('OSC 0 overrides OSC 7337 — last value wins', async () => {
    await mountConnected();
    const osc7337 = getOscHandler(7337)!;
    const osc0 = getOscHandler(0)!;
    // OSC 7337 sets agent
    osc7337('tmuxwindow=agent');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('agent');
    // OSC 0 overrides to shell
    osc0('shell');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
  });

  it('OSC 7337 still works as initial state fallback', async () => {
    await mountConnected();
    const osc7337 = getOscHandler(7337)!;
    osc7337('tmuxwindow=shell');
    expect((page as unknown as { activeWindow: string }).activeWindow).toBe('shell');
  });
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

describe('bind-after-mount still arms frontmost', () => {
  it('a pane mounted before open() (the legacy page order) still auto-reconnects on a retriable close', async () => {
    const registry2 = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'account-r3',
    });
    const page2 = document.createElement('scion-terminal-pane') as ScionTerminalPane;
    // connectedCallback runs with no session bound yet — the exact order that
    // pages/terminal.ts uses (mount the shell, then open()).
    document.body.append(page2);
    try {
      page2.open(registry2, agentId);
      await vi.waitFor(() => {
        frames.splice(0).forEach((frame) => frame(0));
        expect(FakeSocket.instances.length).toBeGreaterThanOrEqual(1);
      });
      const socket = FakeSocket.instances[FakeSocket.instances.length - 1];
      socket.open();
      socket.data(); // confirms the stream live before the drop below
      await page2.updateComplete;
      const session = page2.session!;
      socket.readyState = 3;
      socket.onclose?.({ code: 1006 });
      // Without binding frontmost state on this path, it would stay false
      // forever, and this session would never attempt again without an
      // explicit visibility event.
      expect(session.reconnecting).toBe(true);
    } finally {
      page2.dispose();
      page2.remove();
    }
  });
});

describe('document visibilitychange feeds frontmost', () => {
  afterEach(() => {
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
  });

  it('a hidden document suppresses auto-reconnect; becoming visible triggers it', async () => {
    await mountConnected();
    const session = page.session!;
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    document.dispatchEvent(new Event('visibilitychange'));

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });
    expect(session.reconnecting).toBe(false);
    expect(FakeSocket.instances).toHaveLength(1);

    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    document.dispatchEvent(new Event('visibilitychange'));
    expect(session.reconnecting).toBe(true);
  });

  it('removes the visibilitychange listener on disconnectedCallback', async () => {
    await mountConnected();
    const removed = vi.spyOn(document, 'removeEventListener');
    page.remove();
    expect(removed).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
  });
});

describe('overlay strings', () => {
  it('shows RECONNECTING... with a spinner while an attempt is in flight', async () => {
    await mountConnected();
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 4503 });
    await page.updateComplete;
    expect(page.shadowRoot?.textContent).toContain('RECONNECTING...');
    expect(page.shadowRoot?.querySelector('.disconnected-overlay sl-spinner')).toBeTruthy();
  });

  // Pins the intended derivation, not a regression guard.
  // `applySessionState` only runs on session update(), and `pending` clears
  // in `finally` without one, so the pane's last-seen `reconnecting` value
  // never actually goes stale under today's code — nothing else notifies
  // while `connecting` (resize/sendData are gated on `connected`, and pongs
  // don't notify either). This assertion therefore cannot fail if `attempting`
  // is reverted to derive from `session.reconnecting`; keeping the
  // connection-based derivation is still correct defensively.
  it('shows RECONNECTING... while still waiting for the handshake, after session.reconnecting has cleared', async () => {
    await mountConnected();
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 4503 }); // triggers the automatic attempt
    await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2));
    // The reconnect socket now exists (connection === 'connecting'), but it
    // has not opened yet, and `pending` (session.reconnecting) has already
    // cleared, well before the handshake finishes.
    await vi.waitFor(() => expect(page.session?.reconnecting).toBe(false));
    await page.updateComplete;
    expect(page.shadowRoot?.textContent).toContain('RECONNECTING...');
    expect(page.shadowRoot?.querySelector('.disconnected-overlay sl-spinner')).toBeTruthy();
  });

  it('shows the exact copy, pinned rather than matched as a substring, once an automatic attempt fails', async () => {
    await mountConnected();
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 4503 }); // frontmost: one automatic attempt
    await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2));
    FakeSocket.instances[1].readyState = 3;
    FakeSocket.instances[1].onclose?.({ code: 4503 }); // that attempt also fails
    await page.updateComplete;
    // toBe (not toContain), so a stray trailing period fails.
    expect(page.shadowRoot?.querySelector('.overlay-detail')?.textContent?.trim()).toBe(
      'Automatic reconnection failed, try manually reconnecting later'
    );
  });

  it('shows the exact neutral copy, pinned rather than matched as a substring, after a failed manual reconnect', async () => {
    await mountConnected();
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1000 }); // detached: terminal, no auto attempt
    await page.updateComplete;
    fetcher
      .mockResolvedValueOnce(json({ id: agentId, name: 'test', phase: 'running' }))
      .mockResolvedValueOnce(json({}, 503));
    page.shadowRoot?.querySelector<HTMLButtonElement>('.overlay-reconnect')?.click();
    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('.overlay-detail')?.textContent?.trim()).toBe(
        'Reconnection failed, try manually reconnecting later'
      );
    });
    expect(FakeSocket.instances).toHaveLength(1); // preflight failed before a new socket
  });
});
