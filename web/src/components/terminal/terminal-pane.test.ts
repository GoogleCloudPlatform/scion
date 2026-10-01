// @vitest-environment happy-dom
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScionTerminalPane } from './terminal-pane.js';
import { TerminalSessionRegistry } from '../../client/terminal-sessions.js';

const showToast = vi.fn();
vi.mock('../../utils/toast.js', () => ({ showToast }));

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

// The pane chrome (toolbar, buttons, dialogs, loading/error states) must
// follow the app theme; only the terminal viewport and the overlays drawn on
// it may pin a literal (dark) palette. This is a denylist over every rule, so
// new chrome rules are covered without updating a selector list.
// See miller79/scion#133.
describe('theme', () => {
  let TERMINAL_BACKGROUND = '';
  beforeAll(async () => {
    ({ TERMINAL_BACKGROUND } = await import('./terminal-pane.js'));
  });
  // A viewport selector must end the class name exactly (so, for example,
  // .terminal-wrapper-foo is not exempt).
  const VIEWPORT =
    /^\.(terminal-wrapper|terminal-container|disconnected-overlay|drop-overlay)(?=[\s.:#[>+~]|$)/;
  // Colour-bearing properties, including shadows and every custom property
  // (for example --sl-panel-background-color or --indicator-color); custom
  // properties with non-colour values never match LITERAL_COLOR.
  const COLOR_PROPS =
    /^(color|background(-color)?|border(-(top|right|bottom|left))?(-color)?|outline(-color)?|fill|stroke|box-shadow|text-shadow|--[\w-]+)$/;
  const LITERAL_COLOR = /#[0-9a-f]{3,8}\b|\b(rgba?|hsla?)\(|\b(white|black)\b/i;
  // var(--scion-x) and var(--scion-x, <fallback>), including one level of
  // nested parentheses in the fallback (for example rgba(...)).
  const SCION_VAR = /var\(\s*--scion-[\w-]+\s*(?:,(?:[^()]|\([^()]*\))*)?\)/g;

  /** Leaf style rules from Lit cssText, skipping @keyframes contents. */
  function styleRules(cssText: string): Array<{ selector: string; body: string }> {
    const rules: Array<{ selector: string; body: string }> = [];
    const stack: string[] = [];
    let buf = '';
    for (const ch of cssText.replace(/\/\*[\s\S]*?\*\//g, '')) {
      if (ch === '{') {
        stack.push(buf.trim());
        buf = '';
      } else if (ch === '}') {
        const selector = stack.pop() ?? '';
        if (!selector.startsWith('@') && !stack.some((s) => s.startsWith('@keyframes'))) {
          rules.push({ selector, body: buf });
        }
        buf = '';
      } else {
        buf += ch;
      }
    }
    return rules;
  }

  function paneStyles(): string {
    const ctor = customElements.get('scion-terminal-pane') as unknown as {
      styles: { cssText: string };
    };
    return ctor.styles.cssText;
  }

  it('uses only --scion-* tokens for colours outside the terminal viewport', () => {
    const rules = styleRules(paneStyles());
    expect(rules.length).toBeGreaterThan(20);
    const offenders: string[] = [];
    for (const { selector, body } of rules) {
      const parts = selector.split(',').map((p) => p.trim());
      if (parts.every((p) => VIEWPORT.test(p))) continue;
      for (const decl of body.split(';')) {
        const idx = decl.indexOf(':');
        if (idx < 0) continue;
        const prop = decl.slice(0, idx).trim();
        const value = decl.slice(idx + 1).trim();
        if (!COLOR_PROPS.test(prop)) continue;
        if (LITERAL_COLOR.test(value.replace(SCION_VAR, ''))) {
          offenders.push(`${selector} { ${prop}: ${value} }`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  it('themes the host and keeps the viewport on the xterm background', () => {
    const rules = styleRules(paneStyles());
    const host = rules.find((r) => r.selector === ':host')?.body ?? '';
    expect(host).toMatch(/background:\s*var\(--scion-surface\b/);
    expect(host).toMatch(/(^|;)\s*color:\s*var\(--scion-text\b/);
    const wrapper = rules.find((r) => r.selector === '.terminal-wrapper')?.body ?? '';
    const bg = /(^|;)\s*background:\s*([^;]+)/.exec(wrapper)?.[2].trim();
    expect(bg?.toLowerCase()).toBe(TERMINAL_BACKGROUND.toLowerCase());
  });

  // The colour denylist cannot see a control with no rule at all: an
  // unstyled <button> falls back to the browser's native palette in both
  // themes (GoogleCloudPlatform/scion#2011 review). Every chrome button must
  // be matched by at least one static style rule.
  it('styles every chrome button outside the terminal viewport, in every render branch', async () => {
    // Base selectors: a button must have a rule that applies at rest, so
    // interaction-state rules (:hover, :focus, :active, :disabled) do not
    // count. Remaining structural pseudo-classes (:first-child, ...) are
    // stripped, and :host rules are skipped (not matchable from inside).
    const selectors = styleRules(paneStyles())
      .flatMap((r) => r.selector.split(','))
      .map((p) => p.trim())
      .filter((p) => p && !p.startsWith(':'))
      .filter((p) => !/:(hover|focus|focus-visible|focus-within|active|disabled)\b/.test(p))
      .map((p) => p.replace(/:[\w-]+(\([^)]*\))?/g, '').trim())
      .filter(Boolean);
    const seen = new Set<string>();
    const unstyled: string[] = [];
    const collect = async () => {
      await page.updateComplete;
      for (const b of Array.from(page.shadowRoot!.querySelectorAll<HTMLElement>('button'))) {
        if (b.closest('.terminal-wrapper')) continue;
        const label = b.className || `${b.parentElement?.className ?? ''} > button`;
        seen.add(label.trim());
        if (!selectors.some((sel) => b.matches(sel))) unstyled.push(b.outerHTML.slice(0, 80));
      }
    };
    const pane = page as unknown as {
      error: string | null;
      metadataError: string | null;
      projectId: string;
      agent: Record<string, unknown> | null;
      terminal: unknown;
    };

    // 1. Connected, with every optional toolbar control and the metadata
    //    banner: toggles, graph action, capture auth, ports, metadata retry.
    await mountConnected();
    pane.projectId = 'project-1';
    pane.agent = {
      ...(pane.agent ?? {}),
      id: agentId,
      phase: 'running',
      harnessAuth: 'none',
      resolvedHarness: 'claude',
    };
    pane.error = 'metadata unavailable';
    pane.metadataError = 'metadata unavailable';
    await collect();

    // 2. Disconnected (detached): the toolbar Reconnect button.
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1000 });
    await collect();

    // 3. The session-error branch (no terminal host): the error-state Retry.
    const realSession = page.session!;
    Object.defineProperty(page, 'session', {
      configurable: true,
      get: () => ({ ...realSession, state: { ...realSession.state, error: 'boom' } }),
    });
    const realTerminal = pane.terminal;
    pane.terminal = null;
    page.requestUpdate();
    await collect();
    pane.terminal = realTerminal;
    delete (page as unknown as { session?: unknown }).session;

    // The 8 chrome buttons across the three branches (the inactive window
    // toggle has no class, so it is labelled by its parent).
    expect([...seen].sort()).toEqual([
      'active',
      'capture-auth-btn',
      'error-state > button',
      'metadata-retry',
      'pane-action-btn',
      'port-dropdown-trigger',
      'reconnect-btn',
      'toggle-group > button',
    ]);
    expect(unstyled).toEqual([]);
  });

  // Inline style attributes are not in static styles, so check the rendered
  // chrome too, with the metadata error banner (the one conditional
  // strip outside the viewport) showing.
  it('renders no literal colours in inline styles outside the terminal viewport', async () => {
    await mountConnected();
    const state = page as unknown as { error: string | null; metadataError: string | null };
    state.error = 'metadata unavailable';
    state.metadataError = 'metadata unavailable';
    await page.updateComplete;
    const root = page.shadowRoot!;
    expect(root.querySelector('.error-banner')).not.toBeNull();
    const offenders = Array.from(root.querySelectorAll<HTMLElement>('[style]'))
      .filter((el) => !el.closest('.terminal-wrapper'))
      .map((el) => el.getAttribute('style') ?? '')
      .filter((style) => LITERAL_COLOR.test(style.replace(SCION_VAR, '')));
    expect(offenders).toEqual([]);
  });
});

describe('Capture Auth scope dialog (design ptone/scion#2291 §7)', () => {
  const noAuthAgent = {
    id: agentId,
    name: 'test',
    phase: 'running',
    harnessAuth: 'none',
    resolvedHarness: 'claude',
  };

  /** Makes showCaptureAuth true without going through the metadata registry's fetch. */
  async function makeCaptureEligible() {
    await mountToFrame();
    (page as unknown as { agent: unknown }).agent = noAuthAgent;
    await page.updateComplete;
  }

  function captureAuthButton(): HTMLButtonElement | null {
    return page.shadowRoot?.querySelector<HTMLButtonElement>('.capture-auth-btn') ?? null;
  }

  function scopeDialog(): HTMLElement | null {
    return (
      page.shadowRoot?.querySelector<HTMLElement>('sl-dialog[label="Capture Auth — Choose Scope"]') ??
      null
    );
  }

  function radio(value: 'project' | 'user'): HTMLElement | null {
    return scopeDialog()?.querySelector<HTMLElement>(`sl-radio[value="${value}"]`) ?? null;
  }

  async function openDialog() {
    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());
    await vi.waitFor(() => {
      const state = page as unknown as { captureAuthSettingsLoading: boolean };
      expect(state.captureAuthSettingsLoading).toBe(false);
    });
    await page.updateComplete;
  }

  function clickCapture() {
    scopeDialog()
      ?.querySelector<HTMLElement>('sl-button[variant="primary"]')
      ?.dispatchEvent(new Event('click', { bubbles: true, composed: true }));
  }

  beforeEach(() => {
    showToast.mockClear();
  });

  // Design §10 test 6: setting off — both radios enabled, default is
  // project, and exec sends --scope project.
  it('setting off: both radios enabled, default project, exec sends --scope project', async () => {
    await makeCaptureEligible();
    let execBody: { command: string[] } | null = null;
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        return Promise.resolve(json({ agentSecretsUserScopeOnly: false }));
      }
      if (path.includes('/exec')) {
        return Promise.resolve(json({ output: '', exitCode: 0 }));
      }
      return Promise.resolve(json(noAuthAgent));
    });

    await openDialog();

    expect(radio('project')?.hasAttribute('disabled')).toBe(false);
    expect(radio('user')?.hasAttribute('disabled')).toBe(false);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('project');

    fetcher.mockImplementation((url, init) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/exec')) {
        execBody = JSON.parse(init!.body as string) as { command: string[] };
        return Promise.resolve(json({ output: '', exitCode: 0 }));
      }
      return Promise.resolve(json(noAuthAgent));
    });
    clickCapture();
    await vi.waitFor(() => expect(execBody).not.toBeNull());
    expect(execBody!.command).toContain('--scope');
    expect(execBody!.command[execBody!.command.indexOf('--scope') + 1]).toBe('project');
  });

  // Design §10 test 7: setting on — Project disabled with helper text, the
  // selection is forced to user even if project was selected before, and
  // exec sends --scope user.
  it('setting on: Project disabled with helper text, selection forced to user, exec sends --scope user', async () => {
    await makeCaptureEligible();
    (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope = 'project';
    let execBody: { command: string[] } | null = null;
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        return Promise.resolve(json({ agentSecretsUserScopeOnly: true }));
      }
      if (path.includes('/exec')) {
        return Promise.resolve(json({ output: '', exitCode: 0 }));
      }
      return Promise.resolve(json(noAuthAgent));
    });

    await openDialog();

    expect(radio('project')?.hasAttribute('disabled')).toBe(true);
    expect(radio('user')?.hasAttribute('disabled')).toBe(false);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('user');
    expect(scopeDialog()?.textContent ?? '').toContain(
      'Disabled by your hub administrator: captured credentials can only be stored in'
    );

    fetcher.mockImplementation((url, init) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/exec')) {
        execBody = JSON.parse(init!.body as string) as { command: string[] };
        return Promise.resolve(json({ output: '', exitCode: 0 }));
      }
      return Promise.resolve(json(noAuthAgent));
    });
    clickCapture();
    await vi.waitFor(() => expect(execBody).not.toBeNull());
    expect(execBody!.command[execBody!.command.indexOf('--scope') + 1]).toBe('user');
  });

  // Design §10 test 8: the settings fetch fails — the dialog shows today's
  // (unrestricted) state, failing open. The server still enforces.
  it('settings fetch fails: dialog shows unrestricted state (fail open)', async () => {
    await makeCaptureEligible();
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        return Promise.resolve(new Response('', { status: 500 }));
      }
      return Promise.resolve(json(noAuthAgent));
    });

    await openDialog();

    expect(radio('project')?.hasAttribute('disabled')).toBe(false);
    expect(radio('user')?.hasAttribute('disabled')).toBe(false);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('project');
  });

  // Design §10 test 9: a secret_scope_restricted rejection shows the policy
  // toast, not "Capture failed", and does not open the conflict dialog.
  it('rejection: secret_scope_restricted shows the policy toast, not the conflict dialog', async () => {
    await makeCaptureEligible();
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        return Promise.resolve(json({ agentSecretsUserScopeOnly: false }));
      }
      if (path.includes('/exec')) {
        return Promise.resolve(
          json({
            output:
              'hub returned error 403: {"error":{"code":"secret_scope_restricted","message":"..."}}',
            exitCode: 1,
          })
        );
      }
      return Promise.resolve(json(noAuthAgent));
    });

    await openDialog();
    clickCapture();

    await vi.waitFor(() => expect(showToast).toHaveBeenCalled());
    expect(showToast.mock.calls[0][0]).toContain(
      'Your hub administrator only allows capturing credentials to your profile.'
    );
    expect(showToast.mock.calls[0][0]).not.toContain('Capture failed');

    const conflicts = (page as unknown as { captureAuthConflicts: string[] | null })
      .captureAuthConflicts;
    expect(conflicts).toBeNull();
  });

  /** A promise plus its resolver, for controlling exactly when a mocked fetch settles. */
  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((r) => {
      resolve = r;
    });
    return { promise, resolve };
  }

  // Round-1 review R1: sl-radio-group has no `disabled` property in
  // Shoelace 2.x, so the disable must live on each sl-radio. While the
  // settings fetch is in flight, both radios and Capture must be disabled;
  // once it resolves, they must return to the normal (unrestricted) state.
  it('while the settings fetch is in flight: both radios and Capture are disabled', async () => {
    await makeCaptureEligible();
    const pending = deferred<Response>();
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        return pending.promise;
      }
      return Promise.resolve(json(noAuthAgent));
    });

    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());
    await page.updateComplete;

    expect(
      (page as unknown as { captureAuthSettingsLoading: boolean }).captureAuthSettingsLoading
    ).toBe(true);
    expect(radio('project')?.hasAttribute('disabled')).toBe(true);
    expect(radio('user')?.hasAttribute('disabled')).toBe(true);
    expect(
      scopeDialog()?.querySelector('sl-button[variant="primary"]')?.hasAttribute('disabled')
    ).toBe(true);

    pending.resolve(json({ agentSecretsUserScopeOnly: false }));
    await vi.waitFor(() => {
      const state = page as unknown as { captureAuthSettingsLoading: boolean };
      expect(state.captureAuthSettingsLoading).toBe(false);
    });
    await page.updateComplete;

    expect(radio('project')?.hasAttribute('disabled')).toBe(false);
    expect(radio('user')?.hasAttribute('disabled')).toBe(false);
    expect(
      scopeDialog()?.querySelector('sl-button[variant="primary"]')?.hasAttribute('disabled')
    ).toBe(false);
  });

  // Round-1 review N2: an overlapping settings fetch from a fast
  // close/reopen must not let the older, slower response win, and must not
  // clear loading out from under the newer request.
  it('overlapping settings fetches: only the latest request applies', async () => {
    await makeCaptureEligible();
    const first = deferred<Response>();
    const second = deferred<Response>();
    let call = 0;
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        call += 1;
        return call === 1 ? first.promise : second.promise;
      }
      return Promise.resolve(json(noAuthAgent));
    });

    // First open starts the first (slow) fetch.
    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());

    // Close and reopen before the first fetch resolves — this starts the
    // second (fast) fetch while the first is still pending.
    (page as unknown as { captureAuthScopeDialogOpen: boolean }).captureAuthScopeDialogOpen =
      false;
    await page.updateComplete;
    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());

    // The second (newer) request resolves first, with the setting on.
    second.resolve(json({ agentSecretsUserScopeOnly: true }));
    await vi.waitFor(() => {
      const state = page as unknown as { captureAuthSettingsLoading: boolean };
      expect(state.captureAuthSettingsLoading).toBe(false);
    });
    await page.updateComplete;
    expect(
      (page as unknown as { agentSecretsUserScopeOnly: boolean }).agentSecretsUserScopeOnly
    ).toBe(true);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('user');

    // The first (stale) request now resolves, with the setting off. It must
    // not overwrite the newer result or re-enable loading.
    first.resolve(json({ agentSecretsUserScopeOnly: false }));
    await Promise.resolve();
    await Promise.resolve();
    await page.updateComplete;

    expect(
      (page as unknown as { captureAuthSettingsLoading: boolean }).captureAuthSettingsLoading
    ).toBe(false);
    expect(
      (page as unknown as { agentSecretsUserScopeOnly: boolean }).agentSecretsUserScopeOnly
    ).toBe(true);
    expect(radio('project')?.hasAttribute('disabled')).toBe(true);
  });

  // Round-2 review nit 2: the other resolution order. The stale (older)
  // fetch settles first, while the newer one is still pending — it must
  // not apply its result, and loading must stay true (the dialog must not
  // look "ready" based on a stale response) until the newer fetch settles.
  it('overlapping settings fetches, stale-first order: the older result never applies and loading stays true until the newer one settles', async () => {
    await makeCaptureEligible();
    const first = deferred<Response>();
    const second = deferred<Response>();
    let call = 0;
    fetcher.mockImplementation((url) => {
      const path = typeof url === 'string' ? url : url.toString();
      if (path.includes('/api/v1/settings/public')) {
        call += 1;
        return call === 1 ? first.promise : second.promise;
      }
      return Promise.resolve(json(noAuthAgent));
    });

    // First open starts the first (stale-to-be) fetch.
    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());

    // Close and reopen before it resolves — starts the second (newer) fetch.
    (page as unknown as { captureAuthScopeDialogOpen: boolean }).captureAuthScopeDialogOpen =
      false;
    await page.updateComplete;
    captureAuthButton()!.click();
    await vi.waitFor(() => expect(scopeDialog()).not.toBeNull());

    // The first (older, stale) request resolves first, with the setting
    // off. Applying this would be wrong on two counts: it is not the latest
    // request, and it would incorrectly clear loading while the newer
    // fetch is still in flight.
    first.resolve(json({ agentSecretsUserScopeOnly: false }));
    await Promise.resolve();
    await Promise.resolve();
    await page.updateComplete;

    expect(
      (page as unknown as { captureAuthSettingsLoading: boolean }).captureAuthSettingsLoading
    ).toBe(true);
    expect(
      (page as unknown as { agentSecretsUserScopeOnly: boolean }).agentSecretsUserScopeOnly
    ).toBe(false);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('project');
    expect(radio('project')?.hasAttribute('disabled')).toBe(true); // still loading
    expect(radio('user')?.hasAttribute('disabled')).toBe(true); // still loading

    // The second (newer) request now resolves, with the setting on. This is
    // the one that must actually apply. Resolving it with a value that
    // differs from the stale one (and from the default) makes "applied" and
    // "not applied" observably distinguishable, unlike resolving both with
    // the already-default `false`.
    second.resolve(json({ agentSecretsUserScopeOnly: true }));
    await vi.waitFor(() => {
      const state = page as unknown as { captureAuthSettingsLoading: boolean };
      expect(state.captureAuthSettingsLoading).toBe(false);
    });
    await page.updateComplete;

    expect(
      (page as unknown as { agentSecretsUserScopeOnly: boolean }).agentSecretsUserScopeOnly
    ).toBe(true);
    expect(
      (page as unknown as { captureAuthSelectedScope: string }).captureAuthSelectedScope
    ).toBe('user');
    expect(radio('project')?.hasAttribute('disabled')).toBe(true); // restricted
    expect(radio('user')?.hasAttribute('disabled')).toBe(false);
  });
});
