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

import type { Agent, RuntimeBroker } from '../../shared/types.js';
import { agentPlacementView } from './agent-placement.js';

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    runtimeBrokerId: 'b-flat',
    runtimeBrokerName: 'flat-docker',
    pinnedRuntimeTarget: { id: 't-1', type: 'docker', runtimeBrokerId: 'b-flat' },
    ...overrides,
  } as Agent;
}

function broker(overrides: Partial<RuntimeBroker>): RuntimeBroker {
  return {
    id: 'b-flat',
    name: 'flat-docker',
    slug: 'flat-docker',
    version: '',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '',
    autoProvide: false,
    runtimeTarget: { id: 't-1', type: 'docker', displayName: 'Local Docker' },
    ...overrides,
  } as RuntimeBroker;
}

describe('agentPlacementView', () => {
  it('is null for an unpinned agent (legacy rendering is kept)', () => {
    const unpinned = agent({});
    delete unpinned.pinnedRuntimeTarget;
    expect(agentPlacementView(unpinned, broker({}))).toBeNull();
  });

  it('shows the stored Runtime Broker, target display name and type', () => {
    const v = agentPlacementView(agent({}), broker({}));
    expect(v).not.toBeNull();
    expect(v!.runtimeBrokerName).toBe('flat-docker');
    expect(v!.targetLabel).toBe('Local Docker');
    expect(v!.targetType).toBe('docker');
    expect(v!.targetId).toBe('t-1');
    expect(v!.stale).toBe(false);
  });

  it('falls back to the target type before the Runtime Broker row loads', () => {
    const v = agentPlacementView(agent({}), null);
    expect(v!.targetLabel).toBe('docker');
    expect(v!.runtimeBrokerName).toBe('flat-docker');
    expect(v!.connection).toEqual({ label: 'unknown', status: 'neutral' });
  });

  it('does not take a display name from a different target', () => {
    const v = agentPlacementView(
      agent({}),
      broker({ runtimeTarget: { id: 'other', type: 'docker', displayName: 'Other' } })
    );
    expect(v!.targetLabel).toBe('docker');
  });

  it('keeps connection status and failed runtime operations separate', () => {
    // Connected Runtime Broker, failed runtime operation.
    let v = agentPlacementView(agent({ phase: 'error', message: 'start refused' }), broker({}));
    expect(v!.connection.label).toBe('online');
    expect(v!.connection.status).toBe('success');
    expect(v!.lastRuntimeOperation).toEqual({
      label: 'failed',
      status: 'danger',
      detail: 'start refused',
    });

    // Disconnected Runtime Broker, no failed runtime operation.
    v = agentPlacementView(
      agent({ phase: 'stopped' }),
      broker({ status: 'offline', connectionState: 'disconnected' })
    );
    expect(v!.connection).toEqual({ label: 'offline', status: 'danger', detail: 'disconnected' });
    expect(v!.lastRuntimeOperation).toEqual({ label: 'none recorded', status: 'neutral' });
  });

  it('shows a refused start recorded on a stopped agent, never a success', () => {
    // A Runtime Broker refusal leaves the resting phase and records the
    // refusal as the agent message.
    const v = agentPlacementView(
      agent({ phase: 'stopped', message: 'refused by the Runtime Broker' }),
      broker({})
    );
    expect(v!.lastRuntimeOperation).toEqual({
      label: 'see message',
      status: 'warning',
      detail: 'refused by the Runtime Broker',
    });
    // A running agent's activity message is not a runtime-operation result.
    const running = agentPlacementView(agent({ phase: 'running', message: 'working' }), broker({}));
    expect(running!.lastRuntimeOperation).toEqual({ label: 'none recorded', status: 'neutral' });
    for (const phase of ['stopped', 'created', 'suspended', 'running', 'starting'] as const) {
      expect(
        agentPlacementView(agent({ phase }), broker({}))!.lastRuntimeOperation.status
      ).not.toBe('success');
    }
  });

  it('flags a stale pin and does not report the current Runtime Broker as the pinned one', () => {
    const v = agentPlacementView(
      agent({ runtimeBrokerId: 'b-legacy', runtimeBrokerName: 'legacy-broker' }),
      null
    );
    expect(v!.stale).toBe(true);
    expect(v!.runtimeBrokerId).toBe('b-flat');
    expect(v!.runtimeBrokerName).toBe('b-flat');
  });
});
