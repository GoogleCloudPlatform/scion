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
