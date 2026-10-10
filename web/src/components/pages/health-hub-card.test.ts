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
 * Hub card: every check as a row. The database pool is per instance, in
 * the Hub instances table.
 */

import { describe, it, expect, afterEach } from 'vitest';

import { hubCheckRows, type HealthSummaryHub } from './health-hub-card.js';
import './health-hub-card.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

function hub(over: Partial<HealthSummaryHub> = {}): HealthSummaryHub {
  return {
    status: 'healthy',
    instance_id: 'hub-a',
    version: 'v1',
    uptime: '1h',
    connected_brokers: 1,
    active_agents: 2,
    projects: 3,
    checks: { workspace_storage: 'healthy', database: 'healthy', colocated_broker: 'healthy' },
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

  it('lists every check as a row sorted by name, with its status', async () => {
    const root = await mount(
      hub({
        checks: {
          workspace_storage: 'unhealthy: mount not available',
          database: 'healthy',
          colocated_broker: 'healthy',
        },
      })
    );
    const rows = [...root.querySelectorAll('li[data-check]')] as HTMLElement[];
    expect(rows.map((r) => r.dataset.check)).toEqual([
      'colocated_broker',
      'database',
      'workspace_storage',
    ]);
    const ws = rows[2]!.querySelector('.pill')!;
    expect(ws.textContent?.trim()).toBe('unhealthy: mount not available');
    expect(ws.classList.contains('tone-bad')).toBe(true);
    expect(rows[0]!.querySelector('.pill')!.classList.contains('tone-ok')).toBe(true);
  });

  it('has no pool sub-block: the pool is per instance, in the Hub instances table', async () => {
    const root = await mount(hub());
    expect(root.querySelector('li[data-check="database"]')).not.toBeNull();
    expect(root.querySelector('[data-role="pool"]')).toBeNull();
    expect(root.textContent).not.toContain('Pool');
  });

  it('shows the hub status and that the figures are from this instance', async () => {
    const root = await mount(hub({ status: 'degraded' }));
    const pill = root.querySelector('[data-role="hub-status"]')!;
    expect(pill.textContent?.trim()).toBe('degraded');
    expect(pill.classList.contains('tone-warn')).toBe(true);
    expect(root.textContent).toContain('this instance');
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

describe('hubCheckRows', () => {
  it('lists the checks sorted by name', () => {
    expect(hubCheckRows(hub()).map((r) => r.name)).toEqual([
      'colocated_broker',
      'database',
      'workspace_storage',
    ]);
  });

  it('is empty without checks', () => {
    expect(hubCheckRows(hub({ checks: undefined }))).toEqual([]);
  });
});
