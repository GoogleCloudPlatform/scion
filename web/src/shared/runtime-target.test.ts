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

import { describe, it, expect } from 'vitest';

import {
  presentationRows,
  runtimeTargetRows,
  safeToEvictLabel,
  type DisplayRow,
} from './runtime-target.js';
import {
  fullPresentation,
  presentationWithNeverFields,
} from './__fixtures__/runtime-target-presentation.js';

const asMap = (rows: DisplayRow[]) => Object.fromEntries(rows.map((r) => [r.label, r.value]));

describe('runtime target rows', () => {
  it('shows the stored target and the reported namespace', () => {
    const rows = runtimeTargetRows(
      { id: 'rt-1', type: 'kubernetes', displayName: 'Cluster A' },
      fullPresentation()
    );
    expect(rows).toEqual([
      { label: 'ID', value: 'rt-1' },
      { label: 'Type', value: 'kubernetes' },
      { label: 'Display name', value: 'Cluster A' },
      { label: 'Namespace', value: 'agents' },
    ]);
    expect(runtimeTargetRows({ id: 'rt-2', type: 'docker' })).toEqual([
      { label: 'ID', value: 'rt-2' },
      { label: 'Type', value: 'docker' },
    ]);
  });

  it('renders every published field, env by key only', () => {
    expect(asMap(presentationRows(fullPresentation()))).toEqual({
      'Safe to evict': 'false',
      GKE: 'true',
      'Priority class': 'agents-high',
      'Shared dir storage class': 'standard-rwx',
      'Shared dir size': '40Gi',
      'Shared dir storage backend': 'nfs',
      'Shared dir backends': 'cache=nfs, data=local',
      'Home storage backend': 'nfs',
      'Default harness config': 'claude',
      'Image registry': 'registry.example/team',
      'Clone depth': 'full',
      Resources: 'requests cpu=1 memory=2Gi; limits cpu=2 memory=4Gi; disk 20Gi',
      Volumes: '/mnt/a (nfs, read-only)',
      Secrets: 'API_KEY (file): the API key',
      'Harness override: claude':
        'image img:1 · pull Never · auth vertex-ai · user agent · env keys EMPTY, TOKEN · ' +
        'volumes /mnt/b (nfs, read-only) · resources limits memory=8Gi',
      'Harness override: gemini': 'env keys OTHER',
    });
  });

  it('keeps safe_to_evict tri-state: unset, true and false', () => {
    expect(safeToEvictLabel(undefined)).toBe('unset');
    expect(safeToEvictLabel(null)).toBe('unset');
    expect(safeToEvictLabel(true)).toBe('true');
    expect(safeToEvictLabel(false)).toBe('false');
    for (const [value, want] of [
      [undefined, 'unset'],
      [null, 'unset'],
      [true, 'true'],
      [false, 'false'],
    ] as const) {
      const p = fullPresentation();
      if (value === undefined) delete p.target!.safeToEvict;
      else p.target!.safeToEvict = value;
      expect(asMap(presentationRows(p))['Safe to evict']).toBe(want);
    }
  });

  it('never renders a field FC4-a withholds, even when the payload carries it', () => {
    const rows = presentationRows(presentationWithNeverFields());
    const text = JSON.stringify(rows);
    expect(text).not.toContain('NEVER');
    // The withheld values really are in the payload.
    expect(JSON.stringify(presentationWithNeverFields()).match(/NEVER-/g)).toHaveLength(15);
    expect(asMap(rows)).toEqual(asMap(presentationRows(fullPresentation())));
  });
});
