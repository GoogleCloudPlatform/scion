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
 * The rows come from the shared agent store's hub entry: the palette is a
 * selector over that snapshot, so it shares one walk with every other
 * consumer of the hub list, answers from memory once the list is loaded,
 * and stays current from the store's agent feed. Viability is
 * attach-and-running/stopping, not chat's lifecycle-or-attach
 * messageability — a viewer with only `lifecycle` (no `attach`) can message
 * an agent but cannot open its terminal, and a stopped agent cannot be
 * attached to either.
 */

import { agentStore } from './agent-store.js';
import type { AgentListSnapshot, AgentStore } from './agent-store.js';
import type { RawPaletteAgent } from './chat-palette-data.js';
import { buildAgentCandidate } from './agent-palette-candidate.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import { can, isTerminalAvailable } from '../shared/types.js';

/** The part of the agent store the terminal palette reads. */
export type TerminalPaletteAgentSource = Pick<AgentStore, 'ensure' | 'retain'>;

/** The store entry behind the terminal palette: every agent across every project. */
export const TERMINAL_PALETTE_AGENT_QUERY = { scope: 'hub' } as const;

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

/** Candidates per row array, so a list no store holds any more is not kept alive here. */
const selected = new WeakMap<readonly RawPaletteAgent[], PaletteCandidate[]>();

/**
 * The terminal palette's candidates for a store snapshot. Memoised on the
 * snapshot's row array, which the store replaces only when membership or a
 * row changes, so republishing an unchanged list returns the same array.
 */
export function selectTerminalPaletteCandidates(snapshot: AgentListSnapshot): PaletteCandidate[] {
  let candidates = selected.get(snapshot.agents);
  if (!candidates) {
    candidates = buildTerminalAgentCandidates(snapshot.agents);
    selected.set(snapshot.agents, candidates);
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

function abortError(): DOMException {
  return new DOMException('load aborted or superseded', 'AbortError');
}

/**
 * Every attachable, running-or-stopping agent across every project (the
 * terminal workspace is cross-project — see `terminal-workspace-root.ts`),
 * as Agents-group palette candidates, read from the store's hub entry.
 * While the store walks the list, `onProgress` gets the candidates of every
 * page seen so far.
 *
 * Rejects with the store's error when the walk fails, and with an
 * AbortError when the load was cancelled or superseded (`controller`
 * aborted, or `isCurrent()` false). Aborting detaches this load only: the
 * walk goes on for the store's other consumers.
 */
export async function loadTerminalPaletteAgents(
  { controller, isCurrent, onProgress }: TerminalPaletteAgentsLoadOptions,
  agents: TerminalPaletteAgentSource = agentStore
): Promise<PaletteCandidate[]> {
  let snapshot: AgentListSnapshot;
  try {
    snapshot = await agents.ensure(TERMINAL_PALETTE_AGENT_QUERY, {
      signal: controller.signal,
      ...(onProgress && {
        // The store stops calling an aborted caller, so only a load
        // superseded without an abort needs this check.
        onProgress: (progress: AgentListSnapshot): void => {
          if (isCurrent()) onProgress(buildTerminalAgentCandidates(progress.agents));
        },
      }),
    });
  } catch (err) {
    if (controller.signal.aborted || !isCurrent()) throw abortError();
    throw err;
  }
  // The store rejects an aborted caller, so only a load superseded without
  // an abort needs this check; the host checks its own abort as well.
  if (!isCurrent()) throw abortError();
  return selectTerminalPaletteCandidates(snapshot);
}

/**
 * Keep the store's hub entry live for the terminal workspace and hear its
 * candidates on every ready snapshot (an SSE change, a revalidation).
 * Loading and error snapshots are left to the load that asked for them.
 * Does not fetch by itself. Returns the release function.
 */
export function retainTerminalPaletteAgents(
  listener: (candidates: PaletteCandidate[]) => void,
  agents: TerminalPaletteAgentSource = agentStore
): () => void {
  return agents.retain(TERMINAL_PALETTE_AGENT_QUERY, (snapshot) => {
    if (snapshot.status === 'ready') listener(selectTerminalPaletteCandidates(snapshot));
  });
}
