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
 * Shared Track 1 token usage formatting helpers.
 *
 * Invariants (§3.1, §3.3, §4.6):
 * - Unknown (null/undefined) values always render as "—", never 0.
 * - Partial values (mixed known/unknown events, or unmeasured/stale agents)
 *   are visibly marked with "≥ " for totals and "≈ " for ratios.
 * - Cache hit rate is cache_read / (input + cache_read + cache_write),
 *   returning null when any component is unknown or the denominator <= 0.
 */

import type {
  AgentUsageTokens,
  DashboardMeasurement,
  DashboardTokenTotals,
  UsageMeasurementStatus,
} from '../shared/types.js';
import { formatNumber } from './format-number.js';

export const UNKNOWN_VALUE_DISPLAY = '—';

/**
 * Format an exact nullable token count. Unknown (null/undefined) renders as
 * "—", never 0. When unknownEvents > 0, prefixes with "≥ " to indicate a lower
 * bound from partially unknown events.
 */
export function formatUsageToken(
  value: number | null | undefined,
  unknownEvents = 0,
  saturated = false
): string {
  if (value === null || value === undefined) return UNKNOWN_VALUE_DISPLAY;
  const formatted = `${formatNumber(value)}${saturated ? '+' : ''}`;
  return unknownEvents > 0 ? `≥ ${formatted}` : formatted;
}

/**
 * Format a compact nullable token count (1.2K, 3.4M, 1.1B). Unknown
 * (null/undefined) renders as "—", never 0. When partial is true, prefixes with
 * "≥ " to indicate a lower bound.
 */
export function formatCompactUsageToken(
  value: number | null | undefined,
  partial = false,
  saturated = false
): string {
  if (value === null || value === undefined) return UNKNOWN_VALUE_DISPLAY;
  let compact: string;
  if (value >= 1_000_000_000) {
    compact = `${(value / 1_000_000_000).toFixed(1)}B`;
  } else if (value >= 1_000_000) {
    compact = `${(value / 1_000_000).toFixed(1)}M`;
  } else if (value >= 1_000) {
    compact = `${(value / 1_000).toFixed(1)}K`;
  } else {
    compact = formatNumber(value);
  }
  const withSat = saturated ? `${compact}+` : compact;
  return partial ? `≥ ${withSat}` : withSat;
}

/**
 * Return the maximum unknown-event count affecting summable_tokens
 * (input, output, cache_read, cache_write, or all-null events).
 */
export function summableUnknownEvents(tokens: AgentUsageTokens, nullTokenEvents = 0): number {
  const u = tokens.unknown_events;
  return Math.max(
    nullTokenEvents,
    u?.input ?? 0,
    u?.output ?? 0,
    u?.cache_read ?? 0,
    u?.cache_write ?? 0
  );
}

/**
 * Return true if any token bucket has mixed known+unknown events.
 */
export function hasPartialTokenBuckets(tokens: AgentUsageTokens): boolean {
  const u = tokens.unknown_events;
  if (!u) return false;
  return (
    (u.input ?? 0) > 0 ||
    (u.output ?? 0) > 0 ||
    (u.cache_read ?? 0) > 0 ||
    (u.cache_write ?? 0) > 0 ||
    (u.cache_write_1h ?? 0) > 0 ||
    (u.reasoning ?? 0) > 0
  );
}

/**
 * Compute cache hit rate: cache_read / (input + cache_read + cache_write).
 * Returns null when any required bucket is unknown (null) or the denominator
 * is 0.
 */
export function computeCacheHitRate(tokens: {
  input: number | null;
  cache_read: number | null;
  cache_write: number | null;
}): number | null {
  if (tokens.input === null || tokens.cache_read === null || tokens.cache_write === null) {
    return null;
  }
  const denom = tokens.input + tokens.cache_read + tokens.cache_write;
  if (denom <= 0) return null;
  return tokens.cache_read / denom;
}

export const CACHE_HIT_RATE_UPPER_BOUND_TOOLTIP =
  'Upper bound: some events had unknown input or cache write tokens.';

/**
 * Format a nullable cache hit rate ratio [0, 1] as a percentage string
 * (e.g. "23.1%"). When upperBound is true (unknown input or cache_write
 * tokens), prefixes with "≤ "; otherwise when approx is true, prefixes with
 * "≈ ". Unknown (null/undefined/non-finite) always renders as "—". Measured 0
 * renders as "0.0%".
 */
export function formatCacheHitRateValue(
  rate: number | null | undefined,
  approx = false,
  upperBound = false
): string {
  if (rate === null || rate === undefined || !Number.isFinite(rate)) {
    return UNKNOWN_VALUE_DISPLAY;
  }
  const pct = `${(rate * 100).toFixed(1)}%`;
  if (upperBound) return `≤ ${pct}`;
  return approx ? `≈ ${pct}` : pct;
}

/**
 * Compute and format cache hit rate from canonical snake_case token buckets.
 */
export function formatCacheHitRate(tokens: AgentUsageTokens): string {
  const rate = computeCacheHitRate(tokens);
  if (rate === null) return UNKNOWN_VALUE_DISPLAY;
  const u = tokens.unknown_events;
  const approx = (u?.input ?? 0) > 0 || (u?.cache_read ?? 0) > 0 || (u?.cache_write ?? 0) > 0;
  return formatCacheHitRateValue(rate, approx);
}

/**
 * Return true when a scope measurement summary indicates partial coverage
 * (status is partial/stale/calls_only, or unmeasured/stale agents are present).
 */
export function isMeasurementPartial(
  measurement:
    | Pick<DashboardMeasurement, 'status' | 'unmeasuredAgents' | 'staleAgents'>
    | { status: UsageMeasurementStatus; unmeasuredAgents?: number; staleAgents?: number }
    | null
    | undefined
): boolean {
  if (!measurement) return false;
  if (
    measurement.status === 'partial' ||
    measurement.status === 'stale' ||
    measurement.status === 'calls_only'
  ) {
    return true;
  }
  return (measurement.unmeasuredAgents ?? 0) > 0 || (measurement.staleAgents ?? 0) > 0;
}

/**
 * Return true when a dashboard token totals object has a non-null summable
 * total while at least one summable component bucket (input, output,
 * cacheRead, cacheWrite) is unknown (null), making summableTokens a partial
 * known-components lower bound.
 */
export function hasPartialDashboardTotals(
  totals: DashboardTokenTotals | null | undefined
): boolean {
  if (!totals || totals.summableTokens === null) return false;
  const u = totals.unknownEvents;
  if (
    u &&
    ((u.input ?? 0) > 0 || (u.output ?? 0) > 0 || (u.cacheRead ?? 0) > 0 || (u.cacheWrite ?? 0) > 0)
  ) {
    return true;
  }
  return (
    totals.input === null ||
    totals.output === null ||
    totals.cacheRead === null ||
    totals.cacheWrite === null
  );
}

/**
 * Format a compact stat label describing the response window of a scoped usage
 * summary (e.g. "Tokens (30d)"), so a windowed ledger total is never mistaken
 * for an all-time lifetime sum.
 */
export function formatUsageWindowLabel(
  window: { since: string; until: string } | null | undefined
): string {
  if (!window) return 'Tokens (window)';
  const sinceMs = Date.parse(window.since);
  const untilMs = Date.parse(window.until);
  if (Number.isFinite(sinceMs) && Number.isFinite(untilMs) && untilMs > sinceMs) {
    // Hub day-snapped windows start at UTC midnight (N-1) days before until,
    // so the elapsed duration is (N-1) days + hours since UTC midnight; ceil
    // maps the whole UTC day span consistently to N days without flipping at noon.
    const days = Math.max(1, Math.ceil((untilMs - sinceMs) / 86_400_000));
    return `Tokens (${days}d)`;
  }
  return 'Tokens (window)';
}

/**
 * Format an unobtrusive source and granularity label for dashboard headers.
 * Returns null when source is absent (e.g., older hubs or Cloud Monitoring
 * responses without source metadata).
 */
export function formatDashboardSourceLabel(
  source?: string,
  granularity?: string,
  timeZone?: string
): string | null {
  if (!source) return null;
  const sourceLabel =
    source === 'hub_rollups'
      ? 'Hub rollups'
      : source === 'cloud_monitoring'
        ? 'Cloud Monitoring'
        : source;
  if (!granularity) return sourceLabel;
  const granLabel =
    granularity === 'hour'
      ? 'Local-day buckets'
      : granularity === 'day'
        ? timeZone === 'UTC'
          ? 'Daily (UTC)'
          : 'Daily'
        : granularity;
  return `${sourceLabel} · ${granLabel}`;
}
