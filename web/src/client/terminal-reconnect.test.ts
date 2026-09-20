/**
 * P3.2 (#1659): Explicit reconnect and agent-unavailable transitions.
 *
 * Tests disconnect detection, reconnect deduplication, error classification,
 * SSE-driven unavailability, warm navigation guards, and buffer reset logic.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  TerminalSessionRegistry,
  type TerminalResourceInitializer,
  type TerminalResources,
} from './terminal-sessions.js';

const agentId = '11111111-1111-4111-8111-111111111111';
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
  const resources = {
    write: vi.fn<(bytes: Uint8Array) => void>(),
    size: () => ({ cols: 80, rows: 24 }),
    dispose: vi.fn(),
    reset: vi.fn(),
  } satisfies TerminalResources;
  const initialize = vi.fn<TerminalResourceInitializer>(() => Promise.resolve(resources));
  const registry = new TerminalSessionRegistry(scope);
  return { fetcher, resources, initialize, registry };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('disconnect detection and state (#1659 AC1)', () => {
  it('unexpected socket close sets disconnected with network reason', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    expect(session.state.connection).toBe('connected');
    expect(session.state.disconnectReason).toBeNull();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('network');
    expect(session.state.error).toContain('1006');
  });

  it('clean socket close (code 1000) does not set a disconnect reason', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1000 });

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBeNull();
    expect(session.state.error).toBeNull();
  });

  it('WebSocket error sets connect-error reason', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].onerror?.();

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe('connect-error');
  });

  it('retains resources and session entry after unexpected disconnect', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    expect(f.resources.dispose).not.toHaveBeenCalled();
    expect(f.registry.list()).toHaveLength(1);
    expect(session.state.connection).toBe('disconnected');
  });

  it('sendData returns false and never queues input when disconnected', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    expect(session.sendData('should-not-queue')).toBe(false);

    // Reconnect and verify input was not replayed
    await session.connect();
    FakeSocket.instances[1].open();
    expect(FakeSocket.instances[1].send).not.toHaveBeenCalled();
  });
});

describe('reconnect deduplication (#1659 AC2)', () => {
  it('rapid connect calls share one attempt', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    const p1 = session.connect();
    const p2 = session.connect();
    const p3 = session.connect();

    expect(p1).toBe(p2);
    expect(p2).toBe(p3);

    await p1;
    // Only one reconnect socket created
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it('generation invalidation prevents stale callback corruption', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const oldSocket = FakeSocket.instances[0];
    oldSocket.open();

    oldSocket.readyState = 3;
    oldSocket.onclose?.({ code: 1006 });

    await session.connect();
    const newSocket = FakeSocket.instances[1];
    newSocket.open();

    // Old socket events after reconnect must not corrupt state
    oldSocket.onmessage?.({
      data: JSON.stringify({ type: 'data', data: btoa('stale') }),
    });
    oldSocket.onclose?.({ code: 1006 });
    oldSocket.onerror?.();

    expect(session.state.connection).toBe('connected');
    expect(f.resources.write).not.toHaveBeenCalled();
  });

  it('increments generation on each reconnect attempt', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const states: number[] = [];
    session.subscribe((s) => states.push(s.generation));

    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    await session.connect();
    FakeSocket.instances[1].open();

    // Generation should have incremented
    const uniqueGenerations = [...new Set(states)];
    expect(uniqueGenerations.length).toBeGreaterThanOrEqual(2);
  });
});

describe('error classification (#1659 AC3)', () => {
  it.each([
    [401, 'auth-401', 'Authentication'],
    [403, 'auth-403', 'permission'],
    [404, 'not-found', 'not found'],
    [503, 'server-error', 'denied'],
  ] as const)(
    'preflight %s → disconnectReason=%s with no retry loop',
    async (status, expectedReason, errorFragment) => {
      const f = fixture();
      f.fetcher
        .mockResolvedValueOnce(json(agent))
        .mockResolvedValueOnce(json({ error: { message: 'denied' } }, status));
      const session = f.registry.open(agentId, f.initialize);
      await session.connect();

      expect(session.state.connection).toBe('disconnected');
      expect(session.state.disconnectReason).toBe(expectedReason);
      expect(session.state.error?.toLowerCase()).toContain(errorFragment.toLowerCase());
      expect(FakeSocket.instances).toHaveLength(0);
    }
  );

  it.each([
    [401, 'auth-401'],
    [404, 'not-found'],
    [500, 'server-error'],
  ] as const)('agent metadata fetch %s → disconnectReason=%s', async (status, expectedReason) => {
    const f = fixture();
    f.fetcher.mockResolvedValueOnce(json({}, status));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();

    expect(session.state.connection).toBe('disconnected');
    expect(session.state.disconnectReason).toBe(expectedReason);
  });

  it('offline agent → unavailable with agent-offline reason', async () => {
    const f = fixture();
    f.fetcher.mockResolvedValueOnce(json({ ...agent, activity: 'offline' }));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();

    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-offline');
  });

  it('non-running phase → unavailable with agent-phase reason', async () => {
    const f = fixture();
    f.fetcher.mockResolvedValueOnce(json({ ...agent, phase: 'created' }));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();

    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-phase');
  });
});

describe('SSE agent-stopped/deleted → unavailable (#1659 AC4)', () => {
  it('markUnavailable(agent-stopped) sets unavailable state', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    expect(session.state.connection).toBe('connected');

    session.markUnavailable('agent-stopped', 'Agent has stopped.');

    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-stopped');
    expect(session.state.error).toBe('Agent has stopped.');
  });

  it('markUnavailable(agent-deleted) sets unavailable with deleted reason', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    session.markUnavailable('agent-deleted', 'Agent was deleted.');

    expect(session.state.connection).toBe('unavailable');
    expect(session.state.disconnectReason).toBe('agent-deleted');
    expect(session.state.error).toBe('Agent was deleted.');
    // Socket should have been closed
    expect(FakeSocket.instances[0].close).toHaveBeenCalled();
  });

  it('markUnavailable does not affect a closed session', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    session.close();

    session.markUnavailable('agent-deleted', 'Should not apply.');
    expect(session.state.connection).toBe('closed');
  });

  it('markUnavailable retains session entry and resources', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    session.markUnavailable('agent-stopped', 'Agent has stopped.');

    expect(f.resources.dispose).not.toHaveBeenCalled();
    expect(f.registry.list()).toHaveLength(1);
  });
});

describe('warm navigation guard (#1659 AC5)', () => {
  it('connect() on connected session does not trigger reconnect', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();
    expect(session.state.connection).toBe('connected');
    const initialGeneration = session.state.generation;

    // Simulating warm select by calling connect() on already-connected session
    const result = session.connect();
    expect(result).toEqual(Promise.resolve());
    await result;

    // No new socket, no generation change, no reset
    expect(FakeSocket.instances).toHaveLength(1);
    expect(session.state.generation).toBe(initialGeneration);
    expect(f.resources.reset).not.toHaveBeenCalled();
  });

  it('connect() while connecting shares existing attempt', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const attempt = session.connect();
    const duplicate = session.connect();
    expect(duplicate).toBe(attempt);
    await attempt;
  });
});

describe('buffer reset on reconnect (#1659 AC6)', () => {
  it('successful reconnect resets buffer', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });
    expect(f.resources.reset).not.toHaveBeenCalled();

    await session.connect();
    FakeSocket.instances[1].open();
    expect(f.resources.reset).toHaveBeenCalledTimes(1);
  });

  it('failed reconnect does not reset buffer', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    f.fetcher.mockResolvedValueOnce(json(agent)).mockResolvedValueOnce(json({}, 403));
    await session.connect();

    expect(f.resources.reset).not.toHaveBeenCalled();
    expect(f.resources.dispose).not.toHaveBeenCalled();
  });

  it('failed reconnect followed by successful reconnect resets buffer once', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    // First reconnect fails
    f.fetcher.mockResolvedValueOnce(json(agent)).mockResolvedValueOnce(json({}, 403));
    await session.connect();
    expect(f.resources.reset).not.toHaveBeenCalled();

    // Second reconnect succeeds
    f.fetcher.mockResolvedValue(json(agent));
    await session.connect();
    FakeSocket.instances[1].open();
    expect(f.resources.reset).toHaveBeenCalledTimes(1);
  });
});

describe('reconnecting getter (#1659)', () => {
  it('reconnecting is true during pending connect, false after', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);

    // registry.open() auto-calls connect(), so reconnecting starts true
    expect(session.reconnecting).toBe(true);
    const attempt = session.connect(); // shares the existing attempt
    expect(session.reconnecting).toBe(true);
    await attempt;
    // pending cleared after attempt completes
    expect(session.reconnecting).toBe(false);
  });

  it('reconnecting reflects ongoing attempt even after socket opens', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });

    const attempt = session.connect();
    expect(session.reconnecting).toBe(true);
    await attempt;
    expect(session.reconnecting).toBe(false);
  });
});

describe('disconnect reason cleared on reconnect (#1659)', () => {
  it('connect() clears previous disconnect reason', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    FakeSocket.instances[0].open();

    FakeSocket.instances[0].readyState = 3;
    FakeSocket.instances[0].onclose?.({ code: 1006 });
    expect(session.state.disconnectReason).toBe('network');

    const attempt = session.connect();
    // After connect() is called, the reason should be cleared
    expect(session.state.disconnectReason).toBeNull();
    await attempt;
  });
});
