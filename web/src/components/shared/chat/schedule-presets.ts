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

/**
 * Preset times for the Schedule send dialog, computed as wall-clock times
 * in the user's display zone and returned as `datetime-local` values in
 * that zone. All zone conversion goes through utils/time.ts.
 */

import { parseWallClock, toWallClockInput } from '../../../utils/time.js';

export interface SchedulePreset {
  id: 'in-1h' | 'tomorrow-9' | 'monday-9';
  label: string;
  /** `datetime-local` value (`YYYY-MM-DDTHH:mm`) in the display zone. */
  value: string;
}

/** Adds days to a `YYYY-MM-DD` calendar date. */
function addDays(date: string, days: number): string {
  const [y, m, d] = date.split('-').map(Number);
  const t = new Date(Date.UTC(y ?? 1970, (m ?? 1) - 1, d ?? 1) + days * 86_400_000);
  const pad = (n: number): string => String(n).padStart(2, '0');
  return `${String(t.getUTCFullYear()).padStart(4, '0')}-${pad(t.getUTCMonth() + 1)}-${pad(t.getUTCDate())}`;
}

/** Day of week (0 = Sunday) of a `YYYY-MM-DD` calendar date. */
function weekday(date: string): number {
  const [y, m, d] = date.split('-').map(Number);
  return new Date(Date.UTC(y ?? 1970, (m ?? 1) - 1, d ?? 1)).getUTCDay();
}

/**
 * The presets for `now` in `zone`: in one hour (to the minute), tomorrow at
 * 09:00 and next Monday at 09:00 (a week ahead when today is Monday).
 * Returns an empty list if `zone` is invalid.
 */
export function schedulePresets(now: Date, zone: string): SchedulePreset[] {
  const inOneHour = toWallClockInput(new Date(now.getTime() + 3_600_000).toISOString(), zone);
  const today = toWallClockInput(now.toISOString(), zone).slice(0, 10);
  if (!inOneHour || !today) return [];
  const daysToMonday = (8 - weekday(today)) % 7 || 7;
  return [
    { id: 'in-1h', label: 'In 1 hour', value: inOneHour },
    { id: 'tomorrow-9', label: 'Tomorrow 09:00', value: `${addDays(today, 1)}T09:00` },
    { id: 'monday-9', label: 'Monday 09:00', value: `${addDays(today, daysToMonday)}T09:00` },
  ];
}

/**
 * Minimum lead time the dialog accepts. The hub requires 60 s; the extra
 * margin covers request latency and clock differences.
 */
export const MIN_SCHEDULE_LEAD_MS = 75_000;

/**
 * Latest time the dialog accepts. The hub accepts up to 90 days ahead; a
 * minute less leaves room for request latency.
 */
export const MAX_SCHEDULE_HORIZON_MS = 90 * 86_400_000 - 60_000;

/**
 * The `datetime-local` min and max for the picker in `zone` (minute
 * precision; resolveScheduleTime does the exact check).
 */
export function scheduleInputBounds(now: Date, zone: string): { min: string; max: string } {
  return {
    min: toWallClockInput(new Date(now.getTime() + MIN_SCHEDULE_LEAD_MS).toISOString(), zone),
    max: toWallClockInput(new Date(now.getTime() + MAX_SCHEDULE_HORIZON_MS).toISOString(), zone),
  };
}

/**
 * Converts a `datetime-local` value in `zone` to a UTC ISO instant and
 * checks it is at least MIN_SCHEDULE_LEAD_MS and at most
 * MAX_SCHEDULE_HORIZON_MS after `now`. Returns the instant, or an error
 * message.
 */
export function resolveScheduleTime(
  value: string,
  zone: string,
  now: Date
): { fireAt: string } | { error: string } {
  const fireAt = parseWallClock(value, zone);
  if (!fireAt) return { error: 'Enter a valid date and time' };
  const leadMs = new Date(fireAt).getTime() - now.getTime();
  if (leadMs < MIN_SCHEDULE_LEAD_MS) {
    return { error: 'Choose a time at least a minute from now' };
  }
  if (leadMs > MAX_SCHEDULE_HORIZON_MS) {
    return { error: 'Choose a time within 90 days' };
  }
  return { fireAt };
}
