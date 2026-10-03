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
 * help text.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

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
