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
 * Tests for the cron presentation in the recurring schedule list: a
 * schedule whose stored expression carries a CRON_TZ=/TZ= prefix shows a
 * "zone prefix not supported" badge, and each schedule shows the time zone
 * its expression is evaluated in (UTC when it has none). Also covers
 * next-run display: the next run is shown in the display zone with a zone
 * label, beside the cron expression.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';
import { requestBodyText, requestUrl } from '../../client/__fixtures__/request-url.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let mod: any;

function schedule(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    id: 'sched-1',
    projectId: 'proj-1',
    name: 'standup',
    cronExpr: '0 9 * * *',
    eventType: 'message',
    payload: '{"agentName":"worker","message":"hi"}',
    status: 'active',
    runCount: 0,
    errorCount: 0,
    createdAt: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
}

async function mount(schedules: Record<string, unknown>[]): Promise<HTMLElement> {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ schedules, totalCount: schedules.length }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    )
  );
  const el = document.createElement('scion-schedule-list') as HTMLElement & {
    projectId: string;
    updateComplete: Promise<unknown>;
  };
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function rowFor(el: HTMLElement, name: string): HTMLTableRowElement {
  const root = el.shadowRoot as ShadowRoot;
  const row = Array.from(root.querySelectorAll('tbody tr')).find((tr) =>
    (tr.textContent ?? '').includes(name)
  );
  if (!row) throw new Error(`row ${name} not rendered`);
  return row as HTMLTableRowElement;
}

describe('hasCronZonePrefix', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  it('matches the hub check: case-sensitive prefix on the untrimmed expression', () => {
    expect(mod.hasCronZonePrefix('CRON_TZ=Asia/Tokyo 0 9 * * *')).toBe(true);
    expect(mod.hasCronZonePrefix('TZ=Asia/Tokyo 0 9 * * *')).toBe(true);
    expect(mod.hasCronZonePrefix('0 9 * * *')).toBe(false);
    expect(mod.hasCronZonePrefix('cron_tz=Asia/Tokyo 0 9 * * *')).toBe(false);
    expect(mod.hasCronZonePrefix(' TZ=Asia/Tokyo 0 9 * * *')).toBe(false);
  });
});

describe('scion-schedule-list zone-prefix badge', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows the badge only on rows with a zone-prefixed expression', async () => {
    const el = await mount([
      schedule({
        id: 'a',
        name: 'legacy-cron-tz',
        cronExpr: 'CRON_TZ=Asia/Tokyo 0 9 * * *',
        status: 'paused',
      }),
      schedule({
        id: 'b',
        name: 'legacy-tz',
        cronExpr: 'TZ=Asia/Tokyo 0 9 * * *',
        status: 'paused',
      }),
      schedule({ id: 'c', name: 'plain-utc', cronExpr: '0 9 * * *' }),
    ]);

    for (const name of ['legacy-cron-tz', 'legacy-tz']) {
      const badge = rowFor(el, name).querySelector('.badge.zone-prefix');
      expect(badge, name).not.toBeNull();
      expect(badge?.textContent?.trim()).toBe(mod.ZONE_PREFIX_BADGE_LABEL);
    }
    // Positive control above uses the same selector that must find nothing here.
    expect(rowFor(el, 'plain-utc').querySelector('.badge.zone-prefix')).toBeNull();
  });

  it('shows the badge in the detail dialog for a prefixed schedule', async () => {
    const el = await mount([
      schedule({
        id: 'a',
        name: 'legacy-cron-tz',
        cronExpr: 'CRON_TZ=Asia/Tokyo 0 9 * * *',
        status: 'paused',
      }),
    ]);
    rowFor(el, 'legacy-cron-tz').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.querySelector('.badge.zone-prefix')?.textContent?.trim()).toBe(
      mod.ZONE_PREFIX_BADGE_LABEL
    );
  });

  it('points the cron field at the time zone field, which defaults to UTC', async () => {
    const el = await mount([]);
    const root = el.shadowRoot as ShadowRoot;
    const cronInput = Array.from(root.querySelectorAll('sl-input')).find((i) =>
      (i.getAttribute('help-text') ?? '').includes('cron')
    );
    expect(cronInput?.getAttribute('help-text')).toContain('in the time zone below');
    const zoneInput = root.querySelector('sl-input.create-time-zone');
    expect(zoneInput?.getAttribute('placeholder')).toBe('UTC');
    expect(zoneInput?.getAttribute('help-text')).toContain('Empty means UTC');
  });
});

describe('scion-schedule-list next-run in the display zone', () => {
  // Vitest pins the browser zone to UTC; the preference below differs from
  // it, and the next run falls on midnight in that zone.
  const NOW = '2026-10-01T12:00:00Z';
  const NEXT_RUN = '2026-10-01T15:00:00Z'; // 00:00 on Oct 2 in Asia/Tokyo

  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function useFakeNow(): void {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date(NOW));
  }

  it('shows the next run in the preferred zone, 24-hour, with a zone label', async () => {
    useFakeNow();
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount([schedule({ name: 'nightly', nextRunAt: NEXT_RUN })]);

    const row = rowFor(el, 'nightly');
    expect(row.textContent).toContain('in 3 hours');
    const absolute = row.querySelector('.next-run-absolute');
    expect(absolute?.textContent?.trim()).toBe('Oct 2, 2026, 00:00 (Asia/Tokyo)');
    // The cron column names the schedule's zone (UTC here), next to the
    // next run in the display zone.
    const headers = Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('thead th')).map(
      (th) => th.textContent?.trim()
    );
    expect(headers).toContain('Cron');
    expect(row.querySelector('.schedule-zone')?.textContent?.trim()).toBe('UTC');
  });

  it('shows a next run days away in days, not hours, beside the labelled absolute', async () => {
    useFakeNow();
    setPreferredTimeZone('Asia/Tokyo');
    // Three days after NEXT_RUN: still midnight in Tokyo.
    const el = await mount([schedule({ name: 'weekly', nextRunAt: '2026-10-04T15:00:00Z' })]);
    const row = rowFor(el, 'weekly');
    expect(row.textContent).toContain('in 3 days');
    expect(row.querySelector('.next-run-absolute')?.textContent?.trim()).toBe(
      'Oct 5, 2026, 00:00 (Asia/Tokyo)'
    );
  });

  it('shows "now" for an overdue next run', async () => {
    useFakeNow();
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount([schedule({ name: 'late', nextRunAt: '2026-10-01T11:00:00Z' })]);
    const row = rowFor(el, 'late');
    const cells = Array.from(row.querySelectorAll('td')).map((td) => td.textContent ?? '');
    const nextRunCell = cells.find((c) => c.includes('(Asia/Tokyo)')) ?? '';
    expect(nextRunCell).toMatch(/^\s*now\b/);
    expect(nextRunCell).not.toContain('ago');
  });

  it('re-renders the next run when the display zone changes', async () => {
    useFakeNow();
    const el = await mount([schedule({ name: 'nightly', nextRunAt: NEXT_RUN })]);
    const absolute = () =>
      rowFor(el, 'nightly').querySelector('.next-run-absolute')?.textContent?.trim();
    expect(absolute()).toBe('Oct 1, 2026, 15:00 (UTC)');

    setPreferredTimeZone('Asia/Tokyo');
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    expect(absolute()).toBe('Oct 2, 2026, 00:00 (Asia/Tokyo)');
  });

  it('shows the zoned next run in the detail dialog beside the cron and its zone', async () => {
    useFakeNow();
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount([schedule({ name: 'nightly', nextRunAt: NEXT_RUN })]);
    rowFor(el, 'nightly').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog?.textContent).toContain('Cron:');
    expect(dialog?.querySelector('.schedule-zone')?.textContent?.trim()).toBe('UTC');
    expect(dialog?.querySelector('.next-run-absolute')?.textContent?.trim()).toBe(
      'Oct 2, 2026, 00:00 (Asia/Tokyo)'
    );
  });

  it('shows no next run for a paused schedule', async () => {
    useFakeNow();
    const el = await mount([
      schedule({ name: 'paused-one', status: 'paused', nextRunAt: NEXT_RUN }),
    ]);
    expect(rowFor(el, 'paused-one').querySelector('.next-run-absolute')).toBeNull();
  });
});

// ptone/scion#2643: the list follows nextCursor instead of showing page one only.
describe('scion-schedule-list pagination', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

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

  /** Serves `pages` by cursor ("" for the first); a page set to a Response is returned as is. */
  function stubPages(pages: Record<string, Record<string, unknown>[] | Response>): string[] {
    const urls: string[] = [];
    const cursors = Object.keys(pages);
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        urls.push(url);
        const cursor = new URL(url, 'http://x').searchParams.get('cursor') ?? '';
        const page = pages[cursor];
        if (page instanceof Response) return Promise.resolve(page);
        const next = cursors[cursors.indexOf(cursor) + 1];
        return Promise.resolve(json({ schedules: page, ...(next ? { nextCursor: next } : {}) }));
      })
    );
    return urls;
  }

  async function mountList(): Promise<HTMLElement> {
    const el = document.createElement('scion-schedule-list') as HTMLElement & {
      projectId: string;
      updateComplete: Promise<unknown>;
    };
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  it('loads every page and renders all rows', async () => {
    const urls = stubPages({
      '': [schedule({ id: 'a', name: 'first-page' })],
      c2: [schedule({ id: 'b', name: 'second-page' })],
      c3: [schedule({ id: 'c', name: 'third-page' })],
    });
    const el = await mountList();

    for (const name of ['first-page', 'second-page', 'third-page']) rowFor(el, name);
    expect(urls).toHaveLength(3);
    expect(urls[0]).toContain(`limit=${mod.SCHEDULE_PAGE_SIZE}`);
    expect(urls[0]).not.toContain('cursor=');
    expect(urls[1]).toContain('cursor=c2');
    expect(urls[2]).toContain('cursor=c3');
  });

  it('shows the hub error and no partial list when a later page fails', async () => {
    stubPages({
      '': [schedule({ id: 'a', name: 'first-page' })],
      c2: json({ error: { code: 'internal', message: 'store unavailable' } }, 500),
    });
    const el = await mountList();
    const root = el.shadowRoot as ShadowRoot;
    expect(root.querySelector('tbody')).toBeNull();
    expect(root.querySelector('.error-details')?.textContent).toContain('store unavailable');
  });
});

describe('buildScheduleEdit', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  const base = { name: 'standup', cronExpr: '0 9 * * *', status: 'active' };

  it('sends only the changed fields', () => {
    expect(
      mod.buildScheduleEdit(base, { name: 'standup', cronExpr: '0 10 * * *', resume: false })
    ).toEqual({ patch: { cronExpr: '0 10 * * *' } });
    expect(
      mod.buildScheduleEdit(base, { name: ' renamed ', cronExpr: '0 9 * * *', resume: false })
    ).toEqual({ patch: { name: 'renamed' } });
    expect(
      mod.buildScheduleEdit(base, { name: 'standup', cronExpr: '0 9 * * *', resume: false })
    ).toEqual({ patch: {} });
  });

  it('resumes only a paused schedule', () => {
    const paused = { ...base, cronExpr: 'CRON_TZ=Asia/Tokyo 0 9 * * *', status: 'paused' };
    expect(
      mod.buildScheduleEdit(paused, { name: 'standup', cronExpr: '0 0 * * *', resume: true })
    ).toEqual({ patch: { cronExpr: '0 0 * * *', status: 'active' } });
    expect(
      mod.buildScheduleEdit(base, { name: 'standup', cronExpr: '0 9 * * *', resume: true })
    ).toEqual({ patch: {} });
  });

  it('sends the time zone only when it changed, with UTC as empty', () => {
    const edit = (sched: Record<string, unknown>, timeZone: string): unknown =>
      mod.buildScheduleEdit(sched, {
        name: 'standup',
        cronExpr: '0 9 * * *',
        timeZone,
        resume: false,
      });
    expect(edit(base, ' America/Los_Angeles ')).toEqual({
      patch: { timeZone: 'America/Los_Angeles' },
    });
    expect(edit(base, '')).toEqual({ patch: {} });
    expect(edit(base, 'UTC')).toEqual({ patch: {} });
    const zoned = { ...base, timeZone: 'Europe/London' };
    expect(edit(zoned, 'Europe/London')).toEqual({ patch: {} });
    expect(edit(zoned, '')).toEqual({ patch: { timeZone: '' } });
    expect(edit(zoned, 'UTC')).toEqual({ patch: { timeZone: '' } });
    expect(edit(zoned, 'Asia/Tokyo')).toEqual({ patch: { timeZone: 'Asia/Tokyo' } });
  });

  it('rejects empty fields and a zone prefix', () => {
    for (const fields of [
      { name: ' ', cronExpr: '0 9 * * *', resume: false },
      { name: 'standup', cronExpr: '', resume: false },
      { name: 'standup', cronExpr: 'TZ=UTC 0 9 * * *', resume: false },
      { name: 'standup', cronExpr: 'CRON_TZ=Asia/Tokyo 0 9 * * *', resume: false },
    ]) {
      expect(mod.buildScheduleEdit(base, fields)).toHaveProperty('error');
    }
  });
});

describe('scion-schedule-list edit dialog', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  interface Call {
    method: string;
    url: string;
    body?: unknown;
  }

  /** A fake hub holding one schedule; PATCH applies the body, GET lists it. */
  async function mountWithHub(
    initial: Record<string, unknown>,
    patchResponse?: Response
  ): Promise<{ el: HTMLElement; calls: Call[] }> {
    let row = { ...initial };
    const calls: Call[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? 'GET';
        const url = requestUrl(input);
        const body = init?.body ? (JSON.parse(requestBodyText(init.body)) as unknown) : undefined;
        calls.push({ method, url, body });
        if (method === 'PATCH') {
          if (patchResponse) return Promise.resolve(patchResponse);
          row = { ...row, ...(body as Record<string, unknown>) };
          return Promise.resolve(new Response(JSON.stringify(row), { status: 200 }));
        }
        return Promise.resolve(
          new Response(JSON.stringify({ schedules: [row] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
    const el = document.createElement('scion-schedule-list') as HTMLElement & {
      projectId: string;
      updateComplete: Promise<unknown>;
    };
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    return { el, calls };
  }

  function root(el: HTMLElement): ShadowRoot {
    return el.shadowRoot as ShadowRoot;
  }

  async function openEdit(el: HTMLElement, name: string): Promise<HTMLElement> {
    const btn = rowFor(el, name).querySelector('sl-icon-button[label="Edit"]') as HTMLElement;
    btn.click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = root(el).querySelector('sl-dialog[label^="Edit Schedule"]') as HTMLElement;
    expect(dialog).not.toBeNull();
    return dialog;
  }

  function setInput(input: Element, value: string): void {
    (input as HTMLInputElement).value = value;
    input.dispatchEvent(new Event('sl-input'));
  }

  it('edits a zone-prefixed cron to UTC and resumes the paused schedule', async () => {
    const { el, calls } = await mountWithHub(
      schedule({
        id: 's-1',
        name: 'legacy',
        cronExpr: 'CRON_TZ=Asia/Tokyo 0 9 * * *',
        status: 'paused',
      })
    );
    const dialog = await openEdit(el, 'legacy');
    const cron = dialog.querySelector('sl-input.edit-cron') as HTMLInputElement;
    expect(cron.value).toBe('CRON_TZ=Asia/Tokyo 0 9 * * *');
    expect(cron.getAttribute('help-text')).toContain('not supported');

    setInput(cron, '0 0 * * *');
    const resume = dialog.querySelector('input.edit-resume') as HTMLInputElement;
    resume.checked = true;
    resume.dispatchEvent(new Event('change'));
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    (dialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });

    const patch = calls.find((c) => c.method === 'PATCH');
    expect(patch?.url).toBe('/api/v1/projects/proj-1/schedules/s-1');
    expect(patch?.body).toEqual({ cronExpr: '0 0 * * *', status: 'active' });
    // Closed and reloaded with the saved values.
    expect(root(el).querySelector('sl-dialog[label^="Edit Schedule"]')).toBeNull();
    expect(rowFor(el, 'legacy').querySelector('.badge.zone-prefix')).toBeNull();
    expect(rowFor(el, 'legacy').textContent).toContain('0 0 * * *');
  });

  it('blocks a zone-prefixed expression without calling the hub', async () => {
    const { el, calls } = await mountWithHub(schedule({ id: 's-1', name: 'plain' }));
    const dialog = await openEdit(el, 'plain');
    setInput(dialog.querySelector('sl-input.edit-cron')!, 'TZ=Europe/Paris 0 9 * * *');
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    (dialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    expect(calls.some((c) => c.method === 'PATCH')).toBe(false);
    expect(dialog.querySelector('.dialog-error')?.textContent).toContain('not supported');
  });

  it('keeps the dialog open with the hub error when the save fails', async () => {
    const { el } = await mountWithHub(
      schedule({ id: 's-1', name: 'plain' }),
      new Response(
        JSON.stringify({ error: { code: 'revision_conflict', message: 'refresh and retry' } }),
        { status: 409, headers: { 'Content-Type': 'application/json' } }
      )
    );
    const dialog = await openEdit(el, 'plain');
    setInput(dialog.querySelector('sl-input.edit-cron')!, '*/5 * * * *');
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    (dialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const open = root(el).querySelector('sl-dialog[label^="Edit Schedule"]');
    expect(open).not.toBeNull();
    expect(open?.querySelector('.dialog-error')?.textContent).toContain('refresh and retry');
  });

  it('opens from the detail dialog', async () => {
    const { el } = await mountWithHub(schedule({ id: 's-1', name: 'plain' }));
    rowFor(el, 'plain').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const edit = Array.from(
      root(el).querySelectorAll('sl-dialog[label^="Schedule:"] sl-button')
    ).find((b) => b.textContent?.trim() === 'Edit') as HTMLElement;
    edit.click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    expect(root(el).querySelector('sl-dialog[label^="Edit Schedule"]')).not.toBeNull();
    expect(root(el).querySelector('sl-dialog[label^="Schedule:"]')?.hasAttribute('open')).toBe(
      false
    );
  });

  it('edits the time zone and shows it after the reload', async () => {
    const { el, calls } = await mountWithHub(schedule({ id: 's-1', name: 'plain' }));
    const dialog = await openEdit(el, 'plain');
    const zone = dialog.querySelector('sl-input.edit-time-zone') as HTMLInputElement;
    expect(zone.value).toBe('');
    setInput(zone, 'America/New_York');
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    (dialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const patch = calls.find((c) => c.method === 'PATCH');
    expect(patch?.body).toEqual({ timeZone: 'America/New_York' });
    expect(rowFor(el, 'plain').querySelector('.schedule-zone')?.textContent?.trim()).toBe(
      'America/New_York'
    );
  });

  it('does not offer resume on an active schedule', async () => {
    const { el } = await mountWithHub(schedule({ id: 's-1', name: 'plain' }));
    const dialog = await openEdit(el, 'plain');
    expect(dialog.querySelector('input.edit-resume')).toBeNull();
  });
});

/** A fetch stub whose responses the test releases one by one, in any order. */
function deferredFetch(): {
  calls: { method: string; url: string; resolve: (r: Response) => void }[];
} {
  const calls: { method: string; url: string; resolve: (r: Response) => void }[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      (input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((resolve) => {
          calls.push({ method: init?.method ?? 'GET', url: requestUrl(input), resolve });
        })
    )
  );
  return { calls };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

type ListEl = HTMLElement & {
  projectId: string;
  updateComplete: Promise<unknown>;
};

interface ListInternals {
  loading: boolean;
  loadSchedules(): Promise<void>;
  editSchedule: Record<string, unknown> | null;
  editError: string | null;
}

// A slower, older load must not overwrite a newer one (ptone/scion#2643 review).
describe('scion-schedule-list overlapping loads', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function startTwoLoads(): Promise<{
    el: ListEl;
    calls: ReturnType<typeof deferredFetch>['calls'];
    loadB: Promise<void>;
  }> {
    const { calls } = deferredFetch();
    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el); // load A
    await el.updateComplete;
    const loadB = (el as unknown as ListInternals).loadSchedules();
    await new Promise((r) => setTimeout(r, 0));
    expect(calls).toHaveLength(2);
    return { el, calls, loadB };
  }

  it('keeps the newer result when the older load resolves last', async () => {
    const { el, calls, loadB } = await startTwoLoads();
    calls[1]!.resolve(jsonResponse({ schedules: [schedule({ id: 'b', name: 'newer-row' })] }));
    await loadB;
    await settle(el);
    calls[0]!.resolve(jsonResponse({ schedules: [schedule({ id: 'a', name: 'older-row' })] }));
    await settle(el);

    rowFor(el, 'newer-row');
    expect(el.shadowRoot!.textContent).not.toContain('older-row');
    expect((el as unknown as ListInternals).loading).toBe(false);
  });

  it('ignores an older load that fails after the newer one succeeded', async () => {
    const { el, calls, loadB } = await startTwoLoads();
    calls[1]!.resolve(jsonResponse({ schedules: [schedule({ id: 'b', name: 'newer-row' })] }));
    await loadB;
    await settle(el);
    calls[0]!.resolve(jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500));
    await settle(el);

    rowFor(el, 'newer-row');
    expect(el.shadowRoot!.querySelector('.error-details')).toBeNull();
    expect((el as unknown as ListInternals).loading).toBe(false);
  });
});

// The edit dialog stays open while its PATCH is in flight, so a late result
// cannot land on a different dialog (ptone/scion#2643 review).
describe('scion-schedule-list edit dialog while saving', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mountAndSave(): Promise<{
    el: ListEl;
    dialog: HTMLElement;
    calls: ReturnType<typeof deferredFetch>['calls'];
  }> {
    const { calls } = deferredFetch();
    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await el.updateComplete;
    calls[0]!.resolve(
      jsonResponse({
        schedules: [schedule({ id: 's-1', name: 'one' }), schedule({ id: 's-2', name: 'two' })],
      })
    );
    await settle(el);
    (rowFor(el, 'one').querySelector('sl-icon-button[label="Edit"]') as HTMLElement).click();
    await settle(el);
    const dialog = el.shadowRoot!.querySelector('sl-dialog[label^="Edit Schedule"]') as HTMLElement;
    const cron = dialog.querySelector('sl-input.edit-cron') as HTMLInputElement;
    cron.value = '*/5 * * * *';
    cron.dispatchEvent(new Event('sl-input'));
    await settle(el);
    (dialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el);
    expect(calls.at(-1)?.method).toBe('PATCH');
    return { el, dialog, calls };
  }

  function editDialog(el: HTMLElement): HTMLElement | null {
    return el.shadowRoot!.querySelector('sl-dialog[label^="Edit Schedule"]');
  }

  it('refuses to close while the save is in flight, then shows its error', async () => {
    const { el, dialog, calls } = await mountAndSave();

    const req = new Event('sl-request-close', { cancelable: true });
    dialog.dispatchEvent(req);
    expect(req.defaultPrevented).toBe(true);
    const cancel = dialog.querySelector('sl-button.edit-cancel') as HTMLElement;
    expect(cancel.hasAttribute('disabled')).toBe(true);
    cancel.click();
    await settle(el);
    expect(editDialog(el)).toBe(dialog);
    // Opening another row's dialog is refused too.
    (rowFor(el, 'two').querySelector('sl-icon-button[label="Edit"]') as HTMLElement).click();
    await settle(el);
    expect(editDialog(el)?.getAttribute('label')).toBe('Edit Schedule: one');

    calls
      .at(-1)!
      .resolve(
        jsonResponse({ error: { code: 'revision_conflict', message: 'refresh and retry' } }, 409)
      );
    await settle(el);
    expect(editDialog(el)?.querySelector('.dialog-error')?.textContent).toContain(
      'refresh and retry'
    );
    expect(cancel.hasAttribute('disabled')).toBe(false);
    const after = new Event('sl-request-close', { cancelable: true });
    editDialog(el)!.dispatchEvent(after);
    expect(after.defaultPrevented).toBe(false);
    await settle(el);
    expect(editDialog(el)).toBeNull();
  });

  it('does not close a replacement dialog when a late save succeeds', async () => {
    const { el, calls } = await mountAndSave();
    const i = el as unknown as ListInternals & { editLoading: boolean };
    const other = schedule({ id: 's-2', name: 'two' });
    i.editSchedule = other;
    await settle(el);

    calls.at(-1)!.resolve(jsonResponse(schedule({ id: 's-1', name: 'one' })));
    await settle(el);
    expect(i.editSchedule).toBe(other);
    expect(i.editError).toBeNull();
    // The reload starts only once the in-flight flag is clear.
    expect(i.editLoading).toBe(false);
    expect(calls.at(-1)?.method).toBe('GET');
  });

  it("keeps a second save's in-flight flag while the first save's reload finishes", async () => {
    const { el, calls } = await mountAndSave();
    const i = el as unknown as ListInternals & { editLoading: boolean };
    calls.at(-1)!.resolve(jsonResponse(schedule({ id: 's-1', name: 'one' })));
    await settle(el);
    const reload = calls.at(-1)!;
    expect(reload.method).toBe('GET'); // still pending

    // Start a second save while that reload is in flight. The list shows
    // its loading spinner meanwhile, so drive the dialog directly.
    const d = el as unknown as {
      openEditDialog(s: Record<string, unknown>): void;
      editCron: string;
      handleEdit(e: Event): Promise<void>;
    };
    d.openEditDialog(schedule({ id: 's-2', name: 'two' }));
    d.editCron = '0 1 * * *';
    void d.handleEdit(new Event('submit'));
    await settle(el);
    expect(calls.at(-1)?.method).toBe('PATCH');
    expect(i.editLoading).toBe(true);

    reload.resolve(
      jsonResponse({
        schedules: [schedule({ id: 's-1', name: 'one' }), schedule({ id: 's-2', name: 'two' })],
      })
    );
    await settle(el);
    expect(i.editLoading).toBe(true);
    expect(editDialog(el)?.getAttribute('label')).toBe('Edit Schedule: two');
  });

  it('drops a late result if the dialog was replaced anyway', async () => {
    const { el, calls } = await mountAndSave();
    // Force the replacement the UI guards against.
    const i = el as unknown as ListInternals;
    const other = schedule({ id: 's-2', name: 'two' });
    i.editSchedule = other;
    await settle(el);

    calls.at(-1)!.resolve(jsonResponse({ error: { code: 'internal', message: 'old error' } }, 500));
    await settle(el);
    expect(i.editSchedule).toBe(other);
    expect(i.editError).toBeNull();
  });
});

describe('scion-schedule-list status reason', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('labels each known reason and shows an unknown one as stored', () => {
    expect(mod.scheduleStatusReasonLabel(undefined)).toBe('');
    expect(mod.scheduleStatusReasonLabel('')).toBe('');
    expect(mod.scheduleStatusReasonLabel('creator_deleted')).toContain('Creator deleted');
    expect(mod.scheduleStatusReasonLabel('cron_zone_unsupported')).toContain('Zone prefix');
    expect(mod.scheduleStatusReasonLabel('something_new')).toBe('something_new');
  });

  it('shows the reason under the status of a paused row, and none otherwise', async () => {
    const el = await mount([
      schedule({ id: 'a', name: 'orphaned', status: 'paused', statusReason: 'creator_deleted' }),
      schedule({ id: 'b', name: 'user-paused', status: 'paused' }),
      schedule({ id: 'c', name: 'live-sched', status: 'active' }),
    ]);

    const reason = rowFor(el, 'orphaned').querySelector('.status-reason');
    expect(reason).not.toBeNull();
    expect(reason?.getAttribute('data-reason')).toBe('creator_deleted');
    expect(reason?.textContent?.trim()).toBe(mod.scheduleStatusReasonLabel('creator_deleted'));
    // Positive control above uses the same selector that must find nothing here.
    expect(rowFor(el, 'user-paused').querySelector('.status-reason')).toBeNull();
    expect(rowFor(el, 'live-sched').querySelector('.status-reason')).toBeNull();
  });

  it('shows the reason in the detail dialog', async () => {
    const el = await mount([
      schedule({ id: 'a', name: 'orphaned', status: 'paused', statusReason: 'creator_deleted' }),
    ]);
    rowFor(el, 'orphaned').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.querySelector('.status-reason')?.getAttribute('data-reason')).toBe(
      'creator_deleted'
    );
  });
});

describe('scion-schedule-list create with a time zone', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function createWith(timeZone: string): Promise<Record<string, unknown> | undefined> {
    const bodies: Record<string, unknown>[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if ((init?.method ?? 'GET') === 'POST') {
          bodies.push(JSON.parse(requestBodyText(init!.body!)) as Record<string, unknown>);
          return Promise.resolve(new Response('{}', { status: 201 }));
        }
        void input;
        return Promise.resolve(
          new Response(JSON.stringify({ schedules: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
    const el = document.createElement('scion-schedule-list') as HTMLElement & {
      projectId: string;
      updateComplete: Promise<unknown>;
    };
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    const d = el as unknown as {
      dialogName: string;
      dialogCron: string;
      dialogTimeZone: string;
      dialogAgent: string;
      dialogMessage: string;
      handleCreate(e: Event): Promise<void>;
    };
    d.dialogName = 'digest';
    d.dialogCron = '0 7 * * *';
    d.dialogTimeZone = timeZone;
    d.dialogAgent = 'lead';
    d.dialogMessage = 'digest';
    await d.handleCreate(new Event('submit'));
    return bodies[0];
  }

  it('sends the zone, trimmed', async () => {
    const body = await createWith(' America/Los_Angeles ');
    expect(body?.timeZone).toBe('America/Los_Angeles');
    expect(body?.cronExpr).toBe('0 7 * * *');
  });

  it('omits the zone when it is empty or UTC', async () => {
    expect(await createWith('')).not.toHaveProperty('timeZone');
    expect(await createWith('UTC')).not.toHaveProperty('timeZone');
  });

  it('labels a missing zone as UTC', () => {
    expect(mod.scheduleZoneLabel(undefined)).toBe('UTC');
    expect(mod.scheduleZoneLabel('')).toBe('UTC');
    expect(mod.scheduleZoneLabel('Asia/Tokyo')).toBe('Asia/Tokyo');
  });
});

describe('scion-schedule-list detail dialog wake and interrupt (ptone/scion#7791)', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function detailRowText(dialog: Element | null, label: string): string | undefined {
    if (!dialog) return undefined;
    const row = Array.from(dialog.querySelectorAll('.detail-row')).find(
      (r) => r.querySelector('strong')?.textContent?.trim() === label
    );
    return row?.textContent?.replace(label, '').trim();
  }

  it('shows Wake: Yes and Interrupt: Yes when both are true in payload', async () => {
    const el = await mount([
      schedule({
        id: 's-wake',
        name: 'wake-check',
        eventType: 'message',
        payload: '{"agentName":"watcher","message":"check builds","wake":true,"interrupt":true}',
      }),
    ]);
    rowFor(el, 'wake-check').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog).not.toBeNull();
    expect(detailRowText(dialog, 'Wake:')).toBe('Yes');
    expect(detailRowText(dialog, 'Interrupt:')).toBe('Yes');
  });

  it('shows Wake: No and Interrupt: No when omitted from message payload', async () => {
    const el = await mount([
      schedule({
        id: 's-default',
        name: 'default-msg',
        eventType: 'message',
        payload: '{"agentName":"watcher","message":"plain check"}',
      }),
    ]);
    rowFor(el, 'default-msg').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog).not.toBeNull();
    expect(detailRowText(dialog, 'Wake:')).toBe('No');
    expect(detailRowText(dialog, 'Interrupt:')).toBe('No');
  });

  it('omits Wake and Interrupt rows for dispatch_agent schedules', async () => {
    const el = await mount([
      schedule({
        id: 's-dispatch',
        name: 'spawn-worker',
        eventType: 'dispatch_agent',
        payload: '{"agentName":"worker","template":"default","task":"build"}',
      }),
    ]);
    rowFor(el, 'spawn-worker').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog).not.toBeNull();
    expect(detailRowText(dialog, 'Wake:')).toBeUndefined();
    expect(detailRowText(dialog, 'Interrupt:')).toBeUndefined();
  });
});

describe('scion-schedule-list Wake column (ptone/scion#9082)', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('parseScheduleWake returns boolean for message schedules and null for other types', () => {
    expect(
      mod.parseScheduleWake({ eventType: 'message', payload: '{"agentName":"w","wake":true}' })
    ).toBe(true);
    expect(
      mod.parseScheduleWake({ eventType: 'message', payload: '{"agentName":"w","wake":false}' })
    ).toBe(false);
    expect(mod.parseScheduleWake({ eventType: 'message', payload: '{"agentName":"w"}' })).toBe(
      false
    );
    expect(mod.parseScheduleWake({ eventType: 'message', payload: 'not-json' })).toBe(false);
    expect(
      mod.parseScheduleWake({ eventType: 'dispatch_agent', payload: '{"wake":true}' })
    ).toBeNull();
    expect(
      mod.parseScheduleWake({ eventType: 'exec', payload: '{"command":["true"]}' })
    ).toBeNull();
  });

  it('renders Wake column with Yes/No for message schedules and em-dash for dispatch_agent/exec', async () => {
    const el = await mount([
      schedule({
        id: 's-msg-wake',
        name: 'msg-wake',
        eventType: 'message',
        payload: '{"agentName":"w","message":"hi","wake":true}',
      }),
      schedule({
        id: 's-msg-nowake',
        name: 'msg-nowake',
        eventType: 'message',
        payload: '{"agentName":"w","message":"hi"}',
      }),
      schedule({
        id: 's-dispatch',
        name: 'dispatch-sched',
        eventType: 'dispatch_agent',
        payload: '{"agentName":"w","template":"default","task":"build"}',
      }),
      schedule({
        id: 's-exec',
        name: 'exec-sched',
        eventType: 'exec',
        payload: '{"agentId":"a1","command":["true"]}',
      }),
    ]);

    const headers = Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('thead th')).map(
      (th) => th.textContent?.trim()
    );
    const wakeIdx = headers.indexOf('Wake');
    expect(wakeIdx).toBeGreaterThan(-1);

    const wakeCellText = (name: string): string | undefined =>
      rowFor(el, name).querySelectorAll('td')[wakeIdx]?.textContent?.trim();

    expect(wakeCellText('msg-wake')).toBe('Yes');
    expect(wakeCellText('msg-nowake')).toBe('No');
    expect(wakeCellText('dispatch-sched')).toBe('—');
    expect(wakeCellText('exec-sched')).toBe('—');
  });
});

describe('scion-schedule-list exec authoring, detail history, and PATCH (ptone/scion#7886)', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(async () => {
    document.body.innerHTML = '';
    const ff = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    ff.resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('labels target_deleted statusReason on paused exec schedules', () => {
    expect(mod.scheduleStatusReasonLabel('target_deleted')).toMatch(/target agent deleted/i);
  });

  it('gates the exec option in the create modal on hub.exec_schedules', async () => {
    const { setFeatureFlag } = await import('../../utils/feature-flags.js');
    const el = await mount([]);
    const root = el.shadowRoot as ShadowRoot;

    (root.querySelector('sl-button') as HTMLElement).click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });

    const createDialog = () => root.querySelector('sl-dialog[label="Create Recurring Schedule"]')!;
    const optionValues = () =>
      Array.from(createDialog().querySelectorAll('sl-select[label="Event Type"] sl-option')).map(
        (o) => o.getAttribute('value')
      );

    // Flag OFF: identical to today (message + dispatch_agent only)
    expect(optionValues()).toEqual(['message', 'dispatch_agent']);

    // Flag ON: exec option appears
    setFeatureFlag('hub.exec_schedules', true);
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    expect(optionValues()).toEqual(['message', 'dispatch_agent', 'exec']);
  });

  it('authors a recurring exec schedule via agent picker and argv editor, validates bounds, and surfaces hub 400s', async () => {
    const { setFeatureFlag } = await import('../../utils/feature-flags.js');
    setFeatureFlag('hub.exec_schedules', true);

    const posts: Record<string, unknown>[] = [];
    let postFail: { status: number; message: string } | null = null;

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? 'GET';
        const u = new URL(requestUrl(input), 'http://x');
        if (method === 'POST' && u.pathname === '/api/v1/projects/proj-1/schedules') {
          posts.push(JSON.parse(requestBodyText(init!.body!)) as Record<string, unknown>);
          if (postFail) {
            return Promise.resolve(
              jsonResponse(
                { error: { code: 'invalid_input', message: postFail.message } },
                postFail.status
              )
            );
          }
          return Promise.resolve(jsonResponse({ id: 'sched-exec-1' }, 201));
        }
        if (method === 'GET' && u.pathname === '/api/v1/projects/proj-1/agents') {
          return Promise.resolve(
            jsonResponse({
              agents: [
                {
                  id: 'a-live',
                  name: 'runner',
                  phase: 'running',
                  _capabilities: { actions: ['read', 'attach'] },
                },
                {
                  id: 'a-managed',
                  name: 'managed-bot',
                  phase: 'running',
                  runtime: 'managed:cloud',
                  _capabilities: { actions: ['read', 'attach'] },
                },
              ],
            })
          );
        }
        return Promise.resolve(jsonResponse({ schedules: [] }));
      })
    );

    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);

    const root = el.shadowRoot as ShadowRoot;
    (root.querySelector('sl-button') as HTMLElement).click();
    await settle(el);

    const dialog = root.querySelector('sl-dialog[label="Create Recurring Schedule"]')!;
    const setField = (sel: string, value: string) => {
      const input = dialog.querySelector(sel) as HTMLInputElement;
      input.value = value;
      input.dispatchEvent(new Event('sl-input'));
    };

    setField('sl-input[label="Name"]', 'nightly-audit');
    setField('sl-input[label="Cron Expression"]', '0 2 * * *');

    const typeSelect = dialog.querySelector('sl-select[label="Event Type"]') as HTMLElement & {
      value: string;
    };
    typeSelect.value = 'exec';
    typeSelect.dispatchEvent(new Event('sl-change'));
    await settle(el);

    // Non-exec fields are hidden
    expect(dialog.querySelector('sl-textarea[label="Message"]')).toBeNull();
    expect(dialog.querySelector('sl-input[label="Template"]')).toBeNull();

    const agentSelect = dialog.querySelector('sl-select.exec-agent-select') as HTMLElement & {
      value: string;
    };
    expect(agentSelect).not.toBeNull();
    expect(
      agentSelect.querySelector('sl-option[value="a-managed"]')?.hasAttribute('disabled')
    ).toBe(true);

    agentSelect.value = 'a-live';
    agentSelect.dispatchEvent(new Event('sl-change'));
    await settle(el);

    // Empty argv[0] is caught client-side before POST
    const createBtn = Array.from(dialog.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Create'
    ) as HTMLElement;
    createBtn.click();
    await settle(el);
    expect(posts).toHaveLength(0);
    expect(dialog.querySelector('.dialog-error')?.textContent).toMatch(/argv\[0\]/i);

    // Fill argv: ['/usr/bin/make', 'audit']
    (dialog.querySelector('sl-button.exec-argv-add') as HTMLElement).click();
    await settle(el);
    setField('sl-input.exec-argv-input[data-arg-index="0"]', '/usr/bin/make');
    setField('sl-input.exec-argv-input[data-arg-index="1"]', 'audit');
    setField('sl-input.exec-timeout-input', '300');
    await settle(el);

    // Hub 400 surfaces cleanly
    postFail = { status: 400, message: 'invalid cron field 2' };
    createBtn.click();
    await settle(el);
    expect(posts).toHaveLength(1);
    expect(dialog.querySelector('.dialog-error')?.textContent).toContain('invalid cron field 2');

    postFail = null;
    createBtn.click();
    await settle(el);
    expect(posts).toHaveLength(2);
    expect(posts[1]).toEqual({
      name: 'nightly-audit',
      cronExpr: '0 2 * * *',
      eventType: 'exec',
      agentId: 'a-live',
      command: ['/usr/bin/make', 'audit'],
      timeoutSeconds: 300,
    });
  });

  it('renders revealed vs redacted exec schedule detail, fetches /history, shows no-output note, and has no output view', async () => {
    const sentinel = 'TOP-SECRET-ARGV-TOKEN';
    const historyUrls: string[] = [];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const u = new URL(requestUrl(input), 'http://x');
        if (u.pathname.endsWith('/history')) {
          historyUrls.push(u.pathname + u.search);
          return Promise.resolve(
            jsonResponse({
              events: [
                {
                  id: 'run-1',
                  projectId: 'proj-1',
                  eventType: 'exec',
                  fireAt: '2026-10-01T02:00:00Z',
                  firedAt: '2026-10-01T02:00:01Z',
                  status: 'fired',
                  execSummary: {
                    outcome: 'ok',
                    exitCode: 0,
                    durationMs: 420,
                    outputBytes: 128,
                    startedAt: '2026-10-01T02:00:01Z',
                  },
                },
                {
                  id: 'run-2',
                  projectId: 'proj-1',
                  eventType: 'exec',
                  fireAt: '2026-09-30T02:00:00Z',
                  status: 'skipped',
                  error: 'agent not running (phase=stopped)',
                },
              ],
            })
          );
        }
        return Promise.resolve(
          jsonResponse({
            schedules: [
              schedule({
                id: 'sched-rev',
                name: 'revealed-exec',
                eventType: 'exec',
                payload: JSON.stringify({
                  agentId: 'a-1',
                  agentName: 'runner',
                  command: ['/bin/check', '--strict'],
                  timeoutSeconds: 120,
                }),
              }),
              schedule({
                id: 'sched-red',
                name: 'redacted-exec',
                eventType: 'exec',
                status: 'paused',
                statusReason: 'target_deleted',
                payload: JSON.stringify({
                  agentId: 'a-gone',
                  agentName: 'gone-runner',
                  timeoutSeconds: 60,
                  commandRedacted: true,
                  argc: 2,
                  commandSha256: '998877665544',
                  command: ['echo', sentinel],
                }),
              }),
            ],
          })
        );
      })
    );

    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);

    // Click revealed schedule row -> fetches /history and shows command + history + note
    rowFor(el, 'revealed-exec').click();
    await settle(el);

    const root = el.shadowRoot as ShadowRoot;
    let dialog = root.querySelector('sl-dialog[label="Schedule: revealed-exec"]')!;
    let text = dialog.textContent ?? '';
    expect(historyUrls).toEqual(['/api/v1/projects/proj-1/schedules/sched-rev/history?limit=10']);
    expect(text).toContain('/bin/check');
    expect(text).toContain('--strict');
    expect(text).toContain('120s');
    expect(text).toContain('ok');
    expect(text).toContain('420ms');
    expect(text).toContain('skipped');
    expect(text).toContain('agent not running (phase=stopped)');
    expect(text).toContain("Command output isn't stored; check the agent's own logs.");
    expect(dialog.querySelector('pre, textarea, scion-log-viewer')).toBeNull();

    // Click redacted schedule row -> shows redaction notice, never leaks sentinel
    rowFor(el, 'redacted-exec').click();
    await settle(el);

    dialog = root.querySelector('sl-dialog[label="Schedule: redacted-exec"]')!;
    text = dialog.textContent ?? '';
    expect(dialog.querySelector('.exec-redacted-notice')).not.toBeNull();
    expect(text).toContain('2 args');
    expect(text).toContain('sha256: 998877665544');
    expect(text).not.toContain(sentinel);
    expect(dialog.querySelector('.status-reason')?.getAttribute('data-reason')).toBe(
      'target_deleted'
    );
  });

  it('respects PATCH restrictions: never switches eventType, edits revealed exec fields, allows metadata rename when redacted, surfaces 403, and allows pause/resume/delete', async () => {
    const { setFeatureFlag } = await import('../../utils/feature-flags.js');
    setFeatureFlag('hub.exec_schedules', true);
    const calls: { method: string; url: string; body?: unknown }[] = [];
    let nextPatchStatus = 200;
    let nextPatchError = '';
    let nextResumeStatus = 200;
    let nextResumeError = '';

    const schedules = [
      schedule({
        id: 's-rev',
        name: 'exec-owned',
        eventType: 'exec',
        payload: JSON.stringify({
          agentId: 'a-1',
          agentName: 'runner-1',
          command: ['echo', 'v1'],
          timeoutSeconds: 90,
        }),
      }),
      schedule({
        id: 's-red',
        name: 'exec-foreign',
        eventType: 'exec',
        status: 'paused',
        payload: JSON.stringify({
          agentId: 'a-2',
          agentName: 'runner-2',
          timeoutSeconds: 60,
          commandRedacted: true,
          argc: 2,
          commandSha256: 'aa11bb22',
        }),
      }),
      schedule({
        id: 's-orphan',
        name: 'exec-orphan',
        eventType: 'exec',
        status: 'paused',
        statusReason: 'target_deleted',
        payload: JSON.stringify({
          agentId: 'a-deleted',
          agentName: 'gone-runner',
          timeoutSeconds: 60,
          commandRedacted: true,
          argc: 2,
          commandSha256: 'cc33dd44',
        }),
      }),
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? 'GET';
        const url = requestUrl(input);
        const u = new URL(url, 'http://x');
        const body = init?.body ? (JSON.parse(requestBodyText(init.body)) as unknown) : undefined;
        calls.push({ method, url, body });

        if (method === 'GET' && u.pathname === '/api/v1/projects/proj-1/agents') {
          return Promise.resolve(
            jsonResponse({
              agents: [
                {
                  id: 'a-1',
                  name: 'runner-1',
                  phase: 'running',
                  _capabilities: { actions: ['read', 'attach'] },
                },
                {
                  id: 'a-3',
                  name: 'runner-3',
                  phase: 'running',
                  _capabilities: { actions: ['read', 'attach'] },
                },
              ],
            })
          );
        }
        if (method === 'PATCH') {
          if (nextPatchStatus !== 200) {
            return Promise.resolve(
              jsonResponse(
                { error: { code: 'forbidden', message: nextPatchError } },
                nextPatchStatus
              )
            );
          }
          return Promise.resolve(jsonResponse({ ...schedules[0], ...(body as object) }));
        }
        if (method === 'POST' && u.pathname.endsWith('/resume')) {
          if (nextResumeStatus !== 200) {
            return Promise.resolve(
              jsonResponse(
                { error: { code: 'forbidden', message: nextResumeError } },
                nextResumeStatus
              )
            );
          }
          return Promise.resolve(jsonResponse({ status: 'active' }));
        }
        if (method === 'POST' && u.pathname.endsWith('/pause')) {
          return Promise.resolve(jsonResponse({ status: 'paused' }));
        }
        if (method === 'DELETE') {
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return Promise.resolve(jsonResponse({ schedules }));
      })
    );

    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    const root = el.shadowRoot as ShadowRoot;

    // 1. Edit revealed exec schedule: no eventType selector; change argv[1] and clear timeout to reset to 0
    (rowFor(el, 'exec-owned').querySelector('sl-icon-button[label="Edit"]') as HTMLElement).click();
    await settle(el);

    let editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
    expect(editDialog.querySelector('sl-select[label="Event Type"]')).toBeNull();

    const arg1 = editDialog.querySelector(
      'sl-input.exec-argv-input[data-arg-index="1"]'
    ) as HTMLInputElement;
    expect(arg1.value).toBe('v1');
    arg1.value = 'v2';
    arg1.dispatchEvent(new Event('sl-input'));

    const timeout = editDialog.querySelector('sl-input.exec-timeout-input') as HTMLInputElement;
    expect(timeout.value).toBe('90');
    timeout.value = '';
    timeout.dispatchEvent(new Event('sl-input'));
    await settle(el);

    (editDialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el);

    const revPatch = calls.find((c) => c.method === 'PATCH' && c.url.endsWith('/schedules/s-rev'));
    expect(revPatch?.body).toEqual({
      command: ['echo', 'v2'],
      timeoutSeconds: 0,
    });
    expect(revPatch?.body).not.toHaveProperty('eventType');

    // 2. Edit redacted live-target exec schedule: rename-only (no Replace checkbox), succeeds without sending command/agentId
    (
      rowFor(el, 'exec-foreign').querySelector('sl-icon-button[label="Edit"]') as HTMLElement
    ).click();
    await settle(el);

    editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
    expect(editDialog.querySelector('sl-select[label="Event Type"]')).toBeNull();
    expect(editDialog.querySelector('input.edit-replace-exec')).toBeNull();
    expect(editDialog.querySelector('sl-select.exec-agent-select')).toBeNull();
    expect(editDialog.querySelector('sl-input.exec-argv-input')).toBeNull();
    const nameInput = editDialog.querySelector('sl-input[label="Name"]') as HTMLInputElement;
    nameInput.value = 'exec-foreign-renamed';
    nameInput.dispatchEvent(new Event('sl-input'));
    await settle(el);

    (editDialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el);

    const renamePatch = calls.find(
      (c) => c.method === 'PATCH' && c.url.endsWith('/schedules/s-red')
    );
    expect(renamePatch?.body).toEqual({ name: 'exec-foreign-renamed' });

    // 3. Dispatch-changing edit on redacted exec schedule surfaces hub 403 cleanly
    calls.length = 0;
    nextPatchStatus = 403;
    nextPatchError = 'attach permission denied on target agent';
    (
      rowFor(el, 'exec-foreign').querySelector('sl-icon-button[label="Edit"]') as HTMLElement
    ).click();
    await settle(el);

    editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
    const cronInput = editDialog.querySelector('sl-input.edit-cron') as HTMLInputElement;
    cronInput.value = '*/10 * * * *';
    cronInput.dispatchEvent(new Event('sl-input'));
    await settle(el);

    (editDialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el);

    expect(editDialog.querySelector('.dialog-error')?.textContent).toContain(
      'attach permission denied on target agent'
    );
    (editDialog.querySelector('sl-button.edit-cancel') as HTMLElement).click();
    await settle(el);

    // 4. Pause, Resume (surfacing 403 cleanly), and Delete buttons remain enabled without client attach gating
    const pauseBtn = rowFor(el, 'exec-owned').querySelector(
      'sl-icon-button[label="Pause"]'
    ) as HTMLElement;
    expect(pauseBtn.hasAttribute('disabled')).toBe(false);
    pauseBtn.click();
    await settle(el);
    expect(calls.some((c) => c.method === 'POST' && c.url.endsWith('/s-rev/pause'))).toBe(true);

    nextResumeStatus = 403;
    nextResumeError = 'cannot resume without agent.attach';
    const resumeBtn = rowFor(el, 'exec-foreign').querySelector(
      'sl-icon-button[label="Resume"]'
    ) as HTMLElement;
    expect(resumeBtn.hasAttribute('disabled')).toBe(false);
    resumeBtn.click();
    await settle(el);
    expect(root.querySelector('.error-details')?.textContent).toContain(
      'cannot resume without agent.attach'
    );

    // Clear the error state via Retry before testing retargeting
    (root.querySelector('.error-state sl-button') as HTMLElement).click();
    await settle(el);

    // 5. Retargeting a target_deleted redacted exec schedule via edit-replace-exec + resume sends replacement agentId, command, and status
    calls.length = 0;
    nextPatchStatus = 200;
    (
      rowFor(el, 'exec-orphan').querySelector('sl-icon-button[label="Edit"]') as HTMLElement
    ).click();
    await settle(el);

    editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
    const replaceBox = editDialog.querySelector('input.edit-replace-exec') as HTMLInputElement;
    expect(replaceBox).not.toBeNull();
    replaceBox.checked = true;
    replaceBox.dispatchEvent(new Event('change'));
    await settle(el);

    const agentSelect = editDialog.querySelector('sl-select.exec-agent-select') as HTMLElement & {
      value: string;
    };
    agentSelect.value = 'a-3';
    agentSelect.dispatchEvent(new Event('sl-change'));

    const repArg0 = editDialog.querySelector(
      'sl-input.exec-argv-input[data-arg-index="0"]'
    ) as HTMLInputElement;
    repArg0.value = '/usr/bin/true';
    repArg0.dispatchEvent(new Event('sl-input'));

    const resumeBox = editDialog.querySelector('input.edit-resume') as HTMLInputElement;
    resumeBox.checked = true;
    resumeBox.dispatchEvent(new Event('change'));
    await settle(el);

    (editDialog.querySelector('sl-button.edit-save') as HTMLElement).click();
    await settle(el);

    const retargetPatch = calls.find(
      (c) => c.method === 'PATCH' && c.url.endsWith('/schedules/s-orphan')
    );
    expect(retargetPatch?.body).toEqual({
      agentId: 'a-3',
      command: ['/usr/bin/true'],
      status: 'active',
    });
  });

  it('offers rename only (no Replace target and command) on a redacted live-target exec row (O1)', async () => {
    const ff = await import('../../utils/feature-flags.js');
    ff.setServerFlags({ 'hub.exec_schedules': true });

    const calls: { method: string; url: string; body?: unknown }[] = [];
    const liveRedacted = schedule({
      id: 's-live-red',
      name: 'live-foreign-exec',
      eventType: 'exec',
      status: 'active',
      payload: JSON.stringify({
        agentId: 'a-live',
        agentName: 'live-runner',
        timeoutSeconds: 60,
        commandRedacted: true,
        argc: 3,
        commandSha256: '11223344',
      }),
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? 'GET';
        const url = requestUrl(input);
        const body = init?.body ? (JSON.parse(requestBodyText(init.body)) as unknown) : undefined;
        calls.push({ method, url, body });
        if (method === 'PATCH') {
          return Promise.resolve(jsonResponse({ ...liveRedacted, ...(body as object) }));
        }
        return Promise.resolve(jsonResponse({ schedules: [liveRedacted] }));
      })
    );

    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    const root = el.shadowRoot as ShadowRoot;

    (
      rowFor(el, 'live-foreign-exec').querySelector('sl-icon-button[label="Edit"]') as HTMLElement
    ).click();
    await settle(el);

    const editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
    expect(editDialog.textContent).toContain('Renaming is allowed');
    expect(editDialog.querySelector('sl-input[label="Name"]')).not.toBeNull();
    expect(editDialog.querySelector('input.edit-replace-exec')).toBeNull();
    expect(editDialog.querySelector('sl-select.exec-agent-select')).toBeNull();
    expect(editDialog.querySelector('sl-input.exec-argv-input')).toBeNull();
    expect(editDialog.querySelector('sl-input.exec-timeout-input')).toBeNull();
    expect(calls.some((c) => c.url.includes('/agents'))).toBe(false);
  });

  it('disables exec editing (including rename) while hub.exec_schedules is off, while keeping pause and delete working (O3)', async () => {
    const ff = await import('../../utils/feature-flags.js');
    ff.setServerFlags({ 'hub.exec_schedules': false });

    const calls: { method: string; url: string; body?: unknown }[] = [];
    const schedules = [
      schedule({
        id: 's-exec-off',
        name: 'exec-while-off',
        eventType: 'exec',
        status: 'active',
        payload: JSON.stringify({
          agentId: 'a-1',
          agentName: 'runner-1',
          command: ['echo', 'hi'],
          timeoutSeconds: 60,
        }),
      }),
      schedule({
        id: 's-exec-red-off',
        name: 'exec-red-while-off',
        eventType: 'exec',
        status: 'paused',
        statusReason: 'target_deleted',
        payload: JSON.stringify({
          agentId: 'a-dead',
          agentName: 'dead-runner',
          timeoutSeconds: 60,
          commandRedacted: true,
          argc: 1,
          commandSha256: 'deadbeef',
        }),
      }),
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? 'GET';
        const url = requestUrl(input);
        const u = new URL(url, 'http://x');
        const body = init?.body ? (JSON.parse(requestBodyText(init.body)) as unknown) : undefined;
        calls.push({ method, url, body });
        if (method === 'POST' && u.pathname.endsWith('/pause')) {
          return Promise.resolve(jsonResponse({ status: 'paused' }));
        }
        if (method === 'DELETE') {
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return Promise.resolve(jsonResponse({ schedules }));
      })
    );

    const el = document.createElement('scion-schedule-list') as ListEl;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await settle(el);
    const root = el.shadowRoot as ShadowRoot;

    // Open Edit on both revealed and redacted exec rows with flag off
    for (const rowName of ['exec-while-off', 'exec-red-while-off']) {
      (rowFor(el, rowName).querySelector('sl-icon-button[label="Edit"]') as HTMLElement).click();
      await settle(el);

      const editDialog = root.querySelector('sl-dialog[label^="Edit Schedule"]')!;
      expect(editDialog.querySelector('.exec-edit-disabled-note')?.textContent).toContain(
        'Editing is unavailable while exec schedules are disabled.'
      );
      expect(editDialog.querySelector('sl-input[label="Name"]')).toBeNull();
      expect(editDialog.querySelector('sl-input.edit-cron')).toBeNull();
      expect(editDialog.querySelector('sl-input.edit-time-zone')).toBeNull();
      expect(editDialog.querySelector('sl-select.exec-agent-select')).toBeNull();
      expect(editDialog.querySelector('sl-input.exec-argv-input')).toBeNull();
      expect(editDialog.querySelector('sl-input.exec-timeout-input')).toBeNull();
      expect(editDialog.querySelector('input.edit-replace-exec')).toBeNull();
      expect(editDialog.querySelector('input.edit-resume')).toBeNull();

      const saveBtn = editDialog.querySelector('sl-button.edit-save') as HTMLElement;
      expect(saveBtn.hasAttribute('disabled')).toBe(true);
      editDialog
        .querySelector('form.edit-form')!
        .dispatchEvent(new Event('submit', { cancelable: true }));
      await settle(el);
      expect(calls.some((c) => c.method === 'PATCH')).toBe(false);
      expect(calls.some((c) => c.url.includes('/agents'))).toBe(false);

      (editDialog.querySelector('sl-button.edit-cancel') as HTMLElement).click();
      await settle(el);
    }

    // Pause and Delete keep working on exec rows while flag is off
    (
      rowFor(el, 'exec-while-off').querySelector('sl-icon-button[label="Pause"]') as HTMLElement
    ).click();
    await settle(el);
    expect(calls.some((c) => c.method === 'POST' && c.url.endsWith('/s-exec-off/pause'))).toBe(
      true
    );

    (
      rowFor(el, 'exec-red-while-off').querySelector(
        'sl-icon-button[label="Delete"]'
      ) as HTMLElement
    ).click();
    await settle(el);
    expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/s-exec-red-off'))).toBe(
      true
    );
  });
});

describe('scion-schedule-list initialScheduleId deep-link', () => {
  beforeAll(async () => {
    mod = await import('./schedule-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('auto-opens the detail dialog when initialScheduleId matches a loaded schedule', async () => {
    const schedules = [
      schedule({ id: 'sched-a', name: 'alpha-sched' }),
      schedule({ id: 'sched-b', name: 'beta-sched' }),
    ];
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ schedules, totalCount: schedules.length }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );

    const el = document.createElement('scion-schedule-list') as HTMLElement & {
      projectId: string;
      initialScheduleId: string;
      updateComplete: Promise<unknown>;
    };
    el.projectId = 'proj-1';
    el.initialScheduleId = 'sched-b';
    document.body.appendChild(el);
    await settle(el);

    const dialog = el.shadowRoot?.querySelector('sl-dialog[label="Schedule: beta-sched"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.hasAttribute('open')).toBe(true);
  });
});
