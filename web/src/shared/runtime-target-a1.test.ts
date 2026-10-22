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
 * V6 (part) and V5 (pure part): the broker UI A1 additions to
 * runtime-target.ts: flat classification, the one-line target label, and
 * the Kubernetes placement rows with their toleration text.
 */

import { describe, it, expect } from 'vitest';

import {
  isFlatBroker,
  placementRows,
  runtimeTargetLabel,
  tolerationLabel,
} from './runtime-target.js';
import type { PresentedToleration, RuntimeTargetPresentation } from './types.js';
import { fullPresentation } from './__fixtures__/runtime-target-presentation.js';

type Tol = PresentedToleration;

const k8s = { id: 'rt-1', type: 'kubernetes', displayName: 'Cluster A' };
const docker = { id: 'rt-2', type: 'docker' };

function withPlacement(): RuntimeTargetPresentation {
  const p = fullPresentation();
  p.target = {
    ...p.target!,
    nodeSelector: { 'cloud.google.com/gke-nodepool': 'agents', 'disk/type': 'ssd' },
    tolerations: [{ key: 'dedicated', operator: 'Equal', value: 'agents', effect: 'NoSchedule' }],
    runtimeClassName: 'gvisor',
  };
  return p;
}

describe('isFlatBroker and runtimeTargetLabel', () => {
  it('classifies from the runtime target only', () => {
    expect(isFlatBroker({ runtimeTarget: k8s })).toBe(true);
    expect(isFlatBroker({})).toBe(false);
  });

  it('labels a flat target, with the namespace only when reported', () => {
    expect(runtimeTargetLabel({ runtimeTarget: k8s })).toBe('kubernetes · Cluster A');
    expect(
      runtimeTargetLabel({ runtimeTarget: k8s, runtimeTargetPresentation: fullPresentation() })
    ).toBe('kubernetes · Cluster A · ns agents');
    expect(runtimeTargetLabel({ runtimeTarget: docker })).toBe('docker');
    expect(runtimeTargetLabel({})).toBe('');
  });
});

describe('tolerationLabel', () => {
  it('renders the four named fields', () => {
    const equal = { key: 'dedicated', operator: 'Equal', value: 'agents', effect: 'NoSchedule' };
    expect(tolerationLabel(equal)).toBe('dedicated=agents:NoSchedule');
    expect(tolerationLabel({ key: 'gpu', operator: 'Exists' })).toBe('gpu Exists (any effect)');
    expect(tolerationLabel({ key: 'gpu', operator: 'Exists', effect: 'NoExecute' })).toBe(
      'gpu Exists:NoExecute'
    );
    expect(tolerationLabel({ operator: 'Exists' })).toBe('all taints (Exists)');
    expect(tolerationLabel({ key: 'a', value: 'b' })).toBe('a=b (any effect)');
    expect(tolerationLabel({})).toBe('= (any effect)');
    expect(tolerationLabel({})).not.toContain('undefined');
  });

  it('never renders a field it does not name', () => {
    const t = {
      key: 'dedicated',
      operator: 'Equal',
      value: 'agents',
      effect: 'NoSchedule',
      tolerationSeconds: 30,
      secretish: 'NEVER-RENDER',
    } as PresentedToleration;
    const text = tolerationLabel(t);
    expect(text).toBe('dedicated=agents:NoSchedule');
    expect(text).not.toContain('NEVER');
    expect(text).not.toContain('30');
  });
});

describe('placementRows', () => {
  it('lists node selector, tolerations and runtime class for Kubernetes', () => {
    expect(placementRows(k8s, withPlacement())).toEqual([
      { label: 'Node selector', value: 'cloud.google.com/gke-nodepool=agents, disk/type=ssd' },
      { label: 'Tolerations', value: 'dedicated=agents:NoSchedule' },
      { label: 'Runtime class', value: 'gvisor' },
    ]);
  });

  it('skips a non-object toleration entry and a non-object node selector', () => {
    const p = fullPresentation();
    p.target = {
      ...p.target!,
      nodeSelector: 'ab' as unknown as Record<string, string>,
      tolerations: [null, 'x', { key: 'gpu', operator: 'Exists' }] as unknown as Tol[],
    };
    expect(placementRows(k8s, p)).toEqual([
      { label: 'Tolerations', value: 'gpu Exists (any effect)' },
    ]);
  });

  it('is empty for docker, for an unknown type, and when nothing is reported', () => {
    expect(placementRows(docker, withPlacement())).toEqual([]);
    expect(placementRows({ id: 'x', type: 'cloudrun' }, withPlacement())).toEqual([]);
    expect(placementRows(k8s, fullPresentation())).toEqual([]);
    expect(placementRows(k8s, undefined)).toEqual([]);
  });
});
