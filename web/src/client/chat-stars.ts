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
 * Starred messages in native chat (web.chat_starred_messages): the hub API
 * for the signed-in user's private stars, and the in-memory controller the
 * chat thread and the conversation header share.
 *
 * REST is authoritative. The hub's `user.<id>.chat.stars` SSE event is a
 * content-free invalidation (`chat-stars-updated`): it only makes the
 * controller refetch. Nothing here is persisted to localStorage.
 */

import { apiFetch, extractApiError } from './api.js';
import { isFeatureEnabled } from '../utils/feature-flags.js';

/** The experiment gating starred messages (web and hub). */
export const CHAT_STARRED_MESSAGES_EXPERIMENT = 'web.chat_starred_messages';

/** Whether starred messages are offered in this browser session. */
export function starredMessagesEnabled(): boolean {
  return isFeatureEnabled(CHAT_STARRED_MESSAGES_EXPERIMENT);
}

/**
 * Whether a conversation supports stars: project threads, agent DMs and
 * DMs between users all do; only a missing key does not.
 */
export function conversationSupportsStars(conversationKey: string): boolean {
  return conversationKey !== '';
}

/** The largest number of message IDs one state request may carry. */
export const STAR_STATE_BATCH = 200;

/** One starred message as the hub returns it. Times are UTC ISO instants. */
export interface StarItem {
  messageId: string;
  conversationKey: string;
  conversationKind: 'thread' | 'dm';
  projectId?: string;
  projectName?: string;
  conversationName: string;
  senderName: string;
  messageCreatedAt: string;
  starredAt: string;
  /** Plain text, at most 160 code points; never markup. */
  excerpt: string;
  edited: boolean;
  /** Relative, same-origin link to the message. */
  permalink: string;
}

/** One page of a star list. */
export interface StarPage {
  items: StarItem[];
  nextCursor?: string;
}

/** Thrown by the API helpers with the hub's error message and status. */
export class StarsError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'StarsError';
  }
}

function starPath(messageId: string): string {
  return `/api/v1/chat/stars/${encodeURIComponent(messageId)}`;
}

/** Star a message in a conversation; returns the (kept) save time. */
export async function putStar(messageId: string, conversationKey: string): Promise<string> {
  const res = await apiFetch(starPath(messageId), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ conversationKey }),
  });
  if (!res.ok) {
    throw new StarsError(await extractApiError(res, 'Could not star message'), res.status);
  }
  const body = (await res.json()) as { starred: boolean; starredAt: string };
  return body.starredAt;
}

/** Remove the caller's star of a message (idempotent). */
export async function deleteStar(messageId: string): Promise<void> {
  const res = await apiFetch(starPath(messageId), { method: 'DELETE' });
  if (!res.ok) {
    throw new StarsError(await extractApiError(res, 'Could not unstar message'), res.status);
  }
}

/** Which of `messageIds` (at most STAR_STATE_BATCH) the caller starred. */
export async function fetchStarState(messageIds: readonly string[]): Promise<string[]> {
  const q = new URLSearchParams();
  for (const id of messageIds) q.append('messageId', id);
  const res = await apiFetch(`/api/v1/chat/stars/state?${q.toString()}`);
  if (!res.ok) {
    throw new StarsError(await extractApiError(res, 'Could not load starred messages'), res.status);
  }
  const body = (await res.json()) as { starredMessageIds?: string[] };
  return body.starredMessageIds ?? [];
}

/** One page of a conversation's stars, most recently starred first. */
export async function fetchStarPage(
  conversationKey: string,
  limit: number,
  cursor?: string,
  signal?: AbortSignal
): Promise<StarPage> {
  const q = new URLSearchParams({ conversationKey, limit: String(limit) });
  if (cursor) q.set('cursor', cursor);
  const res = await apiFetch(`/api/v1/chat/stars?${q.toString()}`, signal ? { signal } : undefined);
  if (!res.ok) {
    throw new StarsError(await extractApiError(res, 'Could not load starred messages'), res.status);
  }
  const body = (await res.json()) as StarPage;
  const page: StarPage = { items: body.items ?? [] };
  if (body.nextCursor) page.nextCursor = body.nextCursor;
  return page;
}

/**
 * The current detail of one of the caller's stars, or null when it is no
 * longer available (unstarred, deleted or unreadable: the hub's shared 404).
 */
export async function fetchStarDetail(messageId: string): Promise<StarItem | null> {
  const res = await apiFetch(starPath(messageId));
  if (res.status === 404) return null;
  if (!res.ok) {
    throw new StarsError(await extractApiError(res, 'Could not open starred message'), res.status);
  }
  return (await res.json()) as StarItem;
}

/** A message's star state as the controller knows it. */
export type StarState = 'starred' | 'unstarred' | 'unknown';

/** The API the controller uses; tests replace it. */
export interface StarsApi {
  putStar: typeof putStar;
  deleteStar: typeof deleteStar;
  fetchStarState: typeof fetchStarState;
}

const defaultApi: StarsApi = { putStar, deleteStar, fetchStarState };

/**
 * Local star mutations of this tab, for star lists outside the thread
 * that made them (the conversation header). Each `changed` event's detail
 * is `{ conversationKey }`. Other tabs learn through the SSE invalidation.
 */
export const chatStarsLocalChanges = new EventTarget();

/** A canonical message UUID (the only IDs the hub can star). */
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Whether a message can be starred: only persisted messages have a
 * canonical UUID; an optimistic local row does not have one yet.
 */
export function isStarrableMessageId(messageId: string): boolean {
  return UUID_RE.test(messageId);
}

/** The most message states one controller keeps. */
const MAX_TRACKED = 2000;

/**
 * Per-conversation star state of loaded messages, for the indicator and
 * the Star/Unstar toggle, plus change notification for star lists.
 *
 * - `load(ids)` fetches the state of IDs not yet known (one history
 *   page), in batches of STAR_STATE_BATCH. A failed batch leaves its IDs
 *   unknown (never inferred unstarred), the other batches still run, and
 *   the failed IDs are retried by the next `invalidate()` or
 *   `retryFailed()`.
 * - `markLive(ids)` records messages that just arrived live (an SSE
 *   append, a sent message's server ID) as unstarred without a request:
 *   a new message has no star yet, and a star made elsewhere publishes an
 *   invalidation that corrects it.
 * - `setStarred` sends the desired state, one request in flight per
 *   message, optimistic with a pending flag; a failure restores the prior
 *   state and rethrows.
 * - `invalidate()` (an SSE invalidation, a reconnect) refetches every
 *   known ID. Any state request in flight when an invalidation arrives
 *   (a refresh or a `load`) may have read older data: its answer is not
 *   applied, and its IDs are refetched once afterwards.
 * - A state answer never replaces a local mutation: IDs with a mutation
 *   pending, or started or confirmed after the request began, keep the
 *   mutation's result.
 * - A generation counter drops responses for a previous conversation.
 *
 * Events: `change` whenever a state or pending flag changes;
 * `collection-changed` when the stored collection may have changed (a
 * local mutation succeeded or an invalidation arrived), so lists refetch.
 */
export class ChatStarsController extends EventTarget {
  private conversationKey = '';
  private generation = 0;
  private readonly states = new Map<string, boolean>();
  private readonly pending = new Map<string, boolean>();
  private refreshing = false;
  private refreshAgain = false;
  /** Bumped by every invalidation; a request that saw a bump is stale. */
  private invalidations = 0;
  /** The invalidation count the known states are current with. */
  private appliedInvalidations = 0;
  /** IDs whose state answer was stale, refetched by the next refresh. */
  private readonly stale = new Set<string>();
  /** IDs whose state request failed, retried by the next refresh. */
  private readonly failed = new Set<string>();
  /** Bumped when a local mutation starts and when it is confirmed. */
  private mutationSeq = 0;
  /** The last `mutationSeq` of each locally mutated message. */
  private readonly mutatedAt = new Map<string, number>();
  /** State requests in flight; `mutatedAt` is only needed while > 0. */
  private inflight = 0;
  /** Messages deleted while tracked: never starred again (bounded). */
  private readonly deleted = new Set<string>();

  constructor(private readonly api: StarsApi = defaultApi) {
    super();
  }

  /** The conversation whose messages are tracked; switching clears state. */
  setConversation(conversationKey: string): void {
    if (conversationKey === this.conversationKey) return;
    this.conversationKey = conversationKey;
    this.clear();
  }

  /** Forget every state (conversation switch, sign-out, flag off). */
  clear(): void {
    this.generation++;
    this.states.clear();
    this.pending.clear();
    this.stale.clear();
    this.failed.clear();
    this.mutatedAt.clear();
    this.deleted.clear();
    this.refreshAgain = false;
    this.appliedInvalidations = this.invalidations;
    this.emit('change');
  }

  /** The state of a message, with a pending mutation's desired state. */
  state(messageId: string): StarState {
    const desired = this.pending.get(messageId);
    if (desired !== undefined) return desired ? 'starred' : 'unstarred';
    const known = this.states.get(messageId);
    if (known === undefined) return 'unknown';
    return known ? 'starred' : 'unstarred';
  }

  isPending(messageId: string): boolean {
    return this.pending.has(messageId);
  }

  /** Fetch the state of the IDs not yet known (one loaded history page). */
  async load(messageIds: readonly string[]): Promise<void> {
    const ids = [...new Set(messageIds)].filter(
      (id) => isStarrableMessageId(id) && !this.states.has(id)
    );
    await this.fetchStates(ids);
  }

  /**
   * Record messages that arrived live as unstarred, without a request.
   * Known, pending and unstarrable IDs are left alone, and so are IDs
   * whose state request failed or is due again (never inferred
   * unstarred). While a refresh runs, or after an invalidation the known
   * states have not caught up with (the star may have been made in
   * another tab just before the message event arrived), the IDs are
   * fetched instead.
   */
  markLive(messageIds: readonly string[]): void {
    let changed = false;
    let fetch = false;
    const behind = this.refreshing || this.invalidations !== this.appliedInvalidations;
    for (const id of messageIds) {
      if (!isStarrableMessageId(id) || this.states.has(id) || this.pending.has(id)) continue;
      if (this.failed.has(id) || this.stale.has(id)) continue;
      if (behind) {
        this.stale.add(id);
        fetch = true;
        continue;
      }
      this.track(id, false);
      changed = true;
    }
    if (changed) this.emit('change');
    if (fetch && !this.refreshing) void this.refresh();
  }

  /**
   * Record messages a starred-only history page returned as starred,
   * without a state request: that page already confirms it. Pending
   * mutations and deleted messages are left alone.
   */
  markStarred(messageIds: readonly string[]): void {
    let changed = false;
    for (const id of messageIds) {
      if (!isStarrableMessageId(id) || this.pending.has(id) || this.deleted.has(id)) continue;
      if (this.states.get(id) === true) continue;
      this.stale.delete(id);
      this.failed.delete(id);
      this.track(id, true);
      changed = true;
    }
    if (changed) this.emit('change');
  }

  /**
   * A message was deleted: it is unstarred everywhere at once (the hub
   * removed every star of it). Its known state becomes unstarred, a pending
   * mutation is dropped, and lists are told to refetch. A later state
   * answer cannot restore it: every read path leaves deleted messages out.
   */
  evict(messageId: string): void {
    if (!isStarrableMessageId(messageId)) return;
    const known = this.states.get(messageId);
    const wasPending = this.pending.delete(messageId);
    this.stale.delete(messageId);
    this.failed.delete(messageId);
    this.deleted.delete(messageId);
    this.deleted.add(messageId);
    for (const oldest of this.deleted) {
      if (this.deleted.size <= MAX_TRACKED) break;
      this.deleted.delete(oldest);
    }
    this.markMutated(messageId);
    this.track(messageId, false);
    if (known !== false || wasPending) this.emit('change');
    if (known === true) this.emit('collection-changed');
  }

  /** Refetch the IDs whose state request failed (a menu open), if any. */
  async retryFailed(): Promise<void> {
    if (this.failed.size === 0) return;
    await this.refresh();
  }

  /**
   * Refetch the state of every known message. Invalidations that arrive
   * while a refresh runs coalesce into one more refresh afterwards.
   */
  async invalidate(): Promise<void> {
    this.invalidations++;
    this.emit('collection-changed');
    await this.refresh();
  }

  private async refresh(): Promise<void> {
    if (this.refreshing) {
      this.refreshAgain = true;
      return;
    }
    this.refreshing = true;
    try {
      // The first pass, and a pass after another invalidation, refetch every
      // known state; a pass that only stale or failed IDs asked for
      // refetches just those.
      let full = true;
      do {
        this.refreshAgain = false;
        const ids = full
          ? new Set([...this.states.keys(), ...this.stale, ...this.failed])
          : new Set([...this.stale, ...this.failed]);
        this.stale.clear();
        this.failed.clear();
        await this.fetchStates([...ids]);
        full = this.refreshAgain;
      } while (this.refreshAgain || this.stale.size > 0);
      // The loop ends only after a pass with no new invalidation, so the
      // known states caught up with the last one if every request of the
      // refresh succeeded.
      if (this.failed.size === 0) this.appliedInvalidations = this.invalidations;
    } finally {
      this.refreshing = false;
    }
  }

  /**
   * Star or unstar a message. Ignored while a request for the same message
   * is in flight. Throws the request's error after restoring the prior
   * state; the caller reports it.
   */
  async setStarred(messageId: string, conversationKey: string, starred: boolean): Promise<void> {
    if (this.pending.has(messageId)) return;
    const generation = this.generation;
    this.pending.set(messageId, starred);
    this.markMutated(messageId);
    this.emit('change');
    try {
      if (starred) {
        await this.api.putStar(messageId, conversationKey);
      } else {
        await this.api.deleteStar(messageId);
      }
      if (generation === this.generation) {
        this.markMutated(messageId);
        this.track(messageId, starred);
      }
    } finally {
      if (generation === this.generation) {
        this.pending.delete(messageId);
        this.emit('change');
      }
    }
    this.emit('collection-changed');
    chatStarsLocalChanges.dispatchEvent(
      new CustomEvent('changed', { detail: { conversationKey } })
    );
  }

  private async fetchStates(ids: readonly string[]): Promise<void> {
    if (ids.length === 0) return;
    const generation = this.generation;
    for (let i = 0; i < ids.length; i += STAR_STATE_BATCH) {
      const batch = ids.slice(i, i + STAR_STATE_BATCH);
      const invalidations = this.invalidations;
      const mutationSeq = this.mutationSeq;
      let starred: string[];
      this.inflight++;
      try {
        starred = await this.api.fetchStarState(batch);
      } catch {
        // Unknown stays unknown: never infer unstarred from a failure. The
        // other batches still run; these IDs are retried by the next
        // refresh (an invalidation, a reconnect, a menu open).
        this.inflight--;
        if (generation !== this.generation) return;
        for (const id of batch) this.failed.add(id);
        continue;
      }
      this.inflight--;
      if (generation !== this.generation) return;
      if (invalidations !== this.invalidations) {
        // An invalidation arrived while this request ran: its answer may
        // predate the change, so refetch these IDs instead of applying it.
        for (const id of batch) this.stale.add(id);
        continue;
      }
      const set = new Set(starred);
      for (const id of batch) {
        if (this.pending.has(id) || (this.mutatedAt.get(id) ?? 0) > mutationSeq) continue;
        this.track(id, set.has(id));
      }
      if (this.inflight === 0) this.mutatedAt.clear();
      this.emit('change');
    }
    // A `load` that came back stale outside a refresh starts one; inside a
    // refresh, its loop picks the stale IDs up.
    if (this.stale.size > 0 && !this.refreshing) await this.refresh();
  }

  private markMutated(messageId: string): void {
    this.mutatedAt.set(messageId, ++this.mutationSeq);
  }

  private track(messageId: string, starred: boolean): void {
    this.states.delete(messageId);
    // A deleted message has no star, whatever a request that raced the
    // deletion answered.
    this.states.set(messageId, starred && !this.deleted.has(messageId));
    if (this.states.size <= MAX_TRACKED) return;
    // Maps iterate in insertion order: drop the least recently tracked.
    for (const oldest of this.states.keys()) {
      if (this.states.size <= MAX_TRACKED) break;
      this.states.delete(oldest);
    }
  }

  private emit(type: 'change' | 'collection-changed'): void {
    this.dispatchEvent(new Event(type));
  }
}

/** A visible star collection surface reconciles at most this often. */
export const STAR_SURFACE_RECONCILE_MS = 60_000;

/**
 * Two return signals closer than this (a tab switch fires both
 * `visibilitychange` and the window's `focus`) refetch once.
 */
const RETURN_DEDUPE_MS = 2_000;

/**
 * Keeps a visible star collection surface (the header list while open,
 * the starred-only center) in step when invalidations were missed:
 *
 * - while started and the document is visible, `run` is called once
 *   `intervalMs` has passed since the last refetch, whatever caused it
 *   (a null interval: no polling, only the return below);
 * - returning to the page (visibility or window focus) calls `run` at once;
 * - nothing runs while the document is hidden.
 *
 * The surface calls `noteRefetched()` whenever it refetched for another
 * reason (an SSE invalidation, a reconnect, opening), which restarts the
 * interval, so polls never add to that traffic.
 */
export class VisibleReconciler {
  private timer: ReturnType<typeof setTimeout> | null = null;
  private last = 0;
  private active = false;

  constructor(
    private readonly run: () => void,
    private readonly intervalMs: number | null = STAR_SURFACE_RECONCILE_MS,
    private readonly now: () => number = () => Date.now()
  ) {}

  get started(): boolean {
    return this.active;
  }

  start(): void {
    if (this.active) return;
    this.active = true;
    this.last = this.now();
    document.addEventListener('visibilitychange', this.handleReturn);
    window.addEventListener('focus', this.handleReturn);
    this.schedule();
  }

  stop(): void {
    if (!this.active) return;
    this.active = false;
    document.removeEventListener('visibilitychange', this.handleReturn);
    window.removeEventListener('focus', this.handleReturn);
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }

  /** The surface refetched for another reason: restart the interval. */
  noteRefetched(): void {
    if (!this.active) return;
    this.last = this.now();
    this.schedule();
  }

  private schedule(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    if (this.intervalMs === null) return;
    const wait = Math.max(0, this.last + this.intervalMs - this.now());
    this.timer = setTimeout(this.tick, wait);
  }

  private tick = (): void => {
    this.timer = null;
    if (!this.active) return;
    // Hidden: no poll, and no timer either; the return to the page refetches.
    if (document.visibilityState === 'visible') this.fire();
  };

  private handleReturn = (): void => {
    if (!this.active || document.visibilityState !== 'visible') return;
    if (this.now() - this.last < RETURN_DEDUPE_MS) return;
    this.fire();
  };

  private fire(): void {
    this.last = this.now();
    this.schedule();
    this.run();
  }
}
