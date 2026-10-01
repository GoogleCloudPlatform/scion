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

/** Always-grant lock mock: sufficient here since ownership races are covered
 * by terminal-coordinator-ownership.test.ts. */
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

function fixture(): {
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
  vi.stubGlobal('navigator', { ...navigator, locks: { request: simpleLocksRequest() } });
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

  it('the same generation twice performs one GET; a new generation performs a new GET', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValue(jsonResponse(serverDoc([], null)));

    await f.persistence.restore(false);
    await f.persistence.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(1);

    // A new owner generation (e.g. a takeover) gets a fresh GET.
    f.coordinator.stop();
    const coordinator2 = new TerminalCoordinator(scope, {
      initialize: (): Promise<TerminalResources> =>
        Promise.resolve({
          write: vi.fn(),
          size: () => ({ cols: 80, rows: 24 }),
          dispose: vi.fn(),
          reset: vi.fn(),
        }),
      select: (): void => {},
    });
    const persistence2 = new TerminalWorkspacePersistence({
      coordinator: coordinator2,
      workspace: f.workspace,
      onRestoredSelection: f.onRestoredSelection,
      fetchImpl: f.fetchImpl as unknown as typeof apiFetch,
    });
    await persistence2.restore(false);
    expect(f.fetchImpl).toHaveBeenCalledTimes(2);
    coordinator2.stop();
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

  const getFailureCases: Array<[string, () => Promise<Response>]> = [
    ['network error', () => Promise.reject(new Error('network'))],
    ['404', () => Promise.resolve(jsonResponse({}, 404))],
    [
      '200 with an HTML body',
      () => Promise.resolve(new Response('<html></html>', { status: 200 })),
    ],
    ['200 with []', () => Promise.resolve(jsonResponse([]))],
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

  it('a change during an in-flight PUT sends exactly one more PUT, through the debounce', async () => {
    vi.useFakeTimers();
    const f = fixture();
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB)));
    await f.persistence.restore(false);
    f.fetchImpl.mockClear();

    const keyA = f.coordinator.sessions.find((s) => s.state.agentId === agentA)!.state.key;
    const keyB = f.coordinator.sessions.find((s) => s.state.agentId === agentB)!.state.key;

    let resolvePut!: (r: Response) => void;
    f.fetchImpl.mockReturnValueOnce(new Promise((resolve) => (resolvePut = resolve)));
    f.setFrontmostKey(keyA); // change: B -> A
    await vi.advanceTimersByTimeAsync(1000); // fires the debounce; PUT #1 (frontmost A) starts

    f.setFrontmostKey(keyB); // change while PUT #1 is in flight: A -> B
    resolvePut(jsonResponse(serverDoc([agentA, agentB], agentA, 0, 2)));
    await Promise.resolve(); // let PUT #1 settle
    f.fetchImpl.mockResolvedValueOnce(jsonResponse(serverDoc([agentA, agentB], agentB, 0, 3)));

    await vi.advanceTimersByTimeAsync(1000); // the dirty re-send's own debounce window

    expect(f.fetchImpl).toHaveBeenCalledTimes(2);
    for (const [, options] of f.fetchImpl.mock.calls as [string, ApiFetchOptions][]) {
      expect(options.keepalive).toBe(true);
    }
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
});
