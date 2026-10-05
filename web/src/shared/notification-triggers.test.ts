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

import {
  ALL_TRIGGERS,
  DEFAULT_TRIGGERS,
  defaultTriggersHint,
  triggerLabel,
} from './notification-triggers.js';

describe('triggerLabel', () => {
  it("shows WAITING_FOR_INPUT as 'Waiting for User' (ptone/scion#3301)", () => {
    expect(triggerLabel('WAITING_FOR_INPUT')).toBe('Waiting for User');
  });

  it('labels every selectable trigger', () => {
    expect(ALL_TRIGGERS.map(triggerLabel)).toEqual([
      'Completed',
      'Waiting for User',
      'Limits Exceeded',
      'Stalled',
      'Error',
      'Deleted',
    ]);
  });

  it('falls back to the raw value for unknown triggers', () => {
    expect(triggerLabel('SOMETHING_NEW')).toBe('SOMETHING_NEW');
  });

  it('keeps the API trigger values unchanged', () => {
    expect(DEFAULT_TRIGGERS).toEqual(['COMPLETED', 'WAITING_FOR_INPUT', 'LIMITS_EXCEEDED']);
  });
});

describe('defaultTriggersHint', () => {
  it('lists the default triggers by display label', () => {
    expect(defaultTriggersHint()).toBe(
      'You will be notified when this agent reaches: Completed, Waiting for User, or Limits Exceeded.',
    );
  });
});
