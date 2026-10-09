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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  MOVE_OWNERSHIP_TIMEOUT_MS,
  MOVE_RELEASE_TIMEOUT_MS,
  TerminalCoordinator,
} from './terminal-coordinator.js';
import { TerminalWorkspacePersistence } from './terminal-persistence.js';
import {
  MOVE_OWNERSHIP_ATTEMPTS,
  moveTerminalsHere,
  moveTerminalsWithStatus,
} from './terminal-move.js';
import type { TerminalResources, TerminalSession } from './terminal-sessions.js';

const scope = { hubUrl: 'https://hub.example/', accountId: 'account-1' };
const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';

// ── Fakes shared by both windows ───────────────────────────────────────────

/**
 * attach() reads resources.size() and creates the socket synchronously right
 * after, so the window that last sized a terminal is the one creating it.
 */
let sizingWindow = '';

class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  sent: string[] = [];
  closed = false;
  /** The window whose session created this socket (see sizingWindow). */
  readonly window = sizingWindow;
  constructor(readonly url: string) {
    FakeSocket.instances.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {
    this.closed = true;
    this.readyState = 3;
  }
  /** Opens and delivers tmux's attach redraw, which confirms the stream. */
  goLive(): void {
    this.readyState = 1;
    this.onopen?.();
    this.onmessage?.({ data: JSON.stringify({ type: 'data', data: btoa('screen') }) });
  }
}

/** A BroadcastChannel that delivers between instances in this realm, asynchronously. */
class WindowChannel {
  static all = new Set<WindowChannel>();
  static log: Array<{ type: string }> = [];
  onmessage: ((event: { data: unknown }) => void) | null = null;
  constructor(readonly name: string) {
    WindowChannel.all.add(this);
  }
  postMessage(data: unknown): void {
    const copy = structuredClone(data) as { type: string };
    WindowChannel.log.push(copy);
    for (const peer of WindowChannel.all)
      if (peer !== this && peer.name === this.name)
        setTimeout(() => peer.onmessage?.({ data: copy }), 0);
  }
  close(): void {
    WindowChannel.all.delete(this);
  }
}

/** One Web Lock shared by every window: exclusive, ifAvailable, FIFO queue, abortable. */
function sharedLock(): {
  request: (...args: unknown[]) => Promise<void>;
  heldBy: () => string | null;
} {
  let holder: string | null = null;
  const waiters: Array<{ go: () => void }> = [];
  let nextId = 0;
  const grant = async (cb: (lock: object | null) => Promise<void>): Promise<void> => {
    const id = `grant-${nextId++}`;
    holder = id;
    try {
      await cb({});
    } finally {
      holder = null;
      waiters.shift()?.go();
    }
  };
  return {
    heldBy: () => holder,
    request: (async (
      _name: string,
      opts: { ifAvailable?: boolean; signal?: AbortSignal },
      cb: (lock: object | null) => Promise<void>
    ) => {
      if (holder === null && waiters.length === 0) return grant(cb);
      if (opts.ifAvailable) return cb(null);
      await new Promise<void>((resolve, reject) => {
        const waiter = { go: resolve };
        waiters.push(waiter);
        opts.signal?.addEventListener('abort', () => {
          const i = waiters.indexOf(waiter);
          if (i >= 0) waiters.splice(i, 1);
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
      return grant(cb);
    }) as (...args: unknown[]) => Promise<void>,
  };
}

const json = (body: unknown, status = 200): Response =>
  ({
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 403 ? 'Forbidden' : 'OK',
    json: () => Promise.resolve(body),
  }) as Response;

let savedList: { agentIds: string[]; frontmostAgentId: string | null } = {
  agentIds: [agentA, agentB, agentC],
  frontmostAgentId: agentB,
};
/** Preflight status per window label; 200 unless a test says otherwise. */
const preflightStatus = new Map<string, number>();

interface Win {
  label: string;
  coordinator: TerminalCoordinator;
  persistence: TerminalWorkspacePersistence;
  workspaceFetch: ReturnType<typeof vi.fn>;
  selected: TerminalSession[];
  sockets: () => FakeSocket[];
  /** This window's coordination channel. */
  channel: WindowChannel;
  puts: () => string[];
}

const windows: Win[] = [];

function openWindow(label: string): Win {
  const selected: TerminalSession[] = [];
  const coordinator = new TerminalCoordinator(scope, {
    initialize: (): Promise<TerminalResources> => {
      return Promise.resolve({
        write: vi.fn(),
        size: () => {
          sizingWindow = label;
          return { cols: label === 'B' ? 200 : 80, rows: label === 'B' ? 50 : 24 };
        },
        reset: vi.fn(),
        dispose: vi.fn(),
      });
    },
    select: (session) => {
      selected.push(session);
      void session.connect();
    },
  });
  const workspaceFetch = vi.fn((_path: string, init?: { method?: string; body?: string }) => {
    if (init?.method === 'PUT') {
      const body = JSON.parse(init.body ?? '{}') as typeof savedList;
      savedList = body;
      return Promise.resolve(json({ ...body, revision: 2, updatedAt: null, pruned: 0 }));
    }
    return Promise.resolve(json({ ...savedList, revision: 1, updatedAt: null, pruned: 0 }));
  });
  const persistence = new TerminalWorkspacePersistence({
    coordinator,
    workspace: {
      layoutManager: { subscribe: () => () => {}, getState: () => ({ single: [null] }) },
      withAutoSelectSuspended: <T>(fn: () => T): T => fn(),
      select: (session: TerminalSession) => {
        selected.push(session);
        void session.connect();
      },
    } as never,
    onRestoredSelection: () => {},
    fetchImpl: workspaceFetch as never,
    debounceMs: 10,
  });
  const win: Win = {
    label,
    coordinator,
    persistence,
    workspaceFetch,
    selected,
    sockets: () => FakeSocket.instances.filter((socket) => socket.window === label),
    channel: [...WindowChannel.all].at(-1)!,
    puts: () =>
      workspaceFetch.mock.calls
        .filter((call) => call[1]?.method === 'PUT')
        .map((call) => String(call[1]?.body)),
  };
  windows.push(win);
  return win;
}

const flush = async (rounds = 20): Promise<void> => {
  for (let i = 0; i < rounds; i++) await new Promise((resolve) => setTimeout(resolve, 0));
};

/** Window A owns the terminals and shows agentB connected. */
async function ownerWindow(): Promise<Win> {
  const a = openWindow('A');
  await a.persistence.restore(false);
  await flush();
  for (const s of a.sockets()) s.goLive();
  expect(a.coordinator.isOwner).toBe(true);
  return a;
}

/** Window B, which does not own the terminals and shows the non-owner screen. */
async function targetWindow(label = 'B'): Promise<Win> {
  const b = openWindow(label);
  await b.persistence.restore(true);
  const result = await b.coordinator.open(agentB);
  expect(result.status).toBe('selected');
  expect(b.coordinator.isOwner).toBe(false);
  return b;
}

function moveFrom(b: Win): ReturnType<typeof moveTerminalsHere> {
  return moveTerminalsHere({
    coordinator: b.coordinator,
    persistence: b.persistence,
    workspace: {
      withAutoSelectSuspended: <T>(fn: () => T): T => fn(),
      select: (session) => {
        b.selected.push(session);
      },
    },
    preferredAgentId: agentB,
  });
}

beforeEach(() => {
  FakeSocket.instances = [];
  WindowChannel.all.clear();
  WindowChannel.log = [];
  preflightStatus.clear();
  savedList = { agentIds: [agentA, agentB, agentC], frontmostAgentId: agentB };
  const lock = sharedLock();
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('BroadcastChannel', WindowChannel);
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      close(): void {}
    }
  );
  vi.stubGlobal('isSecureContext', true);
  vi.stubGlobal('navigator', { ...navigator, locks: lock });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const id = String(url).match(/agents\/([0-9a-f-]{36})/)?.[1];
      if (String(url).endsWith('/pty')) {
        const status = preflightStatus.get(currentFetcher()) ?? 200;
        return Promise.resolve(json({}, status));
      }
      return Promise.resolve(json({ id, name: 'agent', phase: 'running', activity: 'idle' }));
    })
  );
});

/** The window whose coordinator is not the owner is the one fetching during a move. */
function currentFetcher(): string {
  const target = windows.find((w) => !w.coordinator.isOwner && !w.coordinator.tornDown);
  return target?.label ?? 'A';
}

afterEach(() => {
  for (const w of windows.splice(0)) {
    w.persistence.dispose();
    try {
      w.coordinator.stop();
    } catch {
      /* test teardown */
    }
  }
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('moving terminals to this window (ptone/scion#3328)', () => {
  it('opens the target first, then asks the owner to close, then claims the lock', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const aSockets = a.sockets();
    expect(aSockets).toHaveLength(1);

    const move = moveFrom(b);
    await flush();
    // The target opened its own stream; the owner still has its stream and the lock.
    const bSockets = b.sockets();
    expect(bSockets).toHaveLength(1);
    expect(aSockets[0].closed).toBe(false);
    expect(a.coordinator.isOwner).toBe(true);
    expect(WindowChannel.log.some((m) => m.type === 'take-over')).toBe(false);

    bSockets[0].goLive();
    const result = await move;

    expect(result).toEqual({ status: 'moved', agentId: agentB });
    expect(aSockets[0].closed).toBe(true);
    expect(a.coordinator.isOwner).toBe(false);
    expect(b.coordinator.isOwner).toBe(true);
    expect(bSockets[0].closed).toBe(false);
    // Take-over went out after the target's stream was live and resized.
    const types = WindowChannel.log.map((m) => m.type);
    expect(types.indexOf('take-over')).toBeGreaterThanOrEqual(0);
    expect(types.indexOf('released')).toBeGreaterThan(types.indexOf('take-over'));
    expect(bSockets[0].sent.map((d) => JSON.parse(d) as { type: string })).toContainEqual({
      type: 'resize',
      cols: 200,
      rows: 50,
    });
  });

  it('reopens exactly the persisted list, connecting only the shown terminal', async () => {
    await ownerWindow();
    const b = await targetWindow();
    const move = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    await move;

    const sessions = b.coordinator.sessions;
    expect(sessions.map((s) => s.state.agentId)).toEqual([agentA, agentB, agentC]);
    expect(sessions.map((s) => s.state.connection)).toEqual(['idle', 'connected', 'idle']);
    expect(b.sockets()).toHaveLength(1);
    expect(b.sockets()[0].url).toContain(agentB);
  });

  it('sends the take-over message only to an owner, which closes without typing into tmux', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const relinquished = vi.fn();
    a.coordinator.onRelinquished(relinquished);
    const aSocket = a.sockets()[0];
    const move = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    await move;

    const takeOver = WindowChannel.log.find((m) => m.type === 'take-over') as Record<
      string,
      unknown
    >;
    expect(takeOver).toMatchObject({ key: b.coordinator.coordinationKey, agentId: agentB });
    expect(relinquished).toHaveBeenCalledOnce();
    expect(a.coordinator.sessions).toEqual([]);
    // close('navigation'): no tmux detach key (prefix d) sent through the old stream.
    expect(aSocket.sent.filter((d) => d.includes('"data"'))).toEqual([]);
  });

  it('keeps the owner streams and lock when the target fails to authorize', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    preflightStatus.set('B', 403);
    const aSocket = a.sockets()[0];
    const openForMove = vi.spyOn(b.coordinator, 'openForMove');

    const result = await moveFrom(b);
    await flush();

    expect(result).toEqual({
      status: 'failed',
      message: 'You do not have permission to attach to this agent.',
    });
    expect(aSocket.closed).toBe(false);
    expect(a.coordinator.isOwner).toBe(true);
    expect(a.coordinator.sessions).toHaveLength(3);
    expect(b.coordinator.isOwner).toBe(false);
    expect(WindowChannel.log.some((m) => m.type === 'take-over')).toBe(false);
    // The target's own entries are closed and gone from its registry.
    const staged = openForMove.mock.results[0].value as readonly TerminalSession[];
    expect(staged).toHaveLength(3);
    expect(staged.map((s) => s.state.connection)).toEqual(['closed', 'closed', 'closed']);
    for (const id of [agentA, agentB, agentC])
      expect(b.coordinator.metadataFor(id)).toBeUndefined();
  });

  it('waits for the session connect timeouts from #4092 and adds none of its own', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    vi.useFakeTimers();
    let settled = false;
    const move = moveFrom(b).then((result) => {
      settled = true;
      return result;
    });
    await vi.advanceTimersByTimeAsync(50);
    expect(b.sockets()).toHaveLength(1);
    // The socket never opens. The move still waits at 9.9s ...
    await vi.advanceTimersByTimeAsync(9_800);
    expect(settled).toBe(false);
    // ... and fails once the session's own 10s open timeout ends the attempt.
    await vi.advanceTimersByTimeAsync(500);
    const result = await move;
    expect(result).toEqual({ status: 'failed', message: 'No response from the terminal stream.' });
    expect(a.sockets()[0].closed).toBe(false);
    expect(a.coordinator.isOwner).toBe(true);
  });

  it('keeps the owner streams when the owner never confirms, and closes the target', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    // A window that is frozen or gone does not answer the take-over.
    const [aChannel] = WindowChannel.all; // A's channel was created first
    aChannel.onmessage = null;
    vi.useFakeTimers();
    const move = moveFrom(b);
    await vi.advanceTimersByTimeAsync(50);
    b.sockets()[0].goLive();
    await vi.advanceTimersByTimeAsync(MOVE_RELEASE_TIMEOUT_MS + 100);
    const result = await move;
    expect(result).toEqual({
      status: 'failed',
      message: 'The window with the terminals did not respond.',
    });
    expect(b.sockets()[0].closed).toBe(true);
    expect(a.sockets()[0].closed).toBe(false);
    expect(a.coordinator.isOwner).toBe(true);
  });

  it('does not take a held lock early', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    vi.useFakeTimers();
    const owned = b.coordinator.awaitOwnership();
    await vi.advanceTimersByTimeAsync(MOVE_OWNERSHIP_TIMEOUT_MS + 100);
    expect(await owned).toBe(false);
    expect(a.coordinator.isOwner).toBe(true);
    expect(b.coordinator.isOwner).toBe(false);
  });

  it('leaves the subscriber contract unchanged: no callbacks before ownership', async () => {
    await ownerWindow();
    const b = await targetWindow();
    const calls: number[] = [];
    b.coordinator.subscribeSessions((sessions) => calls.push(sessions.length));
    const move = moveFrom(b);
    await flush();
    // Entries exist in B's registry, but B is not the owner: nothing delivered.
    expect(calls).toEqual([]);
    expect(b.coordinator.sessions).toEqual([]);
    b.sockets()[0].goLive();
    await move;
    expect(b.coordinator.sessions).toHaveLength(3);
  });

  it('does not save from the old window and saves from the new one', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const aPutsBefore = a.puts().length;
    const move = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    expect((await move).status).toBe('moved');
    await flush(40);
    // The old window closed every session but saved nothing.
    expect(a.puts()).toHaveLength(aPutsBefore);
    expect(savedList.agentIds).toEqual([agentA, agentB, agentC]);

    // A change in the new window is saved from there.
    b.coordinator.sessions.find((s) => s.state.agentId === agentC)!.close();
    await flush(40);
    expect(b.puts()).toHaveLength(1);
    expect(savedList.agentIds).toEqual([agentA, agentB]);
    expect(a.puts()).toHaveLength(aPutsBefore);
  });

  it('lets the old window move the terminals back', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const first = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    expect((await first).status).toBe('moved');

    const back = moveTerminalsHere({
      coordinator: a.coordinator,
      persistence: a.persistence,
      workspace: { withAutoSelectSuspended: (fn) => fn(), select: () => {} },
      preferredAgentId: agentB,
    });
    await flush();
    const fresh = a.sockets().filter((s) => !s.closed && s.readyState === 0);
    expect(fresh).toHaveLength(1);
    fresh[0].goLive();
    expect((await back).status).toBe('moved');
    expect(a.coordinator.isOwner).toBe(true);
    expect(b.coordinator.isOwner).toBe(false);
    expect(b.coordinator.sessions).toEqual([]);
  });

  it('in a window that owns with everything open, opens nothing new', async () => {
    const a = await ownerWindow();
    const before = FakeSocket.instances.length;
    expect(await moveFrom(a)).toEqual({ status: 'moved', agentId: agentB });
    expect(FakeSocket.instances).toHaveLength(before);
    expect(a.coordinator.sessions).toHaveLength(3);
    expect(WindowChannel.log.some((m) => m.type === 'take-over')).toBe(false);
  });

  it('fails when the saved list cannot be read', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    b.workspaceFetch.mockResolvedValueOnce(json({}, 500));
    expect(await moveFrom(b)).toEqual({
      status: 'failed',
      message: 'The saved list of open terminals could not be read.',
    });
    expect(a.coordinator.isOwner).toBe(true);
  });

  it('moves back after the new window closes: the old window owns again with nothing open', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const first = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    expect((await first).status).toBe('moved');
    // B closes; its lock release reaches A's queued wait.
    b.persistence.dispose();
    b.coordinator.stop();
    await flush();
    expect(a.coordinator.isOwner).toBe(true);
    expect(a.coordinator.sessions).toEqual([]);

    const live = new Set(a.sockets());
    const back = moveTerminalsWithStatusFor(a);
    await flush();
    const fresh = a.sockets().filter((s) => !live.has(s));
    expect(fresh).toHaveLength(1);
    expect(fresh[0].url).toContain(agentB);
    fresh[0].goLive();
    const { result, ui } = await back;
    expect(result).toEqual({ status: 'moved', agentId: agentB });
    const shown = a.selected.at(-1)!;
    expect(shown.state.agentId).toBe(agentB);
    expect(shown.state.connection).toBe('connected');
    expect(a.coordinator.sessions.map((s) => s.state.agentId)).toEqual([agentA, agentB, agentC]);
    expect(ui.statuses).toEqual([]);
    expect(ui.actions.at(-1)).toBeNull();
    // Saving is on in A.
    a.coordinator.sessions.find((s) => s.state.agentId === agentC)!.close();
    await flush(40);
    expect(savedList.agentIds).toEqual([agentA, agentB]);
  });

  it('shows the empty viewer, not an error, when the owner has nothing saved', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const first = moveFrom(b);
    await flush();
    b.sockets()[0].goLive();
    expect((await first).status).toBe('moved');
    // Close every terminal in B (saved as an empty list), then close B.
    for (const session of [...b.coordinator.sessions]) session.close();
    await flush(40);
    expect(savedList.agentIds).toEqual([]);
    b.persistence.dispose();
    b.coordinator.stop();
    await flush();
    expect(a.coordinator.isOwner).toBe(true);

    const before = FakeSocket.instances.length;
    const { result, ui } = await moveTerminalsWithStatusFor(a);
    expect(result).toEqual({ status: 'moved', agentId: null });
    expect(ui.statuses).toEqual([]);
    expect(ui.cleared).toBe(1);
    expect(ui.actions.at(-1)).toBeNull();
    expect(FakeSocket.instances).toHaveLength(before);
    expect(a.coordinator.sessions).toEqual([]);
    // Saving is on in A: opening a terminal there is saved.
    const putsBefore = a.puts().length;
    expect((await a.coordinator.open(agentA)).status).toBe('selected');
    await flush(40);
    expect(a.puts().length).toBeGreaterThan(putsBefore);
    expect(savedList.agentIds).toEqual([agentA]);
  });

  it('still fails in a non-owner window when nothing is saved', async () => {
    await ownerWindow();
    const b = await targetWindow();
    savedList = { agentIds: [], frontmostAgentId: null };
    expect(await moveFrom(b)).toEqual({ status: 'failed', message: 'No terminals are open.' });
  });

  it('Retry after a failed move works once the old owner has closed', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    preflightStatus.set('B', 403);
    const failedMove = await moveTerminalsWithStatusFor(b);
    expect(failedMove.result.status).toBe('failed');
    expect(failedMove.ui.actions.at(-1)?.label).toBe('Retry');
    // The old owner closes; B gets the lock while showing the error.
    a.persistence.dispose();
    a.coordinator.stop();
    await flush();
    expect(b.coordinator.isOwner).toBe(true);
    preflightStatus.clear();

    const retry = moveTerminalsWithStatusFor(b);
    await flush();
    const fresh = b.sockets().filter((s) => !s.closed);
    expect(fresh).toHaveLength(1);
    fresh[0].goLive();
    const { result, ui } = await retry;
    expect(result).toEqual({ status: 'moved', agentId: agentB });
    const shown = b.selected.at(-1)!;
    expect(shown.state.agentId).toBe(agentB);
    expect(shown.state.connection).toBe('connected');
    expect(ui.statuses).toEqual([]);
    expect(ui.actions.at(-1)).toBeNull();
    b.coordinator.sessions.find((s) => s.state.agentId === agentC)!.close();
    await flush(40);
    expect(savedList.agentIds).toEqual([agentA, agentB]);
  });

  it('only the owner acts on a take-over; other windows ignore it', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const c = openWindow('C');
    await c.coordinator.open(agentB);
    const bRelinquished = vi.fn();
    b.coordinator.onRelinquished(bRelinquished);
    const released = await c.coordinator.requestRelease(agentB);
    expect(released).toBe(true);
    expect(bRelinquished).not.toHaveBeenCalled();
    expect(WindowChannel.log.filter((m) => m.type === 'released')).toHaveLength(1);
    expect(a.coordinator.isOwner).toBe(false);
  });

  it('openForMove creates nothing in the owner window', async () => {
    const a = await ownerWindow();
    expect(a.coordinator.openForMove([agentC], { connectAgentId: agentC })).toEqual([]);
    expect(a.coordinator.sessions).toHaveLength(3);
  });

  it('teardown ends a pending ownership wait', async () => {
    await ownerWindow();
    const b = await targetWindow();
    const owned = b.coordinator.awaitOwnership();
    b.coordinator.stop();
    expect(await owned).toBe(false);
  });

  it('ignores a take-over that does not name the current owner generation', async () => {
    const a = await ownerWindow();
    const peer = new WindowChannel(a.coordinator.coordinationKey);
    for (const generation of [null, 'stale-generation'])
      peer.postMessage({
        key: a.coordinator.coordinationKey,
        type: 'take-over',
        requestId: crypto.randomUUID(),
        agentId: agentB,
        generation,
      });
    await flush();
    peer.close();
    expect(a.coordinator.isOwner).toBe(true);
    expect(a.sockets()[0].closed).toBe(false);
    expect(WindowChannel.log.some((m) => m.type === 'released')).toBe(false);
  });

  it('asks again when a window that was already waiting gets the lock first', async () => {
    const a = await ownerWindow();
    const c = await targetWindow('C'); // waiting for the lock before B
    const b = await targetWindow();
    const cRelinquished = vi.fn();
    c.coordinator.onRelinquished(cRelinquished);
    vi.useFakeTimers();
    const move = moveFrom(b);
    await vi.advanceTimersByTimeAsync(50);
    b.sockets()[0].goLive();
    await vi.advanceTimersByTimeAsync(50);
    // A released, but the first come, first served lock went to C.
    expect(a.coordinator.isOwner).toBe(false);
    expect(c.coordinator.isOwner).toBe(true);
    expect(b.coordinator.isOwner).toBe(false);
    await vi.advanceTimersByTimeAsync(MOVE_OWNERSHIP_TIMEOUT_MS + 100);
    expect(await move).toEqual({ status: 'moved', agentId: agentB });
    expect(cRelinquished).toHaveBeenCalledOnce();
    expect(b.coordinator.isOwner).toBe(true);
    expect(c.coordinator.isOwner).toBe(false);
    expect(b.sockets()[0].closed).toBe(false);
    expect(WindowChannel.log.filter((m) => m.type === 'take-over')).toHaveLength(2);
  });

  it('closes its own entries when the window that got the lock does not release it', async () => {
    const a = await ownerWindow();
    const c = await targetWindow('C');
    const b = await targetWindow();
    vi.useFakeTimers();
    const move = moveFrom(b);
    await vi.advanceTimersByTimeAsync(50);
    b.sockets()[0].goLive();
    await vi.advanceTimersByTimeAsync(50);
    expect(c.coordinator.isOwner).toBe(true);
    c.channel.onmessage = null; // C does not answer
    await vi.advanceTimersByTimeAsync(MOVE_OWNERSHIP_TIMEOUT_MS + MOVE_RELEASE_TIMEOUT_MS + 200);
    expect(await move).toEqual({
      status: 'failed',
      message: 'The window with the terminals did not respond.',
    });
    // Whichever window holds the lock keeps its streams; B is left with none.
    expect(b.sockets()[0].closed).toBe(true);
    expect(b.coordinator.isOwner).toBe(false);
    expect(c.coordinator.isOwner).toBe(true);
    expect(a.coordinator.isOwner).toBe(false);
  });

  it(`gives up after ${MOVE_OWNERSHIP_ATTEMPTS} attempts when the lock keeps going elsewhere`, async () => {
    await ownerWindow();
    const waiting = [];
    for (const label of ['C', 'D', 'E']) waiting.push(await targetWindow(label));
    const b = await targetWindow();
    vi.useFakeTimers();
    const move = moveTerminalsWithStatusFor(b);
    await vi.advanceTimersByTimeAsync(50);
    b.sockets()[0].goLive();
    await vi.advanceTimersByTimeAsync(MOVE_OWNERSHIP_ATTEMPTS * (MOVE_OWNERSHIP_TIMEOUT_MS + 200));
    const { result, ui } = await move;
    expect(result).toEqual({
      status: 'failed',
      message: 'Another window took the terminals before this one.',
    });
    expect(WindowChannel.log.filter((m) => m.type === 'take-over')).toHaveLength(
      MOVE_OWNERSHIP_ATTEMPTS
    );
    expect(waiting.map((w) => w.coordinator.isOwner)).toEqual([false, false, true]);
    expect(b.sockets()[0].closed).toBe(true);
    expect(ui.statuses.at(-1)).toBe(
      'Terminals could not be moved: Another window took the terminals before this one.'
    );
    expect(ui.actions.at(-1)?.label).toBe('Retry');
  });

  it('completes, on screen and saving, when the owner goes away mid-move', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    // The owner window closes without answering the take-over.
    a.channel.onmessage = null;
    const move = moveTerminalsWithStatusFor(b);
    await flush();
    const target = b.selected.at(-1)!;
    b.sockets()[0].goLive();
    a.persistence.dispose();
    a.coordinator.stop();
    const { result, ui } = await move;

    expect(result).toEqual({ status: 'moved', agentId: agentB });
    expect(b.coordinator.isOwner).toBe(true);
    expect(target.state.agentId).toBe(agentB);
    expect(target.state.connection).toBe('connected');
    expect(b.sockets()[0].closed).toBe(false);
    // No stale error or "Moving terminals…" is left behind.
    expect(ui.statuses).toEqual([]);
    expect(ui.actions.at(-1)).toBeNull();
    // Saving resumed in B.
    b.coordinator.sessions.find((s) => s.state.agentId === agentC)!.close();
    await flush(40);
    expect(savedList.agentIds).toEqual([agentA, agentB]);
  });

  it('closes a non-owner window entries opened for a move on teardown', async () => {
    const a = await ownerWindow();
    const b = await targetWindow();
    const openForMove = vi.spyOn(b.coordinator, 'openForMove');
    const move = moveFrom(b);
    await flush();
    const staged = openForMove.mock.results[0].value as readonly TerminalSession[];
    expect(b.sockets()).toHaveLength(1);
    b.coordinator.stop();
    expect(staged.map((s) => s.state.connection)).toEqual(['closed', 'closed', 'closed']);
    expect(b.sockets()[0].closed).toBe(true);
    expect((await move).status).toBe('failed');
    expect(a.coordinator.isOwner).toBe(true);
    expect(a.sockets()[0].closed).toBe(false);
  });
});

/** Runs moveTerminalsWithStatus for window b, recording what it shows. */
async function moveTerminalsWithStatusFor(b: Win): Promise<{
  result: Awaited<ReturnType<typeof moveTerminalsWithStatus>>;
  ui: {
    statuses: string[];
    actions: Array<{ label: string; disabled: boolean | undefined } | null>;
    cleared: number;
  };
}> {
  const ui = {
    statuses: [] as string[],
    actions: [] as Array<{ label: string; disabled: boolean | undefined } | null>,
    cleared: 0,
  };
  const result = await moveTerminalsWithStatus({
    coordinator: b.coordinator,
    persistence: b.persistence,
    workspace: {
      withAutoSelectSuspended: <T>(fn: () => T): T => fn(),
      select: (session) => {
        b.selected.push(session);
      },
    },
    ui: {
      setStatus: (message) => ui.statuses.push(message),
      setStatusAction: (action) =>
        ui.actions.push(action && { label: action.label, disabled: action.disabled }),
      clearStatus: () => {
        ui.cleared++;
        ui.actions.push(null);
      },
    },
    preferredAgentId: agentB,
    retry: () => {},
  });
  return { result, ui };
}
