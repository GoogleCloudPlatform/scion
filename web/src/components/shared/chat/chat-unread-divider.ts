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
 * Where a thread's "New messages" divider goes.
 *
 * The divider follows the reader's last-read message. When that message is
 * not among the loaded messages but the thread shows the retention marker
 * (the oldest page is loaded and the hub's retention policy removed what
 * came before), the watermark was purged: every message left is newer than
 * it, so the divider falls at the top, under the retention marker (design
 * chat message retention §8.5). Without the marker a missing watermark may
 * just be on an older, unloaded page, so no divider is shown.
 *
 * Returns the index of the message the divider follows, -1 for the top of
 * the thread, or null for no divider.
 */
export function unreadDividerAfter(
  messageIds: readonly string[],
  lastReadMessageId: string,
  historyPurged: boolean
): number | null {
  if (!lastReadMessageId) return null;
  const idx = messageIds.indexOf(lastReadMessageId);
  if (idx >= 0) return idx;
  return historyPurged && messageIds.length > 0 ? -1 : null;
}

/** Whether a divider at position `after` has unread messages below it. */
export function hasMessagesAfter(after: number | null, messageCount: number): boolean {
  return after !== null && after < messageCount - 1;
}
