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
  // Access the channel's onmessage handler by finding the coordinator's channel
  const channel = FakeBroadcastChannel.instances.find(
    (ch) => ch.name === coordinator.coordinationKey
  );
  channel?.onmessage?.({ data });
}

function fixture(): {
  fetcher: ReturnType<typeof vi.fn>;
  resources: TerminalResources;
  initialize: ReturnType<typeof vi.fn>;
  adapter: TerminalCoordinatorAdapter;
  coordinator: TerminalCoordinator;
  selectCalls: TerminalSession[];
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
  vi.stubGlobal('navigator', {
    ...navigator,
    locks: {
      request: vi.fn(
        async (
          _name: string,
          _opts: unknown,
          callback: (lock: unknown) => Promise<void>
        ): Promise<void> => {
          await callback({ name: _name, mode: 'exclusive' });
        }
      ),
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
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('TerminalCoordinator.teardownAccount', () => {
  it('broadcasts account-teardown before stopping', () => {
    const f = fixture();

    // teardownAccount should broadcast and then stop
    f.coordinator.teardownAccount();

    // Verify a message was broadcast
    const teardownMsg = channelMessages.find(
      (msg) => (msg as { type: string }).type === 'account-teardown'
    );
    expect(teardownMsg).toBeDefined();
    expect((teardownMsg as { key: string }).key).toBe(f.coordinator.coordinationKey);

    // Verify coordinator is stopped
    expect(f.coordinator.tornDown).toBe(true);
  });

  it('is idempotent — calling twice does not throw or double-broadcast', () => {
    const f = fixture();

    f.coordinator.teardownAccount();
    const count = channelMessages.length;
    f.coordinator.teardownAccount(); // second call should be a no-op
    expect(channelMessages).toHaveLength(count);
    expect(f.coordinator.tornDown).toBe(true);
  });

  it('resolves pending open requests as stopped', async () => {
    const f = fixture();

    // The coordinator is not the owner, so open will send a discover message.
    const openPromise = f.coordinator.open(agentId, undefined, 5000);

    // Teardown before the open resolves
    f.coordinator.teardownAccount();

    const result = await openPromise;
    expect(result.status).toBe('stopped');
  });

  it('prevents new opens after teardown', async () => {
    const f = fixture();
    f.coordinator.teardownAccount();

    const result = await f.coordinator.open(agentId);
    expect(result.status).toBe('stopped');
  });

  it('handles account-teardown message from a peer', async () => {
    const f = fixture();
    const { ACCOUNT_TEARDOWN_EVENT } = await import('../utils/auth.js');
    const handler = vi.fn();
    window.addEventListener(ACCOUNT_TEARDOWN_EVENT, handler);

    try {
      // Simulate receiving an account-teardown message from another tab
      deliverToPeer(f.coordinator, {
        key: f.coordinator.coordinationKey,
        type: 'account-teardown',
        requestId: 'teardown',
        agentId: '',
        generation: null,
      });

      expect(f.coordinator.tornDown).toBe(true);
      // The receive handler should also dispatch the DOM event so that
      // main.ts can hide the workspace UI and set the torn-down guard.
      expect(handler).toHaveBeenCalledTimes(1);
      const event = handler.mock.calls[0][0] as CustomEvent<{ reason: string }>;
      expect(event.detail.reason).toBe('logout');
    } finally {
      window.removeEventListener(ACCOUNT_TEARDOWN_EVENT, handler);
    }
  });

  it('ignores teardown messages for a different coordination key', () => {
    const f = fixture();

    deliverToPeer(f.coordinator, {
      key: 'terminal-owner:v1:["https://other-hub.example/","other-account"]',
      type: 'account-teardown',
      requestId: 'teardown',
      agentId: '',
      generation: null,
    });

    // The coordinator should NOT be stopped
    expect(f.coordinator.tornDown).toBe(false);
  });
});

describe('TerminalSessionRegistry.dispose (teardown path)', () => {
  it('disposes all sessions and clears state', async () => {
    FakeSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close(): void {}
      }
    );
    vi.stubGlobal(
      'fetch',
      vi.fn((): Promise<Response> => Promise.resolve(json(agent)))
    );
    const resources = {
      write: vi.fn(),
      size: (): { cols: number; rows: number } => ({ cols: 80, rows: 24 }),
      dispose: vi.fn(),
      reset: vi.fn(),
    } satisfies TerminalResources;

    const { TerminalSessionRegistry } = await import('./terminal-sessions.js');
    const registry = new TerminalSessionRegistry(scope);
    const initialize = vi.fn<TerminalResourceInitializer>(() => Promise.resolve(resources));

    const session = registry.open(agentId, initialize);
    await session.connect();
    if (FakeSocket.instances.length > 0) FakeSocket.instances[0].open();

    // Dispose the registry
    registry.dispose();

    // All sessions should be closed
    expect(session.state.connection).toBe('closed');
    expect(registry.list()).toHaveLength(0);
    // Resources should be disposed
    expect(resources.dispose).toHaveBeenCalled();

    // Opening new sessions should throw
    expect(() => registry.open(agentId, initialize)).toThrow('disposed');
  });

  it('is idempotent — calling dispose twice does not throw', async () => {
    const { TerminalSessionRegistry } = await import('./terminal-sessions.js');
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        close(): void {}
      }
    );
    vi.stubGlobal(
      'fetch',
      vi.fn((): Promise<Response> => Promise.resolve(json(agent)))
    );
    const registry = new TerminalSessionRegistry(scope);
    registry.dispose();
    expect(() => registry.dispose()).not.toThrow();
  });
});

describe('Auth teardown event integration', () => {
  it('dispatchTeardown fires scion:account-teardown on window', async () => {
    const { dispatchTeardown, ACCOUNT_TEARDOWN_EVENT } = await import('../utils/auth.js');
    const handler = vi.fn();
    window.addEventListener(ACCOUNT_TEARDOWN_EVENT, handler);
    try {
      dispatchTeardown('logout');
      expect(handler).toHaveBeenCalledTimes(1);
      const event = handler.mock.calls[0][0] as CustomEvent<{ reason: string }>;
      expect(event.detail.reason).toBe('logout');
    } finally {
      window.removeEventListener(ACCOUNT_TEARDOWN_EVENT, handler);
    }
  });

  it('dispatchTeardown fires with auth-expired reason', async () => {
    const { dispatchTeardown, ACCOUNT_TEARDOWN_EVENT } = await import('../utils/auth.js');
    const handler = vi.fn();
    window.addEventListener(ACCOUNT_TEARDOWN_EVENT, handler);
    try {
      dispatchTeardown('auth-expired');
      expect(handler).toHaveBeenCalledTimes(1);
      const event = handler.mock.calls[0][0] as CustomEvent<{ reason: string }>;
      expect(event.detail.reason).toBe('auth-expired');
    } finally {
      window.removeEventListener(ACCOUNT_TEARDOWN_EVENT, handler);
    }
  });
});
