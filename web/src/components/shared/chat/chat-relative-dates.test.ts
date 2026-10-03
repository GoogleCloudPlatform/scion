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
 * Chat members and chat search: ages under a week stay compact
 * ("5m ago"); older instants show an absolute date through `time.ts`, in the
 * display zone with the zone named (tz-refactor task 21).
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

describe('scion-chat-members activity age', () => {
  const fmt = (iso: string): string =>
    (document.createElement('scion-chat-members') as any).formatRelativeTime(iso);

  it('keeps compact ages under a week', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    expect(fmt('2026-09-23T11:55:00Z')).toBe('5 min ago');
    expect(fmt('2026-09-20T12:00:00Z')).toBe('3d ago');
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

  it('keeps compact ages under a week', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(NOW));
    expect(fmt('2026-09-23T11:55:00Z')).toBe('5m ago');
    expect(fmt('2026-09-23T09:00:00Z')).toBe('3h ago');
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
