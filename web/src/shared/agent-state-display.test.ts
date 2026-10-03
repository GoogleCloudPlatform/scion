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
import { activityLabel } from './agent-state-display.js';

describe('activityLabel', () => {
  it('returns the display label for a known activity that defines one', () => {
    expect(activityLabel('blocked')).toBe('waiting');
    expect(activityLabel('waiting_for_input')).toBe('waiting for input');
  });

  it('returns undefined for a known activity without a label', () => {
    expect(activityLabel('thinking')).toBeUndefined();
  });

  it('returns undefined for unknown and empty keys', () => {
    expect(activityLabel('not-a-real-activity')).toBeUndefined();
    expect(activityLabel('')).toBeUndefined();
    expect(activityLabel(undefined)).toBeUndefined();
  });

  it('ignores inherited prototype keys', () => {
    // A non-enumerable `label` on Object.prototype is reachable through
    // `constructor` and `__proto__`; only an own-property check skips them.
    Object.defineProperty(Object.prototype, 'label', {
      value: 'inherited',
      writable: true,
      configurable: true,
    });
    try {
      expect(activityLabel('constructor')).toBeUndefined();
      expect(activityLabel('__proto__')).toBeUndefined();
    } finally {
      delete (Object.prototype as { label?: string }).label;
    }
  });

  it('matches case-insensitively', () => {
    expect(activityLabel('Blocked')).toBe('waiting');
    expect(activityLabel('LIMITS_EXCEEDED')).toBe('limits exceeded');
  });
});
