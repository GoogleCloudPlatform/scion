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

/**
 * Agents-only candidate source for the terminal view's "Jump to agent"
 * palette — deliberately separate from `chat-palette-data.ts`'s
 * `ChatPaletteDataController` (and its DM-recency join, People/Threads
 * groups, and messageability filter), which this must not depend on.
 *
 * Reuses only the chat palette's bounded, progressive Agents walk
 * ({@link loadPaletteAgentsBounded}): it makes no filtering decisions of its
 * own, so the terminal view gets the same per-page progress and idle bound
 * as chat. Viability is attach-and-running/stopping, not chat's
 * lifecycle-or-attach messageability — a viewer with only `lifecycle` (no
 * `attach`) can message an agent but cannot open its terminal, and a stopped
 * agent cannot be attached to either.
 */

import { loadPaletteAgentsBounded, type RawPaletteAgent } from './chat-palette-data.js';
import { buildAgentCandidate } from './agent-palette-candidate.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import { can, isTerminalAvailable } from '../shared/types.js';

/**
 * Whether `agent` can be attached to from the terminal workspace: the
 * {@link isTerminalAvailable} phase/activity rule, plus the `attach`
 * capability the PTY endpoint actually authorizes on
 * (`pkg/hub/pty_handlers.go`) — narrower than chat's `canMessageAgent`
 * (`lifecycle` OR `attach`), since `lifecycle` alone does not grant terminal
 * access.
 */
export function isTerminalPaletteAgentViable(agent: RawPaletteAgent): boolean {
  return isTerminalAvailable(agent) && can(agent._capabilities, 'attach');
}

/**
 * Build Agents-group palette candidates for the terminal view: one
 * {@link buildAgentCandidate} row per viable agent.
 */
export function buildTerminalAgentCandidates(
  agents: readonly RawPaletteAgent[]
): PaletteCandidate[] {
  const candidates: PaletteCandidate[] = [];
  for (const agent of agents) {
    if (!agent.id || !isTerminalPaletteAgentViable(agent)) continue;
    candidates.push(buildAgentCandidate(agent));
  }
  return candidates;
}

/** Options for {@link loadTerminalPaletteAgents}. */
export interface TerminalPaletteAgentsLoadOptions {
  /** The load's own controller: abort it to cancel or supersede the load. */
  controller: AbortController;
  /** Whether this load is still the caller's current one. */
  isCurrent: () => boolean;
  /** Called with the candidates built from every page seen so far. */
  onProgress?: (candidates: PaletteCandidate[]) => void;
}

/**
 * Fetch every attachable, running-or-stopping agent across every project
 * (the terminal workspace is cross-project — see `terminal-workspace-root.ts`)
 * and build them into Agents-group palette candidates, publishing partial
 * candidates per page through `onProgress`.
 *
 * Rejects with `PaletteLoadError` when a still-current load fails or makes
 * no progress for `AGENTS_IDLE_TIMEOUT_MS`, and with an AbortError-like
 * error when the load was cancelled or superseded.
 */
export function loadTerminalPaletteAgents({
  controller,
  isCurrent,
  onProgress,
}: TerminalPaletteAgentsLoadOptions): Promise<PaletteCandidate[]> {
  return loadPaletteAgentsBounded({
    controller,
    isCurrent,
    ...(onProgress && {
      onProgress: (agentsSoFar: readonly RawPaletteAgent[]) =>
        onProgress(buildTerminalAgentCandidates(agentsSoFar)),
    }),
    finish: (agents) => buildTerminalAgentCandidates(agents),
  });
}
