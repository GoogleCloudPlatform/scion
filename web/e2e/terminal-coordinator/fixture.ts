import { TerminalCoordinator } from '../../src/client/terminal-coordinator.js';
import type { TerminalScope } from '../../src/client/terminal-sessions.js';

// Configuration comes from the test runner, never route parameters.
let coordinator: TerminalCoordinator;
let selections = 0;
let initialized = 0;
let disposed = 0;
let selectionGate: Promise<void> | null = null;
let releaseSelection: (() => void) | null = null;
let focusAttempts = 0;
const fixture = {
  start(scope: TerminalScope, deny = false) {
    coordinator = new TerminalCoordinator(scope, {
      initialize: () => {
        initialized++;
        return Promise.resolve({
          write() {},
          size: () => ({ cols: 80, rows: 24 }),
          reset() {},
          dispose() {
            disposed++;
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
  open(agentId: string, requestId: string, timeoutMs = 1000) {
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
