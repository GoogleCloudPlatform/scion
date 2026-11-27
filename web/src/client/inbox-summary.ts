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
 * InboxSummaryStore — client-side store for `GET /api/v1/users/me/inbox/summary`
 * (design §6.5).
 *
 * - Lazily started when `web.inbox_dashboard` is enabled.
 * - Cross-tab coordination: leader election via Web Locks (`scion-inbox-summary`),
 *   broadcasting fetched summaries over `localStorage` (`scion:inbox-summary`).
 * - Debounces invalidation triggers (750 ms trailing / 4 s max-wait) so bursts
 *   of SSE events fold into a single fetch.
 * - Always fetches `previews=0`; Inbox cards render summary-of-state prose from
 *   `count`, `capped`, and `breakdown`.
 * - Safety poll: 5 min while SSE is healthy, 30 s while unhealthy.
 * - Schedules an expiry refresh from `min(category.nextExpiresAt)` across
 *   expiring categories (`pending_secrets`, `agent_secrets`).
 */

import { apiFetch } from './api.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  resetInboxSummaryBadgeCacheForTests,
} from './inbox-summary-loader.js';
import { PENDING_SECRETS_CHANGED_EVENT } from './pending-secrets.js';
import { stateManager } from './state.js';
import { FEATURE_FLAGS_CHANGED_EVENT, isInboxDashboardEnabled } from '../utils/feature-flags.js';

export { INBOX_SUMMARY_CHANGED_EVENT };
export const INBOX_SUMMARY_API = '/api/v1/users/me/inbox/summary';
export const INBOX_LOCK_NAME = 'scion-inbox-summary';
export const INBOX_SHARE_KEY = 'scion:inbox-summary';

export const INBOX_DEBOUNCE_MS = 750;
export const INBOX_MAX_WAIT_MS = 4000;
export const INBOX_POLL_HEALTHY_MS = 5 * 60 * 1000;
export const INBOX_POLL_UNHEALTHY_MS = 30 * 1000;
export const INBOX_FETCH_TIMEOUT_MS = 15_000;
export const INBOX_LOCK_WAIT_MS = 10_000;

const MAX_EXPIRY_TIMER_MS = 60 * 60 * 1000; // 1 hour clamp

export type InboxCategoryState = 'ok' | 'disabled' | 'unavailable' | 'error';

export type InboxCategoryKey = 'decisions' | 'waiting' | 'pending_secrets' | 'reviews' | 'chat';

export type InboxBreakdownKey =
  | 'agents'
  | 'projects'
  | 'toAddress'
  | 'requested'
  | 'unreadConversations'
  | 'dms'
  | 'mentions'
  | 'starredMessages'
  | 'starredMessagesCapped';

export interface InboxCategory<TPreview = unknown> {
  state: InboxCategoryState;
  count: number;
  capped: boolean;
  countsTowardTotal: boolean;
  href: string;
  errorCode?: string;
  previews: TPreview[];
  agentIds?: string[];
  breakdown?: Partial<Record<InboxBreakdownKey, number>> & Record<string, number>;
  nextExpiresAt?: string;
}

export interface InboxSummaryResponse {
  generatedAt: string;
  total: number;
  totalCapped: boolean;
  categories: {
    decisions: InboxCategory;
    waiting: InboxCategory;
    pending_secrets: InboxCategory;
    reviews: InboxCategory;
    chat: InboxCategory;
    agent_secrets?: InboxCategory;
  };
}

export interface InboxHintEvent {
  category?: string;
  agentId?: string;
  activity?: string;
}

interface SharedInboxPayload {
  userId: string;
  fetchedAt: number;
  summary: InboxSummaryResponse;
}

function readSharedPayload(userId: string): SharedInboxPayload | null {
  try {
    const raw = localStorage.getItem(INBOX_SHARE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<SharedInboxPayload>;
    if (
      !parsed ||
      parsed.userId !== userId ||
      typeof parsed.fetchedAt !== 'number' ||
      !parsed.summary
    ) {
      return null;
    }
    return parsed as SharedInboxPayload;
  } catch {
    return null;
  }
}

function writeSharedPayload(payload: SharedInboxPayload): void {
  try {
    localStorage.setItem(INBOX_SHARE_KEY, JSON.stringify(payload));
  } catch {
    // Ignore storage errors.
  }
}

export class InboxSummaryStore {
  private started = false;
  private userId = '';
  private _summary: InboxSummaryResponse | null = null;
  private _loading = false;
  private _error: string | null = null;
  private _unavailable404 = false;

  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private maxWaitTimer: ReturnType<typeof setTimeout> | null = null;
  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private expiryTimer: ReturnType<typeof setTimeout> | null = null;
  private inFlight = false;
  private pendingRefetch = false;
  private pendingEventAt = 0;

  private boundOnInboxChanged = (e: Event): void => {
    const detail = (e as CustomEvent<InboxHintEvent>).detail;
    if (detail?.category === 'waiting' && detail.agentId) {
      const knownIds = this._summary?.categories?.waiting?.agentIds ?? [];
      const isKnown = knownIds.includes(detail.agentId);
      const isWaiting = detail.activity === 'waiting_for_input';
      if ((isWaiting && isKnown) || (!isWaiting && !isKnown)) {
        return;
      }
    }
    this.requestRefresh();
  };

  private boundOnTriggerEvent = (): void => {
    this.requestRefresh();
  };

  private boundOnVisibilityChange = (): void => {
    if (typeof document !== 'undefined' && document.visibilityState === 'visible') {
      this.requestRefresh();
    }
  };

  private boundOnStorage = (e: StorageEvent): void => {
    if (e.key === INBOX_SHARE_KEY && this.userId) {
      const shared = readSharedPayload(this.userId);
      if (shared) {
        this.applySummary(shared.summary);
      }
    }
  };

  private boundOnFeatureFlagsChanged = (): void => {
    if (!isInboxDashboardEnabled()) {
      this.stop();
    }
  };

  get summary(): InboxSummaryResponse | null {
    return this._summary;
  }

  get total(): number {
    return this._summary?.total ?? 0;
  }

  get totalCapped(): boolean {
    return this._summary?.totalCapped ?? false;
  }

  get loading(): boolean {
    return this._loading;
  }

  get error(): string | null {
    return this._error;
  }

  get unavailable404(): boolean {
    return this._unavailable404;
  }

  formatBadge(): string | null {
    if (this.total <= 0) return null;
    return this.totalCapped ? `${this.total}+` : String(this.total);
  }

  /**
   * Lazily starts the store when `web.inbox_dashboard` is enabled.
   */
  ensureStarted(userId?: string): void {
    if (!isInboxDashboardEnabled()) return;
    const prevUserId = this.userId;
    if (userId) {
      this.userId = userId;
    } else if (!this.userId) {
      this.userId = stateManager.getCurrentUserId() || '';
    }
    if (this.started) {
      if (userId && prevUserId && userId !== prevUserId) {
        this.clearDebounceTimers();
        if (this.expiryTimer !== null) {
          clearTimeout(this.expiryTimer);
          this.expiryTimer = null;
        }
        this._summary = null;
        this._error = null;
        this._unavailable404 = false;
        const shared = readSharedPayload(this.userId);
        if (shared) {
          this.applySummary(shared.summary);
        } else {
          this.emitChanged();
        }
        this.requestRefresh({ immediate: true, force: true });
      }
      return;
    }
    this.started = true;
    this._unavailable404 = false;

    stateManager.addEventListener('inbox-changed', this.boundOnInboxChanged);
    stateManager.addEventListener('notification-created', this.boundOnTriggerEvent);
    stateManager.addEventListener('decision-updated', this.boundOnTriggerEvent);
    stateManager.addEventListener('chat-message-received', this.boundOnTriggerEvent);
    stateManager.addEventListener('chat-read-state-updated', this.boundOnTriggerEvent);
    stateManager.addEventListener('chat-stars-updated', this.boundOnTriggerEvent);
    stateManager.addEventListener('connected', this.boundOnTriggerEvent);

    if (typeof window !== 'undefined') {
      window.addEventListener(PENDING_SECRETS_CHANGED_EVENT, this.boundOnTriggerEvent);
      window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.addEventListener('storage', this.boundOnStorage);
    }
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.boundOnVisibilityChange);
    }

    // Adopt any cached cross-tab summary immediately while we refresh.
    if (this.userId) {
      const shared = readSharedPayload(this.userId);
      if (shared) {
        this.applySummary(shared.summary);
      }
    }

    this.schedulePoll();
    void this.fetchNow();
  }

  stop(): void {
    if (!this.started) return;
    this.started = false;
    this.clearTimers();

    stateManager.removeEventListener('inbox-changed', this.boundOnInboxChanged);
    stateManager.removeEventListener('notification-created', this.boundOnTriggerEvent);
    stateManager.removeEventListener('decision-updated', this.boundOnTriggerEvent);
    stateManager.removeEventListener('chat-message-received', this.boundOnTriggerEvent);
    stateManager.removeEventListener('chat-read-state-updated', this.boundOnTriggerEvent);
    stateManager.removeEventListener('chat-stars-updated', this.boundOnTriggerEvent);
    stateManager.removeEventListener('connected', this.boundOnTriggerEvent);

    if (typeof window !== 'undefined') {
      window.removeEventListener(PENDING_SECRETS_CHANGED_EVENT, this.boundOnTriggerEvent);
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.removeEventListener('storage', this.boundOnStorage);
    }
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.boundOnVisibilityChange);
    }
  }

  /**
   * Ensures the store is started when `/profile/inbox` mounts and refreshes
   * immediately if no summary is loaded yet. Returns a cleanup function.
   */
  mountPage(userId?: string): () => void {
    const wasStarted = this.started;
    this.ensureStarted(userId);
    if (wasStarted && !this._summary && !this.inFlight) {
      this.requestRefresh({ immediate: true });
    }
    return () => {};
  }

  /**
   * Schedules a debounced refresh (750 ms trailing / 4 s max-wait), or
   * triggers an immediate fetch when `immediate` is true.
   */
  requestRefresh(opts?: { immediate?: boolean; force?: boolean }): void {
    if (!this.started || !isInboxDashboardEnabled()) return;
    this.pendingEventAt = Date.now();

    if (opts?.immediate) {
      this.clearDebounceTimers();
      void this.fetchNow(opts.force === true);
      return;
    }

    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.clearDebounceTimers();
      void this.fetchNow();
    }, INBOX_DEBOUNCE_MS);

    if (this.maxWaitTimer === null) {
      this.maxWaitTimer = setTimeout(() => {
        this.clearDebounceTimers();
        void this.fetchNow();
      }, INBOX_MAX_WAIT_MS);
    }
  }

  /** @internal test-only reset. */
  resetForTests(): void {
    this.stop();
    resetInboxSummaryBadgeCacheForTests();
    this.userId = '';
    this._summary = null;
    this._loading = false;
    this._error = null;
    this._unavailable404 = false;
    this.inFlight = false;
    this.pendingRefetch = false;
    this.pendingEventAt = 0;
  }

  private clearDebounceTimers(): void {
    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    if (this.maxWaitTimer !== null) {
      clearTimeout(this.maxWaitTimer);
      this.maxWaitTimer = null;
    }
  }

  private clearTimers(): void {
    this.clearDebounceTimers();
    if (this.pollTimer !== null) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
    if (this.expiryTimer !== null) {
      clearTimeout(this.expiryTimer);
      this.expiryTimer = null;
    }
  }

  private pollIntervalMs(): number {
    return stateManager.isConnected ? INBOX_POLL_HEALTHY_MS : INBOX_POLL_UNHEALTHY_MS;
  }

  private schedulePoll(): void {
    if (this.pollTimer !== null) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
    if (!this.started) return;
    const delay = this.pollIntervalMs();
    this.pollTimer = setTimeout(() => {
      this.pollTimer = null;
      if (!this.started) return;
      void this.fetchNow(false, true);
      this.schedulePoll();
    }, delay);
  }

  private scheduleExpiryTimer(summary: InboxSummaryResponse): void {
    if (this.expiryTimer !== null) {
      clearTimeout(this.expiryTimer);
      this.expiryTimer = null;
    }
    const candidates = [summary.categories?.pending_secrets, summary.categories?.agent_secrets];

    let soonestMs = Number.POSITIVE_INFINITY;
    for (const cat of candidates) {
      if (!cat || cat.state !== 'ok' || !cat.nextExpiresAt) continue;
      const ts = Date.parse(cat.nextExpiresAt);
      if (!Number.isNaN(ts) && ts < soonestMs) {
        soonestMs = ts;
      }
    }
    if (!Number.isFinite(soonestMs)) return;

    const delay = Math.min(Math.max(1000, soonestMs - Date.now() + 500), MAX_EXPIRY_TIMER_MS);
    this.expiryTimer = setTimeout(() => {
      this.expiryTimer = null;
      this.requestRefresh({ immediate: true });
    }, delay);
  }

  private applySummary(summary: InboxSummaryResponse): void {
    this._summary = summary;
    this._error = null;
    this._unavailable404 = false;
    this.scheduleExpiryTimer(summary);
    this.emitChanged();
  }

  private emitChanged(): void {
    if (typeof window !== 'undefined' && typeof window.dispatchEvent === 'function') {
      window.dispatchEvent(
        new CustomEvent(INBOX_SUMMARY_CHANGED_EVENT, {
          detail: { summary: this._summary, total: this.total, totalCapped: this.totalCapped },
        })
      );
    }
  }

  private async fetchNow(force = false, isPoll = false): Promise<void> {
    if (!this.started || !isInboxDashboardEnabled()) return;
    if (this.inFlight) {
      if (!isPoll) {
        this.pendingRefetch = true;
      }
      return;
    }

    this.inFlight = true;
    const triggeredAt = isPoll ? 0 : this.pendingEventAt;
    if (!isPoll) {
      this.pendingEventAt = 0;
    }
    const isFirstLoad = this._summary === null;
    if (isFirstLoad) {
      this._loading = true;
      this.emitChanged();
    }

    try {
      await this.runWithLeaderLock(async () => {
        if (!this.started) return;
        const uid = this.userId || stateManager.getCurrentUserId() || '';
        if (uid) this.userId = uid;

        // If another tab already fetched a fresh summary, adopt it instead of
        // issuing a duplicate request:
        // - event-triggered refreshes adopt payloads fetched at/after the event;
        // - safety polls adopt payloads younger than the active poll interval.
        if (!force && uid && (triggeredAt > 0 || isPoll)) {
          const shared = readSharedPayload(uid);
          if (shared) {
            const eventFresh = triggeredAt > 0 && shared.fetchedAt >= triggeredAt;
            const age = Date.now() - shared.fetchedAt;
            const pollFresh = isPoll && age >= 0 && age < this.pollIntervalMs();
            if (eventFresh || pollFresh) {
              this.applySummary(shared.summary);
              return;
            }
          }
        }

        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), INBOX_FETCH_TIMEOUT_MS);
        const fetchedAt = Date.now();
        try {
          const res = await apiFetch(`${INBOX_SUMMARY_API}?previews=0`, {
            signal: controller.signal,
          });
          if (!this.started || (uid && this.userId && this.userId !== uid)) return;
          if (res.status === 404) {
            this._unavailable404 = true;
            this._summary = null;
            this._error = null;
            this.emitChanged();
            return;
          }
          if (!res.ok) {
            this._error = `Failed to load inbox summary (${res.status})`;
            this.emitChanged();
            return;
          }
          const data = (await res.json()) as InboxSummaryResponse;
          if (!this.started || (uid && this.userId && this.userId !== uid)) return;
          this.applySummary(data);
          if (uid) {
            writeSharedPayload({ userId: uid, fetchedAt, summary: data });
          }
        } catch (err) {
          if (!this.started || (uid && this.userId && this.userId !== uid)) return;
          this._error = err instanceof Error ? err.message : 'Failed to load inbox summary';
          this.emitChanged();
        } finally {
          clearTimeout(timeout);
        }
      });
    } finally {
      this._loading = false;
      this.inFlight = false;
      if (isFirstLoad) {
        this.emitChanged();
      }
      if (this.pendingRefetch && this.started) {
        this.pendingRefetch = false;
        void this.fetchNow();
      }
    }
  }

  private async runWithLeaderLock(fn: () => Promise<void>): Promise<void> {
    const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined;
    if (!locks || typeof locks.request !== 'function') {
      await fn();
      return;
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), INBOX_LOCK_WAIT_MS);
    try {
      await locks.request(INBOX_LOCK_NAME, { signal: controller.signal }, async () => {
        clearTimeout(timer);
        await fn();
      });
    } catch {
      clearTimeout(timer);
      // Lock wait timed out or failed; run directly so this tab is never stuck.
      await fn();
    }
  }
}

export const inboxSummaryStore = new InboxSummaryStore();
