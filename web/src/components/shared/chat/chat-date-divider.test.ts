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
 * Tests for chat-date-divider.ts's zone label (AC4, tz-refactor task 11,
 * review round 2 R2-3).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import { render } from 'lit';
import { formatChatDate, renderDateDivider } from './chat-date-divider.js';
import { setPreferredTimeZone } from '../../../utils/time.js';

describe('formatChatDate', () => {
  afterEach(() => setPreferredTimeZone(''));

  it('is a pure date string with no zone suffix (used as a grouping key)', () => {
    setPreferredTimeZone('Asia/Tokyo');
    expect(formatChatDate('2026-09-23T15:00:00Z')).toBe('Sep 24, 2026');
  });

  it('returns "" for an invalid instant', () => {
    expect(formatChatDate('not-a-date')).toBe('');
  });
});

describe('renderDateDivider (AC4, review R2-3)', () => {
  afterEach(() => setPreferredTimeZone(''));

  it('appends the effective zone to the rendered label', () => {
    setPreferredTimeZone('Asia/Tokyo');
    const host = document.createElement('div');
    render(renderDateDivider(formatChatDate('2026-09-23T15:00:00Z')), host);
    const label = host.querySelector('.date-label');
    expect(label?.textContent?.trim()).toBe('Sep 24, 2026 · Asia/Tokyo');
  });

  it('renders no zone suffix for an empty date string', () => {
    const host = document.createElement('div');
    render(renderDateDivider(''), host);
    const label = host.querySelector('.date-label');
    expect(label?.textContent?.trim()).toBe('');
  });
});
