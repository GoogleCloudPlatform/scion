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
 * Scroll-position memory for a chat conversation across chat page instances.
 *
 * Switching from chat to the dashboard swaps the whole chat shell out, so the
 * chat page (and its thread) is destroyed and a fresh one is built on the way
 * back. The URL brings back the conversation; this module brings back where
 * in it the user was. A position is recorded as an anchor message plus a
 * pixel offset, rather than a raw `scrollTop`, so it survives the new
 * instance loading a different window of history.
 *
 * The memory is one-shot: the page that is torn down stores its anchor, the
 * next page to mount takes it (clearing it), and only uses it if it opens the
 * same conversation. Any other destination, such as the terminal pane's
 * "open in chat" jump to an agent DM, simply ignores it.
 */

export interface ChatScrollAnchor {
  /** Conversation the anchor belongs to (topic UUID or DM key). */
  conversationKey: string;
  /** The view was following the newest message. */
  pinnedToBottom: boolean;
  /** Topmost message still visible at the scroller's top edge. */
  messageId: string;
  /** That message's top edge minus the scroller's top edge, in pixels. */
  offset: number;
}

/** A message row as seen by the anchor search. */
export interface AnchorRow {
  id: string;
  top: number;
  bottom: number;
}

/**
 * Find the topmost row whose box reaches below `containerTop`.
 *
 * Rows are in document (top-to-bottom) order, so the search is a binary
 * search on each row's bottom edge; `rowAt` is called lazily so long threads
 * only pay for a handful of layout reads per scroll frame.
 */
export function findTopVisibleRow(
  count: number,
  rowAt: (index: number) => AnchorRow,
  containerTop: number
): AnchorRow | null {
  let lo = 0;
  let hi = count - 1;
  let found: AnchorRow | null = null;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const row = rowAt(mid);
    if (row.bottom > containerTop) {
      found = row;
      hi = mid - 1;
    } else {
      lo = mid + 1;
    }
  }
  return found;
}

/**
 * The `scrollTop` that puts the anchor row's top edge `offset` pixels below
 * the scroller's top edge, given where the row sits now.
 */
export function scrollTopForAnchor(
  currentScrollTop: number,
  rowTop: number,
  containerTop: number,
  offset: number
): number {
  return currentScrollTop + (rowTop - containerTop) - offset;
}

let remembered: ChatScrollAnchor | null = null;

/** Store the anchor of a chat page that is going away (null clears it). */
export function rememberChatScrollAnchor(anchor: ChatScrollAnchor | null): void {
  remembered = anchor ? { ...anchor } : null;
}

/** Take (and clear) the stored anchor. */
export function takeChatScrollAnchor(): ChatScrollAnchor | null {
  const anchor = remembered;
  remembered = null;
  return anchor;
}
