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

/** The size classes of the graphical review page, at each limit. */

import { beforeAll, describe, expect, it } from 'vitest';

type Page = typeof import('./artifact-detail.js');
let page: Page;
beforeAll(async () => {
  const hd = (
    window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
  ).happyDOM;
  if (hd) hd.settings.disableIframePageLoading = true;
  page = await import('./artifact-detail.js');
}, 30_000);

/** text of exactly n UTF-8 bytes, one block. */
const bytes = (n: number): string => 'a'.repeat(n - 1) + '\n';
/** text of exactly n top-level blocks (short paragraphs). */
const blocks = (n: number): string => 'p\n\n'.repeat(n);

describe('review size', () => {
  it('by bytes: at and over the soft limit, at and over the ceiling', async () => {
    const { reviewSize, LARGE_REVIEW_BYTES, MAX_GRAPHICAL_BYTES } = page;
    expect(await reviewSize(bytes(LARGE_REVIEW_BYTES))).toBe('normal');
    expect(await reviewSize(bytes(LARGE_REVIEW_BYTES + 1))).toBe('large');
    expect(await reviewSize(bytes(MAX_GRAPHICAL_BYTES))).toBe('large');
    expect(await reviewSize(bytes(MAX_GRAPHICAL_BYTES + 1))).toBe('too-large');
    // Bytes, not characters: é is two.
    expect(await reviewSize('é'.repeat(LARGE_REVIEW_BYTES / 2))).toBe('normal');
    expect(await reviewSize('é'.repeat(LARGE_REVIEW_BYTES / 2) + 'a')).toBe('large');
  });

  it('by blocks: at and over the soft limit, at and over the ceiling', async () => {
    const { reviewSize, LARGE_REVIEW_BLOCKS, MAX_GRAPHICAL_BLOCKS } = page;
    expect(await reviewSize(blocks(LARGE_REVIEW_BLOCKS))).toBe('normal');
    expect(await reviewSize(blocks(LARGE_REVIEW_BLOCKS + 1))).toBe('large');
    expect(await reviewSize(blocks(MAX_GRAPHICAL_BLOCKS))).toBe('large');
    expect(await reviewSize(blocks(MAX_GRAPHICAL_BLOCKS + 1))).toBe('too-large');
  });

  it('does not count the blocks of text over the byte ceiling', async () => {
    let counted = false;
    const size = await page.reviewSize(bytes(page.MAX_GRAPHICAL_BYTES + 1), () => {
      counted = true;
      return Promise.resolve(0);
    });
    expect(size).toBe('too-large');
    expect(counted).toBe(false);
  });

  it('a count that fails counts as large (the Source view, and the reviewer can switch)', async () => {
    const fail = (): Promise<number> => Promise.reject(new Error('chunk gone'));
    expect(await page.reviewSize('small', fail)).toBe('large');
    expect(await page.reviewSize(bytes(page.MAX_GRAPHICAL_BYTES + 1), fail)).toBe('too-large');
  });
});
