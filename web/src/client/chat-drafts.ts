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
 * Chat composer draft storage, for surfaces outside the chat page.
 *
 * The chat composer persists the unsent text of each conversation in
 * localStorage under `scion-chat-draft-<conversationKey>` and restores it
 * when that conversation's composer mounts. Seeding that entry before
 * navigating to a conversation hands text typed elsewhere (e.g. the quick
 * message dialog) to the composer without a new hand-off channel.
 */

const DRAFT_KEY_PREFIX = 'scion-chat-draft-';

/** The localStorage key the chat composer uses for a conversation's draft. */
export function chatDraftStorageKey(conversationKey: string): string {
  return `${DRAFT_KEY_PREFIX}${conversationKey}`;
}

/**
 * Adds text to a conversation's stored draft. An existing draft is kept and
 * the new text is appended on a new paragraph, so neither is lost. Empty or
 * whitespace-only text is ignored. Returns whether the draft was written.
 */
export function seedChatDraft(conversationKey: string, text: string): boolean {
  if (!conversationKey || !text.trim()) return false;
  try {
    const key = chatDraftStorageKey(conversationKey);
    const existing = localStorage.getItem(key);
    const next = existing?.trim() ? `${existing.trimEnd()}\n\n${text}` : text;
    localStorage.setItem(key, next);
    return true;
  } catch {
    // localStorage may throw in private browsing mode.
    return false;
  }
}
