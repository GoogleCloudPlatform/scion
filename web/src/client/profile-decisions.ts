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
 * Profile Decisions API client & URL state helpers (ptone/scion#7996, Design §4.3 R1–R7, D10, §5.1, §7).
 *
 * Mirrors `pkg/hub/choices_profile.go` (`GET /api/v1/users/me/decisions`).
 */

import { apiFetch, parseApiError } from './api.js';
import {
  DecisionsApiError,
  MAX_DECISIONS_CONVERSATION_BYTES,
  isValidDecisionCalendarDate,
  normalizeDecisionsSearchQuery,
  parseDecisionsUrlState,
  type DecisionsUrlState,
  type MadeDecisionsFilter,
  type MadeDecisionsProvenanceFilter,
  type ProjectDecisionActorView,
  type ProjectDecisionConversationView,
  type ProjectDecisionCounts,
  type ProjectDecisionLogEntryView,
  type ProjectDecisionProjectRef,
  type ProjectDecisionPromptView,
  type SharedDecisionsFilter,
} from './project-decisions.js';
import { stateManager } from './state.js';

/** Maximum UTF-8 byte length accepted for `project` by `pkg/hub/choices_profile.go`. */
export const MAX_PROFILE_DECISIONS_PROJECT_BYTES = 128;

/** Window event dispatched whenever the caller's unfiltered `counts.waitingOnYou` changes. */
export const PROFILE_DECISIONS_COUNT_CHANGED_EVENT = 'scion:profile-decisions-count-changed';

/** Maximum age before a cached `?limit=0` nav probe is revalidated on `<scion-profile-nav>` connect. */
export const PROFILE_DECISION_PROBE_TTL_MS = 30_000;

/**
 * Returns true when a `stateManager` `'chat-message-received'` event carries a
 * structured choice prompt (`UserMessageEvent` in `pkg/hub/events.go` emits
 * `type: 'choice-prompt'` and top-level `decisions`).
 */
export function isChoicePromptMessageEvent(e: Event): boolean {
  const raw = (e as CustomEvent).detail as {
    data?: {
      type?: unknown;
      decisions?: unknown;
    };
    type?: unknown;
    decisions?: unknown;
  } | null;
  const data = raw?.data ?? raw;
  return data?.type === 'choice-prompt' || Array.isArray(data?.decisions);
}

/** Sort order for `/profile/decisions` (`project` groups by project, `recent` is chronological). */
export type ProfileDecisionsSort = 'project' | 'recent';

// Matches Go strings.TrimSpace (which trims U+0085 NEL) plus JS whitespace/U+FEFF BOM at string edges.
const EDGE_SPACE_RE = /^[\s\u0085]+|[\s\u0085]+$/g;
// eslint-disable-next-line no-control-regex -- reject C0/DEL/C1 control characters in URL parameters
const CONTROL_CHAR_RE = /[\u0000-\u001f\u007f-\u009f]/;

/** Actor facet option on profile responses, carrying an optional `projectId` when available. */
export interface ProfileDecisionActorView extends ProjectDecisionActorView {
  projectId?: string;
}

/** One project's overview group on `GET /api/v1/users/me/decisions?tab=made&sort=project` (R5). */
export interface ProfileDecisionMadeGroupView {
  project: ProjectDecisionProjectRef;
  entries: ProjectDecisionLogEntryView[];
  more: boolean;
}

/** Response of `GET /api/v1/users/me/decisions?tab=pending`. */
export interface ProfilePendingDecisionsResponse {
  counts: ProjectDecisionCounts;
  filteredCounts?: ProjectDecisionCounts;
  prompts: ProjectDecisionPromptView[];
  agents: ProfileDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  conversations?: ProjectDecisionConversationView[];
  projects: ProjectDecisionProjectRef[];
  truncatedProjects: boolean;
  nextCursor?: string;
}

/** Response of `GET /api/v1/users/me/decisions?tab=made&sort=recent`. */
export interface ProfileMadeRecentDecisionsResponse {
  sort: 'recent';
  counts: ProjectDecisionCounts;
  entries: ProjectDecisionLogEntryView[];
  agents: ProfileDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  conversations?: ProjectDecisionConversationView[];
  projects: ProjectDecisionProjectRef[];
  truncatedProjects: boolean;
  nextCursor?: string;
}

/** Response of `GET /api/v1/users/me/decisions?tab=made&sort=project` (overview, no cursor). */
export interface ProfileMadeByProjectDecisionsResponse {
  sort: 'project';
  counts: ProjectDecisionCounts;
  groups: ProfileDecisionMadeGroupView[];
  agents: ProfileDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  conversations?: ProjectDecisionConversationView[];
  projects: ProjectDecisionProjectRef[];
  truncatedProjects: boolean;
}

export type ProfileMadeDecisionsResponse =
  | ProfileMadeRecentDecisionsResponse
  | ProfileMadeByProjectDecisionsResponse;

/** Shared filter criteria accepted on both profile tabs. */
export interface ProfileSharedDecisionsFilter extends SharedDecisionsFilter {
  project?: string;
}

/** Filter criteria accepted on `tab=made` on the profile page. */
export interface ProfileMadeDecisionsFilter extends MadeDecisionsFilter {
  project?: string;
}

export interface ProfileDecisionsQueryOptions {
  tab?: 'pending' | 'made' | undefined;
  sort?: ProfileDecisionsSort | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
  filter?: ProfileMadeDecisionsFilter | undefined;
}

/** Validated URL filter/tab/sort state for `/profile/decisions` (D10, R6). */
export interface ProfileDecisionsUrlState extends DecisionsUrlState {
  sort: ProfileDecisionsSort;
  project: string;
}

/** Probe result from `GET /api/v1/users/me/decisions?limit=0` for `<scion-profile-nav>` (§7). */
export interface ProfileDecisionProbeResult {
  available: boolean;
  waitingOnYou: number;
}

/** Returns the natural default sort for a tab on `/profile/decisions` (R6). */
export function defaultProfileDecisionsSort(tab: 'pending' | 'made'): ProfileDecisionsSort {
  return tab === 'made' ? 'recent' : 'project';
}

function trimEdgeSpace(raw: string | undefined | null): string {
  return (raw ?? '').replace(EDGE_SPACE_RE, '');
}

function cleanFilterValue(raw: string | undefined | null): string {
  const trimmed = trimEdgeSpace(raw);
  if (!trimmed || trimmed === 'all') return '';
  return trimmed;
}

const textEncoder = new TextEncoder();

function cleanConversationValue(raw: string | undefined | null): string {
  const trimmed = cleanFilterValue(raw);
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  if (textEncoder.encode(trimmed).length > MAX_DECISIONS_CONVERSATION_BYTES) {
    return '';
  }
  return trimmed;
}

export function cleanProfileProjectValue(raw: string | undefined | null): string {
  const trimmed = cleanFilterValue(raw);
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  if (textEncoder.encode(trimmed).length > MAX_PROFILE_DECISIONS_PROJECT_BYTES) {
    return '';
  }
  return trimmed;
}

function cleanUserValue(raw: string | undefined | null): string {
  const trimmed = cleanFilterValue(raw);
  if (!trimmed || trimmed.length > 256 || CONTROL_CHAR_RE.test(trimmed)) return '';
  return trimmed;
}

function cleanProvenanceValue(raw: string | undefined | null): MadeDecisionsProvenanceFilter {
  const trimmed = cleanFilterValue(raw);
  switch (trimmed) {
    case 'direct':
    case 'interpreted':
    case 'confirmed':
    case 'overridden':
      return trimmed;
    case 'agent_interpreted':
      return 'interpreted';
    default:
      return '';
  }
}

function cleanSortValue(
  raw: string | undefined | null,
  tab: 'pending' | 'made'
): ProfileDecisionsSort {
  const trimmed = trimEdgeSpace(raw);
  if (trimmed === 'project' || trimmed === 'recent') {
    return trimmed;
  }
  return defaultProfileDecisionsSort(tab);
}

/**
 * Parses URL query parameters on load for `/profile/decisions`, silently
 * dropping invalid values and filling per-tab default `sort` (D10, R6).
 */
export function parseProfileDecisionsUrlState(
  search: string | URLSearchParams
): ProfileDecisionsUrlState {
  const params = typeof search === 'string' ? new URLSearchParams(search) : search;
  const base = parseDecisionsUrlState(params);
  const sort = cleanSortValue(params.get('sort'), base.tab);
  const project = cleanProfileProjectValue(params.get('project'));

  return {
    ...base,
    sort,
    project,
  };
}

/**
 * Serializes current profile Decisions tab, sort, and filter state to a query
 * string (without leading `?`) suitable for `replaceSearch()` (D10, R6).
 * Both `tab` and `sort` (including defaults) are always included in the URL.
 */
export function buildProfileDecisionsUrlSearch(state: Partial<ProfileDecisionsUrlState>): string {
  const params = new URLSearchParams();
  const tab: 'pending' | 'made' = state.tab === 'made' ? 'made' : 'pending';
  const sort = cleanSortValue(state.sort, tab);

  params.set('tab', tab);
  params.set('sort', sort);

  const project = cleanProfileProjectValue(state.project);
  if (project) params.set('project', project);

  const q = normalizeDecisionsSearchQuery(state.q);
  if (q) params.set('q', q);

  const conversation = cleanConversationValue(state.conversation);
  if (conversation) params.set('conversation', conversation);

  const agent = cleanFilterValue(state.agent);
  if (agent) params.set('agent', agent);

  if (tab === 'made') {
    const user = cleanUserValue(state.user);
    if (user) params.set('user', user);

    const provenance = cleanProvenanceValue(state.provenance);
    if (provenance) params.set('provenance', provenance);

    const from = cleanFilterValue(state.from);
    if (from && isValidDecisionCalendarDate(from)) params.set('from', from);

    const to = cleanFilterValue(state.to);
    if (to && isValidDecisionCalendarDate(to)) params.set('to', to);
  }

  return params.toString();
}

/**
 * Builds the query string (starting with `?`) for `GET /api/v1/users/me/decisions`.
 * Omits `cursor` when `tab=made` and `sort=project` (overview mode forbids cursors, R5).
 */
export function buildProfileDecisionsQuery(options: ProfileDecisionsQueryOptions = {}): string {
  const params = new URLSearchParams();
  const tab = options.tab ?? 'pending';
  const sort = cleanSortValue(options.sort, tab);
  params.set('tab', tab);
  params.set('sort', sort);

  if (options.limit !== undefined) {
    params.set('limit', String(options.limit));
  }
  const cursor = (options.cursor ?? '').trim();
  if (cursor && !(tab === 'made' && sort === 'project')) {
    params.set('cursor', cursor);
  }

  if (options.filter) {
    const q = normalizeDecisionsSearchQuery(options.filter.q);
    if (q) params.set('q', q);

    const conversation = cleanConversationValue(options.filter.conversation);
    if (conversation) params.set('conversation', conversation);

    const agent = cleanFilterValue(options.filter.agent);
    if (agent) params.set('agent', agent);

    const project = cleanProfileProjectValue(options.filter.project);
    if (project) params.set('project', project);

    if (tab === 'made') {
      const user = cleanFilterValue(options.filter.user);
      if (user) params.set('user', user);

      const provenance = cleanFilterValue(options.filter.provenance);
      if (provenance) params.set('provenance', provenance);

      const from = cleanFilterValue(options.filter.from);
      if (from) params.set('from', from);

      const to = cleanFilterValue(options.filter.to);
      if (to) params.set('to', to);
    }
  }

  return `?${params.toString()}`;
}

function profileDecisionsUrl(options: ProfileDecisionsQueryOptions): string {
  return `/api/v1/users/me/decisions${buildProfileDecisionsQuery(options)}`;
}

async function throwDecisionsApiError(res: Response): Promise<never> {
  const info = await parseApiError(res, `HTTP ${res.status}`);
  throw new DecisionsApiError(info.message, res.status, info.code);
}

/**
 * Module-level shared probe cache + monotonic sequence number + TTL + SSE dirty
 * tracking + in-flight deduplication so multiple `<scion-profile-nav>` instances
 * and route changes reuse a single `?limit=0` probe while invalidating on off-nav
 * choice updates or after `PROFILE_DECISION_PROBE_TTL_MS`.
 */
let cachedProfileDecisionProbe: ProfileDecisionProbeResult | null = null;
let cachedProfileDecisionProbeAt = 0;
let cachedProfileDecisionProbeDirty = false;
let probeInvalidationSeq = 0;
let profileDecisionProbeWriteSeq = 0;
let inFlightProfileDecisionProbe: Promise<ProfileDecisionProbeResult | null> | null = null;
let sharedProbeRefreshTimer: ReturnType<typeof setTimeout> | null = null;
let probeInvalidationListenersInstalled = false;

function ensureProbeInvalidationListeners(): void {
  if (probeInvalidationListenersInstalled) return;
  probeInvalidationListenersInstalled = true;
  stateManager.addEventListener('choice-updated', () => {
    ++probeInvalidationSeq;
    cachedProfileDecisionProbeDirty = true;
  });
  stateManager.addEventListener('chat-message-received', (e: Event) => {
    if (isChoicePromptMessageEvent(e)) {
      ++probeInvalidationSeq;
      cachedProfileDecisionProbeDirty = true;
    }
  });
}

ensureProbeInvalidationListeners();

function isProfileDecisionProbeFresh(): boolean {
  if (cachedProfileDecisionProbe === null) return false;
  if (cachedProfileDecisionProbeDirty) return false;
  return Date.now() - cachedProfileDecisionProbeAt < PROFILE_DECISION_PROBE_TTL_MS;
}

function commitProfileDecisionProbe(
  next: ProfileDecisionProbeResult,
  validForInvalidationSeq = probeInvalidationSeq
): void {
  ++profileDecisionProbeWriteSeq;
  cachedProfileDecisionProbe = next;
  cachedProfileDecisionProbeAt = Date.now();
  if (probeInvalidationSeq === validForInvalidationSeq) {
    cachedProfileDecisionProbeDirty = false;
  }
}

function recordPageFetchForProbe<T extends { counts: ProjectDecisionCounts }>(
  work: () => Promise<T>
): Promise<T> {
  const startInvalidationSeq = probeInvalidationSeq;
  let resolvePageProbe: ((res: ProfileDecisionProbeResult | null) => void) | null = null;
  let ownedProbePromise: Promise<ProfileDecisionProbeResult | null> | null = null;
  if (!isProfileDecisionProbeFresh() && inFlightProfileDecisionProbe === null) {
    ownedProbePromise = new Promise<ProfileDecisionProbeResult | null>((resolve) => {
      resolvePageProbe = resolve;
    });
    inFlightProfileDecisionProbe = ownedProbePromise;
  }

  return work().then(
    (result) => {
      const probeResult: ProfileDecisionProbeResult = {
        available: true,
        waitingOnYou: Math.max(0, Math.trunc(result.counts.waitingOnYou)),
      };
      commitProfileDecisionProbe(probeResult, startInvalidationSeq);
      if (ownedProbePromise && inFlightProfileDecisionProbe === ownedProbePromise) {
        inFlightProfileDecisionProbe = null;
      }
      resolvePageProbe?.(probeResult);
      return result;
    },
    (err: unknown) => {
      if (ownedProbePromise && inFlightProfileDecisionProbe === ownedProbePromise) {
        inFlightProfileDecisionProbe = null;
      }
      if (err instanceof DecisionsApiError && err.status === 404) {
        const notFoundProbe: ProfileDecisionProbeResult = { available: false, waitingOnYou: 0 };
        commitProfileDecisionProbe(notFoundProbe, startInvalidationSeq);
        resolvePageProbe?.(notFoundProbe);
      } else {
        resolvePageProbe?.(null);
      }
      throw err;
    }
  );
}

/** Fetches `GET /api/v1/users/me/decisions?tab=pending&sort=...`. */
export async function fetchProfilePendingDecisions(
  options: {
    sort?: ProfileDecisionsSort | undefined;
    cursor?: string | undefined;
    limit?: number | undefined;
    filter?: ProfileSharedDecisionsFilter | undefined;
    signal?: AbortSignal | undefined;
  } = {}
): Promise<ProfilePendingDecisionsResponse> {
  return recordPageFetchForProbe(async () => {
    const url = profileDecisionsUrl({
      tab: 'pending',
      sort: options.sort ?? 'project',
      cursor: options.cursor,
      limit: options.limit,
      filter: options.filter,
    });
    const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
    if (!res.ok) {
      return throwDecisionsApiError(res);
    }
    const data = (await res.json()) as Partial<ProfilePendingDecisionsResponse> | null;
    return {
      counts: {
        waitingOnYou: data?.counts?.waitingOnYou ?? 0,
        waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
      },
      ...(data?.filteredCounts
        ? {
            filteredCounts: {
              waitingOnYou: data.filteredCounts.waitingOnYou ?? 0,
              waitingOnOthers: data.filteredCounts.waitingOnOthers ?? 0,
            },
          }
        : {}),
      prompts: Array.isArray(data?.prompts) ? data.prompts : [],
      agents: Array.isArray(data?.agents) ? data.agents : [],
      answerers: Array.isArray(data?.answerers) ? data.answerers : [],
      ...(Array.isArray(data?.conversations) ? { conversations: data.conversations } : {}),
      projects: Array.isArray(data?.projects) ? data.projects : [],
      truncatedProjects: Boolean(data?.truncatedProjects),
      ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
    };
  });
}

/** Fetches `GET /api/v1/users/me/decisions?tab=made&sort=...` (`project` overview or `recent` paged). */
export async function fetchProfileMadeDecisions(
  filter: ProfileMadeDecisionsFilter = {},
  options: {
    sort?: ProfileDecisionsSort | undefined;
    cursor?: string | undefined;
    limit?: number | undefined;
    signal?: AbortSignal | undefined;
  } = {}
): Promise<ProfileMadeDecisionsResponse> {
  return recordPageFetchForProbe(async () => {
    const sort: ProfileDecisionsSort = options.sort ?? 'recent';
    const url = profileDecisionsUrl({
      tab: 'made',
      sort,
      cursor: options.cursor,
      limit: options.limit,
      filter,
    });
    const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
    if (!res.ok) {
      return throwDecisionsApiError(res);
    }
    if (sort === 'project') {
      const data = (await res.json()) as Partial<ProfileMadeByProjectDecisionsResponse> | null;
      const rawGroups = Array.isArray(data?.groups) ? data.groups : [];
      return {
        sort: 'project',
        counts: {
          waitingOnYou: data?.counts?.waitingOnYou ?? 0,
          waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
        },
        groups: rawGroups.map((g) => ({
          project: g?.project ?? { id: '', name: '', slug: '' },
          entries: Array.isArray(g?.entries) ? g.entries : [],
          more: Boolean(g?.more),
        })),
        agents: Array.isArray(data?.agents) ? data.agents : [],
        answerers: Array.isArray(data?.answerers) ? data.answerers : [],
        ...(Array.isArray(data?.conversations) ? { conversations: data.conversations } : {}),
        projects: Array.isArray(data?.projects) ? data.projects : [],
        truncatedProjects: Boolean(data?.truncatedProjects),
      };
    }

    const data = (await res.json()) as Partial<ProfileMadeRecentDecisionsResponse> | null;
    return {
      sort: 'recent',
      counts: {
        waitingOnYou: data?.counts?.waitingOnYou ?? 0,
        waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
      },
      entries: Array.isArray(data?.entries) ? data.entries : [],
      agents: Array.isArray(data?.agents) ? data.agents : [],
      answerers: Array.isArray(data?.answerers) ? data.answerers : [],
      ...(Array.isArray(data?.conversations) ? { conversations: data.conversations } : {}),
      projects: Array.isArray(data?.projects) ? data.projects : [],
      truncatedProjects: Boolean(data?.truncatedProjects),
      ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
    };
  });
}

/**
 * Returns the cached `GET /api/v1/users/me/decisions?limit=0` probe result only
 * when it is fresh (not marked dirty by SSE and within `PROFILE_DECISION_PROBE_TTL_MS`).
 */
export function getCachedProfileDecisionProbe(): ProfileDecisionProbeResult | null {
  return isProfileDecisionProbeFresh() ? cachedProfileDecisionProbe : null;
}

/**
 * Returns the last observed endpoint availability (`true` on 200, `false` on 404,
 * `null` before the first probe resolves) regardless of badge-count freshness,
 * so `<scion-profile-nav>` avoids flashing on older 404 hubs while keeping a
 * known-available entry visible during a count refresh.
 */
export function getLastKnownProfileDecisionAvailability(): boolean | null {
  return cachedProfileDecisionProbe ? cachedProfileDecisionProbe.available : null;
}

/** Resets the module-level probe cache and any pending refresh timer (for unit tests). */
export function resetProfileDecisionProbeCacheForTests(): void {
  cachedProfileDecisionProbe = null;
  cachedProfileDecisionProbeAt = 0;
  cachedProfileDecisionProbeDirty = false;
  probeInvalidationSeq = 0;
  profileDecisionProbeWriteSeq = 0;
  inFlightProfileDecisionProbe = null;
  if (sharedProbeRefreshTimer !== null) {
    clearTimeout(sharedProbeRefreshTimer);
    sharedProbeRefreshTimer = null;
  }
}

export interface FetchProfileDecisionProbeOptions {
  signal?: AbortSignal | undefined;
  forceRefresh?: boolean | undefined;
}

async function executeProfileDecisionProbe(
  signal?: AbortSignal
): Promise<{ result: ProfileDecisionProbeResult | null; superseded: boolean }> {
  const startSeq = profileDecisionProbeWriteSeq;
  const startInvalidationSeq = probeInvalidationSeq;
  try {
    const res = await apiFetch('/api/v1/users/me/decisions?limit=0', {
      authErrors: 'caller',
      ...(signal ? { signal } : {}),
    });
    if (res.status === 404) {
      if (profileDecisionProbeWriteSeq !== startSeq) {
        return { result: cachedProfileDecisionProbe, superseded: true };
      }
      const notFound: ProfileDecisionProbeResult = { available: false, waitingOnYou: 0 };
      commitProfileDecisionProbe(notFound, startInvalidationSeq);
      return { result: notFound, superseded: false };
    }
    if (!res.ok) {
      return { result: null, superseded: false };
    }
    const data = (await res.json()) as { counts?: { waitingOnYou?: number } } | null;
    if (profileDecisionProbeWriteSeq !== startSeq) {
      return { result: cachedProfileDecisionProbe, superseded: true };
    }
    const waitingOnYou =
      typeof data?.counts?.waitingOnYou === 'number' && data.counts.waitingOnYou > 0
        ? data.counts.waitingOnYou
        : 0;
    const resolved: ProfileDecisionProbeResult = { available: true, waitingOnYou };
    commitProfileDecisionProbe(resolved, startInvalidationSeq);
    return { result: resolved, superseded: false };
  } catch {
    return { result: null, superseded: false };
  }
}

/**
 * Probes `GET /api/v1/users/me/decisions?limit=0` for the `<scion-profile-nav>`
 * entry and badge (Design §4.3 R1, §7), deduplicating concurrent calls and
 * reusing a fresh cached result across route transitions until invalidated by
 * SSE or `PROFILE_DECISION_PROBE_TTL_MS`:
 * - HTTP 404 -> `{ available: false, waitingOnYou: 0 }` (hide nav item)
 * - HTTP 2xx -> `{ available: true, waitingOnYou }`
 * - Transient/auth failure -> `null` (leave nav state unchanged)
 */
export async function fetchProfileDecisionProbe(
  signalOrOptions?: AbortSignal | FetchProfileDecisionProbeOptions
): Promise<ProfileDecisionProbeResult | null> {
  const opts: FetchProfileDecisionProbeOptions =
    signalOrOptions && 'aborted' in signalOrOptions
      ? { signal: signalOrOptions }
      : (signalOrOptions ?? {});

  if (!opts.forceRefresh && isProfileDecisionProbeFresh()) {
    return cachedProfileDecisionProbe;
  }
  if (!opts.forceRefresh && inFlightProfileDecisionProbe !== null) {
    return inFlightProfileDecisionProbe;
  }

  if (opts.signal) {
    const out = await executeProfileDecisionProbe(opts.signal);
    return out.result;
  }

  const shared = executeProfileDecisionProbe()
    .then((out) => out.result)
    .finally(() => {
      if (inFlightProfileDecisionProbe === shared) {
        inFlightProfileDecisionProbe = null;
      }
    });
  inFlightProfileDecisionProbe = shared;
  return shared;
}

/**
 * Debounces a single shared `?limit=0` refresh across all mounted `<scion-profile-nav>`
 * instances when an SSE decision event arrives outside `/profile/decisions`.
 * Ignores late probe responses that started before a fresher page-derived write.
 */
export function scheduleSharedProfileDecisionProbeRefresh(): void {
  if (sharedProbeRefreshTimer !== null) {
    clearTimeout(sharedProbeRefreshTimer);
  }
  sharedProbeRefreshTimer = setTimeout(() => {
    sharedProbeRefreshTimer = null;
    void executeProfileDecisionProbe().then(({ result, superseded }) => {
      if (!result || superseded) return;
      if (typeof window === 'undefined') return;
      window.dispatchEvent(
        new CustomEvent<{ available: boolean; waitingOnYou: number }>(
          PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
          {
            detail: result,
          }
        )
      );
    });
  }, 100);
}

/** Dispatches a window event so `<scion-profile-nav>` updates its Decisions badge immediately. */
export function dispatchProfileDecisionsCountChanged(waitingOnYou: number): void {
  const normalized = Math.max(0, Math.trunc(waitingOnYou));
  commitProfileDecisionProbe({ available: true, waitingOnYou: normalized });
  if (typeof window === 'undefined') return;
  window.dispatchEvent(
    new CustomEvent<{ available: boolean; waitingOnYou: number }>(
      PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
      {
        detail: { available: true, waitingOnYou: normalized },
      }
    )
  );
}

/** Marks `/api/v1/users/me/decisions` as unsupported (404) and notifies `<scion-profile-nav>`. */
export function dispatchProfileDecisionsNotFound(): void {
  commitProfileDecisionProbe({ available: false, waitingOnYou: 0 });
  if (typeof window === 'undefined') return;
  window.dispatchEvent(
    new CustomEvent<{ available: boolean; waitingOnYou: number }>(
      PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
      {
        detail: { available: false, waitingOnYou: 0 },
      }
    )
  );
}
