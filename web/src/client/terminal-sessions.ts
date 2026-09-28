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

import type { Agent } from '../shared/types.js';
import { isTerminalAvailable } from '../shared/types.js';
import { extractApiError } from './api.js';
import { TerminalMetadata } from './terminal-metadata.js';
import { classifyPtyClose, PTY_CLOSE } from './terminal-close-codes.js';

/** Supplied by authenticated bootstrap, never by a terminal route or peer message. */
export interface TerminalScope {
  readonly hubUrl: string; // HTTP(S) origin plus deployment base path
  readonly accountId: string;
}

export type TerminalConnectionState =
  | 'loading'
  | 'connecting'
  | 'connected'
  | 'disconnected'
  | 'unavailable'
  | 'closed';

/**
 * Classifies the cause of a disconnection or unavailability so the UI
 * can present appropriate feedback and enable/disable the reconnect action.
 */
export type TerminalDisconnectReason =
  | 'network' // WebSocket closed with a retriable code
  | 'detached' // Close code 1000 — a tmux client detached; the session still exists
  | 'session-ended' // Close code 4410 — the tmux session no longer exists
  | 'auth-401' // 401 Unauthorized from preflight, agent fetch, or close code 4401
  | 'auth-403' // 403 Forbidden from preflight or close code 4403
  | 'not-found' // 404 — agent may be deleted, or close code 4404
  | 'agent-offline' // Agent activity is offline
  | 'agent-phase' // Agent phase prevents terminal (not running/stopping)
  | 'agent-stopped' // SSE reported agent stopped (phase change)
  | 'agent-deleted' // SSE reported agent deleted
  | 'server-error' // 5xx or unclassified HTTP error
  | 'connect-error' // WebSocket onerror before open
  | null; // No disconnect (connected, loading, or clean close)

/**
 * Disconnect reasons that mean "unavailable because of the agent's own
 * state", any of which re-arm auto-reconnect once the agent is confirmed
 * running again. The WebSocket close usually reaches the client before SSE
 * reports the phase change, so the session's own attempt often already
 * classified itself as 'agent-phase'/'agent-offline' by the time SSE's
 * stopped event arrives — not just the 'agent-stopped' that
 * markUnavailable() sets.
 */
export const AGENT_UNAVAILABLE_REASONS: ReadonlySet<TerminalDisconnectReason> = new Set([
  'agent-stopped',
  'agent-phase',
  'agent-offline',
]);

export interface TerminalSize {
  readonly cols: number;
  readonly rows: number;
}

/** Serializable view state; transport and renderer handles stay private. */
export interface TerminalSessionState {
  readonly key: string;
  readonly agentId: string;
  readonly generation: number;
  readonly connection: TerminalConnectionState;
  readonly agent: Agent | null;
  readonly error: string | null;
  readonly disconnectReason: TerminalDisconnectReason;
  readonly lastSize: TerminalSize | null;
  /**
   * True once a reconnect attempt (automatic or manual) has failed after this
   * session was previously connected. While true, no further automatic
   * attempt runs until the pane spends BACKGROUND_RESET_MS in the
   * background, or a manual Reconnect succeeds. Cleared optimistically
   * whenever a new attempt starts.
   */
  readonly reconnectFailed: boolean;
  /**
   * True when the failed attempt above was manually triggered (the user
   * clicked Reconnect), as opposed to an automatic foreground attempt. Only
   * meaningful while reconnectFailed is true.
   */
  readonly reconnectFailedManual: boolean;
}

/** One renderer per session. It continues parsing bytes even when hidden. */
export interface TerminalResources {
  write(bytes: Uint8Array): void;
  size(): TerminalSize;
  /** Clear the prior screen only after a reconnect socket opens successfully. */
  reset(): void;
  dispose(): void;
}

/**
 * Check signal after every await before mutating UI. If resources were allocated,
 * either clean them up on abort or return their adapter for session disposal.
 * Successful initialization is retained until close, including across reconnects.
 */
export type TerminalResourceInitializer = (
  agent: Agent,
  signal: AbortSignal
) => Promise<TerminalResources>;

export interface TerminalSession {
  readonly state: TerminalSessionState;
  /** True when a reconnect attempt is in progress; UI can use this to disable repeated clicks. */
  readonly reconnecting: boolean;
  /** Immediately reports current state; returns an unsubscribe function. */
  subscribe(listener: (state: TerminalSessionState) => void): () => void;
  /** Explicit attach/reconnect. Shared promise resolves after socket setup, not handshake. */
  connect(): Promise<void>;
  /**
   * Mark this session as unavailable due to an external signal (SSE agent-stopped/deleted).
   * Sets the connection state and disconnect reason without disrupting transport.
   */
  markUnavailable(reason: 'agent-stopped' | 'agent-deleted', message: string): void;
  /**
   * Foreground-only auto-reconnect. Called by the renderer whenever this
   * pane's effective visibility changes: pane setVisible(true/false), the
   * workspace shown/hidden, or the document visibilitychange event.
   * "Frontmost" is the AND of all three. A false -> true transition attempts
   * a reconnect if this session is armed (disconnected with a retriable
   * cause, or the agent restarted after being stopped) and not already
   * blocked by a prior failed attempt.
   */
  setFrontmost(frontmost: boolean): void;
  /**
   * SSE reported the agent's phase returned to running while this session
   * was unavailable due to the agent's own state: agent-stopped,
   * agent-phase, or agent-offline (the WebSocket drop usually reaches the
   * client before SSE's stopped event, so the session's own attempt often
   * already classified itself as agent-phase/agent-offline). Re-arms
   * auto-reconnect: it fires immediately if the pane is already frontmost,
   * otherwise on the next foregrounding.
   */
  noteAgentAvailable(): void;
  /** Immediate transport write, including xterm protocol responses. Never queues input. */
  sendData(data: string): boolean;
  resize(cols: number, rows: number): void;
  /** navigation is ONLY for the disposable legacy page, not retained workspace navigation. */
  close(reason?: 'explicit' | 'navigation'): void;
}

export type TerminalRegistryListener = (sessions: readonly TerminalSession[]) => void;

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Foreground-only reconnect tuning. No backoff, no attempt cap: exactly one
 * attempt per foregrounding.
 */
const HEARTBEAT_INTERVAL_MS = 20_000;
const HEARTBEAT_TIMEOUT_MS = 10_000;
const BACKGROUND_RESET_MS = 2 * 60_000;
/** Browsers always fire `close` after `error`; this is a defensive fallback only. */
const CONNECT_ERROR_FALLBACK_MS = 1_000;
/**
 * The Hub upgrades the socket before it knows whether the broker stream
 * opened, so a socket can go straight from onopen to a close code without
 * ever proving the stream is live. Bounds how long a *reconnect* attempt
 * waits for the first data frame before it counts as failed. Not applied to
 * the initial connect: see the onopen handler in attach().
 */
const FIRST_FRAME_TIMEOUT_MS = 10_000;

/** Document-local registry. Browser ownership and UI selection belong to the coordinator. */
export class TerminalSessionRegistry {
  private readonly sessions = new Map<string, Session>();
  private readonly listeners = new Set<TerminalRegistryListener>();
  private readonly hubUrl: string;
  private readonly accountId: string;
  readonly metadata: TerminalMetadata;
  private disposed = false;

  constructor(scope: TerminalScope) {
    const url = new URL(scope.hubUrl);
    if (
      !['http:', 'https:'].includes(url.protocol) ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    ) {
      throw new Error('Terminal scope requires an HTTP(S) hub origin and base path.');
    }
    if (!scope.accountId.trim()) throw new Error('Authenticated terminal account required.');
    this.hubUrl = url.href.replace(/\/+$/, '') + '/';
    this.accountId = scope.accountId;
    this.metadata = new TerminalMetadata(this.hubUrl);
  }

  /** Registers synchronously before any asynchronous work. Existing entries never rebind/reconnect. */
  open(agentId: string, initialize: TerminalResourceInitializer): TerminalSession {
    if (this.disposed) throw new Error('Terminal registry is disposed.');
    if (!uuid.test(agentId)) throw new Error('Terminal requires an agent UUID.');
    const id = agentId.toLowerCase();
    const existing = this.sessions.get(id);
    if (existing) return existing;
    const key = JSON.stringify([this.hubUrl, this.accountId, id]);
    const session = new Session(
      key,
      id,
      this.hubUrl,
      initialize,
      () => {
        if (this.sessions.get(id) === session) {
          this.sessions.delete(id);
          this.metadata.release(id);
          this.notify();
        }
      },
      (agent) => this.metadata.seed(id, agent)
    );
    this.sessions.set(id, session);
    this.metadata.retain(id);
    this.notify();
    void session.connect();
    return session;
  }

  /** Final account/document teardown; never used for mode or route changes. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    const errors: unknown[] = [];
    for (const session of this.sessions.values()) {
      try {
        session.close();
      } catch (error) {
        errors.push(error);
      }
    }
    this.metadata.dispose();
    if (errors.length) throw new AggregateError(errors, 'Terminal disposal failed.');
  }

  list(): readonly TerminalSession[] {
    return [...this.sessions.values()];
  }

  /** Collection changes only; individual connection state remains session-owned. */
  subscribe(listener: TerminalRegistryListener): () => void {
    this.listeners.add(listener);
    listener(this.list());
    return () => {
      this.listeners.delete(listener);
    };
  }

  private notify(): void {
    const sessions = this.list();
    for (const listener of this.listeners) listener(sessions);
  }
}

class Session implements TerminalSession {
  private snapshot: TerminalSessionState;
  private readonly listeners = new Set<(state: TerminalSessionState) => void>();
  private controller: AbortController | null = null;
  private socket: WebSocket | null = null;
  private resources: TerminalResources | null = null;
  private pending: Promise<void> | null = null;

  /** True once this session has reached 'connected' at least once. */
  private everConnected = false;
  /** Last frontmost signal pushed by the renderer. */
  private frontmost = false;
  /**
   * True once this attach() attempt's socket has proven the stream is live
   * (its first data frame arrived), not merely reached onopen — the Hub
   * upgrades the socket before it knows whether the broker stream opened.
   * Reset per attempt.
   */
  private attemptReachedOpen = false;
  /** Bounds how long an opened socket may go without a first data frame. */
  private firstFrameTimer: ReturnType<typeof setTimeout> | null = null;
  /**
   * This session owes exactly one automatic reconnect attempt on the next
   * foregrounding. A session property rather than a function of the last
   * disconnectReason/connection combination, so it survives whatever
   * specific cause disarmed it (a retriable WS close, a dead heartbeat, an
   * HTTP 5xx during an attempt, or the agent restarting after being
   * stopped). Cleared on success, on any terminal outcome, and in
   * markUnavailable; a fresh markUnavailable('agent-stopped') requires a
   * new noteAgentAvailable() call to re-arm it.
   */
  private autoArmed = false;
  /** Which trigger started the in-flight/most recent attempt. */
  private currentAttemptManual = true;
  /** The only background timer this session runs: FAILED -> DISCONNECTED after 2 min. */
  private backgroundResetTimer: ReturnType<typeof setTimeout> | null = null;

  /** Application heartbeat (liveness only; never drives retries directly). */
  private heartbeatInterval: ReturnType<typeof setInterval> | null = null;
  private heartbeatCheckTimer: ReturnType<typeof setTimeout> | null = null;
  private heartbeatCapable = false;
  private lastInboundAt = 0;

  constructor(
    key: string,
    agentId: string,
    private readonly hubUrl: string,
    private readonly initialize: TerminalResourceInitializer,
    private readonly remove: () => void,
    private readonly seedMetadata: (agent: Agent) => void
  ) {
    this.snapshot = {
      key,
      agentId,
      generation: 0,
      connection: 'loading',
      agent: null,
      error: null,
      disconnectReason: null,
      lastSize: null,
      reconnectFailed: false,
      reconnectFailedManual: false,
    };
  }

  get state(): TerminalSessionState {
    return this.snapshot;
  }

  get reconnecting(): boolean {
    return this.pending !== null;
  }

  subscribe(listener: (state: TerminalSessionState) => void): () => void {
    this.listeners.add(listener);
    listener(this.snapshot);
    return () => {
      this.listeners.delete(listener);
    };
  }

  private update(patch: Partial<TerminalSessionState>): void {
    this.snapshot = { ...this.snapshot, ...patch };
    for (const listener of this.listeners) listener(this.snapshot);
  }

  connect(): Promise<void> {
    return this.startAttempt(true);
  }

  /**
   * @param manual true for a user-initiated Reconnect click (or any other
   * external caller); false for an automatic foreground attempt
   * (`maybeAutoAttempt`). Only affects the copy shown on failure — the
   * dedup/attach logic is identical either way.
   */
  private startAttempt(manual: boolean): Promise<void> {
    if (this.pending) return this.pending;
    if (['connected', 'connecting', 'closed'].includes(this.state.connection))
      return Promise.resolve();
    this.controller?.abort();
    this.releaseSocket();
    this.stopHeartbeat();
    this.clearFirstFrameGuard();
    this.clearBackgroundResetTimer();
    this.attemptReachedOpen = false;
    this.currentAttemptManual = manual;
    const wasEverConnected = this.everConnected;
    const controller = new AbortController();
    this.controller = controller;
    const generation = this.state.generation + 1;
    // Install the promise before notifying subscribers or invoking consumer code.
    const attempt = Promise.resolve().then(() => this.attach(generation, controller.signal));
    this.pending = attempt;
    // Clear reconnectFailed optimistically: the "Reconnecting..." overlay
    // replaces the prior failure text for the duration of this attempt.
    this.update({
      generation,
      connection: 'loading',
      error: null,
      disconnectReason: null,
      reconnectFailed: false,
      reconnectFailedManual: false,
    });
    void attempt.finally(() => {
      if (this.pending === attempt) this.pending = null;
      // Attempts that settle synchronously (agent fetch/preflight failure, thrown
      // error, agent-unavailable) never reach a socket, so onclose never fires.
      // Attempts that reach a socket are judged later, by onopen/onclose.
      // Only judge an attempt that was not superseded — markUnavailable()/
      // close()/a newer connect() abort this controller or bump the
      // generation, and an aborted/stale attempt's own settlement must not
      // overwrite whatever state that supersession already set.
      // An attempt that organically discovers the agent is stopped, offline,
      // or otherwise unavailable is also not a "failed reconnect attempt" —
      // it already disarms itself (autoArmed = false) and re-arms only via
      // noteAgentAvailable(), so it must not also set reconnectFailed, which
      // would block that re-arm until a manual click or the 2-min reset.
      if (
        !controller.signal.aborted &&
        this.state.generation === generation &&
        wasEverConnected &&
        (this.state.connection === 'disconnected' ||
          (this.state.connection === 'unavailable' &&
            !AGENT_UNAVAILABLE_REASONS.has(this.state.disconnectReason)))
      ) {
        this.markReconnectFailed();
      }
    });
    return attempt;
  }

  markUnavailable(reason: 'agent-stopped' | 'agent-deleted', message: string): void {
    if (this.state.connection === 'closed') return;
    // Close existing socket if any — the agent is no longer reachable.
    this.releaseSocket();
    this.stopHeartbeat();
    this.clearFirstFrameGuard();
    this.clearBackgroundResetTimer();
    this.controller?.abort();
    // Not armed until a fresh noteAgentAvailable() call re-arms it.
    this.autoArmed = false;
    this.update({
      connection: 'unavailable',
      error: message,
      disconnectReason: reason,
      reconnectFailed: false,
      reconnectFailedManual: false,
    });
  }

  setFrontmost(frontmost: boolean): void {
    if (this.state.connection === 'closed' || this.frontmost === frontmost) return;
    this.frontmost = frontmost;
    if (frontmost) {
      this.clearBackgroundResetTimer();
      this.maybeAutoAttempt();
    } else if (this.state.reconnectFailed) {
      this.armBackgroundResetTimer();
    }
  }

  noteAgentAvailable(): void {
    if (
      this.state.connection !== 'unavailable' ||
      !AGENT_UNAVAILABLE_REASONS.has(this.state.disconnectReason)
    )
      return;
    if (!this.everConnected) return;
    this.autoArmed = true;
    this.maybeAutoAttempt();
  }

  /** Runs the single owed attempt, if this session is armed and frontmost. */
  private maybeAutoAttempt(): void {
    if (!this.frontmost || !this.everConnected || this.pending) return;
    if (this.state.reconnectFailed || !this.autoArmed) return;
    void this.startAttempt(false);
  }

  /** An attempt (automatic or manual) failed. Block further auto-attempts. */
  private markReconnectFailed(): void {
    if (this.state.connection === 'closed' || this.state.reconnectFailed) return;
    this.update({ reconnectFailed: true, reconnectFailedManual: this.currentAttemptManual });
    if (!this.frontmost) this.armBackgroundResetTimer();
  }

  /** After 2 minutes in the background, a FAILED pane resets to armed DISCONNECTED. */
  private armBackgroundResetTimer(): void {
    this.clearBackgroundResetTimer();
    this.backgroundResetTimer = setTimeout(() => {
      this.backgroundResetTimer = null;
      if (this.state.connection === 'closed') return;
      if (this.state.reconnectFailed) this.update({ reconnectFailed: false });
    }, BACKGROUND_RESET_MS);
  }

  private clearBackgroundResetTimer(): void {
    if (this.backgroundResetTimer) clearTimeout(this.backgroundResetTimer);
    this.backgroundResetTimer = null;
  }

  private current(generation: number, signal: AbortSignal): boolean {
    return (
      !signal.aborted && this.state.generation === generation && this.state.connection !== 'closed'
    );
  }

  private async attach(generation: number, signal: AbortSignal): Promise<void> {
    const current = (): boolean => this.current(generation, signal);
    try {
      if (!current()) return;
      const endpoint = new URL(`api/v1/agents/${this.state.agentId}`, this.hubUrl).href;
      const response = await fetch(endpoint, { credentials: 'include', signal });
      if (!current()) return;
      if (!response.ok) {
        const reason = classifyHttpStatus(response.status);
        const msg = `HTTP ${response.status}: ${response.statusText}`;
        const error = await extractApiError(response, msg);
        if (!current()) return;
        // An HTTP 5xx (or unclassified status) during an attempt is
        // retriable, same as a retriable WS close; 401/403/404 are not.
        this.autoArmed = isRetryableReason(reason);
        this.update({
          connection: 'disconnected',
          disconnectReason: reason,
          error,
        });
        return;
      }
      const agent = (await response.json()) as Agent;
      if (!current()) return;
      if (agent.id?.toLowerCase() !== this.state.agentId)
        throw new Error('Agent metadata does not match the requested UUID.');
      this.seedMetadata(agent);
      this.update({ agent });
      if (!current()) return;
      if (!isTerminalAvailable(agent)) {
        // Not auto-retriable from here: re-arming for a stopped agent goes
        // through markUnavailable/noteAgentAvailable, driven by SSE, not this path.
        this.autoArmed = false;
        this.update({
          connection: 'unavailable',
          disconnectReason: agent.activity === 'offline' ? 'agent-offline' : 'agent-phase',
          error:
            agent.activity === 'offline'
              ? 'Agent is offline. Terminal is not available while the agent is unreachable.'
              : `Agent phase is ${agent.phase}. Terminal is not available until the agent has started.`,
        });
        return;
      }
      // Preflight is authorization only; it cannot prove the broker attach succeeds.
      const preflight = await fetch(`${endpoint}/pty`, { credentials: 'include', signal });
      if (!current()) return;
      if (!preflight.ok) {
        const reason = classifyHttpStatus(preflight.status);
        const message =
          preflight.status === 403
            ? 'You do not have permission to attach to this agent.'
            : preflight.status === 401
              ? 'Authentication required to access this terminal.'
              : preflight.status === 404
                ? 'Agent not found.'
                : await extractApiError(
                    preflight,
                    `Terminal connection failed: ${preflight.statusText}`
                  );
        if (!current()) return;
        this.autoArmed = isRetryableReason(reason);
        this.update({ connection: 'disconnected', disconnectReason: reason, error: message });
        return;
      }
      if (!current()) return;
      const reconnect = this.resources !== null;
      if (!this.resources) {
        const initialized = await this.initialize(agent, signal);
        if (!current()) {
          initialized.dispose();
          return;
        }
        this.resources = initialized;
      }
      const resources = this.resources;
      const size = resources.size();
      if (!validSize(size.cols, size.rows))
        throw new Error('Terminal dimensions are not available.');
      const url = new URL(`${endpoint}/pty`);
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
      url.searchParams.set('cols', String(size.cols));
      url.searchParams.set('rows', String(size.rows));
      const socket = new WebSocket(url.href);
      this.socket = socket;
      const live = (): boolean => current() && this.socket === socket;
      // onopen only means the Hub upgraded the socket (HTTP 101); it does
      // not mean the broker stream opened. Treating onopen as
      // attempt-succeeded let an accept-then-close sequence (Hub upgrades,
      // then closes with 4503/1011 once OpenStream fails) redial in an
      // unbounded tight loop, wiping resources.reset() on every cycle. The
      // attempt only counts as live once the first data frame confirms it —
      // tmux sends a redraw on attach, so it arrives promptly — with a
      // bounded guard so a silently dead stream cannot hang in
      // "Reconnecting..." forever.
      const confirmAttemptLive = (): void => {
        this.clearFirstFrameGuard();
        this.attemptReachedOpen = true;
        this.everConnected = true;
        this.autoArmed = false;
        this.clearBackgroundResetTimer();
        if (reconnect) resources.reset();
        this.update({
          connection: 'connected',
          error: null,
          disconnectReason: null,
          reconnectFailed: false,
          reconnectFailedManual: false,
        });
        this.startHeartbeat(socket, live);
      };
      socket.onopen = (): void => {
        if (!live()) return;
        // The 10s guard bounds reconnect attempts only, not the initial
        // connect. The Hub's OpenStream is fire-and-forget, so time to first
        // byte is the broker's exec plus `tmux attach-session`; a cold
        // sandbox exec or a loaded host can legitimately take longer than
        // 10s on the very first attach, and today's behavior accepts that.
        // Once a session has connected at least once, subsequent attempts
        // (automatic or manual) are bounded, since by then the agent has
        // already proven it can serve within a normal window. The initial
        // connect still waits for the first data frame (this is what fixes
        // the accept-then-close loop for every attempt), it just never
        // times out doing so.
        if (this.everConnected) this.armFirstFrameGuard(socket, live);
      };
      socket.onmessage = (event: MessageEvent): void => {
        if (!live() || typeof event.data !== 'string') return;
        this.lastInboundAt = Date.now();
        try {
          const message = JSON.parse(event.data) as { type: string; data?: string };
          if (message.type === 'pong') {
            this.heartbeatCapable = true;
            return;
          }
          if (message.type === 'data' && typeof message.data === 'string') {
            if (!this.attemptReachedOpen) confirmAttemptLive();
            resources.write(Uint8Array.from(atob(message.data), (c) => c.charCodeAt(0)));
          }
        } catch (error) {
          console.warn('[Terminal] Failed to parse WebSocket message:', error);
        }
      };
      socket.onclose = (event: CloseEvent): void => {
        if (!live()) return;
        this.socket = null;
        this.stopHeartbeat();
        this.clearFirstFrameGuard();
        const reason = closeReasonFor(event.code);
        // If this attempt's socket never proved live and we had connected
        // before, this was itself a failed reconnect attempt, not a fresh
        // disconnect.
        const wasFailedAttempt =
          reason === 'network' && !this.attemptReachedOpen && this.everConnected;
        // Only 'network' (a retriable close code) arms a further attempt; a
        // terminal or detached code clears it, same as any other terminal outcome.
        this.autoArmed = reason === 'network';
        this.update({
          connection: 'disconnected',
          disconnectReason: reason,
          error: reason === 'detached' ? null : `Connection closed (code: ${event.code})`,
        });
        if (reason === 'network') {
          if (wasFailedAttempt) this.markReconnectFailed();
          else this.maybeAutoAttempt();
        }
      };
      socket.onerror = (): void => {
        // Rule: classify in onclose only (fixes the onerror-before-onclose bug).
        // Browsers always fire close after error; this is a defensive fallback
        // for the rare case onclose never arrives.
        if (!live()) return;
        setTimeout(() => {
          if (!live() || this.socket !== socket) return;
          this.socket = null;
          this.stopHeartbeat();
          this.clearFirstFrameGuard();
          try {
            socket.close();
          } catch {
            /* best effort */
          }
          // Mirrors onclose's own wasFailedAttempt/autoArmed handling: this
          // fallback only fires when close never arrived, so it needs the
          // same "was this a failed reconnect attempt" judgment.
          const wasFailedAttempt = !this.attemptReachedOpen && this.everConnected;
          this.autoArmed = true;
          this.update({
            connection: 'disconnected',
            disconnectReason: 'connect-error',
            error: 'WebSocket connection error',
          });
          if (wasFailedAttempt) this.markReconnectFailed();
          else this.maybeAutoAttempt();
        }, CONNECT_ERROR_FALLBACK_MS);
      };
      this.update({ connection: 'connecting', lastSize: size });
    } catch (error) {
      if (current()) {
        this.autoArmed = true; // fetch/setup threw (e.g. a network TypeError): retriable
        this.update({
          connection: 'disconnected',
          disconnectReason: 'network',
          error: error instanceof Error ? error.message : 'Failed to connect to terminal',
        });
      }
    }
  }

  /** Detects a dead peer even when no close frame ever arrives. */
  private startHeartbeat(socket: WebSocket, live: () => boolean): void {
    this.stopHeartbeat();
    this.heartbeatCapable = false;
    this.lastInboundAt = Date.now();
    this.heartbeatInterval = setInterval(() => {
      if (!live() || this.socket !== socket) {
        this.stopHeartbeat();
        return;
      }
      const sentAt = Date.now();
      try {
        socket.send(JSON.stringify({ type: 'ping', timestamp: sentAt }));
      } catch {
        return;
      }
      if (this.heartbeatCheckTimer) clearTimeout(this.heartbeatCheckTimer);
      this.heartbeatCheckTimer = setTimeout(() => {
        if (!live() || this.socket !== socket) return;
        // Capability detection: only enforce the timeout once a pong has ever
        // arrived on this socket. Against an old Hub, the heartbeat is inert.
        if (this.heartbeatCapable && this.lastInboundAt < sentAt) {
          this.handleDeadSocket(socket);
        }
      }, HEARTBEAT_TIMEOUT_MS);
    }, HEARTBEAT_INTERVAL_MS);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatInterval) clearInterval(this.heartbeatInterval);
    if (this.heartbeatCheckTimer) clearTimeout(this.heartbeatCheckTimer);
    this.heartbeatInterval = null;
    this.heartbeatCheckTimer = null;
  }

  /** Starts the bounded wait for the first data frame after onopen. */
  private armFirstFrameGuard(socket: WebSocket, live: () => boolean): void {
    this.clearFirstFrameGuard();
    this.firstFrameTimer = setTimeout(() => {
      this.firstFrameTimer = null;
      if (!live() || this.socket !== socket) return;
      this.handleFirstFrameTimeout(socket);
    }, FIRST_FRAME_TIMEOUT_MS);
  }

  private clearFirstFrameGuard(): void {
    if (this.firstFrameTimer) clearTimeout(this.firstFrameTimer);
    this.firstFrameTimer = null;
  }

  /**
   * No data arrived within the guard window after opening: treat this as a
   * failed/dead attempt instead of hanging in "Reconnecting..." forever, and
   * instead of silently redialing forever.
   */
  private handleFirstFrameTimeout(socket: WebSocket): void {
    if (this.socket !== socket) return;
    this.socket = null;
    this.stopHeartbeat();
    try {
      socket.onopen = null;
      socket.onclose = null;
      socket.onerror = null;
      socket.onmessage = null;
      socket.close();
    } catch {
      /* best effort */
    }
    this.autoArmed = true;
    this.update({
      connection: 'disconnected',
      disconnectReason: 'network',
      error: 'No response from the terminal stream.',
    });
    // This guard only ever arms when everConnected is already true (onopen
    // only calls armFirstFrameGuard for that case), and attemptReachedOpen
    // is still false here (the first data frame would have cleared the
    // guard before it could fire). Unlike onclose and the onerror fallback,
    // which can also see an established socket die, this guard only fires
    // during a reconnect attempt that never produced data, so it always
    // marks the attempt failed.
    this.markReconnectFailed();
  }

  /** No response to ping: go straight to disconnected without waiting for onclose. */
  private handleDeadSocket(socket: WebSocket): void {
    this.stopHeartbeat();
    this.clearFirstFrameGuard();
    if (this.socket !== socket) return;
    this.socket = null;
    try {
      socket.onopen = null;
      socket.onclose = null;
      socket.onerror = null;
      socket.onmessage = null;
      socket.close();
    } catch {
      /* best effort */
    }
    // The heartbeat only runs once connected, so everConnected is already true.
    this.autoArmed = true;
    this.update({
      connection: 'disconnected',
      disconnectReason: 'network',
      error: 'Connection appears to be lost (no response to ping).',
    });
    this.maybeAutoAttempt();
  }

  sendData(data: string): boolean {
    if (this.state.connection !== 'connected' || this.socket?.readyState !== WebSocket.OPEN)
      return false;
    const bytes = new TextEncoder().encode(data);
    // Avoid spreading arbitrarily long paste input onto the JavaScript stack.
    let binary = '';
    for (const byte of bytes) binary += String.fromCharCode(byte);
    this.socket.send(JSON.stringify({ type: 'data', data: btoa(binary) }));
    return true;
  }

  resize(cols: number, rows: number): void {
    if (
      !validSize(cols, rows) ||
      this.state.connection !== 'connected' ||
      this.socket?.readyState !== WebSocket.OPEN
    )
      return;
    this.socket.send(JSON.stringify({ type: 'resize', cols, rows }));
    this.update({ lastSize: { cols, rows } });
  }

  close(reason: 'explicit' | 'navigation' = 'explicit'): void {
    if (this.state.connection === 'closed') return;
    // Cleanup is exhaustive even if a renderer or subscriber throws. Removal
    // must still release the aggregate subscription for the last session.
    const errors: unknown[] = [];
    const attempt = (cleanup: () => void): void => {
      try {
        cleanup();
      } catch (error) {
        errors.push(error);
      }
    };
    if (reason === 'explicit')
      attempt(() => {
        this.sendData('\x02d');
      });
    this.controller?.abort();
    attempt(() => this.stopHeartbeat());
    attempt(() => this.clearFirstFrameGuard());
    attempt(() => this.clearBackgroundResetTimer());
    attempt(() => this.releaseSocket());
    attempt(() => this.releaseResources());
    attempt(() => this.remove());
    attempt(() =>
      this.update({
        generation: this.state.generation + 1,
        connection: 'closed',
        disconnectReason: null,
      })
    );
    this.listeners.clear();
    if (errors.length) throw new AggregateError(errors, 'Terminal session disposal failed.');
  }

  private releaseSocket(): void {
    const socket = this.socket;
    this.socket = null;
    if (socket) socket.close(1000, 'detach');
  }

  private releaseResources(): void {
    const resources = this.resources;
    this.resources = null;
    resources?.dispose();
  }
}

/** Maps a PTY WebSocket close code to a disconnect reason. */
function closeReasonFor(code: number): TerminalDisconnectReason {
  const disposition = classifyPtyClose(code);
  if (disposition === 'detached') return 'detached';
  if (disposition === 'terminal') {
    switch (code) {
      case PTY_CLOSE.AUTH_REQUIRED:
        return 'auth-401';
      case PTY_CLOSE.FORBIDDEN:
        return 'auth-403';
      case PTY_CLOSE.AGENT_NOT_FOUND:
        return 'not-found';
      case PTY_CLOSE.SESSION_GONE:
        return 'session-ended';
      default:
        // Unknown application code: fail safe, same as the Go/TS classifiers.
        return 'server-error';
    }
  }
  return 'network';
}

/** Whether an HTTP-classified disconnect reason is retriable. */
function isRetryableReason(reason: TerminalDisconnectReason): boolean {
  return reason === 'server-error';
}

function classifyHttpStatus(status: number): TerminalDisconnectReason {
  if (status === 401) return 'auth-401';
  if (status === 403) return 'auth-403';
  if (status === 404) return 'not-found';
  if (status >= 500) return 'server-error';
  return 'server-error'; // unclassified HTTP errors are server-side, not network
}

function validSize(cols: number, rows: number): boolean {
  return Number.isInteger(cols) && Number.isInteger(rows) && cols > 0 && rows > 0;
}
