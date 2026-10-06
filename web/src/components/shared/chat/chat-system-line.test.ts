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
 * Tests for <scion-chat-system-line>'s zone-aware time rendering (AC4,
 * tz-refactor task 11, review round 2 R2-1/R2-3).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../../utils/time.js';

await import('./chat-system-line.js');
type ScionChatSystemLine = import('./chat-system-line.js').ScionChatSystemLine;

async function mount(props: Partial<ScionChatSystemLine>): Promise<ScionChatSystemLine> {
  const el = document.createElement('scion-chat-system-line') as ScionChatSystemLine;
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-chat-system-line', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('renders the time in the effective zone with midnight as 00:00', async () => {
    setPreferredTimeZone('UTC');
    const el = await mount({ message: 'restarted', timestamp: '2026-01-15T00:00:00Z' });
    const time = el.shadowRoot?.querySelector('.system-time');
    expect(time?.textContent).toBe('00:00');
  });

  // AC4, review R2-3: title carries the full instant and zone label.
  it('titles the time with the full instant and zone (AC4, review R2-3)', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount({ message: 'restarted', timestamp: '2026-09-23T15:00:00Z' });
    const time = el.shadowRoot?.querySelector('.system-time');
    expect(time?.textContent).toBe('00:00');
    expect(time?.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
  });

  // Review R2-1: a mounted line re-renders when the preference changes later.
  it('re-renders in the new zone after a mounted line outlives a preference change', async () => {
    const el = await mount({ message: 'restarted', timestamp: '2026-09-23T15:00:00Z' });
    expect(el.shadowRoot?.querySelector('.system-time')?.textContent).toBe('15:00');

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.system-time')?.textContent).toBe('00:00');
  });

  it('renders no time element when timestamp is empty', async () => {
    const el = await mount({ message: 'restarted' });
    expect(el.shadowRoot?.querySelector('.system-time')).toBeNull();
  });
});
