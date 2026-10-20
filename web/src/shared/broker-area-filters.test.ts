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

/** V6: the Brokers area filter chips and the needs-attention rule. */

import { describe, it, expect } from 'vitest';

import {
  failingChecks,
  healthProblem,
  matchesBrokerFilter,
  needsAttention,
  parseBrokerFilter,
} from './broker-area-filters.js';
import type { RuntimeBroker } from './types.js';

function broker(over: Partial<RuntimeBroker>): RuntimeBroker {
  return {
    id: 'b',
    name: 'b',
    slug: 'b',
    version: '1',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-10-20T00:00:00Z',
    autoProvide: false,
    ...over,
  } as RuntimeBroker;
}

const flat = broker({ id: 'f', runtimeTarget: { id: 'rt', type: 'docker' } });
const legacy = broker({ id: 'l' });

describe('broker area filters', () => {
  it('Flat and Legacy split on the runtime target', () => {
    expect(matchesBrokerFilter(flat, 'flat')).toBe(true);
    expect(matchesBrokerFilter(legacy, 'flat')).toBe(false);
    expect(matchesBrokerFilter(legacy, 'legacy')).toBe(true);
    expect(matchesBrokerFilter(flat, 'legacy')).toBe(false);
    expect(matchesBrokerFilter(flat, 'all')).toBe(true);
    expect(matchesBrokerFilter(legacy, 'all')).toBe(true);
  });

  it('needs attention when offline, degraded or a check fails', () => {
    expect(needsAttention(broker({ status: 'offline' }))).toBe(true);
    expect(needsAttention(broker({ status: 'degraded' }))).toBe(true);
    expect(needsAttention(broker({}))).toBe(false);
    expect(needsAttention(broker({}), { checks: { runtime: 'unavailable' } })).toBe(true);
    expect(needsAttention(broker({}), { checks: { runtime: 'available', db: 'healthy' } })).toBe(
      false
    );
    expect(matchesBrokerFilter(broker({}), 'attention', { checks: { nfs: 'degraded' } })).toBe(
      true
    );
  });

  it('a self-reported degraded or unhealthy status is a problem even with no failing check', () => {
    expect(healthProblem({ status: 'unhealthy', checks: {} })).toBe(true);
    expect(healthProblem({ status: 'degraded' })).toBe(true);
    expect(healthProblem({ status: 'healthy', checks: { runtime: 'available' } })).toBe(false);
    expect(needsAttention(broker({}), { status: 'unhealthy' })).toBe(true);
  });

  it('lists failing checks sorted, and parses ?filter=', () => {
    expect(failingChecks({ checks: { z: 'unhealthy', a: 'unknown', ok: 'healthy' } })).toEqual([
      'a',
      'z',
    ]);
    expect(failingChecks(null)).toEqual([]);
    expect(parseBrokerFilter('flat')).toBe('flat');
    expect(parseBrokerFilter('attention')).toBe('attention');
    expect(parseBrokerFilter('bogus')).toBe('all');
    expect(parseBrokerFilter(null)).toBe('all');
  });
});
