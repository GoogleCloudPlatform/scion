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
 * The Chat retention panel of Admin > Server Config (hub.chat_retention):
 * shown only with the web flag on, revision-CAS saves, and validation
 * errors from the form and from the hub.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { CHAT_RETENTION_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());

vi.mock('../../client/api.js', async () => {
  const actual = await vi.importActual<typeof import('../../client/api.js')>('../../client/api.js');
  return { apiErrorMessageFromBody: actual.apiErrorMessageFromBody, apiFetch };
});

const mod = await import('./admin-chat-retention.js');
const { buildRetentionPutBody, retentionSaveErrors, retentionWarning } = mod;
type Panel = import('./admin-chat-retention.js').ScionAdminChatRetention;
type Settings = import('./admin-chat-retention.js').ChatRetentionSettings;

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
    messages_retention_days: null,
    deleted_conversation_grace_days: 30,
    sweep: { chunk_size: 200, pause_ms: 500, tick_budget_seconds: 30, tick_interval_minutes: 10 },
    last_applied_cutoff: null,
    revision: 3,
    ...over,
  };
}

const SWEEP = {
  chunk_size: '200',
  pause_ms: '500',
  tick_budget_seconds: '30',
  tick_interval_minutes: '10',
};

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function mount(): Promise<Panel> {
  const el = document.createElement('scion-admin-chat-retention');
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

/** Type into an sl-input and let the panel re-render. */
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

describe('buildRetentionPutBody', () => {
  it('sends an empty window as an explicit null with the loaded revision', () => {
    expect(
      buildRetentionPutBody({ enabled: true, days: '', graceDays: '30', sweep: SWEEP }, 4)
    ).toEqual({
      ok: true,
      body: {
        enabled: true,
        messages_retention_days: null,
        deleted_conversation_grace_days: 30,
        sweep: {
          chunk_size: 200,
          pause_ms: 500,
          tick_budget_seconds: 30,
          tick_interval_minutes: 10,
        },
        expected_revision: 4,
      },
    });
  });

  it('accepts a window of at least 7 days', () => {
    const r = buildRetentionPutBody(
      { enabled: true, days: ' 30 ', graceDays: '30', sweep: SWEEP },
      1
    );
    expect(r.ok && r.body.messages_retention_days).toBe(30);
  });

  it.each(['6', '0', '-7', '7.5', 'abc', '36501'])('refuses a window of %j', (days) => {
    const r = buildRetentionPutBody({ enabled: true, days, graceDays: '30', sweep: SWEEP }, 1);
    expect(r.ok).toBe(false);
    expect(!r.ok && r.errors[0]).toContain('Retention window');
  });

  it('accepts a grace of at least 1 day', () => {
    const r = buildRetentionPutBody({ enabled: true, days: '', graceDays: '1', sweep: SWEEP }, 1);
    expect(r.ok && r.body.deleted_conversation_grace_days).toBe(1);
  });

  it.each(['', '0', '-1', '1.5', 'x', '36501'])('refuses a grace of %j', (graceDays) => {
    const r = buildRetentionPutBody({ enabled: true, days: '', graceDays, sweep: SWEEP }, 1);
    expect(r.ok).toBe(false);
    expect(!r.ok && r.errors).toEqual([
      'Deleted conversation grace must be a whole number of days from 1 to 36500.',
    ]);
  });

  it('refuses sweep tuning outside its bounds', () => {
    const r = buildRetentionPutBody(
      {
        enabled: false,
        days: '',
        graceDays: '30',
        sweep: { ...SWEEP, chunk_size: '10', pause_ms: '' },
      },
      1
    );
    expect(r.ok).toBe(false);
    expect(!r.ok && r.errors).toEqual([
      'Chunk size must be a whole number from 25 to 1000.',
      'Pause between chunks (ms) must be a whole number from 0 to 10000.',
    ]);
  });
});

describe('retentionWarning', () => {
  it('names the window and the grace', () => {
    expect(retentionWarning(30, 1)).toBe(
      'While enabled, messages older than 30 days are permanently deleted from this hub. ' +
        'Messages of conversations and topics deleted more than 1 day ago are permanently deleted.'
    );
  });

  it('says only deleted conversations are purged without a window', () => {
    expect(retentionWarning(null, 30)).toBe(
      'No retention window is set: only the messages of deleted conversations and topics are purged. ' +
        'Messages of conversations and topics deleted more than 30 days ago are permanently deleted.'
    );
  });
});

describe('retentionWarning with an invalid window', () => {
  it('leaves the window out and keeps the grace', () => {
    expect(retentionWarning('invalid', 30)).toBe(
      'Messages of conversations and topics deleted more than 30 days ago are permanently deleted.'
    );
  });

  it('says nothing when neither field is valid', () => {
    expect(retentionWarning('invalid', null)).toBe('');
  });
});

describe('retentionSaveErrors', () => {
  it('lists the per-field errors of a validation_failed body', () => {
    expect(
      retentionSaveErrors(
        {
          error: 'validation_failed',
          errors: [{ Path: 'sweep/chunk_size', Message: 'must be >= 25' }, { Message: 'bad' }],
        },
        'fallback'
      )
    ).toEqual(['sweep/chunk_size: must be >= 25', 'bad']);
  });

  it('falls back to the error message, then the fallback', () => {
    expect(retentionSaveErrors({ error: { message: 'window too short' } }, 'fallback')).toEqual([
      'window too short',
    ]);
    expect(retentionSaveErrors(null, 'fallback')).toEqual(['fallback']);
  });
});

describe('scion-admin-chat-retention', () => {
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
    expect(q(el, '.chat-retention')).toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('loads when the flag turns on after mount', async () => {
    const el = await mount();
    setFeatureFlag(CHAT_RETENTION_FLAG, true);
    await settle();
    await el.updateComplete;
    expect(apiFetch).toHaveBeenCalledWith('/api/v1/admin/chat-retention');
    expect(q(el, '.chat-retention')).not.toBeNull();
  });

  describe('with the flag on', () => {
    beforeEach(() => setFeatureFlag(CHAT_RETENTION_FLAG, true));

    it('shows the explanation, the loaded values and a collapsed Advanced area', async () => {
      current = settings({
        enabled: true,
        messages_retention_days: 30,
        last_applied_cutoff: '2026-07-31T00:00:00Z',
      });
      const el = await mount();

      const intro = q(el, '.intro')?.textContent ?? '';
      expect(intro).toContain('Nothing is deleted while the sweep is disabled');
      expect(q(el, '.grace-explanation')?.textContent?.replace(/\s+/g, ' ')).toContain(
        'With the sweep enabled and no retention window, only the messages of deleted conversations and topics are purged'
      );
      expect(q<HTMLInputElement>(el, '.retention-grace-days')?.value).toBe('30');
      expect(intro).toContain('permanent');
      expect(intro).toContain('database backup');
      expect(intro).toContain('all chat messages on this hub');
      expect(q(el, '.retention-enabled')?.hasAttribute('checked')).toBe(true);
      expect(q<HTMLInputElement>(el, '.retention-days')?.value).toBe('30');
      expect(q(el, '.last-applied-cutoff')?.textContent).toContain('2026');
      expect(q<HTMLDetailsElement>(el, 'details.advanced')?.open).toBe(false);
      expect(el.shadowRoot?.querySelectorAll('.sweep-input').length).toBe(4);
      expect(q(el, '.delete-warning')?.textContent).toContain(
        'While enabled, messages older than 30 days are permanently deleted from this hub.'
      );
    });

    it('saves with expected_revision and shows the saved values', async () => {
      const el = await mount();
      await type(el, '.retention-days', '');
      await type(el, '.sweep-input[data-key="chunk_size"]', '100');
      putResponse = () =>
        json(settings({ revision: 4, sweep: { ...current.sweep, chunk_size: 100 } }));
      await save(el);

      expect(puts()).toEqual([
        {
          enabled: false,
          messages_retention_days: null,
          deleted_conversation_grace_days: 30,
          sweep: {
            chunk_size: 100,
            pause_ms: 500,
            tick_budget_seconds: 30,
            tick_interval_minutes: 10,
          },
          expected_revision: 3,
        },
      ]);
      expect(q(el, '.status-message.success')?.textContent).toContain('saved');
      expect(q(el, '.meta')?.textContent).toContain('Revision: 4');
    });

    it('warns that only deleted conversations are purged when enabled without a window', async () => {
      current = settings({ enabled: true, deleted_conversation_grace_days: 14 });
      const el = await mount();
      expect(q(el, '.delete-warning')?.textContent).toContain(
        'No retention window is set: only the messages of deleted conversations and topics are purged.'
      );
      expect(q(el, '.delete-warning')?.textContent).toContain('deleted more than 14 days ago');
    });

    it('does not describe a window the field does not hold', async () => {
      current = settings({ enabled: true, messages_retention_days: 30 });
      const el = await mount();
      for (const typed of ['abc', '3']) {
        await type(el, '.retention-days', typed);
        const text = q(el, '.delete-warning')?.textContent ?? '';
        expect(text).not.toContain('No retention window is set');
        expect(text).not.toContain('messages older than');
        expect(text).toContain('deleted more than 30 days ago');
      }
      await type(el, '.retention-grace-days', 'x');
      expect(q(el, '.delete-warning')).toBeNull();
    });

    it('saves a changed grace', async () => {
      const el = await mount();
      await type(el, '.retention-grace-days', '7');
      putResponse = () => json(settings({ revision: 4, deleted_conversation_grace_days: 7 }));
      await save(el);

      expect(puts()[0]).toMatchObject({ deleted_conversation_grace_days: 7, expected_revision: 3 });
      expect(q<HTMLInputElement>(el, '.retention-grace-days')?.value).toBe('7');
    });

    it('does not send a window shorter than 7 days', async () => {
      const el = await mount();
      await type(el, '.retention-days', '3');
      await save(el);

      expect(puts()).toEqual([]);
      expect(q(el, '.save-errors')?.textContent).toContain('from 7 to 36500');
    });

    it('shows the hub’s validation errors', async () => {
      const el = await mount();
      putResponse = () =>
        json(
          {
            error: 'validation_failed',
            errors: [{ Path: 'sweep/pause_ms', Message: 'must be <= 10000' }],
          },
          400
        );
      await save(el);

      expect(q(el, '.save-errors')?.textContent).toContain('sweep/pause_ms: must be <= 10000');
    });

    it('reloads the current values on a revision conflict', async () => {
      const el = await mount();
      await type(el, '.retention-days', '30');
      putResponse = () => {
        current = settings({ revision: 9, messages_retention_days: 90 });
        return json({ error: { code: 'revision_conflict', message: 'conflict' } }, 409);
      };
      await save(el);

      expect(q(el, '.save-errors')?.textContent).toContain('changed by another administrator');
      expect(q<HTMLInputElement>(el, '.retention-days')?.value).toBe('90');
      expect(q(el, '.meta')?.textContent).toContain('Revision: 9');
    });

    it('says the experiment is off when the hub answers 404', async () => {
      apiFetch.mockImplementation(() => Promise.resolve(json({}, 404)));
      const el = await mount();

      expect(q(el, '.unavailable')?.textContent).toContain('hub.chat_retention is off');
      expect(q(el, '.retention-save')).toBeNull();
    });
  });
});
