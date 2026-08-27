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

/** Tests for SSEClient reconnection. */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { SSEClient } from './sse-client.js';

/** Stand-in for EventSource, which happy-dom does not implement. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  static instances: FakeEventSource[] = [];

  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;

  constructor(readonly url: string) {
    super();
    FakeEventSource.instances.push(this);
  }

  closed = false;

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
    this.closed = true;
  }

  /** Server accepted the connection. */
  simulateOpen(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  /**
   * Transport failed after the connection was live. readyState is CLOSED
   * A live connection failing. Per spec this leaves readyState CONNECTING
   * while the browser reestablishes, but the client classifies on whether the
   * connection had opened, not on readyState.
   */
  simulateDrop(): void {
    this.readyState = FakeEventSource.CONNECTING;
    this.onerror?.(new Event('error'));
  }

  /**
   * A handshake that never opened — a rejected one (401, or a redirect to the
   * login page) leaves readyState CLOSED per spec.
   */
  simulateHandshakeFailure(): void {
    this.readyState = FakeEventSource.CLOSED;
    this.onerror?.(new Event('error'));
  }
}

/**
 * Fail the newest handshake and settle the auth probe it triggers, so the
 * caller can advance timers synchronously afterwards.
 */
async function failHandshake(): Promise<void> {
  latest().simulateHandshakeFailure();
  await vi.advanceTimersByTimeAsync(0);
}

/** The EventSource the client is currently holding, i.e. the newest one. */
function latest(): FakeEventSource {
  const es = FakeEventSource.instances[FakeEventSource.instances.length - 1];
  if (!es) throw new Error('no EventSource was constructed');
  return es;
}

let visibility: DocumentVisibilityState = 'visible';

/** Move the tab between foreground and background, as the browser would. */
function setVisibility(state: DocumentVisibilityState): void {
  visibility = state;
  document.dispatchEvent(new Event('visibilitychange'));
}

/** Open a connection and let it reach the live state. */
function connectAndOpen(subjects = ['agent.>']): SSEClient {
  const client = new SSEClient();
  client.connect(subjects);
  latest().simulateOpen();
  return client;
}

beforeEach(() => {
  vi.useFakeTimers();
  // Jitter is proportional and signed; 0.5 is its zero point, keeping timer
  // maths exact.
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  // A handshake that never opened probes this before retrying; an authorised
  // session answers 200 and the retry proceeds.
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ status: 200, redirected: false }));
  visibility = 'visible';
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => visibility,
  });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('SSEClient visibility handling', () => {
  it('reconnects immediately when the tab becomes visible', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    latest().simulateDrop();
    expect(FakeEventSource.instances).toHaveLength(1);

    setVisibility('visible');

    // No timer was advanced: the reconnect happened on the transition.
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('cancels the pending backoff so the tab switch does not double-connect', () => {
    const client = connectAndOpen();
    latest().simulateDrop();
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(2);

    // The 1s backoff queued by the drop must have been cleared, not left
    // to fire into a connection that already exists.
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('resets the attempt counter when the tab becomes visible', async () => {
    const client = connectAndOpen();
    latest().simulateDrop();

    // Burn several attempts while the tab is in the background.
    for (let i = 0; i < 4; i++) {
      vi.advanceTimersByTime(30_000);
      await failHandshake();
    }
    expect(client.reconnectAttemptCount).toBeGreaterThan(1);

    setVisibility('visible');

    expect(client.reconnectAttemptCount).toBe(0);
    client.disconnect();
  });

  it('does not open a second connection while one is connecting', () => {
    const client = new SSEClient();
    client.connect(['agent.>']);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(latest().readyState).toBe(FakeEventSource.CONNECTING);

    setVisibility('visible');

    // The in-flight handshake resolves on its own. Replacing it here is what
    // starved every retry when a user switched tabs repeatedly.
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('does not reconnect while the connection is open', () => {
    const client = connectAndOpen();
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('ignores a transition to hidden', () => {
    const client = connectAndOpen();
    latest().simulateDrop();
    setVisibility('hidden');
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });
});

describe('SSEClient teardown', () => {
  it('removes the visibility listener on disconnect', () => {
    const client = connectAndOpen();
    const removed = vi.spyOn(document, 'removeEventListener');

    client.disconnect();

    expect(removed).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('cancels a pending reconnect timer on disconnect', () => {
    const client = connectAndOpen();
    latest().simulateDrop();

    client.disconnect();
    vi.advanceTimersByTime(120_000);

    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('stays reusable: connect after disconnect re-arms the visibility listener', () => {
    const client = connectAndOpen();
    client.disconnect();

    client.connect(['agent.>']);
    latest().simulateOpen();
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().simulateDrop();

    setVisibility('visible');

    expect(FakeEventSource.instances).toHaveLength(3);
    client.disconnect();
  });
});

describe('SSEClient backoff', () => {
  it('backs off exponentially, capped at 30s', async () => {
    const client = connectAndOpen();
    latest().simulateDrop();

    // First retry after 1s.
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);

    // Second after 2s. The retry's handshake never opens, which is what a
    // server that is still down looks like.
    await failHandshake();
    vi.advanceTimersByTime(1_999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);
    client.disconnect();
  });

  it('widens to the slow cap after the fast attempts and keeps retrying', async () => {
    const client = connectAndOpen();
    latest().simulateDrop();

    for (let attempt = 1; attempt <= 10; attempt++) {
      vi.advanceTimersByTime(30_000);
      expect(FakeEventSource.instances).toHaveLength(attempt + 1);
      await failHandshake();
    }

    // Past the fast attempts the interval widens to 5 minutes.
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(11);
    vi.advanceTimersByTime(240_000);
    expect(FakeEventSource.instances).toHaveLength(12);
    expect(client.reconnectAttemptCount).toBe(11);
    client.disconnect();
  });

  it('resets the backoff once a connection opens', () => {
    const client = connectAndOpen();
    latest().simulateDrop();
    vi.advanceTimersByTime(1_000);
    latest().simulateOpen();

    expect(client.reconnectAttemptCount).toBe(0);
    latest().simulateDrop();
    vi.advanceTimersByTime(1_000);
    expect(FakeEventSource.instances).toHaveLength(3);
    client.disconnect();
  });
});

describe('SSEClient connection events', () => {
  it('dispatches disconnected once per dropped connection, then reconnecting', async () => {
    const client = connectAndOpen();
    const disconnected = vi.fn();
    const reconnecting = vi.fn();
    client.addEventListener('disconnected', disconnected);
    client.addEventListener('reconnecting', reconnecting);

    latest().simulateDrop();
    expect(disconnected).toHaveBeenCalledTimes(1);
    expect(reconnecting).toHaveBeenCalledTimes(1);
    expect((reconnecting.mock.calls[0]?.[0] as CustomEvent).detail).toEqual({ attempt: 1 });

    // Retries that never reach an open connection are not further drops.
    vi.advanceTimersByTime(1_000);
    await failHandshake();
    expect(disconnected).toHaveBeenCalledTimes(1);
    expect(reconnecting).toHaveBeenCalledTimes(2);
    client.disconnect();
  });

  it('reports a fresh drop after the connection comes back', () => {
    const client = connectAndOpen();
    const disconnected = vi.fn();
    client.addEventListener('disconnected', disconnected);

    latest().simulateDrop();
    vi.advanceTimersByTime(1_000);
    latest().simulateOpen();
    latest().simulateDrop();

    expect(disconnected).toHaveBeenCalledTimes(2);
    client.disconnect();
  });

  it('reports a drop when a server-initiated reconnect never lands', () => {
    const client = connectAndOpen();
    const disconnected = vi.fn();
    client.addEventListener('disconnected', disconnected);

    // The hub asks the client to reconnect before a clean shutdown.
    latest().dispatchEvent(new Event('reconnect'));
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().simulateDrop();

    expect(disconnected).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('does not dispatch disconnected on a deliberate disconnect', () => {
    const client = connectAndOpen();
    const disconnected = vi.fn();
    client.addEventListener('disconnected', disconnected);

    client.disconnect();

    expect(disconnected).not.toHaveBeenCalled();
  });
});

describe('SSEClient jitter', () => {
  it('spreads retries so open tabs do not reconnect in lockstep', () => {
    vi.spyOn(Math, 'random').mockReturnValue(1);
    const client = connectAndOpen();
    latest().simulateDrop();

    // 1s base plus the full +10% of proportional jitter.
    vi.advanceTimersByTime(1_099);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });
});

describe('SSEClient single connection', () => {
  it('does not queue a second retry while one is pending', () => {
    const client = connectAndOpen();
    latest().simulateDrop();
    // A second failure on the same dead connection must not stack a timer.
    latest().simulateDrop();

    vi.advanceTimersByTime(1_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('never holds two live connections when a probe and a wake-up race', async () => {
    let settle: (v: unknown) => void = () => {};
    vi.stubGlobal(
      'fetch',
      vi.fn().mockReturnValue(new Promise((res) => (settle = res)))
    );

    // Never opened, so the failure is classified as a possibly-rejected
    // handshake and starts the auth probe. Nothing is queued while the fetch
    // is in flight, which is the window the race lives in.
    const client = new SSEClient();
    client.connect(['agent.>']);
    latest().simulateHandshakeFailure();
    expect(FakeEventSource.instances).toHaveLength(1);

    // The tab wakes mid-probe and reconnects.
    setVisibility('hidden');
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(2);
    const live = latest();

    // The probe now resolves. It must not open a third connection alongside
    // the one the wake-up just made.
    settle({ status: 200, redirected: false });
    await vi.advanceTimersByTimeAsync(300_000);

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(latest()).toBe(live);
    expect(live.closed).toBe(false);

    client.disconnect();
    expect(live.closed).toBe(true);
  });
});

describe('SSEClient connected event', () => {
  it('dispatches connected on every successful open, including reconnects', () => {
    const client = new SSEClient();
    const connected = vi.fn();
    client.addEventListener('connected', connected);

    client.connect(['agent.>']);
    latest().simulateOpen();
    expect(connected).toHaveBeenCalledTimes(1);

    // The drop and recovery cycle must announce the recovery: downstream
    // catch-up (refetching what the dead stream missed) keys off it.
    latest().simulateDrop();
    vi.advanceTimersByTime(1_000);
    latest().simulateOpen();
    expect(connected).toHaveBeenCalledTimes(2);
    client.disconnect();
  });
});
