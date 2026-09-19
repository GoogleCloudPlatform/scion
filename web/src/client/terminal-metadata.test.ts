// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TerminalMetadata } from './terminal-metadata.js';
import { StateManager } from './state.js';

const id = (n: number) => `11111111-1111-4111-8111-${String(n).padStart(12, '0')}`;
const agent = (n = 1) => ({ id: id(n), name: `agent-${n}`, phase: 'running', activity: 'idle' });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
class Source extends EventTarget {
  static OPEN = 1;
  static CONNECTING = 0;
  static instances: Source[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  close = vi.fn(() => {
    this.readyState = 2;
  });
  constructor(readonly url: string) {
    super();
    Source.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
  update(n: number, kind: string, data: unknown) {
    this.dispatchEvent(
      new MessageEvent('update', {
        data: JSON.stringify({ subject: `agent.${id(n)}.${kind}`, data }),
      })
    );
  }
}
let metadata: TerminalMetadata;
let fetcher: ReturnType<typeof vi.fn<typeof fetch>>;
const flush = async () => {
  for (let i = 0; i < 12; i++) await Promise.resolve();
};
beforeEach(() => {
  Source.instances = [];
  vi.stubGlobal('EventSource', Source);
  fetcher = vi.fn<typeof fetch>().mockImplementation((url) => {
    const n = Number(String(url).slice(-12));
    return Promise.resolve(json(agent(n)));
  });
  vi.stubGlobal('fetch', fetcher);
  metadata = new TerminalMetadata('https://hub.example/team/');
});
afterEach(() => {
  metadata.dispose();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('workspace metadata', () => {
  it('coalesces a retained union independently of route scope and cleans up the last session', async () => {
    metadata.retain(id(1));
    metadata.retain(id(2));
    metadata.retain(id(3));
    metadata.release(id(3));
    await flush();
    expect(Source.instances).toHaveLength(1);
    const source = Source.instances[0];
    expect(new URL(source.url).searchParams.getAll('sub')).toEqual([
      `agent.${id(1)}.>`,
      `agent.${id(2)}.>`,
    ]);
    const route = new StateManager();
    route.setScope({ type: 'dashboard' });
    route.setScope({ type: 'chat', spaceIds: [], userId: 'user-1' });
    expect(source.close).not.toHaveBeenCalled();
    metadata.release(id(1));
    metadata.release(id(2));
    expect(source.close).toHaveBeenCalledTimes(1);
    await flush();
    expect(metadata.get(id(1))).toBeUndefined();
    route.disconnect();
  });

  it('waits for actual open and overlays events received while the snapshot is pending', async () => {
    const gate = deferred<Response>();
    fetcher.mockReturnValueOnce(gate.promise);
    metadata.retain(id(1));
    await flush();
    expect(fetcher).not.toHaveBeenCalled();
    const source = Source.instances[0];
    source.open();
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(1);
    source.update(1, 'status', { activity: 'executing' });
    source.update(1, 'ports', { ports: [{ port: 3000 }] });
    gate.resolve(json(agent()));
    await flush();
    expect(metadata.get(id(1))).toMatchObject({
      availability: 'ready',
      agent: { activity: 'executing', exposedPorts: [{ port: 3000 }] },
    });
  });

  it('reconciles missed events on reconnect with bounded concurrency, including queued-agent deltas', async () => {
    for (let n = 1; n <= 8; n++) metadata.retain(id(n));
    await flush();
    const source = Source.instances[0];
    source.open();
    await flush();
    const gates = Array.from({ length: 8 }, () => deferred<Response>());
    fetcher.mockReset();
    for (const gate of gates) fetcher.mockReturnValueOnce(gate.promise);
    source.dispatchEvent(new Event('reconnect'));
    const replacement = Source.instances[1];
    expect(fetcher).not.toHaveBeenCalled();
    replacement.open();
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(4);
    replacement.update(8, 'status', { activity: 'completed' });
    for (let i = 0; i < 4; i++) gates[i].resolve(json(agent(i + 1)));
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(8);
    for (let i = 4; i < 8; i++) gates[i].resolve(json({ ...agent(i + 1), activity: 'idle' }));
    await flush();
    expect(metadata.get(id(8))?.agent?.activity).toBe('completed');
  });

  it('preserves deletion over an in-flight snapshot and explicitly marks unavailable metadata', async () => {
    const gate = deferred<Response>();
    fetcher.mockReturnValueOnce(gate.promise).mockResolvedValueOnce(json({}, 403));
    metadata.retain(id(1));
    metadata.retain(id(2));
    await flush();
    const source = Source.instances[0];
    source.open();
    source.update(1, 'deleted', {});
    await flush();
    gate.resolve(json(agent()));
    await flush();
    expect(metadata.get(id(1))?.availability).toBe('deleted');
    expect(metadata.get(id(2))).toMatchObject({ availability: 'unavailable', agent: null });
  });

  it('batches long subject URLs without evicting any entries', async () => {
    for (let n = 1; n <= 100; n++) metadata.retain(id(n));
    await flush();
    expect(Source.instances.length).toBeGreaterThan(1);
    const subjects = Source.instances.flatMap((s) => new URL(s.url).searchParams.getAll('sub'));
    expect(new Set(subjects).size).toBe(100);
    expect(Source.instances.every((s) => s.url.length <= 2000)).toBe(true);
  });

  it('cancels stale fetches and callbacks on teardown and same-ID reopen', async () => {
    const gate = deferred<Response>();
    fetcher.mockReturnValueOnce(gate.promise);
    metadata.retain(id(1));
    await flush();
    const source = Source.instances[0];
    source.open();
    await flush();
    const signal = fetcher.mock.calls[0][1]?.signal;
    metadata.release(id(1));
    metadata.retain(id(1));
    await flush();
    expect(signal?.aborted).toBe(true);
    source.update(1, 'deleted', {});
    gate.resolve(json(agent()));
    await flush();
    expect(metadata.get(id(1))).toMatchObject({ availability: 'loading', agent: null });
    metadata.dispose();
    Source.instances.at(-1)!.open();
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});

it.each([403, 404])(
  'keeps a %s neighbor labeled while subscribing the accessible union',
  async (status) => {
    metadata.retain(id(1));
    metadata.retain(id(2));
    await flush();
    fetcher.mockImplementation((url) =>
      Promise.resolve(String(url).endsWith(id(1)) ? json({}, status) : json(agent(2)))
    );
    Source.instances[0].onerror?.();
    await flush();
    expect(metadata.get(id(1))?.availability).toBe(status === 404 ? 'deleted' : 'unavailable');
    expect(Source.instances).toHaveLength(2);
    expect(new URL(Source.instances[1].url).searchParams.getAll('sub')).toEqual([
      `agent.${id(2)}.>`,
    ]);
    expect(metadata.get(id(2))?.availability).not.toBe('ready');
    Source.instances[1].open();
    await flush();
    expect(metadata.get(id(2))?.availability).toBe('ready');
    fetcher.mockImplementation((url) =>
      Promise.resolve(json(agent(String(url).endsWith(id(1)) ? 1 : 2)))
    );
    const retry = metadata.refresh(id(1));
    await flush();
    expect(Source.instances).toHaveLength(3);
    Source.instances[2].open();
    await retry;
    expect(metadata.get(id(1))?.availability).toBe('ready');
  }
);

it('does not exclude transient failures or repeatedly diagnose the same rejected batch', async () => {
  metadata.retain(id(1));
  await flush();
  fetcher.mockImplementation((url) =>
    Promise.resolve(String(url).includes('/auth/me') ? json({}) : json({}, 503))
  );
  Source.instances[0].onerror?.();
  await flush();
  expect(metadata.get(id(1))?.availability).toBe('unavailable');
  expect(Source.instances).toHaveLength(1);
  expect(Source.instances[0].close).toHaveBeenCalled();
  // Explicit retry rebuilds the same subject, rather than evicting the entry.
  const retry = metadata.refresh(id(1));
  await flush();
  expect(new URL(Source.instances[1].url).searchParams.getAll('sub')).toEqual([`agent.${id(1)}.>`]);
  Source.instances[1].open();
  await retry;
  expect(metadata.get(id(1))?.availability).toBe('unavailable');
});

it('surfaces readable metadata with rejected SSE as an explicit retry state', async () => {
  metadata.retain(id(1));
  await flush();
  Source.instances[0].onerror?.();
  await flush();
  expect(metadata.get(id(1))?.error).toContain('subscription');
  expect(metadata.get(id(1))?.availability).toBe('unavailable');
  const count = fetcher.mock.calls.length;
  Source.instances[0].onerror?.();
  await flush();
  expect(fetcher).toHaveBeenCalledTimes(count);
  expect(metadata.get(id(1))).toBeDefined();
});

it('supersedes old refresh results and prevents transport seeds overwriting central updates', async () => {
  metadata.retain(id(1));
  await flush();
  Source.instances[0].open();
  await flush();
  const old = deferred<Response>();
  fetcher.mockReturnValueOnce(old.promise);
  const first = metadata.refresh(id(1));
  await flush();
  fetcher.mockResolvedValueOnce(json({ ...agent(), phase: 'stopped' }));
  const second = metadata.refresh(id(1));
  await second;
  Source.instances[0].update(1, 'ports', { ports: [{ port: 4000 }] });
  old.resolve(json(agent()));
  await first;
  await flush();
  metadata.seed(id(1), agent() as never);
  expect(metadata.get(id(1))?.agent).toMatchObject({
    phase: 'stopped',
    exposedPorts: [{ port: 4000 }],
  });
});

it('does not churn the live union for an open/close pair in one turn', async () => {
  metadata.retain(id(1));
  await flush();
  Source.instances[0].open();
  await flush();
  metadata.retain(id(2));
  metadata.release(id(2));
  await flush();
  expect(Source.instances).toHaveLength(1);
  expect(Source.instances[0].close).not.toHaveBeenCalled();
});

it('reports authentication expiry without excluding a subject or marking it deleted', async () => {
  metadata.retain(id(1));
  await flush();
  fetcher.mockImplementation((url) =>
    Promise.resolve(String(url).includes('auth/me') ? json({}) : json({}, 401))
  );
  Source.instances[0].onerror?.();
  await flush();
  expect(metadata.get(id(1))).toMatchObject({
    availability: 'unavailable',
    error: 'Authentication expired. Sign in again.',
  });
  expect(Source.instances).toHaveLength(1);
});

it('aborts bounded diagnostic requests on disposal without rebuilding after late responses', async () => {
  const gates = Array.from({ length: 6 }, () => deferred<Response>());
  fetcher.mockImplementation((url) =>
    String(url).includes('auth/me')
      ? Promise.resolve(json({}))
      : gates[Number(String(url).slice(-12)) - 1].promise
  );
  for (let n = 1; n <= 6; n++) metadata.retain(id(n));
  await flush();
  Source.instances[0].onerror?.();
  await flush();
  const calls = fetcher.mock.calls.filter(([url]) => !String(url).includes('auth/me'));
  expect(calls).toHaveLength(4);
  metadata.dispose();
  expect(calls.every(([, options]) => options?.signal?.aborted)).toBe(true);
  gates.forEach((gate) => gate.resolve(json({}, 403)));
  await flush();
  expect(Source.instances).toHaveLength(1);
  expect(metadata.get(id(1))).toBeUndefined();
});
