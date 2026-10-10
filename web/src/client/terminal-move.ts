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

import type { TerminalCoordinator } from './terminal-coordinator.js';
import type { TerminalWorkspacePersistence } from './terminal-persistence.js';
import type { TerminalSession, TerminalSessionState } from './terminal-sessions.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { MOVING_TERMINALS_LABEL } from './terminal-palette-open.js';

/** The parts of the coordinator, persistence and workspace a move uses. */
export type MoveCoordinator = Pick<
  TerminalCoordinator,
  'supported' | 'isOwner' | 'openForMove' | 'restoreEntries' | 'requestRelease' | 'awaitOwnership'
>;
export type MovePersistence = Pick<TerminalWorkspacePersistence, 'readSavedList' | 'restore'>;
export type MoveWorkspace = Pick<TerminalWorkspaceRoot, 'withAutoSelectSuspended' | 'select'>;

/**
 * How many times a move asks for ownership: once, plus re-asks when another
 * window that was already waiting for the Web Lock got it first (the lock
 * queue is first come, first served).
 */
export const MOVE_OWNERSHIP_ATTEMPTS = 3;

export interface MoveTerminalsOptions {
  coordinator: MoveCoordinator;
  persistence: MovePersistence;
  workspace: MoveWorkspace;
  /** The agent this window asked for (its URL), shown first when it is in the saved list. */
  preferredAgentId: string | null;
}

export type MoveTerminalsResult =
  /** agentId is null when this window owns and the saved list is empty. */
  | { readonly status: 'moved'; readonly agentId: string | null }
  | { readonly status: 'failed'; readonly message: string };

/**
 * Moves the terminals from the window that owns them to this one
 * (ptone/scion#3328). Open before close:
 *
 * 1. Read the saved list of open terminals.
 * 2. Open this window's own entries for exactly that list. The shown
 *    terminal connects, authorizing on its own; the rest stay idle, as
 *    after a restore. Nothing is handed over from the other window.
 * 3. Once it is connected, send a resize with this window's size.
 * 4. Only then ask the owner to close its streams, and wait for the Web
 *    Lock to reach this window after the owner releases it. If another
 *    waiting window gets the lock first, ask that window too, up to
 *    MOVE_OWNERSHIP_ATTEMPTS times in all.
 *
 * Whichever window ends up holding the lock keeps its streams. If this
 * window does not get it (sign-in, authorization, one of the session's own
 * connect timeouts, or no release), its entries are closed and the owner
 * keeps its streams and the lock. If the owner went away mid-move and this
 * window got the lock anyway, the move completes.
 *
 * In a window that already owns (the other window closed after a move, or
 * after a failed one), the move just opens the saved list here and shows
 * the preferred terminal, so Move and Retry are never a dead end. Never
 * rejects.
 */
export async function moveTerminalsHere(
  options: MoveTerminalsOptions
): Promise<MoveTerminalsResult> {
  const { coordinator, persistence, workspace } = options;
  const failed = (message: string): MoveTerminalsResult => ({ status: 'failed', message });
  if (!coordinator.supported) return failed('Terminal workspace is unavailable in this tab.');

  const saved = await persistence.readSavedList();
  if (!saved) return failed('The saved list of open terminals could not be read.');
  if (saved.agentIds.length === 0) {
    if (!coordinator.isOwner) return failed('No terminals are open.');
    // Owning with nothing saved is the ordinary empty viewer, not a failure.
    await persistence.restore(true);
    return { status: 'moved', agentId: null };
  }
  const preferred = options.preferredAgentId?.toLowerCase() ?? null;
  const connectId =
    preferred && saved.agentIds.includes(preferred)
      ? preferred
      : (saved.frontmostAgentId ?? saved.agentIds[saved.agentIds.length - 1]);

  if (coordinator.isOwner) {
    // Nothing to take over: open the saved list as a restore would, with
    // the preferred terminal connected and shown. A connect failure from
    // here on shows in that terminal's own pane, with its Reconnect.
    const restored = workspace.withAutoSelectSuspended(() =>
      coordinator.restoreEntries(saved.agentIds, { connectAgentId: connectId })
    );
    const target = restored.find((session) => session.state.agentId === connectId);
    if (!target) return failed('Terminals could not be opened in this window.');
    workspace.select(target);
    await persistence.restore(true);
    return { status: 'moved', agentId: connectId };
  }

  let sessions: readonly TerminalSession[] = [];
  const discard = (message: string): MoveTerminalsResult => {
    for (const session of sessions) {
      try {
        session.close('navigation');
      } catch {
        /* best effort: the entry is already leaving the registry */
      }
    }
    return failed(message);
  };
  try {
    sessions = workspace.withAutoSelectSuspended(() =>
      coordinator.openForMove(saved.agentIds, { connectAgentId: connectId })
    );
    const target = sessions.find((session) => session.state.agentId === connectId);
    if (!target) return discard('Terminals could not be opened in this window.');
    workspace.select(target);

    const state = await settled(target);
    if (state.connection !== 'connected')
      return discard(state.error ?? 'The terminal could not connect in this window.');
    const size = target.state.lastSize;
    if (size) target.resize(size.cols, size.rows);

    let outcome = await askForOwnership(coordinator, connectId);
    // Re-ask only when an owner released and the lock went to another
    // waiting window; an owner that never answered is not asked again.
    for (
      let attempt = 1;
      outcome === 'lock-elsewhere' && attempt < MOVE_OWNERSHIP_ATTEMPTS;
      attempt++
    )
      outcome = await askForOwnership(coordinator, connectId);
    if (outcome === 'no-answer') return discard('The window with the terminals did not respond.');
    if (outcome === 'lock-elsewhere')
      return discard('Another window took the terminals before this one.');
  } catch (error) {
    return discard(error instanceof Error ? error.message : 'Terminals could not be moved.');
  }
  // Saving resumes for this window's generation; the merge finds every
  // saved entry already open, so it opens or connects nothing.
  await persistence.restore(true);
  return { status: 'moved', agentId: connectId };
}

export type MoveStatusUi = Pick<
  TerminalWorkspaceRoot,
  'setStatus' | 'setStatusAction' | 'clearStatus'
>;

/**
 * Runs a move from the non-owner screen and keeps its status and button in
 * step: "Moving terminals…" (disabled) while it runs; on success the button
 * goes away (the moved terminal is on screen, or, with nothing saved, the
 * status returns to the normal empty viewer); on failure the status shows
 * the error with a Retry button that calls retry.
 */
export async function moveTerminalsWithStatus(
  options: MoveTerminalsOptions & { ui: MoveStatusUi; retry: () => void }
): Promise<MoveTerminalsResult> {
  const { ui } = options;
  ui.setStatusAction({ label: MOVING_TERMINALS_LABEL, disabled: true, onClick: () => {} });
  const result = await moveTerminalsHere(options);
  if (result.status === 'moved') {
    if (result.agentId === null) ui.clearStatus();
    else ui.setStatusAction(null);
  } else {
    ui.setStatus(`Terminals could not be moved: ${result.message}`);
    ui.setStatusAction({ label: 'Retry', onClick: options.retry });
  }
  return result;
}

/**
 * Asks the current owner to release, then waits for the lock. 'owned' once
 * this window owns, including when the owner went away without answering
 * (closed, crashed or reloaded) and the queued wait handed the lock here.
 */
async function askForOwnership(
  coordinator: MoveCoordinator,
  agentId: string
): Promise<'owned' | 'no-answer' | 'lock-elsewhere'> {
  if (coordinator.isOwner) return 'owned';
  const released = await coordinator.requestRelease(agentId);
  if (coordinator.isOwner) return 'owned';
  if (!released) return 'no-answer';
  return (await coordinator.awaitOwnership()) ? 'owned' : 'lock-elsewhere';
}

/**
 * Resolves with the first state that ends the connect: connected, or
 * disconnected/unavailable/closed. Waits as long as the session's own
 * connect timeouts allow; adds none of its own.
 */
function settled(session: TerminalSession): Promise<TerminalSessionState> {
  return new Promise((resolve) => {
    let done = false;
    let unsubscribe: (() => void) | null = null;
    const check = (state: TerminalSessionState): void => {
      if (done) return;
      if (state.connection === 'loading' || state.connection === 'connecting') return;
      if (state.connection === 'idle') return;
      done = true;
      unsubscribe?.();
      resolve(state);
    };
    unsubscribe = session.subscribe(check);
    if (done) unsubscribe();
  });
}
