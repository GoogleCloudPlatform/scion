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
import type { Agent, AgentPhase } from '../shared/types.js';

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
  availability: TerminalAgentMetadata['availability'] = 'ready',
  phase?: AgentPhase
): { state: TerminalSessionState; metadata: TerminalAgentMetadata } {
  return {
    state: { connection, disconnectReason } as TerminalSessionState,
    metadata: {
      agent: phase ? ({ id: 'a', phase } as Agent) : null,
      availability,
      error: null,
    },
  };
}

describe('bulk action eligibility rules', () => {
  it('Reconnect all applies only to dropped sessions whose agent still exists', () => {
    expect(mod.isBulkReconnectEligible(status('disconnected', 'network'))).toBe(true);
    expect(mod.isBulkReconnectEligible(status('unavailable', 'agent-stopped'))).toBe(true);
    expect(mod.isBulkReconnectEligible(status('connected'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('connecting'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('loading'))).toBe(false);
    // Restored from the saved list and shown as "Not connected".
    expect(mod.isBulkReconnectEligible(status('idle'))).toBe(true);
    expect(mod.isBulkReconnectEligible(status('idle', null, 'deleted'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('closed'))).toBe(false);
    expect(mod.isBulkReconnectEligible(status('unavailable', 'agent-deleted'))).toBe(false);
    // Metadata already knows the agent is gone, before the session does.
    expect(mod.isBulkReconnectEligible(status('disconnected', 'network', 'deleted'))).toBe(false);
  });

  it('Remove all inactive applies to deleted, dropped or gone idle sessions only', () => {
    expect(mod.isInactiveEntry(status('disconnected', 'network'))).toBe(true);
    expect(mod.isInactiveEntry(status('unavailable', 'agent-stopped'))).toBe(true);
    expect(mod.isInactiveEntry(status('unavailable', 'agent-deleted'))).toBe(true);
    expect(mod.isInactiveEntry(status('disconnected', 'network', 'deleted'))).toBe(true);
    expect(mod.isInactiveEntry(status('connected'))).toBe(false);
    expect(mod.isInactiveEntry(status('connecting'))).toBe(false);
    expect(mod.isInactiveEntry(status('loading'))).toBe(false);
    expect(mod.isInactiveEntry(status('closed'))).toBe(false);
  });

  it('Remove all inactive counts an idle row only when its agent is gone', () => {
    // Restored rows for live agents stay: after a reload most rows are idle.
    expect(mod.isInactiveEntry(status('idle', null, 'ready', 'running'))).toBe(false);
    expect(mod.isInactiveEntry(status('idle', null, 'ready'))).toBe(false);
    expect(mod.isInactiveEntry(status('idle', null, 'loading'))).toBe(false);
    expect(mod.isInactiveEntry(status('idle', null, 'ready', 'starting'))).toBe(false);
    expect(mod.isInactiveEntry(status('idle', null, 'ready', 'suspended'))).toBe(false);
    // Metadata says the agent is gone.
    expect(mod.isInactiveEntry(status('idle', null, 'ready', 'stopped'))).toBe(true);
    expect(mod.isInactiveEntry(status('idle', null, 'ready', 'error'))).toBe(true);
    expect(mod.isInactiveEntry(status('idle', null, 'deleted'))).toBe(true);
    expect(mod.isInactiveEntry(status('idle', null, 'unavailable'))).toBe(true);
  });

  it('the row Reconnect rule matches the bulk rule except for metadata deletion and idle', () => {
    expect(mod.canReconnectEntry(status('disconnected', 'network', 'deleted'))).toBe(true);
    expect(mod.canReconnectEntry(status('unavailable', 'agent-deleted'))).toBe(false);
    // Idle rows connect through selection; only the bulk action covers them.
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

  /**
   * Publishes agent metadata as the hub snapshot or SSE would. Like
   * setState, this goes through the store's own publish step.
   */
  function setMetadata(
    agentId: string,
    availability: TerminalAgentMetadata['availability'],
    phase?: AgentPhase
  ): void {
    const store = registry.metadata as unknown as {
      entries: Map<string, unknown>;
      publish(entry: unknown, value: TerminalAgentMetadata): void;
    };
    store.publish(store.entries.get(agentId), {
      agent: phase ? ({ id: agentId, name: agentId, phase } as Agent) : null,
      availability,
      error: null,
    });
  }

  /**
   * One row per state: connected, disconnected, deleted, stopped, and idle
   * (restored, not yet opened) for an agent that is still running.
   */
  async function openMixed(): Promise<void> {
    await open(CONNECTED, DISCONNECTED, DELETED, STOPPED, IDLE);
    setState(sessions.get(CONNECTED)!, { connection: 'connected', disconnectReason: null });
    setState(sessions.get(DISCONNECTED)!, {
      connection: 'disconnected',
      disconnectReason: 'network',
    });
    sessions.get(DELETED)!.markUnavailable('agent-deleted', 'Agent was deleted.');
    sessions.get(STOPPED)!.markUnavailable('agent-stopped', 'Agent is stopped.');
    setMetadata(IDLE, 'ready', 'running');
    await flush();
  }

  function bulkReconnect(): HTMLButtonElement {
    return root.element.querySelector<HTMLButtonElement>('.terminal-bulk-reconnect')!;
  }

  function bulkRemove(): HTMLButtonElement {
    return root.element.querySelector<HTMLButtonElement>('.terminal-bulk-remove')!;
  }

  /** The bulk buttons use aria-disabled so they stay focusable. */
  function isDisabled(button: HTMLButtonElement): boolean {
    return button.getAttribute('aria-disabled') === 'true';
  }

  function tooltipFor(button: HTMLButtonElement): HTMLElement {
    return button.closest<HTMLElement>('sl-tooltip')!;
  }

  /** The text assistive tech gets from the button's aria-describedby. */
  function describedBy(button: HTMLButtonElement): string {
    const ids = (button.getAttribute('aria-describedby') ?? '').split(/\s+/).filter(Boolean);
    return ids
      .map((id) => document.getElementById(id))
      .filter((el): el is HTMLElement => !!el && !el.hidden)
      .map((el) => el.textContent ?? '')
      .join(' ')
      .trim();
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
    await open(CONNECTED);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    await flush();
    expect(isDisabled(bulkReconnect())).toBe(true);
    expect(isDisabled(bulkRemove())).toBe(true);
    expect(bulkReconnect().getAttribute('aria-label')).toBe('Reconnect all (0 eligible)');
    expect(bulkRemove().getAttribute('aria-label')).toBe('Remove all inactive (0 eligible)');
  });

  describe('disabled-reason tooltip', () => {
    beforeEach(async () => {
      await open(CONNECTED);
      setState(sessions.get(CONNECTED)!, { connection: 'connected' });
      await flush();
    });

    it('Reconnect all explains there is nothing to reconnect', () => {
      const tip = tooltipFor(bulkReconnect());
      expect(tip.getAttribute('content')).toBe('No disconnected terminals to reconnect');
      expect(tip.hasAttribute('disabled')).toBe(false);
      // No second, native tooltip on top of the Shoelace one.
      expect(bulkReconnect().hasAttribute('title')).toBe(false);
    });

    it('Remove all inactive explains there is nothing to remove', () => {
      const tip = tooltipFor(bulkRemove());
      expect(tip.getAttribute('content')).toBe('No inactive terminals to remove');
      expect(tip.hasAttribute('disabled')).toBe(false);
      expect(bulkRemove().hasAttribute('title')).toBe(false);
    });

    it('the tooltip wraps a span, not the button itself', () => {
      for (const button of [bulkReconnect(), bulkRemove()]) {
        const wrap = button.parentElement!;
        expect(wrap.tagName).toBe('SPAN');
        expect(wrap.parentElement).toBe(tooltipFor(button));
      }
    });

    it('the reason reaches assistive tech through aria-describedby', () => {
      expect(describedBy(bulkReconnect())).toBe('No disconnected terminals to reconnect');
      expect(describedBy(bulkRemove())).toBe('No inactive terminals to remove');
    });

    it('a disabled button stays keyboard focusable so the reason can be reached', () => {
      for (const button of [bulkReconnect(), bulkRemove()]) {
        expect(button.disabled).toBe(false);
        button.focus();
        expect(document.activeElement).toBe(button);
      }
    });

    it('clicking a disabled button does nothing', async () => {
      bulkReconnect().click();
      bulkRemove().click();
      await flush();
      expect(connectSpies.get(CONNECTED)).not.toHaveBeenCalled();
      expect(confirmMock.showConfirm).not.toHaveBeenCalled();
    });
  });

  it('enabled buttons have no disabled-reason tooltip or description', async () => {
    await openMixed();
    for (const button of [bulkReconnect(), bulkRemove()]) {
      expect(isDisabled(button)).toBe(false);
      expect(tooltipFor(button).hasAttribute('disabled')).toBe(true);
      expect(tooltipFor(button).hasAttribute('content')).toBe(false);
      expect(describedBy(button)).toBe('');
    }
  });

  it('with mixed rows, both buttons are enabled with hover help naming the count', async () => {
    await openMixed();
    expect(isDisabled(bulkReconnect())).toBe(false);
    expect(isDisabled(bulkRemove())).toBe(false);
    // Disconnected, stopped and idle are reconnectable; deleted is not.
    expect(bulkReconnect().title).toContain('reconnect 3 terminals');
    expect(bulkReconnect().title).toContain('terminals for deleted agents are left alone');
    expect(bulkReconnect().getAttribute('aria-label')).toBe('Reconnect all (3 eligible)');
    // Disconnected, deleted and stopped are inactive; the connected row and
    // the idle row for a running agent are not.
    expect(bulkRemove().title).toContain('remove 3 terminals');
    expect(bulkRemove().title).toContain('Connected terminals and not yet opened terminals');
    expect(bulkRemove().getAttribute('aria-label')).toBe('Remove all inactive (3 eligible)');
  });

  it('rows restored from the saved list enable both buttons', async () => {
    // How the list looks after a page load: the frontmost row connects,
    // every other row is restored idle and shown as "Not connected".
    // Here one idle row's agent has stopped and the other is running.
    await open(CONNECTED, DISCONNECTED, IDLE);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    setMetadata(DISCONNECTED, 'ready', 'stopped');
    setMetadata(IDLE, 'ready', 'running');
    await flush();
    expect(isDisabled(bulkReconnect())).toBe(false);
    expect(isDisabled(bulkRemove())).toBe(false);
    expect(bulkReconnect().getAttribute('aria-label')).toBe('Reconnect all (2 eligible)');
    // Only the idle row whose agent stopped is inactive.
    expect(bulkRemove().getAttribute('aria-label')).toBe('Remove all inactive (1 eligible)');
    bulkReconnect().click();
    expect(connectSpies.get(DISCONNECTED)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(IDLE)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(CONNECTED)).not.toHaveBeenCalled();
  });

  it('a row that drops after the first render enables both buttons', async () => {
    await open(CONNECTED);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    await flush();
    expect(isDisabled(bulkReconnect())).toBe(true);
    expect(isDisabled(bulkRemove())).toBe(true);
    setState(sessions.get(CONNECTED)!, { connection: 'disconnected', disconnectReason: 'network' });
    await flush();
    expect(isDisabled(bulkReconnect())).toBe(false);
    expect(isDisabled(bulkRemove())).toBe(false);
    expect(tooltipFor(bulkReconnect()).hasAttribute('disabled')).toBe(true);
    bulkReconnect().click();
    expect(connectSpies.get(CONNECTED)).toHaveBeenCalledTimes(1);
  });

  it('Remove all inactive leaves idle rows for running agents alone', async () => {
    // After a reload: the frontmost row connects, the rest are idle and
    // their agents are running.
    await open(CONNECTED, IDLE, STOPPED);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    setMetadata(IDLE, 'ready', 'running');
    setMetadata(STOPPED, 'ready', 'running');
    await flush();
    expect(isDisabled(bulkRemove())).toBe(true);
    expect(bulkRemove().getAttribute('aria-label')).toBe('Remove all inactive (0 eligible)');
    expect(isDisabled(bulkReconnect())).toBe(false);
    expect(await root.removeAllInactive()).toBe(0);
    expect(confirmMock.showConfirm).not.toHaveBeenCalled();
    expect(railAgentIds()).toHaveLength(3);
  });

  it('Remove all inactive removes idle rows whose agent is gone', async () => {
    const ERRORED = '66666666-6666-4666-8666-666666666666';
    const UNAVAILABLE = '77777777-7777-4777-8777-777777777777';
    await open(CONNECTED, IDLE, STOPPED, ERRORED, DELETED, UNAVAILABLE);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    setMetadata(IDLE, 'ready', 'running');
    setMetadata(STOPPED, 'ready', 'stopped');
    setMetadata(ERRORED, 'ready', 'error');
    setMetadata(DELETED, 'deleted');
    setMetadata(UNAVAILABLE, 'unavailable');
    await flush();
    // The deleted row is closed out by the metadata bridge or stays idle;
    // either way it must not be left behind.
    expect(bulkRemove().getAttribute('aria-label')).toBe('Remove all inactive (4 eligible)');
    confirmMock.showConfirm.mockResolvedValue(true);
    expect(await root.removeAllInactive()).toBe(4);
    await flush();
    expect(railAgentIds().sort()).toEqual([CONNECTED, IDLE].sort());
  });

  it('Reconnect all is disabled when the only dropped rows are deleted', async () => {
    await open(CONNECTED, DELETED);
    setState(sessions.get(CONNECTED)!, { connection: 'connected' });
    sessions.get(DELETED)!.markUnavailable('agent-deleted', 'Agent was deleted.');
    await flush();
    expect(isDisabled(bulkReconnect())).toBe(true);
    expect(isDisabled(bulkRemove())).toBe(false);
  });

  it('Reconnect all reconnects only not connected rows whose agent exists', async () => {
    await openMixed();
    bulkReconnect().click();
    expect(connectSpies.get(DISCONNECTED)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(STOPPED)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(IDLE)).toHaveBeenCalledTimes(1);
    expect(connectSpies.get(CONNECTED)).not.toHaveBeenCalled();
    expect(connectSpies.get(DELETED)).not.toHaveBeenCalled();
    // Same path as the row Reconnect: metadata is refreshed for each.
    expect(refreshSpy).toHaveBeenCalledWith(DISCONNECTED);
    expect(refreshSpy).toHaveBeenCalledWith(STOPPED);
    expect(refreshSpy).toHaveBeenCalledWith(IDLE);
    expect(refreshSpy).toHaveBeenCalledTimes(3);
  });

  it('Remove all inactive confirms with the count, then removes only inactive rows', async () => {
    await openMixed();
    confirmMock.showConfirm.mockResolvedValue(true);
    const removed = await root.removeAllInactive();

    expect(confirmMock.showConfirm).toHaveBeenCalledTimes(1);
    const [message, options] = confirmMock.showConfirm.mock.calls[0];
    expect(message).toMatch(/^Remove 3 terminals from the list\?/);
    expect(message).toContain('not yet opened terminals for running agents stay');
    expect(options).toMatchObject({ confirmText: 'Remove 3' });
    expect(removed).toBe(3);
    await flush();
    expect(railAgentIds().sort()).toEqual([CONNECTED, IDLE].sort());
    expect(isDisabled(bulkRemove())).toBe(true);
    // The idle row for the running agent can still be reconnected.
    expect(isDisabled(bulkReconnect())).toBe(false);
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
    expect(isDisabled(bulkRemove())).toBe(false);
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

  it('a row that drops while the dialog is open is not removed', async () => {
    await openMixed();
    let answer!: (value: boolean) => void;
    confirmMock.showConfirm.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    const pending = root.removeAllInactive();
    const [, options] = confirmMock.showConfirm.mock.calls[0];
    expect(options).toMatchObject({ confirmText: 'Remove 3' });
    setState(sessions.get(CONNECTED)!, {
      connection: 'disconnected',
      disconnectReason: 'network',
    });
    answer(true);
    const removed = await pending;
    expect(removed).toBe(3);
    await flush();
    expect(railAgentIds().sort()).toEqual([CONNECTED, IDLE].sort());
  });

  /** Mimics the dialog returning focus to its trigger before it resolves. */
  function confirmReturningFocusTo(trigger: HTMLElement): void {
    confirmMock.showConfirm.mockImplementation(() => {
      trigger.focus();
      return Promise.resolve(true);
    });
  }

  it('after removing, focus moves to Reconnect all while it has rows to act on', async () => {
    // The idle row for the running agent stays and is still reconnectable.
    await openMixed();
    confirmReturningFocusTo(bulkRemove());
    await root.removeAllInactive();
    expect(isDisabled(bulkRemove())).toBe(true);
    expect(document.activeElement).toBe(bulkReconnect());
  });

  it('after removing, focus moves from the disabled button to the first remaining row', async () => {
    await openMixed();
    // The idle row's agent stopped, so it is removed too and nothing is
    // left to reconnect.
    setMetadata(IDLE, 'ready', 'stopped');
    await flush();
    confirmReturningFocusTo(bulkRemove());
    await root.removeAllInactive();
    expect(isDisabled(bulkRemove())).toBe(true);
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
