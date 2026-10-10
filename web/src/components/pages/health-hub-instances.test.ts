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
 * Health dashboard hub instances table (ptone/scion#4136, ptone/scion#4138).
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  ScionHealthHubInstances,
  failingChecks,
  formatDuration,
  hubInstanceLabels,
  integrationCountsDetail,
  integrationCountsText,
  instanceLastSeen,
  instanceStateTone,
  instanceUptime,
  instanceIdFromHash,
  servingFirst,
  stoppedAtLabel,
  poolDetail,
  poolUsage,
  type HealthHubInstance,
  type HealthSummaryHubInstances,
} from './health-hub-instances.js';
import { HUB_INSTANCE_TARGET_EVENT, hubInstanceAnchor } from './health-hub-card.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';
import { formatInstantWithZone, setPreferredTimeZone } from '../../utils/time.js';

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
    database: { pool_active: 3, pool_idle: 2, pool_max: 25, pool_wait_count_total: 4 },
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
  setPreferredTimeZone('');
  history.replaceState(null, '', window.location.pathname);
});

describe('servingFirst', () => {
  const ids = (items: HealthHubInstance[]) => servingFirst(items).map((i) => i.id);

  it('moves the serving instance to the top of its state group', () => {
    expect(
      ids([
        instance({ id: 'a' }),
        instance({ id: 'b' }),
        instance({ id: 'c', serving: true }),
        instance({ id: 'd', state: 'stale' }),
      ])
    ).toEqual(['c', 'a', 'b', 'd']);
    expect(
      ids([
        instance({ id: 'a' }),
        instance({ id: 'b', state: 'stale' }),
        instance({ id: 'c', state: 'stale', serving: true }),
      ])
    ).toEqual(['a', 'c', 'b']);
  });

  it('keeps the order when the serving instance is first or absent', () => {
    expect(ids([instance({ id: 'a', serving: true }), instance({ id: 'b' })])).toEqual(['a', 'b']);
    expect(ids([instance({ id: 'a' }), instance({ id: 'b' })])).toEqual(['a', 'b']);
  });
});

describe('instanceIdFromHash', () => {
  it('reads the instance ID from a row fragment', () => {
    expect(instanceIdFromHash('#' + hubInstanceAnchor('pod-1 x'))).toBe('pod-1 x');
  });

  it('is null for other fragments or a bad encoding', () => {
    expect(instanceIdFromHash('')).toBeNull();
    expect(instanceIdFromHash('#other')).toBeNull();
    expect(instanceIdFromHash('#hub-instance-%E0%A4%A')).toBeNull();
  });
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

  it('formats the stop time in the display zone, falling back to the raw value', () => {
    expect(stoppedAtLabel('2026-10-09T11:50:00Z')).toBe(
      formatInstantWithZone('2026-10-09T11:50:00Z')
    );
    expect(stoppedAtLabel('not-a-time')).toBe('not-a-time');
  });

  it('lists failing checks as name: value, sorted; healthy and available pass', () => {
    expect(
      failingChecks({
        workspace_storage: 'unhealthy',
        database: 'healthy',
        docker: 'available',
        colocated_broker: 'unknown',
        mount: 'unavailable',
      })
    ).toEqual(['colocated_broker: unknown', 'mount: unavailable', 'workspace_storage: unhealthy']);
    expect(failingChecks(undefined)).toEqual([]);
  });

  it('formats the pool as in use / limit, or in use alone without a limit', () => {
    const db = { pool_active: 3, pool_idle: 2, pool_max: 25, pool_wait_count_total: 4 };
    expect(poolUsage(db)).toBe('3/25');
    expect(poolUsage({ ...db, pool_max: 0 })).toBe('3');
    expect(poolDetail(db)).toBe('3 in use, 2 idle, limit 25, 4 waits');
    expect(poolDetail({ ...db, pool_max: 0, pool_wait_count_total: 1 })).toBe(
      '3 in use, 2 idle, no limit, 1 wait'
    );
  });

  it('maps states to tones', () => {
    expect(instanceStateTone('live')).toBe('ok');
    expect(instanceStateTone('stale')).toBe('warn');
    expect(instanceStateTone('stopped')).toBe('neutral');
  });
});

describe('hub instance integration counts', () => {
  const counts = (over = {}) => ({
    total: 0,
    healthy: 0,
    degraded: 0,
    unhealthy: 0,
    unknown: 0,
    ...over,
  });

  it('shows the total and the non-healthy counts', () => {
    expect(integrationCountsText(counts({ total: 2, healthy: 2 }))).toBe('2');
    expect(
      integrationCountsText(counts({ total: 4, healthy: 1, unhealthy: 1, degraded: 1, unknown: 1 }))
    ).toBe('4 (1 unhealthy, 1 degraded, 1 unknown)');
    expect(integrationCountsText(counts({ total: 3, healthy: 3 }), true)).toBe('3+');
  });

  it('is empty when the instance runs no integration or sent no counts', () => {
    expect(integrationCountsText(counts())).toBe('');
    expect(integrationCountsText(undefined)).toBe('');
    expect(integrationCountsDetail(counts())).toBe('');
  });

  it('lists every count in the tooltip', () => {
    expect(integrationCountsDetail(counts({ total: 3, healthy: 1, degraded: 1, unknown: 1 }))).toBe(
      '1 healthy, 1 degraded, 0 unhealthy, 1 unknown'
    );
  });

  it('maps instance IDs to labels, falling back to the ID', () => {
    expect(
      hubInstanceLabels(
        list([instance({ id: 'a1', label: 'hub-a' }), instance({ id: 'b2', label: '' })])
      )
    ).toEqual({ a1: 'hub-a', b2: 'b2' });
    expect(hubInstanceLabels(null)).toEqual({});
  });
});

describe('scion-health-hub-instances', () => {
  it("shows each instance's integration counts, with a dash for none", async () => {
    const root = await mount(
      list([
        instance({
          id: 'a1',
          integration_counts: { total: 2, healthy: 1, degraded: 0, unhealthy: 1, unknown: 0 },
        }),
        instance({ id: 'b2' }),
      ])
    );
    const [a, b] = rows(root);
    expect(cell(a, 'integrations')).toBe('2 (1 unhealthy)');
    expect(a.querySelector('td.integrations')?.getAttribute('title')).toBe(
      '1 healthy, 0 degraded, 1 unhealthy, 0 unknown'
    );
    expect(cell(b, 'integrations')).toBe('—');
  });

  it('renders one row per instance with the eight columns', async () => {
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
          database: { pool_active: 9, pool_idle: 0, pool_max: 10, pool_wait_count_total: 17 },
        }),
      ])
    );
    const headers = [...root.querySelectorAll('th')].map((th) => th.textContent?.trim());
    expect(headers).toEqual([
      'Label',
      'State',
      'Version',
      'Uptime',
      'Status',
      'DB pool',
      'Integrations',
      'Last seen',
    ]);

    const [a, b] = rows(root);
    expect(a.dataset.instanceId).toBe('hub-a-0123');
    expect(cell(a, 'label')).toBe('hub-a (this instance)');
    expect(a.querySelector('td.label')?.getAttribute('title')).toBe('hub-a-0123');
    expect(cell(a, 'state')).toBe('live');
    expect(a.querySelector('td.state .pill')?.classList.contains('tone-ok')).toBe(true);
    expect(cell(a, 'version')).toBe('v1.2.3');
    expect(cell(a, 'uptime')).toBe('2h 30m');
    expect(cell(a, 'status')).toBe('healthy');
    expect(cell(a, 'pool')).toBe('3/25');
    expect(a.querySelector('td.pool')?.getAttribute('title')).toBe(
      '3 in use, 2 idle, limit 25, 4 waits'
    );
    expect(cell(a, 'last-seen')).toBe('12s ago');

    expect(cell(b, 'label')).toBe('hub-b');
    expect(cell(b, 'state')).toBe('stale');
    expect(b.querySelector('td.state .pill')?.classList.contains('tone-warn')).toBe(true);
    expect(cell(b, 'version')).toBe('v1.3.0');
    expect(cell(b, 'uptime')).toBe('—');
    // A stale instance's status is out of date: shown greyed, not as a pill.
    expect(cell(b, 'status')).toBe('last reported: degraded');
    expect(b.querySelector('td.status .pill')).toBeNull();
    // Each instance shows its own pool.
    expect(cell(b, 'pool')).toBe('9/10');
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

  it('re-renders the stop-time tooltip when the display zone changes', async () => {
    const stoppedAt = '2026-10-09T11:50:00Z';
    setPreferredTimeZone('UTC');
    const root = await mount(
      list([instance({ state: 'stopped', last_seen: stoppedAt, stopped_at: stoppedAt })])
    );
    const title = () => rows(root)[0].querySelector('td.state')?.getAttribute('title');
    expect(title()).toContain('(UTC)');

    setPreferredTimeZone('Asia/Kathmandu');
    const el = root.host as ScionHealthHubInstances;
    await el.updateComplete;
    expect(title()).toBe(`stopped ${formatInstantWithZone(stoppedAt)}`);
    expect(title()).toContain('(Asia/Kathmandu)');
  });

  it('shows the failing checks under the status', async () => {
    const root = await mount(
      list([
        instance({
          status: 'degraded',
          checks: { database: 'healthy', colocated_broker: 'unhealthy', mount: 'unknown' },
        }),
        instance({ id: 'hub-b-4567' }),
      ])
    );
    const [a, b] = rows(root);
    const failing = [...a.querySelectorAll('[data-role="failing-checks"] li')].map((li) =>
      li.textContent?.trim()
    );
    expect(failing).toEqual(['colocated_broker: unhealthy', 'mount: unknown']);
    expect(a.querySelector('td.status .pill')?.textContent?.trim()).toBe('degraded');
    expect(b.querySelector('[data-role="failing-checks"]')).toBeNull();
  });

  it('shows a dash when an instance reported no pool', async () => {
    const root = await mount(list([instance({ database: null }), instance({ id: 'x' })]));
    const [a] = rows(root);
    expect(cell(a, 'pool')).toBe('—');
    expect(a.querySelector('td.pool')?.getAttribute('title')).toBe('');
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

  it('lists the serving instance first and gives every row its anchor ID', async () => {
    const root = await mount(
      list([
        instance({ id: 'hub-a-1', label: 'hub-a' }),
        instance({ id: 'hub-b-1', label: 'hub-b', serving: true }),
      ])
    );
    const [first, second] = rows(root);
    expect(first!.dataset.instanceId).toBe('hub-b-1');
    expect(first!.id).toBe(hubInstanceAnchor('hub-b-1'));
    expect(second!.id).toBe(hubInstanceAnchor('hub-a-1'));
  });

  it('highlights the row named by the location hash', async () => {
    history.replaceState(null, '', '#' + hubInstanceAnchor('hub-b-1'));
    const root = await mount(
      list([
        instance({ id: 'hub-a-1', label: 'hub-a' }),
        instance({ id: 'hub-b-1', label: 'hub-b' }),
      ])
    );
    const byId = (id: string) => rows(root).find((r) => r.dataset.instanceId === id)!;
    expect(byId('hub-b-1').classList.contains('target')).toBe(true);
    expect(byId('hub-a-1').classList.contains('target')).toBe(false);

    // A later link to another instance moves the highlight.
    history.replaceState(null, '', '#' + hubInstanceAnchor('hub-a-1'));
    window.dispatchEvent(new HashChangeEvent('hashchange'));
    const el = root.host as ScionHealthHubInstances;
    await el.updateComplete;
    expect(byId('hub-a-1').classList.contains('target')).toBe(true);
    expect(byId('hub-b-1').classList.contains('target')).toBe(false);
  });

  it('scrolls to the row again when the same target is named again', async () => {
    const proto = Element.prototype as unknown as { scrollIntoView?: (o?: unknown) => void };
    const original = proto.scrollIntoView;
    const scrolled: string[] = [];
    proto.scrollIntoView = function (this: Element) {
      scrolled.push((this as HTMLElement).dataset.instanceId ?? '');
    };
    try {
      history.replaceState(null, '', '/health#' + hubInstanceAnchor('hub-a-1'));
      const root = await mount(list([instance({ id: 'hub-a-1', label: 'hub-a' })]));
      const el = root.host as ScionHealthHubInstances;
      await el.updateComplete;
      expect(scrolled).toEqual(['hub-a-1']);

      // A poll re-render does not scroll again.
      el.instances = list([instance({ id: 'hub-a-1', label: 'hub-a' })]);
      await el.updateComplete;
      expect(scrolled).toEqual(['hub-a-1']);

      // A repeated click on the same link (same hash) scrolls again.
      window.dispatchEvent(new CustomEvent(HUB_INSTANCE_TARGET_EVENT, { detail: 'hub-a-1' }));
      await el.updateComplete;
      expect(scrolled).toEqual(['hub-a-1', 'hub-a-1']);
    } finally {
      proto.scrollIntoView = original;
    }
  });

  it('follows in-page links and Back through the target event and popstate', async () => {
    history.replaceState(null, '', '/health');
    const root = await mount(
      list([
        instance({ id: 'hub-a-1', label: 'hub-a' }),
        instance({ id: 'hub-b-1', label: 'hub-b' }),
      ])
    );
    const el = root.host as ScionHealthHubInstances;
    const target = () => rows(root).find((r) => r.classList.contains('target'))?.dataset.instanceId;
    expect(target()).toBeUndefined();

    history.pushState({}, '', '#' + hubInstanceAnchor('hub-a-1'));
    window.dispatchEvent(new CustomEvent(HUB_INSTANCE_TARGET_EVENT, { detail: 'hub-a-1' }));
    await el.updateComplete;
    expect(target()).toBe('hub-a-1');

    // Back to the entry without a fragment.
    history.replaceState({}, '', '/health');
    window.dispatchEvent(new PopStateEvent('popstate', { state: {} }));
    await el.updateComplete;
    expect(target()).toBeUndefined();
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
