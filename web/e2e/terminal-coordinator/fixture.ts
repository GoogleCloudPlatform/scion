import { TerminalCoordinator } from '../../src/client/terminal-coordinator.js';
import type { TerminalScope, TerminalSession } from '../../src/client/terminal-sessions.js';

// Configuration comes from the test runner, never route parameters.
let coordinator: TerminalCoordinator;
let selections = 0;
let initialized = 0;
let disposed = 0;
let throwingDisposer = -1;
const disposalAttempts: number[] = [];
let capturedSessions: readonly TerminalSession[] = [];
let selectionGate: Promise<void> | null = null;
let releaseSelection: (() => void) | null = null;
let focusAttempts = 0;
const fixture = {
  start(scope: TerminalScope, deny = false) {
    coordinator = new TerminalCoordinator(scope, {
      initialize: () => {
        const index = initialized++;
        return Promise.resolve({
          write() {},
          size: () => ({ cols: 80, rows: 24 }),
          reset() {},
          dispose() {
            disposalAttempts.push(index);
            disposed++;
            if (index === throwingDisposer) throw new Error('Renderer disposal failed');
          },
        });
      },
      select: async (session, signal) => {
        if (selectionGate) await selectionGate;
        signal.throwIfAborted();
        selections++;
        document.querySelector('main')!.textContent = `Terminal ${session.state.agentId}`;
      },
      ...(deny
        ? {
            focus: () => {
              focusAttempts++;
              return Promise.resolve('not-confirmed' as const);
            },
          }
        : {}),
    });
  },
  open(agentId: string, requestId?: string, timeoutMs = 1000) {
    return coordinator.open(agentId, requestId, timeoutMs);
  },
  snapshot() {
    return {
      owner: coordinator.isOwner,
      supported: coordinator.supported,
      generation: coordinator.generation,
      coordinationKey: coordinator.coordinationKey,
      selections,
      initialized,
      disposed,
      focusAttempts,
      sessions: coordinator.sessions.map((s) => s.state),
    };
  },
  mode(mode: string) {
    document.querySelector('main')!.textContent = mode;
  },
  blockSelection() {
    selectionGate = new Promise<void>((resolve) => {
      releaseSelection = resolve;
    });
  },
  releaseSelection() {
    releaseSelection?.();
    selectionGate = null;
  },
  throwOnDispose(index: number) {
    throwingDisposer = index;
  },
  captureSessions() {
    capturedSessions = coordinator.sessions;
  },
  teardownState() {
    return {
      disposalAttempts: [...disposalAttempts],
      sends: capturedSessions.map((session) => session.sendData('teardown probe')),
      connections: capturedSessions.map((session) => session.state.connection),
    };
  },
  stop() {
    coordinator.stop();
  },
};
declare global {
  interface Window {
    fixture: typeof fixture;
  }
}
window.fixture = fixture;
