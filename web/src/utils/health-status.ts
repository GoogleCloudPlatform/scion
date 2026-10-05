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
 * Interpretation of the hub's /healthz composite status for display.
 *
 * Status values (see pkg/hub handlers_health.go, criticalHealthChecks):
 *  - healthy:   every check is healthy (the standalone web server reports "ok").
 *  - degraded:  up and serving, but a non-critical check is non-healthy
 *               (e.g. colocated_broker). Shown amber, with the checks named.
 *  - unhealthy: a critical check (database) failed. Shown red.
 */

export type HealthBannerClass = 'healthy' | 'degraded' | 'unhealthy' | 'unknown';

export interface HealthBannerState {
  statusClass: HealthBannerClass;
  label: string;
  /** Non-healthy checks as "key: value", sorted; empty when healthy. */
  problems: string[];
}

type CheckMap = Record<string, string>;

function asCheckMap(value: unknown): CheckMap {
  if (!value || typeof value !== 'object') return {};
  const out: CheckMap = {};
  for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
    if (typeof v === 'string') out[k] = v;
  }
  return out;
}

/**
 * Collects the non-healthy checks from a /healthz body: top-level checks
 * (standalone hub) and the nested hub checks (combined web+hub mode), plus
 * the nested broker's status when it is not healthy. Broker check values are
 * not "healthy"-valued (e.g. docker: "available"), so only its status counts.
 */
export function nonHealthyChecks(health: Record<string, unknown> | null | undefined): string[] {
  if (!health) return [];
  const seen = new Set<string>();
  const add = (checks: CheckMap): void => {
    for (const [k, v] of Object.entries(checks)) {
      if (v !== 'healthy') seen.add(`${k}: ${v}`);
    }
  };
  add(asCheckMap(health.checks));
  const hub = health.hub as Record<string, unknown> | undefined;
  if (hub && typeof hub === 'object') add(asCheckMap(hub.checks));
  const broker = health.broker as Record<string, unknown> | undefined;
  if (broker && typeof broker === 'object') {
    const status = broker.status;
    if (typeof status === 'string' && status !== '' && status !== 'healthy') {
      seen.add(`broker: ${status}`);
    }
  }
  return [...seen].sort();
}

/** Maps a /healthz body (or null before it loads) to banner presentation. */
export function healthBannerState(
  health: Record<string, unknown> | null | undefined
): HealthBannerState {
  const status = typeof health?.status === 'string' && health.status ? health.status : 'unknown';
  switch (status) {
    case 'ok':
    case 'healthy':
      return { statusClass: 'healthy', label: 'Healthy', problems: [] };
    case 'unknown':
      return { statusClass: 'unknown', label: 'Unknown', problems: [] };
    case 'degraded':
      return { statusClass: 'degraded', label: 'Degraded', problems: nonHealthyChecks(health) };
    case 'unhealthy':
      return { statusClass: 'unhealthy', label: 'Unhealthy', problems: nonHealthyChecks(health) };
    default:
      // An unrecognised status is a problem we cannot classify; show it
      // verbatim in the warning style rather than claiming the hub is down.
      return { statusClass: 'degraded', label: status, problems: nonHealthyChecks(health) };
  }
}
