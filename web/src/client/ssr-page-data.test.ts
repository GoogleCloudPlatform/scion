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

import type { PageData } from '../shared/types.js';
import {
  initialPageDataFor,
  MAX_SSR_PAGE_DATA_AGE_MS,
  setInitialPageData,
  takeInitialPageData,
  type DocumentTiming,
} from './ssr-page-data.js';

const user = { id: 'u-1', email: 'u@example.com', name: 'U' };
const payload: PageData = {
  path: '/projects/p-1',
  title: 'Scion',
  user,
  data: { id: 'p-1', name: 'Project One' },
};
const fresh: DocumentTiming = { msSinceNavigationStart: 100, navigationType: 'navigate' };
const at = (ms: number, navigationType: string | null = 'navigate'): DocumentTiming => ({
  msSinceNavigationStart: ms,
  navigationType,
});

describe('initialPageDataFor', () => {
  it('hands over the payload for the same path and user on a young document', () => {
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, fresh)).toBe(payload.data);
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(100, 'reload'))).toBe(
      payload.data
    );
  });

  it('returns nothing without a payload or without data', () => {
    expect(initialPageDataFor(null, '/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
    expect(
      initialPageDataFor({ ...payload, data: undefined }, '/projects/p-1', { id: 'u-1' }, fresh)
    ).toBeUndefined();
  });

  it('returns nothing for a different path (client navigation elsewhere)', () => {
    expect(initialPageDataFor(payload, '/projects/p-2', { id: 'u-1' }, fresh)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1?x=1', { id: 'u-1' }, fresh)).toBeUndefined();
  });

  it('returns nothing for a different user, no user, or a payload without a user', () => {
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-2' }, fresh)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', null, fresh)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: '' }, fresh)).toBeUndefined();
    expect(
      initialPageDataFor({ ...payload, user: undefined }, '/projects/p-1', { id: 'u-1' }, fresh)
    ).toBeUndefined();
    expect(
      initialPageDataFor(
        { ...payload, user: { ...user, id: '' } },
        '/projects/p-1',
        { id: '' },
        fresh
      )
    ).toBeUndefined();
  });

  it('returns nothing once the document is older than the age bound', () => {
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(MAX_SSR_PAGE_DATA_AGE_MS))
    ).toBe(payload.data);
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(MAX_SSR_PAGE_DATA_AGE_MS + 1))
    ).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(NaN))).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(-1))).toBeUndefined();
  });

  it('returns nothing for a back or forward navigation, or an unknown navigation type', () => {
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(100, 'back_forward'))
    ).toBeUndefined();
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(100, null))
    ).toBeUndefined();
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, at(100, 'prerender'))
    ).toBeUndefined();
  });
});

describe('takeInitialPageData', () => {
  afterEach(() => setInitialPageData(null));

  it('hands the payload to the first matching render only', () => {
    setInitialPageData(payload);
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBe(payload.data);
    // A later client-side navigation back to the same path gets nothing.
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
  });

  it('a mismatched first render leaves nothing for a later navigation to the payload path', () => {
    setInitialPageData(payload);
    expect(takeInitialPageData('/', { id: 'u-1' }, fresh)).toBeUndefined();
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
  });

  it('a first render with a query string does not match the payload path, and clears it', () => {
    setInitialPageData(payload);
    expect(takeInitialPageData('/projects/p-1?view=list', { id: 'u-1' }, fresh)).toBeUndefined();
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
  });

  it('a back or forward first render gets nothing and clears the payload', () => {
    setInitialPageData(payload);
    expect(
      takeInitialPageData('/projects/p-1', { id: 'u-1' }, at(100, 'back_forward'))
    ).toBeUndefined();
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
  });

  it('reads the real document timing by default', () => {
    setInitialPageData(payload);
    // The test document has no navigation entry of type navigate/reload
    // with a matching age guarantee, so the default timing either matches
    // or fails closed; either way the payload is cleared.
    takeInitialPageData('/projects/p-1', { id: 'u-1' });
    expect(takeInitialPageData('/projects/p-1', { id: 'u-1' }, fresh)).toBeUndefined();
  });
});
