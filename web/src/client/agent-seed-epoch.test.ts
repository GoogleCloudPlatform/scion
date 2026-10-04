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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { Agent } from '../shared/types.js';
import { StateManager } from './state.js';
import { AgentSeedEpoch } from './agent-seed-epoch.js';

class FakeEventSource extends EventTarget {
  readyState = 0;
  close(): void {
    this.readyState = 2;
  }
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function makeAgent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    ...overrides,
  } as Agent;
}

function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
  (sm as unknown as { flush(): void }).flush();
}

function openEpochs(sm: StateManager): number {
  return (sm as unknown as { seedEpochs: Map<unknown, unknown> }).seedEpochs.size;
}

function newState(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'project', projectId: 'p1' });
  return sm;
}

describe('AgentSeedEpoch', () => {
  it('a live update during the request survives the older response rows', () => {
    const sm = newState();
    const row = makeAgent('a');
    sm.seedAgents([row]);
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.a.status', { phase: 'stopped' });
    const result = epoch.seed([row], { partial: false });
    epoch.close();
    expect(sm.getAgent('a')?.phase).toBe('stopped');
    expect(result.agents.map((a) => [a.id, a.phase])).toEqual([['a', 'stopped']]);
    expect(openEpochs(sm)).toBe(0);
  });

  it('a live create the response predates joins the membership when it passes the rule', () => {
    const sm = newState();
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.n1.created', { id: 'n1', name: 'n1', projectId: 'p1', phase: 'running' });
    emit(sm, 'agent.n2.created', { id: 'n2', name: 'n2', projectId: 'p2', phase: 'running' });
    const result = epoch.seed([makeAgent('a')], {
      partial: false,
      isMember: (agent) => agent.projectId === 'p1',
    });
    epoch.close();
    expect(result.agents.map((a) => a.id)).toEqual(['a', 'n1']);
    expect(result.liveCreated.map((a) => a.id)).toEqual(['n1']);
    expect(result.undecided).toBe(false);
  });

  it('without a rule a live create is not added and the result is undecided', () => {
    const sm = newState();
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.n1.created', { id: 'n1', name: 'n1', projectId: 'p1', phase: 'running' });
    const result = epoch.seed([makeAgent('a')], { partial: true });
    epoch.close();
    expect(result.agents.map((a) => a.id)).toEqual(['a']);
    expect(result.undecided).toBe(true);
  });

  it('a live delete during the request drops the row; changedIds lists live upserts minus deletes', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b')]);
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.a.status', { phase: 'stopped' });
    emit(sm, 'agent.b.status', { phase: 'stopped' });
    emit(sm, 'agent.b.deleted', { agentId: 'b' });
    const result = epoch.seed([makeAgent('a'), makeAgent('b')], { partial: true });
    expect(result.agents.map((a) => a.id)).toEqual(['a']);
    expect(epoch.changedIds).toEqual(['a']);
    epoch.close();
    epoch.close(); // idempotent
    expect(openEpochs(sm)).toBe(0);
  });

  it('unknownChanges merges the live deltas of IDs not in the store per field, minus deletes', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a')]);
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.x.status', { agentId: 'x', phase: 'running' });
    emit(sm, 'agent.y.status', { agentId: 'y', phase: 'running' });
    emit(sm, 'agent.z.status', { agentId: 'z', activity: 'thinking' });
    emit(sm, 'agent.a.status', { phase: 'stopped' });
    emit(sm, 'agent.x.status', {
      agentId: 'x',
      phase: 'stopped',
      lastActivityEvent: '2026-02-01T00:00:00Z',
    });
    emit(sm, 'agent.x.status', { agentId: 'x', lastActivityEvent: '2026-03-01T00:00:00Z' });
    emit(sm, 'agent.y.deleted', { agentId: 'y' });
    expect(epoch.unknownChanges).toEqual(
      new Map([
        ['x', { phase: 'stopped', lastActivityEvent: '2026-03-01T00:00:00Z' }],
        ['z', { activity: 'thinking' }],
      ])
    );
    expect(epoch.changedIds).toEqual(['a']);
    epoch.close();
    emit(sm, 'agent.w.status', { agentId: 'w', phase: 'running' });
    expect(Array.from(epoch.unknownChanges.keys())).toEqual(['x', 'z']);
  });

  it('unknownChanges leaves out an ID created live after its unknown delta', () => {
    const sm = newState();
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.x.status', { agentId: 'x', phase: 'running' });
    emit(sm, 'agent.x.created', makeAgent('x', { phase: 'stopped' }));
    expect(epoch.unknownChanges.size).toBe(0);
    expect(epoch.changedIds).toEqual(['x']);
    epoch.close();
  });

  it('close without seeding still ends the store epoch and stops recording', () => {
    const sm = newState();
    const epoch = new AgentSeedEpoch(sm);
    expect(openEpochs(sm)).toBe(1);
    epoch.close();
    expect(openEpochs(sm)).toBe(0);
    emit(sm, 'agent.n1.created', { id: 'n1', name: 'n1', projectId: 'p1', phase: 'running' });
    expect(epoch.changedIds).toEqual([]);
  });
});
