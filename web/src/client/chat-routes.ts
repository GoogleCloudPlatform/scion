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
 * Chat conversation routes.
 *
 * These patterns are the ones registered in the router's route table
 * (`main.ts` ROUTES) — they are imported from here rather than written out
 * twice, so a path built by `chatConversationPath` can be checked against the
 * exact object the router matches against instead of against a copy of it
 * that can drift.
 */

/** `/chat/space/{projectId}/thread/{topicId}` */
export const CHAT_THREAD_ROUTE = /^\/chat\/space\/[^/]+\/thread\/[^/]+$/;

/** `/chat/space/{projectId}` */
export const CHAT_SPACE_ROUTE = /^\/chat\/space\/[^/]+$/;

/** `/chat/dm/{conversationKeyOrPeerId}` */
export const CHAT_DM_ROUTE = /^\/chat\/dm\/[^/]+$/;

/** A conversation a notification can point at. */
export interface ChatConversationTarget {
  /** Topic UUID for a space thread, or `dm:<kind>:<id>:<kind>:<id>` for a DM. */
  conversationKey: string;
  /** Owning project; absent for DMs. */
  projectId?: string;
}

/**
 * Builds the DM conversation key for a direct conversation between
 * a user and an agent. Returns null if either ID is missing.
 */
export function buildAgentDMKey(agentId: string, userId: string): string | null {
  if (!agentId?.trim() || !userId?.trim()) return null;
  return `dm:agent:${agentId}:user:${userId}`;
}

/** The other participant of a DM, as its conversation key names it. */
export interface DMKeyPeer {
  peerId: string;
  peerKind: 'user' | 'agent';
}

/**
 * The peer a DM conversation key names, seen from the current user: the
 * agent in an agent DM, the other user in a user DM (the user themselves in
 * a DM with themselves). Null for a key that is not a two-party DM key, and
 * for a user DM when the current user is not known or is not a participant.
 */
export function dmPeerFromKey(key: string, currentUserId: string): DMKeyPeer | null {
  const parts = key.split(':');
  if (parts.length !== 5 || parts[0] !== 'dm') return null;
  const sides: DMKeyPeer[] = [];
  for (const [kind, id] of [
    [parts[1], parts[2]],
    [parts[3], parts[4]],
  ]) {
    if ((kind !== 'user' && kind !== 'agent') || !id) return null;
    sides.push({ peerId: id, peerKind: kind });
  }
  const agent = sides.find((s) => s.peerKind === 'agent');
  if (agent) return agent;
  if (!currentUserId) return null;
  const [a, b] = sides;
  if (a.peerId === currentUserId) return b;
  if (b.peerId === currentUserId) return a;
  return null;
}

/**
 * Builds the deep link for a conversation, or null when the event did not
 * carry enough to address one (a thread with no project has no route).
 *
 * DM keys contain colons, which `encodeURIComponent` escapes — the encoded
 * segment therefore never contains a slash and stays a single path segment.
 */
export function chatConversationPath(target: ChatConversationTarget): string | null {
  if (!target) return null;
  const key = target.conversationKey?.trim();
  if (!key) return null;

  if (key.startsWith('dm:')) {
    return `/chat/dm/${encodeURIComponent(key)}`;
  }

  const projectId = target.projectId?.trim();
  if (!projectId) return null;

  return `/chat/space/${encodeURIComponent(projectId)}/thread/${encodeURIComponent(key)}`;
}

/** Parsed components of a native group conversation's `thread:<projectId>:<topicId>` ref. */
export interface ParsedThreadExternalRef {
  projectId: string;
  topicId: string;
}

/**
 * Parses a native thread conversation `externalRef` (`thread:<projectId>:<topicId>`),
 * mirroring `ParseThreadConversationExternalRef` in `pkg/messaging/derive_key.go`.
 * Returns null for non-thread refs, missing parts, or a `dm:`-prefixed topic ID.
 */
export function parseThreadConversationExternalRef(ref?: string): ParsedThreadExternalRef | null {
  if (!ref || !ref.startsWith('thread:')) return null;
  const rest = ref.slice('thread:'.length);
  const sep = rest.indexOf(':');
  if (sep <= 0) return null;
  const projectId = rest.slice(0, sep);
  const topicId = rest.slice(sep + 1);
  if (!projectId || !topicId || topicId.startsWith('dm:')) return null;
  return { projectId, topicId };
}

/** Minimal conversation shape needed to build a `/chat/...` link. */
export interface ConversationRouteInput {
  kind: string;
  surface?: string;
  externalRef?: string;
  projectId?: string;
}

/**
 * Builds the `/chat/...` deep link for a conversation returned by
 * `GET /api/v1/conversations`, or null when the conversation has no native
 * chat route (for example, external-surface or unlinked group conversations).
 */
export function conversationChatPath(conv: ConversationRouteInput): string | null {
  if (!conv || conv.surface !== 'native') return null;

  if (conv.kind === 'direct') {
    const key = conv.externalRef?.trim();
    if (!key || !key.startsWith('dm:')) return null;
    return chatConversationPath({ conversationKey: key });
  }

  if (conv.kind === 'group') {
    const parsed = parseThreadConversationExternalRef(conv.externalRef);
    if (!parsed) return null;
    return chatConversationPath({
      conversationKey: parsed.topicId,
      projectId: parsed.projectId,
    });
  }

  return null;
}

/**
 * The center message filter of a conversation route: `starred` shows only
 * the signed-in user's starred messages (`?messages=starred`), '' the whole
 * history.
 */
export type ChatMessageFilter = '' | 'starred';

/** The query parameter carrying the center message filter. */
export const CHAT_MESSAGE_FILTER_PARAM = 'messages';

/** The message filter a URL query names ('' for none or an unknown value). */
export function messageFilterFromSearch(search: string): ChatMessageFilter {
  return new URLSearchParams(search).get(CHAT_MESSAGE_FILTER_PARAM) === 'starred' ? 'starred' : '';
}

/**
 * Whether an app path (no query or fragment) names one conversation: a
 * legacy thread route, a readable `/chat/<slug>/<thread-id>` route or a DM
 * route. Only these carry a message filter.
 */
export function isChatConversationPath(path: string): boolean {
  // `/chat/space/{projectId}` is a space; the router matches it before the
  // readable two-segment thread route, which it would otherwise look like.
  if (CHAT_SPACE_ROUTE.test(path)) return false;
  return (
    CHAT_THREAD_ROUTE.test(path) || CHAT_DM_ROUTE.test(path) || /^\/chat\/[^/]+\/[^/]+$/.test(path)
  );
}

/**
 * `search` with the message filter set to `filter` (removed for ''), every
 * other parameter kept. Returns '' or a string starting with '?'.
 */
export function searchWithMessageFilter(search: string, filter: ChatMessageFilter): string {
  const params = new URLSearchParams(search);
  if (filter) params.set(CHAT_MESSAGE_FILTER_PARAM, filter);
  else params.delete(CHAT_MESSAGE_FILTER_PARAM);
  const q = params.toString();
  return q ? `?${q}` : '';
}

/** localStorage key of the selected chat rail filter. */
export const RAIL_FILTER_STORAGE_KEY = 'scion-chat-space-filter';

/** The query parameter carrying a one-shot rail filter hydration target. */
export const CHAT_RAIL_FILTER_PARAM = 'filter';

/** Valid URL-hydratable rail filter values (design §7.1, §14.4). */
export type ChatUrlRailFilter = 'unread' | 'mentions' | 'starred';

/**
 * The rail filter a URL query names (`?filter=unread|mentions|starred`), or
 * null when absent or invalid.
 */
export function railFilterFromSearch(search: string): ChatUrlRailFilter | null {
  const raw = new URLSearchParams(search).get(CHAT_RAIL_FILTER_PARAM);
  if (raw === 'unread' || raw === 'mentions' || raw === 'starred') {
    return raw;
  }
  return null;
}

export interface HydrateRailFilterDeps {
  search: string;
  replaceSearch: (params: URLSearchParams) => void;
  persistFilter: (filter: ChatUrlRailFilter) => void;
  applyFilter: (filter: ChatUrlRailFilter) => void;
}

/**
 * One-shot hydration of `?filter=unread|mentions|starred` on chat routes
 * (design §7.1, §14.4). Always strips `filter` via `replaceSearch` (preserving
 * other query params). Invalid values are ignored without persisting or
 * changing the rail.
 */
export function hydrateRailFilterFromSearch(deps: HydrateRailFilterDeps): ChatUrlRailFilter | null {
  const params = new URLSearchParams(deps.search);
  if (!params.has(CHAT_RAIL_FILTER_PARAM)) return null;
  const requested = railFilterFromSearch(deps.search);
  params.delete(CHAT_RAIL_FILTER_PARAM);
  deps.replaceSearch(params);
  if (!requested) return null;
  deps.persistFilter(requested);
  deps.applyFilter(requested);
  return requested;
}
