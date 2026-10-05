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

import { describe, expect, it } from 'vitest';

import { clampMenuToViewport } from './menu-position.js';

const VIEWPORT = { w: 1440, h: 900 };
const MENU = { w: 200, h: 300 };

describe('clampMenuToViewport', () => {
  it('opens at the anchor when the menu fits below and to the right', () => {
    expect(clampMenuToViewport({ x: 100, y: 120 }, MENU, VIEWPORT)).toEqual({ x: 100, y: 120 });
  });

  it('flips left when the menu would overflow the right edge', () => {
    expect(clampMenuToViewport({ x: 1400, y: 120 }, MENU, VIEWPORT)).toEqual({ x: 1200, y: 120 });
  });

  it('flips up when the menu would overflow the bottom edge', () => {
    expect(clampMenuToViewport({ x: 100, y: 850 }, MENU, VIEWPORT)).toEqual({ x: 100, y: 550 });
  });

  it('flips both ways at the bottom-right corner', () => {
    // Flipped to (1235, 595), then pulled in so the margin stays clear.
    const pos = clampMenuToViewport({ x: 1435, y: 895 }, MENU, VIEWPORT);
    expect(pos).toEqual({ x: 1232, y: 592 });
    expect(pos.x + MENU.w).toBeLessThanOrEqual(VIEWPORT.w);
    expect(pos.y + MENU.h).toBeLessThanOrEqual(VIEWPORT.h);
  });

  it('respects the margin when the anchor sits right at the edge', () => {
    // Fits exactly up to the edge, but not with the margin, so it flips.
    expect(clampMenuToViewport({ x: 1240, y: 10 }, MENU, VIEWPORT)).toEqual({ x: 1040, y: 10 });
    expect(clampMenuToViewport({ x: 1232, y: 10 }, MENU, VIEWPORT)).toEqual({ x: 1232, y: 10 });
  });

  it('clamps into the viewport when the menu fits on neither side of the anchor', () => {
    // 300px tall in a 400px viewport, anchored mid-way: flipping up would
    // start above the top, so it clamps to the margin instead.
    expect(clampMenuToViewport({ x: 10, y: 200 }, MENU, { w: 800, h: 400 })).toEqual({
      x: 10,
      y: 92,
    });
  });

  it('pins an oversized menu to the top-left margin', () => {
    expect(clampMenuToViewport({ x: 50, y: 50 }, { w: 500, h: 500 }, { w: 320, h: 400 })).toEqual({
      x: 8,
      y: 8,
    });
  });

  it('accepts a custom margin', () => {
    expect(clampMenuToViewport({ x: 0, y: 0 }, MENU, VIEWPORT, 0)).toEqual({ x: 0, y: 0 });
    expect(clampMenuToViewport({ x: 0, y: 0 }, MENU, VIEWPORT, 16)).toEqual({ x: 16, y: 16 });
  });
});
