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

/** Shared fixtures for the Brokers area page tests (broker UI A1). */

import type { RuntimeBroker } from '../../../shared/types.js';
import { fullPresentation } from '../../../shared/__fixtures__/runtime-target-presentation.js';

/** happy-dom has no EventSource; stateManager.setScope opens one. */
export class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function base(over: Partial<RuntimeBroker>): RuntimeBroker {
  return {
    id: 'b',
    name: 'b',
    slug: 'b',
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-10-20T00:00:00Z',
    autoProvide: false,
    agentCount: 2,
    agentLimit: 30,
    agentLimitSource: 'hub_default',
    _capabilities: { actions: ['read'] },
    ...over,
  };
}

/** A flat Kubernetes broker hosted by this server. */
export const FLAT_K8S = base({
  id: 'flat-k8s',
  name: 'k8s-a',
  slug: 'k8s-a',
  hostedByThisServer: true,
  runtimeTarget: { id: 'rt-k8s', type: 'kubernetes', displayName: 'Cluster A' },
  runtimeTargetPresentation: fullPresentation(),
  runtimeTargetPresentationObservedAt: '2026-10-20T00:00:00Z',
});

/** A remote flat docker broker. */
export const FLAT_DOCKER = base({
  id: 'flat-docker',
  name: 'dock-1',
  slug: 'dock-1',
  runtimeTarget: { id: 'rt-dock', type: 'docker' },
  agentLimitSource: 'not_enforced',
});

/** A legacy broker with three profiles, offline. */
export const LEGACY = base({
  id: 'legacy-1',
  name: 'old-box',
  slug: 'old-box',
  status: 'offline',
  profiles: [
    { name: 'a', type: 'docker', available: true },
    { name: 'b', type: 'kubernetes', available: true },
    { name: 'c', type: 'docker', available: false },
  ],
});

/** An admin status that holds hub.health.read (or not). */
export function adminStatus(canReadHealth: boolean) {
  return {
    isAdmin: canReadHealth,
    isSuperAdmin: false,
    permissions: canReadHealth ? ['hub.health.read'] : [],
  };
}

/** A health summary listing the given brokers' health. */
export function healthSummary(
  items: { id: string; checks?: Record<string, string> }[],
  truncated = false
) {
  return {
    runtime_brokers: {
      items: items.map((i) => ({
        id: i.id,
        name: i.id,
        version: '1',
        status: 'online',
        last_heartbeat: '2026-10-20T00:00:00Z',
        runtime: null,
        workspace_storage: { backend: 'local' },
        health: { status: 'healthy', checks: i.checks ?? { runtime: 'available' } },
        agents: { running: 1, attention: 0 },
      })),
      total: items.length,
      truncated,
    },
  };
}

/** Lets pending fetches resolve and Lit re-render. */
export async function settle(el: HTMLElement & { updateComplete: Promise<boolean> }) {
  for (let i = 0; i < 3; i++) {
    await new Promise((resolve) => setTimeout(resolve, 10));
    await el.updateComplete;
  }
}
