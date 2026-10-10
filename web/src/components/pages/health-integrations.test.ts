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
 * Health dashboard Integrations section (ptone/scion#3584).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

import { apiFetch } from '../../client/api.js';
import {
  ScionHealthIntegrations,
  integrationHealthTone,
  integrationsSectionVisible,
  type HealthSummaryIntegration,
} from './health-integrations.js';
import { ScionPageHealthDashboard } from './health-dashboard.js';

function integration(over: Partial<HealthSummaryIntegration> = {}): HealthSummaryIntegration {
  return {
    name: 'telegram',
    platform: 'telegram',
    health: 'healthy',
    connected: true,
    version: 'v1.2.3',
    ...over,
  };
}

const mounted: HTMLElement[] = [];

async function mount(items: HealthSummaryIntegration[] | null): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-integrations') as ScionHealthIntegrations;
  el.integrations = items;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot as ShadowRoot;
}

function cell(row: Element | null | undefined, cls: string): string {
  return (row?.querySelector(`td.${cls}`)?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.remove();
  document.body.innerHTML = '';
  vi.mocked(apiFetch).mockReset();
});

describe('scion-health-integrations without identity', () => {
  async function mountCounts(
    counts: {
      total: number;
      healthy: number;
      degraded: number;
      unhealthy: number;
      unknown: number;
    } | null,
    items: HealthSummaryIntegration[] = []
  ): Promise<ShadowRoot> {
    const el = document.createElement('scion-health-integrations') as ScionHealthIntegrations;
    el.integrations = items;
    el.detail = false;
    el.counts = counts;
    document.body.appendChild(el);
    mounted.push(el);
    await el.updateComplete;
    return el.shadowRoot as ShadowRoot;
  }

  it('renders no names, rows or links, even if a list were sent', async () => {
    const root = await mountCounts(
      { total: 1, healthy: 0, degraded: 1, unhealthy: 0, unknown: 0 },
      [integration()]
    );
    expect(root.querySelector('table')).toBeNull();
    expect(root.querySelector('a')).toBeNull();
    expect(root.textContent).not.toContain('telegram');
    expect(root.querySelector('[data-count="degraded"]')?.classList.contains('tone-warn')).toBe(
      true
    );
  });

  it('shows the section only when it has rows or counts, and renders nothing with no integrations', async () => {
    expect(integrationsSectionVisible(false, [integration()], null)).toBe(false);
    expect(
      integrationsSectionVisible(false, [], {
        total: 1,
        healthy: 1,
        degraded: 0,
        unhealthy: 0,
        unknown: 0,
      })
    ).toBe(true);
    expect(
      integrationsSectionVisible(true, [], {
        total: 1,
        healthy: 1,
        degraded: 0,
        unhealthy: 0,
        unknown: 0,
      })
    ).toBe(false);
    expect(integrationsSectionVisible(true, [integration()], null)).toBe(true);
    for (const counts of [null, { total: 0, healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 }]) {
      const root = await mountCounts(counts);
      expect(root.querySelector('section')).toBeNull();
    }
  });
});

describe('scion-health-integrations', () => {
  it('renders nothing when there are no plugins', async () => {
    for (const items of [null, []]) {
      const root = await mount(items);
      expect(root.querySelector('.card')).toBeNull();
      expect(root.textContent?.trim()).toBe('');
    }
  });

  it('renders one row per plugin with name, platform, health, connected and version', async () => {
    const root = await mount([integration({ name: 'chat-app', platform: 'gchat' })]);
    const row = root.querySelector('tbody tr[data-integration="chat-app"]');
    expect(cell(row, 'name')).toBe('chat-app');
    expect(cell(row, 'platform')).toBe('gchat');
    expect(cell(row, 'health')).toBe('healthy');
    expect(row?.querySelector('td.health .pill')?.classList.contains('tone-ok')).toBe(true);
    expect(cell(row, 'connected')).toBe('yes');
    expect(cell(row, 'version')).toBe('v1.2.3');
  });

  it('links to the Integrations admin page', async () => {
    const root = await mount([integration({ name: 'a2a bridge' })]);
    expect(root.querySelector('a.manage')?.getAttribute('href')).toBe('/admin/integrations');
    expect(root.querySelector('td.name a')?.getAttribute('href')).toBe(
      '/admin/integrations/a2a%20bridge'
    );
  });

  it('renders an unknown-health plugin neutrally with its reason', async () => {
    const root = await mount([
      integration({
        name: 'discord',
        platform: 'discord',
        health: 'unknown',
        connected: false,
        version: '',
        reason: 'not managed by this hub instance',
      }),
    ]);
    const row = root.querySelector('tbody tr');
    const pill = row?.querySelector('td.health .pill');
    expect(pill?.textContent?.trim()).toBe('unknown');
    expect(pill?.classList.contains('tone-neutral')).toBe(true);
    expect(row?.querySelector('td.health .reason')?.textContent?.trim()).toBe(
      'not managed by this hub instance'
    );
    expect(cell(row, 'connected')).toBe('no');
    expect(cell(row, 'version')).toBe('—');
  });

  it('maps health to theme tones', () => {
    expect(integrationHealthTone('healthy')).toBe('ok');
    expect(integrationHealthTone('degraded')).toBe('warn');
    expect(integrationHealthTone('unhealthy')).toBe('bad');
    expect(integrationHealthTone('unknown')).toBe('neutral');
    expect(integrationHealthTone('')).toBe('neutral');
  });
});

describe('scion-page-health-dashboard integrations', () => {
  function summary(integrations: HealthSummaryIntegration[] | undefined): Response {
    return new Response(
      JSON.stringify({
        status: 'healthy',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 0 },
        runtime_brokers: { items: [], total: 0, truncated: false },
        integrations,
        integrations_detail: true,
        integration_counts: { total: 0, healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 },
        agents: { total: 0, active: 0, errored: 0, considered: 0, by_phase: [], problems: [] },
        dispatch: null,
      }),
      { status: 200, headers: { 'Content-Type': 'application/json' } }
    );
  }

  async function page(): Promise<ScionPageHealthDashboard> {
    const el = new ScionPageHealthDashboard();
    document.body.appendChild(el);
    await refresh(el);
    return el;
  }

  async function refresh(el: ScionPageHealthDashboard): Promise<void> {
    await (el as unknown as { fetchData(): Promise<void> }).fetchData();
    await el.updateComplete;
    const section = el.shadowRoot?.querySelector('scion-health-integrations');
    if (section) await section.updateComplete;
  }

  function row(el: ScionPageHealthDashboard, name: string): Element | null | undefined {
    return el.shadowRoot
      ?.querySelector('scion-health-integrations')
      ?.shadowRoot?.querySelector(`tbody tr[data-integration="${name}"]`);
  }

  it('hides the section when the summary has no plugins', async () => {
    for (const integrations of [[], undefined]) {
      vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(summary(integrations)));
      const el = await page();
      expect(el.shadowRoot?.querySelector('scion-health-hub-card')).not.toBeNull();
      // No section and no empty full-width wrapper adding a second gap.
      expect(el.shadowRoot?.querySelector('scion-health-integrations')).toBeNull();
      for (const full of el.shadowRoot?.querySelectorAll('.grid-full') ?? []) {
        expect(full.children.length).toBeGreaterThan(0);
      }
      el.remove();
    }
  });

  it('updates a row when the next refresh reports the plugin stopped', async () => {
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(summary([integration()])));
    const el = await page();
    expect(cell(row(el, 'telegram'), 'health')).toBe('healthy');
    expect(cell(row(el, 'telegram'), 'connected')).toBe('yes');

    vi.mocked(apiFetch).mockImplementation(() =>
      Promise.resolve(summary([integration({ health: 'unknown', connected: false, version: '' })]))
    );
    await refresh(el);
    expect(cell(row(el, 'telegram'), 'health')).toBe('unknown');
    expect(cell(row(el, 'telegram'), 'connected')).toBe('no');
  });

  it('shows plugins only under Integrations, not as runtime brokers', async () => {
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(summary([integration()])));
    const el = await page();
    const table = el.shadowRoot?.querySelector('scion-health-broker-table');
    await table?.updateComplete;
    expect(table?.shadowRoot?.textContent).not.toContain('telegram');
    expect(row(el, 'telegram')).toBeTruthy();
  });

  it('shows only the aggregate counts when the summary has no integration identity', async () => {
    vi.mocked(apiFetch).mockImplementation(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            status: 'degraded',
            attention: [
              {
                severity: 'warning',
                kind: 'integration',
                subject: { type: 'integration' },
                message: '1 integration unhealthy',
              },
            ],
            hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 0 },
            runtime_brokers: { items: [], total: 0, truncated: false },
            integrations: [],
            integrations_detail: false,
            integration_counts: { total: 3, healthy: 2, degraded: 0, unhealthy: 1, unknown: 0 },
            agents: null,
            dispatch: null,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );
    const el = await page();
    const section = el.shadowRoot?.querySelector('scion-health-integrations');
    const root = section!.shadowRoot!;
    expect(root.querySelector('table')).toBeNull();
    expect(root.querySelector('a')).toBeNull();
    const counts = root.querySelector('[data-role="counts"]')!;
    const parts = [...counts.children].map((c) => c.textContent?.replace(/\s+/g, ' ').trim());
    expect(parts).toEqual(['3 integrations', '2 healthy', '1 unhealthy']);

    const panel = el.shadowRoot?.querySelector('scion-health-attention');
    await panel?.updateComplete;
    const item = panel!.shadowRoot!.querySelector('li');
    expect(item?.textContent?.trim()).toBe('1 integration unhealthy');
    expect(item?.querySelector('a')).toBeNull();
  });
});
