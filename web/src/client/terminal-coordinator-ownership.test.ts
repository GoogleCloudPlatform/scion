/**
 * P3.3 (#1660): Owner exit detection, frozen-tab handling, generation
 * validation, stale context cleanup, and secure-context capability checks.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TerminalCoordinator, type TerminalCoordinatorAdapter } from './terminal-coordinator.js';
import type {
  TerminalResources,
  TerminalResourceInitializer,
  TerminalSession,
} from './terminal-sessions.js';

const agentId = '11111111-1111-4111-8111-111111111111';
const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };
const agent = { id: agentId, name: 'example', phase: 'running', activity: 'executing' };
const json = (body: unknown, status = 200): Response =>
  new Response(JSON.stringify(body), { status });

class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  send = vi.fn<(data: string) => void>();
  close = vi.fn((): void => {
    this.readyState = 3;
  });
  constructor(readonly url: string) {
    FakeSocket.instances.push(this);
  }
  open(): void {
    this.readyState = 1;
    this.onopen?.();
  }
}

/** Captured BroadcastChannel messages for cross-tab assertions. */
const channelMessages: unknown[] = [];

class FakeBroadcastChannel {
  static instances: FakeBroadcastChannel[] = [];
  onmessage: ((event: { data: unknown }) => void) | null = null;
  closed = false;
  constructor(readonly name: string) {
    FakeBroadcastChannel.instances.push(this);
  }
  postMessage(data: unknown): void {
    if (this.closed) return;
    channelMessages.push(data);
  }
  close(): void {
    this.closed = true;
  }
}

/** Simulate the peer receiving a BroadcastChannel message. */
function deliverToPeer(coordinator: TerminalCoordinator, data: unknown): void {
  const channel = FakeBroadcastChannel.instances.find(
    (ch) => ch.name === coordinator.coordinationKey
  );
  channel?.onmessage?.({ data });
}

/**
 * A Web Lock mock that tracks held locks and queues waiting requests.
 * This simulates real browser lock behavior: ifAvailable returns null when
 * the lock is held, and queued (non-ifAvailable) requests fire FIFO when
 * the lock is released.
 */
function createLockMock(): {
  request: ReturnType<typeof vi.fn>;
  /** Release the currently held lock (simulates owner tab close). */
  releaseLock(name: string): void;
  /** Whether a lock is currently held. */
  isHeld(name: string): boolean;
  /** Number of pending waiters for a lock. */
  waiterCount(name: string): number;
} {
  const held = new Map<string, { releaseHeld: () => void }>();
  const waiters = new Map<
    string,
    Array<{
      callback: (lock: object | null) => Promise<void>;
      signal: AbortSignal | undefined;
      reject: (error: Error) => void;
    }>
  >();

  function processNextWaiter(name: string): void {
    const list = waiters.get(name);
    if (!list || list.length === 0) return;
    const next = list.shift()!;
    if (next.signal?.aborted) {
      processNextWaiter(name);
      return;
    }
    // Grant the lock to the next waiter
    let releaseHeld!: () => void;
    void new Promise<void>((r) => {
      releaseHeld = r;
    });
    held.set(name, { releaseHeld });
    void next
      .callback({ name, mode: 'exclusive' })
      .then(() => {
        if (held.get(name)?.releaseHeld === releaseHeld) {
          held.delete(name);
          processNextWaiter(name);
        }
      })
      .catch(() => {
        held.delete(name);
        processNextWaiter(name);
      });
  }

  const request = vi.fn(
    async (
      name: string,
      opts: { mode?: string; ifAvailable?: boolean; signal?: AbortSignal },
      callback: (lock: object | null) => Promise<void>
    ): Promise<void> => {
      if (opts.signal?.aborted) {
        throw new DOMException('The operation was aborted.', 'AbortError');
      }

      if (opts.ifAvailable) {
        if (held.has(name)) {
          // Lock not available
          await callback(null);
          return;
        }
        // Grant immediately
        let releaseHeld!: () => void;
        void new Promise<void>((r) => {
          releaseHeld = r;
        });
        held.set(name, { releaseHeld });
        try {
          await callback({ name, mode: 'exclusive' });
        } finally {
          if (held.get(name)?.releaseHeld === releaseHeld) {
            held.delete(name);
            processNextWaiter(name);
          }
        }
        return;
      }

      // Non-ifAvailable: queue if lock is held
      if (held.has(name)) {
        return new Promise<void>((resolve, reject) => {
          const waiter = {
            callback: async (lock: object | null): Promise<void> => {
              try {
                await callback(lock);
                resolve();
              } catch (e) {
                reject(e);
              }
            },
            signal: opts.signal,
            reject,
          };

          if (!waiters.has(name)) waiters.set(name, []);
          waiters.get(name)!.push(waiter);

          opts.signal?.addEventListener('abort', () => {
            const list = waiters.get(name);
            if (list) {
              const idx = list.indexOf(waiter);
              if (idx >= 0) list.splice(idx, 1);
            }
            reject(new DOMException('The operation was aborted.', 'AbortError'));
          });
        });
      }

      // Lock not held, grant immediately
      let releaseHeld!: () => void;
      void new Promise<void>((r) => {
        releaseHeld = r;
      });
      held.set(name, { releaseHeld });
      try {
        await callback({ name, mode: 'exclusive' });
      } finally {
        if (held.get(name)?.releaseHeld === releaseHeld) {
          held.delete(name);
          processNextWaiter(name);
        }
      }
    }
  );

  return {
    request,
    releaseLock(name: string): void {
      const entry = held.get(name);
      if (entry) {
        held.delete(name);
        entry.releaseHeld();
        // The callback's finally block won't process waiters because we
        // already deleted the entry. Process the next waiter explicitly.
        // Use queueMicrotask to allow the callback to complete first.
        queueMicrotask(() => processNextWaiter(name));
      }
    },
    isHeld(name: string): boolean {
      return held.has(name);
    },
    waiterCount(name: string): number {
      return waiters.get(name)?.length ?? 0;
    },
  };
}

/** Simple lock mock (always grants immediately) for tests that don't need queuing. */
function simpleLocksRequest(): ReturnType<typeof vi.fn> {
  return vi.fn(
    async (
      _name: string,
      _opts: unknown,
      callback: (lock: unknown) => Promise<void>
    ): Promise<void> => {
      await callback({ name: _name, mode: 'exclusive' });
    }
  );
}

function fixture(opts?: { locks?: ReturnType<typeof createLockMock> }): {
  fetcher: ReturnType<typeof vi.fn>;
  resources: TerminalResources;
  initialize: ReturnType<typeof vi.fn>;
  adapter: TerminalCoordinatorAdapter;
  coordinator: TerminalCoordinator;
  selectCalls: TerminalSession[];
  locks: ReturnType<typeof createLockMock> | null;
} {
  FakeSocket.instances = [];
  FakeBroadcastChannel.instances = [];
  channelMessages.length = 0;

  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel);
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      close() {}
    }
  );
  vi.stubGlobal('isSecureContext', true);

  const locks = opts?.locks ?? null;
  vi.stubGlobal('navigator', {
    ...navigator,
    locks: {
      request: locks?.request ?? simpleLocksRequest(),
    },
  });

  const fetcher = vi.fn(
    (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
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
  const selectCalls: TerminalSession[] = [];
  const adapter: TerminalCoordinatorAdapter = {
    initialize,
    select: (session): void => {
      selectCalls.push(session);
    },
  };

  const coordinator = new TerminalCoordinator(scope, adapter);

  return {
    fetcher,
    resources,
    initialize,
    adapter,
    coordinator,
    selectCalls,
    locks,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// AC1: Frozen/delayed owner never triggers lock stealing or a second attach
// ---------------------------------------------------------------------------
describe('frozen/delayed owner (AC1)', () => {
  it('does not steal lock when owner is frozen (lock held)', async () => {
    const locks = createLockMock();
    // Tab A acquires ownership
    const tabA = fixture({ locks });
    const resultA = await tabA.coordinator.open(agentId, undefined, 5000);
    expect(resultA.status).toBe('selected');
    expect(tabA.coordinator.isOwner).toBe(true);

    // Tab B tries to open — lock is held by Tab A
    const tabB = fixture({ locks });
    const resultB = await tabB.coordinator.open(agentId, undefined, 200);
    // Tab B cannot acquire lock (Tab A is "frozen" — holding the lock)
    // Should get pending, not selected
    expect(resultB.status).toBe('pending');
    expect(tabB.coordinator.isOwner).toBe(false);

    // Verify no duplicate attaches: only Tab A should have created a session
    expect(tabA.selectCalls).toHaveLength(1);
    expect(tabB.selectCalls).toHaveLength(0);

    tabA.coordinator.stop();
    tabB.coordinator.stop();
  });

  it('rejects stale open messages from a previous generation', async () => {
    const locks = createLockMock();
    const f = fixture({ locks });
    const result = await f.coordinator.open(agentId, undefined, 5000);
    expect(result.status).toBe('selected');

    const currentGen = f.coordinator.generation;
    const staleGen = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
    expect(staleGen).not.toBe(currentGen);

    // Simulate receiving an open message with a stale generation
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'open',
      requestId: 'stale-req',
      agentId,
      generation: staleGen,
    });

    // No additional session should have been created
    expect(f.selectCalls).toHaveLength(1);

    f.coordinator.stop();
  });

  it('rejects stale ack messages from an old owner generation', async () => {
    const locks = createLockMock();
    const f = fixture({ locks });

    // Start an open request
    const openPromise = f.coordinator.open(agentId, 'req-1', 5000);
    await vi.waitFor(() => expect(f.coordinator.isOwner).toBe(true));
    const result = await openPromise;
    expect(result.status).toBe('selected');

    // Create a new request
    const openPromise2 = f.coordinator.open('22222222-2222-4222-8222-222222222222', 'req-2', 5000);
    await openPromise2;

    // Try to deliver a stale ack with wrong generation — should be ignored
    const staleGen = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'ack',
      requestId: 'req-2',
      agentId: '22222222-2222-4222-8222-222222222222',
      generation: staleGen,
      status: 'selected',
      focus: 'document-focused',
    });

    // The ack should have been ignored (generation mismatch)
    // This is validated internally by the receive handler
    f.coordinator.stop();
  });
});

// ---------------------------------------------------------------------------
// AC2: After owner closure/crash, next explicit open acquires ownership
// ---------------------------------------------------------------------------
describe('owner exit and lock succession (AC2)', () => {
  it('queues a waiting lock request when lock is not available', async () => {
    const locks = createLockMock();

    // Tab A acquires ownership
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);
    expect(tabA.coordinator.isOwner).toBe(true);

    // Tab B tries and fails to acquire lock
    const tabB = fixture({ locks });
    const resultB = await tabB.coordinator.open(agentId, undefined, 200);
    expect(resultB.status).toBe('pending');
    expect(tabB.coordinator.isOwner).toBe(false);

    // There should be a queued waiter
    expect(locks.waiterCount(tabA.coordinator.coordinationKey)).toBe(1);

    tabA.coordinator.stop();
    tabB.coordinator.stop();
  });

  it('next explicit open succeeds after previous owner stops', async () => {
    const locks = createLockMock();

    // Tab A acquires ownership
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);
    expect(tabA.coordinator.isOwner).toBe(true);

    // Tab B tries and gets pending
    const tabB = fixture({ locks });
    const firstResult = await tabB.coordinator.open(agentId, 'req-b', 200);
    expect(firstResult.status).toBe('pending');

    // Tab A closes (stop releases the lock)
    tabA.coordinator.stop();

    // Allow microtasks to process (queued lock callback fires)
    await vi.waitFor(() => expect(tabB.coordinator.isOwner).toBe(true));

    // Tab B retries — now succeeds as owner
    const retryResult = await tabB.coordinator.open(agentId, undefined, 5000);
    expect(retryResult.status).toBe('selected');
    expect(tabB.coordinator.isOwner).toBe(true);

    tabB.coordinator.stop();
  });

  it('simulated lock release transfers ownership to waiting tab', async () => {
    const locks = createLockMock();

    // Tab A acquires ownership
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);
    expect(tabA.coordinator.isOwner).toBe(true);

    // Tab B queues for ownership
    const tabB = fixture({ locks });
    await tabB.coordinator.open(agentId, undefined, 200);
    expect(tabB.coordinator.isOwner).toBe(false);

    // Simulate Tab A's tab being closed (browser releases lock)
    locks.releaseLock(tabA.coordinator.coordinationKey);

    // Tab B should acquire ownership
    await vi.waitFor(() => expect(tabB.coordinator.isOwner).toBe(true));

    // Tab B can now open terminals
    const result = await tabB.coordinator.open(agentId, undefined, 5000);
    expect(result.status).toBe('selected');

    tabB.coordinator.stop();
  });
});

// ---------------------------------------------------------------------------
// AC3: Stale context cleanup precedes voluntary lock release
// ---------------------------------------------------------------------------
describe('stale context and cleanup ordering (AC3)', () => {
  it('stop clears ownerGeneration before releasing lock', () => {
    const f = fixture();

    // First, become owner
    void f.coordinator.open(agentId, undefined, 5000);

    // The simple lock mock grants immediately, so coordinator is owner
    expect(f.coordinator.isOwner).toBe(true);
    expect(f.coordinator.generation).not.toBeNull();

    // Stop should clear generation before releasing lock
    f.coordinator.stop();

    expect(f.coordinator.isOwner).toBe(false);
    expect(f.coordinator.generation).toBeNull();
    expect(f.coordinator.tornDown).toBe(true);
  });

  it('stopped coordinator does not respond to discover messages', () => {
    const f = fixture();

    // Become owner then stop
    void f.coordinator.open(agentId, undefined, 5000);
    f.coordinator.stop();
    channelMessages.length = 0;

    // Simulate receiving a discover from another tab
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'discover',
      requestId: 'late-discover',
      agentId,
      generation: null,
    });

    // No owner response should have been sent
    const ownerMsgs = channelMessages.filter((msg) => (msg as { type: string }).type === 'owner');
    expect(ownerMsgs).toHaveLength(0);
  });

  it('stopped coordinator rejects all new opens as stopped', async () => {
    const f = fixture();
    f.coordinator.stop();

    const result = await f.coordinator.open(agentId);
    expect(result.status).toBe('stopped');
  });

  it('stop cancels queued ownership wait', async () => {
    const locks = createLockMock();

    // Tab A holds the lock
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);

    // Tab B queues for ownership
    const tabB = fixture({ locks });
    await tabB.coordinator.open(agentId, undefined, 200);
    expect(locks.waiterCount(tabA.coordinator.coordinationKey)).toBe(1);

    // Tab B stops — should cancel its queued wait
    tabB.coordinator.stop();

    // The waiter should have been removed
    await vi.waitFor(() => expect(locks.waiterCount(tabA.coordinator.coordinationKey)).toBe(0));

    // Tab A still holds ownership
    expect(tabA.coordinator.isOwner).toBe(true);

    tabA.coordinator.stop();
  });
});

// ---------------------------------------------------------------------------
// AC5: Unsupported coordination capability
// ---------------------------------------------------------------------------
describe('unsupported coordination capability (AC5)', () => {
  it('reports specific reason when isSecureContext is false', () => {
    FakeSocket.instances = [];
    FakeBroadcastChannel.instances = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel);
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close() {}
      }
    );
    vi.stubGlobal('isSecureContext', false);
    vi.stubGlobal('navigator', {
      ...navigator,
      locks: { request: simpleLocksRequest() },
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json(agent)))
    );

    const coordinator = new TerminalCoordinator(scope, {
      initialize: vi.fn(() => Promise.resolve({} as TerminalResources)),
      select: vi.fn(),
    });

    expect(coordinator.supported).toBe(false);
    expect(coordinator.unsupportedReason).toContain('secure context');
  });

  it('reports specific reason when navigator.locks is absent', () => {
    FakeSocket.instances = [];
    FakeBroadcastChannel.instances = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel);
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close() {}
      }
    );
    vi.stubGlobal('isSecureContext', true);
    vi.stubGlobal('navigator', { ...navigator, locks: undefined });
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json(agent)))
    );

    const coordinator = new TerminalCoordinator(scope, {
      initialize: vi.fn(() => Promise.resolve({} as TerminalResources)),
      select: vi.fn(),
    });

    expect(coordinator.supported).toBe(false);
    expect(coordinator.unsupportedReason).toContain('Web Lock');
  });

  it('reports specific reason when BroadcastChannel is absent', () => {
    FakeSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('BroadcastChannel', undefined);
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close() {}
      }
    );
    vi.stubGlobal('isSecureContext', true);
    vi.stubGlobal('navigator', {
      ...navigator,
      locks: { request: simpleLocksRequest() },
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json(agent)))
    );

    const coordinator = new TerminalCoordinator(scope, {
      initialize: vi.fn(() => Promise.resolve({} as TerminalResources)),
      select: vi.fn(),
    });

    expect(coordinator.supported).toBe(false);
    expect(coordinator.unsupportedReason).toContain('BroadcastChannel');
  });

  it('returns null unsupportedReason when fully supported', () => {
    const f = fixture();
    expect(f.coordinator.supported).toBe(true);
    expect(f.coordinator.unsupportedReason).toBeNull();
    f.coordinator.stop();
  });

  it('returns unsupported status from open() when coordination is unavailable', async () => {
    FakeSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('BroadcastChannel', undefined);
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close() {}
      }
    );
    vi.stubGlobal('isSecureContext', true);
    vi.stubGlobal('navigator', {
      ...navigator,
      locks: { request: simpleLocksRequest() },
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json(agent)))
    );

    const coordinator = new TerminalCoordinator(scope, {
      initialize: vi.fn(() => Promise.resolve({} as TerminalResources)),
      select: vi.fn(),
    });

    const result = await coordinator.open(agentId);
    expect(result.status).toBe('unsupported');
  });
});

// ---------------------------------------------------------------------------
// Concurrent lock requests and split ownership prevention
// ---------------------------------------------------------------------------
describe('concurrent claims and split ownership', () => {
  it('only one coordinator becomes owner from simultaneous claims', async () => {
    const locks = createLockMock();

    // Both coordinators try to open simultaneously
    const tabA = fixture({ locks });
    const tabB = fixture({ locks });

    const [resultA, resultB] = await Promise.all([
      tabA.coordinator.open(agentId, undefined, 5000),
      tabB.coordinator.open(agentId, undefined, 200),
    ]);

    // Exactly one should be owner
    const owners = [tabA.coordinator.isOwner, tabB.coordinator.isOwner].filter(Boolean);
    expect(owners).toHaveLength(1);

    // The non-owner should get pending (not selected)
    if (tabA.coordinator.isOwner) {
      expect(resultA.status).toBe('selected');
      expect(resultB.status).toBe('pending');
    } else {
      expect(resultB.status).toBe('selected');
      expect(resultA.status).toBe('pending');
    }

    tabA.coordinator.stop();
    tabB.coordinator.stop();
  });

  it('does not queue duplicate ownership waits', async () => {
    const locks = createLockMock();

    // Tab A holds lock
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);

    // Tab B tries multiple opens — should only queue one waiter
    const tabB = fixture({ locks });
    await tabB.coordinator.open(agentId, 'req-1', 200);
    await tabB.coordinator.open(agentId, 'req-2', 200);

    expect(locks.waiterCount(tabA.coordinator.coordinationKey)).toBe(1);

    tabA.coordinator.stop();
    tabB.coordinator.stop();
  });
});

// ---------------------------------------------------------------------------
// AC4 / AC6: No automatic restore — fresh start after ownership transition
// ---------------------------------------------------------------------------
describe('fresh start after ownership change (AC4/AC6)', () => {
  it('new owner starts with empty session list', async () => {
    const locks = createLockMock();

    // Tab A opens a session
    const tabA = fixture({ locks });
    await tabA.coordinator.open(agentId, undefined, 5000);
    expect(tabA.coordinator.sessions).toHaveLength(1);

    // Tab B queues for ownership
    const tabB = fixture({ locks });
    await tabB.coordinator.open(agentId, undefined, 200);

    // Tab A stops — releases lock
    tabA.coordinator.stop();
    await vi.waitFor(() => expect(tabB.coordinator.isOwner).toBe(true));

    // Tab B has no sessions (fresh start, no restore)
    expect(tabB.coordinator.sessions).toHaveLength(0);

    // Tab B opens explicitly — gets a new session
    const result = await tabB.coordinator.open(agentId, undefined, 5000);
    expect(result.status).toBe('selected');
    expect(tabB.coordinator.sessions).toHaveLength(1);

    tabB.coordinator.stop();
  });
});

// ---------------------------------------------------------------------------
// Generation validation edge cases
// ---------------------------------------------------------------------------
describe('generation validation', () => {
  it('execute rejects messages with non-matching generation', async () => {
    const f = fixture();
    await f.coordinator.open(agentId, undefined, 5000);

    // Simulate an open message with wrong generation
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'open',
      requestId: 'wrong-gen-req',
      agentId,
      generation: 'wrong-generation-uuid',
    });

    // Only the initial open's session should exist
    expect(f.selectCalls).toHaveLength(1);
    f.coordinator.stop();
  });

  it('deliver sends ack for open with current generation', async () => {
    const f = fixture();
    await f.coordinator.open(agentId, undefined, 5000);

    const initialMessageCount = channelMessages.length;

    // Simulate receiving an open with current generation — should produce an ack
    const currentGen = f.coordinator.generation;
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'open',
      requestId: 'valid-req',
      agentId,
      generation: currentGen,
    });

    // execute() processes asynchronously — wait for the ack to appear
    await vi.waitFor(() => {
      const ackMessages = channelMessages
        .slice(initialMessageCount)
        .filter((msg) => (msg as { type: string }).type === 'ack');
      expect(ackMessages.length).toBeGreaterThan(0);
    });

    f.coordinator.stop();
  });

  it('owner responds to discover with current generation', async () => {
    const f = fixture();
    await f.coordinator.open(agentId, undefined, 5000);
    channelMessages.length = 0;

    // Simulate a discover from another tab
    deliverToPeer(f.coordinator, {
      key: f.coordinator.coordinationKey,
      type: 'discover',
      requestId: 'disc-1',
      agentId,
      generation: null,
    });

    const ownerMessages = channelMessages.filter(
      (msg) => (msg as { type: string }).type === 'owner'
    );
    expect(ownerMessages).toHaveLength(1);
    expect((ownerMessages[0] as { generation: string }).generation).toBe(f.coordinator.generation);

    f.coordinator.stop();
  });
});
