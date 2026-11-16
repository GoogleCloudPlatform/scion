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
 * Pure helpers for the Project Settings editor of the per-broker default
 * GCP service account map (`defaultGCPIdentityServiceAccountIDByBroker`,
 * ptone/scion#8185 / ptone/scion#4895).
 */

import { describe, it, expect } from 'vitest';
import type { GCPServiceAccount, RuntimeBroker } from './types.js';
import {
  BROKER_DEFAULTS_FIELD,
  MAX_BROKER_DEFAULTS,
  brokerCollectionProblem,
  brokerErrorsFromBody,
  brokerMapFromRows,
  brokerOptionsForRow,
  brokerRowProblem,
  brokerRowsFromMap,
  brokersMissingDefault,
  effectiveIdentityForBroker,
  isBrokerDefaultMessage,
  knownFlatBrokers,
  sameBrokerMap,
  serviceAccountOptionsForBroker,
  splitBrokerWarnings,
} from './gcp-broker-defaults.js';

const F = BROKER_DEFAULTS_FIELD;

function account(id: string, verified = true): GCPServiceAccount {
  return {
    id,
    scope: 'project',
    scopeId: 'proj-1',
    email: `${id}@example.iam.gserviceaccount.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified,
    createdBy: 'u1',
    createdAt: '2026-01-01T00:00:00Z',
  };
}

function flatBroker(
  id: string,
  name: string,
  type: string,
  extraTarget: Record<string, unknown> = {}
): RuntimeBroker {
  return {
    id,
    name,
    slug: name,
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    runtimeTarget: { type, ...extraTarget },
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  };
}

function legacyBroker(id: string, name: string): RuntimeBroker {
  return {
    id,
    name,
    slug: name,
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    profiles: [{ name: 'k8s-a', type: 'kubernetes', available: true }],
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  };
}

describe('rows and maps', () => {
  it('round-trips a stored map through sorted rows and compares maps', () => {
    let k = 0;
    const rows = brokerRowsFromMap({ 'broker-b': 'sa-2', 'broker-a': 'sa-1' }, () => ++k);
    expect(rows).toEqual([
      { key: 1, brokerId: 'broker-a', serviceAccountId: 'sa-1' },
      { key: 2, brokerId: 'broker-b', serviceAccountId: 'sa-2' },
    ]);
    expect(brokerMapFromRows(rows)).toEqual({ 'broker-a': 'sa-1', 'broker-b': 'sa-2' });
    expect(sameBrokerMap({ a: '1', b: '2' }, { b: '2', a: '1' })).toBe(true);
    expect(sameBrokerMap({ a: '1' }, { a: '2' })).toBe(false);
    expect(sameBrokerMap({}, null)).toBe(true);
  });

  it('leaves incomplete rows out of brokerMapFromRows and reports their problem', () => {
    const empty = { key: 1, brokerId: '', serviceAccountId: '' };
    const noAccount = { key: 2, brokerId: 'broker-a', serviceAccountId: '' };
    const noBroker = { key: 3, brokerId: '', serviceAccountId: 'sa-1' };
    const rows = [empty, noAccount, noBroker];
    expect(brokerMapFromRows(rows)).toEqual({});
    expect(brokerRowProblem(empty, rows)).toMatch(/Choose a Runtime Broker and a service account/);
    expect(brokerRowProblem(noAccount, rows)).toMatch(/Choose a service account/);
    expect(brokerRowProblem(noBroker, rows)).toMatch(/Choose a Runtime Broker/);
  });

  it('flags duplicate Runtime Broker rows', () => {
    const a = { key: 1, brokerId: 'broker-a', serviceAccountId: 'sa-1' };
    const b = { key: 2, brokerId: 'broker-a', serviceAccountId: 'sa-2' };
    expect(brokerRowProblem(a, [a, b])).toContain('"broker-a"');
  });

  it('enforces the 64-entry count limit', () => {
    const within = Array.from({ length: MAX_BROKER_DEFAULTS }, (_, i) => ({
      key: i + 1,
      brokerId: `b-${i}`,
      serviceAccountId: 'sa-1',
    }));
    expect(brokerCollectionProblem(within)).toBe('');
    const over = [
      ...within,
      { key: MAX_BROKER_DEFAULTS + 1, brokerId: 'b-extra', serviceAccountId: 'sa-1' },
    ];
    expect(brokerCollectionProblem(over)).toContain('64');
  });
});

describe('knownFlatBrokers, options and missing defaults', () => {
  it('collects only flat Runtime Brokers, excluding legacy profile-based brokers', () => {
    const known = knownFlatBrokers([
      legacyBroker('leg-1', 'Legacy Broker'),
      flatBroker('flat-cr', 'Cloud Run US', 'cloudrun', { region: 'us-central1' }),
      flatBroker('flat-k8s', 'Alpha GKE', 'kubernetes', { namespace: 'agents' }),
      flatBroker('flat-docker', 'Docker Host', 'docker'),
    ]);
    expect(known.map((b) => b.id)).toEqual(['flat-k8s', 'flat-cr', 'flat-docker']);
    expect(known.find((b) => b.id === 'flat-k8s')?.kubernetes).toBe(true);
    expect(known.find((b) => b.id === 'flat-cr')?.kubernetes).toBe(false);
    expect(known.find((b) => b.id === 'flat-cr')?.targetLabel).toContain('Cloud Run');
  });

  it('prevents duplicate broker selection and keeps stale/deleted stored broker IDs visible for removal', () => {
    const known = knownFlatBrokers([
      flatBroker('flat-a', 'Broker A', 'docker'),
      flatBroker('flat-b', 'Broker B', 'cloudrun'),
    ]);
    const r1 = { key: 1, brokerId: 'flat-a', serviceAccountId: 'sa-1' };
    const r2 = { key: 2, brokerId: 'deleted-broker-id', serviceAccountId: 'sa-2' };
    expect(brokerOptionsForRow(r1, [r1, r2], known)).toEqual([
      expect.objectContaining({ id: 'flat-a', name: 'Broker A', stale: false }),
      expect.objectContaining({ id: 'flat-b', name: 'Broker B', stale: false }),
    ]);
    expect(brokerOptionsForRow(r2, [r1, r2], known)).toEqual([
      expect.objectContaining({ id: 'flat-b', name: 'Broker B', stale: false }),
      expect.objectContaining({ id: 'deleted-broker-id', name: 'deleted-broker-id', stale: true }),
    ]);
  });

  it('offers only verified service accounts', () => {
    const options = serviceAccountOptionsForBroker(
      [account('sa-ok', true), account('sa-unverified', false)],
      'flat-a'
    );
    expect(options.map((sa) => sa.id)).toEqual(['sa-ok']);
  });

  it('flags Kubernetes flat brokers with no default when another flat broker has one', () => {
    const known = knownFlatBrokers([
      flatBroker('flat-cr', 'Cloud Run', 'cloudrun'),
      flatBroker('flat-k8s', 'GKE Prod', 'kubernetes'),
    ]);
    expect(brokersMissingDefault([], known)).toEqual([]);
    expect(
      brokersMissingDefault([{ key: 1, brokerId: 'flat-cr', serviceAccountId: 'sa-1' }], known)
    ).toEqual([expect.objectContaining({ id: 'flat-k8s', name: 'GKE Prod' })]);
  });
});

describe('warnings and errors from the save response', () => {
  const known = knownFlatBrokers([flatBroker('b-uuid-1', 'Cloud Run Broker', 'cloudrun')]);

  it('identifies and splits per-broker warnings', () => {
    const w1 = `${F}[b-uuid-1]: account warning`;
    const w2 = 'unrelated warning';
    expect(isBrokerDefaultMessage(w1)).toBe(true);
    expect(isBrokerDefaultMessage(`${F}: map warning`)).toBe(true);
    expect(isBrokerDefaultMessage(w2)).toBe(false);
    expect(splitBrokerWarnings([w1, w2], known)).toEqual({
      byBroker: { 'b-uuid-1': [w1] },
      general: [w2],
    });
  });

  it('parses bracketed broker IDs, whole-map cap errors, and broker-name phrases', () => {
    expect(
      brokerErrorsFromBody(
        { error: { message: `${F}[b-uuid-1]: Runtime Broker "b-uuid-1" not found` } },
        known
      )
    ).toEqual({
      byBroker: { 'b-uuid-1': [`${F}[b-uuid-1]: Runtime Broker "b-uuid-1" not found`] },
      general: [],
    });

    expect(
      brokerErrorsFromBody({ error: { message: `${F}: at most 64 entries are allowed` } }, known)
    ).toEqual({
      byBroker: {},
      general: [`${F}: at most 64 entries are allowed`],
    });

    // Unverified SA message names broker.Name ("Cloud Run Broker"); maps back to b-uuid-1.
    const unverified =
      'GCP service account is not verified; verify it before setting it as the default for Runtime Broker "Cloud Run Broker".';
    expect(brokerErrorsFromBody({ error: { message: unverified } }, known)).toEqual({
      byBroker: { 'b-uuid-1': [unverified] },
      general: [],
    });

    // Unrelated field errors are ignored.
    expect(
      brokerErrorsFromBody({ error: { message: 'defaultMaxDuration: invalid duration' } }, known)
    ).toEqual({
      byBroker: {},
      general: [],
    });
  });
});

describe('effectiveIdentityForBroker follows the hub ladder', () => {
  const unset = { mode: '', serviceAccountId: '' };

  it('ranks per-broker over project (including block) over hub over none', () => {
    expect(
      effectiveIdentityForBroker(
        'flat-1',
        { 'flat-1': 'sa-b' },
        { mode: 'block', serviceAccountId: '' },
        { mode: 'passthrough', serviceAccountId: '' }
      )
    ).toEqual({ source: 'broker', mode: 'assign', serviceAccountId: 'sa-b' });

    expect(
      effectiveIdentityForBroker(
        'flat-1',
        {},
        { mode: 'assign', serviceAccountId: 'sa-proj' },
        { mode: 'block', serviceAccountId: '' }
      )
    ).toEqual({ source: 'project', mode: 'assign', serviceAccountId: 'sa-proj' });

    expect(
      effectiveIdentityForBroker('flat-1', {}, unset, { mode: 'block', serviceAccountId: '' })
    ).toEqual({ source: 'hub', mode: 'block', serviceAccountId: '' });

    expect(effectiveIdentityForBroker('flat-1', {}, unset, unset)).toEqual({
      source: 'none',
      mode: '',
      serviceAccountId: '',
    });
  });
});
