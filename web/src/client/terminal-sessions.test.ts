import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  TerminalSessionRegistry,
  type TerminalResourceInitializer,
  type TerminalSessionState,
  type TerminalResources,
} from './terminal-sessions.js';

const agentId = '11111111-1111-4111-8111-111111111111';
const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };
const agent = { id: agentId, name: 'example', phase: 'running', activity: 'executing' };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
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
  data(bytes: number[]) {
    this.onmessage?.({
      data: JSON.stringify({ type: 'data', data: btoa(String.fromCharCode(...bytes)) }),
    });
  }
}
function fixture() {
  FakeSocket.instances = [];
  vi.stubGlobal('WebSocket', FakeSocket);
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

describe('terminal sessions', () => {
  it('registers before async work and shares pending and connected opens', async () => {
    const f = fixture();
    const gate = deferred<Response>();
    f.fetcher.mockReturnValueOnce(gate.promise);
    const first = f.registry.open(agentId, f.initialize);
    expect(f.registry.open(agentId.toUpperCase(), f.initialize)).toBe(first);
    const attempt = first.connect();
    expect(first.connect()).toBe(attempt);
    gate.resolve(json(agent));
    await attempt;
    expect(FakeSocket.instances).toHaveLength(1);
    FakeSocket.instances[0].open();
    expect(f.registry.open(agentId, f.initialize)).toBe(first);
    await first.connect();
    expect(f.fetcher).toHaveBeenCalledTimes(2);
    expect(f.initialize).toHaveBeenCalledTimes(1);
    expect(first.state.connection).toBe('connected');
    expect(first.state.agent?.activity).toBe('executing');
  });

  it.each(['metadata', 'metadata body', 'preflight', 'resources'])(
    'close during %s prevents a late socket',
    async (stage) => {
      const f = fixture();
      const response = deferred<Response>();
      const body = deferred<unknown>();
      const resources = deferred<TerminalResources>();
      if (stage === 'metadata') f.fetcher.mockReturnValueOnce(response.promise);
      if (stage === 'metadata body')
        f.fetcher.mockResolvedValueOnce({ ok: true, json: () => body.promise } as Response);
      if (stage === 'preflight')
        f.fetcher.mockResolvedValueOnce(json(agent)).mockReturnValueOnce(response.promise);
      if (stage === 'resources') f.initialize.mockReturnValueOnce(resources.promise);
      const session = f.registry.open(agentId, f.initialize);
      const attempt = session.connect();
      await vi.waitFor(() => {
        if (stage === 'preflight') expect(f.fetcher).toHaveBeenCalledTimes(2);
        else if (stage === 'resources') expect(f.initialize).toHaveBeenCalledTimes(1);
        else expect(f.fetcher).toHaveBeenCalledTimes(1);
      });
      session.close();
      response.resolve(json(agent));
      body.resolve(agent);
      resources.resolve(f.resources);
      await attempt;
      expect(FakeSocket.instances).toHaveLength(0);
      expect(session.state.connection).toBe('closed');
      expect(f.registry.list()).toHaveLength(0);
      const options = f.fetcher.mock.calls[0][1] as RequestInit;
      expect(options.signal?.aborted).toBe(true);
      if (stage === 'resources') expect(f.resources.dispose).toHaveBeenCalledTimes(1);
    }
  );

  it('keeps bytes ordered and never replays disconnected input', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    expect(session.sendData('before')).toBe(false);
    await session.connect();
    const socket = FakeSocket.instances[0];
    expect(session.sendData('pending')).toBe(false);
    socket.open();
    for (const bytes of [[0xe2], [0x82], [0xac, 0x1b], [0x5b, 0x32, 0x4a]]) socket.data(bytes);
    expect(vi.mocked(f.resources.write).mock.calls.map(([bytes]) => [...bytes])).toEqual([
      [0xe2],
      [0x82],
      [0xac, 0x1b],
      [0x5b, 0x32, 0x4a],
    ]);
    session.sendData('€');
    expect(socket.send.mock.calls.map(([data]) => JSON.parse(data) as unknown)).toEqual([
      { type: 'data', data: '4oKs' },
    ]);
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });
    expect(session.sendData('lost')).toBe(false);
    expect(f.registry.open(agentId, f.initialize)).toBe(session);
    expect(FakeSocket.instances).toHaveLength(1);
    await session.connect();
    FakeSocket.instances[1].open();
    expect(FakeSocket.instances[1].send).not.toHaveBeenCalled();
    expect(f.resources.dispose).not.toHaveBeenCalled();
    expect(f.resources.reset).toHaveBeenCalledTimes(1);
  });

  it('ignores old socket callbacks after reconnect and close', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const old = FakeSocket.instances[0];
    old.open();
    old.readyState = 3;
    old.onclose?.({ code: 1006 });
    await session.connect();
    const current = FakeSocket.instances[1];
    current.open();
    old.onmessage?.({ data: JSON.stringify({ type: 'data', data: 'eA==' }) });
    old.onclose?.({ code: 1006 });
    old.onerror?.();
    old.onopen?.();
    expect(session.state.connection).toBe('connected');
    expect(f.resources.reset).toHaveBeenCalledTimes(1);
    expect(f.resources.write).not.toHaveBeenCalled();
    session.close();
    current.onopen?.();
    current.onerror?.();
    current.data([1]);
    expect(session.state.connection).toBe('closed');
    expect(f.resources.write).not.toHaveBeenCalled();
  });

  it('detaches and disposes exactly once on explicit close', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const socket = FakeSocket.instances[0];
    socket.open();
    session.close();
    session.close();
    await session.connect();
    expect(socket.send).toHaveBeenCalledExactlyOnceWith(
      JSON.stringify({ type: 'data', data: 'AmQ=' })
    );
    expect(socket.close).toHaveBeenCalledTimes(1);
    expect(f.resources.dispose).toHaveBeenCalledTimes(1);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it('navigation disposal closes without sending tmux detach', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const socket = FakeSocket.instances[0];
    socket.open();
    session.close('navigation');
    expect(socket.send).not.toHaveBeenCalled();
    expect(socket.close).toHaveBeenCalledTimes(1);
  });

  it('scopes keys and endpoints to trusted hub base path and account', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    expect(FakeSocket.instances[0].url).toBe(
      `wss://hub.example/team/api/v1/agents/${agentId}/pty?cols=80&rows=24`
    );
    const otherAccount = new TerminalSessionRegistry({ ...scope, accountId: 'account-2' });
    const otherBase = new TerminalSessionRegistry({
      ...scope,
      hubUrl: 'https://hub.example/other/',
    });
    expect(otherAccount.open(agentId, f.initialize).state.key).not.toBe(session.state.key);
    expect(otherBase.open(agentId, f.initialize).state.key).not.toBe(session.state.key);
    expect(() => f.registry.open('agent-name', f.initialize)).toThrow();
    for (const registry of [f.registry, otherAccount, otherBase]) {
      for (const entry of registry.list()) entry.close();
    }
  });
});

describe('terminal lifecycle boundaries', () => {
  it('preserves the first adapter and retains its screen on denied reconnect', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const replacement = vi.fn(() => Promise.resolve(f.resources));
    expect(f.registry.open(agentId, replacement)).toBe(session);
    await session.connect();
    const socket = FakeSocket.instances[0];
    socket.open();
    socket.readyState = 3;
    socket.onclose?.({ code: 1006 });
    f.fetcher.mockResolvedValueOnce(json(agent)).mockResolvedValueOnce(json({}, 403));
    await session.connect();
    expect(session.state.error).toContain('permission');
    expect(f.resources.dispose).not.toHaveBeenCalled();
    expect(replacement).not.toHaveBeenCalled();
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it('late resource completion disposes only the closed record after UUID reopen', async () => {
    const f = fixture();
    const late = deferred<TerminalResources>();
    f.initialize.mockReturnValueOnce(late.promise);
    const old = f.registry.open(agentId, f.initialize);
    const oldAttempt = old.connect();
    await vi.waitFor(() => expect(f.initialize).toHaveBeenCalledTimes(1));
    old.close();
    const replacementResources = { ...f.resources, dispose: vi.fn(), write: vi.fn() };
    const current = f.registry.open(agentId, () => Promise.resolve(replacementResources));
    await current.connect();
    FakeSocket.instances[0].open();
    late.resolve(f.resources);
    await oldAttempt;
    expect(f.resources.dispose).toHaveBeenCalledTimes(1);
    expect(replacementResources.dispose).not.toHaveBeenCalled();
    expect(current.state.connection).toBe('connected');
    expect(f.registry.list()).toEqual([current]);
    FakeSocket.instances[0].data([97]);
    expect(replacementResources.write).toHaveBeenCalledExactlyOnceWith(new Uint8Array([97]));
  });

  it.each([401, 403, 404, 503])('does not attach after preflight %s', async (status) => {
    const f = fixture();
    f.fetcher
      .mockResolvedValueOnce(json(agent))
      .mockResolvedValueOnce(json({ error: { message: 'broker unavailable' } }, status));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    expect(session.state.connection).toBe('disconnected');
    expect(session.state.error).toBeTruthy();
    expect(f.initialize).not.toHaveBeenCalled();
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it('keeps unavailable agent activity separate from terminal connection', async () => {
    const f = fixture();
    f.fetcher.mockResolvedValueOnce(json({ ...agent, activity: 'offline' }));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    expect(session.state.connection).toBe('unavailable');
    expect(session.state.agent?.phase).toBe('running');
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it('rejects mismatching metadata identity before preflight', async () => {
    const f = fixture();
    f.fetcher.mockResolvedValueOnce(json({ ...agent, id: '22222222-2222-4222-8222-222222222222' }));
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    expect(session.state.error).toContain('UUID');
    expect(f.fetcher).toHaveBeenCalledTimes(1);
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it('preserves data/resize order and suppresses invalid geometry', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    await session.connect();
    const socket = FakeSocket.instances[0];
    socket.open();
    session.sendData('a');
    session.resize(100, 30);
    session.sendData('b');
    session.resize(0, 0);
    session.resize(-1, 20);
    session.resize(NaN, 20);
    expect(socket.send.mock.calls.map(([data]) => JSON.parse(data) as unknown)).toEqual([
      { type: 'data', data: 'YQ==' },
      { type: 'resize', cols: 100, rows: 30 },
      { type: 'data', data: 'Yg==' },
    ]);
    expect(session.state.lastSize).toEqual({ cols: 100, rows: 30 });
  });

  it('notifies state changes and supports subscriber cancellation', async () => {
    const f = fixture();
    const session = f.registry.open(agentId, f.initialize);
    const listener = vi.fn<(state: TerminalSessionState) => void>();
    const unsubscribe = session.subscribe(listener);
    await session.connect();
    FakeSocket.instances[0].open();
    expect(listener.mock.calls.map(([state]) => state.connection)).toEqual([
      'loading',
      'loading',
      'connecting',
      'connected',
    ]);
    unsubscribe();
    listener.mockClear();
    session.close();
    expect(listener).not.toHaveBeenCalled();
  });
});

it('keeps prior screen/resources until a reconnect socket actually opens', async () => {
  const f = fixture();
  const session = f.registry.open(agentId, f.initialize);
  await session.connect();
  const initial = FakeSocket.instances[0];
  initial.open();
  initial.data([65]);
  initial.readyState = 3;
  initial.onclose?.({ code: 1006 });
  await session.connect();
  expect(f.resources.dispose).not.toHaveBeenCalled();
  expect(f.resources.reset).not.toHaveBeenCalled();
  FakeSocket.instances[1].onerror?.();
  expect(f.resources.reset).not.toHaveBeenCalled();
  expect(f.resources.dispose).not.toHaveBeenCalled();
  await session.connect();
  FakeSocket.instances[2].open();
  expect(f.resources.reset).toHaveBeenCalledTimes(1);
  expect(f.initialize).toHaveBeenCalledTimes(1);
  session.close();
  expect(f.resources.dispose).toHaveBeenCalledTimes(1);
});
