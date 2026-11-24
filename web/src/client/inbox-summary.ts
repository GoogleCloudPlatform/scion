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
 * (design §5.3).
 *
 * - Lazily started when `web.inbox_dashboard` is enabled.
 * - Cross-tab coordination: leader election via Web Locks (`scion-inbox-summary`),
 *   broadcasting fetched summaries over `localStorage` (`scion:inbox-summary`).
 * - Debounces invalidation triggers (750 ms trailing / 4 s max-wait) so bursts
 *   of SSE events fold into a single fetch.
 * - Badge-only consumers fetch `previews=0`; while any tab has `/profile/inbox`
 *   mounted (tracked via `localStorage`), the leader fetches `previews=3`.
 * - Safety poll: 5 min while SSE is healthy, 30 s while unhealthy.
 * - Schedules an expiry timer for the soonest approval `expiresAt`.
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
export const INBOX_MOUNT_KEY = 'scion:inbox-page-mounted';

export const INBOX_DEBOUNCE_MS = 750;
export const INBOX_MAX_WAIT_MS = 4000;
export const INBOX_POLL_HEALTHY_MS = 5 * 60 * 1000;
export const INBOX_POLL_UNHEALTHY_MS = 30 * 1000;
export const INBOX_FETCH_TIMEOUT_MS = 15_000;
export const INBOX_LOCK_WAIT_MS = 10_000;
export const INBOX_PAGE_PREVIEWS = 3;

const MAX_EXPIRY_TIMER_MS = 60 * 60 * 1000; // 1 hour clamp
const MOUNT_STALE_MS = 60 * 60 * 1000;

export type InboxCategoryState = 'ok' | 'disabled' | 'unavailable' | 'error';

export type InboxCategoryKey = 'decisions' | 'waiting' | 'reviews' | 'approvals' | 'chat';

export interface InboxApprovalPreview {
  id: string;
  kind: string;
  projectId: string;
  projectName: string;
  createdAt: string;
  href: string;
  key: string;
  agentSlug: string;
  injectionMode?: string;
  replacesExisting: boolean;
  allowProgeny: boolean;
  expiresAt: string;
}

export interface InboxCategory<TPreview = unknown> {
  state: InboxCategoryState;
  count: number;
  capped: boolean;
  countsTowardTotal: boolean;
  href: string;
  errorCode?: string;
  previews: TPreview[];
  agentIds?: string[];
  breakdown?: Record<string, number>;
}

export interface InboxSummaryResponse {
  generatedAt: string;
  total: number;
  totalCapped: boolean;
  categories: {
    decisions: InboxCategory;
    waiting: InboxCategory;
    reviews: InboxCategory;
    approvals: InboxCategory<InboxApprovalPreview>;
    chat: InboxCategory;
  };
}

export interface InboxHintEvent {
  category?: string;
  agentId?: string;
  activity?: string;
}

interface SharedInboxPayload {
  userId: string;
  previews: number;
  fetchedAt: number;
  summary: InboxSummaryResponse;
}

interface MountMap {
  [tabId: string]: number;
}

function createTabId(): string {
  return `tab-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

function readMountMap(): MountMap {
  try {
    const raw = localStorage.getItem(INBOX_MOUNT_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const now = Date.now();
    const out: MountMap = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === 'number' && now - v < MOUNT_STALE_MS) {
        out[k] = v;
      }
    }
    return out;
  } catch {
    return {};
  }
}

function writeMountMap(map: MountMap): void {
  try {
    if (Object.keys(map).length === 0) {
      localStorage.removeItem(INBOX_MOUNT_KEY);
    } else {
      localStorage.setItem(INBOX_MOUNT_KEY, JSON.stringify(map));
    }
  } catch {
    // Ignore storage quota/availability errors.
  }
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
      typeof parsed.previews !== 'number' ||
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
  private readonly tabId = createTabId();
  private started = false;
  private userId = '';
  private pageMountCount = 0;
  private _summary: InboxSummaryResponse | null = null;
  private _summaryPreviews = 0;
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
        this.applySummary(shared.summary, shared.previews);
      }
    } else if (
      e.key === INBOX_MOUNT_KEY &&
      this.hasAnyMountedPage() &&
      this._summaryPreviews < INBOX_PAGE_PREVIEWS
    ) {
      this.requestRefresh({ immediate: true });
    }
  };

  private boundOnFeatureFlagsChanged = (): void => {
    if (!isInboxDashboardEnabled()) {
      this.stop();
    }
  };

  private boundOnUnload = (): void => {
    if (this.pageMountCount > 0) {
      const map = readMountMap();
      delete map[this.tabId];
      writeMountMap(map);
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
        this._summaryPreviews = 0;
        this._error = null;
        this._unavailable404 = false;
        const shared = readSharedPayload(this.userId);
        if (shared) {
          this.applySummary(shared.summary, shared.previews);
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
    stateManager.addEventListener('connected', this.boundOnTriggerEvent);

    if (typeof window !== 'undefined') {
      window.addEventListener(PENDING_SECRETS_CHANGED_EVENT, this.boundOnTriggerEvent);
      window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.addEventListener('storage', this.boundOnStorage);
      window.addEventListener('beforeunload', this.boundOnUnload);
    }
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.boundOnVisibilityChange);
    }

    // Adopt any cached cross-tab summary immediately while we refresh.
    if (this.userId) {
      const shared = readSharedPayload(this.userId);
      if (shared) {
        this.applySummary(shared.summary, shared.previews);
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
    stateManager.removeEventListener('connected', this.boundOnTriggerEvent);

    if (typeof window !== 'undefined') {
      window.removeEventListener(PENDING_SECRETS_CHANGED_EVENT, this.boundOnTriggerEvent);
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.removeEventListener('storage', this.boundOnStorage);
      window.removeEventListener('beforeunload', this.boundOnUnload);
    }
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.boundOnVisibilityChange);
    }
  }

  /**
   * Registers an active `/profile/inbox` page mount so the leader fetches
   * previews (`previews=3`) instead of counts-only (`previews=0`).
   * Returns a cleanup function to call on unmount.
   */
  mountPage(userId?: string): () => void {
    this.pageMountCount += 1;
    const map = readMountMap();
    map[this.tabId] = Date.now();
    writeMountMap(map);

    this.ensureStarted(userId);
    if (!this._summary || this._summaryPreviews < INBOX_PAGE_PREVIEWS) {
      this.requestRefresh({ immediate: true });
    }

    let cleaned = false;
    return () => {
      if (cleaned) return;
      cleaned = true;
      this.pageMountCount = Math.max(0, this.pageMountCount - 1);
      if (this.pageMountCount === 0) {
        const current = readMountMap();
        delete current[this.tabId];
        writeMountMap(current);
      }
    };
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

  /**
   * Optimistically removes an approval preview and decrements the approvals
   * count/total immediately after an inline approve/reject, then schedules a
   * server refetch to reconcile.
   */
  optimisticRemoveApproval(id: string): void {
    if (!this._summary) {
      this.requestRefresh({ immediate: true });
      return;
    }
    const approvals = this._summary.categories.approvals;
    if (approvals.state !== 'ok') {
      this.requestRefresh({ immediate: true });
      return;
    }
    const nextPreviews = approvals.previews.filter((p) => p.id !== id);
    const nextCount = Math.max(0, approvals.count - 1);
    const delta = approvals.count - nextCount;
    const nextSummary: InboxSummaryResponse = {
      ...this._summary,
      total: Math.max(0, this._summary.total - delta),
      categories: {
        ...this._summary.categories,
        approvals: {
          ...approvals,
          count: nextCount,
          previews: nextPreviews,
        },
      },
    };
    this.applySummary(nextSummary, this._summaryPreviews);
    if (this.userId) {
      writeSharedPayload({
        userId: this.userId,
        previews: this._summaryPreviews,
        fetchedAt: Date.now(),
        summary: nextSummary,
      });
    }
    this.requestRefresh({ immediate: true, force: true });
  }

  /** @internal test-only reset. */
  resetForTests(): void {
    this.stop();
    resetInboxSummaryBadgeCacheForTests();
    this.userId = '';
    this.pageMountCount = 0;
    this._summary = null;
    this._summaryPreviews = 0;
    this._loading = false;
    this._error = null;
    this._unavailable404 = false;
    this.inFlight = false;
    this.pendingRefetch = false;
    this.pendingEventAt = 0;
  }

  private hasAnyMountedPage(): boolean {
    if (this.pageMountCount > 0) return true;
    return Object.keys(readMountMap()).length > 0;
  }

  private desiredPreviews(): number {
    return this.hasAnyMountedPage() ? INBOX_PAGE_PREVIEWS : 0;
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

  private schedulePoll(): void {
    if (this.pollTimer !== null) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
    if (!this.started) return;
    const healthy = stateManager.isConnected;
    const delay = healthy ? INBOX_POLL_HEALTHY_MS : INBOX_POLL_UNHEALTHY_MS;
    this.pollTimer = setTimeout(() => {
      this.pollTimer = null;
      if (!this.started) return;
      void this.fetchNow();
      this.schedulePoll();
    }, delay);
  }

  private scheduleExpiryTimer(summary: InboxSummaryResponse): void {
    if (this.expiryTimer !== null) {
      clearTimeout(this.expiryTimer);
      this.expiryTimer = null;
    }
    const approvals = summary.categories?.approvals;
    if (!approvals || approvals.state !== 'ok' || approvals.previews.length === 0) return;

    let soonestMs = Number.POSITIVE_INFINITY;
    for (const p of approvals.previews) {
      const ts = Date.parse(p.expiresAt);
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

  private applySummary(summary: InboxSummaryResponse, previews: number): void {
    this._summary = summary;
    this._summaryPreviews = previews;
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

  private async fetchNow(force = false): Promise<void> {
    if (!this.started || !isInboxDashboardEnabled()) return;
    if (this.inFlight) {
      this.pendingRefetch = true;
      return;
    }

    this.inFlight = true;
    const triggeredAt = this.pendingEventAt;
    const previews = this.desiredPreviews();
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

        // If another tab just fetched a fresh summary with enough previews after
        // our triggering event, adopt it instead of issuing a duplicate request.
        if (!force && uid && triggeredAt > 0) {
          const shared = readSharedPayload(uid);
          if (shared && shared.fetchedAt >= triggeredAt && shared.previews >= previews) {
            this.applySummary(shared.summary, shared.previews);
            return;
          }
        }

        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), INBOX_FETCH_TIMEOUT_MS);
        const fetchedAt = Date.now();
        try {
          const res = await apiFetch(`${INBOX_SUMMARY_API}?previews=${previews}`, {
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
          this.applySummary(data, previews);
          if (uid) {
            writeSharedPayload({ userId: uid, previews, fetchedAt, summary: data });
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
