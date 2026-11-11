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
 * Skill-registry pages: the "created"/"updated" ages go through `time.ts`'s
 * `formatRelative` instead of a hand-rolled helper (tz-refactor task 21).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => new Promise(() => {})),
  extractApiError: vi.fn(() => 'error'),
}));

/* eslint-disable @typescript-eslint/no-explicit-any */

beforeAll(async () => {
  await import('./admin-skill-registries.js');
  await import('./admin-skill-registry-detail.js');
});

afterEach(() => {
  vi.useRealTimers();
});

describe.each(['scion-page-admin-skill-registries', 'scion-page-admin-skill-registry-detail'])(
  '%s relative ages',
  (tag) => {
    function el(): any {
      return document.createElement(tag);
    }

    it('formats past ages with formatRelative', () => {
      vi.useFakeTimers();
      vi.setSystemTime(new Date('2026-09-23T12:00:00Z'));
      expect(el().formatRelativeTime('2026-09-23T11:57:00Z')).toBe('3 minutes ago');
      expect(el().formatRelativeTime('2026-09-23T09:00:00Z')).toBe('3 hours ago');
      expect(el().formatRelativeTime('2026-09-20T12:00:00Z')).toBe('3 days ago');
      expect(el().formatRelativeTime('2026-09-22T12:00:00Z')).toBe('yesterday');
    });

    it('formats a near or slightly future instant without a negative age', () => {
      vi.useFakeTimers();
      vi.setSystemTime(new Date('2026-09-23T12:00:00Z'));
      expect(el().formatRelativeTime('2026-09-23T12:00:00Z')).toBe('now');
      expect(el().formatRelativeTime('2026-09-23T12:03:00Z')).toBe('in 3 minutes');
    });

    it('shows an em dash for an unparsable timestamp', () => {
      expect(el().formatRelativeTime('not-a-date')).toBe('—');
    });
  }
);
