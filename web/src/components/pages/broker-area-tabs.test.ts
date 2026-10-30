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
 * The Overview and Agents & projects tabs of the Brokers area (broker UI
 * Option A, slice A1): the "Profiles (legacy)" card only on a legacy
 * broker, the capacity read view with its source, and the "Default for
 * project" marker.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { BrokerSettingsResponse, RuntimeBroker } from '../../shared/types.js';
import { FLAT_K8S, LEGACY } from './__fixtures__/broker-area.js';

type El = HTMLElement & { updateComplete: Promise<boolean> } & Record<string, unknown>;

const SETTINGS = {
  brokerId: 'b',
  settings: {},
  effective: {
    maxAgents: { value: 0, source: 'hub_default', count: 1, inherited: { value: 0, source: '' } },
  },
  revision: 0,
  _capabilities: { update: false },
} as BrokerSettingsResponse;

const mounted: HTMLElement[] = [];

async function mount(tag: string, props: Record<string, unknown>): Promise<ShadowRoot> {
  const el = document.createElement(tag) as El;
  Object.assign(el, props);
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

describe('Brokers area tabs', () => {
  beforeAll(async () => {
    await import('./broker-area-overview.js');
    await import('./broker-area-agents.js');
  }, 60_000);

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
  });

  it('Overview: Profiles (legacy) only on a legacy broker', async () => {
    const legacy = await mount('scion-broker-area-overview', {
      broker: LEGACY,
      settings: SETTINGS,
    });
    expect(legacy.querySelector('.profiles-section h2')?.textContent?.trim()).toBe(
      'Profiles (legacy)'
    );
    const flat = await mount('scion-broker-area-overview', {
      broker: { ...FLAT_K8S, profiles: LEGACY.profiles } as RuntimeBroker,
      settings: SETTINGS,
    });
    expect(flat.querySelector('.profiles-section')).toBeNull();
  });

  it('Overview: capacity with its source', async () => {
    const root = await mount('scion-broker-area-overview', { broker: LEGACY, settings: SETTINGS });
    const text = root.querySelector('.capacity-section')?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain('Max concurrent agents: unlimited (from the hub-wide default)');
  });

  it('Agents & projects: the Default for project marker', async () => {
    const root = await mount('scion-broker-area-agents', {
      projects: [
        { projectId: 'p1', projectName: 'One', agentCount: 0 },
        { projectId: 'p2', projectName: 'Two', agentCount: 0 },
      ],
      agents: [],
      defaultFor: new Set(['p2']),
    });
    expect(root.querySelector('[data-project="p1"] .default-marker')).toBeNull();
    expect(root.querySelector('[data-project="p2"] .default-marker')?.textContent?.trim()).toBe(
      'Default for project'
    );
  });
});
