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
 * Log viewer render cost (webperf C6, ptone/scion#5310): the agent log,
 * unified log and agent message viewers render a burst of streamed lines
 * once per animation frame, format each entry's timestamps once at ingest
 * (not once per render), keep entries sorted without re-sorting, and key
 * their rows so a re-render keeps existing row nodes.
 *
 * The streams are driven through a fake EventSource, so these tests go
 * through the viewers' real (unchanged) stream listeners.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/api.js', () => ({
  // Never resolves: the tests feed the viewers through their streams only.
  apiFetch: vi.fn(() => new Promise(() => {})),
  extractApiError: vi.fn(() => 'error'),
}));

import { setPreferredTimeZone } from '../../utils/time.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

/** Fields formatted per row at ingest (date or tooltip, plus row time). */
const FORMATS_PER_ENTRY = 2;
const LINES = 500;

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CLOSED = 2;
  readyState = 1;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((e: Event) => void)[]>();

  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (e: Event) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(fn);
    this.listeners.set(type, list);
  }

  emit(type: string, data: unknown): void {
    const event = new MessageEvent(type, { data: JSON.stringify(data) });
    for (const fn of this.listeners.get(type) ?? []) fn(event);
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

let frames: FrameRequestCallback[] = [];

function flushFrames(): void {
  const pending = frames;
  frames = [];
  for (const cb of pending) cb(performance.now());
}

/** Counts Intl.DateTimeFormat#format calls (`format` is a prototype getter). */
function countFormats(): { count: number; restore: () => void } {
  const proto = Intl.DateTimeFormat.prototype;
  const desc = Object.getOwnPropertyDescriptor(proto, 'format')!;
  const counter = {
    count: 0,
    restore: (): void => {
      Object.defineProperty(proto, 'format', desc);
    },
  };
  Object.defineProperty(proto, 'format', {
    configurable: true,
    get(this: Intl.DateTimeFormat) {
      counter.count++;
      return desc.get!.call(this);
    },
  });
  return counter;
}

beforeAll(async () => {
  await import('./agent-log-viewer.js');
  await import('./agent-message-viewer.js');
  await import('./unified-log-viewer.js');
});

beforeEach(() => {
  frames = [];
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => frames.push(cb));
  vi.stubGlobal(
    'cancelAnimationFrame',
    vi.fn((h: number) => {
      frames[h - 1] = (): void => {};
    })
  );
  setPreferredTimeZone('Asia/Tokyo');
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  setPreferredTimeZone('');
});

const BASE = Date.parse('2026-09-23T15:00:00.000Z');

function iso(i: number): string {
  return new Date(BASE + i * 1000).toISOString();
}

function logLine(i: number, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    timestamp: iso(i),
    severity: 'INFO',
    message: `line ${i}`,
    insertId: `i${i}`,
    ...extra,
  };
}

function hubMessage(i: number): Record<string, unknown> {
  return {
    id: `m${i}`,
    projectId: 'p1',
    sender: 'user:alice',
    senderId: 'u1',
    recipient: 'agent:bob',
    recipientId: 'a1',
    msg: `hello ${i}`,
    type: 'instruction',
    agentId: 'a1',
    createdAt: iso(i),
  };
}

interface ViewerCase {
  name: string;
  mount: () => Promise<AnyEl>;
  /** Emits stream line `i` on the viewer's (fake) EventSource. */
  emit: (source: FakeEventSource, i: number) => void;
  /** Ingests line `i` without a stream (the fetch path). */
  ingest: (el: AnyEl, i: number) => void;
  rowSelector: string;
  /** Selector, inside a row, of the cell holding `line <i>` (the row's key text). */
  keyCell: string;
  /** Selector of the row time cell, and line 0's time in UTC. */
  timeSelector: string;
  utcTime0: string;
  /** Row text order for lines emitted with these indices (display order). */
  rowKeys: (el: AnyEl) => string[];
}

/** Row node per key text (`line <i>`), to check node identity across renders. */
function rowsByKey(el: AnyEl, c: ViewerCase): Map<string, Element> {
  const map = new Map<string, Element>();
  for (const row of el.shadowRoot.querySelectorAll(c.rowSelector) as NodeListOf<Element>) {
    const text = (row.querySelector(c.keyCell)?.textContent ?? '').trim();
    map.set(text.replace('hello ', 'line '), row);
  }
  return map;
}

async function settle(el: AnyEl): Promise<void> {
  flushFrames();
  await el.updateComplete;
}

const cases: ViewerCase[] = [
  {
    name: 'scion-agent-log-viewer',
    async mount(): Promise<AnyEl> {
      const el: AnyEl = document.createElement('scion-agent-log-viewer');
      el.agentId = 'a1';
      document.body.appendChild(el);
      await el.updateComplete;
      el.startStream();
      await el.updateComplete;
      return el;
    },
    emit: (source, i) => source.emit('log', logLine(i)),
    ingest: (el, i) => el.mergeEntries([logLine(i)]),
    rowSelector: 'tr.log-row',
    keyCell: 'td.msg',
    timeSelector: 'td.ts',
    utcTime0: '15:00:00.000',
    rowKeys: (el) =>
      Array.from(el.shadowRoot.querySelectorAll('td.msg') as NodeListOf<HTMLElement>).map((n) =>
        (n.textContent ?? '').trim()
      ),
  },
  {
    name: 'scion-unified-log-viewer',
    async mount(): Promise<AnyEl> {
      const el: AnyEl = document.createElement('scion-unified-log-viewer');
      document.body.appendChild(el);
      await el.updateComplete;
      el.startStream();
      await el.updateComplete;
      return el;
    },
    emit: (source, i) => source.emit('log', logLine(i, { source: 'hub' })),
    ingest: (el, i) => el.mergeEntries([logLine(i, { source: 'hub' })]),
    rowSelector: '.log-row',
    keyCell: '.msg',
    timeSelector: '.log-row .ts',
    utcTime0: '15:00:00.000',
    rowKeys: (el) =>
      Array.from(el.shadowRoot.querySelectorAll('.log-row .msg') as NodeListOf<HTMLElement>).map(
        (n) => (n.textContent ?? '').trim()
      ),
  },
  {
    name: 'scion-agent-message-viewer',
    async mount(): Promise<AnyEl> {
      const el: AnyEl = document.createElement('scion-agent-message-viewer');
      el.agentId = 'a1';
      document.body.appendChild(el);
      await el.updateComplete;
      el.startStream();
      await el.updateComplete;
      return el;
    },
    emit: (source, i) => source.emit('message', hubMessage(i)),
    ingest: (el, i) => el.mergeHubMessages([hubMessage(i)]),
    rowSelector: '.message-row',
    keyCell: '.msg-body',
    timeSelector: '.msg-time',
    utcTime0: '15:00:00',
    rowKeys: (el) =>
      Array.from(el.shadowRoot.querySelectorAll('.msg-body') as NodeListOf<HTMLElement>).map((n) =>
        (n.textContent ?? '').trim().replace('hello ', 'line ')
      ),
  },
];

/** Display order: the agent log and message viewers list newest first. */
function expectedOrder(name: string, indices: number[]): string[] {
  const sorted = [...indices].sort((a, b) => a - b).map((i) => `line ${i}`);
  return name === 'scion-unified-log-viewer' ? sorted : sorted.reverse();
}

describe.each(cases.map((c) => [c.name, c] as const))('%s streaming', (_name, c) => {
  it('500 streamed lines inside one frame produce exactly 1 render', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    expect(source).toBeDefined();
    const render = vi.spyOn(el, 'render');

    for (let i = 0; i < LINES; i++) {
      c.emit(source, i);
      // Lets Lit run any update a line requested on its own; with per-line
      // rendering this loop alone would render 500 times.
      await el.updateComplete;
    }
    expect(render).toHaveBeenCalledTimes(0);

    await settle(el);
    expect(render).toHaveBeenCalledTimes(1);
    expect(el.shadowRoot.querySelectorAll(c.rowSelector)).toHaveLength(LINES);
  });

  it('Intl.DateTimeFormat format count equals new entries, not entries x renders', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    const formats = countFormats();
    try {
      for (let i = 0; i < 100; i++) {
        c.emit(source, i);
        // Several frames, so the existing rows are rendered again and again.
        if (i % 10 === 9) await settle(el);
      }
      await settle(el);
      expect(formats.count).toBe(100 * FORMATS_PER_ENTRY);

      // Re-renders without new entries format nothing.
      el.requestUpdate();
      await el.updateComplete;
      el.requestUpdate();
      await el.updateComplete;
      expect(formats.count).toBe(100 * FORMATS_PER_ENTRY);

      // 5 more entries: 5 entries' worth of formatting, not 105.
      for (let i = 100; i < 105; i++) c.emit(source, i);
      await settle(el);
      expect(formats.count).toBe(105 * FORMATS_PER_ENTRY);

      // Duplicates (same insertId / message id) are ignored entirely.
      c.emit(source, 3);
      await settle(el);
      expect(formats.count).toBe(105 * FORMATS_PER_ENTRY);
    } finally {
      formats.restore();
    }
  });

  it('places out-of-order lines in timestamp order', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    const order = [5, 1, 9, 3, 7, 2];
    for (const i of order) c.emit(source, i);
    await settle(el);
    expect(c.rowKeys(el)).toEqual(expectedOrder(c.name, order));
  });

  it('keys rows: each line keeps its own row node when lines are inserted around it', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    for (const i of [0, 2, 4]) c.emit(source, i);
    await settle(el);
    const before = rowsByKey(el, c);
    expect([...before.keys()].sort()).toEqual(['line 0', 'line 2', 'line 4']);

    // One line in the middle (out of order) and one at the newest end. With
    // index-based (unkeyed) reuse, rows after the insertion point would show
    // other lines' text in the old nodes.
    c.emit(source, 1);
    c.emit(source, 5);
    await settle(el);
    const after = rowsByKey(el, c);
    expect(after.size).toBe(5);
    for (const [key, node] of before) {
      expect(after.get(key), key).toBe(node);
    }
    expect(c.rowKeys(el)).toEqual(expectedOrder(c.name, [0, 1, 2, 4, 5]));
  });

  it('re-formats each row once after a display zone change', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    for (let i = 0; i < 3; i++) c.emit(source, i);
    await settle(el);
    const formats = countFormats();
    try {
      setPreferredTimeZone('UTC');
      await el.updateComplete;
      expect(formats.count).toBe(3 * FORMATS_PER_ENTRY);
      el.requestUpdate();
      await el.updateComplete;
      expect(formats.count).toBe(3 * FORMATS_PER_ENTRY);
    } finally {
      formats.restore();
    }
    const times = Array.from(
      el.shadowRoot.querySelectorAll(c.timeSelector) as NodeListOf<HTMLElement>
    ).map((n) => (n.textContent ?? '').trim());
    expect(times).toContain(c.utcTime0);
  });

  it('cancels a pending frame on disconnect and renders skipped lines on reconnect', async () => {
    const el = await c.mount();
    const source = FakeEventSource.instances[0];
    const cancel = vi.mocked(cancelAnimationFrame);
    c.emit(source, 0);
    expect(frames).toHaveLength(1);

    el.remove();
    expect(cancel).toHaveBeenCalledTimes(1);
    // stopStream's own state change (streaming=false) may still render once;
    // let it finish before watching for the frame's render.
    await el.updateComplete;
    const render = vi.spyOn(el, 'render');
    // Only the unified viewer auto-scrolls after a frame.
    const scroll = typeof el.scrollToBottom === 'function' ? vi.spyOn(el, 'scrollToBottom') : null;
    flushFrames();
    await el.updateComplete;
    await Promise.resolve();
    expect(render).not.toHaveBeenCalled();
    if (scroll) expect(scroll).not.toHaveBeenCalled();

    document.body.appendChild(el);
    await el.updateComplete;
    expect(c.rowKeys(el)).toEqual(['line 0']);
  });

  it('renders lines ingested without a stream after a disconnect and reconnect', async () => {
    // Not streaming, so no stopStream state change renders the detached
    // element, and the cancelled frame leaves the line unrendered: only
    // connectedCallback's requestUpdate shows it.
    const el: AnyEl = document.createElement(c.name);
    el.agentId = 'a1';
    document.body.appendChild(el);
    await el.updateComplete;
    c.ingest(el, 0);
    expect(frames).toHaveLength(1);
    el.remove();
    flushFrames();
    await el.updateComplete;
    expect(c.rowKeys(el)).toEqual([]);

    document.body.appendChild(el);
    await el.updateComplete;
    expect(c.rowKeys(el)).toEqual(['line 0']);
  });
});

describe('date dividers with sorted ingest', () => {
  it('agent log viewer shows one divider per display-zone day, newest first', async () => {
    const el = await cases[0].mount();
    const source = FakeEventSource.instances[0];
    // 14:59:59Z is Sep 23 in Tokyo; 15:00:00Z is Sep 24.
    source.emit('log', logLine(-1));
    source.emit('log', logLine(0));
    source.emit('log', logLine(1));
    await settle(el);
    const dividers = Array.from(
      el.shadowRoot.querySelectorAll('td.date-divider') as NodeListOf<HTMLElement>
    ).map((n) => (n.textContent ?? '').trim());
    expect(dividers).toEqual(['Sep 24, 2026 (Asia/Tokyo)', 'Sep 23, 2026 (Asia/Tokyo)']);
  });
});
