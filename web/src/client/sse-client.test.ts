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

/** A pageshow event; persisted marks a restore from the back/forward cache. */
function pageShow(persisted: boolean): Event {
  const event = new Event('pageshow');
  Object.defineProperty(event, 'persisted', { value: persisted });
  return event;
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

    // The hub asks the client to reconnect before a clean shutdown; the
    // reopen waits the first reconnect's jitter (2 s x 0.5 here).
    latest().dispatchEvent(new Event('reconnect'));
    vi.advanceTimersByTime(1_000);
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
  it('spreads later retries so open tabs do not reconnect in lockstep', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(1);
    const client = connectAndOpen();
    latest().simulateDrop();
    // The first retry is full jitter: 2 s x 1.
    vi.advanceTimersByTime(2_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    await failHandshake();

    // The second keeps the proportional jitter: 2 s base plus the full +10%.
    vi.advanceTimersByTime(2_199);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);
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
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise((res) => (settle = res))));

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

  it('supplies a non-null detail matching SSEClientEventMap on the connected event', () => {
    const client = new SSEClient();
    const connected = vi.fn();
    client.addEventListener('connected', connected);

    client.connect(['agent.>', 'project.123']);
    latest().simulateOpen();

    expect(connected).toHaveBeenCalledTimes(1);
    const event = connected.mock.calls[0]?.[0] as CustomEvent;
    expect(event.detail).not.toBeNull();
    expect(event.detail).toEqual({
      connectionId: '',
      subjects: ['agent.>', 'project.123'],
    });
    client.disconnect();
  });
});

it('uses an explicit trusted deployment endpoint without changing the default', () => {
  const scoped = new SSEClient('https://hub.example/team/events');
  scoped.connect(['agent.one.>']);
  expect(latest().url).toBe('https://hub.example/team/events?sub=agent.one.%3E');
  scoped.disconnect();
  const normal = new SSEClient();
  normal.connect(['project.one.>']);
  expect(latest().url).toBe('/events?sub=project.one.%3E');
  normal.disconnect();
});

it('reports a server reconnect gap before the replacement is ready', () => {
  const client = connectAndOpen();
  const disconnected = vi.fn();
  const connected = vi.fn();
  client.addEventListener('disconnected', disconnected);
  client.addEventListener('connected', connected);
  latest().dispatchEvent(new Event('reconnect'));
  expect(disconnected).toHaveBeenCalledTimes(1);
  expect(connected).not.toHaveBeenCalled();
  vi.advanceTimersByTime(1_000);
  latest().simulateOpen();
  expect(connected).toHaveBeenCalledTimes(1);
  client.disconnect();
});

it('reports failed handshakes separately and scopes the auth probe to its Hub base', async () => {
  const client = new SSEClient('https://hub.example/team/events');
  const failed = vi.fn();
  const connected = vi.fn();
  client.addEventListener('handshake-failed', failed);
  client.addEventListener('connected', connected);
  client.connect(['agent.one.>']);
  latest().simulateHandshakeFailure();
  await vi.advanceTimersByTimeAsync(0);
  expect(failed).toHaveBeenCalledTimes(1);
  expect(connected).not.toHaveBeenCalled();
  expect(fetch).toHaveBeenCalledWith('https://hub.example/team/auth/me', {
    credentials: 'include',
  });
  client.disconnect();
});

describe('SSEClient stale connection detection', () => {
  /** Deliver a named heartbeat event on the current connection. */
  function heartbeat(): void {
    latest().dispatchEvent(new MessageEvent('heartbeat', { data: String(Date.now()) }));
  }

  /** Deliver an update event on the current connection. */
  function update(): void {
    latest().dispatchEvent(
      new MessageEvent('update', { data: JSON.stringify({ subject: 'x', data: {} }) })
    );
  }

  it('replaces an open but silent connection when the tab resumes', () => {
    const client = connectAndOpen();
    const drops = vi.fn();
    const opens = vi.fn();
    client.addEventListener('disconnected', drops);
    client.addEventListener('connected', opens);
    setVisibility('hidden');
    vi.advanceTimersByTime(120_000);

    setVisibility('visible');

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0]!.closed).toBe(true);
    // Reported as a drop, so the reopen drives the usual resync.
    expect(drops).toHaveBeenCalledTimes(1);
    latest().simulateOpen();
    expect(opens).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('keeps a connection that delivered traffic while hidden', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(100_000);
    update();
    vi.advanceTimersByTime(20_000);

    setVisibility('visible');

    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('keeps a connection after a short absence', () => {
    const client = connectAndOpen();
    vi.advanceTimersByTime(100_000);
    setVisibility('hidden');
    vi.advanceTimersByTime(10_000);

    setVisibility('visible');

    // Silent, but idle streams are normal without heartbeat events and the
    // tab was away too briefly to have been suspended.
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('checks on pageshow and online as well', () => {
    const client = connectAndOpen();
    window.dispatchEvent(new Event('offline'));
    vi.advanceTimersByTime(80_000);
    window.dispatchEvent(new Event('online'));
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().simulateOpen();

    window.dispatchEvent(new Event('pagehide'));
    vi.advanceTimersByTime(80_000);
    window.dispatchEvent(pageShow(true));
    expect(FakeEventSource.instances).toHaveLength(3);
    client.disconnect();
  });

  it('holds no timer for an open, fresh connection', () => {
    // Callers' tests count pending timers; an idle client must leave none.
    const client = connectAndOpen();
    expect(vi.getTimerCount()).toBe(0);
    heartbeat();
    expect(vi.getTimerCount()).toBe(1);
    client.disconnect();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('does not reconnect idle visible streams without heartbeat events', () => {
    const client = connectAndOpen();
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('reconnects a visible stream once heartbeat events stop', () => {
    const client = connectAndOpen();
    for (let i = 0; i < 10; i++) {
      vi.advanceTimersByTime(30_000);
      heartbeat();
    }
    expect(FakeEventSource.instances).toHaveLength(1);

    // Heartbeats stop; the next check past the window replaces the stream.
    vi.advanceTimersByTime(75_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(15_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('keeps one reconnect in flight', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(120_000);

    setVisibility('visible');
    window.dispatchEvent(pageShow(true));
    window.dispatchEvent(new Event('online'));
    setVisibility('visible');
    vi.advanceTimersByTime(60_000);

    // The replacement is still connecting; nothing else may replace it.
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(latest().readyState).toBe(FakeEventSource.CONNECTING);
    client.disconnect();
  });

  it('a new connection must earn heartbeat trust again', () => {
    const client = connectAndOpen();
    heartbeat();
    vi.advanceTimersByTime(90_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().simulateOpen();
    // Opening the replacement disarmed the old connection's interval.
    expect(vi.getTimerCount()).toBe(0);

    // The replacement has sent no heartbeat event yet, so silence while
    // visible does not condemn it: no reconnect loop.
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('re-checks a resume that came too soon to judge', () => {
    // A 40s trip: suspended, but not yet silent long enough to condemn.
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(40_000);
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(1);

    // Still silent once 75s have passed since the last traffic: replaced.
    vi.advanceTimersByTime(35_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('times the re-check from the last traffic, even traffic while hidden', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(100_000);
    update();
    vi.advanceTimersByTime(20_000);
    setVisibility('visible');

    vi.advanceTimersByTime(55_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('cancels the re-check on new traffic', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(40_000);
    setVisibility('visible');
    vi.advanceTimersByTime(20_000);
    update();

    // No re-check remains; without heartbeat events no interval is armed.
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('cancels the re-check when the tab is hidden again', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(40_000);
    setVisibility('visible');
    vi.advanceTimersByTime(10_000);
    setVisibility('hidden');

    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('cancels the re-check on disconnect', () => {
    const client = connectAndOpen();
    setVisibility('hidden');
    vi.advanceTimersByTime(40_000);
    setVisibility('visible');
    client.disconnect();

    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('does not reconnect from the interval while hidden, even with heartbeats', () => {
    const client = connectAndOpen();
    heartbeat();
    setVisibility('hidden');
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);

    // Returning is what judges it.
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('never reconnects a healthy feed after a quick app switch', () => {
    const client = connectAndOpen();
    vi.advanceTimersByTime(74_000);
    setVisibility('hidden');
    vi.advanceTimersByTime(1_000);
    setVisibility('visible');

    // Too short to have been suspended: no re-check is armed at all.
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('replaces an already silent stream after a trip long enough to suspend', () => {
    const client = connectAndOpen();
    const drops = vi.fn();
    client.addEventListener('disconnected', drops);
    vi.advanceTimersByTime(100_000);
    setVisibility('hidden');
    vi.advanceTimersByTime(20_000);
    setVisibility('visible');

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(drops).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('ignores the pageshow of an ordinary page load', () => {
    const client = connectAndOpen();
    const drops = vi.fn();
    client.addEventListener('disconnected', drops);
    // Even with the stream closed, a non-persisted pageshow is not a resume.
    latest().readyState = FakeEventSource.CLOSED;
    vi.advanceTimersByTime(100_000);

    window.dispatchEvent(pageShow(false));

    expect(drops).not.toHaveBeenCalled();
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('treats an absence of exactly the minimum as long enough', () => {
    const client = connectAndOpen();
    vi.advanceTimersByTime(100_000);
    setVisibility('hidden');
    vi.advanceTimersByTime(15_000);
    setVisibility('visible');

    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('reports a stream closed silently during a bfcache stay', () => {
    const client = connectAndOpen();
    const drops = vi.fn();
    const opens = vi.fn();
    client.addEventListener('disconnected', drops);
    client.addEventListener('connected', opens);
    window.dispatchEvent(new Event('pagehide'));
    // The browser closes the stream without firing an error event.
    latest().readyState = FakeEventSource.CLOSED;

    window.dispatchEvent(pageShow(true));

    expect(drops).toHaveBeenCalledTimes(1);
    expect(FakeEventSource.instances).toHaveLength(2);

    // A late error from the superseded stream must not report the drop again
    // or open another connection.
    FakeEventSource.instances[0]!.onerror?.(new Event('error'));
    expect(drops).toHaveBeenCalledTimes(1);
    expect(FakeEventSource.instances).toHaveLength(2);

    latest().simulateOpen();
    expect(opens).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('stops checking after disconnect', () => {
    const client = connectAndOpen();
    heartbeat();
    client.disconnect();
    vi.advanceTimersByTime(600_000);
    setVisibility('visible');
    window.dispatchEvent(new Event('online'));
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});

describe('SSEClient hidden-tab close (ptone/scion#5235)', () => {
  /** A client with the hidden-tab close on, connected and live. */
  function hiddenCloseClient(enabled = true): SSEClient {
    const client = new SSEClient('/events', { hiddenCloseEnabled: (): boolean => enabled });
    client.connect(['agent.>']);
    latest().simulateOpen();
    return client;
  }

  it('closes the stream after 60 s hidden, reports the drop and does not reconnect', () => {
    const client = hiddenCloseClient();
    const disconnected = vi.fn();
    client.addEventListener('disconnected', disconnected);

    setVisibility('hidden');
    vi.advanceTimersByTime(59_999);
    expect(latest().closed).toBe(false);
    vi.advanceTimersByTime(1);

    expect(latest().closed).toBe(true);
    expect(client.connected).toBe(false);
    expect(disconnected).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(30 * 60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('keeps the stream when the tab is shown again before 60 s', () => {
    const client = hiddenCloseClient();
    setVisibility('hidden');
    vi.advanceTimersByTime(59_000);
    setVisibility('visible');

    // Past the 60 s mark, and short of the resume staleness check, which
    // would replace a connection silent for 75 s.
    vi.advanceTimersByTime(10_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(latest().closed).toBe(false);
    client.disconnect();
  });

  it('does not restart the countdown when hidden twice', () => {
    const client = hiddenCloseClient();
    setVisibility('hidden');
    vi.advanceTimersByTime(30_000);
    document.dispatchEvent(new Event('visibilitychange'));
    vi.advanceTimersByTime(30_000);
    expect(latest().closed).toBe(true);
    client.disconnect();
  });

  it('reconnects at once when shown after the close, and reports the recovery', () => {
    const client = hiddenCloseClient();
    const connected = vi.fn();
    client.addEventListener('connected', connected);
    setVisibility('hidden');
    vi.advanceTimersByTime(60_000);

    setVisibility('visible');

    // No timer advanced: the reopen happened on the transition.
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(client.reconnectAttemptCount).toBe(0);
    latest().simulateOpen();
    expect(connected).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('cancels a retry pending when the countdown expires', () => {
    const client = hiddenCloseClient();
    setVisibility('hidden');
    vi.advanceTimersByTime(59_500);
    latest().simulateDrop();

    vi.advanceTimersByTime(500);
    vi.advanceTimersByTime(10 * 60_000);
    expect(FakeEventSource.instances).toHaveLength(1);

    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(2);
    client.disconnect();
  });

  it('cancels an auth probe in flight when the countdown expires', async () => {
    let settle: (v: unknown) => void = () => {};
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise((res) => (settle = res))));
    const client = new SSEClient('/events', { hiddenCloseEnabled: (): boolean => true });
    client.connect(['agent.>']);
    setVisibility('hidden');
    latest().simulateHandshakeFailure();

    vi.advanceTimersByTime(60_000);
    settle({ status: 200, redirected: false });
    await vi.advanceTimersByTimeAsync(10 * 60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('a bfcache restore after the close reopens, and later drops retry again', () => {
    const client = hiddenCloseClient();
    setVisibility('hidden');
    vi.advanceTimersByTime(60_000);

    // The page comes back from the back/forward cache with no
    // visibilitychange of its own.
    visibility = 'visible';
    window.dispatchEvent(pageShow(true));
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().simulateOpen();

    latest().simulateDrop();
    vi.advanceTimersByTime(2_000);
    expect(FakeEventSource.instances).toHaveLength(3);
    client.disconnect();
  });

  it('applies the countdown to a connection opened while hidden', () => {
    setVisibility('hidden');
    const client = new SSEClient('/events', { hiddenCloseEnabled: (): boolean => true });
    client.connect(['agent.>']);
    latest().simulateOpen();

    vi.advanceTimersByTime(60_000);
    expect(latest().closed).toBe(true);
    client.disconnect();
  });

  it('keeps the stream while hidden when the experiment is off', () => {
    const client = hiddenCloseClient(false);
    setVisibility('hidden');
    vi.advanceTimersByTime(10 * 60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(latest().closed).toBe(false);
    client.disconnect();
  });

  it('follows the web.sse_hidden_tab_close experiment by default', () => {
    window.__SCION_FEATURES__ = { 'web.sse_hidden_tab_close': true };
    try {
      const client = connectAndOpen();
      setVisibility('hidden');
      vi.advanceTimersByTime(60_000);
      expect(latest().closed).toBe(true);
      client.disconnect();
    } finally {
      delete window.__SCION_FEATURES__;
    }
  });

  it('a disconnect cancels the countdown, and a later connect starts clean', () => {
    const client = hiddenCloseClient();
    setVisibility('hidden');
    client.disconnect();
    vi.advanceTimersByTime(60_000);

    setVisibility('visible');
    client.connect(['agent.>']);
    latest().simulateOpen();
    vi.advanceTimersByTime(10 * 60_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(latest().closed).toBe(false);
    client.disconnect();
  });
});

describe('SSEClient first reconnect full jitter (ptone/scion#5236)', () => {
  /** A live client whose jitter draws come from `draws`, in order. */
  function jitterClient(draws: number[]): SSEClient {
    let i = 0;
    const client = new SSEClient('/events', {
      random: (): number => {
        const r = draws[i++];
        if (r === undefined) throw new Error('jitter draws exhausted');
        return r;
      },
      hiddenCloseEnabled: (): boolean => false,
    });
    client.connect(['agent.>']);
    latest().simulateOpen();
    return client;
  }

  /** Assert the next EventSource opens exactly `ms` from now, not a tick sooner. */
  function expectReopenAfter(ms: number): void {
    const before = FakeEventSource.instances.length;
    if (ms > 0) {
      vi.advanceTimersByTime(ms - 1);
      expect(FakeEventSource.instances).toHaveLength(before);
    }
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(before + 1);
  }

  it.each([
    [0, 0],
    [0.25, 500],
    [0.5, 1_000],
    [0.75, 1_500],
  ])('waits random(0, 2 s) after a server reconnect event (draw %s -> %s ms)', (draw, ms) => {
    const client = jitterClient([draw]);
    latest().dispatchEvent(new Event('reconnect'));
    expectReopenAfter(ms);
    client.disconnect();
  });

  it.each([
    [0, 0],
    [0.25, 500],
    [0.5, 1_000],
    [0.75, 1_500],
  ])('waits random(0, 2 s) after a mid-stream drop (draw %s -> %s ms)', (draw, ms) => {
    const client = jitterClient([draw]);
    latest().simulateDrop();
    expectReopenAfter(ms);
    client.disconnect();
  });

  it.each([
    [0, 0],
    [0.5, 1_000],
    [0.75, 1_500],
  ])('waits random(0, 2 s) after superseded with reason max_age (draw %s -> %s ms)', (draw, ms) => {
    const client = jitterClient([draw]);
    const disconnected = vi.fn();
    client.addEventListener('disconnected', disconnected);
    const old = latest();
    old.dispatchEvent(new MessageEvent('superseded', { data: '{"reason":"max_age"}' }));
    expect(old.closed).toBe(true);
    expect(disconnected).toHaveBeenCalledTimes(1);
    expectReopenAfter(ms);
    client.disconnect();
  });

  it('leaves a superseded stream with another reason, or unparsable data, alone', () => {
    const client = jitterClient([]);
    latest().dispatchEvent(new MessageEvent('superseded', { data: '{"reason":"tab"}' }));
    latest().dispatchEvent(new MessageEvent('superseded', { data: 'not json' }));
    expect(latest().closed).toBe(false);
    vi.advanceTimersByTime(5_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('keeps proportional jitter for the attempts after the first', async () => {
    // Draw 0.5 feeds the full jitter, then 0 and 1 are the -10 % and +10 %
    // ends of the proportional jitter on the 2 s and 4 s backoffs.
    const client = jitterClient([0.5, 0, 1]);
    latest().simulateDrop();
    expectReopenAfter(1_000);
    await failHandshake();
    expectReopenAfter(1_800);
    await failHandshake();
    expectReopenAfter(4_400);
    client.disconnect();
  });

  it('keeps the 1 s proportional backoff for a first connect that never opened', async () => {
    const client = new SSEClient('/events', {
      random: (): number => 0,
      hiddenCloseEnabled: (): boolean => false,
    });
    client.connect(['agent.>']);
    await failHandshake();
    expectReopenAfter(900);
    client.disconnect();
  });

  it('uses Math.random by default', () => {
    for (const [draw, ms] of [
      [0.25, 500],
      [0.75, 1_500],
    ]) {
      vi.spyOn(Math, 'random').mockReturnValue(draw);
      const client = connectAndOpen();
      latest().simulateDrop();
      expectReopenAfter(ms);
      client.disconnect();
    }
  });

  it('spreads the reconnects of many clients after a server close over the jitter window', () => {
    // 50 clients of one instance with a seeded, uniform RNG: the reconnects
    // land across [0, 2 s), not in one tick.
    let seed = 42;
    const rng = (): number => {
      seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
      return seed / 2 ** 31;
    };
    const clients: SSEClient[] = [];
    const opened: number[] = [];
    for (let n = 0; n < 50; n++) {
      const client = new SSEClient('/events', {
        random: rng,
        hiddenCloseEnabled: (): boolean => false,
      });
      client.connect([`agent.a${n}.>`]);
      latest().simulateOpen();
      clients.push(client);
    }
    const start = Date.now();
    const live = [...FakeEventSource.instances];
    for (const es of live) es.dispatchEvent(new Event('reconnect'));
    const seen = FakeEventSource.instances.length;
    for (let ms = 0; ms < 2_000; ms++) {
      vi.advanceTimersByTime(1);
      for (let k = seen + opened.length; k < FakeEventSource.instances.length; k++) {
        opened.push(Date.now() - start);
      }
    }

    expect(opened).toHaveLength(50);
    expect(Math.min(...opened)).toBeLessThan(400);
    expect(Math.max(...opened)).toBeGreaterThan(1_600);
    // No 100 ms bucket holds more than 20 % of the clients.
    const buckets = new Map<number, number>();
    for (const t of opened)
      buckets.set(Math.floor(t / 100), (buckets.get(Math.floor(t / 100)) ?? 0) + 1);
    expect(Math.max(...buckets.values())).toBeLessThanOrEqual(10);
    for (const client of clients) client.disconnect();
  });
});
