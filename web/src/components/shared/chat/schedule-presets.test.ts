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
import { resolveScheduleTime, schedulePresets } from './schedule-presets.js';

describe('schedulePresets', () => {
  it('computes wall-clock presets in the given zone, not the browser zone', () => {
    // Wednesday 2026-10-07 22:30 UTC = Thursday 2026-10-08 00:30 in Berlin (CEST).
    const now = new Date('2026-10-07T22:30:00Z');
    const presets = schedulePresets(now, 'Europe/Berlin');
    expect(presets.map((p) => [p.id, p.value])).toEqual([
      ['in-1h', '2026-10-08T01:30'],
      ['tomorrow-9', '2026-10-09T09:00'],
      ['monday-9', '2026-10-12T09:00'],
    ]);

    const tokyo = schedulePresets(now, 'Asia/Tokyo');
    expect(tokyo.find((p) => p.id === 'tomorrow-9')?.value).toBe('2026-10-09T09:00');
    const la = schedulePresets(now, 'America/Los_Angeles');
    expect(la.find((p) => p.id === 'tomorrow-9')?.value).toBe('2026-10-08T09:00');
  });

  it('Monday 09:00 on a Monday is a week ahead', () => {
    const monday = new Date('2026-10-12T08:00:00Z');
    const presets = schedulePresets(monday, 'UTC');
    expect(presets.find((p) => p.id === 'monday-9')?.value).toBe('2026-10-19T09:00');
  });

  it('returns no presets for an invalid zone', () => {
    expect(schedulePresets(new Date(), 'Not/AZone')).toEqual([]);
  });
});

describe('resolveScheduleTime', () => {
  const now = new Date('2026-10-07T10:00:00Z');

  it('converts the wall-clock value in the zone to a UTC instant', () => {
    expect(resolveScheduleTime('2026-10-08T09:00', 'Europe/Berlin', now)).toEqual({
      fireAt: '2026-10-08T07:00:00.000Z',
    });
  });

  it('rejects an invalid value and a time less than a minute ahead', () => {
    expect(resolveScheduleTime('', 'UTC', now)).toEqual({ error: 'Enter a valid date and time' });
    expect(resolveScheduleTime('2026-10-07T10:00', 'UTC', now)).toEqual({
      error: 'Choose a time at least a minute from now',
    });
    expect('fireAt' in resolveScheduleTime('2026-10-07T10:02', 'UTC', now)).toBe(true);
  });
});
