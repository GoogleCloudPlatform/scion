// @vitest-environment happy-dom
/**
 * Tests for TerminalWorkspaceRoot: data-effective-layout attribute
 * and focus outline suppression in single-pane mode (#1716).
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { TerminalSessionRegistry } from './terminal-sessions.js';

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
  let replaceStateSpy: ReturnType<typeof vi.spyOn>;

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
