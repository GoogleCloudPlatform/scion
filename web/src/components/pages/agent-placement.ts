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
 * Agent placement view for flat Runtime Brokers (ptone/scion#3269).
 *
 * Pure, presentation-only derivation from stored API data: the agent's
 * read-only `pinnedRuntimeTarget` and, when loaded, the Runtime Broker row
 * (`GET /api/v1/runtime-brokers/{id}`). It keeps two indicators apart:
 * the Runtime Broker's connection status (from the broker row) and failed
 * runtime operations on the agent (from the agent's own phase and message).
 * No polling and no new hub fields.
 */

import type { Agent, RuntimeBroker } from '../../shared/types.js';
import type { StatusType } from '../shared/status-badge.js';

export interface PlacementIndicator {
  label: string;
  status: StatusType;
  detail?: string | undefined;
}

export interface AgentPlacementView {
  runtimeBrokerId: string;
  runtimeBrokerName: string;
  targetType: string;
  /** The target's display name, falling back to its type. */
  targetLabel: string;
  targetId: string;
  /**
   * True when the pin names a different Runtime Broker than the agent's
   * current one (a stale pin: starts are refused until it is repaired).
   */
  stale: boolean;
  connection: PlacementIndicator;
  lastRuntimeOperation: PlacementIndicator;
}

/**
 * Returns the placement view for a pinned agent, or null for an unpinned
 * (profile-based) agent, which keeps the existing rendering. broker is the
 * Runtime Broker row of the pinned Runtime Broker, or null while it is not
 * loaded (or could not be read).
 */
export function agentPlacementView(
  agent: Agent,
  broker: RuntimeBroker | null
): AgentPlacementView | null {
  const pin = agent.pinnedRuntimeTarget;
  if (!pin) {
    return null;
  }
  const brokerMatches = broker !== null && broker.id === pin.runtimeBrokerId;
  const displayName =
    brokerMatches && broker.runtimeTarget?.id === pin.id ? broker.runtimeTarget.displayName : '';
  return {
    runtimeBrokerId: pin.runtimeBrokerId,
    runtimeBrokerName:
      (brokerMatches && broker.name) ||
      (agent.runtimeBrokerId === pin.runtimeBrokerId ? agent.runtimeBrokerName : '') ||
      pin.runtimeBrokerId,
    targetType: pin.type,
    targetLabel: displayName || pin.type,
    targetId: pin.id,
    stale: agent.runtimeBrokerId !== pin.runtimeBrokerId,
    connection: brokerConnectionIndicator(brokerMatches ? broker : null),
    lastRuntimeOperation: lastRuntimeOperationIndicator(agent),
  };
}

/** Runtime Broker connection status, from the stored Runtime Broker row only. */
export function brokerConnectionIndicator(broker: RuntimeBroker | null): PlacementIndicator {
  if (!broker) {
    return { label: 'unknown', status: 'neutral' };
  }
  const detail = broker.connectionState || undefined;
  switch (broker.status) {
    case 'online':
      return { label: 'online', status: 'success', detail };
    case 'degraded':
      return { label: 'degraded', status: 'warning', detail };
    case 'offline':
      return { label: 'offline', status: 'danger', detail };
    default:
      return { label: broker.status || 'unknown', status: 'neutral', detail };
  }
}

/** Phases in which the agent is not running and no start is under way. */
const RESTING_PHASES = new Set(['stopped', 'created', 'suspended']);

/**
 * Failed runtime operations on the agent, from the agent's own phase and
 * message: independent of whether its Runtime Broker is connected.
 *
 * An error phase is a failure. A refused start (for example a Runtime
 * Broker refusal) leaves the agent in its resting phase with the refusal
 * as its message, and the stored data does not tell that message apart
 * from any other, so a resting agent's message is shown as a warning to
 * read rather than as a failure. Nothing is ever shown as a success.
 */
export function lastRuntimeOperationIndicator(agent: Agent): PlacementIndicator {
  if (agent.phase === 'error') {
    return { label: 'failed', status: 'danger', detail: agent.message || undefined };
  }
  if (agent.message && RESTING_PHASES.has(agent.phase)) {
    return { label: 'see message', status: 'warning', detail: agent.message };
  }
  return { label: 'none recorded', status: 'neutral' };
}
