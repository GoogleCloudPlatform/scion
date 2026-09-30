/**
 * Tests for TerminalWorkspacePersistence (design ptone/scion#2278), Phase 1
 * scope: restore for the bare /terminals route, the "no write before read"
 * invariant, the snapshot, the debounce, and keepalive PUTs. Uses a fake
 * fetch and fake timers, a minimal fake workspace, and the REAL
 * TerminalCoordinator with a stubbed navigator.locks, so the coordinator's
 * own pagehide listener is installed first, as in the browser.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TerminalCoordinator } from './terminal-coordinator.js';
import { TerminalWorkspacePersistence } from './terminal-persistence.js';
import type { TerminalResources, TerminalSession } from './terminal-sessions.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import type { apiFetch, ApiFetchOptions } from './api.js';

const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };
const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';

/** A distinct canonical UUID per index, for tests that need many agents. */
function makeUuid(i: number): string {
  return `10000000-0000-4000-8000-${i.toString(16).padStart(12, '0')}`;
}

class FakeSocket {
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  send = vi.fn();
  close = vi.fn();
  constructor(readonly url: string) {
    FakeSocket.instances.push(this);
  }
}

class FakeBroadcastChannel {
  static instances: FakeBroadcastChannel[] = [];
  onmessage: ((event: { data: unknown }) => void) | null = null;
  closed = false;
  constructor(readonly name: string) {
    FakeBroadcastChannel.instances.push(this);
  }
  postMessage(): void {}
  close(): void {
    this.closed = true;
  }
}

/** Always-grant lock mock, for tests that don't need queuing. */
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

/**
 * A Web Lock mock that tracks held locks and queues waiting requests, so a
 * single coordinator instance can lose and later regain ownership (a new
 * generation) via waitForOwnership — the same mock shape as
 * terminal-coordinator-ownership.test.ts's createLockMock.
 */
function createLockMock(): {
  request: ReturnType<typeof vi.fn>;
  releaseLock(name: string): void;
  isHeld(name: string): boolean;
} {
  const held = new Map<string, { releaseHeld: () => void }>();
  const waiters = new Map<
    string,
    Array<{ callback: (lock: object | null) => Promise<void>; resolve: () => void }>
  >();

  function processNextWaiter(name: string): void {
    const list = waiters.get(name);
    if (!list || list.length === 0) return;
    const next = list.shift()!;
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
        next.resolve();
      })
      .catch(() => {
        held.delete(name);
        processNextWaiter(name);
        next.resolve();
      });
  }

  const request = vi.fn(
    async (
      name: string,
      opts: { mode?: string; ifAvailable?: boolean },
      callback: (lock: object | null) => Promise<void>
    ): Promise<void> => {
      if (opts.ifAvailable) {
        if (held.has(name)) {
          await callback(null);
          return;
        }
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

      // Non-ifAvailable: queue if held (this is waitForOwnership's request).
      if (held.has(name)) {
        return new Promise<void>((resolve) => {
          if (!waiters.has(name)) waiters.set(name, []);
          waiters.get(name)!.push({ callback, resolve });
        });
      }
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
        queueMicrotask(() => processNextWaiter(name));
      }
    },
    isHeld(name: string): boolean {
      return held.has(name);
    },
  };
}

/**
 * Simulates a lock already held by another tab when fixture() constructs its
 * coordinator, so the coordinator's first claimOwnership() call must queue
 * via waitForOwnership rather than acquire immediately. Returns a function
 * that releases the external hold, letting the coordinator's queued waiter
 * acquire it (a new generation on the same instance).
 */
function holdLockExternally(locks: ReturnType<typeof createLockMock>, name: string): () => void {
  let releaseExternal!: () => void;
  const externalHold = new Promise<void>((resolve) => {
    releaseExternal = resolve;
  });
  void locks.request(name, { mode: 'exclusive' }, () => externalHold);
  return releaseExternal;
}

function agentResponse(id: string): Response {
  return new Response(JSON.stringify({ id, name: 'agent', phase: 'running' }), { status: 200 });
}

/** A minimal fake satisfying the surface TerminalWorkspacePersistence calls
 * on TerminalWorkspaceRoot: layoutManager.subscribe/getState, plus
 * withAutoSelectSuspended and select. */
function fakeWorkspace(): {
  workspace: TerminalWorkspaceRoot;
  selectCalls: TerminalSession[];
  setFrontmostKey: (key: string | null) => void;
} {
  const layoutListeners = new Set<() => void>();
  let single0: string | null = null;
  const selectCalls: TerminalSession[] = [];
  const workspace = {
    layoutManager: {
      subscribe: (cb: () => void): (() => void) => {
        layoutListeners.add(cb);
        return () => layoutListeners.delete(cb);
      },
      getState: (): { single: (string | null)[] } => ({ single: [single0] }),
    },
    withAutoSelectSuspended: <T>(fn: () => T): T => fn(),
    select: (session: TerminalSession): void => {
      selectCalls.push(session);
      single0 = session.state.key;
      for (const cb of layoutListeners) cb();
    },
  };
  return {
    workspace: workspace as unknown as TerminalWorkspaceRoot,
    selectCalls,
    setFrontmostKey: (key: string | null): void => {
      single0 = key;
      for (const cb of layoutListeners) cb();
    },
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

function serverDoc(
  agentIds: string[],
  frontmostAgentId: string | null,
  pruned = 0,
  revision = 1
): {
  agentIds: string[];
  frontmostAgentId: string | null;
  revision: number;
  updatedAt: string;
  pruned: number;
} {
  return {
    agentIds,
    frontmostAgentId,
    revision,
    updatedAt: new Date().toISOString(),
    pruned,
  };
}

function fixture(opts?: { locks?: ReturnType<typeof createLockMock> }): {
  coordinator: TerminalCoordinator;
  workspace: TerminalWorkspaceRoot;
  selectCalls: TerminalSession[];
  setFrontmostKey: (key: string | null) => void;
  onRestoredSelection: ReturnType<typeof vi.fn>;
  fetchImpl: ReturnType<typeof vi.fn<typeof apiFetch>>;
  persistence: TerminalWorkspacePersistence;
} {
  FakeSocket.instances = [];
  FakeBroadcastChannel.instances = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel);
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      close(): void {}
    }
  );
  vi.stubGlobal('isSecureContext', true);
  vi.stubGlobal('navigator', {
    ...navigator,
    locks: { request: opts?.locks?.request ?? simpleLocksRequest() },
  });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => Promise.resolve(agentResponse(String(url).split('/').pop())))
  );

  const coordinator = new TerminalCoordinator(scope, {
    initialize: (): Promise<TerminalResources> =>
      Promise.resolve({
        write: vi.fn(),
        size: () => ({ cols: 80, rows: 24 }),
        dispose: vi.fn(),
        reset: vi.fn(),
      }),
    select: (): void => {},
  });

  const { workspace, selectCalls, setFrontmostKey } = fakeWorkspace();
  const onRestoredSelection = vi.fn();
  const fetchImpl = vi.fn<typeof apiFetch>();

  const persistence = new TerminalWorkspacePersistence({
    coordinator,
    workspace,
    onRestoredSelection,
    fetchImpl,
    debounceMs: 1000,
  });

  return {
    coordinator,
    workspace,
    selectCalls,
    setFrontmostKey,
    onRestoredSelection,
    fetchImpl,
    persistence,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('restore()', () => {
  it('restores [A, B, C] with frontmost B: three entries, only B connects and is selected, onRestoredSelection(B) once', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentB)));

    await f.persistence.restore(false);

    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA, agentB, agentC]);
    expect(f.coordinator.sessions.find((s) => s.state.agentId === agentA)?.state.connection).toBe(
      'idle'
    );
    expect(f.coordinator.sessions.find((s) => s.state.agentId === agentC)?.state.connection).toBe(
      'idle'
    );
    expect(
      f.coordinator.sessions.find((s) => s.state.agentId === agentB)?.state.connection
    ).not.toBe('idle');
    expect(f.selectCalls).toHaveLength(1);
    expect(f.selectCalls[0].state.agentId).toBe(agentB);
    expect(f.onRestoredSelection).toHaveBeenCalledExactlyOnceWith(agentB);
  });

  it('frontmost null connects the last entry', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], null)));

    await f.persistence.restore(false);

    expect(f.selectCalls).toHaveLength(1);
    expect(f.selectCalls[0].state.agentId).toBe(agentB);
    expect(f.onRestoredSelection).toHaveBeenCalledExactlyOnceWith(agentB);
  });

  it('claimOwnership() returning false performs no GET and no PUT', async () => {
    const f = fixture();
    vi.spyOn(f.coordinator, 'claimOwnership').mockResolvedValue(false);

    await f.persistence.restore(false);

    expect(f.fetchImpl).not.toHaveBeenCalled();
  });

  it('a throw from merge() (e.g. workspace.select) does not reject restore(); the generation fails and no further GET is sent', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA)));
    vi.spyOn(f.workspace, 'select').mockImplementation(() => {
      throw new Error('boom');
    });

    await expect(f.persistence.restore(false)).resolves.toBeUndefined();
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the GET ran; the throw happened while applying it

    // A second restore() call in the same generation must not perform a
    // second GET: the generation is marked 'failed', not left stuck
    // 'loading' with inflightGet already cleared (which would otherwise let
    // a later bare /terminals visit re-fetch and re-merge indefinitely).
    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    // No PUT on a later change either: saving was never enabled.
    vi.useFakeTimers();
    f.setFrontmostKey('some-key');
    await vi.advanceTimersByTimeAsync(2000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('the same generation twice performs one GET', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValue(jsonResponse(serverDoc([], null)));

    await f.persistence.restore(false);
    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('restore() performs a fresh GET once the lock becomes available, after an earlier attempt found it held elsewhere', async () => {
    // Note on scope: the real TerminalCoordinator has no supported path back
    // to isOwner === true after it has genuinely held and then lost
    // ownership — ownerGeneration is only ever cleared inside stop(), which
    // also sets stopped = true first, and claimOwnership()/claim() both
    // check stopped and refuse forever after. So "the same coordinator
    // instance loses a generation it actually held and regains a new one"
    // is not constructible against the real class without stop() permanently
    // disabling it first. What IS real and worth covering here: this
    // persistence instance's FIRST restore() call can find the lock held by
    // another tab (claimOwnership() resolves false, this.generation stays
    // null) and a LATER restore() call on the SAME instance, after the
    // queued waitForOwnership() grants the lock, must reset state and
    // perform a proper fresh GET — not treat anything as already settled.
    // The guard that a stale generation cannot authorize a write (design
    // section 3.4, the fix for impl-web-1 finding 4) is covered directly,
    // by simulating a coordinator.generation the instance hasn't restored
    // in, in the next test.
    vi.useFakeTimers();
    const locks = createLockMock();
    const f = fixture({ locks });
    // Simulate another tab already holding the lock, so the coordinator's
    // first claimOwnership() must queue via waitForOwnership rather than
    // acquire immediately.
    const releaseExternalHold = holdLockExternally(locks, f.coordinator.coordinationKey);

    f.fetchImpl.mockResolvedValue(jsonResponse(serverDoc([agentA], agentA)));
    await f.persistence.restore(false);
    // Still not the owner: claimOwnership() resolved false, so restore()
    // returned without ever calling fetchImpl.
    expect(f.fetchImpl).not.toHaveBeenCalled();
    expect(f.coordinator.isOwner).toBe(false);

    // The other tab's hold releases; the coordinator's queued waiter
    // acquires the lock, on the SAME coordinator object. The lock mock
    // settles this via plain promise chaining (no timers), so draining the
    // microtask queue is enough — no real or fake time needed.
    releaseExternalHold();
    for (let i = 0; i < 10; i++) await Promise.resolve();
    expect(f.coordinator.isOwner).toBe(true);

    // The SAME persistence instance, called again (as renderRoute would on
    // the next bare /terminals render), must see this as a new generation:
    // this.generation (still null from the earlier no-op call) differs from
    // coordinator.generation, so it resets state, reinstalls the
    // subscribeSessions/layoutManager listeners for this generation, and
    // performs a fresh GET rather than treating a stale 'merged'/'failed'
    // status as already settled.
    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
    expect(f.selectCalls).toHaveLength(1);
    expect(f.selectCalls[0].state.agentId).toBe(agentA);

    // Listeners are live: a further change writes back through the debounce.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA, 0, 2)));
    const keyB = f.coordinator.restoreEntries([agentB], { connectAgentId: null })[0].state.key;
    f.setFrontmostKey(keyB);
    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);

    f.coordinator.stop();
  });

  it("a write is blocked when this instance has not restored in the coordinator's current generation", async () => {
    // Direct regression test for impl-web-1 finding 4 / impl-web-2 finding 2:
    // onChange()/fire() must require this.generation === coordinator.generation,
    // not just generation === this.generation (the value captured at the
    // last successful restore()). Simulated here by stubbing the
    // coordinator's generation getter after a successful merge, standing in
    // for "the coordinator is, or claims to be, in a generation this
    // instance has not itself restored in" — the scenario the guard exists
    // to reject regardless of how the coordinator got there. Proven by
    // mutation: deleting either `generation !== this.coordinator.generation`
    // check (onChange or fire) makes this test fail (a PUT is sent).
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    vi.spyOn(f.coordinator, 'generation', 'get').mockReturnValue('a-generation-never-restored');

    const keyB = f.coordinator.restoreEntries([agentB], { connectAgentId: null })[0].state.key;
    f.setFrontmostKey(keyB);
    await vi.advanceTimersByTimeAsync(2000);

    expect(f.fetchImpl).not.toHaveBeenCalled();
  });

  it('two concurrent restore() calls share one GET', async () => {
    const f = fixture();
    let resolveGet!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolveGet = resolve)));

    const p1 = f.persistence.restore(false);
    const p2 = f.persistence.restore(false);
    resolveGet(jsonResponse(serverDoc([], null)));
    await Promise.all([p1, p2]);

    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('no write before read: a change while the GET is pending sends nothing; the merge appends it and writes back once', async () => {
    vi.useFakeTimers();
    const f = fixture();
    let resolveGet!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolveGet = resolve)));

    const restorePromise = f.persistence.restore(false);
    // A cross-tab open landing before the GET resolves (execute() -> the
    // adapter's create, after waitForOwnership()): install the listeners
    // happen synchronously inside restore() before the GET is awaited, so
    // this is already observed by subscribeSessions while status is
    // 'loading'.
    const preExisting = f.coordinator.restoreEntries([agentA], { connectAgentId: null });
    expect(preExisting).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(2000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // only the GET so far: the pre-merge notification sent nothing

    resolveGet(jsonResponse(serverDoc([agentB, agentC], agentC)));
    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentA, agentB, agentC], null, 0, 2))
    );
    await restorePromise;
    await vi.advanceTimersByTimeAsync(1000);

    // Exactly one PUT, with the pre-existing entry (A) keeping its position
    // and the saved entries (B, C) appended after it.
    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    expect(putCalls).toHaveLength(1);
    const body = JSON.parse((putCalls[0][1] as ApiFetchOptions).body as string) as {
      agentIds: string[];
    };
    expect(body.agentIds).toEqual([agentA, agentB, agentC]);
  });

  const getFailureCases: Array<[string, () => Promise<Response>]> = [
    ['network error', (): Promise<Response> => Promise.reject(new Error('network'))],
    ['404', (): Promise<Response> => Promise.resolve(jsonResponse({}, 404))],
    [
      '200 with an HTML body',
      (): Promise<Response> => Promise.resolve(new Response('<html></html>', { status: 200 })),
    ],
    ['200 with []', (): Promise<Response> => Promise.resolve(jsonResponse([]))],
    [
      'frontmostAgentId not a member of agentIds',
      (): Promise<Response> => Promise.resolve(jsonResponse(serverDoc([agentA, agentB], agentC))),
    ],
  ];

  it.each(getFailureCases)(
    'GET failure (%s): saving stays disabled, no PUT on a later change',
    async (_label, impl) => {
      vi.useFakeTimers();
      const f = fixture();
      // eslint-disable-next-line @typescript-eslint/no-misused-promises -- vi.fn()'s
      // generic mock type does not narrow to the async apiFetch signature here.
      f.fetchImpl.mockImplementationOnce(impl);

      await f.persistence.restore(false);

      f.setFrontmostKey('some-key'); // a layout change after a failed restore
      await vi.advanceTimersByTimeAsync(5000);

      expect(f.fetchImpl).toHaveBeenCalledTimes(1); // only the failed GET; no PUT
      expect(f.coordinator.sessions).toHaveLength(0); // no entries created from an invalid/failed response
    }
  );
});

describe('write-back and debounce', () => {
  it('an unchanged restore (pruned 0) sends zero PUTs after 2x the debounce', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB, 0)));

    await f.persistence.restore(false);
    await vi.advanceTimersByTimeAsync(2000);

    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the GET only
  });

  it('pruned > 0 sends exactly one PUT after the debounce, with the live (pruned) list', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB, 1)));
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB, 0, 2)));

    await f.persistence.restore(false);
    await vi.advanceTimersByTimeAsync(2000);

    expect(f.fetchImpl).toHaveBeenCalledTimes(2);
    const [, putOptions] = f.fetchImpl.mock.calls[1] as [string, ApiFetchOptions];
    expect(putOptions.method).toBe('PUT');
    expect(putOptions.keepalive).toBe(true);
    expect(JSON.parse(putOptions.body as string)).toEqual({
      agentIds: [agentA, agentB],
      frontmostAgentId: agentB,
    });
  });

  it('five changes within the debounce window produce one PUT', async () => {
    vi.useFakeTimers();
    const f = fixture();
    // frontmost agentC matches what the merge auto-selects (last, since
    // nothing was already open): the baseline equals the post-merge live
    // state, so only the manual frontmost toggling below is a real change.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentC)));
    f.fetchImpl.mockResolvedValue(jsonResponse(serverDoc([agentA, agentB, agentC], agentB, 0, 2)));

    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyFor = (id: string): string =>
      f.coordinator.sessions.find((s) => s.state.agentId === id)!.state.key;
    for (const id of [agentB, agentA, agentB, agentA, agentB]) {
      f.setFrontmostKey(keyFor(id));
      await vi.advanceTimersByTimeAsync(100);
    }
    await vi.advanceTimersByTimeAsync(1000);

    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('a change reverted within the debounce window sends nothing', async () => {
    vi.useFakeTimers();
    const f = fixture();
    // frontmost A matches the merge's own selection (declared frontmost,
    // nothing already open), so the baseline equals the post-merge state.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentA)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    const keyB = f.coordinator.sessions.find((s) => s.state.agentId === agentB)!.state.key;

    f.setFrontmostKey(keyB); // A -> B
    await vi.advanceTimersByTimeAsync(500);
    f.setFrontmostKey(keyA); // B -> A: back to the baseline, within the same window

    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).not.toHaveBeenCalled();
  });

  it('a change during an in-flight PUT sends exactly one more PUT, no earlier than 1s after the dirty re-arm', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentC)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    const keyB = f.coordinator.sessions.find((s) => s.state.agentId === agentB)!.state.key;

    let resolvePut!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolvePut = resolve)));
    f.setFrontmostKey(keyA); // change: C (baseline) -> A
    await vi.advanceTimersByTimeAsync(1000); // fires the debounce; PUT #1 (frontmost A) starts and is now pending

    // A -> B while PUT #1 is in flight. B differs from both the in-flight
    // snapshot (A) and the still-current baseline (C, since PUT #1 has not
    // completed), so the second debounce timer's fire() sees a real change
    // and, finding putInFlight true, marks dirty rather than treating it as
    // a no-op revert to baseline (which A -> B -> C would have been).
    f.setFrontmostKey(keyB);
    // Let the second timer actually fire WHILE PUT #1 is still pending: this
    // is what exercises fire() re-entering while putInFlight is true (the
    // dirtyDuringPut path), not just two independently-timed debounces.
    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // still just PUT #1: the re-entrant fire() marked dirty and returned

    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentA, agentB, agentC], agentB, 0, 2))
    );
    resolvePut(jsonResponse(serverDoc([agentA, agentB, agentC], agentA, 0, 2)));
    // Let PUT #1's promise chain fully settle (fetchImpl -> response.json()
    // -> fire()'s continuation -> its finally{} re-arming the dirty
    // debounce) before checking anything: this crosses several microtask
    // hops, not just one.
    for (let i = 0; i < 10; i++) await Promise.resolve();

    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the dirty re-send goes through the debounce, not immediately
    await vi.advanceTimersByTimeAsync(999);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // not yet: under 1000ms since the re-arm
    await vi.advanceTimersByTimeAsync(1);
    expect(f.fetchImpl).toHaveBeenCalledTimes(2); // now: exactly one more PUT

    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    expect(putCalls).toHaveLength(2);
    for (const [, options] of putCalls) {
      expect((options as ApiFetchOptions).keepalive).toBe(true);
    }
    const body2 = JSON.parse((putCalls[1][1] as ApiFetchOptions).body as string) as {
      frontmostAgentId: string | null;
    };
    expect(body2.frontmostAgentId).toBe(agentB);
  });

  it('a revert to the prior baseline during an in-flight PUT is still saved', async () => {
    // fire() must check putInFlight BEFORE comparing snapshot() against
    // baseline: while a PUT is in flight, this.state.baseline is still the
    // PREVIOUS saved doc, not the one the in-flight PUT is about to
    // establish. If the order were reversed, a change that returns to that
    // previous doc during the in-flight PUT would hit the sameDoc early
    // return and never mark dirty — so once the in-flight PUT lands and
    // advances the baseline to what IT sent, the revert is silently lost:
    // the hub keeps a state the user already left (design section 3.4).
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentC)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    const keyC = f.coordinator.sessions.find((s) => s.state.agentId === agentC)!.state.key;

    let resolvePut!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolvePut = resolve)));
    f.setFrontmostKey(keyA); // baseline C -> A
    await vi.advanceTimersByTimeAsync(1000); // fires the debounce; PUT(A) starts and is now pending

    f.setFrontmostKey(keyC); // A -> C: reverts to the ORIGINAL baseline, while PUT(A) is in flight
    await vi.advanceTimersByTimeAsync(1000); // the second debounce fires while PUT(A) is still pending
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // still just PUT(A); the revert must be marked dirty, not dropped

    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentA, agentB, agentC], agentC, 0, 3))
    );
    resolvePut(jsonResponse(serverDoc([agentA, agentB, agentC], agentA, 0, 2))); // PUT(A) lands; baseline -> A
    for (let i = 0; i < 10; i++) await Promise.resolve();
    await vi.advanceTimersByTimeAsync(1000); // the dirty re-send's own debounce window

    expect(f.fetchImpl).toHaveBeenCalledTimes(2); // the revert to C was saved
    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    const body2 = JSON.parse((putCalls[1][1] as ApiFetchOptions).body as string) as {
      frontmostAgentId: string | null;
    };
    expect(body2.frontmostAgentId).toBe(agentC);
  });

  it('a failed PUT (500) does not advance the baseline; the next change sends the current snapshot', async () => {
    vi.useFakeTimers();
    const f = fixture();
    // frontmost C matches the merge's own auto-select (last), so the
    // baseline is clean after restore and stays at C (unadvanced) across
    // the failed PUT below.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentC)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    const keyB = f.coordinator.sessions.find((s) => s.state.agentId === agentB)!.state.key;

    f.fetchImpl.mockResolvedValueOnce(jsonResponse({}, 500));
    f.setFrontmostKey(keyA); // C -> A: differs from the (unadvanced) baseline C
    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(60000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // no retry timer

    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentA, agentB, agentC], agentB, 0, 2))
    );
    f.setFrontmostKey(keyB); // a further change: A -> B, still differs from baseline C
    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);
  });
});

describe('teardown', () => {
  it('sends nothing while a change is pending in the debounce, including via pagehide', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    f.setFrontmostKey(keyA); // a real pending change
    window.dispatchEvent(new Event('pagehide'));
    await vi.advanceTimersByTimeAsync(2000);

    expect(f.fetchImpl).not.toHaveBeenCalled();
    f.coordinator.stop(); // idempotent; already stopped by pagehide
  });

  it('sends nothing after teardownAccount()', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    f.setFrontmostKey(keyA); // a real pending change
    f.coordinator.teardownAccount();
    await vi.advanceTimersByTimeAsync(2000);

    expect(f.fetchImpl).not.toHaveBeenCalled();
  });

  it('sends nothing after teardownAccount() followed by pagehide', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    f.setFrontmostKey(keyA); // a real pending change
    f.coordinator.teardownAccount();
    window.dispatchEvent(new Event('pagehide')); // idempotent; already stopped
    await vi.advanceTimersByTimeAsync(2000);

    // No PUT at all, so by construction none carries a list shorter than the
    // saved one: teardown closes every session, which would otherwise
    // shrink a computed snapshot to empty.
    expect(f.fetchImpl).not.toHaveBeenCalled();
  });
});

describe('snapshot', () => {
  it('excludes an entry whose metadata availability is deleted', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));
    await f.persistence.restore(false);

    const deletedSession = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!;
    deletedSession.markUnavailable('agent-deleted', 'Agent was deleted.');
    vi.spyOn(f.coordinator, 'metadataFor').mockImplementation((id) =>
      id === agentA
        ? { agent: null, availability: 'deleted', error: 'Agent was deleted.' }
        : undefined
    );

    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentB], agentB, 0, 2)));
    f.setFrontmostKey('trigger');
    await vi.advanceTimersByTimeAsync(1000);

    const putCall = f.fetchImpl.mock.calls.find(
      ([, options]) => (options as ApiFetchOptions)?.method === 'PUT'
    );
    expect(putCall).toBeDefined();
    const [, options] = putCall as [string, ApiFetchOptions];
    const body = JSON.parse(options.body as string) as { agentIds: string[] };
    expect(body.agentIds).toEqual([agentB]);
  });

  it('truncates over 32 entries to the frontmost plus the 31 most recently added, in insertion order', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([], null)));
    await f.persistence.restore(false);

    const ids = Array.from({ length: 35 }, (_, i) => makeUuid(i));
    const created = f.coordinator.restoreEntries(ids, { connectAgentId: null });
    expect(created).toHaveLength(35);
    // Select the FIRST id as frontmost: it is not among the 31 most recently
    // added, so it must be kept only because it is frontmost.
    const frontmostKey = created[0].state.key;
    f.setFrontmostKey(frontmostKey);

    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([ids[0]], ids[0], 0, 2)));
    await vi.advanceTimersByTimeAsync(1000);

    const putCall = f.fetchImpl.mock.calls.find(([, options]) => options?.method === 'PUT');
    expect(putCall).toBeDefined();
    const body = JSON.parse((putCall![1] as ApiFetchOptions).body as string) as {
      agentIds: string[];
      frontmostAgentId: string | null;
    };
    expect(body.agentIds).toHaveLength(32);
    expect(body.agentIds[0]).toBe(ids[0]); // frontmost, kept despite being the oldest
    // The rest are the 31 most recently added (ids[4..34]), in original
    // insertion order — not sorted, not reversed.
    expect(body.agentIds.slice(1)).toEqual(ids.slice(4, 35));
    expect(body.frontmostAgentId).toBe(ids[0]);
  });
});
