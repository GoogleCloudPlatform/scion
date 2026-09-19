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

import {
  TerminalSessionRegistry,
  type TerminalScope,
  type TerminalSession,
  type TerminalResourceInitializer,
} from './terminal-sessions.js';

/** Document focus observation is not a guarantee of desktop foreground activation. */
export type TerminalFocusResult = 'document-focused' | 'not-confirmed';
export interface TerminalCoordinatorAdapter {
  initialize: TerminalResourceInitializer;
  /** Owner-only creation bridge; must return this registry's requested entry. */
  create?(registry: TerminalSessionRegistry, agentId: string): TerminalSession;
  /**
   * Activate the retained single-pane presentation. Guard async work with signal.
   * Combine it with host navigation guards and REJECT a canceled selection so the
   * coordinator does not request focus. This signal covers document/account teardown.
   */
  select(session: TerminalSession, signal: AbortSignal, requestId?: string): void | Promise<void>;
  /** Optional host activation adapter. Must report observation, not selection success. */
  focus?(): Promise<TerminalFocusResult>;
}
export interface TerminalOpenResult {
  readonly status:
    | 'selected'
    | 'pending'
    | 'unsupported'
    | 'stopped'
    | 'request-id-conflict'
    | 'selection-failed';
  readonly requestId: string;
  readonly agentId: string;
  readonly generation: string | null;
  readonly focus: TerminalFocusResult;
}
interface Request {
  agentId: string;
  generation: string | null;
  done: boolean;
  result: Promise<TerminalOpenResult>;
  resolve(result: TerminalOpenResult): void;
}
interface Message {
  key: string;
  type: 'discover' | 'owner' | 'open' | 'ack';
  requestId: string;
  agentId: string;
  generation: string | null;
  status?: TerminalOpenResult['status'];
  focus?: TerminalFocusResult;
}
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const token = (value: unknown): value is string =>
  typeof value === 'string' && value.length > 0 && value.length <= 256;

/**
 * One instance per authenticated document lifetime. Construct from trusted bootstrap
 * identity, never from a route or peer message. Keep it through SPA mode changes.
 * Same-origin peers are not an authentication boundary; the Hub authorizes attaches.
 */
export class TerminalCoordinator {
  readonly coordinationKey: string;
  private readonly registry: TerminalSessionRegistry;
  private channel: BroadcastChannel | null = null;
  private available = false;
  private ownerGeneration: string | null = null;
  private stopped = false;
  private claiming: Promise<boolean> | null = null;
  private release: (() => void) | null = null;
  private readonly lifetime = new AbortController();
  private readonly requests = new Map<string, Request>();
  private readonly executed = new Map<
    string,
    { agentId: string; result: Promise<TerminalOpenResult> }
  >();

  constructor(
    scope: TerminalScope,
    private readonly adapter: TerminalCoordinatorAdapter
  ) {
    // Reuse registry validation; normalization must match its origin/base-path scope.
    this.registry = new TerminalSessionRegistry(scope);
    const hubUrl = new URL(scope.hubUrl).href.replace(/\/+$/, '') + '/';
    this.coordinationKey = `terminal-owner:v1:${JSON.stringify([hubUrl, scope.accountId])}`;
    if (globalThis.isSecureContext && navigator.locks && typeof BroadcastChannel === 'function') {
      try {
        this.channel = new BroadcastChannel(this.coordinationKey);
        this.channel.onmessage = (event: MessageEvent<unknown>): void => this.receive(event.data);
        this.available = true;
      } catch {
        /* Explicit unsupported result; never attach without coordination. */
      }
    }
    window.addEventListener('pagehide', this.onPageHide);
  }

  get supported(): boolean {
    return this.available;
  }
  get isOwner(): boolean {
    return !this.stopped && this.ownerGeneration !== null;
  }
  get generation(): string | null {
    return this.ownerGeneration;
  }
  /** Only the owner's retained host receives document-local session handles. */
  get sessions(): readonly TerminalSession[] {
    return this.isOwner ? this.registry.list() : [];
  }

  /**
   * Retry with the SAME request ID after pending; a new user intent gets a new ID.
   * Timeout is not cancellation. The open intent survives requester navigation;
   * no per-request cross-tab cancellation or automatic retry is performed.
   */
  async open(
    agentId: string,
    suppliedRequestId?: string,
    timeoutMs = 1000
  ): Promise<TerminalOpenResult> {
    if (!uuid.test(agentId)) throw new Error('Terminal requires an agent UUID.');
    if (suppliedRequestId !== undefined && !token(suppliedRequestId))
      throw new Error('Terminal request ID required (maximum 256 characters).');
    if (!Number.isFinite(timeoutMs) || timeoutMs < 0)
      throw new Error('Invalid acknowledgment timeout.');
    agentId = agentId.toLowerCase();
    // No request is submitted in these terminal states, so an omitted ID needs
    // only a result marker. Insecure HTTP does not expose crypto.randomUUID.
    const requestId =
      suppliedRequestId ?? (this.stopped || !this.available ? 'unsubmitted' : crypto.randomUUID());
    const outcome = (status: TerminalOpenResult['status']): TerminalOpenResult => ({
      status,
      agentId,
      requestId,
      generation: null,
      focus: 'not-confirmed',
    });
    if (this.stopped) return outcome('stopped');
    if (!this.available) return outcome('unsupported');
    let request = this.requests.get(requestId);
    if (request && request.agentId !== agentId) return outcome('request-id-conflict');
    if (!request) {
      let resolve!: Request['resolve'];
      const result = new Promise<TerminalOpenResult>((done) => {
        resolve = done;
      });
      request = { agentId, generation: null, done: false, result, resolve };
      this.requests.set(requestId, request);
    }
    if (!request.done) void this.route(requestId, request);
    // Bound the whole operation, including a delayed lock callback. Expiration only
    // changes the caller's result; it never releases, steals or assumes a lock.
    let timer: ReturnType<typeof setTimeout> | undefined;
    const deadline = new Promise<TerminalOpenResult>((resolve) => {
      timer = setTimeout(() => resolve(outcome('pending')), timeoutMs);
    });
    return Promise.race([request.result, deadline]).finally(() => clearTimeout(timer));
  }

  private async route(requestId: string, request: Request): Promise<void> {
    try {
      const owner = await this.claim();
      if (this.stopped || request.done) return;
      if (owner) {
        request.generation = this.ownerGeneration;
        this.execute({
          key: this.coordinationKey,
          type: 'open',
          requestId,
          agentId: request.agentId,
          generation: request.generation,
        });
      } else {
        this.send({
          key: this.coordinationKey,
          type: 'discover',
          requestId,
          agentId: request.agentId,
          generation: null,
        });
      }
    } catch {
      this.available = false;
      this.finish(request, {
        status: 'unsupported',
        requestId,
        agentId: request.agentId,
        generation: null,
        focus: 'not-confirmed',
      });
    }
  }

  private claim(): Promise<boolean> {
    if (this.isOwner) return Promise.resolve(true);
    if (this.claiming) return this.claiming;
    this.claiming = new Promise<boolean>((resolve, reject) => {
      void navigator.locks
        .request(this.coordinationKey, { mode: 'exclusive', ifAvailable: true }, async (lock) => {
          if (!lock || this.stopped) {
            resolve(false);
            return;
          }
          this.ownerGeneration = crypto.randomUUID();
          const held = new Promise<void>((done) => {
            this.release = done;
          });
          resolve(true);
          await held;
        })
        .catch(reject);
    }).finally(() => {
      this.claiming = null;
    });
    return this.claiming;
  }

  private send(message: Message): void {
    this.channel?.postMessage(message);
  }

  private receive(data: unknown): void {
    if (this.stopped || !data || typeof data !== 'object') return;
    const message = data as Partial<Message>;
    if (
      message.key !== this.coordinationKey ||
      !token(message.requestId) ||
      typeof message.agentId !== 'string' ||
      !uuid.test(message.agentId)
    )
      return;
    const request = this.requests.get(message.requestId);
    if (message.type === 'discover' && this.isOwner) {
      this.send({
        key: this.coordinationKey,
        type: 'owner',
        requestId: message.requestId,
        agentId: message.agentId,
        generation: this.ownerGeneration,
      });
    } else if (message.type === 'owner' && token(message.generation)) {
      if (!request || request.done || request.agentId !== message.agentId) return;
      request.generation = message.generation;
      this.send({
        key: this.coordinationKey,
        type: 'open',
        requestId: message.requestId,
        agentId: request.agentId,
        generation: message.generation,
      });
    } else if (message.type === 'open' && token(message.generation)) {
      this.execute({
        key: this.coordinationKey,
        type: 'open',
        requestId: message.requestId,
        agentId: message.agentId.toLowerCase(),
        generation: message.generation,
      });
    } else if (message.type === 'ack') {
      if (
        !request ||
        request.done ||
        !token(message.generation) ||
        request.generation !== message.generation ||
        request.agentId !== message.agentId ||
        !['selected', 'selection-failed', 'request-id-conflict'].includes(message.status ?? '') ||
        !['document-focused', 'not-confirmed'].includes(message.focus ?? '')
      )
        return;
      this.finish(request, {
        status: message.status!,
        focus: message.focus!,
        agentId: message.agentId,
        requestId: message.requestId,
        generation: message.generation,
      });
    }
  }

  private execute(message: Message): void {
    if (!this.isOwner || message.generation !== this.ownerGeneration) return;
    const existing = this.executed.get(message.requestId);
    if (existing && existing.agentId !== message.agentId) {
      this.deliver(message, { ...message, status: 'request-id-conflict', focus: 'not-confirmed' });
      return;
    }
    if (!existing) {
      // Install deduplication before registry/UI callbacks, including reentrant opens.
      const result = Promise.resolve().then(async (): Promise<TerminalOpenResult> => {
        const reply: TerminalOpenResult = {
          requestId: message.requestId,
          agentId: message.agentId,
          generation: message.generation,
          status: 'stopped',
          focus: 'not-confirmed',
        };
        if (!this.isOwner) return reply;
        try {
          const session =
            this.registry.list().find((entry) => entry.state.agentId === message.agentId) ??
            (this.adapter.create
              ? this.adapter.create(this.registry, message.agentId)
              : this.registry.open(message.agentId, this.adapter.initialize));
          if (session.state.agentId !== message.agentId || !this.registry.list().includes(session))
            throw new Error('Terminal adapter returned a foreign session.');
          await this.adapter.select(session, this.lifetime.signal, message.requestId);
          if (!this.isOwner) return reply;
          const focus = await this.focus();
          return { ...reply, status: 'selected', focus };
        } catch {
          return { ...reply, status: 'selection-failed' };
        }
      });
      this.executed.set(message.requestId, { agentId: message.agentId, result });
    }
    void this.executed
      .get(message.requestId)!
      .result.then((result) => this.deliver(message, result));
  }

  private async focus(): Promise<TerminalFocusResult> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      // A host adapter must not delay acknowledgment indefinitely.
      const timeout = new Promise<TerminalFocusResult>((resolve) => {
        timer = setTimeout(() => resolve('not-confirmed'), 150);
      });
      return await Promise.race([
        this.adapter.focus ? this.adapter.focus() : observeFocus(),
        timeout,
      ]);
    } catch {
      return 'not-confirmed';
    } finally {
      clearTimeout(timer);
    }
  }

  private deliver(message: Message, result: TerminalOpenResult): void {
    if (!this.isOwner || message.generation !== this.ownerGeneration) return;
    const ack: Message = { ...message, ...result, type: 'ack' };
    this.receive(ack); // BroadcastChannel does not echo to the sending object.
    this.send(ack);
  }

  private finish(request: Request, result: TerminalOpenResult): void {
    request.done = true;
    request.resolve(result);
  }

  private readonly onPageHide = (): void => this.stop();

  /**
   * Document/account teardown ONLY. Never call on route or mode changes.
   * Attempts every session close; throws AggregateError on failure and retains the lock.
   */
  stop(): void {
    if (this.stopped) return;
    const sessions = this.sessions;
    this.stopped = true;
    this.lifetime.abort();
    window.removeEventListener('pagehide', this.onPageHide);
    this.channel?.close();
    this.channel = null;
    for (const [requestId, request] of this.requests) {
      if (!request.done)
        this.finish(request, {
          status: 'stopped',
          requestId,
          agentId: request.agentId,
          generation: request.generation,
          focus: 'not-confirmed',
        });
    }
    // One renderer failure must not leave other transports live after teardown.
    // Report all failures only after every session has had a chance to close.
    const failures: unknown[] = [];
    for (const session of sessions) {
      try {
        session.close();
      } catch (error) {
        failures.push(error);
      }
    }
    // Keep the lock until document exit on ANY failure. Repeated stop is a no-op;
    // it must not retry failed disposal or accidentally release this authority.
    if (failures.length) throw new AggregateError(failures, 'Terminal session teardown failed.');
    this.ownerGeneration = null;
    this.release?.();
    this.requests.clear();
    this.executed.clear();
  }
}

async function observeFocus(): Promise<TerminalFocusResult> {
  window.focus();
  await new Promise<void>((resolve) => setTimeout(resolve, 100));
  return document.visibilityState === 'visible' && document.hasFocus()
    ? 'document-focused'
    : 'not-confirmed';
}
