// @vitest-environment happy-dom
/**
 * Tests for TerminalWorkspaceRoot: data-effective-layout attribute
 * and focus outline suppression in single-pane mode (#1716).
 */
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
  type MockInstance,
} from 'vitest';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { TerminalSessionRegistry } from './terminal-sessions.js';
import { _appFrameRefCountForTests } from '../components/shared/app-frame.js';
import {
  TERMINAL_PALETTE_NEW_AGENT_EVENT,
  type TerminalPaletteNewAgentDetail,
} from './terminal-workspace-events.js';
import type { ScionQuickPalette } from '../components/shared/palette/quick-palette.js';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import type { TerminalPaletteAgentsLoadOptions } from './terminal-palette-data.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import { TOUCH_PRIMARY_QUERY } from '../utils/input-modality.js';

// Mock terminal-pane custom element before importing workspace root
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
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

/**
 * Lets a test replace the palette's Agents load; every other test gets the
 * real one, which reads the stubbed fetch.
 */
const paletteLoad = vi.hoisted(() => ({
  override: null as
    | null
    | ((options: TerminalPaletteAgentsLoadOptions) => Promise<PaletteCandidate[]>),
}));
vi.mock('./terminal-palette-data.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./terminal-palette-data.js')>();
  return {
    ...actual,
    loadTerminalPaletteAgents: (
      options: TerminalPaletteAgentsLoadOptions
    ): Promise<PaletteCandidate[]> =>
      paletteLoad.override
        ? paletteLoad.override(options)
        : actual.loadTerminalPaletteAgents(options),
  };
});

let WorkspaceRoot: typeof TerminalWorkspaceRoot;

beforeAll(async () => {
  // Ensure custom element is registered
  await import('../components/terminal/terminal-pane.js');
  const mod = await import('./terminal-workspace-root.js');
  WorkspaceRoot = mod.TerminalWorkspaceRoot;
});

function getPaneHost(root: TerminalWorkspaceRoot): HTMLElement {
  return root.element.querySelector('.terminal-pane-host') as HTMLElement;
}

/** Flush queueMicrotask-based refresh. */
async function flush(): Promise<void> {
  await Promise.resolve();
}

describe('data-effective-layout attribute (#1716)', () => {
  let root: TerminalWorkspaceRoot;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              id: '11111111-1111-4111-8111-111111111111',
              name: 'test',
              phase: 'running',
            }),
            { status: 200 }
          )
        )
      )
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
        constructor(public url: string) {
          super();
        }
      }
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('defaults to single on initial load', () => {
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('single');
  });

  it('updates to two-columns when layout is changed', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-columns');
  });

  it('updates to two-rows when layout is changed', async () => {
    root.layoutManager.setLayout('two-rows');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-rows');
  });

  it('updates to four when layout is changed', async () => {
    root.layoutManager.setLayout('four');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('four');
  });

  it('reverts to single when switching back from multi-pane', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('two-columns');
    root.layoutManager.setLayout('single');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });

  it('overrides to single in narrow viewport regardless of active preset', async () => {
    // Simulate narrow viewport by mocking matchMedia
    const narrowQuery = { matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() };
    vi.spyOn(window, 'matchMedia').mockReturnValue(narrowQuery as unknown as MediaQueryList);

    // Recreate root with the narrow mock in place
    root.dispose();
    root.element.remove();
    root = new WorkspaceRoot();
    document.body.append(root.element);

    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');

    root.layoutManager.setLayout('four');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });

  it('overrides to single when a pane is zoomed', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test',
    });

    // Create a session so we can zoom it
    const session = root.create(registry, '11111111-1111-4111-8111-111111111111');
    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('two-columns');

    root.layoutManager.zoom(session.state.key);
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });
});

describe('focus outline suppression in single-pane mode (#1716)', () => {
  let root: TerminalWorkspaceRoot;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              id: '11111111-1111-4111-8111-111111111111',
              name: 'test',
              phase: 'running',
            }),
            { status: 200 }
          )
        )
      )
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
        constructor(public url: string) {
          super();
        }
      }
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('CSS focus rule targets only multi-pane layouts via data-effective-layout', () => {
    // Verify the installed stylesheet contains the scoped selector
    const styles = document.querySelectorAll('style');
    let found = false;
    for (const style of styles) {
      const text = style.textContent ?? '';
      if (
        text.includes("data-effective-layout='two-columns']") &&
        text.includes("data-effective-layout='two-rows']") &&
        text.includes("data-effective-layout='four']") &&
        text.includes('scion-terminal-pane[data-focused]')
      ) {
        found = true;
        // Ensure the old unconditional rule is gone
        // The old rule was: scion-terminal-pane[data-focused] { outline: ... }
        // without the parent selector prefix
        const lines = text.split('\n');
        const unconditionalFocusRule = lines.some(
          (line) =>
            line.trim().startsWith('scion-terminal-pane[data-focused]') &&
            !line.includes('data-effective-layout')
        );
        expect(unconditionalFocusRule).toBe(false);
        break;
      }
    }
    expect(found).toBe(true);
  });

  it('data-effective-layout is single when in single-pane mode so CSS focus rule does not match', () => {
    const host = getPaneHost(root);
    // In single-pane mode (default), the CSS selector won't match because
    // data-effective-layout='single' is not in the selector list
    expect(host.dataset.effectiveLayout).toBe('single');
  });

  it('switching from multi-pane to single-pane changes data-effective-layout so focus outline CSS stops applying', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-columns');

    root.layoutManager.setLayout('single');
    await flush();
    expect(host.dataset.effectiveLayout).toBe('single');
    // CSS rule no longer matches because data-effective-layout='single' is not targeted
  });

  it('switching from single-pane to multi-pane restores data-effective-layout so focus outline CSS applies', async () => {
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('single');

    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(host.dataset.effectiveLayout).toBe('two-columns');
    // CSS rule now matches again for focused panes
  });
});

// ────────────────────────────────────────────────────────────────────────────
// URL layout sync (#1715)
// ────────────────────────────────────────────────────────────────────────────

describe('URL layout sync (#1715)', () => {
  let root: TerminalWorkspaceRoot;
  let replaceStateSpy: MockInstance<History['replaceState']>;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              id: '11111111-1111-4111-8111-111111111111',
              name: 'test',
              phase: 'running',
            }),
            { status: 200 }
          )
        )
      )
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
        constructor(public url: string) {
          super();
        }
      }
    );
    replaceStateSpy = vi.spyOn(window.history, 'replaceState');
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('getActiveSlotAgentIds returns null for empty layout', () => {
    const ids = root.getActiveSlotAgentIds();
    expect(ids).toEqual([null]);
  });

  it('getActiveSlotAgentIds returns agent IDs for occupied slots', () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test',
    });
    root.create(registry, '11111111-1111-4111-8111-111111111111');
    const ids = root.getActiveSlotAgentIds();
    // In single mode, should show the agent ID
    expect(ids[0]).toBe('11111111-1111-4111-8111-111111111111');
  });

  it('findSessionKeyByAgentId returns key for existing session', () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test',
    });
    const session = root.create(registry, '11111111-1111-4111-8111-111111111111');
    const key = root.findSessionKeyByAgentId('11111111-1111-4111-8111-111111111111');
    expect(key).toBe(session.state.key);
  });

  it('findSessionKeyByAgentId returns null for unknown agent', () => {
    const key = root.findSessionKeyByAgentId('99999999-9999-4999-8999-999999999999');
    expect(key).toBeNull();
  });

  it('syncUrlFromLayout updates URL via replaceState for multi-pane presets', () => {
    replaceStateSpy.mockClear();
    root.layoutManager.setLayout('two-columns');
    // Subscriber triggers syncUrlFromLayout synchronously
    const urls = replaceStateSpy.mock.calls.map((c: unknown[]) => String(c[2] ?? ''));
    const layoutCall = urls.find((u: string) => u.includes('lv=1'));
    expect(layoutCall).toBeTruthy();
    expect(layoutCall).toContain('lp=two-columns');
  });

  it('syncUrlFromLayout clears layout params in single mode', () => {
    // Switch to multi-pane first so params are set
    root.layoutManager.setLayout('two-columns');
    replaceStateSpy.mockClear();
    root.layoutManager.setLayout('single');
    // In single mode, URL should not contain layout params
    const urls = replaceStateSpy.mock.calls.map((c: unknown[]) => String(c[2] ?? ''));
    const lastUrl = urls[urls.length - 1];
    if (lastUrl) {
      expect(lastUrl).not.toContain('lv=1');
      expect(lastUrl).not.toContain('lp=');
    }
  });

  it('syncUrlFromLayout is suppressed when flag is set', () => {
    replaceStateSpy.mockClear();
    root.setSuppressUrlSync(true);
    root.layoutManager.setLayout('four');
    // Subscriber was called but syncUrlFromLayout should have been a no-op
    const urls = replaceStateSpy.mock.calls.map((c: unknown[]) => String(c[2] ?? ''));
    const layoutCall = urls.find((u: string) => u.includes('lp=four'));
    expect(layoutCall).toBeUndefined();
    root.setSuppressUrlSync(false);
  });

  it('layout changes trigger URL sync via subscriber', async () => {
    replaceStateSpy.mockClear();
    root.layoutManager.setLayout('two-columns');
    await flush();
    // Should have been called at least once with two-columns
    const urls = replaceStateSpy.mock.calls.map((c: unknown[]) => String(c[2] ?? ''));
    expect(urls.length).toBeGreaterThan(0);
    const layoutCall = urls.find((u: string) => u.includes('lp=two-columns'));
    expect(layoutCall).toBeTruthy();
  });
});

// ────────────────────────────────────────────────────────────────────────────
// SSE bridge: re-arm auto-reconnect once the agent is confirmed running again
// ────────────────────────────────────────────────────────────────────────────

describe('the SSE bridge re-arms auto-reconnect regardless of which agent-state reason disarmed the session', () => {
  class FakeSocket {
    static instances: FakeSocket[] = [];
    readyState = 0;
    onopen: (() => void) | null = null;
    onclose: ((event: { code: number; reason?: string }) => void) | null = null;
    onmessage: ((event: { data: unknown }) => void) | null = null;
    send = vi.fn();
    close = vi.fn();
    constructor(readonly url: string) {
      FakeSocket.instances.push(this);
    }
    open(): void {
      this.readyState = 1;
      this.onopen?.();
    }
    data(payload = ''): void {
      this.onmessage?.({ data: JSON.stringify({ type: 'data', data: btoa(payload) }) });
    }
  }
  class FakeEventSource extends EventTarget {
    static instances: FakeEventSource[] = [];
    onopen: (() => void) | null = null;
    close = vi.fn();
    constructor(readonly url: string) {
      super();
      FakeEventSource.instances.push(this);
    }
  }

  // Distinct from the '11111111-...' ID other describe blocks in this file
  // use: those blocks leave sessions un-disposed (root.element.remove() does
  // not call registry.dispose()), and a shared ID would let a stale session's
  // fetch race this test's phase-keyed response.
  const agentId = '55555555-5555-4555-8555-555555555555';
  let root: TerminalWorkspaceRoot;
  let fetcher: ReturnType<typeof vi.fn>;
  let agentPhase: string;

  function agentResponse(phase: string, activity?: string): Response {
    return new Response(JSON.stringify({ id: agentId, name: 'test', phase, activity }), {
      status: 200,
    });
  }

  beforeEach(() => {
    FakeSocket.instances = [];
    FakeEventSource.instances = [];
    agentPhase = 'running';
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    // Keyed by phase state, not a mockResolvedValueOnce queue: a stale
    // session from another test polling the same global fetch mock cannot
    // steal this test's queued response out of order.
    fetcher = vi.fn(() => Promise.resolve(agentResponse(agentPhase)));
    vi.stubGlobal('fetch', fetcher);
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('EventSource', FakeEventSource);
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  /**
   * Other describe blocks in this file leave sessions un-disposed
   * (root.element.remove() alone does not call registry.dispose()), and a
   * stale one can still poll the shared stubbed WebSocket/fetch globals
   * across a microtask boundary. Filter by this test's own unique agentId
   * rather than trusting array position.
   */
  function mySocket(): FakeSocket {
    const found = FakeSocket.instances.filter((s) => s.url.includes(agentId));
    return found[found.length - 1];
  }

  it('an agent-phase reason (the WS drop reached the client before SSE reported it) still re-arms once SSE confirms running', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'r2',
    });
    const session = root.create(registry, agentId);
    await vi.waitFor(() => expect(mySocket()).toBeDefined());
    const socket = mySocket();
    socket.open();
    socket.data();
    session.setFrontmost(true);

    // The common ordering: the WebSocket drop reaches the client, and the
    // frontmost auto-attempt's own agent fetch already sees the agent
    // stopped, before SSE's own stopped event arrives.
    agentPhase = 'stopped';
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });
    await vi.waitFor(() => expect(session.reconnecting).toBe(false));
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-phase');

    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    const source = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source.onopen?.();

    // The workspace root's own async layout/visibility refresh (queued via
    // queueMicrotask) can toggle this pane's setVisible in between; the
    // waitFor above let those settle. Re-assert frontmost so this test
    // exercises the SSE bridge itself, not layout-refresh timing.
    session.setFrontmost(true);

    // SSE now reports the agent stopped (arriving after the WS drop above):
    // the bridge's markUnavailable('agent-stopped') branch is guarded by
    // connection !== 'unavailable', so it must not fire, and the more
    // specific agent-phase reason must survive.
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'stopped' } }),
      })
    );
    expect(session.state.disconnectReason).toBe('agent-phase');
    expect(session.reconnecting).toBe(false);

    // SSE then reports the agent running again: the bridge must re-arm even
    // though the reason is agent-phase, not agent-stopped.
    agentPhase = 'running';
    session.setFrontmost(true);
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'running' } }),
      })
    );
    expect(session.reconnecting).toBe(true);
  });

  it('SSE reporting the agent running but offline does not re-arm (does not burn the single attempt)', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'r2-offline',
    });
    const session = root.create(registry, agentId);
    await vi.waitFor(() => expect(mySocket()).toBeDefined());
    const socket = mySocket();
    socket.open();
    socket.data();
    session.setFrontmost(true);

    agentPhase = 'stopped';
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });
    await vi.waitFor(() => expect(session.reconnecting).toBe(false));
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-phase');

    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    const source = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source.onopen?.();
    session.setFrontmost(true);

    // SSE reports the agent running again, but still offline (e.g. the
    // runtime has not reported activity yet): the bridge's activity gate
    // must withhold noteAgentAvailable() so an offline-but-running agent
    // does not burn the session's single foregrounding attempt.
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({
          subject: `agent.${agentId}.status`,
          data: { phase: 'running', activity: 'offline' },
        }),
      })
    );
    expect(session.reconnecting).toBe(false);
    expect(session.state.connection).toBe('unavailable');
  });

  it('a crashed agent (SSE phase error, not stopped) still re-arms once it reports running again (ptone/scion#2096)', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'r2-error-phase',
    });
    const session = root.create(registry, agentId);
    await vi.waitFor(() => expect(mySocket()).toBeDefined());
    const socket = mySocket();
    socket.open();
    socket.data();
    expect(session.state.connection).toBe('connected');

    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    const source = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source.onopen?.();

    // The container crashes: SSE reports phase 'error', not 'stopped'. The
    // bridge's markUnavailable branch must treat the two the same.
    agentPhase = 'error';
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'error' } }),
      })
    );
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-stopped');

    session.setFrontmost(true);
    expect(session.reconnecting).toBe(false); // still crashed: foregrounding alone never dials

    // The agent restarts and SSE reports it running again.
    agentPhase = 'running';
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'running' } }),
      })
    );
    expect(session.reconnecting).toBe(true);
  });

  it('a 4410 agent_stopped close racing stale SSE "running" snapshots does not re-arm until SSE independently confirms the agent down (ptone/scion#2096)', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'r2-stale-running-race',
    });
    const session = root.create(registry, agentId);
    await vi.waitFor(() => expect(mySocket()).toBeDefined());
    const socket = mySocket();
    socket.open();
    socket.data();
    session.setFrontmost(true);

    // The broker's own open-time check already knows the container is down
    // and closes with 4410 agent_stopped, but SSE's polled view has not
    // caught up: agentPhase (what the next fetch/status event reports)
    // stays 'running' for now.
    socket.readyState = 3;
    socket.onclose?.({ code: 4410, reason: 'agent_stopped' });
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-stopped');

    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    const source = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source.onopen?.();
    session.setFrontmost(true);

    // N stale snapshots, still reporting the pre-crash phase: none may dial.
    for (let i = 0; i < 3; i++) {
      source.dispatchEvent(
        new MessageEvent('update', {
          data: JSON.stringify({
            subject: `agent.${agentId}.status`,
            data: { phase: 'running', activity: i % 2 === 0 ? 'working' : 'idle' },
          }),
        })
      );
    }
    expect(session.reconnecting).toBe(false);
    expect(FakeSocket.instances.filter((s) => s.url.includes(agentId))).toHaveLength(1);

    // SSE catches up and reports the agent actually down. This alone must
    // not dial either — it is the observation, not the re-arm trigger.
    agentPhase = 'error';
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'error' } }),
      })
    );
    expect(session.reconnecting).toBe(false);
    expect(session.state.connection).toBe('unavailable');

    // Only now does a genuine transition back to running re-arm, with
    // exactly one attempt.
    agentPhase = 'running';
    source.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'running' } }),
      })
    );
    expect(session.reconnecting).toBe(true);
  });
});

describe('idle entries', () => {
  class FakeSocket {
    static instances: FakeSocket[] = [];
    readyState = 0;
    onopen: (() => void) | null = null;
    onclose: ((event: { code: number; reason?: string }) => void) | null = null;
    onmessage: ((event: { data: unknown }) => void) | null = null;
    send = vi.fn();
    close = vi.fn();
    constructor(readonly url: string) {
      FakeSocket.instances.push(this);
    }
    open(): void {
      this.readyState = 1;
      this.onopen?.();
    }
    data(payload = ''): void {
      this.onmessage?.({ data: JSON.stringify({ type: 'data', data: btoa(payload) }) });
    }
  }
  class FakeEventSource extends EventTarget {
    static instances: FakeEventSource[] = [];
    onopen: (() => void) | null = null;
    close = vi.fn();
    constructor(readonly url: string) {
      super();
      FakeEventSource.instances.push(this);
    }
  }

  const agentId = '66666666-6666-4666-8666-666666666666';
  let root: TerminalWorkspaceRoot;
  let fetcher: ReturnType<typeof vi.fn>;
  let agentPhase: string;

  function agentResponse(phase: string, activity?: string): Response {
    return new Response(JSON.stringify({ id: agentId, name: 'test', phase, activity }), {
      status: 200,
    });
  }

  function mySource(): FakeEventSource {
    return FakeEventSource.instances[FakeEventSource.instances.length - 1];
  }

  function railItem(): HTMLElement {
    return root.element.querySelector('.terminal-rail-item') as HTMLElement;
  }

  beforeEach(() => {
    FakeSocket.instances = [];
    FakeEventSource.instances = [];
    agentPhase = 'running';
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    fetcher = vi.fn(() => Promise.resolve(agentResponse(agentPhase)));
    vi.stubGlobal('fetch', fetcher);
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('EventSource', FakeEventSource);
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // create(..., { deferConnect: true }) itself only skips its own
  // layoutManager.open() call; syncSessions()'s separate "no active
  // session" auto-select fallback would otherwise still pick up a lone new
  // entry. The real restore path always wraps entry creation in
  // withAutoSelectSuspended for exactly this reason, so these tests do the
  // same to observe a deliberately-idle, unselected entry.
  it('deferConnect leaves layoutManager state unchanged and creates no socket', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i1',
    });
    const before = root.layoutManager.getState();
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    expect(root.layoutManager.getState()).toEqual(before);
    expect(session.state.connection).toBe('idle');
    expect(fetcher).not.toHaveBeenCalled();
  });

  it('idle copy: rail says "Not connected", no Reconnect button in rail or pane, no error styling', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i2',
    });
    root.withAutoSelectSuspended(() => root.create(registry, agentId, { deferConnect: true }));
    await flush();

    const item = railItem();
    expect(item.dataset.connection).toBe('idle');
    const stateLabel = item.querySelector('.terminal-state-label');
    expect(stateLabel?.textContent).toContain('Not connected');
    const reconnectBtn = item.querySelector<HTMLButtonElement>(
      '[aria-label^="Reconnect"].terminal-icon-action'
    );
    expect(reconnectBtn?.disabled).toBe(true);

    const pane = root.element.querySelector('scion-terminal-pane')!;
    const paneReconnect = pane.shadowRoot?.querySelector('.reconnect-btn');
    expect(paneReconnect).toBeNull();
    const idleOverlay = pane.shadowRoot?.querySelector('.idle-overlay');
    expect(idleOverlay).not.toBeNull();
    expect(idleOverlay?.textContent).toContain('Select this terminal to connect');
    const errorBanner = pane.shadowRoot?.querySelector('.error-banner');
    expect(errorBanner).toBeNull();
  });

  it('selecting an idle entry connects it', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i3',
    });
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    expect(session.state.connection).toBe('idle');
    expect(fetcher).not.toHaveBeenCalled();

    // Selecting moves the pane into the visible slot, which drives
    // setFrontmost(true) and, from 'idle', connect(). The session-level
    // mechanics of a connect attempt are already covered by
    // terminal-sessions.test.ts; here it is enough to prove selection is
    // what triggers it, by observing the agent-fetch and pty-preflight
    // requests.
    root.select(session);
    await flush();
    expect(session.state.connection).not.toBe('idle');
    await vi.waitFor(() =>
      expect(fetcher.mock.calls.some(([url]) => String(url).includes('/pty'))).toBe(true)
    );
  });

  // select() is also the path the saved-terminal-list restore feature uses
  // to focus the persisted frontmost entry once it has been created idle
  // (see terminal-persistence.ts), not just the user clicking a rail item —
  // so a restored frontmost entering view must engage frame mode exactly
  // like any other route into show(true), with no separate wiring needed.
  it('selecting an idle entry also enters frame mode, the same as show(true)', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i3-frame',
    });
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    const start = _appFrameRefCountForTests();

    root.select(session);
    expect(_appFrameRefCountForTests()).toBe(start + 1);

    root.show(false);
    expect(_appFrameRefCountForTests()).toBe(start);
  });

  it('withAutoSelectSuspended: creating into an empty workspace selects nothing inside, but auto-selects outside', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i4',
    });
    root.withAutoSelectSuspended(() => {
      root.create(registry, agentId, { deferConnect: true });
    });
    await flush();
    expect(root.layoutManager.getState().single[0]).toBeNull();

    const agentId2 = '77777777-7777-4777-8777-777777777777';
    fetcher = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ id: agentId2, name: 'test2', phase: agentPhase }), {
          status: 200,
        })
      )
    );
    vi.stubGlobal('fetch', fetcher);
    // Closing the idle entry drives the registry back to empty, then a
    // normal (non-suspended) create auto-selects as usual.
    registry.list()[0].close();
    await flush();
    const session2 = root.create(registry, agentId2);
    await flush();
    expect(root.layoutManager.getState().single[0]).toBe(session2.state.key);
  });

  it('idle and metadata: a stopped agent stays idle; selecting it while still stopped shows unavailable', async () => {
    agentPhase = 'stopped';
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i5',
    });
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    mySource().onopen?.();
    mySource().dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.status`, data: { phase: 'stopped' } }),
      })
    );
    // Stays idle: marking a never-connected entry unavailable would strand
    // it (noteAgentAvailable requires everConnected).
    expect(session.state.connection).toBe('idle');

    // Selecting while still stopped behaves like today's open of a stopped
    // agent: the attempt fails unavailable. (The independent metadata poll
    // and the session's own attach() can race on which one first classifies
    // the failure, so either agent-state reason is acceptable here; both
    // are in AGENT_UNAVAILABLE_REASONS and render the same "Unavailable"
    // copy.)
    root.select(session);
    await flush();
    await vi.waitFor(() => expect(session.state.connection).toBe('unavailable'));
    expect(['agent-phase', 'agent-stopped']).toContain(session.state.disconnectReason);
  });

  it('idle and metadata: a stopped agent stays idle until selected; selecting it once it is running connects', async () => {
    agentPhase = 'stopped';
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i5b',
    });
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    expect(session.state.connection).toBe('idle');

    // The agent restarts before the user ever selects the idle entry. Idle
    // entries ignore the SSE bridge's running-rearm branch regardless (it
    // only acts on 'unavailable' sessions), but selecting always drives a
    // fresh connect() / attach() that sees the agent's current state.
    agentPhase = 'running';
    root.select(session);
    await flush();
    expect(session.state.connection).not.toBe('idle');
    await vi.waitFor(() =>
      expect(fetcher.mock.calls.some(([url]) => String(url).includes('/pty'))).toBe(true)
    );
  });

  it('idle and metadata: a deleted agent shows "Agent was deleted." and selecting it starts no attempt', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'i6',
    });
    const session = root.withAutoSelectSuspended(() =>
      root.create(registry, agentId, { deferConnect: true })
    );
    await flush();
    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(1));
    mySource().onopen?.();
    mySource().dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${agentId}.deleted`, data: {} }),
      })
    );
    await vi.waitFor(() => expect(session.state.connection).toBe('unavailable'));
    expect(session.state.disconnectReason).toBe('agent-deleted');
    expect(session.state.error).toBe('Agent was deleted.');

    // Selecting starts no attempt: setFrontmost(true) on a non-idle,
    // never-connected session falls to maybeAutoAttempt(), which requires
    // everConnected and so no-ops. (The metadata layer's own background poll
    // calls fetch independently of selection, so asserting on the session's
    // own connection/disconnectReason here is the precise check; the global
    // fetch mock is shared with that poll.)
    root.select(session);
    await flush();
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-deleted');
  });
});

describe('show() frame-mode ref counting', () => {
  let root: TerminalWorkspaceRoot;
  let startCount: number;

  beforeEach(() => {
    root = new WorkspaceRoot();
    document.body.append(root.element);
    startCount = _appFrameRefCountForTests();
  });

  afterEach(() => {
    // show(false) only releases the ref if this root itself currently holds
    // one (its own _frameEntered flag), so one call is enough regardless of
    // which test ran — no loop needed. The count comparison (rather than a
    // bare call) fails loudly if a test left the ref unbalanced instead of
    // silently leaking state into other test files.
    root.show(false);
    root.dispose();
    root.element.remove();
    expect(_appFrameRefCountForTests()).toBe(startCount);
  });

  it('enters frame mode on the first show(true) and exits on show(false)', () => {
    const start = _appFrameRefCountForTests();
    root.show(true);
    expect(_appFrameRefCountForTests()).toBe(start + 1);
    root.show(false);
    expect(_appFrameRefCountForTests()).toBe(start);
  });

  it('a repeated show(true) (successive /terminals navigations) does not inflate the count', () => {
    const start = _appFrameRefCountForTests();
    root.show(true);
    root.show(true);
    root.show(false);
    expect(_appFrameRefCountForTests()).toBe(start);
  });

  it('a redundant show(false) before ever showing is a no-op', () => {
    const start = _appFrameRefCountForTests();
    root.show(false);
    expect(_appFrameRefCountForTests()).toBe(start);
  });
});
// ────────────────────────────────────────────────────────────────────────────
// "Jump to agent" palette
// ────────────────────────────────────────────────────────────────────────────

const AGENT_A = '11111111-1111-4111-8111-111111111111';
const AGENT_B = '22222222-2222-4222-8222-222222222222';
const AGENT_C = '33333333-3333-4333-8333-333333333333';
const AGENT_NEW = '44444444-4444-4444-8444-444444444444';

/** The real agent-list shape the palette's own fetch reads; everything else (session/metadata opens) gets a single-agent detail response regardless of URL, matching the other describe blocks' fetch stub above. */
function stubFetchForPalette(
  listedAgents: Array<{ id: string; name?: string; phase?: string; attach?: boolean }>
): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      if (typeof url === 'string' && url.startsWith('/api/v1/agents?')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              agents: listedAgents.map((a) => ({
                id: a.id,
                name: a.name ?? a.id,
                phase: a.phase ?? 'running',
                _capabilities: { actions: a.attach === false ? [] : ['attach'] },
              })),
            }),
            { status: 200 }
          )
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: AGENT_A, name: 'test', phase: 'running' }), {
          status: 200,
        })
      );
    })
  );
}

function stubWebSocketAndEventSource(): void {
  vi.stubGlobal(
    'WebSocket',
    class {
      onopen = null;
      onclose = null;
      send = vi.fn();
      close = vi.fn();
      readyState = 0;
    }
  );
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      onopen = null;
      close = vi.fn();
      constructor(public url: string) {
        super();
      }
    }
  );
}

/** Waits for the lazily created palette element and its first render. */
/** Fires `type` from the palette's own dialog, composed, as Shoelace does. */
function fireFromDialog(palette: ScionQuickPalette, type: 'sl-hide' | 'sl-after-hide'): void {
  palette
    .shadowRoot!.querySelector('sl-dialog')!
    .dispatchEvent(new Event(type, { bubbles: true, composed: true }));
}

async function waitForPalette(root: TerminalWorkspaceRoot): Promise<ScionQuickPalette> {
  const palette = await vi.waitFor(
    () => {
      const el = root.element.querySelector('scion-quick-palette');
      if (!el) throw new Error('palette not mounted yet');
      return el;
    },
    { timeout: 5000 }
  );
  await palette.updateComplete;
  return palette;
}

/** The "Jump to agent" button in the rail's pinned footer. */
function jumpButton(root: TerminalWorkspaceRoot): HTMLButtonElement {
  const btn = root.element.querySelector<HTMLButtonElement>(
    '.terminal-rail > .terminal-rail-footer > .terminal-jump-btn'
  );
  if (!btn) throw new Error('Jump to agent footer button not found');
  return btn;
}

function requestPaletteOpen(root: TerminalWorkspaceRoot): void {
  jumpButton(root).click();
}

/** Opens the palette and waits until it is open with its Agents group loaded. */
async function openLoadedPalette(root: TerminalWorkspaceRoot): Promise<ScionQuickPalette> {
  requestPaletteOpen(root);
  const palette = await waitForPalette(root);
  await vi.waitFor(() => {
    expect(palette.open).toBe(true);
    expect(palette.groups.agents?.status).toBe('ready');
  });
  return palette;
}

function selectAgent(palette: ScionQuickPalette, agentId: string): void {
  palette.dispatchEvent(
    new CustomEvent('palette-select', {
      detail: { target: { kind: 'agent', agentId, displayName: agentId } },
    })
  );
}

/** Opens the palette, waits for the Agents group, and picks `agentId`. */
async function pickFromPalette(
  root: TerminalWorkspaceRoot,
  agentId: string
): Promise<ScionQuickPalette> {
  const palette = await openLoadedPalette(root);
  selectAgent(palette, agentId);
  return palette;
}

function paneFor(root: TerminalWorkspaceRoot, agentId: string): ScionTerminalPane {
  const pane = Array.from(root.element.querySelectorAll('scion-terminal-pane')).find(
    (el) => el.agentId === agentId
  );
  if (!pane) throw new Error(`No pane found for agent ${agentId}`);
  return pane;
}

/** Records the paths of the workspace's own `nav-click` events (the rail's navigation). */
function recordNavigation(root: TerminalWorkspaceRoot): string[] {
  const paths: string[] = [];
  root.element.addEventListener('nav-click', (e) =>
    paths.push((e as CustomEvent<{ path: string }>).detail.path)
  );
  return paths;
}

/** Waits past Shoelace's after-hide focus restore, which runs in a timeout. */
function nextTask(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function mockNarrowViewport(): void {
  const narrowQuery = { matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() };
  vi.spyOn(window, 'matchMedia').mockReturnValue(narrowQuery as unknown as MediaQueryList);
}

/**
 * Finds the real `<scion-terminal-pane>` for `agentId` and gives it real DOM
 * focus — not just `dataset.focused = ''` directly, which a subsequent
 * `setVisible(true)` (from any still-pending layout refresh microtask) would
 * silently overwrite from real `document.activeElement` containment (see
 * `ScionTerminalPane.setVisible`). Call this only after any pending layout
 * refresh has settled (e.g. after a `place()`/`setLayout()` microtask) — a
 * still-hidden/inert pane can't take real focus at all. Not order-sensitive
 * relative to opening the palette or dispatching a selection:
 * `lastFocusedPaneSessionKey` is tracked continuously via a real `focusin`
 * listener (see that field's own doc comment in terminal-workspace-root.ts),
 * not read reactively at either point, so this may be called before or
 * after either.
 */
function markPaneFocused(root: TerminalWorkspaceRoot, agentId: string): void {
  const pane = Array.from(root.element.querySelectorAll('scion-terminal-pane')).find(
    (el) => (el as unknown as { agentId: string }).agentId === agentId
  ) as (HTMLElement & { tabIndex: number }) | undefined;
  if (!pane) throw new Error(`No pane found for agent ${agentId}`);
  pane.tabIndex = 0;
  pane.focus();
}

describe('"Jump to agent" palette: multi-pane placement via create()', () => {
  let root: TerminalWorkspaceRoot;
  let reg: TerminalSessionRegistry;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    stubFetchForPalette([{ id: AGENT_C }, { id: AGENT_NEW }]);
    stubWebSocketAndEventSource();
    root = new WorkspaceRoot();
    document.body.append(root.element);
    // One registry per test, reused for every create() call — matching real
    // usage, where exactly one TerminalSessionRegistry backs the whole
    // workspace for its lifetime (see ensureTerminalCoordinator in
    // main.ts). A fresh registry per create() call would desync: each
    // registry's own subscribe() callback reports only *its* sessions, so
    // binding to a second, still-empty registry would read as "every
    // previously-created session just disappeared" and tear down their
    // panes.
    reg = new TerminalSessionRegistry({ hubUrl: window.location.origin, accountId: 'r1' });
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  function keyFor(agentId: string): string {
    const key = root.findSessionKeyByAgentId(agentId);
    if (!key) throw new Error(`No session for agent ${agentId}`);
    return key;
  }

  /** Two-columns layout with `left` and `right` placed, refreshed, and `focused` (if any) holding real focus. */
  async function fullTwoColumns(left: string, right: string, focused?: string): Promise<void> {
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, left);
    root.create(reg, right);
    root.layoutManager.place(keyFor(left), 'two-columns', 0);
    root.layoutManager.place(keyFor(right), 'two-columns', 1);
    await flush();
    if (focused) markPaneFocused(root, focused);
  }

  it('without a pending placement, create() still overflows to single at capacity (unchanged default)', () => {
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_A), keyFor(AGENT_B)]);

    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().active).toBe('single');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_A), keyFor(AGENT_B)]);
  });

  it('a picked new agent fills the next empty slot instead of overflowing', async () => {
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();

    await pickFromPalette(root, AGENT_C);
    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().active).toBe('two-columns');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_A), keyFor(AGENT_C)]);
  });

  it('a picked new agent replaces the focused slot instead of overflowing when full', async () => {
    await fullTwoColumns(AGENT_A, AGENT_B, AGENT_B);

    await pickFromPalette(root, AGENT_C);
    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().active).toBe('two-columns');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_A), keyFor(AGENT_C)]);
  });

  it('the pending placement is consumed exactly once: the next create() uses the normal overflow default again', async () => {
    await fullTwoColumns(AGENT_A, AGENT_B, AGENT_A);

    await pickFromPalette(root, AGENT_C);
    root.create(reg, AGENT_C);
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_C), keyFor(AGENT_B)]);

    // A normal rail/URL open at capacity must still overflow to single, not
    // keep using the palette's replace-focused behavior.
    root.create(reg, AGENT_NEW);
    expect(root.layoutManager.getState().active).toBe('single');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_C), keyFor(AGENT_B)]);
  });

  it('cancelPalettePlacement clears a pending hint before it is ever consumed', async () => {
    await fullTwoColumns(AGENT_A, AGENT_B, AGENT_A);

    await pickFromPalette(root, AGENT_C);
    root.cancelPalettePlacement(AGENT_C);
    root.create(reg, AGENT_C);

    // Falls back to the normal overflow default since the hint was cancelled.
    expect(root.layoutManager.getState().active).toBe('single');
  });

  it('cancelPalettePlacement for a different agent leaves the hint in place', async () => {
    await fullTwoColumns(AGENT_A, AGENT_B, AGENT_A);

    await pickFromPalette(root, AGENT_C);
    root.cancelPalettePlacement(AGENT_NEW);
    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().active).toBe('two-columns');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_C), keyFor(AGENT_B)]);
  });

  it("a create() for another agent before the picked agent's own does not consume the hint", async () => {
    await fullTwoColumns(AGENT_A, AGENT_B, AGENT_A);

    await pickFromPalette(root, AGENT_C);
    // An unrelated open (e.g. a restore) lands first: normal overflow.
    root.create(reg, AGENT_NEW);
    expect(root.layoutManager.getState().active).toBe('single');
    root.layoutManager.setLayout('two-columns');

    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().active).toBe('two-columns');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_C), keyFor(AGENT_B)]);
  });

  it('forgets the focused pane once its session closes', async () => {
    // B left, A right, A focused. Closing A empties the right slot; reopening
    // A refills it without focusing it. A later pick must then fall back to
    // the left slot rather than replacing the reopened A through a stale key.
    await fullTwoColumns(AGENT_B, AGENT_A, AGENT_A);
    reg
      .list()
      .find((s) => s.state.agentId === AGENT_A)!
      .close();
    await flush();
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_B), null]);
    root.create(reg, AGENT_A);
    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_B), keyFor(AGENT_A)]);
    (document.activeElement as HTMLElement | null)?.blur();

    await pickFromPalette(root, AGENT_C);
    root.create(reg, AGENT_C);

    expect(root.layoutManager.getState().twoColumns).toEqual([keyFor(AGENT_C), keyFor(AGENT_A)]);
  });
});

describe('"Jump to agent" footer in the Open terminals column', () => {
  let root: TerminalWorkspaceRoot;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    stubWebSocketAndEventSource();
    stubFetchForPalette([{ id: AGENT_A, name: 'Alice-bot' }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('pins the footer below the list, outside the scrolling list itself', () => {
    const rail = root.element.querySelector('.terminal-rail')!;
    const list = rail.querySelector('.terminal-rail-list')!;
    const footer = rail.querySelector('.terminal-rail-footer')!;
    expect(footer).not.toBeNull();
    expect(rail.lastElementChild).toBe(footer);
    expect(list.contains(footer)).toBe(false);
    expect(footer.previousElementSibling).toBe(list);
  });

  it('renders a labelled "Jump to agent" button with the compass icon', () => {
    const btn = jumpButton(root);
    expect(btn.type).toBe('button');
    expect(btn.querySelector('.terminal-jump-label')?.textContent).toBe('Jump to agent');
    expect(btn.querySelector('sl-icon')?.getAttribute('name')).toBe('compass');
    expect(btn.getAttribute('aria-haspopup')).toBe('dialog');
    expect(btn.title).toMatch(/^Jump to agent \((⌘K|Ctrl\+K)\)$/);
    expect(btn.querySelector('.terminal-jump-shortcut')?.textContent).toMatch(/^(⌘K|Ctrl\+K)$/);
  });

  /**
   * Recreates the root with a matchMedia stub whose touch-primary query
   * reports `touch`, and a platform of `platform`. Returns a function that
   * flips the touch query and fires its `change` listeners.
   */
  function recreateWithModality(touch: boolean, platform: string): (next: boolean) => void {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue(platform);
    const listeners = new Set<() => void>();
    const touchQuery = {
      matches: touch,
      addEventListener: (_type: string, cb: () => void): void => void listeners.add(cb),
      removeEventListener: (_type: string, cb: () => void): void => void listeners.delete(cb),
    };
    const otherQuery = { matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() };
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        (query === TOUCH_PRIMARY_QUERY ? touchQuery : otherQuery) as unknown as MediaQueryList
    );
    root.dispose();
    root.element.remove();
    root = new WorkspaceRoot();
    document.body.append(root.element);
    return (next: boolean): void => {
      touchQuery.matches = next;
      for (const cb of listeners) cb();
    };
  }

  it('sets title and aria-keyshortcuts (Control+K) on a non-Mac pointer device', () => {
    recreateWithModality(false, 'Linux x86_64');
    const btn = jumpButton(root);
    expect(btn.getAttribute('aria-keyshortcuts')).toBe('Control+K');
    expect(btn.title).toBe('Jump to agent (Ctrl+K)');
  });

  it('sets title and aria-keyshortcuts (Meta+K) on a Mac pointer device', () => {
    recreateWithModality(false, 'MacIntel');
    const btn = jumpButton(root);
    expect(btn.getAttribute('aria-keyshortcuts')).toBe('Meta+K');
    expect(btn.title).toBe('Jump to agent (⌘K)');
  });

  it('omits title and aria-keyshortcuts on a touch-primary device', () => {
    recreateWithModality(true, 'Linux x86_64');
    const btn = jumpButton(root);
    expect(btn.hasAttribute('aria-keyshortcuts')).toBe(false);
    expect(btn.hasAttribute('title')).toBe(false);
  });

  it('follows touch-primary changes after construction', () => {
    const setTouch = recreateWithModality(true, 'Linux x86_64');
    const btn = jumpButton(root);
    setTouch(false);
    expect(btn.getAttribute('aria-keyshortcuts')).toBe('Control+K');
    expect(btn.title).toBe('Jump to agent (Ctrl+K)');
    setTouch(true);
    expect(btn.hasAttribute('aria-keyshortcuts')).toBe(false);
    expect(btn.hasAttribute('title')).toBe(false);
  });

  it('clicking it focuses the button, then opens the agents palette', async () => {
    const btn = jumpButton(root);
    const focusSpy = vi.spyOn(btn, 'focus');
    btn.click();
    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(palette.open).toBe(true));
    expect(palette.label).toBe('Jump to agent');
  });

  it('the header renders no palette button on /terminals', async () => {
    const header = root.element.querySelector('scion-header')!;
    header.user = { id: 'u1', email: 'u@example.com', displayName: 'U' } as never;
    await header.updateComplete;
    expect(header.shadowRoot?.querySelector('.palette-button')).toBeNull();
  });
});

describe('"Jump to agent" palette: open, select, and events', () => {
  let root: TerminalWorkspaceRoot;
  let reg: TerminalSessionRegistry;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    stubWebSocketAndEventSource();
    reg = new TerminalSessionRegistry({ hubUrl: window.location.origin, accountId: 'r2' });
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('creates no palette element until the first open', async () => {
    stubFetchForPalette([{ id: AGENT_A, name: 'Alice-bot' }]);
    // Load the component module up front, so an eager mount would finish
    // within the wait below.
    await import('../components/shared/palette/quick-palette.js');
    root = new WorkspaceRoot();
    document.body.append(root.element);
    await nextTask();

    expect(root.element.querySelector('scion-quick-palette')).toBeNull();
  });

  it('the footer button opens the palette and loads the Agents group', async () => {
    stubFetchForPalette([{ id: AGENT_A, name: 'Alice-bot' }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);

    const palette = await openLoadedPalette(root);

    expect(palette.label).toBe('Jump to agent');
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alice-bot']);
  });

  it('closing the palette aborts its in-flight Agents load', async () => {
    const signals: AbortSignal[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit) => {
        if (init?.signal) signals.push(init.signal);
        return new Promise<Response>(() => {});
      })
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(signals).toHaveLength(1));
    expect(signals[0].aborted).toBe(false);

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));

    expect(signals[0].aborted).toBe(true);
    expect(palette.open).toBe(false);
  });

  it('hiding the workspace closes the open palette, aborts its load, and refocuses nothing', async () => {
    const signals: AbortSignal[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit) => {
        if (init?.signal) signals.push(init.signal);
        return new Promise<Response>(() => {});
      })
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    const invoker = document.createElement('button');
    document.body.appendChild(invoker);
    invoker.focus();
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(palette.open).toBe(true));
    await vi.waitFor(() => expect(signals).toHaveLength(1));
    invoker.blur();

    root.show(false);
    fireFromDialog(palette, 'sl-after-hide');

    expect(palette.open).toBe(false);
    expect(signals[0].aborted).toBe(true);
    expect(document.activeElement).not.toBe(invoker);
    invoker.remove();
  });

  it("a superseded load's late result never replaces the newer load's", async () => {
    const pending: Array<(agents: Array<{ id: string; name: string }>) => void> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            pending.push((agents) =>
              resolve(
                new Response(
                  JSON.stringify({
                    agents: agents.map((a) => ({
                      ...a,
                      phase: 'running',
                      _capabilities: { actions: ['attach'] },
                    })),
                  }),
                  { status: 200 }
                )
              )
            );
          })
      )
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(pending).toHaveLength(1));

    palette.dispatchEvent(new CustomEvent('palette-retry', { detail: { group: 'agents' } }));
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    pending[1]([{ id: AGENT_B, name: 'Newer' }]);
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
    pending[0]([{ id: AGENT_A, name: 'Older' }]);
    await nextTask();

    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Newer']);
  });

  it('a dismiss without a selection returns focus to the element that opened the palette', async () => {
    stubFetchForPalette([{ id: AGENT_A }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    // The footer button focuses itself on click, so it is the invoker.
    const invoker = jumpButton(root);

    const palette = await openLoadedPalette(root);
    invoker.blur();
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');

    expect(document.activeElement).toBe(invoker);
  });

  it('selecting an already-open agent in a multi-pane layout places it directly (addOrReplaceFocused), with no new-agent event or navigation', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }, { id: AGENT_C, name: 'Already-open' }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true); // panes only become real-focusable once the workspace itself is shown
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    root.create(reg, AGENT_C);
    const keyA = root.findSessionKeyByAgentId(AGENT_A)!;
    const keyB = root.findSessionKeyByAgentId(AGENT_B)!;
    const keyC = root.findSessionKeyByAgentId(AGENT_C)!;
    // B at slot 0, A at slot 1 — deliberately not the slot-0 default, so a
    // bug that silently fell back to "always replace slot 0" instead of
    // genuinely reading the focused key would replace the wrong slot (B's,
    // not A's) and this test would catch it.
    root.layoutManager.setLayout('two-columns');
    root.layoutManager.place(keyB, 'two-columns', 0);
    root.layoutManager.place(keyA, 'two-columns', 1);
    // Let the queued refresh (from the place() calls above) actually run —
    // a still-hidden/inert pane (setVisible's own pre-refresh default)
    // can't take real focus, so markPaneFocused below would silently no-op.
    await flush();

    const newAgentListener = vi.fn();
    root.element.addEventListener(TERMINAL_PALETTE_NEW_AGENT_EVENT, newAgentListener);
    const navigation = recordNavigation(root);

    markPaneFocused(root, AGENT_A);
    const palette = await pickFromPalette(root, AGENT_C);

    // Replaced the focused slot (A, at index 1) rather than overflowing to
    // single or falling back to slot 0 (B).
    expect(root.layoutManager.getState().active).toBe('two-columns');
    expect(root.layoutManager.getState().twoColumns).toEqual([keyB, keyC]);
    expect(newAgentListener).not.toHaveBeenCalled();
    expect(navigation).toEqual([]);
    expect(palette.open).toBe(false);
  });

  it('still replaces the pane focused before opening, even though the footer button itself steals real focus before the palette opens', async () => {
    // The footer button's own click handler calls
    // btn.focus({preventScroll: true}) BEFORE opening the palette (see
    // handleJumpButtonClick) — a point-in-time focus read taken inside
    // openPalette (rather than tracked continuously as focus actually
    // moves) would already see the button, not the pane, by the time it
    // runs.
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }, { id: AGENT_NEW }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    await flush();
    markPaneFocused(root, AGENT_B);

    // The real footer button: its click handler focuses it, then opens.
    const btn = jumpButton(root);
    btn.click();
    expect(document.activeElement).toBe(btn);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => {
      expect(palette.open).toBe(true);
      expect(palette.groups.agents?.status).toBe('ready');
    });
    selectAgent(palette, AGENT_NEW);
    root.create(reg, AGENT_NEW);

    expect(root.layoutManager.getState().twoColumns).toEqual([
      root.findSessionKeyByAgentId(AGENT_A),
      root.findSessionKeyByAgentId(AGENT_NEW),
    ]);
  });

  it('selecting a brand-new agent in a multi-pane layout dispatches TERMINAL_PALETTE_NEW_AGENT_EVENT and does not navigate', async () => {
    stubFetchForPalette([{ id: AGENT_NEW, name: 'Brand-new' }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();
    const navigation = recordNavigation(root);
    const palette = await openLoadedPalette(root);

    const detail = await new Promise<TerminalPaletteNewAgentDetail>((resolve) => {
      root.element.addEventListener(
        TERMINAL_PALETTE_NEW_AGENT_EVENT,
        (e) => resolve((e as CustomEvent<TerminalPaletteNewAgentDetail>).detail),
        { once: true }
      );
      selectAgent(palette, AGENT_NEW);
    });

    expect(detail).toEqual({ agentId: AGENT_NEW });
    expect(navigation).toEqual([]);
  });

  it('in the single layout, a pick navigates to the agent like a rail click, for a new and an already-open agent', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }, { id: AGENT_NEW }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    await flush();
    expect(root.layoutManager.getState().active).toBe('single');
    const newAgentListener = vi.fn();
    root.element.addEventListener(TERMINAL_PALETTE_NEW_AGENT_EVENT, newAgentListener);
    const navigation = recordNavigation(root);

    await pickFromPalette(root, AGENT_NEW);
    await pickFromPalette(root, AGENT_A);

    expect(navigation).toEqual([`/terminals/${AGENT_NEW}`, `/terminals/${AGENT_A}`]);
    expect(newAgentListener).not.toHaveBeenCalled();
  });

  it('on a narrow viewport, a pick in a multi-pane layout switches to single and navigates, for a new and an already-open agent', async () => {
    mockNarrowViewport();
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }, { id: AGENT_NEW }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    await flush();
    const keyA = root.findSessionKeyByAgentId(AGENT_A)!;
    const keyB = root.findSessionKeyByAgentId(AGENT_B)!;
    const navigation = recordNavigation(root);

    await pickFromPalette(root, AGENT_NEW);
    expect(root.layoutManager.getState().active).toBe('single');
    root.layoutManager.setLayout('two-columns');
    await pickFromPalette(root, AGENT_B);

    expect(root.layoutManager.getState().active).toBe('single');
    expect(navigation).toEqual([`/terminals/${AGENT_NEW}`, `/terminals/${AGENT_B}`]);
    // The multi-pane assignments are kept for when the user switches back.
    expect(root.layoutManager.getState().twoColumns).toEqual([keyA, keyB]);
  });

  it('focuses the picked pane once the close settles, not before', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    await flush();
    const focusB = vi.spyOn(paneFor(root, AGENT_B), 'focusTerminal');

    const palette = await pickFromPalette(root, AGENT_B);
    await flush();
    expect(focusB).not.toHaveBeenCalled();

    fireFromDialog(palette, 'sl-after-hide');
    expect(focusB).not.toHaveBeenCalled();
    await nextTask();

    expect(focusB).toHaveBeenCalledTimes(1);
  });

  it("focuses a picked new agent's pane once it is created after the close settles", async () => {
    stubFetchForPalette([{ id: AGENT_NEW }]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();

    const palette = await pickFromPalette(root, AGENT_NEW);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    root.create(reg, AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    await flush();

    expect(focusNew).toHaveBeenCalledTimes(1);
  });

  it('a stale selection (candidate no longer in the loaded group) is ignored — no event, nothing placed', async () => {
    stubFetchForPalette([]);
    root = new WorkspaceRoot();
    document.body.append(root.element);
    const newAgentListener = vi.fn();
    root.element.addEventListener(TERMINAL_PALETTE_NEW_AGENT_EVENT, newAgentListener);
    const navigation = recordNavigation(root);

    // The group loaded empty, but the select event fires anyway (a race
    // between a stale render and a fresh refresh) — must not be trusted.
    await pickFromPalette(root, AGENT_NEW);

    expect(newAgentListener).not.toHaveBeenCalled();
    expect(navigation).toEqual([]);
  });
});

function agentCandidate(agentId: string, label = agentId): PaletteCandidate {
  return {
    id: JSON.stringify(['agent', agentId]),
    group: 'agents',
    label,
    searchFields: [label],
    secondaryLabel: '',
    activityMs: 0,
    target: { kind: 'agent', agentId, displayName: label },
  };
}

/** Counts the palette's own agent-list fetches. */
function agentListLoads(): number {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).startsWith('/api/v1/agents?'))
    .length;
}

describe('"Jump to agent" palette: palette and focus lifecycle', () => {
  let root: TerminalWorkspaceRoot;
  let reg: TerminalSessionRegistry;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    stubWebSocketAndEventSource();
    reg = new TerminalSessionRegistry({ hubUrl: window.location.origin, accountId: 'r3' });
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    paletteLoad.override = null;
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  /** Two-columns layout with A and B placed and shown. */
  async function twoColumns(): Promise<void> {
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    await flush();
  }

  /** Picks `agentId` and waits until the palette's close has settled. */
  async function pickAndSettle(agentId: string): Promise<ScionQuickPalette> {
    const palette = await pickFromPalette(root, agentId);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    return palette;
  }

  /** Queues a workspace refresh, the way any session or layout change does. */
  async function refresh(): Promise<void> {
    root.layoutManager.setLayout('two-columns');
    root.layoutManager.setLayout('two-rows');
    root.layoutManager.setLayout('two-columns');
    await flush();
  }

  it('focuses the picked pane once: later refreshes leave focus alone', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_B }]);
    await twoColumns();
    const focusB = vi.spyOn(paneFor(root, AGENT_B), 'focusTerminal');

    await pickAndSettle(AGENT_B);
    expect(focusB).toHaveBeenCalledTimes(1);
    await refresh();

    expect(focusB).toHaveBeenCalledTimes(1);
  });

  it("a new open forgets the previous pick's focus target", async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();
    await pickAndSettle(AGENT_NEW);

    const palette = await openLoadedPalette(root);
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');
    root.create(reg, AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    await flush();

    expect(focusNew).not.toHaveBeenCalled();
  });

  it("never focuses the picked agent's pane while it is hidden", async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();
    await pickAndSettle(AGENT_NEW);

    // A background restore creates the pane without showing it.
    root.create(reg, AGENT_NEW, { deferConnect: true });
    const pane = paneFor(root, AGENT_NEW);
    const focusNew = vi.spyOn(pane, 'focusTerminal');
    await flush();

    expect(pane.hidden).toBe(true);
    expect(focusNew).not.toHaveBeenCalled();
  });

  it('when the placement is cancelled before the pane exists, a pane that turns up later does not take focus', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();
    await pickAndSettle(AGENT_NEW);

    root.cancelPalettePlacement(AGENT_NEW);
    root.create(reg, AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    await flush();

    expect(focusNew).not.toHaveBeenCalled();
  });

  it('cancelling the placement after the pane was created keeps its pending focus', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();

    const palette = await pickFromPalette(root, AGENT_NEW);
    root.create(reg, AGENT_NEW);
    root.cancelPalettePlacement(AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(focusNew).toHaveBeenCalledTimes(1);
  });

  it('once the user focuses another pane, a picked pane created later does not take focus', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();
    await pickAndSettle(AGENT_NEW);

    markPaneFocused(root, AGENT_A);
    root.create(reg, AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    await flush();

    expect(focusNew).not.toHaveBeenCalled();
  });

  it('focus returning to the pane the palette was opened from, before the close settles, keeps the pick', async () => {
    stubFetchForPalette([{ id: AGENT_A }, { id: AGENT_NEW }]);
    root.show(true);
    root.layoutManager.setLayout('two-columns');
    root.create(reg, AGENT_A);
    await flush();

    const palette = await pickFromPalette(root, AGENT_NEW);
    // Shoelace's focus restore lands back in the pane that opened the palette.
    markPaneFocused(root, AGENT_A);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    root.create(reg, AGENT_NEW);
    const focusNew = vi.spyOn(paneFor(root, AGENT_NEW), 'focusTerminal');
    await flush();

    expect(focusNew).toHaveBeenCalledTimes(1);
  });

  it('after the palette component fails to load, the next open loads it again and opens', async () => {
    stubFetchForPalette([{ id: AGENT_A, name: 'Alice-bot' }]);
    const createElement = document.createElement.bind(document);
    let failures = 0;
    vi.spyOn(document, 'createElement').mockImplementation(
      (tag: string, options?: ElementCreationOptions) => {
        if (tag === 'scion-quick-palette' && failures === 0) {
          failures++;
          throw new Error('Failed to fetch dynamically imported module');
        }
        return createElement(tag, options);
      }
    );

    requestPaletteOpen(root);
    await vi.waitFor(() => expect(failures).toBe(1));
    await nextTask();
    expect(root.element.querySelector('scion-quick-palette')).toBeNull();

    const palette = await openLoadedPalette(root);

    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alice-bot']);
  });

  it('an open request while the palette is already open does not reload it', async () => {
    stubFetchForPalette([{ id: AGENT_A }]);
    const palette = await openLoadedPalette(root);
    expect(agentListLoads()).toBe(1);

    requestPaletteOpen(root);
    await nextTask();

    expect(agentListLoads()).toBe(1);
    expect(palette.open).toBe(true);
  });

  it('closing the palette mid-load leaves no error behind', async () => {
    stubFetchForPalette([]);
    paletteLoad.override = ({ controller }): Promise<PaletteCandidate[]> =>
      new Promise((_resolve, reject) => {
        controller.signal.addEventListener('abort', () =>
          reject(new DOMException('load aborted or superseded', 'AbortError'))
        );
      });
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(palette.open).toBe(true));

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    await nextTask();

    expect(palette.groups.agents?.status).toBe('loading');
  });

  it("a superseded load's failure does not replace the newer load's result", async () => {
    stubFetchForPalette([]);
    const loads: Array<{ resolve: (c: PaletteCandidate[]) => void; reject: (e: Error) => void }> =
      [];
    paletteLoad.override = (): Promise<PaletteCandidate[]> =>
      new Promise((resolve, reject) => {
        loads.push({ resolve, reject });
      });
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(loads).toHaveLength(1));
    palette.dispatchEvent(new CustomEvent('palette-retry', { detail: { group: 'agents' } }));
    await vi.waitFor(() => expect(loads).toHaveLength(2));
    loads[1].resolve([agentCandidate(AGENT_B, 'Newer')]);
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));

    loads[0].reject(new Error('agents list failed'));
    await nextTask();

    expect(palette.groups.agents?.status).toBe('ready');
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Newer']);
  });

  it('a reload keeps the previous candidates while it loads and after it fails', async () => {
    stubFetchForPalette([]);
    const loads: Array<{ resolve: (c: PaletteCandidate[]) => void; reject: (e: Error) => void }> =
      [];
    paletteLoad.override = (): Promise<PaletteCandidate[]> =>
      new Promise((resolve, reject) => {
        loads.push({ resolve, reject });
      });
    requestPaletteOpen(root);
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(loads).toHaveLength(1));
    loads[0].resolve([agentCandidate(AGENT_A, 'Alice-bot')]);
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));

    palette.dispatchEvent(new CustomEvent('palette-retry', { detail: { group: 'agents' } }));
    await vi.waitFor(() => expect(loads).toHaveLength(2));

    expect(palette.groups.agents?.status).toBe('loading');
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alice-bot']);

    loads[1].reject(new Error('agents list failed'));
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('error'));
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alice-bot']);
  });

  it('a current load that fails shows the error', async () => {
    stubFetchForPalette([]);
    paletteLoad.override = (): Promise<PaletteCandidate[]> =>
      Promise.reject(new Error('agents list failed'));

    requestPaletteOpen(root);
    const palette = await waitForPalette(root);

    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('error'));
    expect(palette.groups.agents?.error).toBe('agents list failed');
  });
});

describe('"Jump to agent" palette: keyboard shortcut', () => {
  let root: TerminalWorkspaceRoot;
  let reg: TerminalSessionRegistry;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    stubFetchForPalette([]);
    stubWebSocketAndEventSource();
    root = new WorkspaceRoot();
    document.body.append(root.element);
    root.show(true);
    reg = new TerminalSessionRegistry({ hubUrl: window.location.origin, accountId: 'r4' });
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  /** Dispatches a keydown; returns true when nothing called preventDefault(). */
  function press(init: KeyboardEventInit, target: EventTarget = document): boolean {
    const event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init });
    return target.dispatchEvent(event);
  }

  async function expectOpened(): Promise<ScionQuickPalette> {
    const palette = await waitForPalette(root);
    await vi.waitFor(() => expect(palette.open).toBe(true));
    return palette;
  }

  async function expectNotOpened(): Promise<void> {
    await nextTask();
    expect(root.element.querySelector('scion-quick-palette')).toBeNull();
  }

  it('does nothing while the workspace is hidden (e.g. the user is on /chat)', async () => {
    root.show(false);
    expect(press({ key: 'k', metaKey: true })).toBe(true);
    await expectNotOpened();
  });

  it('Meta+K opens the palette from outside any pane', async () => {
    expect(press({ key: 'k', metaKey: true })).toBe(false);
    await expectOpened();
  });

  it('Ctrl+K opens the palette from outside any pane', async () => {
    expect(press({ key: 'k', ctrlKey: true })).toBe(false);
    await expectOpened();
  });

  it('Meta+K opens the palette even when the keydown originates inside a terminal pane', async () => {
    root.create(reg, AGENT_A);
    const pane = root.element.querySelector('scion-terminal-pane')!;

    expect(press({ key: 'k', metaKey: true }, pane)).toBe(false);

    await expectOpened();
  });

  it('Ctrl+K does NOT open the palette when the keydown originates inside a terminal pane (it must keep reaching the PTY)', async () => {
    root.create(reg, AGENT_A);
    const pane = root.element.querySelector('scion-terminal-pane')!;

    expect(press({ key: 'k', ctrlKey: true }, pane)).toBe(true);

    await expectNotOpened();
  });

  it('neither Ctrl+K nor Meta+K fire with both modifiers, Alt, or Shift held', async () => {
    expect(press({ key: 'k', ctrlKey: true, metaKey: true })).toBe(true);
    expect(press({ key: 'k', ctrlKey: true, altKey: true })).toBe(true);
    expect(press({ key: 'k', ctrlKey: true, shiftKey: true })).toBe(true);
    await expectNotOpened();
  });

  function openModal(kind: 'sl-dialog' | 'sl-drawer' | 'dialog'): HTMLElement {
    const el = document.createElement(kind) as HTMLElement & { open?: boolean };
    if (kind === 'dialog') el.setAttribute('open', '');
    else el.open = true;
    return el;
  }

  it.each(['sl-dialog', 'sl-drawer', 'dialog'] as const)(
    'an unrelated open %s blocks the shortcut',
    async (kind) => {
      const modal = openModal(kind);
      document.body.appendChild(modal);
      try {
        expect(press({ key: 'k', metaKey: true })).toBe(true);

        await expectNotOpened();
      } finally {
        modal.remove();
      }
    }
  );

  it("an open dialog inside a terminal pane's shadow root blocks Meta+K from the pane", async () => {
    root.create(reg, AGENT_A);
    const pane = root.element.querySelector('scion-terminal-pane')!;
    await pane.updateComplete;
    const modal = openModal('sl-dialog');
    pane.shadowRoot!.appendChild(modal);

    expect(press({ key: 'k', metaKey: true, composed: true }, modal)).toBe(true);

    await expectNotOpened();
  });

  it('an open dialog inside a nested shadow root blocks the shortcut', async () => {
    root.create(reg, AGENT_A);
    const pane = root.element.querySelector('scion-terminal-pane')!;
    await pane.updateComplete;
    const inner = document.createElement('div');
    inner.attachShadow({ mode: 'open' }).appendChild(openModal('dialog'));
    pane.shadowRoot!.appendChild(inner);

    expect(press({ key: 'k', metaKey: true })).toBe(true);

    await expectNotOpened();
  });

  it("an open dialog inside a hidden retained pane's shadow root does not block the shortcut", async () => {
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    const hiddenPane = paneFor(root, AGENT_A);
    const visiblePane = paneFor(root, AGENT_B);
    await hiddenPane.updateComplete;
    expect(hiddenPane.hidden).toBe(true);
    expect(visiblePane.hidden).toBe(false);
    hiddenPane.shadowRoot!.appendChild(openModal('sl-dialog'));

    expect(press({ key: 'k', metaKey: true })).toBe(false);

    await expectOpened();
  });

  it("an open dialog inside the visible pane's shadow root still blocks while another pane is hidden", async () => {
    root.create(reg, AGENT_A);
    root.create(reg, AGENT_B);
    const visiblePane = paneFor(root, AGENT_B);
    await visiblePane.updateComplete;
    expect(paneFor(root, AGENT_A).hidden).toBe(true);
    visiblePane.shadowRoot!.appendChild(openModal('sl-dialog'));

    expect(press({ key: 'k', metaKey: true })).toBe(true);

    await expectNotOpened();
  });

  it("the palette's own dialog does not block the shortcut", async () => {
    press({ key: 'k', metaKey: true });
    const palette = await expectOpened();
    press({ key: 'k', metaKey: true });
    await palette.updateComplete;
    const ownDialog = palette.shadowRoot!.querySelector('sl-dialog') as HTMLElement & {
      open?: boolean;
    };
    expect(ownDialog).not.toBeNull();
    ownDialog.open = true; // as while its close is still settling

    expect(press({ key: 'k', metaKey: true })).toBe(false);

    await vi.waitFor(() => expect(palette.open).toBe(true));
  });

  it('a closed sl-dialog or dialog does not block the shortcut', async () => {
    const slDialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    slDialog.open = false;
    const dialog = document.createElement('dialog');
    document.body.append(slDialog, dialog);
    try {
      expect(press({ key: 'k', metaKey: true })).toBe(false);

      await expectOpened();
    } finally {
      slDialog.remove();
      dialog.remove();
    }
  });

  it('does not activate while an IME composition is in progress', async () => {
    expect(press({ key: 'k', metaKey: true, isComposing: true })).toBe(true);
    await expectNotOpened();
  });

  it('does not activate when another handler already claimed the keydown', async () => {
    const claim = (e: Event): void => e.preventDefault();
    document.body.addEventListener('keydown', claim);

    press({ key: 'k', metaKey: true }, document.body);

    await expectNotOpened();
    document.body.removeEventListener('keydown', claim);
  });

  it('opens with Caps Lock on (an upper-case K)', async () => {
    expect(press({ key: 'K', metaKey: true })).toBe(false);
    await expectOpened();
  });

  it('does not activate on a repeat keydown', async () => {
    expect(press({ key: 'k', metaKey: true, repeat: true })).toBe(true);
    await expectNotOpened();
  });

  it('pressing the shortcut again while the palette is open closes it without reloading', async () => {
    const fetchMock = vi.mocked(fetch);
    press({ key: 'k', metaKey: true });
    const palette = await expectOpened();
    const agentLoads = fetchMock.mock.calls.filter(([url]) =>
      String(url).startsWith('/api/v1/agents?')
    ).length;

    expect(press({ key: 'k', ctrlKey: true })).toBe(false);

    expect(palette.open).toBe(false);
    expect(
      fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/agents?')).length
    ).toBe(agentLoads);
  });

  it('a second press while the palette is still loading cancels the open', async () => {
    press({ key: 'k', metaKey: true });
    press({ key: 'k', metaKey: true });

    const palette = await waitForPalette(root);
    await nextTask();

    expect(palette.open).toBe(false);
  });

  it('dispose() closes an open palette and removes its element', async () => {
    press({ key: 'k', metaKey: true });
    const palette = await expectOpened();

    root.dispose();

    expect(palette.open).toBe(false);
    expect(palette.isConnected).toBe(false);
  });

  it('dispose() removes the document-level shortcut listener', async () => {
    root.dispose();

    expect(press({ key: 'k', metaKey: true })).toBe(true);

    await expectNotOpened();
  });

  it('dispose() removes the document-level focusin listener it installed', () => {
    const add = vi.spyOn(document, 'addEventListener');
    const remove = vi.spyOn(document, 'removeEventListener');
    const other = new WorkspaceRoot();
    const installed = add.mock.calls.filter(([type]) => type === 'focusin').map(([, l]) => l);
    expect(installed).toHaveLength(1);

    other.dispose();

    expect(remove).toHaveBeenCalledWith('focusin', installed[0]);
  });
});
