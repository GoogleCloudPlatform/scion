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
 * Log viewers in the display zone (tz-refactor task 21): the agent log,
 * agent message and unified log viewers render their timestamps through
 * `time.ts`, in the effective display zone (not the browser zone, which
 * vitest pins to UTC), on a 24-hour clock where midnight is 00:00, with the
 * zone named next to the absolute date, and re-render on a zone change.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

vi.mock('../../client/api.js', () => ({
  // Never resolves: the tests set entries directly and must not race a load.
  apiFetch: vi.fn(() => new Promise(() => {})),
  extractApiError: vi.fn(() => 'error'),
}));

import { setPreferredTimeZone } from '../../utils/time.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

beforeAll(async () => {
  await import('./agent-log-viewer.js');
  await import('./agent-message-viewer.js');
  await import('./unified-log-viewer.js');
});

afterEach(() => {
  document.body.innerHTML = '';
  setPreferredTimeZone('');
});

/** 15:00:00.000Z is midnight the next day in Tokyo (+09:00). */
const TOKYO_MIDNIGHT = '2026-09-23T15:00:00.000Z';

function text(el: AnyEl, selector: string): string[] {
  return Array.from(el.shadowRoot.querySelectorAll(selector) as NodeListOf<HTMLElement>).map((n) =>
    (n.textContent ?? '').trim()
  );
}

describe('scion-agent-log-viewer timestamps', () => {
  async function mount(): Promise<AnyEl> {
    const el: AnyEl = document.createElement('scion-agent-log-viewer');
    el.agentId = 'a1';
    document.body.appendChild(el);
    el.entries = [
      { timestamp: TOKYO_MIDNIGHT, severity: 'INFO', message: 'hello', insertId: 'i1' },
      {
        timestamp: '2026-09-23T15:04:05.007Z',
        severity: 'INFO',
        message: 'later',
        insertId: 'i2',
      },
    ];
    await el.updateComplete;
    return el;
  }

  it('renders midnight as 00:00:00.000 in the display zone, divider names the zone', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    expect(text(el, 'td.ts')).toEqual(['00:00:00.000', '00:04:05.007']);
    expect(text(el, 'td.date-divider')).toEqual(['Sep 24, 2026 (Asia/Tokyo)']);
  });

  it('re-renders on a display zone change', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    setPreferredTimeZone('America/New_York');
    await el.updateComplete;
    expect(text(el, 'td.ts')).toEqual(['11:00:00.000', '11:04:05.007']);
    expect(text(el, 'td.date-divider')).toEqual(['Sep 23, 2026 (America/New_York)']);
  });
});

describe('scion-agent-message-viewer timestamps', () => {
  function message(timestamp: string, insertId: string) {
    return {
      sender: 'user:alice',
      recipient: 'agent:bob',
      direction: 'sent',
      msgType: 'instruction',
      body: 'hi',
      urgent: false,
      broadcasted: false,
      timestamp,
      insertId,
      raw: null,
    };
  }

  async function mount(): Promise<AnyEl> {
    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.agentId = 'a1';
    document.body.appendChild(el);
    el.messages = [message(TOKYO_MIDNIGHT, 'm1')];
    await el.updateComplete;
    return el;
  }

  it('renders midnight as 00:00:00 in the display zone, divider names the zone', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    expect(text(el, '.msg-time')).toEqual(['00:00:00']);
    expect(text(el, '.date-divider')).toEqual(['Sep 24, 2026 (Asia/Tokyo)']);
  });

  it('re-renders on a display zone change', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    setPreferredTimeZone('Asia/Kathmandu');
    await el.updateComplete;
    // +05:45
    expect(text(el, '.msg-time')).toEqual(['20:45:00']);
    expect(text(el, '.date-divider')).toEqual(['Sep 23, 2026 (Asia/Kathmandu)']);
  });
});

describe('scion-unified-log-viewer timestamps', () => {
  async function mount(): Promise<AnyEl> {
    const el: AnyEl = document.createElement('scion-unified-log-viewer');
    document.body.appendChild(el);
    el.entries = [
      {
        timestamp: TOKYO_MIDNIGHT,
        severity: 'ERROR',
        message: 'boom',
        insertId: 'u1',
        source: 'hub',
      },
    ];
    await el.updateComplete;
    return el;
  }

  it('renders midnight as 00:00:00.000 in the display zone, tooltip names the zone', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    expect(text(el, '.log-row .ts')).toEqual(['00:00:00.000']);
    const ts = el.shadowRoot.querySelector('.log-row .ts') as HTMLElement;
    expect(ts.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
  });

  it('detail panel shows the display-zone instant, then the raw value labelled UTC', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    (el.shadowRoot.querySelector('.log-row') as HTMLElement).click();
    await el.updateComplete;
    expect(text(el, '.detail-meta-item')[0].replace(/\s+/g, ' ')).toBe(
      'Timestamp: Sep 24, 2026, 00:00:00.000 (Asia/Tokyo) · 2026-09-23T15:00:00.000Z UTC'
    );
  });

  it('detail panel shows an unparsable timestamp as given, without throwing', async () => {
    const el: AnyEl = document.createElement('scion-unified-log-viewer');
    document.body.appendChild(el);
    el.entries = [
      {
        timestamp: 'not-a-date',
        severity: 'ERROR',
        message: 'boom',
        insertId: 'u2',
        source: 'hub',
      },
    ];
    await el.updateComplete;
    (el.shadowRoot.querySelector('.log-row') as HTMLElement).click();
    await el.updateComplete;
    expect(text(el, '.detail-meta-item')[0].replace(/\s+/g, ' ')).toBe('Timestamp: not-a-date');
  });

  it('re-renders on a display zone change', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount();
    setPreferredTimeZone('UTC');
    await el.updateComplete;
    expect(text(el, '.log-row .ts')).toEqual(['15:00:00.000']);
  });
});
