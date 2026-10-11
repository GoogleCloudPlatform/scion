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
 * Every page subscribes to the user's own chat subject, and the hub sends
 * there everything that can move the count: DM messages, thread messages
 * to the thread's members (`user.<id>.chat.message`), and the user's own
 * reads, mutes and mark-unreads (`user.<id>.chat.read-state`). A thread
 * message also arrives on its project subject when the page subscribes to
 * that; the debounce folds the two copies into one request.
 *
 * A failed request (an error status included) keeps the last count and is
 * not retried; the next event asks again.
 *
 * Cost control. The count is as heavy as the rail's space list (it is the
 * same rollup), so the counter avoids asking more than it must:
 *
 * - At most one request is in flight; events during it fold into a single
 *   trailing refresh.
 * - Tabs share answers. A refresh runs under a Web Lock and records its
 *   answer, with the user it is for and when the oldest request behind it
 *   started, in localStorage; a tab of the same user whose triggering event
 *   came before that adopts the answer instead of asking again. N tabs
 *   reacting to one event send one request.
 * - Every answer is bounded (UNREAD_ASK_TIMEOUT_MS): a hung request is
 *   abandoned as a failure, releasing the lock and the single flight.
 * - The chat page installs a source (`setSource`) that derives the count
 *   from the shared `/chat/spaces` and `/chat/dms` loads the rail and page
 *   already make for the same event, so on the chat page a message costs
 *   no extra rollup. The hub pins `unread-count` to exactly that sum
 *   (TestChatUnreadCount_BadgeEqualsRail).
 */

import { apiFetch } from './api.js';
import { chatLoadClock } from './chat-list-cache.js';
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

/** The Web Lock that serializes count requests across this origin's tabs. */
export const UNREAD_LOCK_NAME = 'scion-chat-unread-count';

/** The localStorage key holding the latest answer any tab got. */
export const UNREAD_SHARE_KEY = 'scion-chat-unread-share';

/**
 * How long a tab waits for the cross-tab lock before asking on its own. A
 * tab that holds the lock behind a hung request must not freeze the badge
 * everywhere else.
 */
export const UNREAD_LOCK_WAIT_MS = 10_000;

/**
 * How long one answer may take. A request past it is abandoned (the
 * endpoint's is aborted) and counts as a failure, keeping the last count,
 * so a hung request can neither freeze this tab's badge nor hold the
 * cross-tab lock.
 */
export const UNREAD_ASK_TIMEOUT_MS = 15_000;

/** One answer from a count source. */
export interface UnreadCountAnswer {
  count: number;
  /**
   * When the oldest request behind the answer was started, on the
   * `chatLoadClock` clock. Shared with other tabs as the answer's age, so
   * an answer built on a joined, older request is not taken for a newer one.
   */
  startedAt: number;
}

/**
 * A count source other than the endpoint. `startedAfter` is on the
 * `chatLoadClock` clock: an answer from a request started after it reflects
 * every event the refresh is for. Resolves to null when it cannot answer.
 */
export type UnreadCountSource = (startedAfter: number) => Promise<UnreadCountAnswer | null>;

/** The answer one tab got, as shared through localStorage. */
interface SharedAnswer {
  /** Whose count it is: a tab only adopts its own user's answers. */
  userId: string;
  count: number;
  /** Wall-clock time (Date.now) the oldest request behind it was started. */
  askedAt: number;
}

function readSharedAnswer(userId: string): SharedAnswer | null {
  try {
    const raw = localStorage.getItem(UNREAD_SHARE_KEY);
    if (!raw) return null;
    const v = JSON.parse(raw) as Partial<SharedAnswer>;
    if (typeof v.count !== 'number' || typeof v.askedAt !== 'number') return null;
    if (v.userId !== userId) return null;
    return { userId, count: v.count, askedAt: v.askedAt };
  } catch {
    return null;
  }
}

/**
 * A `chatLoadClock` instant as wall-clock time.
 *
 * Assumes the two clocks advance together. Where performance.now() pauses
 * during system sleep but Date.now() does not, an instant from before a
 * sleep converts too late, so an answer whose request spanned the sleep
 * looks newer than it is, and another tab may adopt it for an event from
 * just after wake. UNREAD_ASK_TIMEOUT_MS does not close this: browser
 * timers pause on the same clock. The window is small anyway: a request
 * must be in flight at the moment of sleep, a fetch cut by sleep usually
 * fails (no answer, nothing shared), and the cost is one stale count until
 * the next event.
 */
function perfToWall(at: number): number {
  return Date.now() - Math.max(0, chatLoadClock() - at);
}

/**
 * `promise`, or null once `ms` pass first; `onTimeout` runs then. A
 * rejection is null too.
 */
function withTimeout<T>(
  promise: Promise<T>,
  ms: number,
  onTimeout?: () => void
): Promise<T | null> {
  return new Promise<T | null>((resolve) => {
    const timer = setTimeout(() => {
      onTimeout?.();
      resolve(null);
    }, ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      () => {
        clearTimeout(timer);
        resolve(null);
      }
    );
  });
}

function writeSharedAnswer(answer: SharedAnswer): void {
  try {
    localStorage.setItem(UNREAD_SHARE_KEY, JSON.stringify(answer));
  } catch {
    // Storage full or disabled: tabs just ask on their own.
  }
}

/** The Web Locks API, or null where the browser (or test DOM) lacks it. */
function webLocks(): LockManager | null {
  if (typeof navigator === 'undefined') return null;
  const locks = (navigator as Navigator & { locks?: LockManager }).locks;
  return locks && typeof locks.request === 'function' ? locks : null;
}

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
  /** The running refresh, if any. */
  private inFlight: Promise<void> | null = null;
  /** A refresh was asked for while one was running: run once more after it. */
  private queued = false;
  /** Newest trigger not yet answered, on the chatLoadClock clock. */
  private pendingAfter = -Infinity;
  /** The same trigger on the wall clock, for comparing with other tabs. */
  private pendingWall = -Infinity;
  /** See setSource. */
  private source: UnreadCountSource | null = null;
  /** The signed-in user; without one, answers are not shared across tabs. */
  private userId = '';
  private readonly boundSchedule = (e: Event): void =>
    this.scheduleRefresh(e.timeStamp > 0 ? e.timeStamp : undefined);

  /** The latest known unread conversation count. */
  get count(): number {
    return this.current;
  }

  /**
   * Begins tracking. By default the first refresh is deferred to the page's
   * first idle period, so it stays off the critical path of the page's own
   * requests. With `immediate` it is sent now.
   */
  start(options: { immediate?: boolean; userId?: string } = {}): void {
    if (this.listening) return;
    this.userId = options.userId ?? '';
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
    this.queued = false;
    // A refresh still in flight belongs to the stopped session: a later
    // start() must not let it publish its answer. (It may still share it,
    // but under the userId captured when it began, so no other user
    // adopts it.)
    this.refreshId++;
    this.cancelPending();
    this.cancelInitialRefresh();
    this.publish(0);
  }

  /**
   * Replaces where the count comes from: a source while the chat page is
   * open (see the module comment), the endpoint again with null.
   */
  setSource(source: UnreadCountSource | null): void {
    this.source = source;
  }

  /**
   * Coalesces a burst of events into a single refresh. `eventAt` is when
   * the triggering event was delivered (an Event's timeStamp); without it,
   * now.
   */
  scheduleRefresh(eventAt?: number): void {
    if (!this.listening) return;
    this.noteTrigger(eventAt);
    // This refresh supersedes a pending first refresh.
    this.cancelInitialRefresh();
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, UNREAD_REFRESH_DEBOUNCE_MS);
  }

  /**
   * Fetches the count now. While a refresh is running, a call folds into
   * one trailing refresh after it and resolves when that one is done.
   */
  refresh(): Promise<void> {
    this.cancelInitialRefresh();
    // A debounced refresh answers the triggers scheduleRefresh recorded,
    // from when their events were delivered; a direct call is for now.
    if (this.pendingAfter === -Infinity) this.noteTrigger();
    if (this.inFlight) {
      this.queued = true;
      return this.inFlight;
    }
    const run = async (): Promise<void> => {
      try {
        do {
          this.queued = false;
          await this.refreshOnce();
        } while (this.queued && !this.stopped);
      } finally {
        this.inFlight = null;
      }
    };
    this.inFlight = run();
    return this.inFlight;
  }

  /** Records a trigger the next refresh must answer. */
  private noteTrigger(eventAt?: number): void {
    const at = eventAt ?? chatLoadClock();
    // An event delivered at `at` (perf clock) happened this long ago.
    const wall = perfToWall(at);
    this.pendingAfter = Math.max(this.pendingAfter, at);
    this.pendingWall = Math.max(this.pendingWall, wall);
  }

  private async refreshOnce(): Promise<void> {
    const localId = ++this.refreshId;
    const after = this.pendingAfter;
    const wall = this.pendingWall;
    this.pendingAfter = -Infinity;
    this.pendingWall = -Infinity;
    const count = await this.askShared(after, wall);
    // Discard stale results: a newer refresh was started while we awaited.
    if (this.stopped || localId !== this.refreshId) return;
    // A failed load (offline, chat disabled) keeps the last known count
    // rather than flashing the badge to zero.
    if (count !== null) this.publish(count);
  }

  /**
   * Answers a refresh for triggers up to `after` (perf clock) / `wall`
   * (wall clock): adopts another tab's answer whose request started after
   * the trigger, or asks and shares the answer. Without Web Locks, or if
   * the lock does not come in time, it just asks.
   */
  private async askShared(after: number, wall: number): Promise<number | null> {
    const locks = webLocks();
    const userId = this.userId;
    if (!locks || !userId) return (await this.ask(after))?.count ?? null;
    const signal =
      typeof AbortSignal !== 'undefined' && typeof AbortSignal.timeout === 'function'
        ? AbortSignal.timeout(UNREAD_LOCK_WAIT_MS)
        : undefined;
    try {
      // LockManager.request resolves to the callback's value, typed any here.
      return (await locks.request(UNREAD_LOCK_NAME, signal ? { signal } : {}, async () => {
        const shared = readSharedAnswer(userId);
        if (shared && shared.askedAt > wall) return shared.count;
        const answer = await this.ask(after);
        if (!answer) return null;
        writeSharedAnswer({ userId, count: answer.count, askedAt: perfToWall(answer.startedAt) });
        return answer.count;
      })) as number | null;
    } catch {
      return (await this.ask(after))?.count ?? null;
    }
  }

  /**
   * One answer from the installed source, or the endpoint, bounded by
   * UNREAD_ASK_TIMEOUT_MS.
   */
  private async ask(after: number): Promise<UnreadCountAnswer | null> {
    const source = this.source;
    if (source) return withTimeout(source(after), UNREAD_ASK_TIMEOUT_MS);
    const startedAt = chatLoadClock();
    const count = await this.fetchCount();
    return count === null ? null : { count, startedAt };
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

  /** The endpoint's count, aborted after UNREAD_ASK_TIMEOUT_MS. */
  private async fetchCount(): Promise<number | null> {
    const abort = typeof AbortController !== 'undefined' ? new AbortController() : null;
    const request = (async (): Promise<number | null> => {
      const res = await apiFetch(CHAT_UNREAD_COUNT_URL, abort ? { signal: abort.signal } : {});
      if (!res.ok) return null;
      return unreadConversations(await res.json());
    })();
    return withTimeout(request, UNREAD_ASK_TIMEOUT_MS, () => abort?.abort());
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
  onChatRoute: boolean,
  userId?: string
): boolean {
  if (!signedIn || !chatEnabled) return false;
  counter.start(userId ? { immediate: onChatRoute, userId } : { immediate: onChatRoute });
  return true;
}

/** The page-wide unread counter. */
export const chatUnread = new ChatUnreadCounter();
