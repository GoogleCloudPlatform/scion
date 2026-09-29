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
 * Real paginated list adapter for the native chat quick command palette
 * (Phase 1: the Agents group only).
 *
 * Fetches GET /api/v1/agents (fully paginated, independent of current-space
 * membership) and GET /api/v1/chat/dms, joins them into normalized
 * {@link PaletteCandidate}s, and exposes cancellation so a page controller
 * can discard a stale in-flight load (identity change, palette closed before
 * the response arrived, or a newer load superseding an older one).
 */

import { apiFetch } from './api.js';
import type { ApiFetchOptions } from './api.js';
import type {
  AgentMessageability,
  AgentMessageabilityDetail,
  Capabilities,
} from '../shared/types.js';
import { canMessageAgent } from '../shared/types.js';
import { activityMsFromTimestamp } from '../utils/chat-palette-match.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import { dmCandidateId } from './chat-palette-types.js';

/** Agents page size. The server default is much larger; 100 keeps pages small enough to show progress. */
const AGENTS_PAGE_LIMIT = 100;

/**
 * Safety bound on the number of pages followed for one agents load. Well
 * above any realistic hub size — this exists only so a server bug cannot
 * hang the palette in an infinite pagination loop.
 */
const MAX_AGENT_PAGES = 500;

/** The subset of the agent-list response shape this module reads. */
export interface RawPaletteAgent {
  id: string;
  name?: string;
  slug?: string;
  _capabilities?: Capabilities;
  _messageability?: AgentMessageability | AgentMessageabilityDetail;
}

interface AgentListResponse {
  agents?: RawPaletteAgent[];
  nextCursor?: string;
}

/** The subset of a DM list entry this module reads. */
export interface RawPaletteDm {
  conversationKey: string;
  peerId: string;
  peerKind: string;
  peerName?: string;
  lastActivityAt?: string;
}

interface DmListResponse {
  dms?: RawPaletteDm[];
}

/** Raised when a group's data cannot be loaded (network failure, bad response shape, or a pagination defect). */
export class PaletteLoadError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PaletteLoadError';
  }
}

/**
 * Fetch every authorized agent, following `nextCursor` until it is empty —
 * not until a page's items array is empty, since a filtered intermediate
 * page can legitimately return zero items while still carrying a cursor.
 * A cursor value repeating across pages is treated as a load error rather
 * than an infinite loop.
 */
export async function fetchAllPaletteAgents(signal?: AbortSignal): Promise<RawPaletteAgent[]> {
  const all: RawPaletteAgent[] = [];
  const seenCursors = new Set<string>();
  let cursor = '';
  let pages = 0;

  do {
    const url = cursor
      ? `/api/v1/agents?limit=${AGENTS_PAGE_LIMIT}&cursor=${encodeURIComponent(cursor)}`
      : `/api/v1/agents?limit=${AGENTS_PAGE_LIMIT}`;
    const options: ApiFetchOptions | undefined = signal ? { signal } : undefined;
    const res = await apiFetch(url, options);
    if (!res.ok) {
      throw new PaletteLoadError(`agents list request failed: ${res.status}`);
    }
    let raw: unknown;
    try {
      raw = await res.json();
    } catch (err) {
      // A cancelled or superseded load aborts `signal` out from under an
      // in-flight body read: `res.json()` then rejects with an AbortError,
      // not because the body was malformed. Rethrow it as-is so the caller's
      // AbortError handling (see ChatPaletteDataController) sees "no
      // update," not a load failure — only a genuinely bad body becomes a
      // PaletteLoadError.
      if (signal?.aborted) {
        throw err;
      }
      throw new PaletteLoadError('agents list response was not valid JSON');
    }
    // A JSON body can be any of null, an array, or a primitive (string,
    // number, boolean) and still parse successfully — none of those are a
    // valid list page, and treating them as "zero agents" (or letting
    // `data.agents`/`data.nextCursor` below throw a TypeError on a
    // non-object receiver) would silently truncate or crash on a
    // misconfigured endpoint. Reject anything that isn't a plain object.
    if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
      throw new PaletteLoadError('agents list response body was not an object');
    }
    const data = raw as AgentListResponse;
    if (Array.isArray(data.agents)) {
      all.push(...data.agents);
    }
    const next = typeof data.nextCursor === 'string' ? data.nextCursor : '';
    if (next) {
      if (seenCursors.has(next)) {
        throw new PaletteLoadError('agents list returned a repeated pagination cursor');
      }
      seenCursors.add(next);
    }
    cursor = next;
    pages++;
  } while (cursor && pages < MAX_AGENT_PAGES);

  if (cursor && pages >= MAX_AGENT_PAGES) {
    throw new PaletteLoadError('agents list did not terminate within the page safety bound');
  }

  return all;
}

/**
 * Fetch the current user's DM list. Failure here propagates and fails the
 * whole Agents group at the call site ({@link ChatPaletteDataController.loadAgentsGroup}) —
 * silently degrading every agent to `activityMs=0` would read as "no agent
 * has a DM yet" rather than "recency is unknown right now".
 */
export async function fetchPaletteDms(signal?: AbortSignal): Promise<RawPaletteDm[]> {
  const options: ApiFetchOptions | undefined = signal ? { signal } : undefined;
  const res = await apiFetch('/api/v1/chat/dms', options);
  if (!res.ok) {
    throw new PaletteLoadError(`dm list request failed: ${res.status}`);
  }
  let raw: unknown;
  try {
    raw = await res.json();
  } catch (err) {
    // See the matching comment in fetchAllPaletteAgents: a
    // cancelled/superseded load's abort can land mid-body-read, and that
    // AbortError must propagate as-is rather than being repackaged as a load
    // failure.
    if (signal?.aborted) {
      throw err;
    }
    throw new PaletteLoadError('dm list response was not valid JSON');
  }
  // Same rationale as the object guard in fetchAllPaletteAgents above: null,
  // an array, or a primitive body is a load failure, not zero DMs.
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new PaletteLoadError('dm list response body was not an object');
  }
  const data = raw as DmListResponse;
  return Array.isArray(data.dms) ? data.dms : [];
}

/**
 * Whether an agent is a viable DM peer. `_messageability.canMessage` is
 * authoritative when present (explicit `false` wins even if capabilities
 * would otherwise allow management); otherwise fall back to
 * `canMessageAgent(_capabilities)`. Missing both fails closed. Stopped or
 * otherwise non-running phase does not by itself deny an agent — only
 * messageability does.
 */
export function isPaletteAgentViable(agent: RawPaletteAgent): boolean {
  const messageability = agent._messageability;
  if (messageability && typeof messageability.canMessage === 'boolean') {
    return messageability.canMessage;
  }
  return canMessageAgent(agent._capabilities);
}

/**
 * Join agents with their existing DM (if any) into Agents-group palette
 * candidates. A viable agent with no DM yet still appears, with
 * `activityMs=0` — its DM is created deterministically on first message, no
 * API call needed (see `openDM` in chat.ts).
 */
export function buildAgentCandidates(
  agents: RawPaletteAgent[],
  dms: RawPaletteDm[]
): PaletteCandidate[] {
  const dmByAgentId = new Map<string, RawPaletteDm>();
  for (const dm of dms) {
    if (dm.peerKind === 'agent' && dm.peerId) {
      dmByAgentId.set(dm.peerId, dm);
    }
  }

  const candidates: PaletteCandidate[] = [];
  for (const agent of agents) {
    if (!agent.id || !isPaletteAgentViable(agent)) continue;
    const displayName = agent.name || agent.slug || agent.id;
    const dm = dmByAgentId.get(agent.id);
    const searchFields = [displayName];
    if (agent.slug && agent.slug !== displayName) searchFields.push(agent.slug);

    candidates.push({
      id: dmCandidateId('agent', agent.id),
      group: 'agents',
      label: displayName,
      secondaryLabel: agent.slug ?? '',
      searchFields,
      activityMs: activityMsFromTimestamp(dm?.lastActivityAt),
      target: {
        kind: 'dm',
        peerKind: 'agent',
        peerId: agent.id,
        displayName,
      },
    });
  }
  return candidates;
}

/**
 * Per-open controller for the Agents group: fetches agents + DMs, builds
 * candidates, and guards against a superseded/aborted load overwriting a
 * later one. One controller instance is reused across opens by the page.
 */
export class ChatPaletteDataController {
  private generation = 0;
  private abortController: AbortController | null = null;

  /** Abort any in-flight load without starting a new one (palette closed, controller disposed). */
  cancel(): void {
    this.generation++;
    this.abortController?.abort();
    this.abortController = null;
  }

  /**
   * Load the Agents group. Resolves to the candidate list, or rejects with
   * {@link PaletteLoadError}. A load superseded by a later call to
   * {@link loadAgentsGroup} or {@link cancel} rejects with an AbortError-like
   * error the caller should treat as "no update", not a failure to display.
   */
  async loadAgentsGroup(): Promise<PaletteCandidate[]> {
    this.abortController?.abort();
    const controller = new AbortController();
    this.abortController = controller;
    const myGeneration = ++this.generation;

    try {
      const agents = await fetchAllPaletteAgents(controller.signal);

      if (myGeneration !== this.generation) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      // The DM join only supplies recency, not agent membership, but a
      // failure here must still be visible rather than silently degrading
      // every agent to activityMs=0 (which reads as "no agent has a DM yet"
      // rather than "recency is unknown right now") — so it fails the whole
      // group. The dialog's existing retry control reloads both fetches
      // together. A softer "ready, but recency unavailable" state that keeps
      // the agent list interactive while flagging the join failure would be a
      // reasonable enhancement; deferred rather than added speculatively here.
      const dms = await fetchPaletteDms(controller.signal);

      if (myGeneration !== this.generation) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      return buildAgentCandidates(agents, dms);
    } catch (err) {
      // Backstop, independent of fetchAllPaletteAgents/fetchPaletteDms's own
      // abort handling: *any* error surfacing from either fetch — including
      // one that doesn't originate from an aborted `res.json()` at all, e.g.
      // a 403 whose `!res.ok` check fires unconditionally regardless of the
      // signal's state, or apiFetch's own internal `response.clone().json()`
      // read (api.ts) landing while this load's signal happens to be
      // aborted — must not be published as a real failure once this load has
      // been cancelled or superseded. Re-classify it as the
      // same AbortError-shaped rejection the explicit generation checks
      // above already use, whatever the error's actual type; a genuine
      // failure of a still-current, non-aborted load passes through
      // unchanged.
      if (controller.signal.aborted || myGeneration !== this.generation) {
        throw new DOMException('load aborted or superseded', 'AbortError');
      }
      throw err;
    }
  }
}
