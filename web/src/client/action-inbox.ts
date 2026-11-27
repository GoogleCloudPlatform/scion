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
 * Generic message-action inbox API client, URL state helpers, and `?limit=0`
 * probe cache (design §5.5, AC-12).
 *
 * Kind-agnostic: mirrors `pkg/hub/action_inbox.go` (`GET /api/v1/users/me/<kind-path>`).
 * Contains no kind-specific branches so future message-action kinds reuse it directly.
 */

import { apiFetch, parseApiError } from './api.js';
import { stateManager } from './state.js';

export type ActionInboxTab = 'open' | 'resolved';
export type ActionInboxSort = 'recent' | 'project';

export interface ActionInboxCounts {
  answerable: number;
  open: number;
}

export interface ActionInboxFacetItem {
  id: string;
  name: string;
  count: number;
}

export interface ActionInboxFacets {
  agents: ActionInboxFacetItem[];
  projects: ActionInboxFacetItem[];
}

export interface ActionInboxResponse<TCard> {
  counts: ActionInboxCounts;
  filteredCounts?: ActionInboxCounts;
  cards: TCard[];
  facets: ActionInboxFacets;
  truncatedProjects: boolean;
  capped: boolean;
  nextCursor?: string;
}

export interface ActionInboxUrlState {
  tab: ActionInboxTab;
  project: string;
  agent: string;
  q: string;
  sort: ActionInboxSort;
  cursor: string;
}

export interface ActionInboxProbeResult {
  available: boolean;
  answerable: number;
  open: number;
}

export class ActionInboxApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(message: string, status: number, code = '') {
    super(message);
    this.name = 'ActionInboxApiError';
    this.status = status;
    this.code = code;
  }
}

/** Maximum age before a cached `?limit=0` probe is revalidated. */
export const ACTION_INBOX_PROBE_TTL_MS = 30_000;

const EDGE_SPACE_RE = /^[\s\u0085]+|[\s\u0085]+$/g;
// eslint-disable-next-line no-control-regex -- strip/reject C0/DEL/C1 control characters in URL params
const CONTROL_CHAR_RE = /[\u0000-\u001f\u007f-\u009f]/;

function cleanParam(raw: string | undefined | null): string {
  const trimmed = (raw ?? '').replace(EDGE_SPACE_RE, '');
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  return trimmed;
}

function cleanFilterParam(raw: string | undefined | null): string {
  const cleaned = cleanParam(raw);
  return cleaned === 'all' ? '' : cleaned;
}

/**
 * Parses URL query parameters for a message-action inbox page, dropping invalid
 * values and applying defaults (`tab: 'open'`, `sort: 'recent'`).
 */
export function parseActionInboxUrlState(search: string | URLSearchParams): ActionInboxUrlState {
  const params = typeof search === 'string' ? new URLSearchParams(search) : search;
  const rawTab = cleanParam(params.get('tab'));
  const tab: ActionInboxTab = rawTab === 'resolved' ? 'resolved' : 'open';

  const rawSort = cleanParam(params.get('sort'));
  const sort: ActionInboxSort = rawSort === 'project' ? 'project' : 'recent';

  const project = cleanFilterParam(params.get('project'));
  const agent = cleanFilterParam(params.get('agent'));
  const q = cleanParam(params.get('q'));
  const cursor = cleanParam(params.get('cursor'));

  return { tab, project, agent, q, sort, cursor };
}

/**
 * Serializes message-action inbox state to a query string (without leading `?`)
 * suitable for `replaceSearch()`.
 */
export function buildActionInboxUrlSearch(state: Partial<ActionInboxUrlState>): string {
  const params = new URLSearchParams();
  const tab: ActionInboxTab = state.tab === 'resolved' ? 'resolved' : 'open';
  const sort: ActionInboxSort = state.sort === 'project' ? 'project' : 'recent';

  if (tab !== 'open') params.set('tab', tab);
  if (sort !== 'recent') params.set('sort', sort);

  const project = cleanFilterParam(state.project);
  if (project) params.set('project', project);

  const agent = cleanFilterParam(state.agent);
  if (agent) params.set('agent', agent);

  const q = cleanParam(state.q);
  if (q) params.set('q', q);

  const cursor = cleanParam(state.cursor);
  if (cursor) params.set('cursor', cursor);

  return params.toString();
}

/**
 * Builds the API query string (with leading `?`) for `GET /api/v1/users/me/<kind-path>`.
 */
export function buildActionInboxQuery(
  state: Partial<ActionInboxUrlState>,
  opts?: { limit?: number }
): string {
  const params = new URLSearchParams();
  const tab: ActionInboxTab = state.tab === 'resolved' ? 'resolved' : 'open';
  const sort: ActionInboxSort = state.sort === 'project' ? 'project' : 'recent';
  params.set('tab', tab);
  params.set('sort', sort);

  const project = cleanFilterParam(state.project);
  if (project) params.set('project', project);

  const agent = cleanFilterParam(state.agent);
  if (agent) params.set('agent', agent);

  const q = cleanParam(state.q);
  if (q) params.set('q', q);

  const cursor = cleanParam(state.cursor);
  if (cursor) params.set('cursor', cursor);

  if (opts?.limit !== undefined) {
    params.set('limit', String(opts.limit));
  }

  return `?${params.toString()}`;
}

interface ProbeCacheEntry {
  result: ActionInboxProbeResult | null;
  fetchedAt: number;
  dirty: boolean;
  invalidationSeq: number;
  writeSeq: number;
  inFlight: Promise<ActionInboxProbeResult | null> | null;
}

const probeCache = new Map<string, ProbeCacheEntry>();
let probeListenerInstalled = false;

function getOrCreateProbeEntry(kind: string): ProbeCacheEntry {
  let entry = probeCache.get(kind);
  if (!entry) {
    entry = {
      result: null,
      fetchedAt: 0,
      dirty: false,
      invalidationSeq: 0,
      writeSeq: 0,
      inFlight: null,
    };
    probeCache.set(kind, entry);
  }
  return entry;
}

function ensureProbeListener(): void {
  if (probeListenerInstalled) return;
  probeListenerInstalled = true;
  stateManager.addEventListener('message-action-updated', (e: Event) => {
    const raw = (e as CustomEvent).detail as
      | ({ data?: { kind?: unknown } } & { kind?: unknown })
      | undefined;
    const data = raw?.data ?? raw;
    const kind = typeof data?.kind === 'string' ? data.kind : '';
    if (!kind) return;
    const entry = probeCache.get(kind);
    if (entry) {
      entry.invalidationSeq++;
      entry.dirty = true;
    }
  });
}

ensureProbeListener();

function isProbeFresh(entry: ProbeCacheEntry): boolean {
  if (entry.result === null || entry.dirty) return false;
  return Date.now() - entry.fetchedAt < ACTION_INBOX_PROBE_TTL_MS;
}

function commitProbe(
  entry: ProbeCacheEntry,
  next: ActionInboxProbeResult,
  validForInvalidationSeq: number
): void {
  entry.writeSeq++;
  entry.result = next;
  entry.fetchedAt = Date.now();
  if (entry.invalidationSeq === validForInvalidationSeq) {
    entry.dirty = false;
  }
}

/** Returns the cached probe result for `kind` only if fresh. */
export function getCachedActionInboxProbe(kind: string): ActionInboxProbeResult | null {
  const entry = probeCache.get(kind);
  if (!entry || !isProbeFresh(entry)) return null;
  return entry.result;
}

/** Returns the last observed endpoint availability (`true`, `false`, or `null` if unknown). */
export function getLastKnownActionInboxAvailability(kind: string): boolean | null {
  const entry = probeCache.get(kind);
  return entry?.result ? entry.result.available : null;
}

/** Marks the cached probe for `kind` dirty so the next call revalidates. */
export function invalidateActionInboxProbe(kind: string): void {
  const entry = probeCache.get(kind);
  if (!entry) return;
  entry.invalidationSeq++;
  entry.dirty = true;
}

/** @internal Clears all cached probes (for unit tests). */
export function resetActionInboxProbeCacheForTests(): void {
  probeCache.clear();
}

/**
 * Fetches a page of message-action inbox cards from `apiPath`.
 * When `opts.kind` is provided, updates the shared probe cache for that kind.
 */
export async function fetchActionInbox<TCard>(
  apiPath: string,
  state: Partial<ActionInboxUrlState> = {},
  opts?: { limit?: number; signal?: AbortSignal; kind?: string }
): Promise<ActionInboxResponse<TCard>> {
  const entry = opts?.kind ? getOrCreateProbeEntry(opts.kind) : null;
  const startInvalidationSeq = entry ? entry.invalidationSeq : 0;

  const url = `${apiPath}${buildActionInboxQuery(state, opts?.limit !== undefined ? { limit: opts.limit } : undefined)}`;
  const res = await apiFetch(url, {
    suppressAccessDeniedToast: true,
    ...(opts?.signal ? { signal: opts.signal } : {}),
  });

  if (!res.ok) {
    if (res.status === 404 && entry) {
      commitProbe(entry, { available: false, answerable: 0, open: 0 }, startInvalidationSeq);
    }
    const info = await parseApiError(res, `HTTP ${res.status}`);
    throw new ActionInboxApiError(info.message, res.status, info.code);
  }

  const data = (await res.json()) as Partial<ActionInboxResponse<TCard>> | null;
  const counts: ActionInboxCounts = {
    answerable: Math.max(0, Math.trunc(data?.counts?.answerable ?? 0)),
    open: Math.max(0, Math.trunc(data?.counts?.open ?? 0)),
  };

  if (entry) {
    commitProbe(
      entry,
      { available: true, answerable: counts.answerable, open: counts.open },
      startInvalidationSeq
    );
  }

  return {
    counts,
    ...(data?.filteredCounts
      ? {
          filteredCounts: {
            answerable: Math.max(0, Math.trunc(data.filteredCounts.answerable ?? 0)),
            open: Math.max(0, Math.trunc(data.filteredCounts.open ?? 0)),
          },
        }
      : {}),
    cards: Array.isArray(data?.cards) ? data.cards : [],
    facets: {
      agents: Array.isArray(data?.facets?.agents) ? data.facets.agents : [],
      projects: Array.isArray(data?.facets?.projects) ? data.facets.projects : [],
    },
    truncatedProjects: Boolean(data?.truncatedProjects),
    capped: Boolean(data?.capped),
    ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
  };
}

/**
 * Probes `GET <apiPath>?limit=0` for `kind`, deduplicating concurrent calls and
 * caching fresh results for `ACTION_INBOX_PROBE_TTL_MS` (invalidated by SSE).
 * - HTTP 404 -> `{ available: false, answerable: 0, open: 0 }`
 * - HTTP 2xx -> `{ available: true, answerable, open }`
 * - Transient/auth failure -> `null`
 */
export async function fetchActionInboxProbe(
  kind: string,
  apiPath: string,
  opts?: { signal?: AbortSignal; forceRefresh?: boolean }
): Promise<ActionInboxProbeResult | null> {
  const entry = getOrCreateProbeEntry(kind);

  if (!opts?.forceRefresh && isProbeFresh(entry)) {
    return entry.result;
  }
  if (!opts?.forceRefresh && entry.inFlight !== null) {
    return entry.inFlight;
  }

  const run = async (signal?: AbortSignal): Promise<ActionInboxProbeResult | null> => {
    const startWriteSeq = entry.writeSeq;
    const startInvalidationSeq = entry.invalidationSeq;
    try {
      const res = await apiFetch(`${apiPath}?limit=0`, {
        authErrors: 'caller',
        suppressAccessDeniedToast: true,
        ...(signal ? { signal } : {}),
      });
      if (res.status === 404) {
        if (entry.writeSeq !== startWriteSeq) return entry.result;
        const notFound: ActionInboxProbeResult = { available: false, answerable: 0, open: 0 };
        commitProbe(entry, notFound, startInvalidationSeq);
        return notFound;
      }
      if (!res.ok) return null;
      const data = (await res.json()) as {
        counts?: { answerable?: number; open?: number };
      } | null;
      if (entry.writeSeq !== startWriteSeq) return entry.result;
      const resolved: ActionInboxProbeResult = {
        available: true,
        answerable: Math.max(0, Math.trunc(data?.counts?.answerable ?? 0)),
        open: Math.max(0, Math.trunc(data?.counts?.open ?? 0)),
      };
      commitProbe(entry, resolved, startInvalidationSeq);
      return resolved;
    } catch {
      return null;
    }
  };

  if (opts?.signal) {
    return run(opts.signal);
  }

  const shared = run().finally(() => {
    if (entry.inFlight === shared) {
      entry.inFlight = null;
    }
  });
  entry.inFlight = shared;
  return shared;
}
