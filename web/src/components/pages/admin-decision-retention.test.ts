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
 * errors, last run summary, and confirmation before a save that deletes more.
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
  lastDecisionRunSummary,
  needsDecisionPurgeConfirmation,
} = mod;
type Panel = import('./admin-decision-retention.js').ScionAdminDecisionRetention;
type Settings = import('./admin-decision-retention.js').DecisionRetentionSettings;
type Run = import('./admin-decision-retention.js').DecisionRetentionRun;

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

async function save(el: Panel): Promise<void> {
  q(el, '.retention-save')?.click();
  await settle();
  await el.updateComplete;
}

describe('buildDecisionRetentionPutBody', () => {
  it('sends an empty window as an explicit null with the loaded revision', () => {
    expect(buildDecisionRetentionPutBody({ enabled: true, days: '', sweep: SWEEP }, 4)).toEqual({
      ok: true,
      body: {
        enabled: true,
        retention_days: null,
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
    expect(decisionRetentionWarning(180)).toBe(
      'While enabled, settled decisions with no activity for 180 days are permanently deleted from this hub.'
    );
  });

  it('explains keep-forever when no window is set', () => {
    expect(decisionRetentionWarning(null)).toBe(
      'No hub default window is set: settled decisions are kept forever.'
    );
  });

  it('returns empty string when the window input is invalid', () => {
    expect(decisionRetentionWarning('invalid')).toBe('');
  });
});

describe('lastDecisionRunSummary', () => {
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

  it('uses singular nouns for 1 decision, 1 answer, 1 project', () => {
    const text = lastDecisionRunSummary([
      run({ projects: 1, totals: { decisions: 1, answers: 1 } }),
    ]);
    expect(text).toContain('1 decision, 1 answer deleted in 1 project');
  });

  it('skips dry-run previews and reports running and failed states', () => {
    const skipped = lastDecisionRunSummary([
      run({ id: 'p', trigger: 'preview', dry_run: true, totals: { decisions: 9999, answers: 0 } }),
      run({ id: 's', totals: { decisions: 10, answers: 8 }, projects: 2 }),
    ]);
    expect(skipped).toContain('10 decisions, 8 answers deleted in 2 projects');
    expect(lastDecisionRunSummary([run({ status: 'running', finished_at: null })])).toContain(
      'Running (started'
    );
    expect(lastDecisionRunSummary([run({ status: 'failed', error: 'timeout' })])).toContain(
      'Failed'
    );
    expect(lastDecisionRunSummary([run({ status: 'failed', error: 'timeout' })])).toContain(
      'timeout'
    );
  });
});

describe('needsDecisionPurgeConfirmation', () => {
  const on = { enabled: true, retention_days: 180 };
  it.each([
    [
      'enabling with a window',
      { enabled: false, retention_days: null },
      { enabled: true, retention_days: 180 },
      true,
    ],
    [
      'enabling without a window',
      { enabled: false, retention_days: null },
      { enabled: true, retention_days: null },
      false,
    ],
    [
      'setting a first window while enabled',
      { enabled: true, retention_days: null },
      { enabled: true, retention_days: 90 },
      true,
    ],
    ['shortening the window', on, { enabled: true, retention_days: 30 }, true],
    ['lengthening the window', on, { enabled: true, retention_days: 365 }, false],
    ['keeping the same window', on, { enabled: true, retention_days: 180 }, false],
    ['removing the window', on, { enabled: true, retention_days: null }, false],
    ['disabling', on, { enabled: false, retention_days: 30 }, false],
    [
      'staying disabled',
      { enabled: false, retention_days: null },
      { enabled: false, retention_days: 30 },
      false,
    ],
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
          errors: [{ Path: 'sweep/page_size', Message: 'must be >= 50' }, { Message: 'bad' }],
        },
        'fallback'
      )
    ).toEqual(['sweep/page_size: must be >= 50', 'bad']);
  });

  it('falls back to the error message, then the fallback', () => {
    expect(
      decisionRetentionSaveErrors({ error: { message: 'window too short' } }, 'fallback')
    ).toEqual(['window too short']);
    expect(decisionRetentionSaveErrors(null, 'fallback')).toEqual(['fallback']);
  });
});

describe('scion-admin-decision-retention', () => {
  let current: Settings;
  let putResponse: () => Response;

  beforeEach(() => {
    current = settings();
    putResponse = () => json(current);
    apiFetch.mockReset();
    apiFetch.mockImplementation((_path, init) =>
      Promise.resolve(init?.method === 'PUT' ? putResponse() : json(current))
    );
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

    it('shows the mocks intro copy, loaded values, last run summary, and collapsed Advanced area', async () => {
      current = settings({
        enabled: true,
        retention_days: 180,
        runs: [run()],
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
      expect(q<HTMLDetailsElement>(el, 'details.advanced')?.open).toBe(false);
      expect(el.shadowRoot?.querySelectorAll('.sweep-input').length).toBe(3);
      expect(q(el, '.delete-warning')?.textContent).toContain(
        'While enabled, settled decisions with no activity for 180 days are permanently deleted from this hub.'
      );
    });

    it('saves with expected_revision and updates the revision', async () => {
      const el = await mount();
      await type(el, '.retention-days', '');
      await type(el, '.sweep-input[data-key="page_size"]', '100');
      putResponse = () =>
        json(settings({ revision: 4, sweep: { ...current.sweep, page_size: 100 } }));
      await save(el);

      expect(puts()).toEqual([
        {
          enabled: false,
          retention_days: null,
          sweep: {
            page_size: 100,
            tick_budget_seconds: 30,
            tick_interval_minutes: 60,
          },
          expected_revision: 3,
        },
      ]);
      expect(q(el, '.status-message.success')?.textContent).toContain('saved');
      expect(q(el, '.meta')?.textContent).toContain('Revision: 4');
    });

    it('does not send a window shorter than 30 days', async () => {
      const el = await mount();
      await type(el, '.retention-days', '29');
      await save(el);

      expect(puts()).toEqual([]);
      expect(q(el, '.save-errors')?.textContent).toContain('from 30 to 36500');
    });

    it('shows the hub validation errors', async () => {
      const el = await mount();
      putResponse = () =>
        json(
          {
            error: 'validation_failed',
            errors: [{ Path: 'sweep/page_size', Message: 'must be <= 1000' }],
          },
          400
        );
      await save(el);

      expect(q(el, '.save-errors')?.textContent).toContain('sweep/page_size: must be <= 1000');
    });

    it('reloads current values on a 409 revision conflict', async () => {
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

    it('asks for confirmation before enabling with a window, and saves only on confirm', async () => {
      const el = await mount();
      const sw = q<HTMLInputElement>(el, '.retention-enabled');
      if (sw) sw.checked = true;
      sw?.dispatchEvent(new Event('sl-change'));
      await el.updateComplete;
      await type(el, '.retention-days', '180');
      await save(el);

      expect(q(el, '.confirm-dialog')?.hasAttribute('open')).toBe(true);
      expect(q(el, '.confirm-change')?.textContent).toContain('180-day window');
      expect(puts()).toEqual([]);

      q(el, '.confirm-cancel')?.click();
      await el.updateComplete;
      expect(q(el, '.confirm-dialog')?.hasAttribute('open')).toBe(false);
      expect(puts()).toEqual([]);

      await save(el);
      putResponse = () => json(settings({ enabled: true, retention_days: 180, revision: 4 }));
      q(el, '.confirm-save')?.click();
      await settle();
      await el.updateComplete;
      expect(puts()).toEqual([expect.objectContaining({ enabled: true, retention_days: 180 })]);
      expect(q(el, '.confirm-dialog')?.hasAttribute('open')).toBe(false);
    });

    it('saves a longer window without asking for confirmation', async () => {
      current = settings({ enabled: true, retention_days: 90 });
      const el = await mount();
      await type(el, '.retention-days', '180');
      await save(el);
      expect(q(el, '.confirm-dialog')?.hasAttribute('open')).toBe(false);
      expect(puts()).toEqual([expect.objectContaining({ retention_days: 180 })]);
    });

    it('says the experiment is off when the hub answers 404', async () => {
      apiFetch.mockImplementation(() => Promise.resolve(json({}, 404)));
      const el = await mount();

      expect(q(el, '.unavailable')?.textContent).toContain('hub.decision_retention is off');
      expect(q(el, '.retention-save')).toBeNull();
    });
  });
});
