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
  readonly lastSize: TerminalSize | null;
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
  /** Immediately reports current state; returns an unsubscribe function. */
  subscribe(listener: (state: TerminalSessionState) => void): () => void;
  /** Explicit attach/reconnect. Shared promise resolves after socket setup, not handshake. */
  connect(): Promise<void>;
  /** Immediate transport write, including xterm protocol responses. Never queues input. */
  sendData(data: string): boolean;
  resize(cols: number, rows: number): void;
  /** navigation is ONLY for the disposable legacy page, not retained workspace navigation. */
  close(reason?: 'explicit' | 'navigation'): void;
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Document-local registry. Browser ownership and UI selection belong to the coordinator. */
export class TerminalSessionRegistry {
  private readonly sessions = new Map<string, Session>();
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
        }
      },
      (agent) => this.metadata.seed(id, agent)
    );
    this.sessions.set(id, session);
    this.metadata.retain(id);
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
}

class Session implements TerminalSession {
  private snapshot: TerminalSessionState;
  private readonly listeners = new Set<(state: TerminalSessionState) => void>();
  private controller: AbortController | null = null;
  private socket: WebSocket | null = null;
  private resources: TerminalResources | null = null;
  private pending: Promise<void> | null = null;

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
      lastSize: null,
    };
  }

  get state(): TerminalSessionState {
    return this.snapshot;
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
    if (this.pending) return this.pending;
    if (['connected', 'connecting', 'closed'].includes(this.state.connection))
      return Promise.resolve();
    this.controller?.abort();
    this.releaseSocket();
    const controller = new AbortController();
    this.controller = controller;
    const generation = this.state.generation + 1;
    // Install the promise before notifying subscribers or invoking consumer code.
    const attempt = Promise.resolve().then(() => this.attach(generation, controller.signal));
    this.pending = attempt;
    this.update({ generation, connection: 'loading', error: null });
    void attempt.finally(() => {
      if (this.pending === attempt) this.pending = null;
    });
    return attempt;
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
      if (!response.ok)
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      const agent = (await response.json()) as Agent;
      if (!current()) return;
      if (agent.id?.toLowerCase() !== this.state.agentId)
        throw new Error('Agent metadata does not match the requested UUID.');
      this.seedMetadata(agent);
      this.update({ agent });
      if (!current()) return;
      if (!isTerminalAvailable(agent)) {
        this.update({
          connection: 'unavailable',
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
        throw new Error(message);
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
      socket.onopen = (): void => {
        if (!live()) return;
        if (reconnect) resources.reset();
        this.update({ connection: 'connected', error: null });
      };
      socket.onmessage = (event: MessageEvent): void => {
        if (!live() || typeof event.data !== 'string') return;
        try {
          const message = JSON.parse(event.data) as { type: string; data?: string };
          if (message.type === 'data' && typeof message.data === 'string') {
            resources.write(Uint8Array.from(atob(message.data), (c) => c.charCodeAt(0)));
          }
        } catch (error) {
          console.warn('[Terminal] Failed to parse WebSocket message:', error);
        }
      };
      socket.onclose = (event: CloseEvent): void => {
        if (!live()) return;
        this.socket = null;
        this.update({
          connection: 'disconnected',
          error: event.code === 1000 ? null : `Connection closed (code: ${event.code})`,
        });
      };
      socket.onerror = (): void => {
        if (!live()) return;
        this.releaseSocket();
        this.update({ connection: 'disconnected', error: 'WebSocket connection error' });
      };
      this.update({ connection: 'connecting', lastSize: size });
    } catch (error) {
      if (current())
        this.update({
          connection: 'disconnected',
          error: error instanceof Error ? error.message : 'Failed to connect to terminal',
        });
    }
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
    attempt(() => this.releaseSocket());
    attempt(() => this.releaseResources());
    attempt(() => this.remove());
    attempt(() => this.update({ generation: this.state.generation + 1, connection: 'closed' }));
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

function validSize(cols: number, rows: number): boolean {
  return Number.isInteger(cols) && Number.isInteger(rows) && cols > 0 && rows > 0;
}
