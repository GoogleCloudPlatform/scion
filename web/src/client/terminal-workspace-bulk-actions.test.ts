// @vitest-environment happy-dom
/**
 * Tests for the Open terminals bulk actions: "Reconnect all" and
 * "Remove all inactive". Covers which rows each action is eligible for,
 * the buttons' enabled state and hover help, and the confirmation step.
 */
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
  type MockInstance,
} from 'vitest';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import {
  TerminalSessionRegistry,
  type TerminalSession,
  type TerminalSessionState,
} from './terminal-sessions.js';
import type { TerminalAgentMetadata } from './terminal-metadata.js';

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    dispose = vi.fn();
    reset = vi.fn();
    write = vi.fn();
    textarea: HTMLTextAreaElement | null = null;
    focus = vi.fn();
    blur = vi.fn();
    refresh = vi.fn();
    parser = { registerOscHandler: vi.fn() };
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    attachCustomKeyEventHandler = vi.fn();
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

const confirmMock = vi.hoisted(() => ({
  showConfirm: vi.fn<(message: string, options?: unknown) => Promise<boolean>>(),
}));
vi.mock('../components/shared/confirm-dialog.js', () => confirmMock);

type Module = typeof import('./terminal-workspace-root.js');
let mod: Module;

beforeAll(async () => {
  await import('../components/terminal/terminal-pane.js');
  mod = await import('./terminal-workspace-root.js');
});

const CONNECTED = '11111111-1111-4111-8111-111111111111';
const DISCONNECTED = '22222222-2222-4222-8222-222222222222';
const DELETED = '33333333-3333-4333-8333-333333333333';
const STOPPED = '44444444-4444-4444-8444-444444444444';
const IDLE = '55555555-5555-4555-8555-555555555555';

/** Flushes the root's queueMicrotask-based refresh. */
async function flush(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

/**
 * Puts a session into a given connection state. Driving a real socket
 * through connect and close is covered by terminal-sessions.test.ts; the
 * bulk actions only read the resulting state, so the tests set it
 * directly through the session's own state update.
 */
function setState(session: TerminalSession, patch: Partial<TerminalSessionState>): void {
  (session as unknown as { update(p: Partial<TerminalSessionState>): void }).update(patch);
}

function status(
  connection: TerminalSessionState['connection'],
  disconnectReason: TerminalSessionState['disconnectReason'] = null,
  availability: TerminalAgentMetadata['availability'] = 'ready'
): { state: TerminalSessionState; metadata: TerminalAgentMetadata } {
  return {
    state: { connection, disconnectReason } as TerminalSessionState,
    metadata: { agent: null, availability, error: null },
  };
}

describe('bulk action eligibility rules', () => {
  it('Reconnect all applies only to dropped sessions whose agent still exists', () => {
    expect(mod.isBulkReconnectEligible(status('disconnected', 'network'))).toBe(true);
    expect(mod.isBulkReconnectEligible(status('unavailable', 'agent-stopped'))).toBe(true);
    expect(mod.isBulkReconnectEligible(status('connected'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('connecting'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('loading'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('idle'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('closed'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('unavailable', 'agent-deleted'))).toBe(false);
    // Metadata already knows the agent is gone, before the session does.
    expect(mod.isBulkReconnectEligible(status('disconnected', 'network', 'deleted'))).toBe(false);
  });

  it('Remove all inactive applies to deleted or dropped sessions only', () => {
    expect(mod.isInactiveEntry(status('disconnected', 'network'))).toBe(true);
    expect(mod.isInactiveEntry(status('unavailable', 'agent-stopped'))).toBe(true);
    expect(mod.isInactiveEntry(status('unavailable', 'agent-deleted'))).toBe(true);
    expect(mod.isInactiveEntry(status('disconnected', 'network', 'deleted'))).toBe(true);
    expect(mod.isInactiveEntry(status('connected'))).toBe(false);
    expect(mod.isInactiveEntry(status('connecting'))).toBe(false);
    expect(mod.isInactiveEntry(status('loading'))).toBe(false);
    expect(mod.isInactiveEntry(status('idle'))).toBe(false);
    expect(mod.isInactiveEntry(status('closed'))).toBe(false);
  });

  it('the row Reconnect rule matches the bulk rule except for metadata deletion', () => {
    expect(mod.canReconnectEntry(status('disconnected', 'network', 'deleted'))).toBe(true);
    expect(mod.canReconnectEntry(status('unavailable', 'agent-deleted'))).toBe(false);
    expect(mod.canReconnectEntry(status('idle'))).toBe(false);
  });
});

describe('Open terminals bulk actions', () => {
  let root: TerminalWorkspaceRoot;
  let registry: TerminalSessionRegistry;
  const sessions = new Map<string, TerminalSession>();
  const connectSpies = new Map<string, MockInstance<() => Promise<void>>>();
  let refreshSpy: MockInstance;

  beforeEach(() => {
    confirmMock.showConfirm.mockReset();
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
      }
    );
    registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'bulk',
    });
    root = new mod.TerminalWorkspaceRoot();
    document.body.append(root.element);
    sessions.clear();
    connectSpies.clear();
  });

  afterEach(() => {
    root.dispose();
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  /** Opens an idle (never connected) entry for each agent. */
  async function open(...agentIds: string[]): Promise<void> {
    root.withAutoSelectSuspended(() => {
      for (const id of agentIds) {
        const session = root.create(registry, id, { deferConnect: true });
        // No socket work in these tests: Reconnect is observed, not run.
        connectSpies.set(id, vi.spyOn(session, 'connect').mockResolvedValue());
        sessions.set(id, session);
      }
    });
    refreshSpy = vi.spyOn(registry.metadata, 'refresh').mockResolvedValue(undefined as never);
    await flush();
  }

  /** One row per state: connected, disconnected, deleted, stopped, idle. */
  async function openMixed(): Promise<void> {
    await open(CONNECTED, DISCONNECTED, DELETED, STOPPED, IDLE);
    setState(sessions.get(CONNECTED)!, { connection: 'connected', disconnectReason: null });
    setState(sessions.get(DISCONNECTED)!, {
      connection: 'disconnected',
      disconnectReason: 'network',
    });
    sessions.get(DELETED)!.markUnavailable('agent-deleted', 'Agent was deleted.');
    sessions.get(STOPPED)!.markUnavailable('agent-stopped', 'Agent is stopped.');
    await flush();
  }

  function bulkReconnect(): HTMLButtonElement {
    return root.element.querySelector<HTMLButtonElement>('.terminal-bulk-reconnect')!;
  }

  function bulkRemove(): HTMLButtonElement {
    return root.element.querySelector<HTMLButtonElement>('.terminal-bulk-remove')!;
  }

  function railAgentIds(): string[] {
    return [...root.element.querySelectorAll<HTMLElement>('.terminal-rail-select')].map(
      (el) => [...sessions].find(([, s]) => el.dataset.railFocusId === `${s.state.key}:select`)![0]
    );
  }

  it('the bar sits above the list and is hidden while the list is empty', async () => {
    const bar = root.element.querySelector<HTMLElement>('.terminal-rail-bulk')!;
    expect(bar.hidden).toBe(true);
    await open(CONNECTED);
    expect(bar.hidden).toBe(false);
    expect(bar.nextElementSibling?.classList.contains('terminal-rail-list')).toBe(true);
    // Reconnect all, then Remove all inactive: the same order as the row
    // Reconnect and Close columns beneath them.
    const buttons = [...bar.querySelectorAll('button')];
    expect(buttons).toEqual([bulkReconnect(), bulkRemove()]);
  });

  it('both buttons are disabled when nothing is eligible', async () => {
    await open(CONNECTED, IDLE);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    await flush();
    expect(bulkReconnect().disabled).toBe(true);
    expect(bulkRemove().disabled).toBe(true);
    expect(bulkReconnect().title).toContain('no disconnected terminals');
    expect(bulkRemove().title).toContain('no deleted or disconnected terminals');
  });

  it('with mixed rows, both buttons are enabled with hover help naming the count', async () => {
    await openMixed();
    expect(bulkReconnect().disabled).toBe(false);
    expect(bulkRemove().disabled).toBe(false);
    // Disconnected and stopped are reconnectable; deleted is not.
    expect(bulkReconnect().title).toContain('reconnect 2 terminals');
    expect(bulkReconnect().title).toContain('terminals for deleted agents are left alone');
    // Disconnected, stopped and deleted are inactive.
    expect(bulkRemove().title).toContain('remove 3 terminals');
    expect(bulkRemove().title).toContain('Connected terminals stay open');
  });

  it('Reconnect all is disabled when the only dropped rows are deleted', async () => {
    await open(CONNECTED, DELETED);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    sessions.get(DELETED)!.markUnavailable('agent-deleted', 'Agent was deleted.');
    await flush();
    expect(bulkReconnect().disabled).toBe(true);
    expect(bulkRemove().disabled).toBe(false);
  });

  it('Reconnect all reconnects only disconnected rows whose agent exists', async () => {
    await openMixed();
    bulkReconnect().click();
    expect(connectSpies.get(DISCONNECTED)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(STOPPED)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(CONNECTED)).not.toHaveBeenCalled();
    expect(connectSpies.get(DELETED)).not.toHaveBeenCalled();
    expect(connectSpies.get(IDLE)).not.toHaveBeenCalled();
    // Same path as the row Reconnect: metadata is refreshed for each.
    expect(refreshSpy).toHaveBeenCalledWith(DISCONNECTED);
    expect(refreshSpy).toHaveBeenCalledWith(STOPPED);
    expect(refreshSpy).toHaveBeenCalledTimes(2);
  });

  it('Remove all inactive confirms with the count, then removes only inactive rows', async () => {
    await openMixed();
    confirmMock.showConfirm.mockResolvedValue(true);
    const removed = await root.removeAllInactive();

    expect(confirmMock.showConfirm).toHaveBeenCalledTimes(1);
    const [message, options] = confirmMock.showConfirm.mock.calls[0];
    expect(message).toMatch(/^Remove 3 terminals from the list\?/);
    expect(options).toMatchObject({ confirmText: 'Remove 3' });
    expect(removed).toBe(3);
    await flush();
    expect(railAgentIds().sort()).toEqual([CONNECTED, IDLE].sort());
    expect(bulkRemove().disabled).toBe(true);
    expect(bulkReconnect().disabled).toBe(true);
  });

  it('the button click runs the same confirmation flow', async () => {
    await openMixed();
    confirmMock.showConfirm.mockResolvedValue(true);
    bulkRemove().click();
    await vi.waitFor(() => expect(railAgentIds()).toHaveLength(2));
    expect(confirmMock.showConfirm).toHaveBeenCalledTimes(1);
  });

  it('cancelling the confirmation leaves the list unchanged', async () => {
    await openMixed();
    const before = railAgentIds();
    confirmMock.showConfirm.mockResolvedValue(false);
    const removed = await root.removeAllInactive();
    await flush();
    expect(removed).toBe(0);
    expect(railAgentIds()).toEqual(before);
    expect(railAgentIds()).toHaveLength(5);
    expect(bulkRemove().disabled).toBe(false);
  });

  it('a row that reconnects while the dialog is open is kept', async () => {
    await openMixed();
    let answer!: (value: boolean) => void;
    confirmMock.showConfirm.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    const pending = root.removeAllInactive();
    setState(sessions.get(DISCONNECTED)!, { connection: 'connected', disconnectReason: null });
    answer(true);
    expect(await pending).toBe(2);
    await flush();
    expect(railAgentIds().sort()).toEqual([CONNECTED, DISCONNECTED, IDLE].sort());
  });

  /** Mimics the dialog returning focus to its trigger before it resolves. */
  function confirmReturningFocusTo(trigger: HTMLElement): void {
    confirmMock.showConfirm.mockImplementation(() => {
      trigger.focus();
      return Promise.resolve(true);
    });
  }

  it('after removing, focus moves from the disabled button to the first remaining row', async () => {
    await openMixed();
    confirmReturningFocusTo(bulkRemove());
    await root.removeAllInactive();
    expect(bulkRemove().disabled).toBe(true);
    const focused = document.activeElement as HTMLElement;
    expect(focused).not.toBe(document.body);
    expect(focused.classList.contains('terminal-rail-select')).toBe(true);
    expect(focused).toBe(root.element.querySelector('.terminal-rail-select'));
  });

  it('after removing every row, focus moves to the terminal list panel', async () => {
    await open(DISCONNECTED, DELETED);
    setState(sessions.get(DISCONNECTED)!, {
      connection: 'disconnected',
      disconnectReason: 'network',
    });
    sessions.get(DELETED)!.markUnavailable('agent-deleted', 'Agent was deleted.');
    await flush();
    confirmReturningFocusTo(bulkRemove());
    expect(await root.removeAllInactive()).toBe(2);
    expect(railAgentIds()).toEqual([]);
    expect(document.activeElement).toBe(root.element.querySelector('.terminal-rail'));
  });

  it('after removing, focus the user moved elsewhere is left alone', async () => {
    await openMixed();
    const elsewhere = document.createElement('button');
    document.body.append(elsewhere);
    confirmReturningFocusTo(elsewhere);
    await root.removeAllInactive();
    expect(document.activeElement).toBe(elsewhere);
    elsewhere.remove();
  });

  it('the row Close button still removes a single row', async () => {
    await openMixed();
    const key = sessions.get(DISCONNECTED)!.state.key;
    [...root.element.querySelectorAll<HTMLButtonElement>('.terminal-rail-actions button')]
      .find((el) => el.dataset.railFocusId === `${key}:close`)!
      .click();
    await flush();
    expect(railAgentIds()).not.toContain(DISCONNECTED);
    expect(railAgentIds()).toHaveLength(4);
    expect(confirmMock.showConfirm).not.toHaveBeenCalled();
  });
});
