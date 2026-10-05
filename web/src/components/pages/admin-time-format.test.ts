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
 * Admin, access-boundary and role-binding views format times through
 * `time.ts` (tz-refactor task 20): absolute times render in the effective
 * display zone with a 24-hour clock and a zone label, and relative times go
 * through `formatRelative`, keeping each view's own guard for a missing or
 * unparsable value.
 *
 * Vitest pins the browser zone to UTC; every test here sets a different
 * display preference (Asia/Tokyo, UTC+9) so a formatter that still used the
 * browser zone would print a different wall-clock time. 15:00Z is midnight
 * in Tokyo, so the expected strings also prove midnight renders as `00:00`.
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { render } from 'lit';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./admin-access-boundaries.js');
await import('./admin-access-boundary-detail.js');
await import('./admin-maintenance.js');
await import('./admin-scheduler.js');
await import('./admin-users.js');
await import('./metrics-dashboard.js');
await import('../shared/access-boundary-audit-timeline.js');
await import('../shared/access-boundary-definition-summary.js');
await import('../shared/access-boundary-impact-summary.js');
await import('../shared/access-boundary-preview.js');

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

const el = (tag: string): AnyEl => document.createElement(tag);

/** Midnight on Sep 24 in Asia/Tokyo. */
const TOKYO_MIDNIGHT = '2026-09-23T15:00:00Z';
const NOW = '2026-10-01T12:00:00Z';

describe('admin and access-boundary time formatting (tz-refactor task 20)', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date(NOW));
    setPreferredTimeZone('Asia/Tokyo');
  });

  afterEach(() => {
    setPreferredTimeZone('');
    vi.useRealTimers();
  });

  const DATETIME_HELPERS: Array<[tag: string, method: string]> = [
    ['scion-page-admin-access-boundary-detail', 'formatDatetime'],
    ['scion-access-boundary-definition-summary', 'formatDatetime'],
    ['scion-access-boundary-audit-timeline', 'formatDatetime'],
    ['scion-access-boundary-preview', 'formatDatetime'],
    ['scion-page-admin-maintenance', 'formatDateTime'],
  ];

  for (const [tag, method] of DATETIME_HELPERS) {
    it(`${tag}.${method} renders the display zone, 24-hour, with a zone label`, () => {
      expect(el(tag)[method](TOKYO_MIDNIGHT)).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
      expect(el(tag)[method]('2026-09-24T09:30:00Z')).toBe('Sep 24, 2026, 18:30 (Asia/Tokyo)');
    });
  }

  it('keeps each view guard for a missing or unparsable value', () => {
    expect(el('scion-page-admin-access-boundary-detail').formatDatetime(null)).toBe('—');
    expect(el('scion-page-admin-access-boundary-detail').formatDatetime('bogus')).toBe('bogus');
    expect(el('scion-access-boundary-definition-summary').formatDatetime(undefined)).toBe(
      'Not set'
    );
    expect(el('scion-access-boundary-audit-timeline').formatDatetime('bogus')).toBe('bogus');
    expect(el('scion-access-boundary-preview').formatDatetime('bogus')).toBe('bogus');
    expect(el('scion-access-boundary-impact-summary').formatDate('bogus')).toBe('bogus');
    expect(el('scion-page-admin-maintenance').formatDateTime(undefined)).toBe('');
    expect(el('scion-page-admin-maintenance').formatDateTime('bogus')).toBe('');
    expect(el('scion-page-admin-maintenance').formatDate('bogus')).toBe('');
  });

  it('date-only views use the display zone date and label it', () => {
    // 15:00Z on Sep 23 is already Sep 24 in Tokyo.
    expect(el('scion-access-boundary-impact-summary').formatDate(TOKYO_MIDNIGHT)).toBe(
      'Sep 24, 2026 (Asia/Tokyo)'
    );
    expect(el('scion-page-admin-maintenance').formatDate(TOKYO_MIDNIGHT)).toBe(
      'Sep 24, 2026 (Asia/Tokyo)'
    );
  });

  it('access-boundary list schedule shows both bounds in the display zone with one label', () => {
    const page = el('scion-page-admin-access-boundaries');
    expect(
      page.formatSchedule({
        appliesWhen: { notBefore: TOKYO_MIDNIGHT, expiresAt: '2026-09-30T23:15:00Z' },
      })
    ).toBe('From Sep 24, 2026, 00:00 Until Oct 1, 2026, 08:15 (Asia/Tokyo)');
    expect(page.formatSchedule({ appliesWhen: { expiresAt: TOKYO_MIDNIGHT } })).toBe(
      'Until Sep 24, 2026, 00:00 (Asia/Tokyo)'
    );
    // A bound that does not parse falls back to its raw value; the zone label
    // stays only while the other bound was converted.
    expect(
      page.formatSchedule({ appliesWhen: { notBefore: 'bogus', expiresAt: TOKYO_MIDNIGHT } })
    ).toBe('From bogus Until Sep 24, 2026, 00:00 (Asia/Tokyo)');
    expect(page.formatSchedule({ appliesWhen: { notBefore: 'bogus', expiresAt: 'junk' } })).toBe(
      'From bogus Until junk'
    );
    expect(page.formatSchedule({ appliesWhen: null })).toBe('Always');
    expect(page.formatSchedule({ appliesWhen: {} })).toBe('Always');
  });

  it('relative helpers format through formatRelative and keep their guards', () => {
    const maintenance = el('scion-page-admin-maintenance');
    expect(maintenance.formatRelativeTime('2026-10-01T09:00:00Z')).toBe('3 hours ago');
    expect(maintenance.formatRelativeTime(undefined)).toBe('');
    expect(maintenance.formatRelativeTime('bogus')).toBe('');

    for (const tag of ['scion-page-admin-scheduler', 'scion-page-admin-users']) {
      const page = el(tag);
      expect(page.formatRelativeTime('2026-09-29T12:00:00Z')).toBe('2 days ago');
      expect(page.formatRelativeTime(undefined)).toBe('Never');
      expect(page.formatRelativeTime('bogus')).toBe('Never');
    }
    // Invite expiry on the users page is in the future.
    expect(el('scion-page-admin-users').formatRelativeTime('2026-10-03T12:00:00Z')).toBe(
      'in 2 days'
    );
  });

  it('scheduler next-run is future-relative and says "now" once due', () => {
    const page = el('scion-page-admin-scheduler');
    expect(page.formatFutureTime('2026-10-01T12:05:00Z')).toBe('in 5 minutes');
    expect(page.formatFutureTime('2026-10-03T12:00:00Z')).toBe('in 2 days');
    expect(page.formatFutureTime('2026-10-01T11:00:00Z')).toBe('now');
    expect(page.formatFutureTime('bogus')).toBe('bogus');
  });

  it('scheduler tick count renders through formatNumber', () => {
    const container = document.createElement('div');
    render(
      el('scion-page-admin-scheduler').renderOverview({
        tickCount: 12345,
        tickInterval: '1m',
        activeTimers: 2,
        recurringHandlers: [],
      }),
      container
    );
    expect(container.querySelector('.stat-value')?.textContent).toBe('12,345');
  });

  it('metrics dashboard numbers format through formatNumber', () => {
    const page = el('scion-page-metrics');
    expect(page.formatCompactNumber(999)).toBe('999');
    expect(page.formatCompactNumber(12_345)).toBe('12.3K');
    expect(page.formatCompactNumber(2_500_000)).toBe('2.5M');
  });
});
