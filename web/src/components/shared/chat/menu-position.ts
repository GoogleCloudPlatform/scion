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
 * Viewport clamping for the chat's custom right-click menus.
 *
 * Pure geometry, so it can be unit-tested without a DOM: given where the
 * pointer opened the menu, the menu's measured size and the viewport size,
 * return the top-left corner that keeps the whole menu on screen.
 */

export interface Point {
  x: number;
  y: number;
}

export interface Size {
  w: number;
  h: number;
}

/**
 * Place a menu of `size` at `anchor`, inside `viewport` with a `margin` gap.
 *
 * On each axis the menu opens toward the bottom-right of the anchor, as it
 * always has. If that would overflow and the other side of the anchor has
 * room, it flips there (left or up). The result is then clamped to
 * `[margin, viewport - size - margin]`, so a menu that fits on neither side
 * still lands fully on screen, as close to the anchor as it can. A menu
 * larger than the viewport pins to the top-left margin, so its start (the
 * first items) stays visible.
 */
export function clampMenuToViewport(anchor: Point, size: Size, viewport: Size, margin = 8): Point {
  return {
    x: clampAxis(anchor.x, size.w, viewport.w, margin),
    y: clampAxis(anchor.y, size.h, viewport.h, margin),
  };
}

function clampAxis(anchor: number, size: number, viewport: number, margin: number): number {
  let start = anchor;
  if (start + size + margin > viewport && anchor - size >= margin) start = anchor - size;
  const max = viewport - size - margin;
  if (start > max) start = max;
  if (start < margin) start = margin;
  return start;
}
