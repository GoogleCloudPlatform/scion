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
 * Chat members and chat search: ages under a week (and future instants) use
 * `formatRelative(iso, { style: 'narrow' })` ("5m ago"); older instants show
 * an absolute date through `time.ts`, in the display zone with the zone named
 * (tz-refactor task 21).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => new Promise(() => {})),
  extractApiError: vi.fn(() => 'error'),
}));

import { render } from 'lit';
import { setPreferredTimeZone } from '../../../utils/time.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

beforeAll(async () => {
  await import('./chat-members.js');
  await import('./chat-search.js');
});

afterEach(() => {
  setPreferredTimeZone('');
  vi.useRealTimers();
});

const NOW = '2026-09-23T12:00:00Z';

/**
 * Compact ages under a week, with the exact narrow `en` output. Values are
 * rounded (not floored) per unit with `Math.round`, so an exact past half
 * rounds toward zero, and `numeric: 'auto'` gives "now" and "yesterday".
 */
const COMPACT_CASES: Array<[string, string, string]> = [
  ['same instant', '2026-09-23T12:00:00Z', 'now'],
  ['59 seconds', '2026-09-23T11:59:01Z', '59s ago'],
  ['5 minutes', '2026-09-23T11:55:00Z', '5m ago'],
  ['59m40s rounds to 1 hour', '2026-09-23T11:00:20Z', '1h ago'],
  ['exactly 59.5 minutes stays at 59 (Math.round of -59.5)', '2026-09-23T11:00:30Z', '59m ago'],
  ['3 hours', '2026-09-23T09:00:00Z', '3h ago'],
  ['23h40m rounds to 1 day', '2026-09-22T12:20:00Z', 'yesterday'],
  ['3 days', '2026-09-20T12:00:00Z', '3d ago'],
  ['6 days', '2026-09-17T12:00:00Z', '6d ago'],
  ['6d14h rounds to 7 days', '2026-09-16T22:00:00Z', '7d ago'],
  ['5 minutes in the future', '2026-09-23T12:05:00Z', 'in 5m'],
];

describe('scion-chat-members activity age', () => {
  const fmt = (iso: string): string =>
    (document.createElement('scion-chat-members') as any).formatRelativeTime(iso);

  it.each(COMPACT_CASES)('shows a compact age: %s', (_name, iso, want) => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    expect(fmt(iso)).toBe(want);
  });

  it('shows an older date in the display zone, with the zone named', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    // vitest pins the browser zone to UTC; 15:00Z is the next day in Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    expect(fmt('2026-09-01T15:00:00Z')).toBe('Sep 2, 2026 (Asia/Tokyo)');
  });
});

describe('scion-chat-search result time', () => {
  const fmt = (iso: string): string =>
    (document.createElement('scion-chat-search') as any).formatTime(iso);

  it.each(COMPACT_CASES)('shows a compact age: %s', (_name, iso, want) => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    expect(fmt(iso)).toBe(want);
  });

  it('switches to an absolute date at exactly 7 days', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    expect(fmt('2026-09-16T12:00:00Z')).toBe('Sep 16, 2026');
  });

  it('shows an older date compactly in the display zone, with the zone in the title', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    // vitest pins the browser zone to UTC; 15:00Z is the next day in Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    const el = document.createElement('scion-chat-search') as any;
    const container = document.createElement('div');
    render(
      el.renderResult({
        threadName: 'general',
        conversationKey: 'c1',
        senderName: 'alice',
        snippet: 'hi',
        timestamp: '2026-09-01T15:00:00Z',
      }),
      container
    );
    const time = container.querySelector('.result-time') as HTMLElement;
    expect(time.textContent?.trim()).toBe('Sep 2, 2026');
    expect(time.getAttribute('title')).toBe('Sep 2, 2026, 00:00 (Asia/Tokyo)');
  });
});
