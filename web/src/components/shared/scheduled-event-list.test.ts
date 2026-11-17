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

describe('scion-scheduled-event-list pagination (nextCursor)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function json(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;
  }

  function eventRow(id: string, agentName: string): Record<string, unknown> {
    return {
      id,
      projectId: 'p1',
      eventType: 'message',
      fireAt: '2026-10-01T15:00:00Z',
      payload: JSON.stringify({ agentName }),
      status: 'pending',
      createdAt: '2026-10-01T11:00:00Z',
      createdBy: 'u1',
    };
  }

  it('follows nextCursor across pages and renders every event', async () => {
    const mod = await import('./scheduled-event-list.js');
    const urls: string[] = [];
    const pages: Record<string, Record<string, unknown>[]> = {
      '': [eventRow('e1', 'agent-page-1')],
      cur2: [eventRow('e2', 'agent-page-2')],
      cur3: [eventRow('e3', 'agent-page-3')],
    };
    const cursors = Object.keys(pages);
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        urls.push(raw);
        const cursor = new URL(raw, 'http://x').searchParams.get('cursor') ?? '';
        const next = cursors[cursors.indexOf(cursor) + 1];
        return Promise.resolve(
          json({ events: pages[cursor] ?? [], ...(next ? { nextCursor: next } : {}) })
        );
      })
    );

    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    document.body.appendChild(el);
    await settle(el);

    const text = el.shadowRoot!.textContent ?? '';
    expect(text).toContain('agent-page-1');
    expect(text).toContain('agent-page-2');
    expect(text).toContain('agent-page-3');
    expect(urls).toHaveLength(3);
    expect(urls[0]).toContain(`limit=${(mod as AnyEl).SCHEDULED_EVENT_PAGE_SIZE}`);
    expect(urls[1]).toContain('cursor=cur2');
    expect(urls[2]).toContain('cursor=cur3');
  });

  it('surfaces the hub error and shows no partial list when a later page fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const cursor = new URL(raw, 'http://x').searchParams.get('cursor') ?? '';
        if (!cursor) {
          return Promise.resolve(
            json({ events: [eventRow('e1', 'agent-page-1')], nextCursor: 'cur2' })
          );
        }
        return Promise.resolve(
          json({ error: { code: 'internal', message: 'events page 2 failed' } }, 500)
        );
      })
    );

    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    document.body.appendChild(el);
    await settle(el);

    expect(el.shadowRoot!.querySelector('tbody')).toBeNull();
    expect(el.shadowRoot!.querySelector('.error-details')?.textContent).toContain(
      'events page 2 failed'
    );
  });
});

// A slower, older load must not overwrite a newer one or keep walking pages (O2).
describe('scion-scheduled-event-list overlapping loads', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  function eventRow(id: string, agentName: string): Record<string, unknown> {
    return {
      id,
      projectId: 'p1',
      eventType: 'message',
      fireAt: '2026-10-01T15:00:00Z',
      payload: JSON.stringify({ agentName }),
      status: 'pending',
      createdAt: '2026-10-01T11:00:00Z',
      createdBy: 'u1',
    };
  }

  async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;
  }

  function deferredFetch(): {
    calls: { method: string; url: string; resolve: (r: Response) => void }[];
  } {
    const calls: { method: string; url: string; resolve: (r: Response) => void }[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(
        (input: RequestInfo | URL, init?: RequestInit) =>
          new Promise<Response>((resolve) => {
            const url =
              typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
            calls.push({ method: init?.method ?? 'GET', url, resolve });
          })
      )
    );
    return { calls };
  }

  async function startTwoLoads(): Promise<{
    el: AnyEl;
    calls: ReturnType<typeof deferredFetch>['calls'];
    loadB: Promise<void>;
  }> {
    await import('./scheduled-event-list.js');
    const { calls } = deferredFetch();
    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    document.body.appendChild(el); // load A
    await el.updateComplete;
    const loadB = el.loadEvents() as Promise<void>;
    await new Promise((r) => setTimeout(r, 0));
    expect(calls).toHaveLength(2);
    return { el, calls, loadB };
  }

  it('keeps the newer result when the older load resolves last', async () => {
    const { el, calls, loadB } = await startTwoLoads();
    calls[1]!.resolve(jsonResponse({ events: [eventRow('b', 'newer-event-agent')] }));
    await loadB;
    await settle(el);
    calls[0]!.resolve(jsonResponse({ events: [eventRow('a', 'older-event-agent')] }));
    await settle(el);

    const text = el.shadowRoot!.textContent ?? '';
    expect(text).toContain('newer-event-agent');
    expect(text).not.toContain('older-event-agent');
    expect(el.loading).toBe(false);
  });

  it('ignores an older load that fails after the newer one succeeded', async () => {
    const { el, calls, loadB } = await startTwoLoads();
    calls[1]!.resolve(jsonResponse({ events: [eventRow('b', 'newer-event-agent')] }));
    await loadB;
    await settle(el);
    calls[0]!.resolve(jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500));
    await settle(el);

    expect(el.shadowRoot!.textContent ?? '').toContain('newer-event-agent');
    expect(el.shadowRoot!.querySelector('.error-details')).toBeNull();
    expect(el.loading).toBe(false);
  });

  it('stops fetching further pages of a superseded multi-page load', async () => {
    const { el, calls, loadB } = await startTwoLoads();
    calls[1]!.resolve(jsonResponse({ events: [eventRow('b', 'newer-event-agent')] }));
    await loadB;
    await settle(el);
    calls[0]!.resolve(
      jsonResponse({ events: [eventRow('a', 'older-event-agent')], nextCursor: 'stale-page-2' })
    );
    await settle(el);

    expect(calls).toHaveLength(2);
    expect(el.shadowRoot!.textContent ?? '').toContain('newer-event-agent');
    expect(el.shadowRoot!.textContent ?? '').not.toContain('older-event-agent');
    expect(el.shadowRoot!.querySelector('.error-details')).toBeNull();
  });
});

describe('scion-scheduled-event-list exec authoring and detail (ptone/scion#7886)', () => {
  afterEach(async () => {
    document.body.innerHTML = '';
    const ff = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    ff.resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;
  }

  function json(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  it('hides the event type selector when hub.exec_schedules is off and shows it when on', async () => {
    const { setFeatureFlag } = await import('../../utils/feature-flags.js');
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json({ events: [] })))
    );

    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    el.compact = true;
    document.body.appendChild(el);
    await settle(el);

    el.openCreateDialog();
    await settle(el);

    expect(el.shadowRoot!.querySelector('sl-select.event-type-select')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-textarea[label="Message"]')).not.toBeNull();

    setFeatureFlag('hub.exec_schedules', true);
    await settle(el);

    const typeSelect = el.shadowRoot!.querySelector('sl-select.event-type-select');
    expect(typeSelect).not.toBeNull();
    const values = Array.from(typeSelect!.querySelectorAll('sl-option')).map((o) =>
      o.getAttribute('value')
    );
    expect(values).toEqual(['message', 'exec']);
  });

  it('authors an exec event via agent picker and argv editor, validates bounds, and surfaces hub 400s', async () => {
    const { setFeatureFlag } = await import('../../utils/feature-flags.js');
    setFeatureFlag('hub.exec_schedules', true);

    const posts: Record<string, unknown>[] = [];
    let postFailMessage: string | null = null;

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const u = new URL(raw, 'http://x');
        if (init?.method === 'POST' && u.pathname === '/api/v1/projects/p1/scheduled-events') {
          posts.push(JSON.parse(init.body as string) as Record<string, unknown>);
          if (postFailMessage) {
            return Promise.resolve(
              json({ error: { code: 'invalid_input', message: postFailMessage } }, 400)
            );
          }
          return Promise.resolve(json({ id: 'evt-new' }, 201));
        }
        if (u.pathname === '/api/v1/projects/p1/agents') {
          return Promise.resolve(
            json({
              agents: [
                {
                  id: 'agent-ok',
                  name: 'worker-ok',
                  phase: 'running',
                  _capabilities: { actions: ['read', 'attach'] },
                },
                {
                  id: 'agent-no-attach',
                  name: 'worker-locked',
                  phase: 'running',
                  _capabilities: { actions: ['read'] },
                },
              ],
            })
          );
        }
        return Promise.resolve(json({ events: [] }));
      })
    );

    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    el.projectId = 'p1';
    el.compact = true;
    document.body.appendChild(el);
    await settle(el);

    el.openCreateDialog();
    await settle(el);

    const typeSelect = el.shadowRoot!.querySelector(
      'sl-select.event-type-select'
    ) as HTMLElement & {
      value: string;
    };
    typeSelect.value = 'exec';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await settle(el);

    // Message fields are hidden when exec is selected.
    expect(el.shadowRoot!.querySelector('sl-textarea[label="Message"]')).toBeNull();

    const agentSelect = el.shadowRoot!.querySelector(
      'sl-select.exec-agent-select'
    ) as HTMLElement & {
      value: string;
    };
    expect(agentSelect).not.toBeNull();
    const lockedOpt = agentSelect.querySelector('sl-option[value="agent-no-attach"]');
    expect(lockedOpt?.hasAttribute('disabled')).toBe(true);

    agentSelect.value = 'agent-ok';
    agentSelect.dispatchEvent(new Event('sl-change'));
    await settle(el);

    // Submitting with empty argv[0] is blocked client-side before POST.
    await el.handleCreate(new Event('submit'));
    await settle(el);
    expect(posts).toHaveLength(0);
    expect(el.shadowRoot!.querySelector('.dialog-error')?.textContent).toMatch(/argv\[0\]/i);

    // Add two more argument rows, remove the middle one, keep explicit empty final arg.
    const addBtn = () => el.shadowRoot!.querySelector('sl-button.exec-argv-add') as HTMLElement;
    addBtn().click();
    await settle(el);
    addBtn().click();
    await settle(el);
    expect(el.shadowRoot!.querySelectorAll('.exec-argv-input')).toHaveLength(3);

    const setArg = (idx: number, val: string) => {
      const input = el.shadowRoot!.querySelector(
        `sl-input.exec-argv-input[data-arg-index="${idx}"]`
      ) as HTMLInputElement;
      input.value = val;
      input.dispatchEvent(new Event('sl-input'));
    };
    setArg(0, '/usr/bin/git');
    setArg(1, 'remove-me');
    setArg(2, '');
    await settle(el);

    const removeButtons = el.shadowRoot!.querySelectorAll('sl-icon-button.exec-argv-remove');
    (removeButtons[1] as HTMLElement).click();
    await settle(el);
    expect(el.shadowRoot!.querySelectorAll('.exec-argv-input')).toHaveLength(2);

    const timeoutInput = el.shadowRoot!.querySelector(
      'sl-input.exec-timeout-input'
    ) as HTMLInputElement;
    timeoutInput.value = '120';
    timeoutInput.dispatchEvent(new Event('sl-input'));
    await settle(el);

    // Hub 400 surfaces cleanly in dialogError and keeps dialog open.
    postFailMessage = 'hub rejected fireIn';
    await el.handleCreate(new Event('submit'));
    await settle(el);
    expect(posts).toHaveLength(1);
    expect(el.shadowRoot!.querySelector('.dialog-error')?.textContent).toContain(
      'hub rejected fireIn'
    );
    expect(el.dialogOpen).toBe(true);

    // Retry succeeds and sends clean exec body with no message/wake/interrupt fields.
    postFailMessage = null;
    await el.handleCreate(new Event('submit'));
    await settle(el);
    expect(posts).toHaveLength(2);
    expect(posts[1]).toEqual({
      eventType: 'exec',
      agentId: 'agent-ok',
      command: ['/usr/bin/git', ''],
      timeoutSeconds: 120,
      fireIn: '30m',
    });
    expect(el.dialogOpen).toBe(false);
  });

  it('renders revealed and redacted exec event details with execSummary, no-output note, and no output view', async () => {
    const sentinel = 'REDACTED-EVENT-SECRET-99';
    const el = document.createElement('scion-scheduled-event-list') as AnyEl;
    document.body.appendChild(el);
    await el.updateComplete;
    el.loading = false;
    el.events = [
      {
        id: 'evt-revealed',
        projectId: 'p1',
        eventType: 'exec',
        fireAt: '2026-10-01T15:00:00Z',
        firedAt: '2026-10-01T15:00:01Z',
        payload: JSON.stringify({
          agentId: 'a-1',
          agentName: 'worker',
          command: ['pytest', '-q'],
          timeoutSeconds: 90,
        }),
        status: 'fired',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'alice@example.com',
        execSummary: {
          outcome: 'nonzero',
          exitCode: 2,
          durationMs: 1500,
          outputBytes: 4096,
          startedAt: '2026-10-01T15:00:01Z',
          finishedAt: '2026-10-01T15:00:02.5Z',
        },
      },
      {
        id: 'evt-redacted',
        projectId: 'p1',
        eventType: 'exec',
        fireAt: '2026-10-01T16:00:00Z',
        payload: JSON.stringify({
          agentId: 'a-2',
          agentName: 'secret-worker',
          timeoutSeconds: 60,
          commandRedacted: true,
          argc: 4,
          commandSha256: 'abcd1234ef56',
          command: ['sh', '-c', sentinel],
        }),
        status: 'skipped',
        error: 'agent not running (phase=stopped)',
        createdAt: '2026-10-01T11:00:00Z',
        createdBy: 'bob@example.com',
      },
    ];
    await el.updateComplete;

    const rows = el.shadowRoot!.querySelectorAll('tbody tr');
    (rows[0] as HTMLElement).click();
    await el.updateComplete;

    let dialog = el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]')!;
    let text = dialog.textContent ?? '';
    expect(text).toContain('pytest');
    expect(text).toContain('-q');
    expect(text).toContain('90s');
    expect(text).toContain('nonzero');
    expect(text).toContain('2');
    expect(text).toContain('1.5s');
    expect(text).toContain("Command output isn't stored; check the agent's own logs.");
    expect(dialog.querySelector('.exec-redacted-notice')).toBeNull();
    // D2: no output view or raw output bytes dump panel
    expect(dialog.querySelector('pre, textarea, scion-log-viewer')).toBeNull();

    (rows[1] as HTMLElement).click();
    await el.updateComplete;

    dialog = el.shadowRoot!.querySelector('sl-dialog[label="Scheduled Event"]')!;
    text = dialog.textContent ?? '';
    expect(dialog.querySelector('.exec-redacted-notice')).not.toBeNull();
    expect(text).toContain('4 args');
    expect(text).toContain('sha256: abcd1234ef56');
    expect(text).toContain("Command output isn't stored; check the agent's own logs.");
    expect(text).not.toContain(sentinel);
  });
});
