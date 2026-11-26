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
 * Notification Tray Component
 *
 * Self-contained bell icon + popover panel that polls for unacknowledged
 * notifications and lets the user acknowledge them individually or in bulk.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';
import { getBrandName, getNotificationIconUrl } from '../../client/branding.js';
import { stateManager } from '../../client/state.js';
import { dispatchTrayCount } from '../../client/tray-count-events.js';
import {
  canShowPushNotification,
  enablePushWithPermission,
  pushPermission,
  setPushOptIn,
  PUSH_PREFERENCE_EVENT,
  type PushPermissionState,
} from '../../client/push-preference.js';
import type { User, Notification } from '../../shared/types.js';
import { formatRelative } from '../../utils/time.js';
import {
  FEATURE_FLAGS_CHANGED_EVENT,
  decisionsEnabled,
  isInboxDashboardEnabled,
} from '../../utils/feature-flags.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  ensureInboxSummaryStarted,
  formatInboxSummaryBadge,
  getInboxSummaryTotal,
} from '../../client/inbox-summary-loader.js';
import { navigateTo } from '../../client/navigation.js';
import { coalescedRefresh, type CoalescedRefresh } from '../../client/coalesced-refresh.js';
import { visibleInterval } from '../../client/visible-interval.js';

/**
 * A scheduled dispatch blocked by an agent row in phase error. The row's
 * agentId is the errored agent, so "View agent" leads to the delete that
 * unblocks the schedule.
 */
export const SCHEDULE_BLOCKED_STATUS = 'SCHEDULE_BLOCKED';

/**
 * A review of one of the user's artifacts was finalized by another principal
 * allowed to review it (ptone/scion#6853). The row's `excerpt` is the start of the
 * reviewer's summary note, attributed by `message`; its artifact comes from
 * `resourceId` and `projectId` only.
 */
export const ARTIFACT_REVIEW_SUMMARY_KIND = 'artifact_review_summary';
export const ARTIFACT_REVIEW_SUBMITTED_STATUS = 'ARTIFACT_REVIEW_SUBMITTED';

/**
 * Artifact review-request lifecycle notifications (ptone/scion#9296, #9306):
 * a reviewer is asked to review an artifact, or a reviewer dismisses a request
 * with an optional note shown as the excerpt.
 */
export const ARTIFACT_REVIEW_REQUEST_KIND = 'artifact_review_request';
export const ARTIFACT_REVIEW_REQUESTED_STATUS = 'ARTIFACT_REVIEW_REQUESTED';
export const ARTIFACT_REVIEW_DISMISSED_STATUS = 'ARTIFACT_REVIEW_DISMISSED';

/**
 * An agent saved a user secret for the user; it waits for the user's
 * approval in Profile > Secrets. The row carries text only, never a value.
 */
export const SECRET_PENDING_KIND = 'secret_pending';
export const SECRET_PENDING_STATUS = 'SECRET_PENDING';

/** The path where the user decides on pending secrets. */
const PROFILE_SECRETS_PATH = '/profile/secrets';

/** Dispatched on `window` when another surface (e.g. Inbox) acks a notification. */
export const NOTIFICATION_ACKED_EVENT = 'scion:notification-acked';

/** The Profile > Secrets path for a pending secret row, or null for any other row. */
export function pendingSecretLink(n: Notification): string | null {
  return n.kind === SECRET_PENDING_KIND || n.status === SECRET_PENDING_STATUS
    ? PROFILE_SECRETS_PATH
    : null;
}

/**
 * Coalesced cross-project Decisions notification (ptone/scion#7997). At most
 * one unacknowledged row exists per user; it stays pinned at the top of the
 * tray and links to Profile > Decisions (Waiting on you).
 */
export const DECISIONS_WAITING_KIND = 'decisions_waiting';
export const DECISIONS_WAITING_STATUS = 'DECISIONS_WAITING';
export const PROFILE_DECISIONS_PENDING_PATH = '/profile/decisions?tab=pending';

/** Whether a notification row is the coalesced Decisions bell item. */
export function isDecisionsWaiting(n: Pick<Notification, 'kind' | 'status'>): boolean {
  return n.kind === DECISIONS_WAITING_KIND || n.status === DECISIONS_WAITING_STATUS;
}

/** The Profile > Decisions path for a decisions_waiting row, or null for any other row. */
export function decisionsWaitingLink(n: Notification): string | null {
  return isDecisionsWaiting(n) ? PROFILE_DECISIONS_PENDING_PATH : null;
}

function parseDecisionsCount(n: Pick<Notification, 'resourceId'>): number {
  const parsed = Number.parseInt(n.resourceId ?? '', 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 1;
}

function pinDecisionsWaitingFirst(list: Notification[]): Notification[] {
  const pinned = list.filter((n) => isDecisionsWaiting(n));
  if (pinned.length === 0) return list;
  const rest = list.filter((n) => !isDecisionsWaiting(n));
  return [...pinned, ...rest];
}

/**
 * Statuses of the chat rows the hub used to write into the bell (mentions
 * and DMs). It writes none now, but rows made before that can remain.
 */
const RETIRED_CHAT_STATUSES: ReadonlySet<string> = new Set(['MENTION', 'DM_RECEIVED']);

/** The nil UUID some older notification rows carry in place of an agent. */
const NIL_UUID = '00000000-0000-0000-0000-000000000000';

/** Whether a notification row names an agent the "View agent" link can open. */
function hasAgentLink(agentId: string | undefined): boolean {
  return !!agentId && agentId !== NIL_UUID;
}

/** A lowercase canonical UUID, as the hub writes ids. */
const CANONICAL_UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Line breaks the excerpt shows as a space. */
const EXCERPT_BREAKS = /[\t\n\v\f\r\u0085\u2028\u2029]+/g;
/** Control and format characters, removed from the excerpt. */
const EXCERPT_UNSAFE = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

function isArtifactExcerptKind(kind: string | undefined): boolean {
  return kind === ARTIFACT_REVIEW_SUMMARY_KIND || kind === ARTIFACT_REVIEW_REQUEST_KIND;
}

/**
 * The excerpt of a review summary or review-request row, or '' for any other
 * row: no other kind shows one. The hub already cleans it; line breaks become
 * spaces, control and format characters are removed and runs of spaces collapse
 * here again, so a row from anywhere else still shows as one line of plain
 * text.
 */
export function reviewSummaryExcerpt(n: Notification): string {
  if (!isArtifactExcerptKind(n.kind) || typeof n.excerpt !== 'string') return '';
  return n.excerpt
    .replace(EXCERPT_BREAKS, ' ')
    .replace(EXCERPT_UNSAFE, '')
    .replace(/ {2,}/g, ' ')
    .trim();
}

/**
 * The artifact page path of a review summary or review-request row, built from
 * its project and resource fields alone (both canonical UUIDs, the project not
 * the nil UUID), or null. The message and excerpt are never read. It is the path
 * artifactPagePath (client/artifacts.ts) builds, written out here so the
 * tray does not load the artifacts client. An artifact not homed in a
 * project (the hub writes the nil UUID) deliberately gets no link; the CLI
 * still prints its scion://artifact/ reference.
 */
export function reviewSummaryLink(n: Notification): string | null {
  if (!isArtifactExcerptKind(n.kind) || n.resourceType !== 'artifact') return null;
  const artifactId = n.resourceId ?? '';
  const projectId = n.projectId ?? '';
  if (
    !CANONICAL_UUID.test(artifactId) ||
    !CANONICAL_UUID.test(projectId) ||
    projectId === NIL_UUID
  ) {
    return null;
  }
  return `/projects/${projectId}/artifacts/${artifactId}`;
}

/**
 * The notification a `notification-created` event carries in full, or null.
 *
 * The user-scoped `user.{id}.notification` subject delivers the row itself
 * (UserNotificationEvent): every field the tray renders (id, status,
 * message, createdAt, agentId). Missing or malformed payloads yield null
 * and fall back to fetching.
 *
 * The payload has no `acknowledged` field: a notification that was just
 * created is unacknowledged by definition, so a missing field passes and
 * only an explicit `true` is rejected. A payload addressed to another user
 * (`subscriberId`) is rejected too, as a second gate behind the subject.
 */
export function notificationFromEvent(data: unknown, userId: string | null): Notification | null {
  if (!userId || !data || typeof data !== 'object') return null;
  const d = data as Record<string, unknown>;
  const str = (v: unknown): v is string => typeof v === 'string';
  if (!str(d.id) || !d.id || !str(d.status) || !d.status || !str(d.message)) return null;
  if (!str(d.createdAt) || Number.isNaN(Date.parse(d.createdAt))) return null;
  if (!str(d.agentId)) return null;
  if (d.acknowledged !== undefined && d.acknowledged !== false) return null;
  if (d.subscriberId !== undefined && d.subscriberId !== userId) return null;
  const optional: Partial<Notification> = {};
  if (str(d.kind)) optional.kind = d.kind;
  if (str(d.resourceType)) optional.resourceType = d.resourceType;
  if (str(d.resourceId)) optional.resourceId = d.resourceId;
  if (str(d.excerpt)) optional.excerpt = d.excerpt;
  return {
    ...optional,
    id: d.id,
    subscriptionId: str(d.subscriptionId) ? d.subscriptionId : '',
    agentId: d.agentId,
    projectId: str(d.projectId) ? d.projectId : '',
    subscriberType: 'user',
    subscriberId: userId,
    status: d.status,
    message: d.message,
    dispatched: false,
    acknowledged: false,
    createdAt: d.createdAt,
  };
}

/**
 * Extracts a user-scoped SSE update whose `acknowledged` flag is `true`
 * (e.g. `decisions_waiting` clearing when `waitingOnYou` reaches 0), or null.
 * Kept separate from `notificationFromEvent` so callers expecting only
 * unacknowledged rows from `notificationFromEvent` see unchanged behavior.
 */
export function acknowledgedNotificationFromEvent(
  data: unknown,
  userId: string | null
): { id: string; isDecisions: boolean } | null {
  if (!userId || !data || typeof data !== 'object') return null;
  const d = data as Record<string, unknown>;
  if (d.acknowledged !== true) return null;
  if (typeof d.id !== 'string' || !d.id) return null;
  if (d.subscriberId !== undefined && d.subscriberId !== userId) return null;
  const isDecisions = d.kind === DECISIONS_WAITING_KIND || d.status === DECISIONS_WAITING_STATUS;
  return { id: d.id, isDecisions };
}

const POLL_INTERVAL_MS = 5 * 60_000; // 5 minutes — fallback only; SSE delivers in real-time
const UNHEALTHY_POLL_INTERVAL_MS = 30_000; // 30 seconds while SSE is disconnected
const FOCUS_REFETCH_COOLDOWN_MS = 5_000; // avoid double-fetching when visibilitychange + focus fire together

@customElement('scion-notification-tray')
export class ScionNotificationTray extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @state() private notifications: Notification[] = [];
  @state() private open = false;

  /** Mirrors the shared push preference so the panel can render its state. */
  @state() private pushEnabled = false;
  @state() private pushPermission: PushPermissionState = 'default';

  /** Mirrors `web.inbox_dashboard` flag and `InboxSummaryStore` total/badge (design §6.1). */
  @state() private inboxEnabled = false;
  @state() private inboxTotal = 0;
  @state() private inboxBadge: string | null = null;

  /** Stops the fallback poll; paused while the tab is hidden. */
  private stopPollTimer: (() => void) | null = null;
  /** Whether the shared SSE stream has reported a drop without yet reconnecting. */
  private sseUnhealthy = false;
  /** Wall-clock timestamp of the most recent list fetch, for focus-refetch coalescing. */
  private lastFetchAt = 0;
  /** The user id loading was started for; null when nothing is running. */
  private activeUserId: string | null = null;
  private boundOnClickOutside = this.onClickOutside.bind(this);
  private boundOnNotification = this.onNotificationEvent.bind(this);
  private boundOnSseDisconnected = (): void => this.onSseDisconnected();
  private boundOnSseConnected = (): void => this.onSseConnected();
  private boundOnVisibilityChange = (): void => this.onVisibilityChange();
  private boundOnWindowFocus = (): void => this.onWindowFocus();
  private boundOnPushPreference = (): void => this.syncPushState();
  private boundOnFeatureFlagsChanged = (): void => this.onFeatureFlagsChanged();
  private boundOnInboxSummaryChanged = (): void => this.syncInboxState();
  private boundOnInboxChanged = (e: Event): void => this.onInboxChanged(e);
  private boundOnNotificationAcked = (e: Event): void => this.onNotificationAcked(e);

  /** IDs already seen — used to detect genuinely new notifications. */
  private seenIds = new Set<string>();

  /** Last observed count for decisions_waiting; governs browser push on increases. */
  private lastDecisionsCount = 0;

  /** Mirrors decisionsEnabled() so async flag loading at boot triggers reconcile + refresh. */
  private decisionsFlagEnabled = false;

  /** Suppresses browser push for the initial fetch so existing notifications don't fire. */
  private initialFetchDone = false;

  /** The user id that the list, seenIds and initialFetchDone belong to. */
  private stateUserId: string | null = null;

  /** Counts list requests sent; each request knows its own number. */
  private fetchSeq = 0;

  /**
   * The number of the newest list request whose response was applied. A
   * response to an older request, landing after it, is stale and ignored.
   */
  private appliedSeq = 0;

  /**
   * Rows an event added or updated in place, with the fetchSeq when the event
   * arrived. A list request sent at or before that number may have been
   * answered before the row existed (or with an older in-place version of a
   * deterministic row such as decisions_waiting), so its response must neither
   * drop nor overwrite the row.
   */
  private upsertedAtSeq = new Map<string, number>();

  /**
   * Rows acknowledged since the latest list request started, with the fetchSeq
   * at the time of acknowledgement. Prevents an already-in-flight list request
   * from resurrecting a row that SSE or user ack dismissed while it ran.
   */
  private ackedAtSeq = new Map<string, number>();

  /**
   * Fetches for events that carry no usable notification: the first of a
   * burst fetches at once, the rest collapse into one trailing fetch.
   */
  private eventFetch: CoalescedRefresh = coalescedRefresh(() => this.fetchNotifications(), {
    waitMs: 500,
    maxWaitMs: 3000,
    leading: true,
  });

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    this.decisionsFlagEnabled = decisionsEnabled();
    this.inboxEnabled = isInboxDashboardEnabled();
    this.syncPushState();
    // Keep in step with the profile settings page, which writes the same
    // preference. Two toggles disagreeing about one setting is worse than one.
    window.addEventListener(PUSH_PREFERENCE_EVENT, this.boundOnPushPreference);
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnInboxSummaryChanged);
    window.addEventListener(NOTIFICATION_ACKED_EVENT, this.boundOnNotificationAcked);
    this.syncUser();
    this.syncInboxState();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopPolling();
    this.stopListeningForNotifications();
    // A reconnect starts afresh, as a first mount does.
    this.activeUserId = null;
    window.removeEventListener(PUSH_PREFERENCE_EVENT, this.boundOnPushPreference);
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    window.removeEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnInboxSummaryChanged);
    window.removeEventListener(NOTIFICATION_ACKED_EVENT, this.boundOnNotificationAcked);
    document.removeEventListener('click', this.boundOnClickOutside, true);
  }

  override willUpdate(changed: Map<string, unknown>): void {
    // Clear the previous user's state before this render, so no render pairs
    // the new user with the previous user's list.
    if (changed.has('user')) this.resetOnUserChange();
    if (changed.has('notifications')) this.announceCount();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('user')) this.syncUser();
    this.detectTruncation();
  }

  /**
   * Starts or stops loading for the signed-in user.
   *
   * Keyed on the user id, not the object: connectedCallback and the first
   * updated() both see the same user on mount, and the header can hand over a
   * new object for the same user (an auth refresh). Neither is a reason to
   * fetch again or to restart polling.
   */
  private syncUser(): void {
    const id = this.user?.id ?? null;
    if (id) {
      if (id === this.activeUserId) return;
      // Loading stops on disconnect and restarts in connectedCallback.
      if (!this.isConnected) return;
      this.activeUserId = id;
      this.decisionsFlagEnabled = decisionsEnabled();
      this.inboxEnabled = isInboxDashboardEnabled();
      void this.fetchNotifications();
      this.reconcileDecisionsBell();
      if (this.inboxEnabled) {
        ensureInboxSummaryStarted(id);
        this.syncInboxState();
      }
      this.startPolling();
      this.listenForNotifications();
    } else {
      this.activeUserId = null;
      this.stopPolling();
      this.stopListeningForNotifications();
      this.notifications = [];
      this.inboxTotal = 0;
      this.inboxBadge = null;
    }
  }

  private onFeatureFlagsChanged(): void {
    const enabled = decisionsEnabled();
    if (enabled !== this.decisionsFlagEnabled) {
      this.decisionsFlagEnabled = enabled;
      if (this.activeUserId) {
        if (enabled) {
          this.reconcileDecisionsBell();
          void this.fetchNotifications();
        } else {
          this.lastDecisionsCount = 0;
          this.notifications = this.notifications.filter((n) => !isDecisionsWaiting(n));
        }
      }
    }

    const nextInbox = isInboxDashboardEnabled();
    if (nextInbox !== this.inboxEnabled) {
      this.inboxEnabled = nextInbox;
      if (nextInbox) {
        if (this.activeUserId) {
          ensureInboxSummaryStarted(this.activeUserId);
        }
        this.syncInboxState();
      } else {
        this.inboxTotal = 0;
        this.inboxBadge = null;
      }
    }
  }

  private syncInboxState(): void {
    if (!this.inboxEnabled) {
      this.inboxTotal = 0;
      this.inboxBadge = null;
      return;
    }
    this.inboxTotal = getInboxSummaryTotal();
    this.inboxBadge = formatInboxSummaryBadge();
  }

  /**
   * Fires a zero-row pending decisions read (B6) so the hub reconciles the
   * caller's coalesced `decisions_waiting` bell row asynchronously after
   * responding; any healed row arrives via `user.{id}.notification`.
   */
  private reconcileDecisionsBell(): void {
    if (!decisionsEnabled() || !this.user?.id) return;
    void apiFetch('/api/v1/users/me/decisions?tab=pending&limit=0').catch(() => {});
  }

  // ---------------------------------------------------------------------------
  // Per-user state
  // ---------------------------------------------------------------------------

  /**
   * Resets per-user state when the signed-in user id changes, so the next
   * user's first fetch behaves like a first load: it shows only that user's
   * notifications, suppresses push for the ones that already exist, and does
   * not treat the previous user's notifications as seen.
   */
  private resetOnUserChange(): void {
    const id = this.user?.id ?? null;
    if (id === this.stateUserId) return;
    this.stateUserId = id;
    this.notifications = [];
    this.seenIds = new Set();
    this.lastDecisionsCount = 0;
    this.upsertedAtSeq.clear();
    this.ackedAtSeq.clear();
    this.initialFetchDone = false;
    // The next user's first fetch answers any event the previous user's
    // coalescer held, and their first event fetches at once.
    this.eventFetch.cancel();
  }

  /**
   * Tells the header how many items the list now holds. Called for every
   * change to the list, including a clear on a user change or sign-out; a
   * dropped response for a previous user changes nothing, so it announces
   * nothing.
   */
  private announceCount(): void {
    dispatchTrayCount(this, 'notifications', this.notifications.length);
  }

  /**
   * Whether a response to a request started while requestUserId was signed in
   * may be applied. A response for a previous user, or one that arrives after
   * sign-out, is dropped.
   */
  private isForCurrentUser(requestUserId: string | null): boolean {
    return requestUserId !== null && requestUserId === (this.user?.id ?? null);
  }

  // ---------------------------------------------------------------------------
  // Push preference
  // ---------------------------------------------------------------------------

  private syncPushState(): void {
    this.pushPermission = pushPermission();
    // Permission can be revoked in site settings long after the opt-in was
    // stored, so the toggle reflects both halves rather than the flag alone.
    this.pushEnabled = canShowPushNotification('agent');
  }

  /**
   * Turns desktop notifications for agent events on or off. Chat message
   * alerts have their own toggle in profile settings.
   *
   * This is the only path that asks the browser for permission, and it is
   * reachable from chat because the tray is in the chat header. Permission is
   * never requested on load: browsers penalise unprompted requests, and a
   * permission dialog nobody asked for is the reason they do.
   */
  private async handlePushToggle(): Promise<void> {
    if (this.pushEnabled) {
      setPushOptIn('agent', false);
    } else {
      await enablePushWithPermission('agent');
    }
    this.syncPushState();
  }

  private detectTruncation(): void {
    const messages = this.shadowRoot?.querySelectorAll('.notif-message');
    if (!messages) return;
    messages.forEach((el) => {
      const badge = el.parentElement?.querySelector('.truncation-badge') as HTMLElement | null;
      if (!badge) return;
      badge.style.display = el.scrollHeight > el.clientHeight ? 'inline-flex' : 'none';
    });
  }

  // ---------------------------------------------------------------------------
  // Polling
  // ---------------------------------------------------------------------------

  private startPolling(): void {
    this.stopPolling();
    const intervalMs = this.sseUnhealthy ? UNHEALTHY_POLL_INTERVAL_MS : POLL_INTERVAL_MS;
    this.stopPollTimer = visibleInterval(() => void this.fetchNotifications(), intervalMs, {
      catchUpOnVisible: false,
    });
    document.addEventListener('visibilitychange', this.boundOnVisibilityChange);
    window.addEventListener('focus', this.boundOnWindowFocus);
  }

  private stopPolling(): void {
    this.stopPollTimer?.();
    this.stopPollTimer = null;
    document.removeEventListener('visibilitychange', this.boundOnVisibilityChange);
    window.removeEventListener('focus', this.boundOnWindowFocus);
  }

  private onVisibilityChange(): void {
    if (document.hidden || !this.activeUserId) return;
    if (Date.now() - this.lastFetchAt < FOCUS_REFETCH_COOLDOWN_MS) return;
    void this.fetchNotifications();
  }

  private onWindowFocus(): void {
    if (document.hidden || !this.activeUserId) return;
    if (Date.now() - this.lastFetchAt < FOCUS_REFETCH_COOLDOWN_MS) return;
    void this.fetchNotifications();
  }

  // ---------------------------------------------------------------------------
  // Real-time delivery (via shared stateManager SSE connection)
  // ---------------------------------------------------------------------------

  private listenForNotifications(): void {
    stateManager.addEventListener('notification-created', this.boundOnNotification);
    stateManager.addEventListener('inbox-changed', this.boundOnInboxChanged);
    stateManager.addEventListener('disconnected', this.boundOnSseDisconnected);
    stateManager.addEventListener('connected', this.boundOnSseConnected);
  }

  private stopListeningForNotifications(): void {
    stateManager.removeEventListener('notification-created', this.boundOnNotification);
    stateManager.removeEventListener('inbox-changed', this.boundOnInboxChanged);
    stateManager.removeEventListener('disconnected', this.boundOnSseDisconnected);
    stateManager.removeEventListener('connected', this.boundOnSseConnected);
    this.sseUnhealthy = false;
    this.eventFetch.cancel();
  }

  private onInboxChanged(e: Event): void {
    const detail = (e as CustomEvent<{ category?: string }> | undefined)?.detail;
    if (detail?.category === 'notifications') {
      this.eventFetch.schedule(e.timeStamp > 0 ? e.timeStamp : undefined);
    }
  }

  private onNotificationAcked(e: Event): void {
    const id = (e as CustomEvent<{ id?: string }> | undefined)?.detail?.id;
    if (!id) return;
    this.upsertedAtSeq.delete(id);
    this.ackedAtSeq.set(id, this.fetchSeq);
    this.seenIds.delete(id);
    if (this.notifications.some((x) => x.id === id)) {
      this.notifications = this.notifications.filter((x) => x.id !== id);
    }
  }

  private onSseDisconnected(): void {
    if (!this.activeUserId) return;
    if (!this.sseUnhealthy) {
      this.sseUnhealthy = true;
      this.startPolling();
    }
  }

  private onSseConnected(): void {
    if (!this.activeUserId || !this.sseUnhealthy) return;
    this.sseUnhealthy = false;
    this.startPolling();
    void this.fetchNotifications();
    this.reconcileDecisionsBell();
  }

  private onNotificationEvent(e: Event): void {
    // The stateManager fires notification-created when a notification SSE
    // event arrives. A user-scoped event carries the whole row: show it
    // without a request. Anything else is fetched, coalesced. Before the
    // first fetch has finished there is no list to add to (and no seen set
    // to tell a new row by), so that fetch is joined by fetching as well.
    const detail = (e as CustomEvent<{ data?: unknown } | undefined>).detail;
    const userId = this.user?.id ?? null;
    if (this.initialFetchDone) {
      const ack = acknowledgedNotificationFromEvent(detail?.data, userId);
      if (ack) {
        if (ack.isDecisions) {
          this.lastDecisionsCount = 0;
        }
        this.upsertedAtSeq.delete(ack.id);
        this.ackedAtSeq.set(ack.id, this.fetchSeq);
        this.seenIds.delete(ack.id);
        if (this.notifications.some((x) => x.id === ack.id)) {
          this.notifications = this.notifications.filter((x) => x.id !== ack.id);
        }
        return;
      }
    }
    const n = this.initialFetchDone ? notificationFromEvent(detail?.data, userId) : null;
    if (n) {
      this.upsertNotification(n);
      return;
    }
    this.eventFetch.schedule(e.timeStamp > 0 ? e.timeStamp : undefined);
  }

  /** Adds a notification delivered by an event, or replaces its row. */
  private upsertNotification(n: Notification): void {
    // Chat rows no longer belong in the bell (see fetchNotifications).
    if (RETIRED_CHAT_STATUSES.has(n.status)) return;
    if (isDecisionsWaiting(n)) {
      if (!decisionsEnabled()) return;
      const nextCount = parseDecisionsCount(n);
      const currentlyShown = this.notifications.some((x) => x.id === n.id);
      if (!currentlyShown || nextCount > this.lastDecisionsCount) {
        this.dispatchBrowserNotification(n);
      }
      this.lastDecisionsCount = nextCount;
      this.seenIds.add(n.id);
    } else if (!this.seenIds.has(n.id)) {
      this.dispatchBrowserNotification(n);
      this.seenIds.add(n.id);
    }
    this.ackedAtSeq.delete(n.id);
    this.upsertedAtSeq.set(n.id, this.fetchSeq);
    // The list is newest first, with decisions_waiting pinned at index 0.
    const at = this.notifications.findIndex((x) => x.id === n.id);
    const updated =
      at < 0 ? [n, ...this.notifications] : this.notifications.map((x, i) => (i === at ? n : x));
    this.notifications = pinDecisionsWaitingFirst(updated);
  }

  // ---------------------------------------------------------------------------
  // API
  // ---------------------------------------------------------------------------

  private async fetchNotifications(): Promise<void> {
    const requestUserId = this.user?.id ?? null;
    const seq = ++this.fetchSeq;
    this.lastFetchAt = Date.now();
    try {
      const res = await apiFetch('/api/v1/notifications?acknowledged=false');
      if (!res.ok) return;
      const data = (await res.json()) as Notification[] | null;
      if (!this.isForCurrentUser(requestUserId)) return;
      // A newer request's answer is already shown: this one is older.
      if (seq < this.appliedSeq) return;
      this.appliedSeq = seq;
      const decisionsOn = decisionsEnabled();
      // Older chat rows can still be unacknowledged; chat no longer belongs
      // in the bell, so they are neither listed, counted nor popped.
      const incoming = (data ?? []).filter(
        (n) => !RETIRED_CHAT_STATUSES.has(n.status) && (decisionsOn || !isDecisionsWaiting(n))
      );

      // Rows added, updated in place (such as decisions_waiting), or
      // acknowledged after this request was sent are newer than its answer:
      // keep the live event version and ignore any stale copy in incoming.
      const newerInPlaceIds = new Set<string>();
      for (const [id, at] of this.upsertedAtSeq) {
        if (at >= seq) {
          newerInPlaceIds.add(id);
        } else {
          this.upsertedAtSeq.delete(id);
        }
      }
      const newerAckedIds = new Set<string>();
      for (const [id, at] of this.ackedAtSeq) {
        if (at >= seq) {
          newerAckedIds.add(id);
        } else {
          this.ackedAtSeq.delete(id);
        }
      }
      const freshIncoming = incoming.filter(
        (n) => !newerInPlaceIds.has(n.id) && !newerAckedIds.has(n.id)
      );

      // Detect new notifications (IDs not previously seen, or decisions_waiting
      // appearing / increasing) and dispatch browser push — but only after the
      // first fetch so we don't blast existing unacknowledged rows on page load.
      if (this.initialFetchDone) {
        for (const n of freshIncoming) {
          if (isDecisionsWaiting(n)) {
            const nextCount = parseDecisionsCount(n);
            const currentlyShown = this.notifications.some((x) => x.id === n.id);
            if (!currentlyShown || nextCount > this.lastDecisionsCount) {
              this.dispatchBrowserNotification(n);
            }
          } else if (!this.seenIds.has(n.id)) {
            this.dispatchBrowserNotification(n);
          }
        }
      }

      const freshIncomingIds = new Set(freshIncoming.map((n) => n.id));
      const kept = this.notifications.filter(
        (n) =>
          !freshIncomingIds.has(n.id) &&
          newerInPlaceIds.has(n.id) &&
          (decisionsOn || !isDecisionsWaiting(n))
      );
      const next = pinDecisionsWaitingFirst([...kept, ...freshIncoming]);
      const decisionsRow = next.find((n) => isDecisionsWaiting(n));
      this.lastDecisionsCount = decisionsRow ? parseDecisionsCount(decisionsRow) : 0;

      // Update seen set to current snapshot
      this.seenIds = new Set(next.map((n) => n.id));
      this.initialFetchDone = true;
      this.notifications = next;
    } catch {
      // Silently ignore network errors during polling
    }
  }

  /**
   * Fires a browser Notification if the user has opted in via profile settings
   * and the browser has granted permission.
   */
  private dispatchBrowserNotification(n: Notification): void {
    if (!canShowPushNotification('agent')) {
      return;
    }

    const decisions = isDecisionsWaiting(n);
    const title = decisions ? 'Decisions waiting' : this.browserNotificationTitle(n);
    // The Notification API shows the body as plain text.
    const excerpt = reviewSummaryExcerpt(n);
    new window.Notification(title, {
      body: excerpt ? `${n.message}\n\u201c${excerpt}\u201d` : n.message,
      tag: decisions ? DECISIONS_WAITING_KIND : n.id, // deduplicate in OS notification center
      icon: getNotificationIconUrl(),
    });
  }

  private browserNotificationTitle(n: Notification): string {
    switch (n.status) {
      case 'COMPLETED':
        return 'Agent Completed';
      case 'WAITING_FOR_INPUT':
        return 'Agent Waiting on Parent';
      case 'LIMITS_EXCEEDED':
        return 'Agent Limits Exceeded';
      case SCHEDULE_BLOCKED_STATUS:
        return 'Schedule Blocked';
      case ARTIFACT_REVIEW_SUBMITTED_STATUS: {
        const match =
          /^Review \(v(\d+)\) of your artifact /.exec(n.message) ??
          /\(v(\d+) of your artifact\):$/.exec(n.message);
        return match ? `Review submitted on v${match[1]}` : 'Review submitted';
      }
      case ARTIFACT_REVIEW_REQUESTED_STATUS:
        return 'Review requested';
      case ARTIFACT_REVIEW_DISMISSED_STATUS:
        return 'Review request dismissed';
      case SECRET_PENDING_STATUS:
        return 'Secret Awaiting Approval';
      case DECISIONS_WAITING_STATUS:
        return 'Decisions waiting';
      default:
        return `${getBrandName()} Notification`;
    }
  }

  private async ackOne(id: string): Promise<void> {
    const requestUserId = this.user?.id ?? null;
    try {
      await apiFetch(`/api/v1/notifications/${id}/ack`, { method: 'POST' });
      if (!this.isForCurrentUser(requestUserId)) return;
      this.notifications = this.notifications.filter((n) => n.id !== id);
      this.upsertedAtSeq.delete(id);
      this.ackedAtSeq.set(id, this.fetchSeq);
    } catch {
      // Ignore
    }
  }

  private async ackAll(): Promise<void> {
    const requestUserId = this.user?.id ?? null;
    try {
      await apiFetch('/api/v1/notifications/ack-all', { method: 'POST' });
      if (!this.isForCurrentUser(requestUserId)) return;
      for (const n of this.notifications) {
        this.ackedAtSeq.set(n.id, this.fetchSeq);
      }
      this.notifications = [];
      this.upsertedAtSeq.clear();
    } catch {
      // Ignore
    }
  }

  // ---------------------------------------------------------------------------
  // Panel toggle & click-outside
  // ---------------------------------------------------------------------------

  private toggle(): void {
    this.open = !this.open;
    if (this.open) {
      // Defer listener so the current click doesn't immediately close
      requestAnimationFrame(() => {
        document.addEventListener('click', this.boundOnClickOutside, true);
      });
      // Refresh on open and reconcile Decisions bell asynchronously (B6)
      void this.fetchNotifications();
      this.reconcileDecisionsBell();
    } else {
      document.removeEventListener('click', this.boundOnClickOutside, true);
    }
  }

  private onClickOutside(e: Event): void {
    const path = e.composedPath();
    if (!path.includes(this)) {
      this.open = false;
      document.removeEventListener('click', this.boundOnClickOutside, true);
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private statusIcon(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'check-circle-fill';
      case 'WAITING_FOR_INPUT':
        return 'exclamation-circle-fill';
      case 'LIMITS_EXCEEDED':
        return 'x-circle-fill';
      case SCHEDULE_BLOCKED_STATUS:
        return 'calendar-x';
      case SECRET_PENDING_STATUS:
        return 'shield-lock';
      case DECISIONS_WAITING_STATUS:
        return 'signpost-split';
      case ARTIFACT_REVIEW_REQUESTED_STATUS:
        return 'person-check';
      case ARTIFACT_REVIEW_DISMISSED_STATUS:
        return 'x-circle';
      default:
        return 'info-circle-fill';
    }
  }

  private statusClass(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'status-success';
      case 'WAITING_FOR_INPUT':
      case SECRET_PENDING_STATUS:
        return 'status-warning';
      case 'LIMITS_EXCEEDED':
      case SCHEDULE_BLOCKED_STATUS:
        return 'status-danger';
      case DECISIONS_WAITING_STATUS:
        return 'status-primary';
      default:
        return 'status-info';
    }
  }

  private relativeTime(iso: string): string {
    const ms = new Date(iso).getTime();
    if (Number.isNaN(ms)) return '—';
    // A future instant is clock skew between hub and browser.
    if (ms > Date.now()) return 'just now';
    return formatRelative(iso, { style: 'narrow' });
  }

  // ---------------------------------------------------------------------------
  // Styles
  // ---------------------------------------------------------------------------

  static override styles = css`
    :host {
      position: relative;
      display: inline-flex;
      align-items: center;
    }

    /* Bell button */
    .bell-btn {
      position: relative;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2.25rem;
      height: 2.25rem;
      border: none;
      border-radius: var(--scion-radius);
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      transition:
        background 0.15s ease,
        color 0.15s ease;
    }

    .bell-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .bell-btn sl-icon {
      font-size: 1.25rem;
    }

    /* Badge */
    .badge {
      position: absolute;
      top: 2px;
      right: 2px;
      min-width: 16px;
      height: 16px;
      padding: 0 4px;
      border-radius: var(--scion-radius-full);
      background: var(--sl-color-danger-600, #dc2626);
      color: var(--sl-color-neutral-0, #fff);
      font-size: 0.625rem;
      font-weight: 700;
      line-height: 16px;
      text-align: center;
      pointer-events: none;
    }

    .badge.pulse {
      animation: badge-pulse 2s ease-in-out infinite;
    }

    @keyframes badge-pulse {
      0%,
      100% {
        transform: scale(1);
        opacity: 1;
      }
      50% {
        transform: scale(1.15);
        opacity: 0.85;
      }
    }

    /* Panel */
    .panel {
      position: absolute;
      top: calc(100% + 0.5rem);
      right: 0;
      width: 360px;
      max-height: 480px;
      display: flex;
      flex-direction: column;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg);
      box-shadow:
        0 10px 15px -3px rgba(0, 0, 0, 0.1),
        0 4px 6px -4px rgba(0, 0, 0, 0.1);
      z-index: 1000;
      overflow: hidden;
    }

    .panel-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .panel-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .panel-header-actions {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .inbox-link {
      color: var(--scion-primary, #3b82f6);
      font-size: 0.75rem;
      font-weight: 600;
      text-decoration: none;
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius-sm);
      transition: background 0.15s ease;
    }

    .inbox-link:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .empty-inbox-link {
      color: var(--scion-primary, #3b82f6);
      font-weight: 500;
      text-decoration: none;
    }

    .empty-inbox-link:hover {
      text-decoration: underline;
    }

    .mark-all-btn {
      border: none;
      background: transparent;
      color: var(--scion-primary, #3b82f6);
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius-sm);
      transition: background 0.15s ease;
    }

    .mark-all-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .panel-list {
      flex: 1;
      overflow-y: auto;
      overscroll-behavior: contain;
    }

    /* Notification item */
    .notif-item {
      display: flex;
      gap: 0.625rem;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      transition: background 0.1s ease;
    }

    .notif-item:last-child {
      border-bottom: none;
    }

    .notif-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .notif-item.pinned {
      background: color-mix(
        in srgb,
        var(--scion-primary, #3b82f6) 5%,
        var(--scion-surface, #ffffff)
      );
    }

    .notif-item.pinned:hover {
      background: color-mix(
        in srgb,
        var(--scion-primary, #3b82f6) 9%,
        var(--scion-surface, #ffffff)
      );
    }

    .notif-icon {
      flex-shrink: 0;
      display: flex;
      align-items: flex-start;
      padding-top: 2px;
    }

    .notif-icon sl-icon {
      font-size: 1rem;
    }

    .status-success sl-icon {
      color: var(--scion-status-success, #22c55e);
    }

    .status-warning sl-icon {
      color: var(--scion-status-warning, #f59e0b);
    }

    .status-danger sl-icon {
      color: var(--scion-status-danger, #ef4444);
    }

    .status-primary sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .status-info sl-icon {
      color: var(--scion-text-muted, #64748b);
    }

    .notif-body {
      flex: 1;
      min-width: 0;
    }

    .notif-title {
      font-size: 0.75rem;
      font-weight: 600;
      line-height: 1.3;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.125rem;
    }

    .notif-message {
      font-size: 0.8125rem;
      line-height: 1.4;
      color: var(--scion-text, #1e293b);
      word-break: break-word;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
    }

    .notif-excerpt {
      margin-top: 0.25rem;
      padding-left: 0.5rem;
      border-left: 2px solid var(--scion-border, #e2e8f0);
      font-size: 0.75rem;
      line-height: 1.4;
      font-style: italic;
      color: var(--scion-text-muted, #64748b);
      word-break: break-word;
      white-space: normal;
    }

    .truncation-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0 0.375rem;
      margin-top: 0.125rem;
      font-size: 0.6875rem;
      font-weight: 700;
      line-height: 1.25rem;
      border-radius: var(--scion-radius);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      letter-spacing: 0.05em;
    }

    .truncation-badge:hover {
      background: var(--scion-border, #e2e8f0);
    }

    .notif-meta {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.25rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .notif-meta a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .notif-meta a.decisions-open-link {
      font-weight: 600;
    }

    .notif-meta a:hover {
      text-decoration: underline;
    }

    .mark-read-link {
      border: none;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
      cursor: pointer;
      padding: 0;
      transition: color 0.15s ease;
    }

    .mark-read-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .scope-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.125rem;
      font-size: 0.625rem;
      font-weight: 500;
      padding: 0.0625rem 0.3125rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      vertical-align: middle;
    }

    .scope-indicator sl-icon {
      font-size: 0.5625rem;
    }

    .panel-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .push-toggle {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      border: none;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius-sm);
      transition: background 0.15s ease;
    }

    .push-toggle.on {
      color: var(--scion-primary, #3b82f6);
    }

    .push-toggle:hover:not(:disabled) {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .push-toggle:disabled {
      cursor: not-allowed;
      opacity: 0.6;
    }

    .manage-link {
      border: none;
      background: transparent;
      color: var(--scion-primary, #3b82f6);
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius-sm);
      text-decoration: none;
      transition: background 0.15s ease;
    }

    .manage-link:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    /* Empty state */
    .empty-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 2.5rem 1rem;
      gap: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2rem;
      opacity: 0.4;
    }

    .empty-state span {
      font-size: 0.8125rem;
    }

    /* Mobile */
    @media (max-width: 640px) {
      .panel {
        position: fixed;
        top: auto;
        bottom: 0;
        left: 0;
        right: 0;
        width: 100%;
        max-height: 70vh;
        border-radius: var(--scion-radius-lg) var(--scion-radius-lg) 0 0;
      }
    }
  `;

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    const count = this.notifications.length;

    return html`
      <button
        class="bell-btn"
        @click=${(): void => this.toggle()}
        aria-label="Notifications"
        aria-haspopup="true"
        aria-expanded=${this.open}
      >
        <sl-icon name="bell"></sl-icon>
        ${count > 0
          ? html`<span class="badge pulse">${count > 99 ? '99+' : count}</span>`
          : nothing}
      </button>
      ${this.open ? this.renderPanel() : nothing}
    `;
  }

  private renderPanel() {
    const count = this.notifications.length;
    const inboxOn = this.isConnected ? this.inboxEnabled : isInboxDashboardEnabled();
    const inboxBadge = this.isConnected ? this.inboxBadge : formatInboxSummaryBadge();
    return html`
      <div class="panel" role="dialog" aria-label="Notifications">
        <div class="panel-header">
          <h3 class="panel-title">Notifications</h3>
          ${inboxOn || count > 0
            ? html`<div class="panel-header-actions">
                ${inboxOn
                  ? html`<a
                      href="/profile/inbox"
                      class="inbox-link"
                      data-tray-inbox-link
                      @click=${(e: Event): void => this.navigateToPath(e, '/profile/inbox')}
                      >${inboxBadge ? `Inbox \u00b7 ${inboxBadge} \u2192` : 'Inbox \u2192'}</a
                    >`
                  : nothing}
                ${count > 0
                  ? html`<button class="mark-all-btn" @click=${(): void => void this.ackAll()}>
                      Mark all read
                    </button>`
                  : nothing}
              </div>`
            : nothing}
        </div>
        <div class="panel-list">
          ${count > 0 ? this.notifications.map((n) => this.renderItem(n)) : this.renderEmpty()}
        </div>
        <div class="panel-footer">
          ${this.renderPushToggle()}
          <a
            href="/profile/settings"
            class="manage-link"
            @click=${(e: Event): void => {
              e.preventDefault();
              this.open = false;
              document.removeEventListener('click', this.boundOnClickOutside, true);
              navigateTo('/profile/settings');
            }}
            >Manage subscriptions</a
          >
        </div>
      </div>
    `;
  }

  /**
   * The agent-event desktop-notification toggle. Lives in the tray because
   * the tray is the one notification surface present on every page, chat
   * included — the profile settings page is not reachable without leaving a
   * conversation. It mirrors the profile page's agent event alerts toggle.
   */
  private renderPushToggle() {
    if (this.pushPermission === 'unsupported') return nothing;

    // Permission alone decides this. A user who opted in and later revoked
    // permission in site settings still has the stored flag set, and reading
    // it here left the button live: the click could not re-prompt (permission
    // is 'denied', not 'default'), so it silently cleared the flag and only
    // the next render admitted the button was blocked all along.
    const blocked = this.pushPermission === 'denied';
    const label = blocked
      ? 'Agent alerts blocked'
      : this.pushEnabled
        ? 'Agent alerts on'
        : 'Agent alerts off';

    return html`
      <button
        class="push-toggle ${this.pushEnabled ? 'on' : ''}"
        role="switch"
        aria-checked=${this.pushEnabled}
        ?disabled=${blocked}
        title=${blocked ? 'Allow notifications in your browser site settings' : label}
        @click=${(): void => void this.handlePushToggle()}
      >
        <sl-icon name=${this.pushEnabled ? 'bell-fill' : 'bell-slash'}></sl-icon>
        ${label}
      </button>
    `;
  }

  private renderItem(n: Notification) {
    // The excerpt is another principal's text: it is bound as a text node,
    // in its own element under the message that attributes it, never as
    // markup, and the link beside it comes from the resource fields.
    const excerpt = reviewSummaryExcerpt(n);
    const artifactLink = reviewSummaryLink(n);
    const secretsLink = pendingSecretLink(n);
    const decisionsLink = decisionsWaitingLink(n);
    const isDecisions = decisionsLink !== null;
    const iconStatus = isDecisions ? DECISIONS_WAITING_STATUS : n.status;
    return html`
      <div class="notif-item ${isDecisions ? 'pinned' : ''}">
        <div class="notif-icon ${this.statusClass(iconStatus)}">
          <sl-icon name=${this.statusIcon(iconStatus)}></sl-icon>
        </div>
        <div class="notif-body">
          ${isDecisions
            ? html`<div class="notif-title">Decisions waiting</div>`
            : n.status === ARTIFACT_REVIEW_DISMISSED_STATUS
              ? html`<div class="notif-title">Review request dismissed</div>`
              : nothing}
          <div class="notif-message">${n.message}</div>
          <sl-tooltip content=${n.message} hoist>
            <span class="truncation-badge" style="display:none">...</span>
          </sl-tooltip>
          ${excerpt ? html`<div class="notif-excerpt">“${excerpt}”</div>` : nothing}
          <div class="notif-meta">
            <span>${this.relativeTime(n.createdAt)}</span>
            ${decisionsLink
              ? html`<a
                  href=${decisionsLink}
                  class="decisions-open-link"
                  @click=${(e: Event): void => this.navigateToPath(e, decisionsLink)}
                >
                  Open Decisions
                </a>`
              : nothing}
            ${artifactLink
              ? html`<a
                  href=${artifactLink}
                  @click=${(e: Event): void => this.navigateToPath(e, artifactLink)}
                >
                  View artifact
                </a>`
              : nothing}
            ${secretsLink
              ? html`<a
                  href=${secretsLink}
                  @click=${(e: Event): void => this.navigateToPath(e, secretsLink)}
                >
                  Review secrets
                </a>`
              : nothing}
            ${!hasAgentLink(n.agentId)
              ? // Older rows can carry no agent (the nil UUID); a link would
                // 404, so offer none.
                nothing
              : html`<a
                  href="/agents/${n.agentId}"
                  @click=${(e: Event): void => this.navigateToAgent(e, n.agentId)}
                >
                  View agent
                </a>`}
            <button class="mark-read-link" @click=${(): void => void this.ackOne(n.id)}>
              Mark read
            </button>
          </div>
        </div>
      </div>
    `;
  }

  private renderEmpty() {
    const inboxOn = this.isConnected ? this.inboxEnabled : isInboxDashboardEnabled();
    const inboxTotal = this.isConnected ? this.inboxTotal : getInboxSummaryTotal();
    return html`
      <div class="empty-state">
        <sl-icon name="bell-slash"></sl-icon>
        ${inboxOn && inboxTotal > 0
          ? html`<span>
              No new notifications.
              <a
                href="/profile/inbox"
                class="empty-inbox-link"
                data-tray-empty-inbox-link
                @click=${(e: Event): void => this.navigateToPath(e, '/profile/inbox')}
                >Open Inbox</a
              >
            </span>`
          : html`<span>No notifications</span>`}
      </div>
    `;
  }

  private navigateToAgent(e: Event, agentId: string): void {
    this.navigateToPath(e, `/agents/${agentId}`);
  }

  private navigateToPath(e: Event, path: string): void {
    e.preventDefault();
    this.open = false;
    document.removeEventListener('click', this.boundOnClickOutside, true);
    navigateTo(path);
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-notification-tray': ScionNotificationTray;
  }
}
