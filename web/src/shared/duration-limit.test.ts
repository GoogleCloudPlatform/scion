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
import { isZeroDurationLimit } from './duration-limit.js';

describe('isZeroDurationLimit', () => {
  it.each([
    '0',
    '+0',
    '-0',
    '0ns',
    '0us',
    '0\u00b5s', // micro sign µs
    '0\u03bcs', // Greek small letter mu μs
    '0ms',
    '0s',
    '0m',
    '0h',
    '+0s',
    '-0s',
    '-0m',
    '+0h',
    '00s',
    '0.0s',
    '0.s',
    '.0s',
    '0.000h',
    '0h0m',
    '0m0s',
    '0h0m0s',
    '-0h0m0s',
    '+0h0.0m0s',
    '0h0m0s0ms0us0\u00b5s0\u03bcs0ns',
  ])('returns true for Go zero-duration spelling %j', (input) => {
    expect(isZeroDurationLimit(input)).toBe(true);
  });

  it.each([
    '1ns',
    '1s',
    '30m',
    '2h',
    '1h30m',
    '0h1m',
    '0h0m0.1s',
    '0.5s',
    '.5m',
    '-5m',
    '+10s',
    '0.001ms',
  ])('returns false for non-zero duration %j', (input) => {
    expect(isZeroDurationLimit(input)).toBe(false);
  });

  it.each([
    '',
    ' ',
    ' 0',
    '0 ',
    ' 0s',
    '0s ',
    '00',
    '0.0',
    '.0',
    '+',
    '-',
    '.s',
    's',
    '0d',
    '0S',
    '0H',
    '0ms0',
    'soon',
    'invalid',
    'Unlimited',
    'unlimited',
  ])('returns false for empty or invalid duration %j', (input) => {
    expect(isZeroDurationLimit(input)).toBe(false);
  });

  it.each([undefined, null, 0, 1, false, true, {}, []])(
    'returns false for non-string value %p',
    (input) => {
      expect(isZeroDurationLimit(input)).toBe(false);
    }
  );
});
