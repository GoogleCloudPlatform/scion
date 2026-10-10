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

import { describe, it, expect, afterEach, vi } from 'vitest';

import {
  fleetHealthyText,
  hubFailingChecks,
  hubInstanceAnchor,
  HUB_INSTANCE_TARGET_EVENT,
  type HealthSummaryHub,
} from './health-hub-card.js';
import {
  hasInPageState,
  isInPagePop,
  IN_PAGE_STATE_KEY,
  type RouteShell,
} from '../../client/route-history.js';
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
    vi.restoreAllMocks();
    history.replaceState(null, '', '/');
  });

  function failingHub(): HealthSummaryHub {
    return hub({
      status: 'degraded',
      instances: { live: 2, healthy: 1, degraded: 1, unhealthy: 0 },
      unhealthy_checks: [
        {
          instance_id: 'hub-b-1',
          instance_label: 'hub-b',
          name: 'audit_log_writer',
          value: 'degraded',
        },
      ],
    });
  }

  it('moves to the instance row within the page on a link click, as in-page history', async () => {
    history.replaceState({ from: 'router' }, '', '/health');
    const replace = vi.spyOn(history, 'replaceState');
    const targets: string[] = [];
    const onTarget = (e: Event) => targets.push((e as CustomEvent<string>).detail);
    window.addEventListener(HUB_INSTANCE_TARGET_EVENT, onTarget);
    try {
      const root = await mount(failingHub());
      const link = root.querySelector('[data-role="failing-checks"] a')!;
      const click = new MouseEvent('click', {
        bubbles: true,
        composed: true,
        cancelable: true,
        button: 0,
      });
      link.dispatchEvent(click);

      // No fragment navigation: the router would render the route again.
      expect(click.defaultPrevented).toBe(true);
      expect(window.location.pathname).toBe('/health');
      expect(window.location.hash).toBe('#' + hubInstanceAnchor('hub-b-1'));
      // The new entry and the one it came from are in-page entries, so the
      // router leaves Back and Forward to the page.
      expect(history.state).toEqual({ [IN_PAGE_STATE_KEY]: { hubInstance: 'hub-b-1' } });
      const shell = { currentPath: '/health' } as RouteShell;
      expect(isInPagePop(history.state, '/health', shell, false)).toBe(true);
      expect(replace).toHaveBeenCalledTimes(1);
      const marked = replace.mock.calls[0]![0];
      expect(hasInPageState(marked)).toBe(true);
      expect(marked).toMatchObject({ from: 'router' });
      expect(targets).toEqual(['hub-b-1']);
    } finally {
      window.removeEventListener(HUB_INSTANCE_TARGET_EVENT, onTarget);
    }
  });

  it('leaves a modified click to the browser', async () => {
    history.replaceState(null, '', '/health');
    const push = vi.spyOn(history, 'pushState');
    const root = await mount(failingHub());
    const link = root.querySelector('[data-role="failing-checks"] a')!;
    const click = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
      ctrlKey: true,
    });
    link.addEventListener('click', (e) => e.preventDefault(), { once: true });
    link.dispatchEvent(click);
    expect(push).not.toHaveBeenCalled();
  });

  it('shows no fleet line or failing checks for an older replica during a rolling upgrade', async () => {
    const older = {
      ...hub(),
      instances: undefined,
      unhealthy_checks: ['colocated_broker: unhealthy: registration failed'],
    } as unknown as HealthSummaryHub;
    const root = await mount(older);
    expect(root.querySelector('[data-role="fleet"]')).toBeNull();
    expect(root.querySelector('[data-role="failing-checks"]')).toBeNull();
    expect(root.textContent).not.toContain('undefined');
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
      instances: ['hub-a', 'hub-b'],
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
    expect(block.querySelector('[data-role="sa-check-instances"]')?.textContent?.trim()).toBe(
      'On hub-a, hub-b'
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

describe('hubFailingChecks', () => {
  it('keeps only object entries', () => {
    const check = { instance_id: 'a', instance_label: 'a', name: 'database', value: 'unhealthy' };
    const mixed = { ...hub(), unhealthy_checks: ['database: unhealthy', null, check] };
    expect(hubFailingChecks(mixed as unknown as HealthSummaryHub)).toEqual([check]);
    expect(hubFailingChecks(hub({ unhealthy_checks: undefined }))).toEqual([]);
  });
});

describe('hubInstanceAnchor', () => {
  it('encodes the instance ID', () => {
    expect(hubInstanceAnchor('pod-1 x/y')).toBe('hub-instance-pod-1%20x%2Fy');
  });
});
