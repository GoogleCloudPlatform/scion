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
 * Broker routes (broker UI Option A, slice A1).
 *
 * The router (`main.ts` resolveRoute) delegates the two broker paths here,
 * so the whole flag decision is one pure, testable function:
 *
 * - experiment hub.flat_runtime_brokers off: the existing pages, with
 *   exactly the tags and loaders they had before;
 * - on: the Brokers area pages.
 *
 * Paths are unchanged either way. The flag only selects what renders;
 * every hub check is server-side.
 */

/** Same shape as the router's route entries. */
export interface BrokerRouteConfig {
  pattern: RegExp;
  tag: string;
  load: () => Promise<unknown>;
}

/** `/brokers` */
export const BROKERS_LIST_ROUTE = /^\/brokers$/;

/** `/brokers/{id}` */
export const BROKER_DETAIL_ROUTE = /^\/brokers\/[^/]+$/;

/** The existing pages (flag off). */
export const LEGACY_BROKER_ROUTES: readonly BrokerRouteConfig[] = [
  {
    pattern: BROKERS_LIST_ROUTE,
    tag: 'scion-page-brokers',
    load: () => import('../components/pages/brokers.js'),
  },
  {
    pattern: BROKER_DETAIL_ROUTE,
    tag: 'scion-page-broker-detail',
    load: () => import('../components/pages/broker-detail.js'),
  },
];

/** The Brokers area pages (flag on). */
export const AREA_BROKER_ROUTES: readonly BrokerRouteConfig[] = [
  {
    pattern: BROKERS_LIST_ROUTE,
    tag: 'scion-page-brokers-area',
    load: () => import('../components/pages/brokers-area.js'),
  },
  {
    pattern: BROKER_DETAIL_ROUTE,
    tag: 'scion-page-broker-area-detail',
    load: () => import('../components/pages/broker-area-detail.js'),
  },
];

/**
 * The route for a broker path, or null when `path` is not a broker path.
 * `enabled` is the value of the hub.flat_runtime_brokers experiment.
 */
export function resolveBrokerRoute(path: string, enabled: boolean): BrokerRouteConfig | null {
  const routes = enabled ? AREA_BROKER_ROUTES : LEGACY_BROKER_ROUTES;
  return routes.find((r) => r.pattern.test(path)) ?? null;
}
