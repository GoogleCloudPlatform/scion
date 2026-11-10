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
 * Reads the hub agent list from the shared agent store, the hub's recently
 * seen users and server-side user search from the shared user directory
 * (`user-directory.ts`, independent of current-space membership), GET
 * /api/v1/chat/dms, GET /api/v1/chat/spaces and per-space
 * GET /api/v1/chat/spaces/{id}/threads, joins them into normalized
 * {@link PaletteCandidate}s, and exposes
 * cancellation so a page controller can discard a stale in-flight load
 * (identity change, palette closed before the response arrived, or a newer
 * load superseding an older one).
 */

import { apiFetch } from './api.js';
import type { ApiFetchOptions } from './api.js';
import { agentStore } from './agent-store.js';
import type { AgentListSnapshot, AgentStore } from './agent-store.js';
import { canMessageAgent } from '../shared/types.js';
import { activityMsFromTimestamp } from '../utils/palette-match.js';
import { formatFileSize } from '../utils/chat-file-links.js';
import { formatInstant } from '../utils/time.js';
import type { PaletteCandidate, PaletteThreadTarget, RawPaletteAgent } from './palette-types.js';
import { dmCandidateId, documentCandidateId, threadCandidateId } from './palette-types.js';
import { agentRowText, type ProjectSlugLookup } from './agent-palette-candidate.js';
import type { RecentFile } from './chat-recent-files.js';
import { loadRecentUsers, searchUsers } from './user-directory.js';

/** Shortest trimmed query the People group sends to the hub's user search. */
export const PEOPLE_SEARCH_MIN_CHARS = 2;

/** A query as sent to the hub's user search: trimmed, each run of whitespace one space. */
export function normalizePeopleQuery(query: string): string {
  return query.trim().replace(/\s+/g, ' ');
}

/** Maximum concurrent per-space thread-list requests. */
const MAX_CONCURRENT_THREAD_REQUESTS = 4;

/**
 * Idle bound for the Agents group's DM fetch: a request that makes no
 * forward progress at all (a dropped connection, a proxy that never
 * responds) is aborted and surfaced as a retryable error instead of leaving
 * the group on "Loading…" forever. The agent rows themselves come from the
 * agent store, whose walk has its own per-page timeout.
 *
 * Exported so `chat.ts` can bound the People group's `/auth/me` identity
 * fetch by the same value (see `_resolveSelfUserId` in chat.ts).
 */
export const AGENTS_IDLE_TIMEOUT_MS = 90 * 1000;

/**
 * Distinguishes an idle-timeout-triggered abort of the Agents group's
 * controller from an explicit cancel/supersede, so
 * {@link ChatPaletteDataController.loadAgentsGroup} can surface the former
 * as a load error rather than swallowing it the way an ordinary
 * superseded/cancelled load is swallowed.
 */
const AGENTS_IDLE_TIMEOUT_REASON = Symbol('agents-group-idle-timeout');

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
    // A cancelled or superseded load aborts `signal` out from under an
    // in-flight body read: `res.json()` then rejects with an AbortError,
    // not because the body was malformed. Rethrow it as-is so the caller's
    // AbortError handling (see ChatPaletteDataController) sees "no update,"
    // not a load failure; only a genuinely bad body becomes a
    // PaletteLoadError.
    if (signal?.aborted) {
      throw err;
    }
    throw new PaletteLoadError('dm list response was not valid JSON');
  }
  // A JSON body can be null, an array or a primitive and still parse; none
  // of those is a DM list, and treating one as "zero DMs" would silently
  // hide a misconfigured endpoint. Reject anything that isn't an object.
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
 *
 * Row text is the shared {@link agentRowText}: the secondary line names the
 * agent's project by the row's slug, falling back to the slug `projectSlug`
 * knows for a row without one.
 */
export function buildAgentCandidates(
  agents: readonly RawPaletteAgent[],
  dms: readonly RawPaletteDm[],
  projectSlug?: ProjectSlugLookup
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
    const { label: displayName, secondaryLabel, searchFields } = agentRowText(agent, projectSlug);
    const dm = dmByAgentId.get(agent.id);

    candidates.push({
      id: dmCandidateId('agent', agent.id),
      group: 'agents',
      label: displayName,
      secondaryLabel,
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
    activityMs > 0 ? formatInstant(new Date(activityMs).toISOString(), 'date') : '',
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
  const results = new Array<
    { item: T; index: number; result: R } | { item: T; index: number; error: unknown }
  >(items.length);
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
 * Wait for `promise`, rejecting with an AbortError as soon as `signal`
 * aborts. For a shared load (the recent-users page) that one caller must not
 * abort for the others: the load runs on, this caller stops waiting.
 */
function untilAborted<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(new DOMException('load aborted', 'AbortError'));
  let onAbort = (): void => {};
  const aborted = new Promise<never>((_resolve, reject) => {
    onAbort = () => reject(new DOMException('load aborted', 'AbortError'));
    signal.addEventListener('abort', onAbort, { once: true });
  });
  return Promise.race([promise, aborted]).finally(() =>
    signal.removeEventListener('abort', onAbort)
  );
}

/** What a People load read: the recent users, the DM list and who is asking. */
interface PeopleBase {
  users: RawPaletteUser[];
  dms: RawPaletteDm[];
  selfUserId: string;
}

/** The part of the agent store the palette's Agents group reads. */
export type PaletteAgentSource = Pick<AgentStore, 'ensure' | 'peek'>;

/** The store entry behind the palette's Agents group. */
const PALETTE_AGENT_QUERY = { scope: 'hub' } as const;

/**
 * How long the DM list of the last Agents load serves later loads for
 * recency, unless {@link ChatPaletteDataController.markAgentDmsStale} was
 * called since.
 */
export const AGENT_DMS_CACHE_MS = 30 * 1000;

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
  /** DMs from the last successful Agents load: the recency join for {@link deriveAgentCandidates}. */
  private agentDms: RawPaletteDm[] | null = null;
  private agentDmsAt = 0;
  private agentDmsStale = false;
  /** Bumped by {@link markAgentDmsStale}, so a mark during a DM fetch is not lost. */
  private agentDmsMarks = 0;
  private peopleGeneration = 0;
  private peopleAbort: AbortController | null = null;
  /**
   * Inputs of the last successful People load, kept so a search can rebuild
   * the group without another request. Null until a load has succeeded.
   */
  private peopleBase: PeopleBase | null = null;
  /** Search hits for the current query, merged into the group after the recent users. */
  private peopleSearchHits: RawPaletteUser[] = [];
  private peopleSearchGeneration = 0;
  private peopleSearchAbort: AbortController | null = null;
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
    this.abortPeopleSearch();
    this.threadsGeneration++;
    this.threadsAbort?.abort();
    this.threadsAbort = null;
  }

  /**
   * `projectSlug` resolves an agent's project ID to the slug the page
   * already knows; rows of a project whose slug it does not know show the
   * project name.
   */
  constructor(
    private readonly agents: PaletteAgentSource = agentStore,
    private readonly projectSlug: ProjectSlugLookup = () => undefined
  ) {}

  /**
   * Load the Agents group. Resolves to the candidate list, or rejects with
   * the agent list's or DM list's load error. A load superseded by a later
   * call to {@link loadAgentsGroup} or {@link cancel} rejects with an
   * AbortError-like error the caller should treat as "no update", not a
   * failure to display.
   *
   * The agent rows come from the shared store's hub entry, which answers
   * from memory once loaded and keeps itself current from the agent feed.
   * The DM list supplies recency; it is fetched unless the last one is
   * still fresh (see {@link AGENT_DMS_CACHE_MS}).
   *
   * `onProgress`, when given, is called with a cumulative candidate list
   * (recency unknown, `activityMs=0`, since DMs are not fetched yet) after
   * each page of the store's first walk, so a caller can publish partial
   * results before a long list finishes. Never called with a superseded
   * load's data. The resolved value is always the DM-joined list.
   *
   * The DM fetch is bounded by {@link AGENTS_IDLE_TIMEOUT_MS}: a request
   * that makes no progress for that long rejects with
   * {@link PaletteLoadError} rather than leaving the group loading.
   */
  async loadAgentsGroup(
    onProgress?: (candidates: PaletteCandidate[]) => void
  ): Promise<PaletteCandidate[]> {
    this.agentsAbort?.abort();
    const controller = new AbortController();
    this.agentsAbort = controller;
    const myGeneration = ++this.agentsGeneration;
    const isCurrent = (): boolean => myGeneration === this.agentsGeneration;
    let idleTimer: ReturnType<typeof setTimeout> | null = null;

    try {
      const snapshot = await this.agents.ensure(PALETTE_AGENT_QUERY, {
        signal: controller.signal,
        ...(onProgress
          ? {
              onProgress: (progress: AgentListSnapshot): void => {
                if (isCurrent())
                  onProgress(buildAgentCandidates(progress.agents, [], this.projectSlug));
              },
            }
          : {}),
      });
      if (!isCurrent()) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      // The DM join only supplies recency, not agent membership, but a
      // failure here still fails the whole group: degrading every agent to
      // activityMs=0 would read as "no agent has a DM yet" rather than
      // "recency is unknown right now". The dialog's retry reloads both.
      let dms = this.freshAgentDms();
      if (!dms) {
        const marks = this.agentDmsMarks;
        idleTimer = setTimeout(
          () => controller.abort(AGENTS_IDLE_TIMEOUT_REASON),
          AGENTS_IDLE_TIMEOUT_MS
        );
        dms = await fetchPaletteDms(controller.signal);
        if (!isCurrent()) {
          throw new DOMException('superseded by a later load', 'AbortError');
        }
        this.agentDms = dms;
        this.agentDmsAt = Date.now();
        this.agentDmsStale = this.agentDmsMarks !== marks;
      }

      const latest = this.agents.peek(PALETTE_AGENT_QUERY);
      return buildAgentCandidates(
        latest?.status === 'ready' ? latest.agents : snapshot.agents,
        dms,
        this.projectSlug
      );
    } catch (err) {
      if (
        controller.signal.aborted &&
        controller.signal.reason === AGENTS_IDLE_TIMEOUT_REASON &&
        isCurrent()
      ) {
        throw new PaletteLoadError('direct messages took too long to load');
      }
      return reclassifyIfStale(err, controller.signal.aborted, !isCurrent());
    } finally {
      if (idleTimer) clearTimeout(idleTimer);
      if (this.agentsAbort === controller) this.agentsAbort = null;
    }
  }

  /**
   * Rebuild the Agents group from a store snapshot, joined against the DMs
   * of the last successful {@link loadAgentsGroup}. Returns `null` until a
   * load has succeeded, or when an Agents load is in flight (its result
   * supersedes any rebuild).
   */
  deriveAgentCandidates(snapshot: AgentListSnapshot): PaletteCandidate[] | null {
    if (!this.agentDms || this.agentsAbort) return null;
    return buildAgentCandidates(snapshot.agents, this.agentDms, this.projectSlug);
  }

  /**
   * The Agents group as it can be shown at once: the store's ready hub
   * snapshot joined with a fresh DM list. Returns `null` when either is
   * missing; the caller then shows the group as loading.
   */
  peekAgentsGroup(): PaletteCandidate[] | null {
    const dms = this.freshAgentDms();
    const snapshot = this.agents.peek(PALETTE_AGENT_QUERY);
    if (!dms || snapshot?.status !== 'ready') return null;
    return buildAgentCandidates(snapshot.agents, dms, this.projectSlug);
  }

  /** A chat or DM change may have moved recency: the next Agents load fetches the DM list. */
  markAgentDmsStale(): void {
    this.agentDmsStale = true;
    this.agentDmsMarks++;
  }

  private freshAgentDms(): RawPaletteDm[] | null {
    if (!this.agentDms || this.agentDmsStale) return null;
    return Date.now() - this.agentDmsAt < AGENT_DMS_CACHE_MS ? this.agentDms : null;
  }

  /**
   * Load the People group: one page of the hub's most recently seen users
   * (`loadRecentUsers`, cached page-wide for its TTL) joined with the DM
   * list for recency, plus any search hits for the current query (see
   * {@link searchPeople}). Users beyond that page are reached by search.
   * Same shape and guarantees as {@link loadAgentsGroup}: a failure of
   * either list fails the whole group, and a superseded/aborted load
   * rejects with an AbortError-like error.
   */
  async loadPeopleGroup(selfUserId: string): Promise<PaletteCandidate[]> {
    this.peopleAbort?.abort();
    const controller = new AbortController();
    this.peopleAbort = controller;
    const myGeneration = ++this.peopleGeneration;
    const isCurrent = (): boolean => myGeneration === this.peopleGeneration;

    try {
      const recent = await untilAborted(loadRecentUsers(), controller.signal);

      if (!isCurrent() || controller.signal.aborted) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }
      if (!recent) {
        throw new PaletteLoadError('recent users could not be loaded');
      }

      const dms = await fetchPaletteDms(controller.signal);

      if (!isCurrent()) {
        throw new DOMException('superseded by a later load', 'AbortError');
      }

      this.peopleBase = { users: recent.users, dms, selfUserId };
      return this.buildPeopleCandidates(this.peopleBase);
    } catch (err) {
      return reclassifyIfStale(err, controller.signal.aborted, !isCurrent());
    } finally {
      if (this.peopleAbort === controller) this.peopleAbort = null;
    }
  }

  /**
   * Search the hub's users for `query` (server-side `?search=`, bounded by
   * `USER_SEARCH_LIMIT` and `USER_SEARCH_TIMEOUT_MS`) and return the People
   * group with the hits merged in. Only active users are asked for; the
   * group's own status check still applies. The query is normalized by
   * {@link normalizePeopleQuery}. Any earlier search is aborted first. A
   * query shorter than {@link PEOPLE_SEARCH_MIN_CHARS} sends nothing and
   * behaves like {@link clearPeopleSearch}.
   *
   * Resolves to `null` until a People load has succeeded; the hits are
   * kept and the next successful load includes them. A failed or timed-out
   * search counts as no hits, so the group keeps its recent users. A search
   * superseded by a later one, {@link abortPeopleSearch} or {@link cancel}
   * rejects with an AbortError-like error.
   */
  async searchPeople(query: string): Promise<PaletteCandidate[] | null> {
    const q = normalizePeopleQuery(query);
    if (q.length < PEOPLE_SEARCH_MIN_CHARS) return this.clearPeopleSearch();
    this.abortPeopleSearch();
    const controller = new AbortController();
    this.peopleSearchAbort = controller;
    const myGeneration = this.peopleSearchGeneration;
    const isCurrent = (): boolean => myGeneration === this.peopleSearchGeneration;

    try {
      const hits = await searchUsers(q, { signal: controller.signal, status: 'active' });
      if (!isCurrent()) {
        throw new DOMException('superseded by a later search', 'AbortError');
      }
      this.peopleSearchHits = hits;
      return this.peopleBase ? this.buildPeopleCandidates(this.peopleBase) : null;
    } catch (err) {
      return reclassifyIfStale(err, controller.signal.aborted, !isCurrent());
    } finally {
      if (this.peopleSearchAbort === controller) this.peopleSearchAbort = null;
    }
  }

  /**
   * Abort an in-flight {@link searchPeople}, keeping the hits already
   * merged. Called on every query edit, so a slow search cannot land after
   * the query has moved on.
   */
  abortPeopleSearch(): void {
    this.peopleSearchGeneration++;
    this.peopleSearchAbort?.abort();
    this.peopleSearchAbort = null;
  }

  /**
   * Abort any search and drop its hits. Returns the People group rebuilt
   * from the last load's recent users, or `null` when there were no hits to
   * drop or no load has succeeded.
   */
  clearPeopleSearch(): PaletteCandidate[] | null {
    this.abortPeopleSearch();
    if (this.peopleSearchHits.length === 0) return null;
    this.peopleSearchHits = [];
    return this.peopleBase ? this.buildPeopleCandidates(this.peopleBase) : null;
  }

  /** The recent users followed by the search hits not among them, as candidates. */
  private buildPeopleCandidates(base: PeopleBase): PaletteCandidate[] {
    const seen = new Set(base.users.map((u) => u.id));
    const extra = this.peopleSearchHits.filter((u) => !seen.has(u.id));
    return buildUserCandidates([...base.users, ...extra], base.dms, base.selfUserId);
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
    return outcomes.map((outcome) => ChatPaletteDataController.toThreadFetchResult(outcome));
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
