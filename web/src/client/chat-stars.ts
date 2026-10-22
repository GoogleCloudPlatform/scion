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
 * Whether a conversation supports stars. Phase 1 serves project threads
 * only; DM keys (`dm:…`) come later.
 */
export function conversationSupportsStars(conversationKey: string): boolean {
  return conversationKey !== '' && !conversationKey.startsWith('dm:');
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

/** The most message states one controller keeps. */
const MAX_TRACKED = 2000;

/**
 * Per-conversation star state of loaded messages, for the indicator and
 * the Star/Unstar toggle, plus change notification for star lists.
 *
 * - `load(ids)` fetches the state of IDs not yet known, in batches of
 *   STAR_STATE_BATCH. A failed fetch leaves them unknown (never inferred
 *   unstarred).
 * - `setStarred` sends the desired state, one request in flight per
 *   message, optimistic with a pending flag; a failure restores the prior
 *   state and rethrows.
 * - `invalidate()` (an SSE invalidation, a reconnect) refetches every
 *   known ID; an invalidation during a fetch refetches once afterwards.
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
    this.refreshAgain = false;
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
    // Only persisted messages have a UUID; an optimistic local row does not.
    const ids = [...new Set(messageIds)].filter((id) => UUID_RE.test(id) && !this.states.has(id));
    await this.fetchStates(ids);
  }

  /**
   * Refetch the state of every known message. Invalidations that arrive
   * while a refresh runs coalesce into one more refresh afterwards.
   */
  async invalidate(): Promise<void> {
    this.emit('collection-changed');
    if (this.refreshing) {
      this.refreshAgain = true;
      return;
    }
    this.refreshing = true;
    try {
      do {
        this.refreshAgain = false;
        await this.fetchStates([...this.states.keys()]);
      } while (this.refreshAgain);
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
    this.emit('change');
    try {
      if (starred) {
        await this.api.putStar(messageId, conversationKey);
      } else {
        await this.api.deleteStar(messageId);
      }
      if (generation === this.generation) this.track(messageId, starred);
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
      let starred: string[];
      try {
        starred = await this.api.fetchStarState(batch);
      } catch {
        // Unknown stays unknown: never infer unstarred from a failure.
        return;
      }
      if (generation !== this.generation) return;
      const set = new Set(starred);
      for (const id of batch) this.track(id, set.has(id));
      this.emit('change');
    }
  }

  private track(messageId: string, starred: boolean): void {
    this.states.delete(messageId);
    this.states.set(messageId, starred);
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
