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
 * Hub card: the fleet of hub instances. Status, "N of M instances
 * healthy", version and the failing checks of live instances, each linked
 * to its instance row. Per-instance figures are in the Hub instances table.
 */

import { describe, it, expect, afterEach } from 'vitest';

import { fleetHealthyText, hubInstanceAnchor, type HealthSummaryHub } from './health-hub-card.js';
import './health-hub-card.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

function hub(over: Partial<HealthSummaryHub> = {}): HealthSummaryHub {
  return {
    status: 'healthy',
    instance_id: 'hub-a-1',
    version: 'v1',
    connected_brokers: 1,
    active_agents: 2,
    projects: 3,
    instances: { live: 3, healthy: 3, degraded: 0, unhealthy: 0 },
    unhealthy_checks: [],
    ...over,
  };
}

async function mount(h: HealthSummaryHub | null): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-hub-card');
  el.hub = h;
  document.body.appendChild(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

describe('scion-health-hub-card', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('shows the fleet status and N of M instances healthy', async () => {
    const root = await mount(
      hub({ status: 'degraded', instances: { live: 3, healthy: 2, degraded: 0, unhealthy: 1 } })
    );
    const pill = root.querySelector('[data-role="hub-status"]')!;
    expect(pill.textContent?.trim()).toBe('degraded');
    expect(pill.classList.contains('tone-warn')).toBe(true);
    expect(root.querySelector('[data-role="fleet"]')?.textContent?.trim()).toBe(
      '2 of 3 instances healthy'
    );
    expect(root.textContent).not.toContain('Checks and figures from this instance');
  });

  it('lists the failing checks with their instance labels, linked to the instance rows', async () => {
    const root = await mount(
      hub({
        status: 'unhealthy',
        instances: { live: 3, healthy: 1, degraded: 0, unhealthy: 2 },
        unhealthy_checks: [
          { instance_id: 'hub-b-1', instance_label: 'hub-b', name: 'database', value: 'unhealthy' },
          { instance_id: 'hub-c-1', instance_label: '', name: 'database', value: 'unhealthy' },
          {
            instance_id: 'hub-b-1',
            instance_label: 'hub-b',
            name: 'audit_log_writer',
            value: 'degraded',
          },
        ],
      })
    );
    const rows = [...root.querySelectorAll('[data-role="failing-checks"] li')] as HTMLElement[];
    expect(rows.map((r) => r.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'database on hub-b unhealthy',
      'database on hub-c-1 unhealthy',
      'audit_log_writer on hub-b degraded',
    ]);
    const link = rows[0]!.querySelector('a')!;
    expect(link.getAttribute('href')).toBe('#' + hubInstanceAnchor('hub-b-1'));
    expect(link.getAttribute('title')).toBe('hub-b-1');
    expect(rows[0]!.querySelector('.pill')!.classList.contains('tone-bad')).toBe(true);
    expect(rows[2]!.querySelector('.pill')!.classList.contains('tone-warn')).toBe(true);
  });

  it('shows no check list, uptime or pool when the fleet is healthy', async () => {
    const root = await mount(hub());
    expect(root.querySelector('[data-role="failing-checks"]')).toBeNull();
    expect(root.querySelector('[data-role="pool"]')).toBeNull();
    expect(root.textContent).not.toContain('Uptime');
    expect(root.querySelector('[data-role="version"]')?.textContent?.trim()).toBe('v1');
  });

  it('shows a mixed version as sent', async () => {
    const root = await mount(hub({ version: 'mixed' }));
    expect(root.querySelector('[data-role="version"]')?.textContent?.trim()).toBe('mixed');
  });

  it('says the instance data is not available when the fleet counts are missing', async () => {
    const root = await mount(hub({ status: 'unknown', instances: null, version: '' }));
    expect(root.querySelector('[data-role="fleet"]')?.textContent?.trim()).toBe(
      'Hub instance data not available'
    );
    expect(root.querySelector('[data-role="hub-status"]')?.classList.contains('tone-neutral')).toBe(
      true
    );
    expect(root.querySelector('[data-role="version"]')?.textContent?.trim()).toBe('—');
  });

  it('shows the service account check diagnostic only when the section is present', async () => {
    let root = await mount(hub());
    expect(root.querySelector('[data-role="sa-check"]')).toBeNull();
    document.body.innerHTML = '';

    const el = document.createElement('scion-health-hub-card');
    el.hub = hub({ status: 'degraded' });
    el.serviceAccountCheck = {
      status: 'degraded',
      cause: 'hub_identity_missing_access',
      remedy: "Grant the hub's identity that access.",
      docs_url: 'https://example.com/docs#check',
      since: '2026-10-08T12:00:00Z',
      last_seen: '2026-10-08T12:05:00Z',
    };
    document.body.appendChild(el);
    await el.updateComplete;
    root = el.shadowRoot!;
    const block = root.querySelector('[data-role="sa-check"]')!;
    expect(block.querySelector('.pill')?.textContent?.trim()).toBe('Cannot run');
    expect(block.querySelector('.pill')?.classList.contains('tone-warn')).toBe(true);
    expect(block.querySelector('.sa-check-remedy')?.textContent?.trim()).toBe(
      "Grant the hub's identity that access."
    );
    const link = block.querySelector('a')!;
    expect(link.getAttribute('href')).toBe('https://example.com/docs#check');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');

    // Only an https docs URL becomes a link; the remedy still shows.
    el.serviceAccountCheck = { ...el.serviceAccountCheck, docs_url: 'http://example.com/docs' };
    await el.updateComplete;
    expect(root.querySelector('[data-role="sa-check"] a')).toBeNull();
    expect(root.querySelector('.sa-check-remedy')).not.toBeNull();
  });

  it('shows not available rather than an empty card when the hub block is missing', async () => {
    const root = await mount(null);
    expect(root.textContent).toContain('Hub data not available');
  });

  it('uses theme tokens only, with no hex fallbacks', () => {
    for (const body of elementStyleRules('scion-health-hub-card').values()) {
      expect(body).not.toMatch(/#[0-9a-f]{3,8}\b|rgba?\(/i);
      for (const m of body.matchAll(/var\((--[\w-]+)/g)) expect(m[1]).toMatch(/^--scion-/);
    }
  });
});

describe('fleetHealthyText', () => {
  it('counts healthy of live instances', () => {
    expect(fleetHealthyText({ live: 3, healthy: 2, degraded: 1, unhealthy: 0 })).toBe(
      '2 of 3 instances healthy'
    );
    expect(fleetHealthyText({ live: 1, healthy: 1, degraded: 0, unhealthy: 0 })).toBe(
      '1 of 1 instance healthy'
    );
    expect(fleetHealthyText({ live: 0, healthy: 0, degraded: 0, unhealthy: 0 })).toBe(
      'No hub instance is reporting'
    );
  });

  it('is empty when the counts were not reported', () => {
    expect(fleetHealthyText(null)).toBe('');
    expect(fleetHealthyText(undefined)).toBe('');
  });
});

describe('hubInstanceAnchor', () => {
  it('encodes the instance ID', () => {
    expect(hubInstanceAnchor('pod-1 x/y')).toBe('hub-instance-pod-1%20x%2Fy');
  });
});
