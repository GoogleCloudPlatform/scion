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

import type { TerminalOpenResult } from './terminal-coordinator.js';

/**
 * How many times a palette-picked agent's 'pending' open is retried before
 * giving up on placement. After that the request's URL registration and
 * placement hint are dropped, so if this tab still executes it later (a lock
 * claim taking longer than all the attempts), `select` treats it as an
 * ordinary open: it navigates to the agent's URL and places the pane as a
 * single-agent view. That case is rare enough that it is not tracked further.
 */
export const PALETTE_OPEN_ATTEMPTS = 10;

/** The part of `TerminalCoordinator` a palette open uses. */
export interface PaletteOpenCoordinator {
  readonly supported: boolean;
  readonly isOwner: boolean;
  open(agentId: string, requestId?: string): Promise<TerminalOpenResult>;
}

/** The part of `TerminalWorkspaceRoot` a palette open uses. */
export interface PaletteOpenWorkspace {
  cancelPalettePlacement(agentId: string): void;
}

export interface PaletteOpenOptions {
  coordinator: PaletteOpenCoordinator;
  workspace: PaletteOpenWorkspace;
  agentId: string;
  /**
   * `main.ts`'s request-ID-to-navigation map. An entry tells the coordinator's
   * `select` adapter not to navigate to the single-agent path.
   */
  navigations: Map<string, number>;
  /** The navigation this open belongs to. */
  navigationId: number;
  /** Reads the current navigation, to drop feedback for a superseded one. */
  currentNavigationId: () => number;
  /** Shows a short message to the user. */
  notify: (message: string) => void;
  createRequestId?: () => string;
}

/**
 * What a tab that does not own the terminal sessions tells the user after an
 * open: the owning tab shows the terminal, so this tab can only report on it.
 */
export function nonOwnerOpenStatus(status: TerminalOpenResult['status']): string {
  if (status === 'selected') return 'Terminal selected in its owning tab.';
  if (status === 'pending') return 'Waiting for the owning tab to select this terminal.';
  return 'Terminal workspace is unavailable in this tab.';
}

/**
 * Opens an agent the "Jump to agent" palette picked in a multi-pane layout,
 * when this tab has no session for it yet.
 *
 * The open is registered in `navigations` so the coordinator's `select`
 * adapter keeps the multi-pane URL instead of navigating to the single-agent
 * path. While no tab has claimed the session yet, `open()` resolves
 * 'pending'; it is retried with the same request ID, up to
 * {@link PALETTE_OPEN_ATTEMPTS} times, so the workspace's placement hint
 * stays in place until the open settles. Once it settles, however it ends,
 * the registration and the hint are both cleared.
 *
 * A tab that does not own the sessions never creates the pane itself, so it
 * reports the outcome through `notify` instead.
 */
export async function openPalettePickedAgent(options: PaletteOpenOptions): Promise<void> {
  const { coordinator, workspace, agentId, navigations, navigationId } = options;
  const requestId = coordinator.supported
    ? (options.createRequestId ?? ((): string => crypto.randomUUID()))()
    : undefined;
  if (requestId) navigations.set(requestId, navigationId);
  const report = (status: TerminalOpenResult['status']): void => {
    if (coordinator.isOwner || options.currentNavigationId() !== navigationId) return;
    options.notify(nonOwnerOpenStatus(status));
  };
  try {
    for (let attempt = 0; attempt < PALETTE_OPEN_ATTEMPTS; attempt++) {
      const result = await coordinator.open(agentId, requestId);
      if (result.status !== 'pending') {
        report(result.status);
        break;
      }
      if (attempt === 0) report('pending');
    }
  } catch {
    // open() rejected (an invalid request or an adapter failure): nothing to place.
  } finally {
    if (requestId) navigations.delete(requestId);
    workspace.cancelPalettePlacement(agentId);
  }
}
