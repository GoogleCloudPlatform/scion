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

import { describe, expect, it } from 'vitest';
import {
  isDNS1123Subdomain,
  isLabelValue,
  isQualifiedName,
  validateK8sRuntimeFields,
} from './k8s-placement.js';

// Mirrors the Go checks the hub runs on save (config.RuntimeSaveChecks).
describe('k8s placement validator', () => {
  it('qualified names', () => {
    for (const ok of ['pool', 'scion.dev/pool', 'a-b_c.d', 'cloud.google.com/gke-nodepool']) {
      expect(isQualifiedName(ok), ok).toBe(true);
    }
    for (const bad of ['', 'bad key!', '/x', 'Example.com/x', 'a/b/c', '-x', 'x'.repeat(64)]) {
      expect(isQualifiedName(bad), bad).toBe(false);
    }
  });
  it('label values and DNS-1123 names', () => {
    expect(isLabelValue('')).toBe(true);
    expect(isLabelValue('agents')).toBe(true);
    expect(isLabelValue('a b')).toBe(false);
    expect(isDNS1123Subdomain('gvisor')).toBe(true);
    expect(isDNS1123Subdomain('a.b-c')).toBe(true);
    expect(isDNS1123Subdomain('Not_Valid')).toBe(false);
    expect(isDNS1123Subdomain('')).toBe(false);
  });
  it('a valid entry has no issues', () => {
    expect(
      validateK8sRuntimeFields('gke', {
        node_selector: { 'scion.dev/pool': 'agents' },
        tolerations: [
          { key: 'scion.dev/pool', operator: 'Equal', value: 'agents', effect: 'NoSchedule' },
          { operator: 'Exists' },
        ],
        runtime_class_name: 'gvisor',
        kubernetes_service_account_mappings: { 'sa@p.iam.gserviceaccount.com': 'agent-ksa' },
        kubernetes_block_service_account: 'block-ksa',
        priority_class_name: 'agents',
      })
    ).toEqual([]);
  });
  it('names each invalid field', () => {
    const paths = validateK8sRuntimeFields('gke', {
      node_selector: { 'bad key!': 'v' },
      tolerations: [{ key: 'k', operator: 'Exists', value: 'x' }, { operator: 'Equal' }],
      runtime_class_name: 'Not_Valid',
      kubernetes_service_account_mappings: { NotAnEmail: 'ksa' },
      kubernetes_block_service_account: 'Bad_Name',
      priority_class_name: 'Bad_Name',
    }).map((i) => i.path);
    expect([...new Set(paths)].sort()).toEqual([
      'runtimes.gke.kubernetes_block_service_account',
      'runtimes.gke.kubernetes_service_account_mappings',
      'runtimes.gke.node_selector',
      'runtimes.gke.priority_class_name',
      'runtimes.gke.runtime_class_name',
      'runtimes.gke.tolerations',
    ]);
  });
  it('toleration rules match the API server', () => {
    const msgs = (t: object) =>
      validateK8sRuntimeFields('k', { tolerations: [t] }).map((i) => i.message);
    expect(msgs({ operator: 'Equal' })[0]).toContain('must be Exists when the key is empty');
    expect(msgs({ key: 'k', operator: 'Exists', value: 'v' })[0]).toContain('must be empty');
    expect(msgs({ key: 'k', operator: 'exists' })[0]).toContain('not supported');
    expect(msgs({ key: 'k', effect: 'Never' })[0]).toContain('effect');
    expect(msgs({ key: 'k', value: 'v' })).toEqual([]);
  });
});
