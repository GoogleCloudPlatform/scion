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
 * Open decisions in a single chat conversation (ptone/scion#8594):
 * API client and live-update controller for
 * `GET /api/v1/chat/conversations/{key}/decisions`.
 */

import { apiFetch, extractApiError } from './api.js';
import { isOpenDecision } from './decision-prompts.js';
import { stateManager } from './state.js';
import { subscribeMessageActionUpdated } from '../components/shared/chat/message-action-state.js';
import type { DecisionAnswerMode, DecisionView } from '../shared/types.js';

/** Per-viewer prompt counts for a single conversation (prompts, not rows). */
export interface ConversationDecisionCounts {
  waitingOnYou: number;
  waitingOnOthers: number;
}

/** One open decision prompt returned by the conversation decisions endpoint. */
export interface ConversationOpenPromptItem {
  promptMessageId: string;
  promptAvailable: boolean;
  createdAt: string;
  askerAgentId: string;
  askerAgentName: string;
  answerMode: DecisionAnswerMode;
  openCount: number;
  totalCount: number;
  question: string;
  waitingOnYou: boolean;
  awaitingConfirmation: boolean;
  disabledReason?: string;
}

/** Response of `GET /api/v1/chat/conversations/{key}/decisions`. */
export interface ConversationOpenDecisionsResponse {
  counts: ConversationDecisionCounts;
  prompts: ConversationOpenPromptItem[];
  truncated: boolean;
}

/** Thrown by `fetchConversationOpenDecisions` with HTTP status. */
export class ConversationDecisionsError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'ConversationDecisionsError';
  }
}

/**
 * Fetches open decision prompts for `conversationKey`.
 * Returns `null` on 404 (experiment off, older hub, or conversation no longer
 * readable) so the toolbar component can hide quietly.
 */
export async function fetchConversationOpenDecisions(
  conversationKey: string,
  signal?: AbortSignal
): Promise<ConversationOpenDecisionsResponse | null> {
  const path = `/api/v1/chat/conversations/${encodeURIComponent(conversationKey)}/decisions`;
  const res = await apiFetch(path, signal ? { signal } : undefined);
  if (res.status === 404) return null;
  if (!res.ok) {
    throw new ConversationDecisionsError(
      await extractApiError(res, 'Could not load open decisions'),
      res.status
    );
  }
  const body = (await res.json()) as Partial<ConversationOpenDecisionsResponse> | null;
  return {
    counts: {
      waitingOnYou: body?.counts?.waitingOnYou ?? 0,
      waitingOnOthers: body?.counts?.waitingOnOthers ?? 0,
    },
    prompts: Array.isArray(body?.prompts) ? body.prompts : [],
    truncated: Boolean(body?.truncated),
  };
}

/** Trailing coalesce delay (ms) for live decision events. */
export const OPEN_DECISIONS_COALESCE_MS = 250;

/** Payload shape of `chat.decision.updated` routed through `stateManager`. */
interface DecisionUpdatedPayload {
  messageId?: string;
  threadId?: string;
  decision?: DecisionView;
}

/** Snapshot emitted to the component whenever state changes. */
export interface ConversationOpenDecisionsSnapshot {
  counts: ConversationDecisionCounts;
  prompts: ConversationOpenPromptItem[];
  truncated: boolean;
  loading: boolean;
  error: string;
  hiddenBy404: boolean;
  loadedOnce: boolean;
  /** Incremented only when a server response is applied (never on optimistic edits). */
  serverSnapshotSeq: number;
}

export interface ConversationOpenDecisionsControllerOptions {
  /** Called whenever the snapshot changes. */
  onChange: (snapshot: ConversationOpenDecisionsSnapshot) => void;
  /** Override fetch function (for tests). */
  fetchFn?: typeof fetchConversationOpenDecisions;
  /** Override SSE EventTarget (defaults to `stateManager`). */
  eventSource?: EventTarget;
  /** Override coalesce delay in ms (defaults to `OPEN_DECISIONS_COALESCE_MS`). */
  coalesceMs?: number;
}

/**
 * Live controller for one conversation's open decisions:
 * - Fetches on conversation set/switch (bumping generation and aborting stale requests).
 * - Subscribes to `chat.decision.updated` (optimistic drop/decrement + 250 ms coalesced refetch),
 *   `chat-message-received` (for `decision-prompt` messages in this conversation),
 *   `chat-message-deleted` (marks listed prompt unavailable + coalesced refetch),
 *   `connected` (SSE reconnect), and `visibilitychange` (page return, no polling).
 * - At most one refetch in flight, with at most one owed refetch queued behind it.
 */
export class ConversationOpenDecisionsController {
  private readonly onChange: (snapshot: ConversationOpenDecisionsSnapshot) => void;
  private readonly fetchFn: typeof fetchConversationOpenDecisions;
  private readonly eventSource: EventTarget;
  private readonly coalesceMs: number;

  private conversationKey = '';
  private connected = false;
  private generation = 0;
  /** Bumped on every optimistic edit so stale in-flight responses are discarded. */
  private optimisticEpoch = 0;
  private abort: AbortController | null = null;
  private inFlight = false;
  private refetchOwed = false;
  private coalesceTimer: ReturnType<typeof setTimeout> | null = null;

  private unsubscribeDecision: (() => void) | null = null;

  private state: ConversationOpenDecisionsSnapshot = {
    counts: { waitingOnYou: 0, waitingOnOthers: 0 },
    prompts: [],
    truncated: false,
    loading: false,
    error: '',
    hiddenBy404: false,
    loadedOnce: false,
    serverSnapshotSeq: 0,
  };

  constructor(opts: ConversationOpenDecisionsControllerOptions) {
    this.onChange = opts.onChange;
    this.fetchFn = opts.fetchFn ?? fetchConversationOpenDecisions;
    this.eventSource = opts.eventSource ?? stateManager;
    this.coalesceMs = opts.coalesceMs ?? OPEN_DECISIONS_COALESCE_MS;
  }

  get snapshot(): ConversationOpenDecisionsSnapshot {
    return this.state;
  }

  connect(): void {
    if (this.connected) return;
    this.connected = true;
    this.unsubscribeDecision = subscribeMessageActionUpdated<DecisionUpdatedPayload>(
      'decision',
      this.handleDecisionUpdated,
      {
        eventSource: this.eventSource,
        threadId: () => this.conversationKey,
      }
    );
    this.eventSource.addEventListener('chat-message-received', this.handleMessageReceived);
    this.eventSource.addEventListener('chat-message-deleted', this.handleMessageDeleted);
    this.eventSource.addEventListener('connected', this.handleReconnect);
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.handleVisibilityChange);
    }
    if (this.conversationKey) {
      void this.refetchNow();
    }
  }

  disconnect(): void {
    if (!this.connected) return;
    this.connected = false;
    if (this.unsubscribeDecision) {
      this.unsubscribeDecision();
      this.unsubscribeDecision = null;
    }
    this.eventSource.removeEventListener('chat-message-received', this.handleMessageReceived);
    this.eventSource.removeEventListener('chat-message-deleted', this.handleMessageDeleted);
    this.eventSource.removeEventListener('connected', this.handleReconnect);
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.handleVisibilityChange);
    }
    this.clearCoalesceTimer();
    this.generation++;
    this.abort?.abort();
    this.abort = null;
    this.inFlight = false;
    this.refetchOwed = false;
  }

  setConversation(key: string): void {
    if (key === this.conversationKey) return;
    this.conversationKey = key;
    this.clearCoalesceTimer();
    this.generation++;
    this.optimisticEpoch = 0;
    this.abort?.abort();
    this.abort = null;
    this.inFlight = false;
    this.refetchOwed = false;
    this.state = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      prompts: [],
      truncated: false,
      loading: false,
      error: '',
      hiddenBy404: false,
      loadedOnce: false,
      serverSnapshotSeq: 0,
    };
    this.emit();
    if (this.connected && this.conversationKey) {
      void this.refetchNow();
    }
  }

  /** Trigger an immediate refetch (e.g. panel open, retry, reconnect, page return). */
  async refetchNow(): Promise<void> {
    if (!this.connected || !this.conversationKey) return;
    this.clearCoalesceTimer();
    if (this.inFlight) {
      this.refetchOwed = true;
      return;
    }
    const key = this.conversationKey;
    const generation = this.generation;
    const startEpoch = this.optimisticEpoch;
    const abort = new AbortController();
    this.abort = abort;
    this.inFlight = true;
    this.state = { ...this.state, loading: true };
    this.emit();
    try {
      const resp = await this.fetchFn(key, abort.signal);
      if (generation !== this.generation || abort.signal.aborted) return;
      // An optimistic edit happened while this request was in flight: discard
      // this pre-edit response so it cannot flicker dropped rows back in, and
      // ensure a fresh refetch runs immediately in finally.
      if (startEpoch !== this.optimisticEpoch) {
        this.refetchOwed = true;
        return;
      }
      if (resp === null) {
        this.state = {
          counts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          truncated: false,
          loading: false,
          error: '',
          hiddenBy404: true,
          loadedOnce: true,
          serverSnapshotSeq: this.state.serverSnapshotSeq + 1,
        };
        this.emit();
        return;
      }
      this.state = {
        counts: resp.counts,
        prompts: resp.prompts,
        truncated: resp.truncated,
        loading: false,
        error: '',
        hiddenBy404: false,
        loadedOnce: true,
        serverSnapshotSeq: this.state.serverSnapshotSeq + 1,
      };
      this.emit();
    } catch (err) {
      if (generation !== this.generation || abort.signal.aborted) return;
      const message =
        err instanceof Error && err.message ? err.message : 'Could not load open decisions.';
      this.state = { ...this.state, loading: false, error: message };
      this.emit();
    } finally {
      if (generation === this.generation) {
        this.inFlight = false;
        this.abort = null;
        if (this.refetchOwed) {
          this.refetchOwed = false;
          void this.refetchNow();
        }
      }
    }
  }

  /** Schedule a 250 ms trailing coalesced refetch. */
  scheduleRefetch(): void {
    if (!this.connected || !this.conversationKey) return;
    if (this.inFlight) {
      this.refetchOwed = true;
      return;
    }
    this.clearCoalesceTimer();
    this.coalesceTimer = setTimeout(() => {
      this.coalesceTimer = null;
      void this.refetchNow();
    }, this.coalesceMs);
  }

  private clearCoalesceTimer(): void {
    if (this.coalesceTimer !== null) {
      clearTimeout(this.coalesceTimer);
      this.coalesceTimer = null;
    }
  }

  private emit(): void {
    this.onChange(this.state);
  }

  private handleDecisionUpdated = (payload: DecisionUpdatedPayload): void => {
    if (!this.conversationKey) return;
    const msgId = payload.messageId;
    const dec = payload.decision;
    if (msgId && dec && !isOpenDecision(dec)) {
      const idx = this.state.prompts.findIndex((p) => p.promptMessageId === msgId);
      if (idx >= 0) {
        this.optimisticEpoch++;
        const entry = this.state.prompts[idx];
        if (entry.openCount <= 1) {
          const nextPrompts = this.state.prompts.filter((p) => p.promptMessageId !== msgId);
          const nextCounts = {
            waitingOnYou: Math.max(
              0,
              this.state.counts.waitingOnYou - (entry.waitingOnYou ? 1 : 0)
            ),
            waitingOnOthers: Math.max(
              0,
              this.state.counts.waitingOnOthers - (entry.waitingOnYou ? 0 : 1)
            ),
          };
          this.state = { ...this.state, prompts: nextPrompts, counts: nextCounts };
          this.emit();
        } else {
          const nextPrompts = this.state.prompts.map((p) =>
            p.promptMessageId === msgId ? { ...p, openCount: p.openCount - 1 } : p
          );
          this.state = { ...this.state, prompts: nextPrompts };
          this.emit();
        }
      }
    }
    this.scheduleRefetch();
  };

  private handleMessageReceived = (e: Event): void => {
    if (!this.conversationKey) return;
    const raw = (e as CustomEvent).detail as
      | {
          data?: {
            threadId?: string;
            conversationKey?: string;
            type?: string;
          };
          threadId?: string;
          conversationKey?: string;
          type?: string;
        }
      | undefined;
    const data = raw?.data ?? raw;
    if (!data) return;
    const key = data.threadId ?? data.conversationKey ?? '';
    if (key !== this.conversationKey) return;
    if (data.type === 'decision-prompt') {
      this.scheduleRefetch();
    }
  };

  private handleMessageDeleted = (e: Event): void => {
    if (!this.conversationKey) return;
    const raw = (e as CustomEvent).detail as
      | { data?: { messageId?: string }; messageId?: string }
      | undefined;
    const messageId = raw?.data?.messageId ?? raw?.messageId;
    if (!messageId) return;
    if (this.state.prompts.some((p) => p.promptMessageId === messageId)) {
      this.optimisticEpoch++;
      const nextPrompts = this.state.prompts.map((p) =>
        p.promptMessageId === messageId ? { ...p, promptAvailable: false } : p
      );
      this.state = { ...this.state, prompts: nextPrompts };
      this.emit();
      this.scheduleRefetch();
    }
  };

  private handleReconnect = (): void => {
    void this.refetchNow();
  };

  private handleVisibilityChange = (): void => {
    if (typeof document !== 'undefined' && document.visibilityState === 'visible') {
      void this.refetchNow();
    }
  };
}
