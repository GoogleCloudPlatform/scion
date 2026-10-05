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
 * Unread conversation count: the chat mode badge in the header and the tab
 * title badge show the same number.
 *
 * The number comes from `GET /api/v1/chat/unread-count`, whose
 * `conversations` field counts the caller's unread, unmuted conversations
 * (member threads plus DMs). The server owns membership and muting, so this
 * module never filters or adds anything itself.
 *
 * It refreshes, debounced, on every chat message and read-state event, and
 * whenever the chat page reports that its own lists changed (the user read
 * or muted something in this tab, which the server does not echo back).
 *
 * Known staleness: off the chat page only DM events arrive (thread messages
 * come on project subjects a page may not subscribe to), and a read or mute
 * in another tab publishes nothing, so the count can lag until the next
 * event. Both are fixed on the hub side: fanning thread messages out to
 * member participants on `user.<id>.chat.message`, and publishing the
 * reader's own read and mute changes on `user.<id>.chat.read-state`.
 */

import { apiFetch } from './api.js';
import { setUnreadBadge } from './page-title.js';
import { stateManager } from './state.js';

/** The unread count endpoint. */
export const CHAT_UNREAD_COUNT_URL = '/api/v1/chat/unread-count';

/**
 * Fired on `window` whenever the count changes. The detail carries the new
 * count; `chatUnread.count` holds it for late subscribers.
 */
export const CHAT_UNREAD_COUNT_EVENT = 'scion:chat-unread-count';

export interface ChatUnreadCountDetail {
  count: number;
}

/**
 * Refresh coalescing window. A burst of messages in a busy thread raises one
 * event each; without this the badge would issue a request per message.
 * Matches the debounce the chat page uses for its own reloads.
 */
export const UNREAD_REFRESH_DEBOUNCE_MS = 500;

/**
 * How long a deferred first refresh waits for the page's first idle period
 * before it asks to run anyway. This is the timeout passed to
 * requestIdleCallback (or the fallback timer's delay), so it is a minimum
 * wait for the forced run, not a guaranteed maximum.
 */
export const INITIAL_REFRESH_MAX_DELAY_MS = 3000;

type InitialHandle =
  | { kind: 'idle'; id: number }
  | { kind: 'timeout'; id: ReturnType<typeof setTimeout> };

/** The body of `GET /api/v1/chat/unread-count`. */
export interface ChatUnreadCountBody {
  conversations?: number;
  threads?: number;
  dms?: number;
}

/**
 * The conversation count from a response body, or null when the body does
 * not carry a usable one (so the caller keeps the last known count).
 */
export function unreadConversations(body: unknown): number | null {
  const n = (body as ChatUnreadCountBody | null)?.conversations;
  if (typeof n !== 'number' || !Number.isFinite(n)) return null;
  return Math.max(0, Math.floor(n));
}

/**
 * The browser's idle-callback API, or null where there is no window (a
 * non-browser environment) or the browser lacks it; callers then use a timer.
 */
function idleCallbacks(): Pick<Window, 'requestIdleCallback' | 'cancelIdleCallback'> | null {
  if (
    typeof window === 'undefined' ||
    typeof window.requestIdleCallback !== 'function' ||
    typeof window.cancelIdleCallback !== 'function'
  ) {
    return null;
  }
  return window;
}

/**
 * Owns the unread conversation count for the lifetime of the page.
 */
export class ChatUnreadCounter {
  private current = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private listening = false;
  private stopped = false;
  /** Incrementing counter to detect stale refresh results. */
  private refreshId = 0;
  /** The pending first refresh, until it runs or is superseded. */
  private initial: InitialHandle | null = null;
  private readonly boundSchedule = (): void => this.scheduleRefresh();

  /** The latest known unread conversation count. */
  get count(): number {
    return this.current;
  }

  /**
   * Begins tracking. By default the first refresh is deferred to the page's
   * first idle period, so it stays off the critical path of the page's own
   * requests. With `immediate` it is sent now.
   */
  start(options: { immediate?: boolean } = {}): void {
    if (this.listening) return;
    // Anything that can create or clear an unread conversation.
    stateManager.addEventListener('chat-message-received', this.boundSchedule);
    stateManager.addEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = true;
    this.stopped = false;
    if (options.immediate) void this.refresh();
    else this.scheduleInitialRefresh();
  }

  /** Stops tracking and clears the count (a signed-out page shows none). */
  stop(): void {
    if (!this.listening) return;
    stateManager.removeEventListener('chat-message-received', this.boundSchedule);
    stateManager.removeEventListener('chat-read-state-updated', this.boundSchedule);
    this.listening = false;
    this.stopped = true;
    this.cancelPending();
    this.cancelInitialRefresh();
    this.publish(0);
  }

  /** Coalesces a burst of events into a single refresh. */
  scheduleRefresh(): void {
    if (!this.listening) return;
    // This refresh supersedes a pending first refresh.
    this.cancelInitialRefresh();
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, UNREAD_REFRESH_DEBOUNCE_MS);
  }

  /** Fetches the count now. */
  async refresh(): Promise<void> {
    this.cancelInitialRefresh();
    const localId = ++this.refreshId;
    const count = await this.fetchCount();
    // Discard stale results: a newer refresh was started while we awaited.
    if (this.stopped || localId !== this.refreshId) return;
    // A failed load (offline, chat disabled) keeps the last known count
    // rather than flashing the badge to zero.
    if (count !== null) this.publish(count);
  }

  private scheduleInitialRefresh(): void {
    this.cancelInitialRefresh();
    const run = (): void => {
      this.initial = null;
      void this.refresh();
    };
    const idle = idleCallbacks();
    if (idle) {
      this.initial = {
        kind: 'idle',
        id: idle.requestIdleCallback(run, { timeout: INITIAL_REFRESH_MAX_DELAY_MS }),
      };
    } else {
      this.initial = { kind: 'timeout', id: setTimeout(run, INITIAL_REFRESH_MAX_DELAY_MS) };
    }
  }

  private cancelInitialRefresh(): void {
    const pending = this.initial;
    if (!pending) return;
    this.initial = null;
    if (pending.kind === 'idle') idleCallbacks()?.cancelIdleCallback(pending.id);
    else clearTimeout(pending.id);
  }

  private cancelPending(): void {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  private publish(count: number): void {
    setUnreadBadge(count);
    if (count === this.current) return;
    this.current = count;
    if (typeof window !== 'undefined') {
      window.dispatchEvent(
        new CustomEvent<ChatUnreadCountDetail>(CHAT_UNREAD_COUNT_EVENT, { detail: { count } })
      );
    }
  }

  private async fetchCount(): Promise<number | null> {
    try {
      const res = await apiFetch(CHAT_UNREAD_COUNT_URL);
      if (!res.ok) return null;
      return unreadConversations(await res.json());
    } catch {
      return null;
    }
  }
}

/**
 * Starts the counter only for a signed-in user with native chat enabled. With
 * chat disabled the endpoint it reads is not registered, and with nobody
 * signed in there is nothing to count. Returns whether it started.
 *
 * On a chat route the first refresh is sent at once; elsewhere it waits for
 * the first idle period.
 */
export function startChatUnreadIfEligible(
  counter: Pick<ChatUnreadCounter, 'start'>,
  signedIn: boolean,
  chatEnabled: boolean,
  onChatRoute: boolean
): boolean {
  if (!signedIn || !chatEnabled) return false;
  counter.start({ immediate: onChatRoute });
  return true;
}

/** The page-wide unread counter. */
export const chatUnread = new ChatUnreadCounter();
