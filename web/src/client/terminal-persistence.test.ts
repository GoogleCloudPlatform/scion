/**
 * Tests for TerminalWorkspacePersistence: restore for the bare /terminals
 * route and the URL-driven cases (urlIntent, the
 * restore budget with late merge, and the rate-limited background retry),
 * the "no write before read" invariant, the snapshot, the debounce, and
 * keepalive PUTs. Uses a fake fetch and fake timers, a minimal fake
 * workspace, and the REAL TerminalCoordinator with a stubbed
 * navigator.locks, so the coordinator's own pagehide listener is installed
 * first, as in the browser.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TerminalCoordinator } from './terminal-coordinator.js';
import { TerminalWorkspacePersistence, restoreUrlIntent } from './terminal-persistence.js';
import type { TerminalResources, TerminalSession } from './terminal-sessions.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import type { apiFetch, ApiFetchOptions } from './api.js';

const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };
const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';

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
  request: ReturnType<
    typeof vi.fn<
      (
        name: string,
        opts: { mode?: string; ifAvailable?: boolean },
        callback: (lock: object | null) => Promise<void>
      ) => Promise<void>
    >
  >;
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

/**
 * A minimal Response stub, not a real one: `.json()` resolves via a plain
 * `Promise.resolve()` (settles in exactly one microtask), instead of a real
 * Response's stream-backed `.json()`, whose completion is scheduled outside
 * the microtask queue and is not guaranteed to have settled after any fixed
 * number of `await Promise.resolve()` flushes, especially under load. The
 * persistence module only ever reads `.status` and calls `.json()` (see
 * fetchWorkspace/putWorkspace/parseResponse), so that is all this needs to
 * provide.
 */
function jsonResponse(body: unknown, status = 200): Response {
  return { status, json: () => Promise.resolve(body) } as Response;
}

/**
 * Drains the microtask queue until `predicate()` is true, checking after
 * every drain rather than assuming a fixed number of ticks is enough: a
 * hard-coded tick count ties the test to the exact depth of whatever
 * promise chain it's waiting on, which has no reason to stay fixed as the
 * code under test changes. Waiting on the real, observable condition
 * instead removes that dependency. Throws with `description` if the
 * predicate never holds, so a genuine regression fails clearly instead of
 * the test silently racing on.
 */
async function waitFor(predicate: () => boolean, description: string): Promise<void> {
  for (let i = 0; i < 1000; i++) {
    if (predicate()) return;
    await Promise.resolve();
  }
  throw new Error(`waitFor: ${description} did not become true`);
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

/**
 * Every fixture()'s persistence instance, disposed in the shared afterEach
 * below. A successful restore() arms a real debounce timer (armDebounce()
 * uses the platform setTimeout, not a fake one, unless the test has called
 * vi.useFakeTimers()) whenever the write-back debounce is armed. A test that
 * never advances or clears it would otherwise leave a live timer running
 * past the end of the test: it fires later, against that test's now-stale
 * fetchImpl mock (which has nothing left queued), producing a spurious
 * console.warn that lands wherever the console.warn spy happens to be
 * installed by then — a different, later test. dispose() clears the
 * debounce timer, so tracking and disposing every fixture here prevents
 * that regardless of whether a given test uses fake timers.
 */
let activePersistence: TerminalWorkspacePersistence[] = [];

function fixture(opts?: {
  locks?: ReturnType<typeof createLockMock>;
  restoreBudgetMs?: number;
  retryIntervalMs?: number;
}): {
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
    vi.fn((url: string) => Promise.resolve(agentResponse(String(url).split('/').pop() ?? '')))
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
  // Defaults to throwing so a call beyond what a test queued via
  // mockResolvedValueOnce/mockResolvedValue fails loudly and specifically,
  // instead of surfacing indirectly as an extra, unexplained console.warn
  // (or, previously, a TypeError from reading .status off an undefined
  // response).
  const fetchImpl = vi.fn<typeof apiFetch>(() => {
    throw new Error('unexpected fetchImpl call: no response was queued for it');
  });

  const persistence = new TerminalWorkspacePersistence({
    coordinator,
    workspace,
    onRestoredSelection,
    fetchImpl,
    debounceMs: 1000,
    restoreBudgetMs: opts?.restoreBudgetMs ?? 1500,
    retryIntervalMs: opts?.retryIntervalMs ?? 10000,
  });

  activePersistence.push(persistence);

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
  for (const p of activePersistence) p.dispose();
  activePersistence = [];
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('restoreUrlIntent()', () => {
  it('is false for the bare /terminals route', () => {
    expect(restoreUrlIntent('/terminals', '')).toBe(false);
  });

  it('is true for an agent path', () => {
    expect(restoreUrlIntent(`/terminals/${agentA}`, '')).toBe(true);
  });

  it('is true for a layout query naming a preset but no slots', () => {
    expect(restoreUrlIntent('/terminals', '?lv=1&lp=two-columns')).toBe(true);
  });

  it('is true for a layout query naming slots', () => {
    expect(restoreUrlIntent('/terminals', `?lv=1&lp=two-columns&s0=${agentA}&s1=${agentB}`)).toBe(
      true
    );
  });

  it('is false for a query that fails to parse as a layout (unknown version)', () => {
    expect(restoreUrlIntent('/terminals', '?lv=99&lp=two-columns')).toBe(false);
  });

  it('is false for an unrelated path', () => {
    expect(restoreUrlIntent('/terminals-not-really', '')).toBe(false);
  });
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

  it('a throw from merge() (e.g. workspace.select) does not reject restore(); within the retry interval, no further GET is sent', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA)));
    vi.spyOn(f.workspace, 'select').mockImplementation(() => {
      throw new Error('boom');
    });

    await expect(f.persistence.restore(false)).resolves.toBeUndefined();
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the GET ran; the throw happened while applying it

    // A second restore() call within the retry interval must not perform a
    // second GET: the generation is marked 'failed', not left stuck
    // 'loading' with inflightGet already cleared (which would otherwise let
    // a later bare /terminals visit re-fetch and re-merge immediately,
    // bypassing the rate limit). Status 'failed' also enables the same
    // rate-limited background retry a failed GET gets (see the next test):
    // this test only pins the "not immediately" half of that.
    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    // No PUT on a later change either, still within the interval: saving
    // was never enabled.
    vi.useFakeTimers();
    f.setFrontmostKey('some-key');
    await vi.advanceTimersByTimeAsync(2000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('a throw from merge() retries after the interval: the retry re-merges (entries are already open, so select() is not called again), enables saving, and warns only once', async () => {
    vi.useFakeTimers();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const f = fixture({ retryIntervalMs: 10000 });
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA)));
    const select = vi.spyOn(f.workspace, 'select').mockImplementationOnce(() => {
      throw new Error('boom');
    });

    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalledTimes(1);
    // The entry was still created (restoreEntries ran before the throwing
    // select() call), just not selected.
    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA]);

    // After the interval, a restore() call starts a background retry.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA, 0, 2)));
    await vi.advanceTimersByTimeAsync(10000);
    await f.persistence.restore(false);
    // No timer has been armed yet (the first attempt failed before ever
    // reaching merge()); a successful merge's armDebounce() call is what
    // schedules the first one, so this is the retry's completion signal.
    await waitFor(() => vi.getTimerCount() > 0, 'the retried merge armed the write-back debounce');
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);

    // The retried merge finds agentA already in the registry (alreadyOpen),
    // so connectId is null and select() is not called again — the second
    // attempt does not hit the same throw.
    expect(select).toHaveBeenCalledTimes(1);

    // Saving is enabled: a further change writes back.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB, 0, 3)));
    const keyB = f.coordinator.restoreEntries([agentB], { connectAgentId: null })[0].state.key;
    f.setFrontmostKey(keyB);
    await vi.advanceTimersByTimeAsync(1000);
    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    expect(putCalls).toHaveLength(1);
    const body = JSON.parse((putCalls[0][1] as ApiFetchOptions).body as string) as {
      agentIds: string[];
      frontmostAgentId: string | null;
    };
    expect(body).toEqual({ agentIds: [agentA, agentB], frontmostAgentId: agentB });

    // Still exactly one warning for this generation, from the first throw.
    expect(warn).toHaveBeenCalledTimes(1);
  });

  it('a throw from merge() that repeats on the retry still logs only once per generation', async () => {
    vi.useFakeTimers();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const f = fixture({ retryIntervalMs: 10000 });
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA)));
    // Unlike a throw from select() (which only runs when something needs
    // connecting, so it does not repeat once the entry is already open),
    // restoreEntries() runs on every merge call regardless of alreadyOpen —
    // mocking it to always throw simulates a failure that genuinely
    // repeats on the retry.
    vi.spyOn(f.coordinator, 'restoreEntries').mockImplementation(() => {
      throw new Error('boom');
    });

    await f.persistence.restore(false);
    expect(warn).toHaveBeenCalledTimes(1);

    // The retry, once the interval has passed, throws again.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA, 0, 2)));
    await vi.advanceTimersByTimeAsync(10000);
    await f.persistence.restore(false);
    await waitFor(() => f.fetchImpl.mock.calls.length >= 2, 'the retry GET was sent');
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);

    // Still exactly one warning for this generation, not two.
    expect(warn).toHaveBeenCalledTimes(1);
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
    // The guard that a stale generation cannot authorize a write is covered
    // directly, by simulating a coordinator.generation the instance hasn't
    // restored in, in the next test.
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
    await waitFor(() => f.coordinator.isOwner, 'the queued waiter acquired ownership');
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
    // Direct regression test: onChange()/fire() must require
    // this.generation === coordinator.generation,
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

  it('the first restore() call in a generation waits at most the restore budget, even if the GET is still pending; a later call never waits on the network', async () => {
    vi.useFakeTimers();
    const f = fixture({ restoreBudgetMs: 1500 });
    let resolveGet!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolveGet = resolve)));

    let firstSettled = false;
    void f.persistence.restore(false).then(() => {
      firstSettled = true;
    });

    await vi.advanceTimersByTimeAsync(1499);
    expect(firstSettled).toBe(false); // the budget has not elapsed yet
    await vi.advanceTimersByTimeAsync(1);
    expect(firstSettled).toBe(true); // the budget elapsed; restore() returned even though the GET is still pending
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);
    expect(f.coordinator.sessions).toHaveLength(0); // nothing merged yet

    // A second call, with the same GET still pending, must not wait on the
    // network either: navigating within the viewer should never lag on it.
    await expect(f.persistence.restore(false)).resolves.toBeUndefined();
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // no new GET

    // The pending GET arrives late and merges (a late merge).
    resolveGet(jsonResponse(serverDoc([agentA], agentA)));
    await waitFor(() => f.coordinator.sessions.length > 0, 'the late GET merged');
    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA]);
  });

  it('after a GET failure, a restore() call within the retry interval starts no new GET; one after it starts exactly one background GET without waiting for it, and its success enables saving', async () => {
    vi.useFakeTimers();
    const f = fixture({ retryIntervalMs: 10000 });
    f.fetchImpl.mockRejectedValueOnce(new Error('network'));

    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(9999);
    await f.persistence.restore(false); // still within the interval: no new GET
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1); // now exactly retryIntervalMs since the failed attempt
    let resolveRetryGet!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolveRetryGet = resolve)));
    // Resolves without waiting on the still-pending retry GET: if restore()
    // awaited it, this would hang until the test times out.
    await expect(f.persistence.restore(false)).resolves.toBeUndefined();
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);
    expect(f.coordinator.sessions).toHaveLength(0); // the retry GET has not resolved yet

    resolveRetryGet(jsonResponse(serverDoc([agentA], agentA)));
    await waitFor(() => f.coordinator.sessions.length > 0, 'the retry GET merged');
    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA]);

    // Saving is enabled again: a further change writes back.
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA], agentA, 0, 2)));
    const keyB = f.coordinator.restoreEntries([agentB], { connectAgentId: null })[0].state.key;
    f.setFrontmostKey(keyB);
    await vi.advanceTimersByTimeAsync(1000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(3);
  });

  it('a GET failure logs once per generation, not again on a failed retry', async () => {
    vi.useFakeTimers();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const f = fixture({ retryIntervalMs: 10000 });
    f.fetchImpl.mockRejectedValueOnce(new Error('network'));

    await f.persistence.restore(false);
    expect(warn).toHaveBeenCalledTimes(1);

    // The background retry, triggered by a later restore() call once the
    // interval has passed, also fails.
    f.fetchImpl.mockRejectedValueOnce(new Error('network'));
    await vi.advanceTimersByTimeAsync(10000);
    await f.persistence.restore(false);
    await waitFor(() => f.fetchImpl.mock.calls.length >= 2, 'the retry GET was sent');
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);

    // Still exactly one warning for this generation, not two.
    expect(warn).toHaveBeenCalledTimes(1);
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

describe('URL intent', () => {
  it('restore(true) creates every restored entry idle, selects nothing, and does not call onRestoredSelection', async () => {
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));

    await f.persistence.restore(true);

    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA, agentB]);
    for (const session of f.coordinator.sessions) expect(session.state.connection).toBe('idle');
    expect(f.selectCalls).toHaveLength(0);
    expect(f.onRestoredSelection).not.toHaveBeenCalled();
  });

  it('/terminals/<saved frontmost>: the URL agent is already the saved frontmost, so re-selecting it sends zero PUTs after 2x the debounce', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentB)));
    await f.persistence.restore(true);

    // Simulate main.ts's path-open code, which runs after restore() merges:
    // it finds B already restored (idle) and selects it, which is what the
    // real coordinator.open()/adapter.select path does for an id that
    // already has a session.
    const sessionB = f.coordinator.sessions.find((s) => s.state.agentId === agentB)!;
    f.workspace.select(sessionB);

    await vi.advanceTimersByTimeAsync(2000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the GET only: snapshot equals the baseline
  });

  it('/terminals/<X> with X not saved: appending X as frontmost sends exactly one PUT, [A,B,C,X]/X', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentB)));
    await f.persistence.restore(true);

    // Simulate main.ts's path-open code opening and selecting a NEW agent
    // (X = agentD), not among the saved ids.
    const [sessionX] = f.coordinator.restoreEntries([agentD], { connectAgentId: agentD });
    f.workspace.select(sessionX);

    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentA, agentB, agentC, agentD], agentD, 0, 2))
    );
    await vi.advanceTimersByTimeAsync(1000);

    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    expect(putCalls).toHaveLength(1);
    const body = JSON.parse((putCalls[0][1] as ApiFetchOptions).body as string) as {
      agentIds: string[];
      frontmostAgentId: string | null;
    };
    expect(body.agentIds).toEqual([agentA, agentB, agentC, agentD]);
    expect(body.frontmostAgentId).toBe(agentD);
  });

  it('a layout query naming already-saved slots sends zero PUTs', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB, agentC], agentA)));
    await f.persistence.restore(true);

    // Simulate the #1715 layout-restore block opening the URL's slot agents
    // (A and C, both already saved) and setting single[0] to the first
    // occupied slot (A), matching layoutManager.restore's behaviour.
    const sessionA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!;
    f.workspace.select(sessionA);

    await vi.advanceTimersByTimeAsync(2000);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1); // the GET only
  });

  it('a late merge (GET past the restore budget) with the URL agent already open appends the saved entries after it, exactly one PUT', async () => {
    vi.useFakeTimers();
    const f = fixture({ restoreBudgetMs: 1500 });
    let resolveGet!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolveGet = resolve)));

    const restorePromise = f.persistence.restore(true);
    // The URL/path-open code runs while the GET is still pending (when a
    // GET is slower than the budget, the URL/path code runs first): it
    // opens and selects agent X (not saved) directly, before the merge sees
    // it.
    const [sessionX] = f.coordinator.restoreEntries([agentD], { connectAgentId: agentD });
    f.workspace.select(sessionX);

    await vi.advanceTimersByTimeAsync(1500); // the restore budget elapses; the GET is still pending
    await restorePromise;
    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentD]);

    f.fetchImpl.mockResolvedValueOnce(
      jsonResponse(serverDoc([agentD, agentA, agentB, agentC], agentD, 0, 2))
    );
    resolveGet(jsonResponse(serverDoc([agentA, agentB, agentC], agentB)));
    await waitFor(
      () => f.coordinator.sessions.length === 4,
      'the late merge appended the saved entries'
    );
    await vi.advanceTimersByTimeAsync(1000);

    expect(f.coordinator.sessions.map((s) => s.state.agentId)).toEqual([
      agentD,
      agentA,
      agentB,
      agentC,
    ]);
    const putCalls = f.fetchImpl.mock.calls.filter(([, o]) => o?.method === 'PUT');
    expect(putCalls).toHaveLength(1);
    const body = JSON.parse((putCalls[0][1] as ApiFetchOptions).body as string) as {
      agentIds: string[];
      frontmostAgentId: string | null;
    };
    expect(body.agentIds).toEqual([agentD, agentA, agentB, agentC]);
    expect(body.frontmostAgentId).toBe(agentD);
  });
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
    // Wait for PUT #1's promise chain to fully settle (fetchImpl ->
    // response.json() -> fire()'s continuation -> its finally{} re-arming
    // the dirty debounce): no timer is pending until that finally{} block
    // runs, so a newly-armed timer is the signal it has.
    await waitFor(() => vi.getTimerCount() > 0, "PUT #1's finally{} re-armed the dirty debounce");

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
    // the hub keeps a state the user already left.
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
    // No timer is pending until PUT(A)'s finally{} re-arms the dirty
    // debounce; wait for that instead of assuming a fixed tick count.
    await waitFor(() => vi.getTimerCount() > 0, "PUT(A)'s finally{} re-armed the dirty debounce");
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
