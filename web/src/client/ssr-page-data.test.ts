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

import { describe, it, expect } from 'vitest';

import type { PageData } from '../shared/types.js';
import { initialPageDataFor, MAX_SSR_PAGE_DATA_AGE_MS } from './ssr-page-data.js';

const user = { id: 'u-1', email: 'u@example.com', name: 'U' };
const payload: PageData = {
  path: '/projects/p-1',
  title: 'Scion',
  user,
  data: { id: 'p-1', name: 'Project One' },
};

describe('initialPageDataFor', () => {
  it('hands over the payload for the same path and user on a young document', () => {
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, 100)).toBe(payload.data);
  });

  it('returns nothing without a payload or without data', () => {
    expect(initialPageDataFor(null, '/projects/p-1', { id: 'u-1' }, 100)).toBeUndefined();
    expect(
      initialPageDataFor({ ...payload, data: undefined }, '/projects/p-1', { id: 'u-1' }, 100)
    ).toBeUndefined();
  });

  it('returns nothing for a different path (client navigation elsewhere)', () => {
    expect(initialPageDataFor(payload, '/projects/p-2', { id: 'u-1' }, 100)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1?x=1', { id: 'u-1' }, 100)).toBeUndefined();
  });

  it('returns nothing for a different user, no user, or a payload without a user', () => {
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-2' }, 100)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', null, 100)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: '' }, 100)).toBeUndefined();
    expect(
      initialPageDataFor({ ...payload, user: undefined }, '/projects/p-1', { id: 'u-1' }, 100)
    ).toBeUndefined();
    expect(
      initialPageDataFor(
        { ...payload, user: { ...user, id: '' } },
        '/projects/p-1',
        { id: '' },
        100
      )
    ).toBeUndefined();
  });

  it('returns nothing once the document is older than the age bound', () => {
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, MAX_SSR_PAGE_DATA_AGE_MS)
    ).toBe(payload.data);
    expect(
      initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, MAX_SSR_PAGE_DATA_AGE_MS + 1)
    ).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, NaN)).toBeUndefined();
    expect(initialPageDataFor(payload, '/projects/p-1', { id: 'u-1' }, -1)).toBeUndefined();
  });
});
