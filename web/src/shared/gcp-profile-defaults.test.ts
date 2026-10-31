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
  effectiveIdentityForProfile,
  knownProfiles,
  mapFromRows,
  profileErrorsFromBody,
  profileOptionsForRow,
  profilesMissingDefault,
  rowProblem,
  rowsFromMap,
  sameProfileMap,
  splitProfileWarnings,
  type ProfileDefaultRow,
} from './gcp-profile-defaults.js';

const F = 'defaultGCPIdentityServiceAccountIDByProfile';

function counter(): () => number {
  let n = 0;
  return () => ++n;
}

function row(key: number, profile: string, serviceAccountId: string): ProfileDefaultRow {
  return { key, profile, serviceAccountId };
}

describe('rows and maps', () => {
  it('builds sorted rows from a map and maps complete rows back', () => {
    const rows = rowsFromMap({ b: 'sa-2', a: 'sa-1' }, counter());
    expect(rows.map((r) => r.profile)).toEqual(['a', 'b']);
    expect(mapFromRows([...rows, row(9, 'c', '')])).toEqual({ a: 'sa-1', b: 'sa-2' });
    expect(rowsFromMap(undefined, counter())).toEqual([]);
  });

  it('compares maps by entries', () => {
    expect(sameProfileMap({ a: '1' }, { a: '1' })).toBe(true);
    expect(sameProfileMap({ a: '1' }, { a: '2' })).toBe(false);
    expect(sameProfileMap({}, undefined)).toBe(true);
    expect(sameProfileMap({ a: '1' }, {})).toBe(false);
  });

  it('reports incomplete and duplicate rows', () => {
    const a = row(1, 'a', 'sa-1');
    const dup = row(2, 'a', 'sa-2');
    expect(rowProblem(a, [a])).toBe('');
    expect(rowProblem(row(3, '', ''), [])).toMatch(/profile and a service account/);
    expect(rowProblem(row(3, 'x', ''), [])).toMatch(/service account/);
    expect(rowProblem(row(3, '', 'sa'), [])).toMatch(/Choose a profile/);
    expect(rowProblem(a, [a, dup])).toMatch(/more than one row/);
  });
});

describe('profiles', () => {
  const brokers = [
    {
      profiles: [
        { name: 'k8s-a', type: 'kubernetes', available: true },
        { name: 'local', type: 'docker', available: true },
      ],
    },
    { profiles: [{ name: 'k8s-b', type: 'Kubernetes', available: false }] },
    { profiles: [{ name: 'local', type: 'podman', available: true }] },
    {},
  ];

  it('unions profile names across brokers', () => {
    expect(knownProfiles(brokers)).toEqual([
      { name: 'k8s-a', types: ['kubernetes'], kubernetes: true },
      { name: 'k8s-b', types: ['kubernetes'], kubernetes: true },
      { name: 'local', types: ['docker', 'podman'], kubernetes: false },
    ]);
  });

  it('offers profiles no other row uses, plus the row own unknown profile', () => {
    const known = knownProfiles(brokers);
    const a = row(1, 'k8s-a', 'sa');
    const stale = row(2, 'gone', 'sa');
    expect(profileOptionsForRow(stale, [a, stale], known)).toEqual(['gone', 'k8s-b', 'local']);
    expect(profileOptionsForRow(a, [a, stale], known)).toEqual(['k8s-a', 'k8s-b', 'local']);
  });

  it('flags Kubernetes profiles without a default only when a sibling has one', () => {
    const known = knownProfiles(brokers);
    expect(profilesMissingDefault([], known)).toEqual([]);
    expect(profilesMissingDefault([row(1, 'k8s-a', 'sa')], known)).toEqual(['k8s-b']);
    expect(profilesMissingDefault([row(1, 'local', 'sa')], known)).toEqual(['k8s-a', 'k8s-b']);
  });
});

describe('save response messages', () => {
  it('splits per-profile warnings from the rest', () => {
    const out = splitProfileWarnings([
      `${F}[k8s-a]: GCP service account x is not mapped`,
      'something else',
      7,
    ]);
    expect(out.byProfile).toEqual({
      'k8s-a': [`${F}[k8s-a]: GCP service account x is not mapped`],
    });
    expect(out.general).toEqual(['something else']);
    expect(splitProfileWarnings(undefined)).toEqual({ byProfile: {}, general: [] });
  });

  it('reads details.errors entries first', () => {
    const out = profileErrorsFromBody({
      error: {
        message: 'validation failed',
        details: {
          field: `${F}.k8s-a`,
          errors: [
            { field: `${F}.k8s-a`, message: 'bad a' },
            { field: F, message: 'too many' },
            { field: 'defaultModel', message: 'other field' },
          ],
        },
      },
    });
    expect(out).toEqual({ byProfile: { 'k8s-a': ['bad a'] }, general: ['too many'] });
  });

  it('falls back to details.field, then to the message', () => {
    expect(
      profileErrorsFromBody({ error: { message: 'm', details: { field: `${F}.p1` } } })
    ).toEqual({ byProfile: { p1: ['m'] }, general: [] });

    const prefixed = `${F}[p2]: a service account ID is required; remove the entry to clear it`;
    expect(
      profileErrorsFromBody({ error: { code: 'invalid_request', message: prefixed } })
    ).toEqual({ byProfile: { p2: [prefixed] }, general: [] });

    const whole = `${F}: at most 64 entries are allowed`;
    expect(profileErrorsFromBody({ error: { message: whole } })).toEqual({
      byProfile: {},
      general: [whole],
    });

    const phrase =
      'GCP service account is not verified; verify it before setting it as the default for profile "p3". Verify it.';
    expect(profileErrorsFromBody({ error: { message: phrase } }).byProfile).toEqual({
      p3: [phrase],
    });
  });

  it('does not take a longer field name sharing the prefix for the map', () => {
    expect(profileErrorsFromBody({ error: { message: `${F}Extra: bad` } })).toEqual({
      byProfile: {},
      general: [],
    });
  });

  it('ignores errors about other fields and non-JSON shapes', () => {
    expect(profileErrorsFromBody({ error: { message: 'default GCP identity mode bad' } })).toEqual({
      byProfile: {},
      general: [],
    });
    expect(profileErrorsFromBody(null)).toEqual({ byProfile: {}, general: [] });
    expect(profileErrorsFromBody({ error: `${F}[p]: flat` }).byProfile).toEqual({
      p: [`${F}[p]: flat`],
    });
  });
});

describe('effectiveIdentityForProfile', () => {
  const none = { mode: '', serviceAccountId: '' };

  it('follows profile, project, hub, none', () => {
    const map = { a: 'sa-a' };
    const project = { mode: 'block', serviceAccountId: '' };
    const hub = { mode: 'assign', serviceAccountId: 'sa-hub' };
    expect(effectiveIdentityForProfile('a', map, project, hub)).toEqual({
      source: 'profile',
      mode: 'assign',
      serviceAccountId: 'sa-a',
    });
    expect(effectiveIdentityForProfile('b', map, project, hub)).toEqual({
      source: 'project',
      mode: 'block',
      serviceAccountId: '',
    });
    expect(effectiveIdentityForProfile('b', map, none, hub)).toEqual({
      source: 'hub',
      mode: 'assign',
      serviceAccountId: 'sa-hub',
    });
    expect(effectiveIdentityForProfile('b', {}, none, none).source).toBe('none');
  });

  it('treats an assign tier with no account as block', () => {
    expect(
      effectiveIdentityForProfile(
        'b',
        {},
        { mode: 'assign', serviceAccountId: '' },
        { mode: 'passthrough', serviceAccountId: '' }
      )
    ).toEqual({ source: 'project', mode: 'block', serviceAccountId: '' });
  });
});
