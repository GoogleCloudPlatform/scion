/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Foreground-only web reconnect for terminal panes.
 *
 * Model: CONNECTED -> (retriable close or liveness timeout) -> DISCONNECTED
 * -> (frontmost) -> ATTEMPTING -> CONNECTED or FAILED, then
 * FAILED -> (2 min while not frontmost) -> DISCONNECTED. Exactly one
 * attempt per foregrounding, no backoff, no attempt cap.
 *
 * Covers close-code classification without an auto attempt, foreground/
 * background gating and the 2-minute background reset, multi-pane
 * visibility and document-hidden gating, agent-stop/restart re-arming,
 * heartbeat liveness, and the first-frame guard's scope and cleanup.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  TerminalSessionRegistry,
  type TerminalResourceInitializer,
  type TerminalResources,
  type TerminalSession,
} from './terminal-sessions.js';

const agentId = '11111111-1111-4111-8111-111111111111';
const otherId = '22222222-2222-4222-8222-222222222222';
const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };
const agent = { id: agentId, name: 'example', phase: 'running', activity: 'executing' };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number; reason?: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  send = vi.fn<(data: string) => void>();
  close = vi.fn(() => {
    this.readyState = 3;
  });
  constructor(readonly url: string) {
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

function fixture() {
  FakeSocket.instances = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      close() {}
    }
  );
  const fetcher = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) =>
    Promise.resolve(json(agent))
  );
  vi.stubGlobal('fetch', fetcher);
  let size = { cols: 80, rows: 24 };
  const resources = {
    write: vi.fn<(bytes: Uint8Array) => void>(),
    size: vi.fn(() => size),
    dispose: vi.fn(),
    reset: vi.fn(),
  } satisfies TerminalResources;
  const setSize = (cols: number, rows: number): void => {
    size = { cols, rows };
  };
  const initialize = vi.fn<TerminalResourceInitializer>(() => Promise.resolve(resources));
  const registry = new TerminalSessionRegistry(scope);
  return { fetcher, resources, initialize, registry, setSize };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

/** Connect and open the next socket; returns it. */
async function connectAndOpen(session: TerminalSession): Promise<FakeSocket> {
  await session.connect();
  const socket = FakeSocket.instances[FakeSocket.instances.length - 1];
  socket.open();
  socket.data(); // confirms the stream live, same as tmux's redraw on attach
  return socket;
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

describe('terminal close codes classify without an auto attempt', () => {
  it.each([
    [1000, 'detached'],
    [4410, 'session-ended'],
    [4404, 'not-found'],
  ] as const)(
    'close %d -> disconnectReason=%s, and frontmost never dials (terminal, not retriable)',
    async (code, reason) => {
      const f = fixture();
      const session = f.registry.open(agentId, f.initialize);
      const socket = await connectAndOpen(session);
      session.setFrontmost(true);
      socket.readyState = 3;
      socket.onclose?.({ code });

      expect(session.state.connection).toBe('disconnected');
      expect(session.state.disconnectReason).toBe(reason);
      expect(FakeSocket.instances).toHaveLength(1);
    }
  );
});

describe('close()/markUnavailable() during the 2-min background wait suppress it', () => {
  async function reachFailedWhileFrontmost(session: TerminalSession): Promise<void> {
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 4503 }); // frontmost: one immediate attempt
    await session.connect();
    FakeSocket.instances[1].readyState = 3;
    FakeSocket.instances[1].onclose?.({ code: 4503 }); // that attempt also fails
    expect(session.state.reconnectFailed).toBe(true);
  }

  it('close() before the reset elapses prevents any later reconnect', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await reachFailedWhileFrontmost(session);

    session.setFrontmost(false); // arms the 2-minute background timer
    session.close();
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1_000);

    expect(FakeSocket.instances).toHaveLength(2);
    expect(session.state.connection).toBe('closed');
  });

  it('markUnavailable() before the reset elapses suppresses the later reconnect', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await reachFailedWhileFrontmost(session);

    session.setFrontmost(false);
    session.markUnavailable('agent-deleted', 'Agent was deleted.');
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1_000);

    expect(FakeSocket.instances).toHaveLength(2);
    expect(session.state.connection).toBe('unavailable');
  });
});

describe('a session that never connected does not auto-retry', () => {
  it('the first-ever attempt failing before open does not attempt again, even frontmost', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const socket = FakeSocket.instances[0];
    // Socket never opens; a retriable close arrives instead.
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });

    expect(session.state.connection).toBe('disconnected');
    session.setFrontmost(true);
    expect(FakeSocket.instances).toHaveLength(1);
  });
});

describe('input during a reconnect attempt is dropped, never replayed', () => {
  it('sendData during the auto-triggered attempt returns false and is never sent', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });

    expect(session.reconnecting).toBe(true);
    expect(session.sendData('typed-during-reconnect')).toBe(false);

    await session.connect();
    FakeSocket.instances[1].open();
    expect(FakeSocket.instances[1].send).not.toHaveBeenCalled();
  });
});

describe('a resize while waiting to foreground reaches the reconnect URL', () => {
  it('the new socket URL carries the size measured at attempt time', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 }); // backgrounded: armed, waiting

    f.setSize(120, 40); // window resized while waiting for the next foregrounding
    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();

    const url = new URL(FakeSocket.instances[1].url);
    expect(url.searchParams.get('cols')).toBe('120');
    expect(url.searchParams.get('rows')).toBe('40');
  });
});

describe('a frontmost pane retries once, immediately, on a retriable close', () => {
  it.each([1006, 4503] as const)(
    'code %d while already frontmost -> immediate attempt with no extra focus event; success resets once',
    async (code) => {
      const f = fixture();
      const session = f.registry.open(agentId, f.initialize);
      const socket0 = await connectAndOpen(session);
      session.setFrontmost(true); // already frontmost before the drop

      socket0.readyState = 3;
      socket0.onclose?.({ code });
      // No further setFrontmost call: the attempt must already be running.
      expect(session.reconnecting).toBe(true);

      await session.connect();
      expect(FakeSocket.instances).toHaveLength(2);
      FakeSocket.instances[1].open();
      FakeSocket.instances[1].data(); // confirms the reconnect attempt live

      expect(session.state.connection).toBe('connected');
      expect(f.resources.reset).toHaveBeenCalledTimes(1);
      expect(session.state.reconnectFailed).toBe(false);
    }
  );

  it('failure shows FAILED (reconnectFailed) and does not attempt again on its own', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 4503 });
    await session.connect();

    FakeSocket.instances[1].readyState = 3;
    FakeSocket.instances[1].onclose?.({ code: 4503 }); // the one attempt also fails

    expect(session.state.reconnectFailed).toBe(true);
    expect(session.state.connection).toBe('disconnected');
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('a background pane arms but does not attempt until foregrounded', () => {
  it('retriable close while backgrounded stays disconnected with zero attempts, then attempts once when shown', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    // Never frontmost yet: default is background.
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('network');
    expect(FakeSocket.instances).toHaveLength(1);

    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('a failed pane resets to disconnected after 2 minutes in the background', () => {
  async function reachFailedWhileFrontmost(session: TerminalSession): Promise<void> {
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 4503 });
    await session.connect();
    FakeSocket.instances[1].readyState = 3;
    FakeSocket.instances[1].onclose?.({ code: 4503 });
    expect(session.state.reconnectFailed).toBe(true);
  }

  it('2 min in the background resets FAILED; the reset alone does not dial; the next foregrounding attempts once', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await reachFailedWhileFrontmost(session);

    session.setFrontmost(false);
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1);
    expect(session.state.reconnectFailed).toBe(false);
    expect(FakeSocket.instances).toHaveLength(2); // reset alone does not dial

    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(3);
  });

  it('foregrounding before 2 minutes leaves FAILED with no attempt', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await reachFailedWhileFrontmost(session);

    session.setFrontmost(false);
    await vi.advanceTimersByTimeAsync(60_000); // 1 min: short of the 2-min reset
    session.setFrontmost(true);

    expect(session.state.reconnectFailed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(2);

    // The timer only runs while backgrounded: even a full 2 minutes elapsing
    // later while frontmost must not fire.
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1);
    expect(session.state.reconnectFailed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('multi-pane: every visible pane attempts once; a hidden document attempts nothing', () => {
  it('two independent sessions each attempt once when their panes are made frontmost', async () => {
    const f = fixture();
    const agent2 = { ...agent, id: otherId };
    f.fetcher.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      return Promise.resolve(json(url.includes(otherId) ? agent2 : agent));
    });
    const resources2 = {
      write: vi.fn<(bytes: Uint8Array) => void>(),
      size: () => ({ cols: 80, rows: 24 }),
      dispose: vi.fn(),
      reset: vi.fn(),
    } satisfies TerminalResources;

    const s1 = f.registry.open(agentId, f.initialize);
    const socket1 = await connectAndOpen(s1);
    const s2 = f.registry.open(otherId, () => Promise.resolve(resources2));
    const socket2 = await connectAndOpen(s2);

    socket1.readyState = 3;
    socket1.onclose?.({ code: 1006 });
    socket2.readyState = 3;
    socket2.onclose?.({ code: 1006 });
    expect(FakeSocket.instances).toHaveLength(2); // backgrounded: no attempts yet

    s1.setFrontmost(true);
    s2.setFrontmost(true);
    // Proves each session's own foregrounding, not connect(), dialed the attempt.
    expect(s1.reconnecting).toBe(true);
    expect(s2.reconnecting).toBe(true);
    await s1.connect();
    await s2.connect();
    expect(FakeSocket.instances).toHaveLength(4);
  });

  it('a pane visible in the layout but hidden by the document attempts nothing until the document is visible', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    // Layout-visible, but setFrontmost(false) models a hidden document.
    session.setFrontmost(false);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    expect(FakeSocket.instances).toHaveLength(1);

    session.setFrontmost(true); // document becomes visible again
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('agent-stopped re-arms once the agent is running again', () => {
  it('no attempt while stopped; one attempt once running again while already frontmost', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    FakeSocket.instances[0].data();
    session.markUnavailable('agent-stopped', 'Agent has stopped.');
    expect(session.state.connection).toBe('unavailable');

    session.setFrontmost(true);
    expect(FakeSocket.instances).toHaveLength(1); // foregrounding alone: still stopped

    session.noteAgentAvailable(); // SSE: phase running again
    expect(session.reconnecting).toBe(true); // proves noteAgentAvailable, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it('agent running again while backgrounded waits for the next foregrounding', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    FakeSocket.instances[0].data();
    session.markUnavailable('agent-stopped', 'Agent has stopped.');

    session.noteAgentAvailable(); // not frontmost: armed, no attempt yet
    expect(session.reconnecting).toBe(false);
    expect(FakeSocket.instances).toHaveLength(1);

    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('focusing or re-showing a healthy connected pane never reconnects', () => {
  it('toggling frontmost while connected causes no attempt', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    FakeSocket.instances[0].data();

    session.setFrontmost(true);
    session.setFrontmost(false);
    session.setFrontmost(true);

    expect(session.state.connection).toBe('connected');
    expect(FakeSocket.instances).toHaveLength(1);
  });
});

describe('no background timers other than the 2-minute reset', () => {
  it('a disconnected, backgrounded (armed) session never dials on its own', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    const fetchesAtConnect = f.fetcher.mock.calls.length;

    // Never frontmost: armed and waiting. No timer should exist for this state.
    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });
    await vi.advanceTimersByTimeAsync(10 * 60_000);

    expect(FakeSocket.instances).toHaveLength(1);
    expect(f.fetcher).toHaveBeenCalledTimes(fetchesAtConnect);
  });
});

describe('heartbeat liveness (capability-detected; never drives retries directly)', () => {
  it('without a pong ever received, the heartbeat never forces a disconnect', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    FakeSocket.instances[0].data();

    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(session.state.connection).toBe('connected');
  });

  it('a pong, then silence for 10s after the next ping, disconnects without waiting for onclose', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const socket = FakeSocket.instances[0];
    socket.open();
    socket.data(); // confirms live and starts the heartbeat
    // Backgrounded: isolates the heartbeat's own detection from the
    // frontmost auto-attempt behaviour, which other tests cover separately.
    // It detects a dead link and enters disconnected, but it does not
    // drive retries.

    await vi.advanceTimersByTimeAsync(20_000); // ping #1 sent
    socket.onmessage?.({ data: JSON.stringify({ type: 'pong' }) }); // capability detected
    await vi.advanceTimersByTimeAsync(20_000); // ping #2 sent; nothing replies
    await vi.advanceTimersByTimeAsync(10_000); // timeout window elapses

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('network');
    expect(FakeSocket.instances).toHaveLength(1); // no attempt: not frontmost

    // Foregrounding afterward runs the single owed attempt.
    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true); // proves foregrounding, not connect(), dialed the attempt
    await session.connect();
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('an HTTP failure during an attempt still re-arms after the 2-min reset', () => {
  it('preflight 503 -> FAILED -> 2 min in the background -> foreground -> attempts again', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);

    // The frontmost auto-attempt reaches the broker preflight and is refused.
    f.fetcher.mockResolvedValueOnce(json(agent)).mockResolvedValueOnce(json({}, 503));
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    await session.connect(); // awaits that attempt's settlement

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('server-error');
    expect(session.state.reconnectFailed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(1); // preflight failed before a new socket

    session.setFrontmost(false);
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1);
    expect(session.state.reconnectFailed).toBe(false);

    // Arming must not be derived from disconnectReason === 'network' alone,
    // or a session left disconnected/'server-error' would never attempt again.
    f.fetcher.mockResolvedValue(json(agent));
    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true);
    await session.connect();
    FakeSocket.instances[1].open();
    FakeSocket.instances[1].data(); // confirms the reconnect attempt live
    expect(session.state.connection).toBe('connected');
  });
});

describe('a markUnavailable race during an in-flight attempt', () => {
  it('an attempt superseded by markUnavailable(agent-stopped) never marks reconnectFailed, so the agent-restart re-arm still works', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);

    // The frontmost auto-attempt starts and stalls mid-flight, awaiting the
    // agent fetch.
    const gate = deferred<Response>();
    f.fetcher.mockReturnValueOnce(gate.promise);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    expect(session.reconnecting).toBe(true);

    // SSE reports the agent stopped while that attempt is still in flight.
    session.markUnavailable('agent-stopped', 'Agent has stopped.');
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.reconnectFailed).toBe(false);

    // The stalled (now-superseded) attempt settles: wait for its `finally`
    // to actually run (pending clears) rather than counting microtask hops.
    gate.resolve(json(agent));
    await vi.waitFor(() => expect(session.reconnecting).toBe(false));

    // A superseded attempt's `finally` must not judge itself against the
    // *current* state; that would flip reconnectFailed here.
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-stopped');
    expect(session.state.reconnectFailed).toBe(false);

    // The agent-restart re-arm still works: the agent comes back, and the
    // pane is frontmost, so the attempt fires immediately (blocked forever
    // if reconnectFailed had wrongly been set, since no background timer
    // runs while frontmost).
    session.noteAgentAvailable();
    expect(session.reconnecting).toBe(true);
  });
});

describe('accept-then-close does not loop', () => {
  it('open, then close 4503 without any data frame -> FAILED with exactly 2 sockets', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 4503 }); // frontmost: one immediate attempt
    await session.connect();

    // The Hub upgrades the reconnect attempt's socket (HTTP 101 succeeds),
    // but then closes it before any data frame arrives (OpenStream failed).
    // Before the fix, onopen alone counted as "attempt succeeded", so this
    // close looked like a fresh disconnect and redialed immediately,
    // forever, wiping resources.reset() on every cycle.
    FakeSocket.instances[1].open();
    FakeSocket.instances[1].readyState = 3;
    FakeSocket.instances[1].onclose?.({ code: 4503 });

    expect(session.state.reconnectFailed).toBe(true);
    expect(session.state.connection).toBe('disconnected');
    expect(FakeSocket.instances).toHaveLength(2); // one attempt per foregrounding, not a loop

    // Staying frontmost must not redial on its own either.
    session.setFrontmost(false);
    session.setFrontmost(true);
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it('a first-frame-guard timeout (socket opens, no data ever arrives) counts as a failed attempt, not a loop', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 4503 });
    await session.connect();

    FakeSocket.instances[1].open(); // opens, but the stream never sends anything
    await vi.advanceTimersByTimeAsync(10_000 + 1);

    expect(session.state.reconnectFailed).toBe(true);
    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('network');
    expect(FakeSocket.instances).toHaveLength(2);
  });
});

describe('the first-frame guard applies to reconnect attempts only, not the initial connect', () => {
  it('an initial connect with no data for over 10s is not failed', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open(); // Hub upgrades; the broker exec/attach is still slow

    await vi.advanceTimersByTimeAsync(10_000 + 1);

    // Before this fix, the 10s guard would have applied to every attempt,
    // including this one, and failed a legitimately slow cold start (e.g. a
    // Cloud Run sandbox exec) that main accepts today.
    expect(session.state.connection).toBe('connecting');
    expect(session.state.reconnectFailed).toBe(false);
    expect(FakeSocket.instances).toHaveLength(1); // no guard-triggered redial

    // It still only counts as connected once data actually arrives, however
    // late — counting a data frame as live is unconditional, unlike the guard.
    FakeSocket.instances[0].data();
    expect(session.state.connection).toBe('connected');
  });

  it('a reconnect attempt (after the session has connected once) is still bounded by the guard', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    await session.connect();

    FakeSocket.instances[1].open(); // opens, but the stream never sends anything
    await vi.advanceTimersByTimeAsync(10_000 + 1);

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.reconnectFailed).toBe(true);
  });
});

describe('additional coverage for the arming and onerror-fallback fixes', () => {
  it('an agent-fetch 5xx during an attempt re-arms after the 2-min reset', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);

    f.fetcher.mockResolvedValueOnce(json({}, 502));
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    await session.connect();
    expect(session.state.disconnectReason).toBe('server-error');
    expect(session.state.reconnectFailed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(1); // the agent fetch failed before any socket

    session.setFrontmost(false);
    await vi.advanceTimersByTimeAsync(2 * 60_000 + 1);
    session.setFrontmost(true);
    expect(session.reconnecting).toBe(true);
  });

  it('the onerror fallback (no close ever arrives) judges a failed reconnect attempt correctly', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    await session.connect();

    // The reconnect attempt's socket errors and never closes; only the 1s
    // fallback fires. Before the fix, this fallback always treated itself as
    // a fresh disconnect and immediately redialed again (a loop), instead of
    // recognizing this attempt itself had failed.
    FakeSocket.instances[1].onerror?.();
    await vi.advanceTimersByTimeAsync(1_000 + 1);

    expect(session.state.disconnectReason).toBe('connect-error');
    expect(session.state.reconnectFailed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(2); // no immediate extra redial
  });
});

describe("the first-frame guard's timer is cleared on every teardown path", () => {
  /** Gets a reconnect attempt into 'connecting' with the guard armed (opened, no data yet). */
  async function armReconnectGuard(session: TerminalSession): Promise<FakeSocket> {
    const socket0 = await connectAndOpen(session);
    session.setFrontmost(true);
    socket0.readyState = 3;
    socket0.onclose?.({ code: 1006 });
    await vi.waitFor(() => expect(FakeSocket.instances.length).toBe(2));
    await vi.waitFor(() => expect(session.state.connection).toBe('connecting'));
    const socket1 = FakeSocket.instances[1];
    socket1.open(); // opens, but sends no data: the guard is now armed
    return socket1;
  }

  it('cleared on close(): no stray timer, and nothing fires afterward', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await armReconnectGuard(session);
    expect(vi.getTimerCount()).toBeGreaterThan(0);

    session.close();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(session.state.connection).toBe('closed');
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it('cleared on markUnavailable(): no stray timer, and nothing fires afterward', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await armReconnectGuard(session);

    session.markUnavailable('agent-stopped', 'x');
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.reconnectFailed).toBe(false);
    expect(FakeSocket.instances).toHaveLength(2); // the stale guard never redialed
  });

  it('cleared when the guarded socket itself closes: no stray timer left running', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const socket1 = await armReconnectGuard(session);

    socket1.readyState = 3;
    socket1.onclose?.({ code: 4503 });
    expect(session.state.reconnectFailed).toBe(true);
    expect(vi.getTimerCount()).toBe(0); // still frontmost: no background-reset timer either
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(FakeSocket.instances).toHaveLength(2);
  });

  // startAttempt() also clears the guard at the top (defense in depth: every
  // other exit from 'connecting' already clears it, above, so this line
  // never has a live timer to clear given today's invariants — reverting it
  // alone does not fail this or any other committed test). This pins the
  // no-leftover-timer behavior across a full ended-attempt -> fresh-attempt
  // cycle, the shape a startAttempt-only bug would need to break.
  it('a fresh attempt after an ended one starts from a clean slate: no leftover timer', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await armReconnectGuard(session);
    session.markUnavailable('agent-stopped', 'x'); // ends the guarded attempt; state -> 'unavailable'
    expect(vi.getTimerCount()).toBe(0);

    session.setFrontmost(true);
    session.noteAgentAvailable(); // the agent-restart re-arm: a fresh attempt
    await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(3));
    expect(vi.getTimerCount()).toBe(0); // the new attempt hasn't reached 'connecting' yet
  });
});
