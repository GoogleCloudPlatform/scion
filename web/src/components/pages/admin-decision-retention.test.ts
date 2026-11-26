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
 * The Decision retention panel of Admin > Server Config (hub.decision_retention):
 * shown only with the web flag on, revision-CAS saves, client and hub validation
 * errors, per-project overrides, preview and manual run maintenance ops, last run
 * summary, and confirmation before a save that deletes more.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { DECISION_RETENTION_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());

vi.mock('../../client/api.js', async () => {
  const actual = await vi.importActual<typeof import('../../client/api.js')>('../../client/api.js');
  return { apiErrorMessageFromBody: actual.apiErrorMessageFromBody, apiFetch };
});

const mod = await import('./admin-decision-retention.js');
const {
  buildDecisionRetentionPutBody,
  decisionRetentionSaveErrors,
  decisionRetentionWarning,
  lastDecisionPreviewSummary,
  lastDecisionRunSummary,
  needsDecisionPurgeConfirmation,
  validateDecisionRetentionOverride,
} = mod;
type Panel = import('./admin-decision-retention.js').ScionAdminDecisionRetention;
type Settings = import('./admin-decision-retention.js').DecisionRetentionSettings;
type Run = import('./admin-decision-retention.js').DecisionRetentionRun;

const PROJ_PAYMENTS = '11111111-1111-1111-1111-111111111111';
const PROJ_SCRATCH = '22222222-2222-2222-2222-222222222222';
const PROJ_WEB = '33333333-3333-3333-3333-333333333333';

const PROJECTS = [
  { id: PROJ_PAYMENTS, name: 'payments-api', slug: 'payments-api' },
  { id: PROJ_SCRATCH, name: 'scratch-experiments', slug: 'scratch-experiments' },
  { id: PROJ_WEB, name: 'web-console', slug: 'web-console' },
];

function run(over: Partial<Run> = {}): Run {
  return {
    id: 'r1',
    trigger: 'sweep',
    dry_run: false,
    started_at: '2026-11-25T14:00:00Z',
    finished_at: '2026-11-25T14:00:12Z',
    status: 'ok',
    projects: 6,
    totals: { decisions: 1284, answers: 1190 },
    retention_days: 180,
    ...over,
  };
}

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function settings(over: Partial<Settings> = {}): Settings {
  return {
    enabled: false,
    retention_days: null,
    project_overrides: {},
    sweep: { page_size: 200, tick_budget_seconds: 30, tick_interval_minutes: 60 },
    revision: 3,
    runs: [],
    ...over,
  };
}

const SWEEP = {
  page_size: '200',
  tick_budget_seconds: '30',
  tick_interval_minutes: '60',
};

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function mount(): Promise<Panel> {
  const el = document.createElement('scion-admin-decision-retention');
  document.body.appendChild(el);
  await el.updateComplete;
  await settle();
  await el.updateComplete;
  return el;
}

function q<T extends Element = HTMLElement>(el: Panel, sel: string): T | null {
  return el.shadowRoot?.querySelector<T>(sel) ?? null;
}

function puts(): Array<Record<string, unknown>> {
  return apiFetch.mock.calls
    .filter(([, init]) => init?.method === 'PUT')
    .map(([, init]) => JSON.parse(init?.body as string) as Record<string, unknown>);
}

async function type(el: Panel, sel: string, value: string): Promise<void> {
  const input = q<HTMLInputElement>(el, sel);
  if (!input) throw new Error(`no ${sel}`);
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await el.updateComplete;
}

async function select(el: Panel, sel: string, value: string): Promise<void> {
  const control = q<HTMLInputElement>(el, sel);
  if (!control) throw new Error(`no ${sel}`);
  control.value = value;
  control.dispatchEvent(new Event('sl-change'));
  await el.updateComplete;
}

async function save(el: Panel): Promise<void> {
  q(el, '.retention-save')?.click();
  await settle();
  await el.updateComplete;
}

describe('validateDecisionRetentionOverride', () => {
  it('accepts keep_forever regardless of days input', () => {
    expect(validateDecisionRetentionOverride('keep_forever', '')).toEqual({
      ok: true,
      override: { mode: 'keep_forever' },
    });
  });

  it('accepts days within 30..36500 and rejects out-of-range values', () => {
    expect(validateDecisionRetentionOverride('days', '30')).toEqual({
      ok: true,
      override: { mode: 'days', days: 30 },
    });
    expect(validateDecisionRetentionOverride('days', '29').ok).toBe(false);
    expect(validateDecisionRetentionOverride('days', '36501').ok).toBe(false);
    expect(validateDecisionRetentionOverride('days', '').ok).toBe(false);
  });
});

describe('buildDecisionRetentionPutBody', () => {
  it('sends an empty window as null and includes project_overrides with the loaded revision', () => {
    expect(
      buildDecisionRetentionPutBody(
        {
          enabled: true,
          days: '',
          overrides: {
            [PROJ_PAYMENTS]: { mode: 'keep_forever' },
            [PROJ_SCRATCH]: { mode: 'days', days: 30 },
          },
          sweep: SWEEP,
        },
        4
      )
    ).toEqual({
      ok: true,
      body: {
        enabled: true,
        retention_days: null,
        project_overrides: {
          [PROJ_PAYMENTS]: { mode: 'keep_forever' },
          [PROJ_SCRATCH]: { mode: 'days', days: 30 },
        },
        sweep: {
          page_size: 200,
          tick_budget_seconds: 30,
          tick_interval_minutes: 60,
        },
        expected_revision: 4,
      },
    });
  });

  it('accepts windows at the 30 and 36500 day bounds', () => {
    const min = buildDecisionRetentionPutBody({ enabled: true, days: ' 30 ', sweep: SWEEP }, 1);
    expect(min.ok && min.body.retention_days).toBe(30);
    const max = buildDecisionRetentionPutBody({ enabled: true, days: '36500', sweep: SWEEP }, 1);
    expect(max.ok && max.body.retention_days).toBe(36500);
  });

  it.each(['29', '7', '0', '-30', '30.5', 'abc', '36501'])('refuses a window of %j', (days) => {
    const r = buildDecisionRetentionPutBody({ enabled: true, days, sweep: SWEEP }, 1);
    expect(r.ok).toBe(false);
    expect(!r.ok && r.errors[0]).toContain('from 30 to 36500');
  });

  it('refuses sweep tuning outside its bounds', () => {
    const r = buildDecisionRetentionPutBody(
      {
        enabled: false,
        days: '',
        sweep: { page_size: '49', tick_budget_seconds: '41', tick_interval_minutes: '' },
      },
      1
    );
    expect(r.ok).toBe(false);
    expect(!r.ok && r.errors).toEqual([
      'Page size must be a whole number from 50 to 1000.',
      'Tick budget (seconds) must be a whole number from 1 to 40.',
      'Tick interval (minutes) must be a whole number from 1 to 1440.',
    ]);
  });
});

describe('decisionRetentionWarning', () => {
  it('names the window when set', () => {
    expect(decisionRetentionWarning(180)).toContain('180 days are permanently deleted');
  });

  it('distinguishes keep-forever with and without day-window overrides', () => {
    expect(decisionRetentionWarning(null, false)).toContain('kept forever');
    expect(decisionRetentionWarning(null, true)).toContain('day-window override');
  });

  it('returns empty string when the window input is invalid', () => {
    expect(decisionRetentionWarning('invalid')).toBe('');
  });
});

describe('lastDecisionRunSummary and lastDecisionPreviewSummary', () => {
  it('returns None yet when there are no non-dry-run runs', () => {
    expect(lastDecisionRunSummary(undefined)).toBe('None yet');
    expect(lastDecisionRunSummary([])).toBe('None yet');
    expect(lastDecisionRunSummary([run({ trigger: 'preview', dry_run: true })])).toBe('None yet');
  });

  it('formats completed runs with decisions, answers, and projects', () => {
    const text = lastDecisionRunSummary([run()]);
    expect(text).toContain('2026');
    expect(text).toContain('1,284 decisions, 1,190 answers deleted in 6 projects');
  });

  it('formats preview count as an estimate', () => {
    expect(lastDecisionPreviewSummary([])).toBe('');
    expect(lastDecisionPreviewSummary([], true)).toBe('Preview running…');
    expect(
      lastDecisionPreviewSummary([
        run({ trigger: 'preview', dry_run: true, totals: { decisions: 2310, answers: 2100 } }),
      ])
    ).toBe('Preview estimate: 2,310 decisions would be deleted now.');
    expect(
      lastDecisionPreviewSummary([
        run({ trigger: 'preview', dry_run: true, status: 'failed', error: 'deadline' }),
      ])
    ).toContain('Last preview failed: deadline');
  });
});

describe('needsDecisionPurgeConfirmation', () => {
  const on = { enabled: true, retention_days: 180, project_overrides: {} };
  it.each([
    [
      'enabling with a hub window',
      { enabled: false, retention_days: null, project_overrides: {} },
      { enabled: true, retention_days: 180, project_overrides: {} },
      true,
    ],
    [
      'enabling with only a days override',
      { enabled: false, retention_days: null, project_overrides: {} },
      {
        enabled: true,
        retention_days: null,
        project_overrides: { [PROJ_SCRATCH]: { mode: 'days', days: 30 } },
      },
      true,
    ],
    [
      'enabling with no window',
      { enabled: false, retention_days: null, project_overrides: {} },
      { enabled: true, retention_days: null, project_overrides: {} },
      false,
    ],
    [
      'shortening hub default',
      on,
      { enabled: true, retention_days: 30, project_overrides: {} },
      true,
    ],
    [
      'adding a shorter project override while enabled',
      on,
      {
        enabled: true,
        retention_days: 180,
        project_overrides: { [PROJ_SCRATCH]: { mode: 'days', days: 30 } },
      },
      true,
    ],
    [
      'removing a keep_forever hold while hub default is active',
      {
        enabled: true,
        retention_days: 180,
        project_overrides: { [PROJ_PAYMENTS]: { mode: 'keep_forever' } },
      },
      on,
      true,
    ],
    [
      'adding a keep_forever hold',
      on,
      {
        enabled: true,
        retention_days: 180,
        project_overrides: { [PROJ_PAYMENTS]: { mode: 'keep_forever' } },
      },
      false,
    ],
    [
      'lengthening hub default',
      on,
      { enabled: true, retention_days: 365, project_overrides: {} },
      false,
    ],
    ['disabling', on, { enabled: false, retention_days: 30, project_overrides: {} }, false],
  ])('%s: %s', (_name, saved, next, want) => {
    expect(needsDecisionPurgeConfirmation(saved, next)).toBe(want);
  });
});

describe('decisionRetentionSaveErrors', () => {
  it('lists the per-field errors of a validation_failed body', () => {
    expect(
      decisionRetentionSaveErrors(
        {
          error: 'validation_failed',
          errors: [
            { Path: `project_overrides.${PROJ_WEB}`, Message: 'project not found' },
            { Message: 'bad' },
          ],
        },
        'fallback'
      )
    ).toEqual([`project_overrides.${PROJ_WEB}: project not found`, 'bad']);
  });
});

describe('scion-admin-decision-retention', () => {
  let current: Settings;
  let putResponse: () => Response;

  beforeEach(() => {
    current = settings();
    putResponse = () => json(current);
    apiFetch.mockReset();
    apiFetch.mockImplementation((path, init) => {
      if (path === '/api/v1/projects') {
        return Promise.resolve(json({ projects: PROJECTS }));
      }
      return Promise.resolve(init?.method === 'PUT' ? putResponse() : json(current));
    });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  it('renders nothing and calls no API with the web flag off', async () => {
    const el = await mount();
    expect(q(el, '.decision-retention')).toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('loads when the flag turns on after mount', async () => {
    const el = await mount();
    setFeatureFlag(DECISION_RETENTION_FLAG, true);
    await settle();
    await el.updateComplete;
    expect(apiFetch).toHaveBeenCalledWith('/api/v1/admin/decision-retention');
    expect(q(el, '.decision-retention')).not.toBeNull();
  });

  describe('with the flag on', () => {
    beforeEach(() => setFeatureFlag(DECISION_RETENTION_FLAG, true));

    it('shows the mocks intro copy, loaded values, overrides table, preview estimate, and last run', async () => {
      current = settings({
        enabled: true,
        retention_days: 180,
        project_overrides: {
          [PROJ_PAYMENTS]: { mode: 'keep_forever' },
          [PROJ_SCRATCH]: { mode: 'days', days: 30 },
        },
        runs: [
          run({
            id: 'p1',
            trigger: 'preview',
            dry_run: true,
            totals: { decisions: 2310, answers: 2000 },
          }),
          run({ id: 's1' }),
        ],
      });
      const el = await mount();

      const intro = (q(el, '.intro')?.textContent ?? '').replace(/\s+/g, ' ');
      expect(intro).toContain('Nothing is deleted while retention is disabled.');
      expect(intro).toContain('Open decisions are never deleted.');
      expect(intro).toContain('Deletes are permanent. Take a database backup before enabling.');
      expect(q(el, '.retention-enabled')?.hasAttribute('checked')).toBe(true);
      expect(q<HTMLInputElement>(el, '.retention-days')?.value).toBe('180');
      expect(q(el, '.last-run')?.textContent).toContain(
        '1,284 decisions, 1,190 answers deleted in 6 projects'
      );
      expect(q(el, '.preview')?.textContent).toBe(
        'Preview estimate: 2,310 decisions would be deleted now.'
      );

      const rows = Array.from(el.shadowRoot?.querySelectorAll('tr.override-row') ?? []);
      expect(rows.length).toBe(2);
      expect(rows[0]?.textContent).toContain('payments-api');
      expect(rows[0]?.textContent).toContain('Keep forever');
      expect(rows[0]?.textContent).toContain('never deleted');
      expect(rows[1]?.textContent).toContain('scratch-experiments');
      expect(rows[1]?.textContent).toContain('30 days');
      expect(rows[1]?.textContent).toContain('after 30 days');
    });

    it('adds and removes project overrides and sends them in PUT', async () => {
      const el = await mount();
      await select(el, '.override-project', PROJ_PAYMENTS);
      await select(el, '.override-mode', 'keep_forever');
      q(el, '.add-override')?.click();
      await el.updateComplete;

      await select(el, '.override-project', PROJ_SCRATCH);
      await select(el, '.override-mode', 'days');
      await type(el, '.override-days', '45');
      q(el, '.add-override')?.click();
      await el.updateComplete;

      expect(el.shadowRoot?.querySelectorAll('tr.override-row').length).toBe(2);

      // Remove scratch-experiments before saving
      const scratchRow = q(el, `tr.override-row[data-project-id="${PROJ_SCRATCH}"]`);
      scratchRow?.querySelector<HTMLElement>('.remove-override')?.click();
      await el.updateComplete;
      expect(el.shadowRoot?.querySelectorAll('tr.override-row').length).toBe(1);

      putResponse = () =>
        json(
          settings({
            revision: 4,
            project_overrides: { [PROJ_PAYMENTS]: { mode: 'keep_forever' } },
          })
        );
      await save(el);

      expect(puts()).toEqual([
        expect.objectContaining({
          project_overrides: { [PROJ_PAYMENTS]: { mode: 'keep_forever' } },
          expected_revision: 3,
        }),
      ]);
    });

    it('validates add-override inputs before mutating the table', async () => {
      const el = await mount();
      q(el, '.add-override')?.click();
      await el.updateComplete;
      expect(q(el, '.override-error')?.textContent).toContain('Choose a project');

      await select(el, '.override-project', PROJ_SCRATCH);
      await type(el, '.override-days', '15');
      q(el, '.add-override')?.click();
      await el.updateComplete;
      expect(q(el, '.override-error')?.textContent).toContain('from 30 to 36500');
      expect(el.shadowRoot?.querySelectorAll('tr.override-row').length).toBe(0);
    });

    it('starts a preview via decision-retention-preview even while disabled', async () => {
      const el = await mount();
      apiFetch.mockImplementation((path, init) => {
        if (path === '/api/v1/projects') return Promise.resolve(json({ projects: PROJECTS }));
        if (path.endsWith('/decision-retention-preview/run')) {
          return Promise.resolve(json({ runId: 'run-9', status: 'running' }));
        }
        if (path.includes('/runs/')) return Promise.resolve(json({ status: 'running' }));
        return Promise.resolve(init?.method === 'PUT' ? putResponse() : json(current));
      });
      q(el, '.run-preview')?.click();
      await settle();
      await el.updateComplete;

      const post = apiFetch.mock.calls.find(([path]) =>
        path.endsWith('/decision-retention-preview/run')
      );
      expect(post?.[1]?.method).toBe('POST');
      expect(JSON.parse(post?.[1]?.body as string)).toEqual({ params: {} });
      expect(q(el, '.preview')?.textContent).toBe('Preview running…');
      el.remove();
    });

    it('disables Run now while retention is off and confirms before starting decision-retention-run', async () => {
      const el = await mount();
      expect(q(el, '.run-manual')?.hasAttribute('disabled')).toBe(true);
      el.remove();

      current = settings({
        enabled: true,
        retention_days: 180,
        runs: [
          run({
            id: 'p1',
            trigger: 'preview',
            dry_run: true,
            totals: { decisions: 50, answers: 40 },
          }),
        ],
      });
      const el2 = await mount();
      expect(q(el2, '.run-manual')?.hasAttribute('disabled')).toBe(false);

      apiFetch.mockImplementation((path, init) => {
        if (path === '/api/v1/projects') return Promise.resolve(json({ projects: PROJECTS }));
        if (path.endsWith('/decision-retention-run/run')) {
          return Promise.resolve(json({ runId: 'run-12', status: 'running' }));
        }
        if (path.includes('/runs/')) return Promise.resolve(json({ status: 'running' }));
        return Promise.resolve(init?.method === 'PUT' ? putResponse() : json(current));
      });

      q(el2, '.run-manual')?.click();
      await el2.updateComplete;
      expect(q(el2, '.manual-run-dialog')?.hasAttribute('open')).toBe(true);
      expect(q(el2, '.manual-run-preview')?.textContent).toContain(
        'Actual deletions may be slightly higher'
      );

      q(el2, '.manual-run-confirm')?.click();
      await settle();
      await el2.updateComplete;

      const post = apiFetch.mock.calls.find(([path]) =>
        path.endsWith('/decision-retention-run/run')
      );
      expect(post?.[1]?.method).toBe('POST');
      el2.remove();
    });

    it('surfaces 409 revision conflict and reloads settings', async () => {
      const el = await mount();
      await type(el, '.retention-days', '90');
      putResponse = () => {
        current = settings({ revision: 9, retention_days: 180 });
        return json({ error: { code: 'revision_conflict', message: 'conflict' } }, 409);
      };
      await save(el);

      expect(q(el, '.save-errors')?.textContent).toContain('changed by another administrator');
      expect(q<HTMLInputElement>(el, '.retention-days')?.value).toBe('180');
      expect(q(el, '.meta')?.textContent).toContain('Revision: 9');
    });

    it('says the experiment is off when the hub answers 404', async () => {
      apiFetch.mockImplementation((path) => {
        if (path === '/api/v1/projects') return Promise.resolve(json({ projects: PROJECTS }));
        return Promise.resolve(json({}, 404));
      });
      const el = await mount();

      expect(q(el, '.unavailable')?.textContent).toContain('hub.decision_retention is off');
      expect(q(el, '.retention-save')).toBeNull();
    });
  });
});
