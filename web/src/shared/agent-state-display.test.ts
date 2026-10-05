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
import { ACTIVITY_DISPLAY, stateLabel } from './agent-state-display.js';

describe('stateLabel', () => {
  it("shows the 'blocked' activity as 'waiting on others' (ptone/scion#1571)", () => {
    expect(stateLabel('blocked')).toBe('waiting on others');
  });

  it('keeps the blocked icon, variant and no pulse', () => {
    expect(ACTIVITY_DISPLAY.blocked).toEqual({
      emoji: '🕓',
      icon: 'clock-history',
      variant: 'neutral',
      pulse: false,
      label: 'waiting on others',
    });
  });

  it('uses other defined display labels', () => {
    expect(stateLabel('waiting_for_input')).toBe('waiting for input');
    expect(stateLabel('limits_exceeded')).toBe('limits exceeded');
  });

  it('falls back to the raw value for states without a label', () => {
    expect(stateLabel('thinking')).toBe('thinking');
    expect(stateLabel('running')).toBe('running');
    expect(stateLabel('something-new')).toBe('something-new');
  });
});
