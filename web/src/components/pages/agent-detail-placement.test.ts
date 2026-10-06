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
 * Placement card on the agent detail page (ptone/scion#3269): a pinned
 * (flat Runtime Broker) agent shows its stored Runtime Broker and runtime
 * target, with connection status and failed runtime operations as separate
 * indicators, and no Runtime Broker Profile; an unpinned agent keeps the
 * existing rendering.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { Agent, RuntimeBroker } from '../../shared/types.js';

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

function makeAgent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    runtimeBrokerId: 'b-flat',
    runtimeBrokerName: 'flat-docker',
    appliedConfig: { profile: 'local' },
    ...overrides,
  } as Agent;
}

const flatBroker = {
  id: 'b-flat',
  name: 'flat-docker',
  slug: 'flat-docker',
  version: '',
  status: 'offline',
  connectionState: 'disconnected',
  lastHeartbeat: '',
  autoProvide: false,
  runtimeTarget: { id: 't-1', type: 'docker', displayName: 'Local Docker' },
} as RuntimeBroker;

type Internals = {
  placementBroker: RuntimeBroker | null;
  renderPlacementCard(a: Agent): TemplateResult | typeof nothing;
  renderRuntimeCard(a: Agent, inline: undefined): TemplateResult;
};

function host(tpl: TemplateResult | typeof nothing): HTMLElement {
  const div = document.createElement('div');
  render(tpl, div);
  return div;
}

function page(broker: RuntimeBroker | null): Internals {
  const el = document.createElement('scion-page-agent-detail');
  const internals = el as unknown as Internals;
  internals.placementBroker = broker;
  return internals;
}

describe('agent detail Placement card', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('shows the stored placement with separate connection and runtime-operation indicators', () => {
    const agent = makeAgent({
      phase: 'error',
      message: 'start refused by the Runtime Broker',
      pinnedRuntimeTarget: { id: 't-1', type: 'docker', runtimeBrokerId: 'b-flat' },
    });
    const el = host(page(flatBroker).renderPlacementCard(agent));
    expect(el.querySelector('.placement-card')).not.toBeNull();
    expect(el.querySelector('.placement-target')?.textContent?.trim()).toBe(
      'Local Docker (docker)'
    );
    expect(el.querySelector('.placement-target-id')?.textContent?.trim()).toBe('t-1');
    expect(el.querySelector('a.broker-link')?.getAttribute('href')).toBe('/brokers/b-flat');
    const conn = el.querySelector('.placement-connection scion-status-badge');
    const op = el.querySelector('.placement-runtime-op scion-status-badge');
    expect(conn?.getAttribute('label')).toBe('offline (disconnected)');
    expect(conn?.getAttribute('status')).toBe('danger');
    expect(op?.getAttribute('label')).toBe('failed');
    expect(el.querySelector('.placement-runtime-op-detail')?.textContent).toBe(
      'start refused by the Runtime Broker'
    );
    expect(el.querySelector('.placement-stale')).toBeNull();
  });

  it('shows a refused start on a stopped agent with its recorded message', () => {
    const agent = makeAgent({
      phase: 'stopped',
      message: 'refused by the Runtime Broker',
      pinnedRuntimeTarget: { id: 't-1', type: 'docker', runtimeBrokerId: 'b-flat' },
    });
    const el = host(page(flatBroker).renderPlacementCard(agent));
    const op = el.querySelector('.placement-runtime-op scion-status-badge');
    expect(op?.getAttribute('label')).toBe('see message');
    expect(op?.getAttribute('status')).toBe('warning');
    expect(el.querySelector('.placement-runtime-op-detail')?.textContent).toBe(
      'refused by the Runtime Broker'
    );
  });

  it('renders nothing for an unpinned agent', () => {
    const el = host(page(null).renderPlacementCard(makeAgent({})));
    expect(el.querySelector('.placement-card')).toBeNull();
  });

  it('hides the Runtime Broker Profile for a pinned agent and keeps it for a legacy agent', () => {
    const labels = (a: Agent): (string | undefined)[] =>
      Array.from(
        host(page(null).renderRuntimeCard(a, undefined)).querySelectorAll('.info-label')
      ).map((l) => l.textContent?.trim());
    expect(labels(makeAgent({}))).toContain('Profile');
    expect(
      labels(
        makeAgent({ pinnedRuntimeTarget: { id: 't-1', type: 'docker', runtimeBrokerId: 'b-flat' } })
      )
    ).not.toContain('Profile');
  });

  it('marks a stale pin', () => {
    const agent = makeAgent({
      runtimeBrokerId: 'b-legacy',
      pinnedRuntimeTarget: { id: 't-1', type: 'docker', runtimeBrokerId: 'b-flat' },
    });
    const el = host(page(null).renderPlacementCard(agent));
    expect(el.querySelector('.placement-stale')).not.toBeNull();
  });
});
