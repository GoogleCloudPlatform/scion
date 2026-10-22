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
 * V5: the Brokers area runtime target card: P4.1-a's rows (tri-state
 * safe_to_evict, no withheld field in the DOM) plus the Kubernetes
 * Placement group, which renders only the named fields.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { RuntimeBroker, RuntimeTargetPresentation } from '../../shared/types.js';
import {
  fullPresentation,
  presentationWithNeverFields,
} from '../../shared/__fixtures__/runtime-target-presentation.js';

type Card = HTMLElement & { broker: RuntimeBroker | null; updateComplete: Promise<boolean> };

function flatBroker(type: string, presentation?: RuntimeTargetPresentation): RuntimeBroker {
  return {
    id: 'b-1',
    name: 'flat',
    slug: 'flat',
    version: '1',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-10-14T12:00:00Z',
    autoProvide: false,
    runtimeTarget: { id: 'rt-1234', type, displayName: 'Cluster A' },
    runtimeTargetPresentation: presentation,
    runtimeTargetPresentationObservedAt: presentation ? '2026-10-14T12:00:00Z' : undefined,
  } as RuntimeBroker;
}

function withPlacement(extra: Record<string, unknown> = {}): RuntimeTargetPresentation {
  const p = fullPresentation();
  p.target = {
    ...p.target!,
    nodeSelector: { 'cloud.google.com/gke-nodepool': 'agents' },
    tolerations: [
      { key: 'dedicated', operator: 'Equal', value: 'agents', effect: 'NoSchedule', ...extra },
      { operator: 'Exists' },
      { key: 'gpu', operator: 'Exists' },
    ],
    runtimeClassName: 'gvisor',
  };
  return p;
}

const mounted: HTMLElement[] = [];

async function mount(broker: RuntimeBroker): Promise<ShadowRoot> {
  const el = document.createElement('scion-broker-runtime-target-card') as Card;
  el.broker = broker;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

function rows(root: ShadowRoot, cls: string): Record<string, string> | null {
  const dl = root.querySelector(`dl.${cls}`);
  if (!dl) return null;
  const out: Record<string, string> = {};
  const dts = Array.from(dl.querySelectorAll('dt'));
  const dds = Array.from(dl.querySelectorAll('dd'));
  dts.forEach((dt, i) => {
    out[dt.textContent!.trim()] = dds[i].textContent!.trim();
  });
  return out;
}

describe('scion-broker-runtime-target-card', () => {
  beforeAll(async () => {
    await import('./broker-runtime-target-card.js');
  }, 30_000);

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
  });

  it('shows target, reported defaults and caption, read-only', async () => {
    const root = await mount(flatBroker('kubernetes', fullPresentation()));
    expect(rows(root, 'target-rows')).toEqual({
      ID: 'rt-1234',
      Type: 'kubernetes',
      'Display name': 'Cluster A',
      Namespace: 'agents',
    });
    expect(rows(root, 'presentation-rows')!['Safe to evict']).toBe('false');
    expect(root.querySelector('.presentation-caption')?.textContent).toContain(
      'Reported by the instance at'
    );
    expect(root.querySelector('sl-input, sl-button, input, button, form')).toBeNull();
  });

  it('keeps safe_to_evict tri-state and never renders a withheld field', async () => {
    const p = fullPresentation();
    delete p.target!.safeToEvict;
    const unset = rows(await mount(flatBroker('kubernetes', p)), 'presentation-rows');
    expect(unset!['Safe to evict']).toBe('unset');
    const root = await mount(flatBroker('kubernetes', presentationWithNeverFields()));
    expect(root.innerHTML).not.toContain('NEVER');
  });

  it('shows the Placement group for Kubernetes with reported fields', async () => {
    const root = await mount(flatBroker('kubernetes', withPlacement()));
    expect(rows(root, 'placement-rows')).toEqual({
      'Node selector': 'cloud.google.com/gke-nodepool=agents',
      Tolerations: 'dedicated=agents:NoSchedule; all taints (Exists); gpu Exists (any effect)',
      'Runtime class': 'gvisor',
    });
    expect(root.innerHTML).not.toContain('undefined');
  });

  it('never renders an unnamed toleration field', async () => {
    const root = await mount(
      flatBroker('kubernetes', withPlacement({ tolerationSeconds: 30, secretish: 'NEVER-RENDER' }))
    );
    expect(root.innerHTML).not.toContain('NEVER-RENDER');
    expect(root.innerHTML).not.toContain('secretish');
    expect(root.innerHTML).not.toContain('tolerationSeconds');
  });

  it('has no Placement group for docker, an unknown type, or missing fields', async () => {
    expect(rows(await mount(flatBroker('docker', withPlacement())), 'placement-rows')).toBeNull();
    expect(rows(await mount(flatBroker('cloudrun', withPlacement())), 'placement-rows')).toBeNull();
    const none = await mount(flatBroker('kubernetes', fullPresentation()));
    expect(rows(none, 'placement-rows')).toBeNull();
  });

  it('renders nothing for a legacy broker', async () => {
    const legacy = { ...flatBroker('kubernetes', fullPresentation()), runtimeTarget: undefined };
    const root = await mount(legacy);
    expect(root.querySelector('.runtime-target-card')).toBeNull();
  });
});
