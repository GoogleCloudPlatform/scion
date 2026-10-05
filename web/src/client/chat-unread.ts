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
 * Unread chat count for the tab title badge.
 *
 * Two halves, kept separately because they have different sources: the space
 * rollup (threads across all projects) and DMs. On the chat page both arrive
 * with data the page already loaded, and are pushed in rather than fetched
 * again; anywhere else, this module fetches them itself.
 *
 * Muting is honoured in both halves. A muted conversation is the user saying
 * "stop telling me about this", and a number in the tab title is telling them.
 */

import { apiFetch } from './api.js';
import { isChatNotificationStatus } from './chat-notifications.js';
import { setUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

/**
 * Refresh coalescing window. A burst of messages in a busy thread raises one
 * event each; without this the badge would issue a pair of requests per
 * message. Matches the debounce the chat page uses for its own reloads.
 */
export const UNREAD_REFRESH_DEBOUNCE_MS = 500;

/**
 * Upper bound on how long the first refresh waits after start(). The first
 * refresh runs at the page's first idle period so it does not compete with
 * the page's own requests, and no later than this. It is also how long the
 * chat page's hold lasts (see holdFirstRefreshForPagePushes).
 */
export const INITIAL_REFRESH_MAX_DELAY_MS = 3000;

type InitialHandle =
  | { kind: 'idle'; id: number }
  | { kind: 'timeout'; id: ReturnType<typeof setTimeout> };

/** The unread fields of `GET /api/v1/chat/spaces`. */
export interface UnreadSpace {
  unreadCount?: number;
}

/** The unread fields of `GET /api/v1/chat/dms`. */
export interface UnreadDM {
  hasUnread?: boolean;
  muted?: boolean;
}

/**
 * Unread threads across all spaces.
 *
 * `unreadCount` is the server's rollup and already excludes muted threads, so
 * this must not filter again — it would double-count the exclusion.
 */
export function countUnreadSpaces(spaces: readonly UnreadSpace[]): number {
  return spaces.reduce((total, s) => total + Math.max(0, s.unreadCount ?? 0), 0);
}

/** Unread, unmuted DM conversations. */
export function countUnreadDMs(dms: readonly UnreadDM[]): number {
  return dms.filter((dm) => dm.hasUnread && !dm.muted).length;
}

/**
 * Owns the tab-title unread count for the lifetime of the page.
 */
export class ChatUnreadCounter {
  private spaceUnread = 0;
  private dmUnread = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private listening = false;
  private stopped = false;
  /** Incrementing counter to detect stale refresh results. */
  private refreshId = 0;
  /** The pending first refresh, until it runs or is superseded. */
  private initial: InitialHandle | null = null;
  private startedAt = 0;
  private holdingForPagePushes = false;
  /** Which halves have been pushed in since start(). */
  private spacesPushed = false;
  private dmsPushed = false;
  private readonly boundSchedule = (): void => this.scheduleRefresh();
  private readonly boundNotification = (e: Event): void => this.onNotification(e);

  /**
   * Begins tracking. The first refresh is deferred to the page's first idle
   * period (at most INITIAL_REFRESH_MAX_DELAY_MS), so it stays off the
   * critical path of the page's own requests.
   */
  start(): void {
    if (this.listening) return;
    // Anything that can create or clear an unread conversation.
    stateManager.addEventListener('notification-created', this.boundNotification);
    stateManager.addEventListener('chat-message-received', this.boundSchedule);
    stateManager.addEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = true;
    this.stopped = false;
    this.spacesPushed = false;
    this.dmsPushed = false;
    this.holdingForPagePushes = false;
    this.startedAt = Date.now();
    this.scheduleInitialRefresh();
  }

  stop(): void {
    if (!this.listening) return;
    stateManager.removeEventListener('notification-created', this.boundNotification);
    stateManager.removeEventListener('chat-message-received', this.boundSchedule);
    stateManager.removeEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = false;
    this.stopped = true;
    this.cancelPending();
    this.cancelInitialRefresh();
  }

  /**
   * Holds the first refresh for data the chat page is about to push in.
   *
   * The chat page loads both halves itself, and those loads are slow enough
   * that an idle-time refresh would usually go out alongside them. Once this
   * is called, the first refresh waits until INITIAL_REFRESH_MAX_DELAY_MS
   * after start() instead of the first idle period; if both halves were pushed
   * by then it sends nothing, otherwise it fetches both as usual.
   *
   * Only the first refresh is affected: this does nothing once that refresh
   * has run or been superseded, and refreshes for live chat events are never
   * held.
   */
  holdFirstRefreshForPagePushes(): void {
    if (!this.initial || this.holdingForPagePushes) return;
    this.holdingForPagePushes = true;
    this.cancelInitialRefresh();
    const remaining = Math.max(0, this.startedAt + INITIAL_REFRESH_MAX_DELAY_MS - Date.now());
    this.initial = { kind: 'timeout', id: setTimeout(() => this.runInitialRefresh(), remaining) };
  }

  /**
   * Space rollup, from data the chat rail already loaded.
   *
   * Neither setter cancels a pending refresh. Each owns one half, and the
   * refresh they would cancel carries both — so cancelling starves the other
   * half for as long as pushes keep arriving. The cost of not cancelling is
   * one redundant fetch that overwrites a push with equally-correct server
   * data; the cost of cancelling was a tab-title badge that stopped moving
   * during exactly the burst it exists to report.
   */
  setSpaceUnread(spaces: readonly UnreadSpace[]): void {
    this.spaceUnread = countUnreadSpaces(spaces);
    this.spacesPushed = true;
    this.publish();
  }

  /** DM half, from data the chat page already loaded. */
  setDMUnread(dms: readonly UnreadDM[]): void {
    this.dmUnread = countUnreadDMs(dms);
    this.dmsPushed = true;
    this.publish();
  }

  /**
   * Refreshes for chat notifications only.
   *
   * Agent-status notifications cannot change an unread chat count, and they
   * still broadcast to every logged-in session (#1125) — so without this
   * guard one agent-status event anywhere on the deployment costs every
   * signed-in browser a `/chat/spaces` + `/chat/dms` pair, on every page.
   * `/chat/spaces` is the heaviest chat endpoint there is.
   *
   * Unscoped events carry no payload, so they fail the same test as a
   * non-chat status and need no separate branch.
   */
  private onNotification(e: Event): void {
    const { detail } = e as CustomEvent<{ data?: { status?: string } } | undefined>;
    if (!isChatNotificationStatus(detail?.data?.status)) return;
    this.scheduleRefresh();
  }

  /** Coalesces a burst of events into a single refresh. */
  scheduleRefresh(): void {
    // This refresh carries both halves, so a pending first refresh would
    // only repeat it.
    this.cancelInitialRefresh();
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, UNREAD_REFRESH_DEBOUNCE_MS);
  }

  /** Recomputes both halves from the server. */
  async refresh(): Promise<void> {
    this.cancelInitialRefresh();
    const localId = ++this.refreshId;
    const [spaces, dms] = await Promise.all([this.fetchSpaces(), this.fetchDMs()]);
    // Discard stale results: a newer refresh was started while we awaited.
    if (this.stopped || localId !== this.refreshId) return;
    if (spaces) this.spaceUnread = countUnreadSpaces(spaces);
    if (dms) this.dmUnread = countUnreadDMs(dms);
    this.publish();
  }

  private scheduleInitialRefresh(): void {
    this.cancelInitialRefresh();
    const run = (): void => this.runInitialRefresh();
    if (typeof window.requestIdleCallback === 'function') {
      this.initial = {
        kind: 'idle',
        id: window.requestIdleCallback(run, { timeout: INITIAL_REFRESH_MAX_DELAY_MS }),
      };
    } else {
      this.initial = { kind: 'timeout', id: setTimeout(run, INITIAL_REFRESH_MAX_DELAY_MS) };
    }
  }

  private runInitialRefresh(): void {
    this.initial = null;
    // The chat page already supplied both halves: fetching them again would
    // repeat its requests for the same answer.
    if (this.spacesPushed && this.dmsPushed) return;
    void this.refresh();
  }

  private cancelInitialRefresh(): void {
    const pending = this.initial;
    if (!pending) return;
    this.initial = null;
    if (pending.kind === 'idle') window.cancelIdleCallback(pending.id);
    else clearTimeout(pending.id);
  }

  private cancelPending(): void {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  private publish(): void {
    setUnreadBadge(this.spaceUnread + this.dmUnread);
  }

  private async fetchSpaces(): Promise<UnreadSpace[] | null> {
    try {
      const res = await apiFetch('/api/v1/chat/spaces');
      if (!res.ok) return null;
      const data = (await res.json()) as { spaces?: UnreadSpace[] };
      return data?.spaces ?? [];
    } catch {
      // Offline or chat disabled — keep the last known count rather than
      // flashing the badge to zero.
      return null;
    }
  }

  private async fetchDMs(): Promise<UnreadDM[] | null> {
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (!res.ok) return null;
      const data = (await res.json()) as { dms?: UnreadDM[] };
      return data?.dms ?? [];
    } catch {
      return null;
    }
  }
}

/**
 * Starts the counter only for a signed-in user with native chat enabled. With
 * chat disabled the endpoints it reads are not registered, and with nobody
 * signed in there is nothing to count. Returns whether it started.
 */
export function startChatUnreadIfEligible(
  counter: Pick<ChatUnreadCounter, 'start'>,
  signedIn: boolean,
  chatEnabled: boolean
): boolean {
  if (!signedIn || !chatEnabled) return false;
  counter.start();
  return true;
}

/** The page-wide unread counter. */
export const chatUnread = new ChatUnreadCounter();
