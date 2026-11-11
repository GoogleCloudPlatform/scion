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
 * The service-account picker's mapping-state helpers (ptone/scion#4391).
 */

import { describe, it, expect } from 'vitest';

import {
  assignStatusLabel,
  assignStatusTarget,
  assignStatusTargetKey,
  orderByAssignStatus,
  projectServiceAccountListUrl,
  withoutAssignStatus,
} from './gcp-sa-assign-status.js';
import type { GCPServiceAccount, GCPServiceAccountAssignStatus } from './types.js';

function account(id: string, assignStatus?: GCPServiceAccountAssignStatus): GCPServiceAccount {
  return {
    id,
    scope: 'project',
    scopeId: 'p1',
    email: `${id}@example.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    createdBy: 'user-1',
    createdAt: '2026-01-01T00:00:00Z',
    ...(assignStatus ? { assignStatus } : {}),
  };
}

describe('assignStatusTarget', () => {
  it('is known only for a Kubernetes target with a broker and a chosen profile', () => {
    expect(assignStatusTarget(true, 'b1', 'k8s')).toEqual({ brokerId: 'b1', profile: 'k8s' });
    expect(assignStatusTarget(false, 'b1', 'docker')).toBeNull();
    expect(assignStatusTarget(true, '', 'k8s')).toBeNull();
    expect(assignStatusTarget(true, 'b1', '')).toBeNull();
  });

  it('keys distinct targets differently and no target as empty', () => {
    expect(assignStatusTargetKey(null)).toBe('');
    expect(assignStatusTargetKey({ brokerId: 'b1', profile: 'a' })).not.toBe(
      assignStatusTargetKey({ brokerId: 'b1', profile: 'b' })
    );
  });
});

describe('projectServiceAccountListUrl', () => {
  it('asks for the plain list with no target, as before', () => {
    expect(projectServiceAccountListUrl('p1', null)).toBe(
      '/api/v1/projects/p1/gcp-service-accounts?includeHubScoped=true'
    );
  });

  it('asks for the mapping state on the target broker and profile', () => {
    const url = new URL(
      projectServiceAccountListUrl('p1', { brokerId: 'broker 1', profile: 'k8s/a' }),
      'http://hub.example.com'
    );
    expect(url.pathname).toBe('/api/v1/projects/p1/gcp-service-accounts');
    expect(url.searchParams.get('includeHubScoped')).toBe('true');
    expect(url.searchParams.get('assignStatus')).toBe('true');
    expect(url.searchParams.get('profile')).toBe('k8s/a');
    expect(url.searchParams.get('broker')).toBe('broker 1');
  });
});

describe('assignStatusLabel', () => {
  it('labels each state, saying "mapped" and never "ready"', () => {
    const mapped = assignStatusLabel({ state: 'mapped', message: 'm' });
    expect(mapped).toBe('mapped');
    expect(mapped).not.toMatch(/ready/i);
    expect(assignStatusLabel({ state: 'not_mapped', message: 'm' })).toBe(
      'not mapped on this profile'
    );
    expect(assignStatusLabel({ state: 'not_required', message: 'm' })).toBe('no mapping needed');
  });

  it('gives unknown a short reason', () => {
    expect(assignStatusLabel({ state: 'unknown', reason: 'report_stale', message: 'm' })).toBe(
      'mapping unknown: report stale'
    );
    expect(assignStatusLabel({ state: 'unknown', reason: 'report_incomplete', message: 'm' })).toBe(
      'mapping unknown: report incomplete'
    );
    expect(
      assignStatusLabel({ state: 'unknown', reason: 'report_old_version', message: 'm' })
    ).toBe('mapping unknown: older broker');
    expect(
      assignStatusLabel({ state: 'unknown', reason: 'runtime_unrecognized', message: 'm' })
    ).toBe('mapping unknown: runtime not recognised');
  });

  it('falls back to a neutral label for a reason or state it does not know', () => {
    expect(assignStatusLabel({ state: 'unknown', reason: 'something_new', message: 'm' })).toBe(
      'mapping unknown'
    );
    expect(assignStatusLabel({ state: 'unknown', message: 'm' })).toBe('mapping unknown');
    expect(assignStatusLabel({ state: 'future_state', message: 'm' })).toBe('mapping unknown');
  });

  it('is empty when the hub sent no mapping state (older hub)', () => {
    expect(assignStatusLabel(undefined)).toBe('');
  });
});

describe('orderByAssignStatus', () => {
  it('moves not-mapped accounts last and keeps everything else in order', () => {
    const list = [
      account('a', { state: 'not_mapped', message: 'm' }),
      account('b', { state: 'mapped', message: 'm' }),
      account('c', { state: 'unknown', reason: 'report_stale', message: 'm' }),
      account('d', { state: 'not_mapped', message: 'm' }),
      account('e'),
    ];
    expect(orderByAssignStatus(list).map((sa) => sa.id)).toEqual(['b', 'c', 'e', 'a', 'd']);
  });

  it('leaves a list without mapping state unchanged', () => {
    const list = [account('a'), account('b')];
    expect(orderByAssignStatus(list).map((sa) => sa.id)).toEqual(['a', 'b']);
  });
});

describe('withoutAssignStatus', () => {
  it('drops the mapping state and keeps the accounts', () => {
    const out = withoutAssignStatus([
      account('a', { state: 'mapped', message: 'm' }),
      account('b'),
    ]);
    expect(out.map((sa) => sa.id)).toEqual(['a', 'b']);
    expect(out.every((sa) => sa.assignStatus === undefined)).toBe(true);
  });
});
