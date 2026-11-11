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
import { healthBannerState, nonHealthyChecks } from './health-status.js';

describe('healthBannerState', () => {
  it('shows unknown before the health fetch resolves', () => {
    expect(healthBannerState(null)).toEqual({
      statusClass: 'unknown',
      label: 'Unknown',
      problems: [],
    });
  });

  it('treats healthy and ok as healthy', () => {
    expect(healthBannerState({ status: 'healthy' }).statusClass).toBe('healthy');
    expect(healthBannerState({ status: 'ok' }).statusClass).toBe('healthy');
  });

  it('shows degraded in its own (warning) style and names the cause', () => {
    const state = healthBannerState({
      status: 'degraded',
      web: { status: 'ok' },
      hub: {
        status: 'degraded',
        checks: { database: 'healthy', colocated_broker: 'unhealthy: registration failed' },
      },
    });
    expect(state.statusClass).toBe('degraded');
    expect(state.label).toBe('Degraded');
    expect(state.problems).toEqual(['colocated_broker: unhealthy: registration failed']);
  });

  it('shows unhealthy in the danger style', () => {
    const state = healthBannerState({ status: 'unhealthy', checks: { database: 'unhealthy' } });
    expect(state.statusClass).toBe('unhealthy');
    expect(state.label).toBe('Unhealthy');
    expect(state.problems).toEqual(['database: unhealthy']);
  });

  it('does not claim an unrecognised status is down', () => {
    const state = healthBannerState({ status: 'weird' });
    expect(state.statusClass).toBe('degraded');
    expect(state.label).toBe('weird');
  });
});

describe('nonHealthyChecks', () => {
  it("names a degraded broker's problem checks", () => {
    expect(
      nonHealthyChecks({
        status: 'degraded',
        hub: { status: 'healthy', checks: { database: 'healthy' } },
        broker: {
          status: 'degraded',
          checks: { docker: 'available', nfs_mounts: 'unhealthy: share1 not mounted' },
        },
      })
    ).toEqual(['broker.nfs_mounts: unhealthy: share1 not mounted']);
  });

  it('falls back to the broker status when no broker check qualifies', () => {
    expect(
      nonHealthyChecks({
        status: 'degraded',
        hub: { status: 'healthy', checks: { database: 'healthy' } },
        broker: { status: 'degraded', checks: { docker: 'available' } },
      })
    ).toEqual(['broker: degraded']);
  });

  it('ignores broker checks when the broker is healthy', () => {
    expect(
      nonHealthyChecks({
        status: 'healthy',
        broker: { status: 'healthy', checks: { runtime: 'unavailable' } },
      })
    ).toEqual([]);
  });

  it('sorts and dedupes across top-level and nested checks', () => {
    expect(
      nonHealthyChecks({
        checks: { b: 'unhealthy', a: 'unhealthy: x' },
        hub: { checks: { b: 'unhealthy' } },
      })
    ).toEqual(['a: unhealthy: x', 'b: unhealthy']);
  });
});
