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
 * Filter chips of the Brokers area list (broker UI Option A, slice A1):
 * All, Flat, Legacy and Needs attention. Pure functions, so the list page
 * and its tests share one rule.
 */

import { isFlatBroker } from './runtime-target.js';
import type { RuntimeBroker } from './types.js';

/** The permission the admin health summary endpoint authorizes. */
export const HEALTH_READ_PERMISSION = 'hub.health.read';

export type BrokerFilter = 'all' | 'flat' | 'legacy' | 'attention';

export const BROKER_FILTERS: { id: BrokerFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'flat', label: 'Flat' },
  { id: 'legacy', label: 'Legacy' },
  { id: 'attention', label: 'Needs attention' },
];

/** A broker's self-reported health, as the admin health summary has it. */
export interface BrokerHealthInfo {
  status?: string;
  checks?: Record<string, string>;
}

/** Check results that count as passing. */
const PASSING_CHECKS = new Set(['healthy', 'available']);

/** The names of a health report's failing checks, sorted. */
export function failingChecks(health: BrokerHealthInfo | null | undefined): string[] {
  return Object.entries(health?.checks ?? {})
    .filter(([, result]) => !PASSING_CHECKS.has(result))
    .map(([name]) => name)
    .sort();
}

/**
 * Whether a self-reported health is a problem: the broker reports itself
 * degraded or unhealthy, or a check fails. The same rule as the health
 * dashboard's table (a status that is not healthy is never shown as ok); kept
 * here because shared/ code does not import page components.
 */
export function healthProblem(health: BrokerHealthInfo | null | undefined): boolean {
  if (health?.status === 'degraded' || health?.status === 'unhealthy') return true;
  return failingChecks(health).length > 0;
}

/**
 * Whether a broker needs attention: it is offline or degraded, or (when its
 * health is known) its self-reported health is a problem.
 */
export function needsAttention(
  broker: Pick<RuntimeBroker, 'status'>,
  health?: BrokerHealthInfo | null
): boolean {
  if (broker.status === 'offline' || broker.status === 'degraded') return true;
  return healthProblem(health);
}

/** Parses a ?filter= value; anything unknown is "all". */
export function parseBrokerFilter(value: string | null | undefined): BrokerFilter {
  switch (value) {
    case 'flat':
    case 'legacy':
    case 'attention':
      return value;
    default:
      return 'all';
  }
}

/** Whether a broker passes the chosen filter chip. */
export function matchesBrokerFilter(
  broker: RuntimeBroker,
  filter: BrokerFilter,
  health?: BrokerHealthInfo | null
): boolean {
  switch (filter) {
    case 'flat':
      return isFlatBroker(broker);
    case 'legacy':
      return !isFlatBroker(broker);
    case 'attention':
      return needsAttention(broker, health);
    default:
      return true;
  }
}
