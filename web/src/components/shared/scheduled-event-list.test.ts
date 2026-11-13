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
 * Tests for <scion-scheduled-event-list>'s create-dialog zone label
 * (tz-refactor task 11, review round 4, R4-1), and its fire-time cell,
 * whose tooltip shows the absolute instant in the display zone
 * (tz-refactor task 19).
 */

import { describe, it, expect, afterEach, vi } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./scheduled-event-list.js');
type ScionScheduledEventList = import('./scheduled-event-list.js').ScionScheduledEventList;

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

describe('scion-scheduled-event-list create dialog zone label (review R4-1)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('updates the "Times in" help text when the effective zone changes after mount', async () => {
    // No projectId, so connectedCallback's loadEvents() returns immediately
    // without issuing a fetch (and `loading` never flips to `false`) —
    // `compact` mode renders the dialog regardless of the loading state,
    // unlike the full-page layout, which gates it behind `!loading`.
    const el = document.createElement('scion-scheduled-event-list') as ScionScheduledEventList;
    el.compact = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const comp = el as AnyEl;
    comp.dialogOpen = true;
    comp.dialogTimingMode = 'at';
    await comp.updateComplete;

    const dateInput = () => el.shadowRoot!.querySelector('sl-input[label="Date & Time"]')!;
    expect(dateInput().getAttribute('help-text')).toBe('Times in: UTC');

    setPreferredTimeZone('Asia/Tokyo');
    await comp.updateComplete;

    expect(dateInput().getAttribute('help-text')).toBe('Times in: Asia/Tokyo');
  });
});

describe('scion-scheduled-event-list fire time in the display zone', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
    vi.useRealTimers();
  });

  async function mountWith(events: Record<string, unknown>[]): Promise<AnyEl> {
    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    document.body.appendChild(el);
    await el.updateComplete;
    el.loading = false;
    el.events = events;
    await el.updateComplete;
    return el;
  }

  it('labels a pending fire time with the absolute instant in the preferred zone', async () => {
    // Browser zone is pinned to UTC; 15:00Z is midnight in Asia/Tokyo.
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-10-01T12:00:00Z'));
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mountWith([
      {
        id: 'e1',
        projectId: 'p1',
        eventType: 'message',
        fireAt: '2026-10-01T15:00:00Z',
        payload: '{"agentName":"worker"}',
        status: 'pending',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'u1',
      },
    ]);

    const cell = el.shadowRoot!.querySelector('tbody tr td:nth-child(3) span');
    expect(cell?.textContent?.trim()).toBe('in 3 hours');
    expect(cell?.getAttribute('title')).toBe('Oct 2, 2026, 00:00 (Asia/Tokyo)');
  });

  it('shows days for a far fire time and "now" for an overdue one', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-10-01T12:00:00Z'));
    const event = (id: string, fireAt: string) => ({
      id,
      projectId: 'p1',
      eventType: 'message',
      fireAt,
      payload: '{"agentName":"worker"}',
      status: 'pending',
      createdAt: '2026-10-01T11:00:00Z',
      createdBy: 'u1',
    });
    const el = await mountWith([
      event('far', '2026-10-04T12:00:00Z'),
      event('late', '2026-10-01T11:00:00Z'),
    ]);

    const cells = Array.from(
      el.shadowRoot!.querySelectorAll('tbody tr td:nth-child(3) span') as NodeListOf<Element>
    ).map((c) => c.textContent?.trim());
    expect(cells).toEqual(['in 3 days', 'now']);
  });
});

describe('scion-scheduled-event-list create dialog wake option (ptone/scion#7734)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  async function createWith(wake: boolean): Promise<Record<string, unknown>> {
    const posts: Record<string, unknown>[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit): Promise<Response> => {
        if (init?.method === 'POST') {
          posts.push(JSON.parse(init.body as string) as Record<string, unknown>);
          return Promise.resolve(
            new Response('{}', { status: 201, headers: { 'Content-Type': 'application/json' } })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ events: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    el.compact = true;
    document.body.appendChild(el);
    await el.updateComplete;

    el.openCreateDialog();
    await el.updateComplete;
    const box = Array.from(el.shadowRoot!.querySelectorAll('label.checkbox-label')).find((l) =>
      (l as HTMLElement).textContent?.includes('Wake agent')
    ) as HTMLElement | undefined;
    expect(box, 'the dialog offers a Wake agent option').toBeDefined();
    expect((box!.querySelector('input') as HTMLInputElement).checked).toBe(false);

    el.dialogAgent = 'worker';
    el.dialogMessage = 'recheck';
    el.dialogWake = wake;
    await el.handleCreate(new Event('submit'));
    expect(posts).toHaveLength(1);
    return posts[0];
  }

  it('sends wake: true when the option is checked', async () => {
    expect((await createWith(true)).wake).toBe(true);
  });

  it('sends wake: false by default', async () => {
    expect((await createWith(false)).wake).toBe(false);
  });
});

describe('scion-scheduled-event-list detail dialog wake and interrupt (ptone/scion#7791)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  async function mountWith(events: Record<string, unknown>[]): Promise<AnyEl> {
    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    document.body.appendChild(el);
    await el.updateComplete;
    el.loading = false;
    el.events = events;
    await el.updateComplete;
    return el;
  }

  function detailRowText(dialog: Element | null, label: string): string | undefined {
    if (!dialog) return undefined;
    const row = Array.from(dialog.querySelectorAll('.detail-row')).find(
      (r) => r.querySelector('strong')?.textContent?.trim() === label
    );
    return row?.textContent?.replace(label, '').trim();
  }

  it('opens event detail on row click showing Wake: Yes and Interrupt: Yes when true', async () => {
    const el = await mountWith([
      {
        id: 'evt-wake',
        projectId: 'p1',
        eventType: 'message',
        fireAt: '2026-10-01T15:00:00Z',
        payload: '{"agentName":"worker","message":"recheck","wake":true,"interrupt":true}',
        status: 'pending',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'alice@example.com',
      },
    ]);

    const row = el.shadowRoot!.querySelector('tbody tr') as HTMLElement;
    row.click();
    await el.updateComplete;

    const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.hasAttribute('open')).toBe(true);
    expect(detailRowText(dialog, 'Target Agent:')).toBe('worker');
    expect(detailRowText(dialog, 'Message:')).toBe('recheck');
    expect(detailRowText(dialog, 'Wake:')).toBe('Yes');
    expect(detailRowText(dialog, 'Interrupt:')).toBe('Yes');
  });

  it('shows Wake: No and Interrupt: No when omitted from message payload', async () => {
    const el = await mountWith([
      {
        id: 'evt-default',
        projectId: 'p1',
        eventType: 'message',
        fireAt: '2026-10-01T15:00:00Z',
        payload: '{"agentName":"worker","message":"plain"}',
        status: 'pending',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'alice@example.com',
      },
    ]);

    const row = el.shadowRoot!.querySelector('tbody tr') as HTMLElement;
    row.click();
    await el.updateComplete;

    const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]');
    expect(dialog).not.toBeNull();
    expect(detailRowText(dialog, 'Wake:')).toBe('No');
    expect(detailRowText(dialog, 'Interrupt:')).toBe('No');
  });

  it('omits Wake and Interrupt rows for non-message events', async () => {
    const el = await mountWith([
      {
        id: 'evt-dispatch',
        projectId: 'p1',
        eventType: 'dispatch_agent',
        fireAt: '2026-10-01T15:00:00Z',
        payload: '{"agentName":"worker","template":"default","task":"build"}',
        status: 'pending',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'alice@example.com',
      },
    ]);

    const row = el.shadowRoot!.querySelector('tbody tr') as HTMLElement;
    row.click();
    await el.updateComplete;

    const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]');
    expect(dialog).not.toBeNull();
    expect(detailRowText(dialog, 'Wake:')).toBeUndefined();
    expect(detailRowText(dialog, 'Interrupt:')).toBeUndefined();
  });

  it('does not open detail dialog when clicking the row cancel action', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ events: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
    const el = await mountWith([
      {
        id: 'evt-cancel',
        projectId: 'p1',
        eventType: 'message',
        fireAt: '2026-10-01T15:00:00Z',
        payload: '{"agentName":"worker","message":"recheck","wake":true}',
        status: 'pending',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'alice@example.com',
      },
    ]);

    const cancelBtn = el.shadowRoot!.querySelector(
      'tbody tr sl-icon-button[label="Cancel"]'
    ) as HTMLElement;
    expect(cancelBtn).not.toBeNull();
    cancelBtn.click();
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]')).toBeNull();
  });
});
