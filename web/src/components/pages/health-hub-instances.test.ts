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
 * Health dashboard hub instances table (ptone/scion#4136).
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  ScionHealthHubInstances,
  formatDuration,
  instanceLastSeen,
  instanceStateTone,
  instanceUptime,
  type HealthHubInstance,
  type HealthSummaryHubInstances,
} from './health-hub-instances.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';
import { formatInstantWithZone } from '../../utils/time.js';

const GENERATED_AT = '2026-10-09T12:00:00Z';

function instance(over: Partial<HealthHubInstance> = {}): HealthHubInstance {
  return {
    id: 'hub-a-0123',
    label: 'hub-a',
    version: 'v1.2.3',
    state: 'live',
    serving: false,
    started_at: '2026-10-09T09:30:00Z',
    last_seen: '2026-10-09T11:59:48Z',
    stopped_at: null,
    status: 'healthy',
    checks: { database: 'healthy' },
    ...over,
  };
}

function list(
  items: HealthHubInstance[],
  over: Partial<HealthSummaryHubInstances> = {}
): HealthSummaryHubInstances {
  return {
    items,
    live: items.filter((i) => i.state === 'live').length,
    total: items.length,
    truncated: false,
    ...over,
  };
}

const mounted: HTMLElement[] = [];

async function mount(instances: HealthSummaryHubInstances | null): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-hub-instances') as ScionHealthHubInstances;
  el.instances = instances;
  el.generatedAt = GENERATED_AT;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot as ShadowRoot;
}

function rows(root: ShadowRoot): HTMLTableRowElement[] {
  return [...root.querySelectorAll<HTMLTableRowElement>('tbody tr')];
}

function cell(row: Element, cls: string): string {
  return (row.querySelector(`td.${cls}`)?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.remove();
});

describe('formatDuration', () => {
  it('uses the two largest units', () => {
    expect(formatDuration(12_000)).toBe('12s');
    expect(formatDuration(4 * 60_000 + 10_000)).toBe('4m 10s');
    expect(formatDuration(2 * 3_600_000 + 5 * 60_000)).toBe('2h 5m');
    expect(formatDuration(3 * 86_400_000 + 4 * 3_600_000 + 59_000)).toBe('3d 4h');
  });

  it('renders negative or invalid durations as empty', () => {
    expect(formatDuration(-1)).toBe('');
    expect(formatDuration(Number.NaN)).toBe('');
  });
});

describe('hub instance cells', () => {
  it('computes uptime from generated_at for a live instance only', () => {
    expect(instanceUptime(instance(), GENERATED_AT)).toBe('2h 30m');
    expect(instanceUptime(instance({ state: 'stale' }), GENERATED_AT)).toBe('');
    expect(instanceUptime(instance({ state: 'stopped' }), GENERATED_AT)).toBe('');
  });

  it('computes last seen from generated_at, not the browser clock', () => {
    expect(instanceLastSeen(instance(), GENERATED_AT)).toBe('12s ago');
    expect(instanceLastSeen(instance({ last_seen: '2026-10-09T11:59:14Z' }), GENERATED_AT)).toBe(
      '46s ago'
    );
  });

  it('maps states to tones', () => {
    expect(instanceStateTone('live')).toBe('ok');
    expect(instanceStateTone('stale')).toBe('warn');
    expect(instanceStateTone('stopped')).toBe('neutral');
  });
});

describe('scion-health-hub-instances', () => {
  it('renders one row per instance with the six columns', async () => {
    const root = await mount(
      list([
        instance({ id: 'hub-a-0123', label: 'hub-a', serving: true }),
        instance({
          id: 'hub-b-4567',
          label: 'hub-b',
          version: 'v1.3.0',
          state: 'stale',
          status: 'degraded',
          last_seen: '2026-10-09T11:59:00Z',
        }),
      ])
    );
    const headers = [...root.querySelectorAll('th')].map((th) => th.textContent?.trim());
    expect(headers).toEqual(['Label', 'State', 'Version', 'Uptime', 'Status', 'Last seen']);

    const [a, b] = rows(root);
    expect(a.dataset.instanceId).toBe('hub-a-0123');
    expect(cell(a, 'label')).toBe('hub-a (this instance)');
    expect(a.querySelector('td.label')?.getAttribute('title')).toBe('hub-a-0123');
    expect(cell(a, 'state')).toBe('live');
    expect(a.querySelector('td.state .pill')?.classList.contains('tone-ok')).toBe(true);
    expect(cell(a, 'version')).toBe('v1.2.3');
    expect(cell(a, 'uptime')).toBe('2h 30m');
    expect(cell(a, 'status')).toBe('healthy');
    expect(cell(a, 'last-seen')).toBe('12s ago');

    expect(cell(b, 'label')).toBe('hub-b');
    expect(cell(b, 'state')).toBe('stale');
    expect(b.querySelector('td.state .pill')?.classList.contains('tone-warn')).toBe(true);
    expect(cell(b, 'version')).toBe('v1.3.0');
    expect(cell(b, 'uptime')).toBe('—');
    // A stale instance's status is out of date: shown greyed, not as a pill.
    expect(cell(b, 'status')).toBe('last reported: degraded');
    expect(b.querySelector('td.status .pill')).toBeNull();
    expect(cell(b, 'last-seen')).toBe('1m 0s ago');
  });

  it('greys a stopped instance and shows its last reported status', async () => {
    const root = await mount(
      list([
        instance({ id: 'hub-a-0123', serving: true }),
        instance({
          id: 'hub-old-89ab',
          label: 'hub-old',
          state: 'stopped',
          status: 'healthy',
          last_seen: '2026-10-09T11:50:00Z',
          stopped_at: '2026-10-09T11:50:00Z',
        }),
      ])
    );
    const [live, stopped] = rows(root);
    expect(live.classList.contains('stopped')).toBe(false);
    expect(stopped.classList.contains('stopped')).toBe(true);
    expect(stopped.dataset.state).toBe('stopped');
    expect(cell(stopped, 'state')).toBe('stopped');
    expect(stopped.querySelector('td.state .pill')?.classList.contains('tone-neutral')).toBe(true);
    // The stop time is shown in the display zone, like other absolute times.
    expect(stopped.querySelector('td.state')?.getAttribute('title')).toBe(
      `stopped ${formatInstantWithZone('2026-10-09T11:50:00Z')}`
    );
    expect(formatInstantWithZone('2026-10-09T11:50:00Z')).not.toBe('');
    expect(live.querySelector('td.state')?.hasAttribute('title')).toBe(false);
    expect(cell(stopped, 'uptime')).toBe('—');
    expect(cell(stopped, 'status')).toBe('last reported: healthy');
    expect(stopped.querySelector('td.status .pill')).toBeNull();
    expect(cell(stopped, 'last-seen')).toBe('10m 0s ago');

    expect(elementStyleRules('scion-health-hub-instances').get('tr.stopped td')).toContain(
      'var(--scion-text-muted)'
    );
  });

  it('computes uptime and last seen from as_of (the database clock), not generated_at', async () => {
    // generatedAt is GENERATED_AT (12:00:00); the database clock is 11:00:00.
    const root = await mount(
      list(
        [
          instance({
            started_at: '2026-10-09T10:30:00Z',
            last_seen: '2026-10-09T10:59:50Z',
          }),
        ],
        { as_of: '2026-10-09T11:00:00Z' }
      )
    );
    const row = rows(root)[0];
    expect(cell(row, 'uptime')).toBe('30m 0s');
    expect(cell(row, 'last-seen')).toBe('10s ago');
  });

  it('falls back to the ID when the label is empty', async () => {
    const root = await mount(list([instance({ label: '' })]));
    expect(cell(rows(root)[0], 'label')).toBe('hub-a-0123');
  });

  it('shows the truncation note', async () => {
    const root = await mount(list([instance()], { total: 60, truncated: true }));
    expect(root.querySelector('.note')?.textContent).toContain('Showing 1 of 60');
  });

  it('shows not available when the section is null', async () => {
    const root = await mount(null);
    expect(root.textContent).toContain('Hub instance data not available');
    expect(rows(root)).toHaveLength(0);
  });

  it('shows an empty state when no instance is listed', async () => {
    const root = await mount(list([]));
    expect(root.textContent).toContain('No hub instance is reporting');
  });

  it('styles with theme tokens only, with no hex fallbacks', () => {
    const css = [...elementStyleRules('scion-health-hub-instances').values()].join('\n');
    expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
  });
});
