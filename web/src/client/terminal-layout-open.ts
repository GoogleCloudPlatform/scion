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

/** The part of `TerminalCoordinator` a layout restore uses. */
export interface LayoutOpenCoordinator {
  readonly supported: boolean;
  open(agentId: string, requestId?: string): Promise<TerminalOpenResult>;
}

/** The part of `TerminalWorkspaceRoot` a layout restore uses. */
export interface LayoutOpenWorkspace {
  findSessionKeyByAgentId(agentId: string): string | null;
}

export interface LayoutOpenOptions {
  coordinator: LayoutOpenCoordinator;
  workspace: LayoutOpenWorkspace;
  /** Agent IDs from the layout URL, in slot order; null is an empty slot. */
  slots: ReadonlyArray<string | null>;
  /**
   * `main.ts`'s request-ID-to-navigation map. An entry tells the coordinator's
   * `select` adapter not to navigate to the single-agent path.
   */
  navigations: Map<string, number>;
  /** The navigation this restore belongs to. */
  navigationId: number;
  /** Reads the current navigation, to drop a restore that was superseded. */
  currentNavigationId: () => number;
}

/**
 * Resolves the session key for every slot of a layout restored from the URL,
 * opening a session for each agent that has none yet.
 *
 * The opens run at the same time rather than one after another: each
 * `open()` waits for its own focus observation before it settles, so opening
 * slots in turn added that wait once per slot. They are still started in
 * slot order, so sessions are created and selected in the same order as
 * before; the caller's `layoutManager.restore()` then decides what is shown
 * and selected. An agent named in more than one slot is opened once.
 *
 * Returns the keys in slot order (null for an empty slot or an agent that
 * could not be opened), or null if the navigation was superseded, in which
 * case the caller must not apply the layout.
 */
export async function openLayoutSlots(
  options: LayoutOpenOptions
): Promise<Array<string | null> | null> {
  const { coordinator, workspace, slots, navigations, navigationId } = options;
  const toOpen = [
    ...new Set(
      slots.filter(
        (agentId): agentId is string =>
          agentId !== null && workspace.findSessionKeyByAgentId(agentId) === null
      )
    ),
  ];
  if (toOpen.length > 0) {
    if (options.currentNavigationId() !== navigationId) return null;
    await Promise.all(
      toOpen.map(async (agentId) => {
        const requestId = coordinator.supported ? crypto.randomUUID() : undefined;
        if (requestId) navigations.set(requestId, navigationId);
        try {
          const result = await coordinator.open(agentId, requestId);
          if (requestId && result.status !== 'pending') navigations.delete(requestId);
        } catch {
          // Agent unavailable/unauthorized/deleted — its slot stays empty.
        }
      })
    );
    if (options.currentNavigationId() !== navigationId) return null;
  }
  return slots.map((agentId) => (agentId ? workspace.findSessionKeyByAgentId(agentId) : null));
}
