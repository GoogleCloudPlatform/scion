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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ScionAccessBoundaryPreview } from './access-boundary-preview.js';

// The expiry countdown in isolation. The element is never connected, so Lit
// does not render and the preview needs only `expiresAt`.
interface ExpiryInternals {
  preview: { expiresAt: string } | null;
  isExpired: boolean;
  expiryTimeLeft: string;
  startExpiryTimer(): void;
  cleanup(): void;
}

describe('scion-access-boundary-preview expiry countdown', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-19T12:00:00Z'));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const create = (expiresAt: string): ExpiryInternals => {
    const el = new ScionAccessBoundaryPreview() as unknown as ExpiryInternals;
    el.preview = { expiresAt };
    return el;
  };

  it('starts no timer for a preview that has already expired', () => {
    const el = create('2026-10-19T11:59:00Z');
    el.startExpiryTimer();
    expect(el.isExpired).toBe(true);
    expect(el.expiryTimeLeft).toBe('Expired');
    expect(vi.getTimerCount()).toBe(0);
  });

  it('counts down a live preview and stops its timer on expiry', () => {
    const el = create('2026-10-19T12:00:02Z');
    el.startExpiryTimer();
    expect(el.isExpired).toBe(false);
    expect(el.expiryTimeLeft).toBe('0:02');
    expect(vi.getTimerCount()).toBe(1);
    vi.advanceTimersByTime(1000);
    expect(el.expiryTimeLeft).toBe('0:01');
    vi.advanceTimersByTime(1000);
    expect(el.isExpired).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('cleanup stops a running countdown', () => {
    const el = create('2026-10-19T12:05:00Z');
    el.startExpiryTimer();
    expect(vi.getTimerCount()).toBe(1);
    el.cleanup();
    expect(vi.getTimerCount()).toBe(0);
  });
});
