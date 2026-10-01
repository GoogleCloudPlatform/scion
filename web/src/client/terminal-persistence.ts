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
 * Per-user terminal workspace persistence (design ptone/scion#2278).
 *
 * Restores the saved list of open terminal agents (and which one was
 * frontmost) from the Hub when the terminal viewer opens, and saves it back
 * on open, close, reorder and frontmost change, debounced.
 *
 * Phase 1 scope (this file, as landed): the bare `/terminals` (no query)
 * restore case only — GET, the "no write before read" invariant, the
 * snapshot, the debounce, and keepalive PUTs. There is no unload listener
 * (design section 3.4: every PUT is keepalive, and a change still in the
 * debounce window at unload is an accepted loss). Phase 2 adds urlIntent
 * (the URL-driven table in design section 3.5.3), the first-attempt restore
 * budget with late merge, and the rate-limited background retry after a
 * failed GET (section 3.5.1).
 */

import { apiFetch, type ApiFetchOptions } from './api.js';
import type { TerminalCoordinator } from './terminal-coordinator.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';

const TERMINAL_WORKSPACE_PATH = '/api/v1/users/me/terminal-workspace';
const MAX_AGENT_IDS = 32;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The document shape the client sends and compares against its baseline. */
interface TerminalWorkspaceDoc {
  readonly agentIds: readonly string[];
  readonly frontmostAgentId: string | null;
}

/** The full response shape from GET/PUT (design section 3.2). */
interface ServerTerminalWorkspace extends TerminalWorkspaceDoc {
  readonly revision: number;
  readonly updatedAt: string | null;
  readonly pruned: number;
}

type GenerationState =
  | { readonly status: 'loading' }
  | { readonly status: 'merged'; baseline: TerminalWorkspaceDoc | null }
  | { readonly status: 'failed' };

export interface TerminalWorkspacePersistenceDeps {
  coordinator: TerminalCoordinator;
  workspace: TerminalWorkspaceRoot;
  /**
   * Called when restore selects the frontmost into an empty viewer. The
   * caller (main.ts) does the base-path-aware URL update (design section
   * 3.5.2): replaceState to `<base>/terminals/<agentId>` and
   * terminalWorkspace.setCurrentPath, only while the route is still bare
   * `/terminals`.
   */
  onRestoredSelection: (agentId: string) => void;
  /** Injectable for tests; default apiFetch (GET and PUT). */
  fetchImpl?: typeof apiFetch;
  /** Trailing debounce over snapshot changes. Default 1000ms. */
  debounceMs?: number;
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
  if (b.frontmostAgentId !== null && typeof b.frontmostAgentId !== 'string') return false;
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

/** TerminalWorkspacePersistence — see the file doc comment. */
export class TerminalWorkspacePersistence {
  private readonly coordinator: TerminalCoordinator;
  private readonly workspace: TerminalWorkspaceRoot;
  private readonly onRestoredSelection: (agentId: string) => void;
  private readonly fetchImpl: typeof apiFetch;
  private readonly debounceMs: number;

  /** The owner generation this instance's state applies to. */
  private generation: string | null = null;
  private state: GenerationState | null = null;
  private inflightGet: Promise<void> | null = null;

  private unsubscribeSessions: (() => void) | null = null;
  private unsubscribeLayout: (() => void) | null = null;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private putInFlight = false;
  private dirtyDuringPut = false;
  private disposed = false;

  constructor(deps: TerminalWorkspacePersistenceDeps) {
    this.coordinator = deps.coordinator;
    this.workspace = deps.workspace;
    this.onRestoredSelection = deps.onRestoredSelection;
    this.fetchImpl = deps.fetchImpl ?? apiFetch;
    this.debounceMs = deps.debounceMs ?? 1000;
  }

  /**
   * Restores the saved list for the current owner generation. urlIntent is
   * accepted now (matching the eventual interface) but Phase 1's only caller
   * passes false for the bare `/terminals` route; Phase 2 wires it from the
   * URL. Never rejects.
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
      this.installNotificationListeners(generation);
    }
    if (this.state?.status !== 'loading') return; // already merged or failed this generation
    if (!this.inflightGet) this.inflightGet = this.performRestore(generation, urlIntent);
    await this.inflightGet;
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
   * Before the merge, notifications are ignored and never arm the debounce
   * (design section 3.5.5): the merge reads the live snapshot anyway.
   */
  private onChange(generation: string): void {
    if (this.disposed || generation !== this.generation) return;
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
      console.warn(
        '[Terminal] restoring the saved terminal list failed; saving stays disabled until a later attempt succeeds.'
      );
      return;
    }
    this.merge(generation, doc, urlIntent);
  }

  /**
   * Merge algorithm (design section 3.5.2), restricted to the bare-path
   * case this phase implements: with urlIntent false and nothing already
   * open, the saved frontmost (or the last saved entry) connects; every
   * other restored entry stays idle.
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
    const generation = this.generation;
    const snap = this.snapshot();
    const baseline = this.state.baseline;
    if (baseline !== null && sameDoc(baseline, snap)) return; // no-op change (or the usual reload)

    if (this.putInFlight) {
      this.dirtyDuringPut = true;
      return;
    }
    this.putInFlight = true;
    try {
      const stored = await this.putWorkspace(snap);
      if (this.disposed || generation !== this.generation || this.coordinator.tornDown) return;
      if (stored) {
        this.state = {
          status: 'merged',
          baseline: { agentIds: stored.agentIds, frontmostAgentId: stored.frontmostAgentId },
        };
      }
      // A rejected/failed PUT does not advance the baseline (design section
      // 3.5.4): the unsaved snapshot goes out with the next debounced send,
      // which the next change triggers. There is no PUT retry timer.
    } catch {
      // network error: same as above, baseline not advanced.
    } finally {
      this.putInFlight = false;
      if (this.dirtyDuringPut) {
        this.dirtyDuringPut = false;
        this.armDebounce();
      }
    }
  }

  /**
   * Snapshot = { agentIds, frontmostAgentId }. agentIds is the coordinator's
   * session list (insertion/"added" order), excluding any entry whose
   * metadata availability is 'deleted' (design section 3.3 (3)). Truncated
   * to the frontmost plus the 31 most recently added, in original insertion
   * order, when over MAX_AGENT_IDS (section 3.5.5).
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

  private async putWorkspace(doc: TerminalWorkspaceDoc): Promise<ServerTerminalWorkspace | null> {
    const options: ApiFetchOptions = {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agentIds: doc.agentIds, frontmostAgentId: doc.frontmostAgentId }),
      keepalive: true,
    };
    const response = await this.fetchImpl(TERMINAL_WORKSPACE_PATH, options);
    if (response.status !== 200) return null;
    return this.parseResponse(response);
  }

  /**
   * Validated before use: status 200 (checked by the caller), JSON,
   * agentIds an array of UUID strings, frontmostAgentId a string or null.
   * Anything else counts as a failed request (design section 3.5.0) — this
   * also covers a dev server or proxy answering with an HTML fallback, or
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
