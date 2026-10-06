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
 * Tests for the UTC-only cron presentation in the recurring schedule list:
 * a schedule whose stored expression carries a CRON_TZ=/TZ= prefix shows a
 * "zone prefix not supported" badge, and the cron field keeps its "(UTC)"
 * help text. Also covers next-run display: the next run is shown in the
 * display zone with a zone label, beside the UTC cron expression.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

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

  it('keeps the "(UTC)" help text on the cron field', async () => {
    const el = await mount([]);
    const cronInput = Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('sl-input')).find(
      (i) => (i.getAttribute('help-text') ?? '').includes('cron')
    );
    expect(cronInput?.getAttribute('help-text')).toContain('(UTC)');
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
    // The cron column stays labelled UTC, next to the zoned next run.
    const headers = Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('thead th')).map(
      (th) => th.textContent?.trim()
    );
    expect(headers).toContain('Cron (UTC)');
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

  it('shows the zoned next run in the detail dialog beside the UTC cron', async () => {
    useFakeNow();
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mount([schedule({ name: 'nightly', nextRunAt: NEXT_RUN })]);
    rowFor(el, 'nightly').click();
    await settle(el as unknown as { updateComplete: Promise<unknown> });
    const dialog = (el.shadowRoot as ShadowRoot).querySelector('sl-dialog[label^="Schedule:"]');
    expect(dialog?.textContent).toContain('Cron (UTC):');
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
        const url = String(input);
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
        const url = String(input);
        const body = init?.body ? (JSON.parse(String(init.body)) as unknown) : undefined;
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
          calls.push({ method: init?.method ?? 'GET', url: String(input), resolve });
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
