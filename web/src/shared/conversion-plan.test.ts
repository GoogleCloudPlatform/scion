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
  capacityOutcomeLabel,
  findingText,
  profileMapRows,
  profileRefLabel,
  splitGroups,
  type ConversionPlanPresentation,
} from './conversion-plan.js';
import type { RuntimeTargetPresentation } from './types.js';

const docker = (key: string): RuntimeTargetPresentation => ({
  schemaVersion: 1,
  key,
  name: key,
  target: { type: 'docker' },
});

describe('conversion plan helpers', () => {
  it('profileMapRows: mapped profiles plus blocked unmapped ones, sorted', () => {
    const plan: ConversionPlanPresentation = {
      schemaVersion: 1,
      profileMap: { b: 'i-b', a: 'i-a' },
      findings: [
        {
          code: 'unsupported_flat_target_type',
          severity: 'blocking',
          path: 'profiles.c.runtime',
          profiles: ['c'],
        },
        {
          code: 'legacy_field_no_effect',
          severity: 'notice',
          path: 'profiles.d.default_template',
          profiles: ['d'],
        },
      ],
    };
    expect(profileMapRows(plan)).toEqual([
      { profile: 'a', instance: 'i-a' },
      { profile: 'b', instance: 'i-b' },
      { profile: 'c', noMappingCode: 'unsupported_flat_target_type' },
    ]);
  });

  it('profileMapRows: absent fields give no rows', () => {
    expect(profileMapRows({ schemaVersion: 1 })).toEqual([]);
  });

  it('splitGroups: only the split notice at profiles.*, and unresolved groups have no differences', () => {
    const plan: ConversionPlanPresentation = {
      schemaVersion: 1,
      instances: [docker('g-a')],
      profileMap: { a: 'g-a' },
      findings: [
        { code: 'profile_conflict', severity: 'notice', path: 'profiles.*', profiles: ['a', 'b'] },
        {
          code: 'profile_conflict',
          severity: 'notice',
          path: 'profiles.a.node_selector',
          profiles: ['a'],
        },
      ],
    };
    const groups = splitGroups(plan);
    expect(groups).toHaveLength(1);
    expect(groups[0].instances).toEqual(['g-a']);
    expect(groups[0].differences).toEqual([]);
  });

  it('fixed texts: every frozen code has one, an unknown code gets the CLI pointer', () => {
    for (const code of [
      'unsupported_flat_target_type',
      'flat_target_scope_violation',
      'profile_conflict',
      'profile_conflict_resolved',
      'legacy_field_no_effect',
      'legacy_field_removed',
      'unknown_settings_key',
      'field_not_valid_for_target_type',
      'plan_truncated',
    ]) {
      expect(findingText(code)).not.toContain('scion runtime-broker convert --plan');
    }
    expect(findingText('something_new')).toContain('scion runtime-broker convert --plan');
  });

  it('labels', () => {
    expect(capacityOutcomeLabel('no_limit')).toBe('No limit to split');
    expect(profileRefLabel({ profile: 'a', mapping: 'mapped', instance: 'i' })).toBe('i');
    expect(profileRefLabel({ profile: 'a', mapping: 'no_mapping' })).toBe('no mapping');
    expect(profileRefLabel({ mapping: 'profile_unknown' })).toBe('profile unknown');
    expect(profileRefLabel(undefined)).toBe('—');
  });
});
