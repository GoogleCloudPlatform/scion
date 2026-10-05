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

import { describe, it, expect, afterEach } from 'vitest';
import { horizontalScrollRoom, scrollerTakesDrag } from './horizontal-scroll.js';

/** An element with the given overflow and scroll geometry. */
function scroller(opts: {
  overflowX?: string;
  scrollWidth: number;
  clientWidth: number;
  scrollLeft?: number;
  direction?: string;
}): HTMLElement {
  const el = document.createElement('div');
  el.style.overflowX = opts.overflowX ?? 'auto';
  if (opts.direction) el.style.direction = opts.direction;
  Object.defineProperty(el, 'scrollWidth', { value: opts.scrollWidth });
  Object.defineProperty(el, 'clientWidth', { value: opts.clientWidth });
  Object.defineProperty(el, 'scrollLeft', { value: opts.scrollLeft ?? 0 });
  document.body.appendChild(el);
  return el;
}

describe('horizontalScrollRoom', () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  it('finds no room on a path without sideways scrollers', () => {
    const plain = document.createElement('div');
    document.body.appendChild(plain);
    expect(horizontalScrollRoom([plain, document.body, document, window])).toEqual({
      rightward: false,
      leftward: false,
    });
  });

  it('ignores content that overflows a box that does not scroll', () => {
    const el = scroller({ overflowX: 'hidden', scrollWidth: 800, clientWidth: 300 });
    expect(horizontalScrollRoom([el])).toEqual({ rightward: false, leftward: false });
  });

  it('ignores a scroller with nothing to scroll', () => {
    const el = scroller({ scrollWidth: 300.5, clientWidth: 300 });
    expect(horizontalScrollRoom([el])).toEqual({ rightward: false, leftward: false });
  });

  it('at its start a scroller only takes a leftward drag', () => {
    const room = horizontalScrollRoom([scroller({ scrollWidth: 800, clientWidth: 300 })]);
    expect(room).toEqual({ rightward: false, leftward: true });
    expect(scrollerTakesDrag(room, -50)).toBe(true);
    expect(scrollerTakesDrag(room, 50)).toBe(false);
  });

  it('part-way along a scroller takes both directions', () => {
    const el = scroller({ scrollWidth: 800, clientWidth: 300, scrollLeft: 200 });
    expect(horizontalScrollRoom([el])).toEqual({ rightward: true, leftward: true });
  });

  it('at its end a scroller only takes a rightward drag', () => {
    const el = scroller({ scrollWidth: 800, clientWidth: 300, scrollLeft: 499.5 });
    const room = horizontalScrollRoom([el]);
    expect(room).toEqual({ rightward: true, leftward: false });
    expect(scrollerTakesDrag(room, -50)).toBe(false);
  });

  it('mirrors the directions for a right-to-left scroller', () => {
    // At its start (offset 0, content anchored right) only a rightward drag
    // reveals more.
    const start = scroller({ scrollWidth: 800, clientWidth: 300, direction: 'rtl' });
    expect(horizontalScrollRoom([start])).toEqual({ rightward: true, leftward: false });
    const end = scroller({
      scrollWidth: 800,
      clientWidth: 300,
      scrollLeft: -500,
      direction: 'rtl',
    });
    expect(horizontalScrollRoom([end])).toEqual({ rightward: false, leftward: true });
  });

  it('combines every scroller on the path', () => {
    const inner = scroller({ scrollWidth: 800, clientWidth: 300 });
    const outer = scroller({ scrollWidth: 800, clientWidth: 300, scrollLeft: 500 });
    expect(horizontalScrollRoom([inner, outer])).toEqual({ rightward: true, leftward: true });
  });

  it('never counts the page root as a sideways scroller', () => {
    const roots = [document.documentElement, document.body];
    for (const root of roots) {
      root.style.overflowX = 'auto';
      Object.defineProperty(root, 'scrollWidth', { value: 800, configurable: true });
      Object.defineProperty(root, 'clientWidth', { value: 300, configurable: true });
      Object.defineProperty(root, 'scrollLeft', { value: 200, configurable: true });
    }
    try {
      expect(horizontalScrollRoom([document.body, document.documentElement])).toEqual({
        rightward: false,
        leftward: false,
      });
    } finally {
      for (const root of roots) {
        root.style.overflowX = '';
        Reflect.deleteProperty(root, 'scrollWidth');
        Reflect.deleteProperty(root, 'clientWidth');
        Reflect.deleteProperty(root, 'scrollLeft');
      }
    }
  });
});
