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

import { describe, it, expect, vi, beforeEach } from 'vitest';

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }));
vi.mock('../utils/toast.js', () => ({ showToast }));

import {
  responseWarnings,
  showResponseWarnings,
  showResponseBodyWarnings,
} from './response-warnings.js';

beforeEach(() => {
  showToast.mockClear();
});

describe('responseWarnings', () => {
  it('returns the non-empty string warnings once each', () => {
    expect(responseWarnings({ warnings: ['a', '', '  ', 'b', 'a', 3, null] })).toEqual(['a', 'b']);
  });

  it('returns nothing for a body without a warnings list', () => {
    expect(responseWarnings(undefined)).toEqual([]);
    expect(responseWarnings(null)).toEqual([]);
    expect(responseWarnings('warnings')).toEqual([]);
    expect(responseWarnings({})).toEqual([]);
    expect(responseWarnings({ warnings: 'a' })).toEqual([]);
  });
});

describe('showResponseWarnings', () => {
  it('shows each warning as a toast that stays until closed', () => {
    expect(showResponseWarnings({ warnings: ['first', 'second'] })).toEqual(['first', 'second']);
    expect(showToast).toHaveBeenCalledTimes(2);
    expect(showToast).toHaveBeenNthCalledWith(1, 'first', 'warning', { duration: Infinity });
    expect(showToast).toHaveBeenNthCalledWith(2, 'second', 'warning', { duration: Infinity });
  });

  it('shows nothing without warnings', () => {
    expect(showResponseWarnings({ agent: { id: 'a' } })).toEqual([]);
    expect(showToast).not.toHaveBeenCalled();
  });
});

describe('showResponseBodyWarnings', () => {
  it('reads the JSON body and shows its warnings', async () => {
    const res = { json: () => Promise.resolve({ warnings: ['w'] }) } as Response;
    await expect(showResponseBodyWarnings(res)).resolves.toEqual(['w']);
    expect(showToast).toHaveBeenCalledWith('w', 'warning', { duration: Infinity });
  });

  it('shows nothing when the body is not JSON', async () => {
    const res = { json: () => Promise.reject(new SyntaxError('no JSON')) } as Response;
    await expect(showResponseBodyWarnings(res)).resolves.toEqual([]);
    expect(showToast).not.toHaveBeenCalled();
  });
});
