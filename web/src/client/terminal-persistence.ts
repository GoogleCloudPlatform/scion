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
 * Per-user terminal workspace persistence.
 *
 * Restores the saved list of open terminal agents (and which one was
 * frontmost) from the Hub when the terminal viewer opens, and saves it back
 * on open, close, reorder and frontmost change, debounced.
 *
 * Scope: GET, the "no write before read" invariant, the snapshot, the
 * debounce, and keepalive PUTs; `urlIntent` and the URL-driven merge table
 * (an explicit URL decides what connects; the saved list only decides rail
 * membership); the first-attempt restore budget with late merge, and the
 * rate-limited background retry after a failed GET. There is no unload
 * listener: every PUT is keepalive, and a change still in the debounce
 * window at unload is an accepted loss.
 */

import { apiFetch, type ApiFetchOptions } from './api.js';
import type { TerminalCoordinator } from './terminal-coordinator.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { parseLayoutUrl } from './terminal-layout.js';

const TERMINAL_WORKSPACE_PATH = '/api/v1/users/me/terminal-workspace';
const MAX_AGENT_IDS = 32;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const TERMINAL_AGENT_PATH =
  /^\/terminals\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Pure helper: true when the URL names an agent path or parses to a layout
 * query (even one naming a preset but no slots). In either case an explicit
 * URL decides what is visible and connected, so restore() must not
 * auto-connect the saved frontmost — every restored entry stays idle, and
 * the URL-driven code that runs after restore() is what connects something.
 */
export function restoreUrlIntent(pathname: string, search: string): boolean {
  return TERMINAL_AGENT_PATH.test(pathname) || parseLayoutUrl(search) !== null;
}

/** The document shape the client sends and compares against its baseline. */
interface TerminalWorkspaceDoc {
  readonly agentIds: readonly string[];
  readonly frontmostAgentId: string | null;
}

/** The full response shape from GET/PUT. */
interface ServerTerminalWorkspace extends TerminalWorkspaceDoc {
  readonly revision: number;
  readonly updatedAt: string | null;
  readonly pruned: number;
}

type GenerationState =
  | { readonly status: 'loading' }
  | { readonly status: 'merged'; baseline: TerminalWorkspaceDoc | null }
  | { readonly status: 'failed' };

/**
 * `null` = the request itself failed (network error). `'invalid-response'` =
 * the request succeeded (HTTP 200) but the body did not pass validation, so
 * it is not known whether the write landed. A `number` is a non-200 HTTP
 * status the server returned.
 */
type PutFailureStatus = number | null | 'invalid-response';

export interface TerminalWorkspacePersistenceDeps {
  coordinator: TerminalCoordinator;
  workspace: TerminalWorkspaceRoot;
  /**
   * Called when restore selects the frontmost into an empty viewer. The
   * caller (main.ts) does the base-path-aware URL update: replaceState to
   * `<base>/terminals/<agentId>` and terminalWorkspace.setCurrentPath, only
   * while the route is still bare `/terminals`.
   */
  onRestoredSelection: (agentId: string) => void;
  /** Injectable for tests; default apiFetch (GET and PUT). */
  fetchImpl?: typeof apiFetch;
  /** Trailing debounce over snapshot changes. Default 1000ms. */
  debounceMs?: number;
  /** How long the first restore() call in a generation waits for its GET
   *  before returning regardless. Default 1500ms. */
  restoreBudgetMs?: number;
  /** Minimum gap between GET attempts after a failure. GET only; PUTs have
   *  no retry timer. Default 10000ms. */
  retryIntervalMs?: number;
}

function isValidDoc(body: unknown): body is {
  agentIds: string[];
  frontmostAgentId: string | null;
  revision: number;
  updatedAt: string | null;
  pruned: number;
} {
  if (!body || typeof body !== 'object') return false;
  const b = body as Record<string, unknown>;
  if (
    !Array.isArray(b.agentIds) ||
    !b.agentIds.every((id) => typeof id === 'string' && uuid.test(id))
  )
    return false;
  if (b.frontmostAgentId !== null) {
    // The hub validates this on write, but the response is otherwise
    // untrusted: a frontmost that is not a member of agentIds must not be
    // treated as valid, or merge() would pick a
    // connectId that restoreEntries never creates (every restored entry
    // would be deferConnect, auto-select suspended, and nothing selected).
    if (typeof b.frontmostAgentId !== 'string' || !b.agentIds.includes(b.frontmostAgentId))
      return false;
  }
  if (typeof b.revision !== 'number') return false;
  if (typeof b.pruned !== 'number') return false;
  if (b.updatedAt !== null && typeof b.updatedAt !== 'string') return false;
  return true;
}

function sameDoc(a: TerminalWorkspaceDoc, b: TerminalWorkspaceDoc): boolean {
  return (
    a.frontmostAgentId === b.frontmostAgentId &&
    a.agentIds.length === b.agentIds.length &&
    a.agentIds.every((id, i) => id === b.agentIds[i])
  );
}

/**
 * Resolves after `p` settles or after `ms`, whichever comes first — never
 * rejects. This backs the restore budget: the first restore() call in a
 * generation returns whether or not the GET has finished. Clears the
 * timeout once `p` settles first, so a fast GET does not leave a dangling
 * timer running under fake timers in tests.
 */
function raceWithTimeout(p: Promise<void>, ms: number): Promise<void> {
  return new Promise((resolve) => {
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        resolve();
      }
    }, ms);
    void p.finally(() => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve();
      }
    });
  });
}

/** TerminalWorkspacePersistence — see the file doc comment. */
export class TerminalWorkspacePersistence {
  private readonly coordinator: TerminalCoordinator;
  private readonly workspace: TerminalWorkspaceRoot;
  private readonly onRestoredSelection: (agentId: string) => void;
  private readonly fetchImpl: typeof apiFetch;
  private readonly debounceMs: number;
  private readonly restoreBudgetMs: number;
  private readonly retryIntervalMs: number;

  /** The owner generation this instance's state applies to. */
  private generation: string | null = null;
  private state: GenerationState | null = null;
  private inflightGet: Promise<void> | null = null;
  /** When the current or most recent GET attempt in this generation
   *  started, for the retryIntervalMs gate. Reset on a new generation. */
  private lastAttemptAt: number | null = null;
  /** Set once the very first restore() call in this generation has started
   *  its attempt: a race between the GET and the restore budget, shared by
   *  every restore() call in the generation so none of them, including the
   *  first, waits on the network more than once — every later call returns
   *  at once. Awaiting it after it has already settled resolves in a
   *  microtask, not a network round trip. */
  private firstAttemptGate: Promise<void> | null = null;

  private unsubscribeSessions: (() => void) | null = null;
  private unsubscribeLayout: (() => void) | null = null;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private putInFlight = false;
  private dirtyDuringPut = false;
  private disposed = false;
  /** Set once a PUT failure has been logged for the current generation, so a
   *  repeated failure (the same body re-sent on every debounce fire) does not
   *  spam the console. Reset on a new generation and on the next success. */
  private putFailureLogged = false;
  /** Set once a restore failure (a failed GET, or a throw while applying a
   *  successful one — coordinator.restoreEntries/workspace.select can throw,
   *  for example on a disposed registry) has been logged for the current
   *  generation, so a repeated failure on the rate-limited background retry
   *  (up to once per retryIntervalMs — both failure modes leave status
   *  'failed', so both are retried the same way, see performRestore's catch
   *  block) does not spam the console. Reset on a new generation only:
   *  unlike putFailureLogged, a failed restore never "succeeds and then
   *  fails again" within one generation to reset it early — once this
   *  generation's restore succeeds it is 'merged' for good. */
  private restoreFailureLogged = false;

  constructor(deps: TerminalWorkspacePersistenceDeps) {
    this.coordinator = deps.coordinator;
    this.workspace = deps.workspace;
    this.onRestoredSelection = deps.onRestoredSelection;
    this.fetchImpl = deps.fetchImpl ?? apiFetch;
    this.debounceMs = deps.debounceMs ?? 1000;
    this.restoreBudgetMs = deps.restoreBudgetMs ?? 1500;
    this.retryIntervalMs = deps.retryIntervalMs ?? 10000;
  }

  /**
   * Restores the saved list for the current owner generation. The first
   * call in a generation starts the GET and waits
   * for it for at most restoreBudgetMs; it returns whether or not the GET
   * has finished by then; a GET that finishes later still merges on
   * arrival. Every later call in the same generation returns at once and
   * never waits on the network: if the previous attempt failed, is not in
   * flight, and at least retryIntervalMs has passed since it started, this
   * call starts one new GET in the background (not awaited) and returns;
   * that GET merges and enables saving if it succeeds. Never rejects.
   */
  async restore(urlIntent: boolean): Promise<void> {
    if (this.disposed) return;
    let claimed: boolean;
    try {
      claimed = await this.coordinator.claimOwnership();
    } catch {
      claimed = false;
    }
    if (!claimed || this.disposed) return;
    const generation = this.coordinator.generation;
    if (generation === null) return;

    if (this.generation !== generation) {
      this.generation = generation;
      this.state = { status: 'loading' };
      this.inflightGet = null;
      this.lastAttemptAt = null;
      this.firstAttemptGate = null;
      this.putFailureLogged = false;
      this.restoreFailureLogged = false;
      this.installNotificationListeners(generation);
    }

    if (this.state?.status === 'merged') return;

    if (this.state?.status === 'failed') {
      // Status 'failed' means an earlier call in this generation already
      // ran the first attempt (and set firstAttemptGate): later calls only
      // start a rate-limited background retry here, and never await it.
      const now = Date.now();
      if (
        !this.inflightGet &&
        (this.lastAttemptAt === null || now - this.lastAttemptAt >= this.retryIntervalMs)
      ) {
        this.lastAttemptAt = now;
        this.state = { status: 'loading' };
        this.inflightGet = this.performRestore(generation, urlIntent);
      }
      return;
    }

    // status === 'loading'
    if (!this.inflightGet) {
      // The very first attempt in this generation.
      this.lastAttemptAt = Date.now();
      this.inflightGet = this.performRestore(generation, urlIntent);
    }
    if (!this.firstAttemptGate) {
      this.firstAttemptGate = raceWithTimeout(this.inflightGet, this.restoreBudgetMs);
    }
    // Shared by every call in the generation: once the first attempt's
    // budget has elapsed (or the GET has completed), this is already
    // settled, so a later call resolves in a microtask, not a network wait.
    await this.firstAttemptGate;
  }

  /** Tears down listeners and timers. Does not touch the saved server state. */
  dispose(): void {
    this.disposed = true;
    this.unsubscribeSessions?.();
    this.unsubscribeSessions = null;
    this.unsubscribeLayout?.();
    this.unsubscribeLayout = null;
    if (this.debounceTimer) clearTimeout(this.debounceTimer);
    this.debounceTimer = null;
  }

  private installNotificationListeners(generation: string): void {
    this.unsubscribeSessions?.();
    this.unsubscribeLayout?.();
    this.unsubscribeSessions = this.coordinator.subscribeSessions(() => this.onChange(generation));
    this.unsubscribeLayout = this.workspace.layoutManager.subscribe(() =>
      this.onChange(generation)
    );
  }

  /**
   * Before the merge, notifications are ignored and never arm the debounce:
   * the merge reads the live snapshot anyway. Also requires this.generation
   * to still be the coordinator's current generation: a stale generation's
   * 'merged' status must not authorize a write in a generation this
   * instance has not restored in. Today the coordinator cannot regain
   * ownership after losing it (only stop() releases, and it disables
   * re-claiming); this guard keeps that invariant if that ever changes.
   */
  private onChange(generation: string): void {
    if (this.disposed || generation !== this.generation) return;
    if (generation !== this.coordinator.generation) return;
    if (this.state?.status !== 'merged') return;
    this.armDebounce();
  }

  private async performRestore(generation: string, urlIntent: boolean): Promise<void> {
    let doc: ServerTerminalWorkspace | null;
    try {
      doc = await this.fetchWorkspace();
    } catch {
      doc = null;
    }
    this.inflightGet = null;
    if (this.disposed || generation !== this.generation || this.coordinator.tornDown) return;
    if (!doc) {
      this.state = { status: 'failed' };
      // Logged once per generation, not on every failed background retry
      // (up to once per retryIntervalMs) — a
      // long-lived generation against a hub that keeps 404ing during a
      // rolling deploy would otherwise warn every 10s while the user
      // navigates the viewer.
      this.logRestoreFailure(
        '[Terminal] restoring the saved terminal list failed; saving stays disabled until a later attempt succeeds.'
      );
      return;
    }
    try {
      this.merge(generation, doc, urlIntent);
    } catch (err) {
      // coordinator.restoreEntries/workspace.select can throw (a disposed
      // pane or registry, for example). A throw here must not reject
      // restore()'s "never rejects" contract, and status 'failed' is exactly
      // what enables the same rate-limited background retry as a failed GET
      // (the 'failed' branch above, in restore()): a later call, once
      // retryIntervalMs has passed, sends a fresh GET and merges again. That
      // retry typically succeeds even though the first attempt threw,
      // because the entries this merge already created via restoreEntries
      // are still in the registry, so the retried merge finds them in
      // alreadyOpen and does not call select() again. Logged once per
      // generation, the same as the GET-failure path, not on every retry.
      this.state = { status: 'failed' };
      this.logRestoreFailure(
        '[Terminal] failed to apply the restored terminal list; saving stays disabled until a later attempt succeeds.',
        err
      );
    }
  }

  private logRestoreFailure(message: string, err?: unknown): void {
    if (this.restoreFailureLogged) return;
    this.restoreFailureLogged = true;
    if (err !== undefined) console.warn(message, err);
    else console.warn(message);
  }

  /**
   * Merge algorithm. With urlIntent false and nothing already open (the
   * bare `/terminals` case), the saved frontmost (or the last saved entry)
   * connects. With urlIntent true, or when the caller (main.ts's URL/path-
   * open code, running either before a late merge or after this one)
   * already has entries open, nothing here connects: every restored entry
   * stays idle, and the URL-driven code is what selects and connects
   * something.
   */
  private merge(generation: string, doc: ServerTerminalWorkspace, urlIntent: boolean): void {
    const alreadyOpen = this.coordinator.sessions.map((session) => session.state.agentId);
    const toAdd = doc.agentIds.filter((id) => !alreadyOpen.includes(id));
    const connectId =
      urlIntent || alreadyOpen.length > 0
        ? null
        : (doc.frontmostAgentId ?? doc.agentIds[doc.agentIds.length - 1] ?? null);

    this.workspace.withAutoSelectSuspended(() => {
      this.coordinator.restoreEntries(toAdd, { connectAgentId: connectId });
    });

    if (connectId) {
      const session = this.coordinator.sessions.find((s) => s.state.agentId === connectId);
      if (session) {
        this.workspace.select(session);
        this.onRestoredSelection(connectId);
      }
    }

    // The hub row still holds the unpruned list when pruned > 0, so there is
    // no baseline to compare against: the write-back always goes out.
    this.state = {
      status: 'merged',
      baseline:
        doc.pruned > 0 ? null : { agentIds: doc.agentIds, frontmostAgentId: doc.frontmostAgentId },
    };
    if (generation === this.coordinator.generation) this.armDebounce();
  }

  private armDebounce(): void {
    if (this.debounceTimer) return; // already armed; further changes coalesce into it
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      void this.fire();
    }, this.debounceMs);
  }

  private async fire(): Promise<void> {
    if (this.disposed || !this.coordinator.isOwner || this.coordinator.tornDown) return;
    if (this.state?.status !== 'merged') return;
    if (this.generation !== this.coordinator.generation) return; // see onChange's doc comment

    // Check putInFlight before comparing against the baseline: while a PUT
    // is in flight, this.state.baseline is still the PREVIOUS saved doc, not
    // the one the in-flight PUT is about to establish. Comparing against it
    // here would let a change that returns to that previous doc (a revert)
    // hit the sameDoc early return and never mark dirty — so once the
    // in-flight PUT lands and advances the baseline to what it sent, the
    // user's revert is never saved, silently leaving the hub out of sync
    // with what the viewer shows. Marking dirty unconditionally here is
    // safe even when the change is ultimately a no-op against
    // whatever baseline the in-flight PUT settles on: the re-armed fire()
    // re-evaluates snapshot() against the (by then current) baseline itself.
    if (this.putInFlight) {
      this.dirtyDuringPut = true;
      return;
    }

    const generation = this.generation;
    const snap = this.snapshot();
    const baseline = this.state.baseline;
    if (baseline !== null && sameDoc(baseline, snap)) return; // no-op change (or the usual reload)

    this.putInFlight = true;
    try {
      const result = await this.putWorkspace(snap);
      if (this.disposed || generation !== this.generation || this.coordinator.tornDown) return;
      if (result.ok) {
        this.state = {
          status: 'merged',
          baseline: {
            agentIds: result.doc.agentIds,
            frontmostAgentId: result.doc.frontmostAgentId,
          },
        };
        this.putFailureLogged = false;
      } else {
        // A rejected/failed PUT does not advance the baseline: the unsaved
        // snapshot goes out with the next debounced send, which the next
        // change triggers. There is no PUT retry timer. Logged once per
        // generation so a persistent failure does not spam the console on
        // every send.
        this.logPutFailure(result.status);
      }
    } catch (err) {
      // putWorkspace does not throw in normal operation (it catches its own
      // fetch and treats a validation failure as a failed result), but this
      // guards fire() itself against an unexpected throw so it cannot become
      // an unhandled rejection.
      this.logPutFailure(null);
      console.warn('[Terminal] unexpected error while saving the terminal list.', err);
    } finally {
      this.putInFlight = false;
      if (this.dirtyDuringPut) {
        this.dirtyDuringPut = false;
        this.armDebounce();
      }
    }
  }

  private logPutFailure(status: PutFailureStatus): void {
    if (this.putFailureLogged) return;
    this.putFailureLogged = true;
    if (status === null) {
      console.warn(
        '[Terminal] saving the terminal list failed (network error); it will be retried on the next change.'
      );
    } else if (status === 'invalid-response') {
      console.warn(
        '[Terminal] saving the terminal list got an unreadable response (HTTP 200 with an invalid body); it will be retried on the next change.'
      );
    } else if (status >= 500) {
      console.warn(
        `[Terminal] saving the terminal list failed (HTTP ${status}); it will be retried on the next change.`
      );
    } else {
      console.warn(
        `[Terminal] saving the terminal list was rejected (HTTP ${status}); it will be retried on the next change.`
      );
    }
  }

  /**
   * Snapshot = { agentIds, frontmostAgentId }. agentIds is the coordinator's
   * session list (insertion/"added" order), excluding any entry whose
   * metadata availability is 'deleted'. Truncated to the frontmost plus the
   * 31 most recently added, in original insertion order, when over
   * MAX_AGENT_IDS.
   */
  private snapshot(): TerminalWorkspaceDoc {
    const sessions = this.coordinator.sessions.filter(
      (session) => this.coordinator.metadataFor(session.state.agentId)?.availability !== 'deleted'
    );
    let agentIds = sessions.map((session) => session.state.agentId);

    const frontmostKey = this.workspace.layoutManager.getState().single[0];
    const frontmostSession = frontmostKey
      ? sessions.find((session) => session.state.key === frontmostKey)
      : undefined;
    const frontmostAgentId =
      frontmostSession && agentIds.includes(frontmostSession.state.agentId)
        ? frontmostSession.state.agentId
        : null;

    if (agentIds.length > MAX_AGENT_IDS) {
      const keep = new Set<string>();
      if (frontmostAgentId) keep.add(frontmostAgentId);
      for (let i = agentIds.length - 1; i >= 0 && keep.size < MAX_AGENT_IDS; i--)
        keep.add(agentIds[i]);
      agentIds = agentIds.filter((id) => keep.has(id));
    }

    return { agentIds, frontmostAgentId };
  }

  private async fetchWorkspace(): Promise<ServerTerminalWorkspace | null> {
    const response = await this.fetchImpl(TERMINAL_WORKSPACE_PATH, { method: 'GET' });
    if (response.status !== 200) return null;
    return this.parseResponse(response);
  }

  private async putWorkspace(
    doc: TerminalWorkspaceDoc
  ): Promise<{ ok: true; doc: ServerTerminalWorkspace } | { ok: false; status: PutFailureStatus }> {
    const options: ApiFetchOptions = {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agentIds: doc.agentIds, frontmostAgentId: doc.frontmostAgentId }),
      keepalive: true,
    };
    let response: Response;
    try {
      response = await this.fetchImpl(TERMINAL_WORKSPACE_PATH, options);
    } catch {
      return { ok: false, status: null };
    }
    if (response.status !== 200) return { ok: false, status: response.status };
    const parsed = await this.parseResponse(response);
    // A 200 whose body fails validation is not the server rejecting the
    // write (it never got far enough to have an opinion); it is an
    // unexpected/malformed response (e.g. a proxy or dev-server fallback).
    // Keep it out of the "rejected (HTTP <status>)" case below so the log
    // doesn't claim the write was refused when it may well have landed.
    if (!parsed) return { ok: false, status: 'invalid-response' };
    return { ok: true, doc: parsed };
  }

  /**
   * Validated before use: status 200 (checked by the caller), JSON,
   * agentIds an array of UUID strings, frontmostAgentId a string or null.
   * Anything else counts as a failed request — this also covers a dev
   * server or proxy answering with an HTML fallback, or
   * the Vite mock's `200 []`.
   */
  private async parseResponse(response: Response): Promise<ServerTerminalWorkspace | null> {
    let body: unknown;
    try {
      body = await response.json();
    } catch {
      return null;
    }
    if (!isValidDoc(body)) return null;
    return {
      agentIds: body.agentIds,
      frontmostAgentId: body.frontmostAgentId,
      revision: body.revision,
      updatedAt: body.updatedAt,
      pruned: body.pruned,
    };
  }
}
