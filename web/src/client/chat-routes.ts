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
