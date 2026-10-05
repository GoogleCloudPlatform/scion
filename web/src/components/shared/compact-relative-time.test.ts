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
 * Compact relative-time cells (agent and skill lists, health dashboard,
 * trays, home) format through `formatRelative` with the narrow style
 * (tz-refactor task 19), keeping their guards: an em dash (or the
 * component's own word) for an unparsable time and "just now" for a future
 * instant (clock skew between hub and browser).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';

await import('../pages/agents.js');
await import('../pages/project-detail.js');
await import('../pages/skills.js');
await import('../pages/skill-detail.js');
await import('../pages/health-dashboard.js');
await import('./notification-tray.js');

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

const NOW = '2026-10-01T12:00:00Z';

const HELPERS: Array<[tag: string, method: string, invalid: string]> = [
  ['scion-page-agents', 'formatRelativeTime', '—'],
  ['scion-page-project-detail', 'formatRelativeTime', '—'],
  ['scion-page-skills', 'formatRelativeTime', '—'],
  ['scion-page-skill-detail', 'formatRelativeTime', '—'],
  ['scion-page-health-dashboard', 'timeAgo', 'unknown'],
  ['scion-notification-tray', 'relativeTime', '—'],
];

describe('compact relative times (tz-refactor task 19)', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date(NOW));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  for (const [tag, method, invalid] of HELPERS) {
    it(`${tag}.${method} formats through formatRelative with its guards`, () => {
      const el = document.createElement(tag) as AnyEl;
      const fmt = (iso: string) => el[method](iso) as string;
      expect(fmt('2026-10-01T11:59:30Z')).toBe('30s ago');
      expect(fmt('2026-10-01T11:55:00Z')).toBe('5m ago');
      expect(fmt('2026-10-01T09:00:00Z')).toBe('3h ago');
      expect(fmt('2026-09-29T12:00:00Z')).toBe('2d ago');
      expect(fmt('2026-09-30T12:00:00Z')).toBe('yesterday');
      expect(fmt('2026-10-01T12:05:00Z')).toBe('just now');
      expect(fmt('not-a-date')).toBe(invalid);
    });
  }

  it('health dashboard keeps "never" for a missing time', () => {
    const el = document.createElement('scion-page-health-dashboard') as AnyEl;
    expect(el.timeAgo('')).toBe('never');
  });
});
