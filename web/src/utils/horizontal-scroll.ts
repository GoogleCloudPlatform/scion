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
 * Which way the horizontal scrollers under a touch could still scroll when
 * the touch began. A sideways drag that one of them can absorb belongs to
 * that scroller (a code block, a wide table), not to a page-level swipe.
 */
export interface HorizontalScrollRoom {
  /** Some scroller under the touch could absorb a rightward drag. */
  rightward: boolean;
  /** Some scroller under the touch could absorb a leftward drag. */
  leftward: boolean;
}

/** Sub-pixel slack, so a scroller resting a fraction short of its end counts as at the end. */
const EDGE_SLACK_PX = 1;

/**
 * Measure the horizontal scroll room of every element on an event's
 * composed path (so scrollers inside shadow roots count) that scrolls
 * sideways: `overflow-x` of `auto` or `scroll` with content wider than its
 * box. The `html` and `body` elements are skipped. Right-to-left scrollers
 * are handled: their offset runs negative.
 */
export function horizontalScrollRoom(path: readonly EventTarget[]): HorizontalScrollRoom {
  const room: HorizontalScrollRoom = { rightward: false, leftward: false };
  for (const target of path) {
    // The page root never claims a drag: page-level scrolling is not a
    // scroller under the touch.
    if (!(target instanceof Element) || target.tagName === 'HTML' || target.tagName === 'BODY') {
      continue;
    }
    const max = target.scrollWidth - target.clientWidth;
    if (max <= EDGE_SLACK_PX) continue;
    const style = getComputedStyle(target);
    if (style.overflowX !== 'auto' && style.overflowX !== 'scroll') continue;
    // scrollLeft runs 0..max left-to-right, and 0..-max right-to-left.
    const offset = Math.abs(target.scrollLeft);
    const canBack = offset > EDGE_SLACK_PX;
    const canForward = offset < max - EDGE_SLACK_PX;
    const rtl = style.direction === 'rtl';
    // A rightward drag moves the content right, revealing what is to the
    // left: towards the start in left-to-right text, the end in right-to-left.
    if (rtl ? canForward : canBack) room.rightward = true;
    if (rtl ? canBack : canForward) room.leftward = true;
  }
  return room;
}

/**
 * Whether a horizontal drag of `dx` px (positive = rightward) could still
 * be absorbed by a scroller under the touch.
 */
export function scrollerTakesDrag(room: HorizontalScrollRoom, dx: number): boolean {
  return dx > 0 ? room.rightward : room.leftward;
}
