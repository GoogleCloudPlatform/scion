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
import { formatNumber } from './format-number.js';

describe('formatNumber', () => {
  it('formats with FORMAT_LOCALE grouping', () => {
    expect(formatNumber(1234567)).toBe('1,234,567');
  });

  it('formats decimals', () => {
    expect(formatNumber(1234.5)).toBe('1,234.5');
  });

  it('formats zero and negative numbers', () => {
    expect(formatNumber(0)).toBe('0');
    expect(formatNumber(-42)).toBe('-42');
  });
});
