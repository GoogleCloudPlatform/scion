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
 * Real paginated list adapter for the native chat quick command palette,
 * covering the Agents/DM, People, and Threads groups.
 *
 * Fetches GET /api/v1/agents, GET /api/v1/users (both fully paginated,
 * independent of current-space membership), GET /api/v1/chat/dms, GET
 * /api/v1/chat/spaces and per-space GET /api/v1/chat/spaces/{id}/threads,
 * joins them into normalized {@link PaletteCandidate}s, and exposes
 * cancellation so a page controller can discard a stale in-flight load
 * (identity change, palette closed before the response arrived, or a newer
 * load superseding an older one).
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
import { formatFileSize } from '../utils/chat-file-links.js';
import type { PaletteCandidate, PaletteThreadTarget } from './chat-palette-types.js';
import { dmCandidateId, documentCandidateId, threadCandidateId } from './chat-palette-types.js';
import type { RecentFile } from './chat-recent-files.js';

/** Agents page size. The server default is much larger; 100 keeps pages small enough to show progress. */
const AGENTS_PAGE_LIMIT = 100;

/**
 * Safety bound on the number of pages followed for one agents load. Well
 * above any realistic hub size — this exists only so a server bug cannot
 * hang the palette in an infinite pagination loop.
 */
const MAX_AGENT_PAGES = 500;

/** Users page size. The server default (50) is smaller than agents'; request the same page size as agents for parity. */
const USERS_PAGE_LIMIT = 100;

/** Safety bound on pages followed for one users load — see {@link MAX_AGENT_PAGES}. */
const MAX_USER_PAGES = 500;

/** Maximum concurrent per-space thread-list requests. */
const MAX_CONCURRENT_THREAD_REQUESTS = 4;

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

/** The subset of the user-list response shape this module reads. */
export interface RawPaletteUser {
  id: string;
  displayName?: string;
  email?: string;
  avatarUrl?: string;
  status?: string;
}

interface UserListResponse {
  users?: RawPaletteUser[];
  nextCursor?: string;
}

/** The subset of a `/api/v1/chat/spaces` entry this module reads. */
export interface RawPaletteSpace {
  projectId: string;
  projectName?: string;
  projectSlug?: string;
}

interface SpacesListResponse {
  spaces?: RawPaletteSpace[];
}

/** The subset of a `/api/v1/chat/spaces/{id}/threads` entry this module reads. */
export interface RawPaletteThread {
  id: string;
  projectId: string;
  name?: string;
  defaultAgent?: string;
  lastActivityAt?: string;
}

interface ThreadsListResponse {
  threads?: RawPaletteThread[];
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
 * Fetch every authorized user, following `nextCursor` until it is empty — the
 * same contract as {@link fetchAllPaletteAgents}, fully paginating
 * `GET /api/v1/users`. `/api/v1/users`'s own cursor has no cross-request
 * binding/validation the way agents' does, but the client-side traversal
 * contract (repeated cursor -> error, not an infinite loop) is identical, so
 * this mirrors that function rather than guessing a different failure mode
 * for a difference the client can't observe.
 */
export async function fetchAllPaletteUsers(signal?: AbortSignal): Promise<RawPaletteUser[]> {
  const all: RawPaletteUser[] = [];
  const seenCursors = new Set<string>();
  let cursor = '';
  let pages = 0;

  do {
    const url = cursor
      ? `/api/v1/users?limit=${USERS_PAGE_LIMIT}&cursor=${encodeURIComponent(cursor)}`
      : `/api/v1/users?limit=${USERS_PAGE_LIMIT}`;
    const options: ApiFetchOptions | undefined = signal ? { signal } : undefined;
    const res = await apiFetch(url, options);
    if (!res.ok) {
      throw new PaletteLoadError(`users list request failed: ${res.status}`);
    }
    let data: UserListResponse | null;
    try {
      data = (await res.json()) as UserListResponse | null;
    } catch (err) {
      // See the matching comment in fetchAllPaletteAgents: a
      // cancelled/superseded load's abort can land mid-body-read, and that
      // AbortError must propagate as-is rather than being repackaged as a
      // load failure.
      if (signal?.aborted) {
        throw err;
      }
      throw new PaletteLoadError('users list response was not valid JSON');
    }
    if (data === null || typeof data !== 'object' || Array.isArray(data)) {
      throw new PaletteLoadError('users list response body was not a JSON object');
    }
    if (Array.isArray(data.users)) {
      all.push(...data.users);
    }
    const next = typeof data.nextCursor === 'string' ? data.nextCursor : '';
    if (next) {
      if (seenCursors.has(next)) {
        throw new PaletteLoadError('users list returned a repeated pagination cursor');
      }
      seenCursors.add(next);
    }
    cursor = next;
    pages++;
  } while (cursor && pages < MAX_USER_PAGES);

  if (cursor && pages >= MAX_USER_PAGES) {
    throw new PaletteLoadError('users list did not terminate within the page safety bound');
  }

  return all;
}

/**
 * Whether a user is a viable People candidate. The real hub's
 * `store.User.Status` enum is `active | suspended | invited`
 * (`pkg/store/models.go`) — there is no `"disabled"` value, so a bare
 * literal `status === 'disabled'` check would be dead code against any real
 * `GET /api/v1/users` response.
 * Treat anything that is not `active` (or the field being absent/empty,
 * which existing callers such as `loadHubMembers` also treat as viable) as
 * excluded. This covers `suspended` and `invited` today, and — by
 * construction, not a separate literal comparison — would also cover a
 * hypothetical future `'disabled'` value without needing a code change if
 * the backend ever added one.
 */
export function isPaletteUserViable(user: RawPaletteUser): boolean {
  const status = user.status;
  if (!status) return true;
  return status === 'active';
}

/**
 * Join authorized users with their existing DM (if any) into People-group
 * palette candidates. Omits the current user and any non-viable user (see
 * {@link isPaletteUserViable} for what "disabled" means against real data),
 * matching the same convention `loadHubMembers` already uses. A viable user
 * with no DM yet still appears, with `activityMs=0`, exactly like an agent
 * without a DM (`buildAgentCandidates`) — its DM is created deterministically
 * on first message via the same sorted-user key `openDM`/`buildDMKey`
 * already build.
 *
 * Only iterates the authorized `users` list — never the `dms` list — so a DM
 * whose peer is no longer in that list (denied, disabled, or deleted) cannot
 * resurrect a candidate for it.
 */
export function buildUserCandidates(
  users: RawPaletteUser[],
  dms: RawPaletteDm[],
  selfUserId: string
): PaletteCandidate[] {
  const dmByUserId = new Map<string, RawPaletteDm>();
  for (const dm of dms) {
    if (dm.peerKind === 'user' && dm.peerId) {
      dmByUserId.set(dm.peerId, dm);
    }
  }

  const candidates: PaletteCandidate[] = [];
  for (const user of users) {
    if (!user.id || user.id === selfUserId) continue;
    if (!isPaletteUserViable(user)) continue;
    const displayName = user.displayName || user.email || user.id;
    const dm = dmByUserId.get(user.id);
    const searchFields = [displayName];
    if (user.email && user.email !== displayName) searchFields.push(user.email);

    candidates.push({
      id: dmCandidateId('user', user.id),
      group: 'people',
      label: displayName,
      secondaryLabel: user.email ?? '',
      searchFields,
      activityMs: activityMsFromTimestamp(dm?.lastActivityAt),
      target: {
        kind: 'dm',
        peerKind: 'user',
        peerId: user.id,
        displayName,
      },
    });
  }
  return candidates;
}

/**
 * Fetch every space the caller can read. `/api/v1/chat/spaces` is not
 * paginated: it returns every authorized project in one response, capped
 * server-side, so — unlike agents/users — there is no cursor loop here.
 */
export async function fetchPaletteSpaces(signal?: AbortSignal): Promise<RawPaletteSpace[]> {
  const options: ApiFetchOptions | undefined = signal ? { signal } : undefined;
  const res = await apiFetch('/api/v1/chat/spaces', options);
  if (!res.ok) {
    throw new PaletteLoadError(`spaces list request failed: ${res.status}`);
  }
  let data: SpacesListResponse | null;
  try {
    data = (await res.json()) as SpacesListResponse | null;
  } catch (err) {
    if (signal?.aborted) {
      throw err;
    }
    throw new PaletteLoadError('spaces list response was not valid JSON');
  }
  if (data === null || typeof data !== 'object' || Array.isArray(data)) {
    throw new PaletteLoadError('spaces list response body was not a JSON object');
  }
  return Array.isArray(data.spaces) ? data.spaces : [];
}

/** Fetch every thread for one space. The endpoint is not paginated. */
export async function fetchPaletteThreadsForSpace(
  projectId: string,
  signal?: AbortSignal
): Promise<RawPaletteThread[]> {
  const options: ApiFetchOptions | undefined = signal ? { signal } : undefined;
  const res = await apiFetch(
    `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`,
    options
  );
  if (!res.ok) {
    throw new PaletteLoadError(`space ${projectId} thread list request failed: ${res.status}`);
  }
  let data: ThreadsListResponse | null;
  try {
    data = (await res.json()) as ThreadsListResponse | null;
  } catch (err) {
    if (signal?.aborted) {
      throw err;
    }
    throw new PaletteLoadError(`space ${projectId} thread list response was not valid JSON`);
  }
  if (data === null || typeof data !== 'object' || Array.isArray(data)) {
    throw new PaletteLoadError(
      `space ${projectId} thread list response body was not a JSON object`
    );
  }
  return Array.isArray(data.threads) ? data.threads : [];
}

/**
 * Build Threads-group palette candidates for every thread of one space.
 * Includes every returned row — muted threads too — with no client-side
 * archived/closed/unjoined filtering; only threads the server itself
 * returns from listing are ever shown.
 */
export function buildThreadCandidates(
  space: RawPaletteSpace,
  threads: RawPaletteThread[]
): PaletteCandidate[] {
  const spaceLabel = space.projectName || space.projectSlug || space.projectId;
  const candidates: PaletteCandidate[] = [];
  for (const thread of threads) {
    if (!thread.id) continue;
    const threadName = thread.name || thread.id;
    const searchFields = [threadName];
    if (spaceLabel && spaceLabel !== threadName) searchFields.push(spaceLabel);

    // Use `space.projectId` — the ID this thread was actually fetched under
    // (`GET /api/v1/chat/spaces/{space.projectId}/threads`) — rather than the
    // row's own `thread.projectId`. Pairing a *different* `thread.projectId`
    // with `space.projectSlug` would route to a slug for the wrong project if
    // the two ever disagreed. `space.projectId` is authoritative for "which
    // project this fetch was scoped to" by construction; a mismatching
    // `thread.projectId` would indicate a server data-integrity bug, not a
    // case worth silently trusting.
    const target: PaletteThreadTarget = {
      kind: 'thread',
      projectId: space.projectId,
      threadId: thread.id,
      threadName,
      ...(space.projectSlug ? { projectSlug: space.projectSlug } : {}),
      ...(thread.defaultAgent ? { defaultAgent: thread.defaultAgent } : {}),
    };

    candidates.push({
      id: threadCandidateId(target.projectId, thread.id),
      group: 'threads',
      label: threadName,
      secondaryLabel: spaceLabel,
      searchFields,
      activityMs: activityMsFromTimestamp(thread.lastActivityAt),
      target,
    });
  }
  return candidates;
}

/**
 * The secondary line for a Documents row: the project and container path for
 * a detected path, or the project and attachment metadata for an attachment,
 * so records that share a file name remain distinguishable.
 */
function documentSecondaryLabel(file: RecentFile): string {
  const projectName = file.source.projectName;
  if (file.target.kind === 'path') {
    return projectName
      ? `${projectName} — ${file.target.containerPath}`
      : file.target.containerPath;
  }
  // Size and date, joined only where both are known — appended so two
  // attachments sharing a name and project (the same filename reattached, or
  // shared across conversations) are still distinguishable in the list. A Go
  // zero timestamp (never a real send time) is treated the same as a missing
  // one, matching activityMsFromTimestamp's own definition of "unknown" for
  // this same field.
  const activityMs = activityMsFromTimestamp(file.source.sentAt);
  const metadata = [
    formatFileSize(file.target.size),
    activityMs > 0
      ? new Date(activityMs).toLocaleDateString('en', { month: 'short', day: 'numeric' })
      : '',
  ]
    .filter(Boolean)
    .join(' · ');
  const label = metadata ? `Attachment · ${metadata}` : 'Attachment';
  return projectName ? `${projectName} — ${label}` : label;
}

/**
 * Build Documents candidates from the recent-files store's current
 * snapshot. Unlike the other three groups, there is no network fetch here:
 * `chatRecentFiles` is an already-live, identity-scoped index, and this only
 * maps its records into the shared candidate shape so Documents matches
 * share the same global ranking as Agents/Threads/People. Search fields are
 * the file's display name (every record), its container path (path targets),
 * and its captured project label when known.
 */
export function buildDocumentCandidates(records: readonly RecentFile[]): PaletteCandidate[] {
  const candidates: PaletteCandidate[] = [];
  for (const file of records) {
    const searchFields = [file.name];
    if (file.target.kind === 'path') searchFields.push(file.target.containerPath);
    if (file.source.projectName) searchFields.push(file.source.projectName);

    candidates.push({
      id: documentCandidateId(file.key),
      group: 'documents',
      label: file.name,
      secondaryLabel: documentSecondaryLabel(file),
      searchFields,
      activityMs: activityMsFromTimestamp(file.source.sentAt),
      target: { kind: 'document', file },
    });
  }
  return candidates;
}

/**
 * Run `fn` over `items` with at most `limit` concurrently in flight at once.
 * Unlike chunking items into fixed-size batches, a worker pool keeps exactly
 * `limit` requests in flight the whole time — a batch boundary never leaves
 * a fast request idle waiting for the slowest one in its chunk. Every item
 * settles (success or failure) independently; nothing here rejects the
 * overall call.
 */
async function mapWithConcurrency<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>
): Promise<
  Array<{ item: T; index: number; result: R } | { item: T; index: number; error: unknown }>
> {
  const results: Array<
    { item: T; index: number; result: R } | { item: T; index: number; error: unknown }
  > = new Array(items.length);
  let nextIndex = 0;

  async function worker(): Promise<void> {
    for (;;) {
      const index = nextIndex++;
      if (index >= items.length) return;
      const item = items[index];
      try {
        const result = await fn(item, index);
        results[index] = { item, index, result };
      } catch (error) {
        results[index] = { item, index, error };
      }
    }
  }

  const workerCount = Math.max(1, Math.min(limit, items.length));
  await Promise.all(Array.from({ length: workerCount }, () => worker()));
  return results;
}

/** The outcome of loading (or retrying) the Threads group as a whole. */
export interface ThreadsGroupResult {
  candidates: PaletteCandidate[];
  /** True when one or more spaces' thread lists failed to load this pass. */
  incomplete: boolean;
}

/**
 * Re-classify any error escaping a group's fetches as the same
 * `AbortError`-shaped rejection the explicit generation checks use, once
 * that load has been cancelled or superseded — whatever the error's actual
 * type. A genuine failure of a still-current, non-aborted load passes
 * through unchanged.
 */
function reclassifyIfStale(err: unknown, aborted: boolean, stale: boolean): never {
  if (aborted || stale) {
    throw new DOMException('load aborted or superseded', 'AbortError');
  }
  throw err;
}

/**
 * Per-open controller for the palette's Agents, People and Threads groups:
 * fetches each group's real list(s), builds candidates, and guards against a
 * superseded/aborted load overwriting a later one. One controller instance
 * is reused across opens by the page. Each group has its own
 * generation/AbortController pair so the three groups can load concurrently
 * without one group's cancellation aborting another's in-flight request.
 */
export class ChatPaletteDataController {
  private agentsGeneration = 0;
  private agentsAbort: AbortController | null = null;
  private peopleGeneration = 0;
  private peopleAbort: AbortController | null = null;
  private threadsGeneration = 0;
  private threadsAbort: AbortController | null = null;

  /**
   * Per-space thread state from the most recent Threads load/retry, kept so
   * {@link retryThreadsGroup} can re-fetch *only* the spaces that failed
   * while re-using the other spaces' already-successful threads to rebuild
   * the full candidate list.
   */
  private threadsSpacesById = new Map<string, RawPaletteSpace>();
  private threadsByProjectId = new Map<string, RawPaletteThread[]>();
  private threadsFailedProjectIds = new Set<string>();

  /** Abort any in-flight load without starting a new one (palette closed, controller disposed). */
  cancel(): void {
    this.agentsGeneration++;
    this.agentsAbort?.abort();
    this.agentsAbort = null;
    this.peopleGeneration++;
    this.peopleAbort?.abort();
    this.peopleAbort = null;
    this.threadsGeneration++;
    this.threadsAbort?.abort();
    this.threadsAbort = null;
  }

  /**
   * Load the Agents group. Resolves to the candidate list, or rejects with
   * {@link PaletteLoadError}. A load superseded by a later call to
   * {@link loadAgentsGroup} or {@link cancel} rejects with an AbortError-like
   * error the caller should treat as "no update", not a failure to display.
   */
  async loadAgentsGroup(): Promise<PaletteCandidate[]> {
    this.agentsAbort?.abort();
    const controller = new AbortController();
    this.agentsAbort = controller;
    const myGeneration = ++this.agentsGeneration;

    try {
      const agents = await fetchAllPaletteAgents(controller.signal);

      if (myGeneration !== this.agentsGeneration) {
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

      if (myGeneration !== this.agentsGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      return buildAgentCandidates(agents, dms);
    } catch (err) {
      return reclassifyIfStale(
        err,
        controller.signal.aborted,
        myGeneration !== this.agentsGeneration
      );
    }
  }

  /**
   * Load the People group. Same shape and guarantees as
   * {@link loadAgentsGroup}: a DM-join failure fails the whole group rather
   * than silently degrading recency, and a superseded/aborted load rejects
   * with an AbortError-like error.
   */
  async loadPeopleGroup(selfUserId: string): Promise<PaletteCandidate[]> {
    this.peopleAbort?.abort();
    const controller = new AbortController();
    this.peopleAbort = controller;
    const myGeneration = ++this.peopleGeneration;

    try {
      const users = await fetchAllPaletteUsers(controller.signal);

      if (myGeneration !== this.peopleGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      const dms = await fetchPaletteDms(controller.signal);

      if (myGeneration !== this.peopleGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      return buildUserCandidates(users, dms, selfUserId);
    } catch (err) {
      return reclassifyIfStale(
        err,
        controller.signal.aborted,
        myGeneration !== this.peopleGeneration
      );
    }
  }

  /**
   * Load the Threads group as a full, coherent snapshot: fetch every space,
   * then every space's threads (bounded to {@link MAX_CONCURRENT_THREAD_REQUESTS}
   * concurrent requests). A space whose own thread list fails is recorded so
   * a subsequent {@link retryThreadsGroup} call can re-fetch just that space;
   * the group as a whole still resolves with every space that *did* succeed,
   * flagged `incomplete: true`, rather than failing outright — unless every
   * attempted space failed, in which case failing outright is correct — a
   * failed group must never be silently presented as merely empty.
   *
   * Rejects with an AbortError-like error, exactly like
   * {@link loadAgentsGroup}, if superseded/cancelled — including if the
   * *spaces* fetch itself fails for that reason.
   */
  async loadThreadsGroup(): Promise<ThreadsGroupResult> {
    this.threadsAbort?.abort();
    const controller = new AbortController();
    this.threadsAbort = controller;
    const myGeneration = ++this.threadsGeneration;

    try {
      const spaces = await fetchPaletteSpaces(controller.signal);

      if (myGeneration !== this.threadsGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      this.threadsSpacesById.clear();
      this.threadsByProjectId.clear();
      this.threadsFailedProjectIds.clear();
      const validSpaces: RawPaletteSpace[] = [];
      for (const space of spaces) {
        if (space.projectId) {
          this.threadsSpacesById.set(space.projectId, space);
          validSpaces.push(space);
        }
      }

      const results = await this.fetchThreadsForSpaces(validSpaces, controller.signal);

      if (myGeneration !== this.threadsGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      this.applyThreadFetchResults(results);
      return this.buildThreadsResult(validSpaces.length);
    } catch (err) {
      return reclassifyIfStale(
        err,
        controller.signal.aborted,
        myGeneration !== this.threadsGeneration
      );
    }
  }

  /**
   * Re-fetch only the spaces whose thread list failed on the most recent
   * {@link loadThreadsGroup}/{@link retryThreadsGroup} call, merging the
   * result with the other spaces' already-successful threads to rebuild the
   * full candidate list. Falls back to a full {@link loadThreadsGroup} when
   * there is nothing on record to retry (no prior load, or nothing failed) —
   * there is no narrower retry to perform.
   */
  async retryThreadsGroup(): Promise<ThreadsGroupResult> {
    if (this.threadsFailedProjectIds.size === 0) {
      return this.loadThreadsGroup();
    }

    this.threadsAbort?.abort();
    const controller = new AbortController();
    this.threadsAbort = controller;
    const myGeneration = ++this.threadsGeneration;

    try {
      const retrySpaces = Array.from(this.threadsFailedProjectIds)
        .map((projectId) => this.threadsSpacesById.get(projectId))
        .filter((space): space is RawPaletteSpace => Boolean(space));

      const results = await this.fetchThreadsForSpaces(retrySpaces, controller.signal);

      if (myGeneration !== this.threadsGeneration) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      this.applyThreadFetchResults(results);
      return this.buildThreadsResult(this.threadsSpacesById.size);
    } catch (err) {
      return reclassifyIfStale(
        err,
        controller.signal.aborted,
        myGeneration !== this.threadsGeneration
      );
    }
  }

  /** One space's thread-fetch outcome, returned by {@link fetchThreadsForSpaces} rather than applied directly, so the caller can check its own generation/abort state before any shared state is written. Both callers of {@link fetchThreadsForSpaces} only ever pass spaces with a non-empty `projectId`, so `outcome.item.projectId` is always usable as-is. */
  private static toThreadFetchResult(
    outcome:
      | { item: RawPaletteSpace; index: number; result: RawPaletteThread[] }
      | { item: RawPaletteSpace; index: number; error: unknown }
  ): { projectId: string; threads: RawPaletteThread[] } | { projectId: string; error: unknown } {
    const projectId = outcome.item.projectId;
    if ('error' in outcome) return { projectId, error: outcome.error };
    return { projectId, threads: outcome.result };
  }

  /**
   * Fetch threads for `spaces` at bounded concurrency and return each
   * space's outcome, *without* applying it to
   * {@link threadsByProjectId}/{@link threadsFailedProjectIds} yet — the
   * caller must check its own generation/abort state first and only call
   * {@link applyThreadFetchResults} if the load is still current. Applying
   * these writes unconditionally, before that check, would let a superseded
   * load's aborted per-space rejections land in the *next* load's shared
   * failed-spaces set, once that later load had already cleared and begun
   * repopulating the same shared state.
   */
  private async fetchThreadsForSpaces(
    spaces: readonly RawPaletteSpace[],
    signal: AbortSignal
  ): Promise<
    Array<
      { projectId: string; threads: RawPaletteThread[] } | { projectId: string; error: unknown }
    >
  > {
    const outcomes = await mapWithConcurrency(spaces, MAX_CONCURRENT_THREAD_REQUESTS, (space) =>
      fetchPaletteThreadsForSpace(space.projectId, signal)
    );
    return outcomes.map(ChatPaletteDataController.toThreadFetchResult);
  }

  /** Apply {@link fetchThreadsForSpaces}'s results to the shared per-space state. Only call this after confirming the load is still current. */
  private applyThreadFetchResults(
    results: Array<
      { projectId: string; threads: RawPaletteThread[] } | { projectId: string; error: unknown }
    >
  ): void {
    for (const result of results) {
      if ('error' in result) {
        this.threadsFailedProjectIds.add(result.projectId);
        continue;
      }
      this.threadsByProjectId.set(result.projectId, result.threads);
      this.threadsFailedProjectIds.delete(result.projectId);
    }
  }

  /**
   * Build a {@link ThreadsGroupResult} from the controller's current
   * per-space state. `attemptedSpaceCount` distinguishes "no spaces exist"
   * (an honestly empty, non-incomplete group) from "every attempted space
   * failed" (a fully failed load, thrown as an error rather than presented
   * as an empty-but-`ready` group).
   */
  private buildThreadsResult(attemptedSpaceCount: number): ThreadsGroupResult {
    const succeededProjectIds = Array.from(this.threadsByProjectId.keys());
    if (attemptedSpaceCount > 0 && succeededProjectIds.length === 0) {
      throw new PaletteLoadError('every space failed to load its thread list');
    }
    const candidates: PaletteCandidate[] = [];
    for (const projectId of succeededProjectIds) {
      const space = this.threadsSpacesById.get(projectId);
      const threads = this.threadsByProjectId.get(projectId);
      if (!space || !threads) continue;
      candidates.push(...buildThreadCandidates(space, threads));
    }
    return { candidates, incomplete: this.threadsFailedProjectIds.size > 0 };
  }
}
