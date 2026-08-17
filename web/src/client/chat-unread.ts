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
import { setUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

/**
 * Refresh coalescing window. A burst of messages in a busy thread raises one
 * event each; without this the badge would issue a pair of requests per
 * message. Matches the debounce the chat page uses for its own reloads.
 */
export const UNREAD_REFRESH_DEBOUNCE_MS = 500;

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
  private readonly boundSchedule = (): void => this.scheduleRefresh();

  /** Begins tracking, with one immediate refresh. */
  start(): void {
    if (this.listening) return;
    // Anything that can create or clear an unread conversation.
    stateManager.addEventListener('notification-created', this.boundSchedule);
    stateManager.addEventListener('chat-message-received', this.boundSchedule);
    stateManager.addEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = true;
    void this.refresh();
  }

  stop(): void {
    if (!this.listening) return;
    stateManager.removeEventListener('notification-created', this.boundSchedule);
    stateManager.removeEventListener('chat-message-received', this.boundSchedule);
    stateManager.removeEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = false;
    this.cancelPending();
  }

  /** Space rollup, from data the chat rail already loaded. */
  setSpaceUnread(spaces: readonly UnreadSpace[]): void {
    this.spaceUnread = countUnreadSpaces(spaces);
    this.publish();
    // A push from the page is fresher than anything a queued fetch would
    // return, and would otherwise be overwritten by it moments later.
    this.cancelPending();
  }

  /**
   * DM half, from data the chat page already loaded.
   *
   * Deliberately does not cancel a pending refresh, unlike `setSpaceUnread`:
   * this owns only `dmUnread`, and the refresh it would cancel also carries
   * the space half. The chat page pushes DMs in on every inbound message, so
   * cancelling here starves the thread count for the whole of a busy burst.
   */
  setDMUnread(dms: readonly UnreadDM[]): void {
    this.dmUnread = countUnreadDMs(dms);
    this.publish();
  }

  /** Coalesces a burst of events into a single refresh. */
  scheduleRefresh(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, UNREAD_REFRESH_DEBOUNCE_MS);
  }

  /** Recomputes both halves from the server. */
  async refresh(): Promise<void> {
    const [spaces, dms] = await Promise.all([this.fetchSpaces(), this.fetchDMs()]);
    if (spaces) this.spaceUnread = countUnreadSpaces(spaces);
    if (dms) this.dmUnread = countUnreadDMs(dms);
    this.publish();
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
      return data.spaces ?? [];
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
      return data.dms ?? [];
    } catch {
      return null;
    }
  }
}

/** The page-wide unread counter. */
export const chatUnread = new ChatUnreadCounter();
