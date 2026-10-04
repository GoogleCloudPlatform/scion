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
 * Complete-set agent drain.
 *
 * Two layers:
 *
 * - {@link drainAgents} walks the legacy (`created desc`, no `sort` param)
 *   agent list by following `nextCursor`, at most {@link DRAIN_MAX_REQUESTS}
 *   requests of {@link DRAIN_PAGE_LIMIT} rows each, retrying each page up to
 *   {@link DRAIN_PAGE_RETRIES} times. It is a pure network walk: it touches
 *   no shared state.
 * - {@link AgentDrainRunner} runs that walk inside the seed-epoch protocol of
 *   the client state store, so that live (SSE) changes that land while the
 *   walk is in flight are neither overwritten by the older REST snapshot nor
 *   lost: it waits briefly for the live connection, opens a seed epoch,
 *   drains, seeds the result with the epoch token, then folds in agents
 *   created live during the walk and drops agents deleted live during it.
 *
 * Neither layer ever reports a set as complete unless the walk reached a
 * page with no `nextCursor`. A cap, an error, or an abort is always visible
 * in the result.
 */

import type { Agent, Capabilities } from '../shared/types.js';
import { apiFetch } from './api.js';
import type { StateManager } from './state.js';
import { stateManager } from './state.js';
import { AgentSeedEpoch } from './agent-seed-epoch.js';

/** Rows requested per drain page (the server's maximum page size). */
export const DRAIN_PAGE_LIMIT = 500;

/** Requests a drain issues before it stops and reports `capped` (2,000 candidate rows). */
export const DRAIN_MAX_REQUESTS = 4;

/** Extra attempts per page after a failed one (so each page is tried at most three times). */
export const DRAIN_PAGE_RETRIES = 2;

/**
 * How long {@link AgentDrainRunner} waits for the live connection before draining anyway and
 * marking the result stale.
 */
export const DRAIN_CONNECT_TIMEOUT_MS = 3000;

/**
 * The item view requested from the server. `full` sends no `view`
 * parameter (the server default, so the first page is byte-identical to
 * the legacy request); `compact` sends `view=compact`.
 */
export type AgentDrainView = 'full' | 'compact';

/**
 * Why a drain stopped short. `status` is the HTTP status of the last failed attempt, when there
 * was a response.
 */
export interface AgentDrainError {
  message: string;
  status?: number;
}

/**
 * Outcome of a drain. Exactly one of these holds:
 * - `complete`: the walk reached a page with no `nextCursor`; `agents` is
 *   the whole set the server returned.
 * - `capped`: {@link DRAIN_MAX_REQUESTS} requests succeeded and a
 *   `nextCursor` was still left; `agents` is the readable part of the
 *   newest 2,000 candidate rows (it can be far fewer than 2,000 rows when
 *   the server filters by read access).
 * - `error` is set: a page still failed after its retries; `agents` holds
 *   whatever earlier pages returned (possibly nothing).
 */
export interface AgentDrainResult {
  agents: Agent[];
  complete: boolean;
  capped: boolean;
  error: AgentDrainError | null;
  /** Scope-level capabilities from the first page that carried them (`_capabilities`). */
  capabilities?: Capabilities | undefined;
  /**
   * Number of requests that returned a usable page, a discarded short
   * first page included (retried attempts are not counted). A host reads
   * one request as "the first answer was the whole set".
   */
  requests: number;
  /**
   * The drain stopped with `error` before any page whose rows it uses
   * arrived: there is no result to show, and a host keeps its previous
   * data. False whenever at least one page arrived, even one with zero
   * readable items, so a later page failure is an incomplete result, not a
   * first-page failure.
   */
  firstPageFailed: boolean;
}

/**
 * A legacy page the caller already fetched (for example an old server's
 * answer to a sorted request), from which the drain continues instead of
 * requesting the first page again. It must come from the same filters as
 * the drain's own URL.
 */
export interface DrainFirstPage {
  agents: Agent[];
  /** The cursor the drain continues from. */
  nextCursor: string;
  capabilities?: Capabilities | undefined;
}

/** The `fetch`-shaped function a drain uses. Defaults to `apiFetch`. */
export type AgentDrainFetch = (url: string, init: { signal?: AbortSignal }) => Promise<Response>;

/** Options for {@link drainAgents}. */
export interface DrainAgentsOptions {
  /**
   * Aborting rejects the drain with the fetch's `AbortError` (it never resolves with a partial
   * result).
   */
  signal?: AbortSignal;
  view?: AgentDrainView;
  /** Defaults to {@link DRAIN_MAX_REQUESTS}. */
  maxRequests?: number;
  /** Defaults to {@link DRAIN_PAGE_RETRIES}. */
  retries?: number;
  /** Delay before retry attempt `n` is `n * retryDelayMs`. Defaults to 250. */
  retryDelayMs?: number;
  fetchFn?: AgentDrainFetch;
  /**
   * An already-fetched first page. When it holds at least
   * {@link DRAIN_PAGE_LIMIT} agents (the server ignored the request's
   * smaller `limit`), its agents come first, it counts as one of the
   * `maxRequests` pages, and the rest are requested from its cursor. A
   * shorter page is discarded: the drain starts again from the first page
   * with no cursor and sends at most `maxRequests` pages of
   * {@link DRAIN_PAGE_LIMIT}, so the result never covers more than
   * `maxRequests * DRAIN_PAGE_LIMIT` candidate rows. The discarded page
   * still counts in `requests`.
   */
  firstPage?: DrainFirstPage;
}

type LegacyListBody =
  | { agents?: Agent[]; nextCursor?: string; _capabilities?: Capabilities }
  | Agent[];

class RetryableFailure extends Error {
  constructor(
    message: string,
    readonly status?: number
  ) {
    super(message);
  }
}

function pageUrl(base: string, view: AgentDrainView, cursor: string | undefined): string {
  const u = new URL(base, 'http://drain.invalid');
  u.searchParams.set('limit', String(DRAIN_PAGE_LIMIT));
  if (view === 'compact') u.searchParams.set('view', 'compact');
  else u.searchParams.delete('view');
  if (cursor) u.searchParams.set('cursor', cursor);
  else u.searchParams.delete('cursor');
  return `${u.pathname}${u.search}`;
}

function isAbort(err: unknown, signal?: AbortSignal): boolean {
  return (err instanceof Error && err.name === 'AbortError') || signal?.aborted === true;
}

function abortError(): Error {
  const err = new Error('The drain was aborted');
  err.name = 'AbortError';
  return err;
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) {
    return signal?.aborted ? Promise.reject(abortError()) : Promise.resolve();
  }
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = (): void => {
      clearTimeout(timer);
      reject(abortError());
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

/**
 * Whether a non-OK status is worth retrying: server errors, 408 and 429. Other 4xx answers (a bad
 * label, a forbidden project) will not change on retry.
 */
function retryableStatus(status: number): boolean {
  return status >= 500 || status === 408 || status === 429;
}

async function fetchPageOnce(
  url: string,
  fetchFn: AgentDrainFetch,
  signal: AbortSignal | undefined
): Promise<LegacyListBody> {
  let response: Response;
  try {
    response = await fetchFn(url, signal ? { signal } : {});
  } catch (err) {
    if (isAbort(err, signal)) throw err;
    throw new RetryableFailure(err instanceof Error ? err.message : 'Network error');
  }
  if (!response.ok) {
    const message = `HTTP ${response.status}${response.statusText ? `: ${response.statusText}` : ''}`;
    if (retryableStatus(response.status)) throw new RetryableFailure(message, response.status);
    const err = new Error(message) as Error & { status?: number };
    err.status = response.status;
    throw err;
  }
  try {
    return (await response.json()) as LegacyListBody;
  } catch (err) {
    if (isAbort(err, signal)) throw err;
    throw new RetryableFailure('Invalid response body');
  }
}

/**
 * Walk the legacy agent list at `url` (a list path with any filter
 * parameters, e.g. `label`; `limit`, `cursor` and `view` are set here).
 *
 * Guarantees:
 * - At most `maxRequests` successful page requests, a full carried
 *   `firstPage` included (a short one is discarded and not counted toward
 *   the cap); each page is attempted at most `retries + 1` times. Network
 *   errors, unparseable bodies, 5xx, 408 and 429 are retried; any other
 *   non-OK status ends the drain at once with `error.status` set.
 * - A page with zero items and a `nextCursor` does not end the drain (the
 *   server may filter a whole page by read access); it counts toward the
 *   cap like any other page.
 * - `complete` is true only when a page arrived with no `nextCursor`.
 * - Agents are returned in walk order, de-duplicated by ID.
 * - Never touches shared state, and never swallows an abort: an aborted
 *   signal rejects with an `AbortError`.
 */
export async function drainAgents(
  url: string,
  options: DrainAgentsOptions = {}
): Promise<AgentDrainResult> {
  const {
    signal,
    view = 'full',
    maxRequests = DRAIN_MAX_REQUESTS,
    retries = DRAIN_PAGE_RETRIES,
    retryDelayMs = 250,
    fetchFn = apiFetch,
    firstPage,
  } = options;

  const byId = new Map<string, Agent>();
  let capabilities: Capabilities | undefined;
  let cursor: string | undefined;
  // Pages whose rows are in the result; the loop stops at `maxRequests`.
  let pages = 0;
  // A discarded short first page: a request that was answered, but whose
  // rows are not used.
  let discarded = 0;

  if (firstPage && firstPage.agents.length >= DRAIN_PAGE_LIMIT) {
    for (const a of firstPage.agents) {
      if (!byId.has(a.id)) byId.set(a.id, a);
    }
    capabilities = firstPage.capabilities;
    cursor = firstPage.nextCursor;
    pages = 1;
  } else if (firstPage) {
    discarded = 1;
  }

  const result = (
    extra: Pick<AgentDrainResult, 'complete' | 'capped' | 'error'>
  ): AgentDrainResult => ({
    agents: Array.from(byId.values()),
    capabilities,
    requests: discarded + pages,
    firstPageFailed: extra.error !== null && pages === 0,
    ...extra,
  });

  while (pages < maxRequests) {
    let body: LegacyListBody | undefined;
    let lastError: AgentDrainError | null = null;
    for (let attempt = 0; attempt <= retries; attempt++) {
      if (attempt > 0) await wait(attempt * retryDelayMs, signal);
      if (signal?.aborted) throw abortError();
      try {
        body = await fetchPageOnce(pageUrl(url, view, cursor), fetchFn, signal);
        lastError = null;
        break;
      } catch (err) {
        if (isAbort(err, signal)) throw err instanceof Error ? err : abortError();
        if (err instanceof RetryableFailure) {
          lastError = { message: err.message, ...(err.status ? { status: err.status } : {}) };
          continue;
        }
        const status = (err as { status?: number }).status;
        return result({
          complete: false,
          capped: false,
          error: {
            message: err instanceof Error ? err.message : 'Failed to load agents',
            ...(status ? { status } : {}),
          },
        });
      }
    }
    if (!body) {
      return result({
        complete: false,
        capped: false,
        error: lastError ?? { message: 'Failed to load agents' },
      });
    }

    pages++;
    const agents = Array.isArray(body) ? body : (body.agents ?? []);
    for (const a of agents) {
      if (!byId.has(a.id)) byId.set(a.id, a);
    }
    if (!Array.isArray(body) && body._capabilities && !capabilities) {
      capabilities = body._capabilities;
    }
    const next = Array.isArray(body) ? undefined : body.nextCursor;
    if (!next) {
      return result({ complete: true, capped: false, error: null });
    }
    cursor = next;
  }

  return result({ complete: false, capped: true, error: null });
}

/** The parts of the state store the runner uses. `stateManager` satisfies it. */
export type AgentDrainState = Pick<
  StateManager,
  | 'scopeGeneration'
  | 'sseConnected'
  | 'beginSeedEpoch'
  | 'seedAgents'
  | 'endSeedEpoch'
  | 'getAgent'
  | 'getDeletedAgentIds'
  | 'addEventListener'
  | 'removeEventListener'
>;

/** Options for one {@link AgentDrainRunner.run}. */
export interface AgentDrainRunOptions {
  /** Legacy list path with filters, as for {@link drainAgents}. */
  url: string;
  view: AgentDrainView;
  /**
   * The page's client-side membership rule for agents created live while
   * the walk is in flight (for example "belongs to project X"). When
   * omitted, membership of a live create cannot be decided on the client:
   * the create is not added, and the result is marked `stale` instead.
   */
  isMember?: (agent: Agent) => boolean;
  /** Continue from a page the caller already fetched (see {@link DrainFirstPage}). */
  firstPage?: DrainFirstPage;
  /**
   * The seed epoch the caller opened before fetching `firstPage`, so live
   * changes since that request are kept. The run takes it over and closes
   * it on every path. Without it the run opens its own epoch.
   */
  epoch?: AgentSeedEpoch;
}

/**
 * A drain result after the seed-epoch protocol. `agents` is the membership
 * to render: the drained set plus decidable live creates, minus live
 * deletes, each as the state store's current object (so a live update that
 * arrived during the walk is reflected). `stale` means the result may miss
 * live changes: the live connection was not up within
 * {@link DRAIN_CONNECT_TIMEOUT_MS}, a live create could not be decided, or
 * the live connection resynced while the walk (or a carried first page's
 * request) was in flight.
 */
export interface SeededDrainResult extends AgentDrainResult {
  stale: boolean;
}

/** Dependencies of {@link AgentDrainRunner}; every field defaults to the production one. */
export interface AgentDrainRunnerDeps {
  state?: AgentDrainState;
  fetchFn?: AgentDrainFetch;
  connectTimeoutMs?: number;
  retryDelayMs?: number;
}

/**
 * Runs one drain at a time inside the seed-epoch protocol.
 *
 * `run()`:
 * 1. aborts any previous run (its promise resolves `null`);
 * 2. waits for the live connection of the current scope generation, or
 *    {@link DRAIN_CONNECT_TIMEOUT_MS} (a timeout or rejection marks the
 *    result stale);
 * 3. opens a seed epoch and records IDs created live from then on;
 * 4. drains ({@link drainAgents});
 * 5. seeds the drained agents with the epoch token (`partial` for the
 *    compact view, so a compact seed never strips full fields);
 * 6. builds the membership and closes the epoch.
 *
 * A run resolves `null` when it was superseded, aborted, or the state
 * store's scope changed while it was in flight; nothing is seeded then.
 * Superseding, `abort()` and a scope change all abort the in-flight page
 * fetch, so no further page is requested. The epoch is always closed, on
 * success, failure and abort. The runner
 * never changes the SSE scope, never marks the agent set complete in the
 * state store, and never issues a request after `abort()`.
 */
export class AgentDrainRunner {
  private readonly state: AgentDrainState;
  private readonly fetchFn: AgentDrainFetch;
  private readonly connectTimeoutMs: number;
  private readonly retryDelayMs: number | undefined;
  private controller: AbortController | null = null;
  private generation = 0;

  constructor(deps: AgentDrainRunnerDeps = {}) {
    this.state = deps.state ?? stateManager;
    this.fetchFn = deps.fetchFn ?? apiFetch;
    this.connectTimeoutMs = deps.connectTimeoutMs ?? DRAIN_CONNECT_TIMEOUT_MS;
    this.retryDelayMs = deps.retryDelayMs;
  }

  /** Whether a run is in flight. */
  get running(): boolean {
    return this.controller !== null;
  }

  /** Abort the in-flight run, if any. Its `run()` promise resolves `null`. */
  abort(): void {
    this.generation++;
    this.controller?.abort();
    this.controller = null;
  }

  /**
   * Start a drain, superseding any in-flight one. Resolves `null` if this
   * run was superseded or aborted before it finished; otherwise the
   * seeded result (which can carry `error`, `capped` or `stale`).
   */
  async run(options: AgentDrainRunOptions): Promise<SeededDrainResult | null> {
    this.abort();
    const gen = this.generation;
    const controller = new AbortController();
    this.controller = controller;
    const { signal } = controller;

    const scopeGen = this.state.scopeGeneration;
    // A scope change makes anything still to come belong to a scope the
    // store no longer holds: stop fetching at once.
    const onScopeChanged = (): void => {
      if (this.state.scopeGeneration !== scopeGen) controller.abort();
    };
    this.state.addEventListener('scope-changed', onScopeChanged);
    let epoch: AgentSeedEpoch | undefined = options.epoch;
    try {
      const lateConnect = await this.waitForConnection(signal);
      if (gen !== this.generation || scopeGen !== this.state.scopeGeneration) return null;

      epoch ??= new AgentSeedEpoch(this.state);
      let drained: AgentDrainResult;
      try {
        drained = await drainAgents(options.url, {
          signal,
          view: options.view,
          fetchFn: this.fetchFn,
          ...(this.retryDelayMs !== undefined ? { retryDelayMs: this.retryDelayMs } : {}),
          ...(options.firstPage ? { firstPage: options.firstPage } : {}),
        });
      } catch (err) {
        if (gen !== this.generation || isAbort(err, signal)) return null;
        throw err;
      }
      // A scope change mid-walk makes the snapshot belong to a scope the
      // store no longer holds (the store has already invalidated the
      // epoch token): discard it rather than render it.
      if (gen !== this.generation || scopeGen !== this.state.scopeGeneration) return null;

      const seeded = epoch.seed(drained.agents, {
        partial: options.view === 'compact',
        ...(options.isMember ? { isMember: options.isMember } : {}),
      });
      return {
        ...drained,
        agents: seeded.agents,
        stale: lateConnect || seeded.undecided || epoch.sawResync,
      };
    } finally {
      this.state.removeEventListener('scope-changed', onScopeChanged);
      epoch?.close();
      if (this.controller === controller) this.controller = null;
    }
  }

  /** Resolves `true` if the connection did not come up in time (or the wait was rejected). */
  private async waitForConnection(signal: AbortSignal): Promise<boolean> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<boolean>((resolve) => {
      timer = setTimeout(() => resolve(true), this.connectTimeoutMs);
    });
    const onAbort = (): void => resolveAborted(true);
    let resolveAborted: (value: boolean) => void = () => {};
    const aborted = new Promise<boolean>((resolve) => {
      resolveAborted = resolve;
    });
    signal.addEventListener('abort', onAbort, { once: true });
    const connected = this.state.sseConnected(this.state.scopeGeneration).then(
      () => false,
      () => true
    );
    try {
      return await Promise.race([connected, timeout, aborted]);
    } finally {
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
    }
  }
}
