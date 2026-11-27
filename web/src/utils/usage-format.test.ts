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
  computeCacheHitRate,
  formatCacheHitRateValue,
  formatCompactUsageToken,
  formatDashboardSourceLabel,
  formatUsageToken,
  isMeasurementPartial,
} from './usage-format.js';

describe('usage-format helpers', () => {
  it('formats unknown (null/undefined) tokens as em dash, never 0', () => {
    expect(formatUsageToken(null)).toBe('—');
    expect(formatUsageToken(undefined)).toBe('—');
    expect(formatCompactUsageToken(null)).toBe('—');
    expect(formatCompactUsageToken(undefined)).toBe('—');
    expect(formatCompactUsageToken(null, true)).toBe('—');
  });

  it('formats measured zero, exact, compact, partial, and saturated token totals', () => {
    expect(formatUsageToken(0)).toBe('0');
    expect(formatUsageToken(42, 2)).toBe('≥ 42');
    expect(formatUsageToken(42, 0, true)).toBe('42+');
    expect(formatCompactUsageToken(0)).toBe('0');
    expect(formatCompactUsageToken(999)).toBe('999');
    expect(formatCompactUsageToken(1_500)).toBe('1.5K');
    expect(formatCompactUsageToken(2_500_000, true)).toBe('≥ 2.5M');
    expect(formatCompactUsageToken(1_200_000_000, false, true)).toBe('1.2B+');
  });

  it('computes and formats cache hit rate with null, zero, and approximate handling', () => {
    expect(computeCacheHitRate({ input: 12_000, cache_read: 36_000, cache_write: 2_000 })).toBe(
      0.72
    );
    expect(computeCacheHitRate({ input: 100, cache_read: 0, cache_write: 0 })).toBe(0);
    expect(computeCacheHitRate({ input: 0, cache_read: 0, cache_write: 0 })).toBeNull();
    expect(computeCacheHitRate({ input: null, cache_read: 10, cache_write: 0 })).toBeNull();
    expect(computeCacheHitRate({ input: 10, cache_read: null, cache_write: 0 })).toBeNull();
    expect(computeCacheHitRate({ input: 10, cache_read: 0, cache_write: null })).toBeNull();

    expect(formatCacheHitRateValue(null)).toBe('—');
    expect(formatCacheHitRateValue(undefined)).toBe('—');
    expect(formatCacheHitRateValue(Number.NaN)).toBe('—');
    expect(formatCacheHitRateValue(0)).toBe('0.0%');
    expect(formatCacheHitRateValue(0.72)).toBe('72.0%');
    expect(formatCacheHitRateValue(0.72, true)).toBe('≈ 72.0%');
  });

  it('detects partial measurement coverage from status or agent counts', () => {
    expect(isMeasurementPartial(null)).toBe(false);
    expect(isMeasurementPartial({ status: 'measured', unmeasuredAgents: 0, staleAgents: 0 })).toBe(
      false
    );
    expect(isMeasurementPartial({ status: 'measured', unmeasuredAgents: 1, staleAgents: 0 })).toBe(
      true
    );
    expect(isMeasurementPartial({ status: 'measured', unmeasuredAgents: 0, staleAgents: 2 })).toBe(
      true
    );
    expect(isMeasurementPartial({ status: 'partial', unmeasuredAgents: 0, staleAgents: 0 })).toBe(
      true
    );
    expect(isMeasurementPartial({ status: 'stale', unmeasuredAgents: 0, staleAgents: 0 })).toBe(
      true
    );
    expect(
      isMeasurementPartial({ status: 'calls_only', unmeasuredAgents: 0, staleAgents: 0 })
    ).toBe(true);
    expect(
      isMeasurementPartial({ status: 'unmeasured', unmeasuredAgents: 0, staleAgents: 0 })
    ).toBe(false);
  });

  it('formats dashboard source and granularity labels unobtrusively', () => {
    expect(formatDashboardSourceLabel(undefined)).toBeNull();
    expect(formatDashboardSourceLabel('cloud_monitoring')).toBe('Cloud Monitoring');
    expect(formatDashboardSourceLabel('hub_rollups', 'hour', 'America/Chicago')).toBe(
      'Hub rollups · Hourly'
    );
    expect(formatDashboardSourceLabel('hub_rollups', 'day', 'UTC')).toBe(
      'Hub rollups · Daily (UTC)'
    );
    expect(formatDashboardSourceLabel('hub_rollups', 'day', 'America/Chicago')).toBe(
      'Hub rollups · Daily'
    );
  });
});
