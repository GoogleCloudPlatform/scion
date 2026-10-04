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
import type { AgentSeedEpochState } from './agent-seed-epoch.js';

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

  it('sawChanges is set by an upsert, a create, a delete or an unknown-ID delta, until close', () => {
    const cases: Array<[string, (sm: StateManager) => void]> = [
      ['upsert', (sm) => emit(sm, 'agent.a.status', { phase: 'stopped' })],
      ['create', (sm) => emit(sm, 'agent.n.created', makeAgent('n'))],
      ['delete', (sm) => emit(sm, 'agent.a.deleted', { agentId: 'a' })],
      ['unknown', (sm) => emit(sm, 'agent.x.status', { agentId: 'x', phase: 'stopped' })],
    ];
    for (const [name, change] of cases) {
      const sm = newState();
      sm.seedAgents([makeAgent('a')]);
      const epoch = new AgentSeedEpoch(sm);
      expect(epoch.sawChanges, name).toBe(false);
      change(sm);
      expect(epoch.sawChanges, name).toBe(true);
      epoch.close();
    }
    const sm = newState();
    sm.seedAgents([makeAgent('a')]);
    const epoch = new AgentSeedEpoch(sm);
    epoch.close();
    emit(sm, 'agent.a.status', { phase: 'stopped' });
    expect(epoch.sawChanges).toBe(false);
  });

  it('deletedChanges lists the IDs deleted live, known or not, until close', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b')]);
    const epoch = new AgentSeedEpoch(sm);
    expect(epoch.deletedChanges).toEqual([]);
    emit(sm, 'agent.a.deleted', { agentId: 'a' });
    emit(sm, 'agent.x.deleted', { agentId: 'x' });
    emit(sm, 'agent.b.status', { phase: 'stopped' });
    expect(epoch.deletedChanges.sort()).toEqual(['a', 'x']);
    epoch.close();
    emit(sm, 'agent.b.deleted', { agentId: 'b' });
    expect(epoch.deletedChanges.sort()).toEqual(['a', 'x']);
  });

  it('seed reports the response rows it left out as deleted, before or during the request', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b'), makeAgent('c')]);
    emit(sm, 'agent.a.deleted', { agentId: 'a' });
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.b.deleted', { agentId: 'b' });
    const result = epoch.seed([makeAgent('a'), makeAgent('b'), makeAgent('c')], {
      partial: false,
    });
    epoch.close();
    expect(result.agents.map((a) => a.id)).toEqual(['c']);
    expect(result.dropped).toEqual(['a', 'b']);
  });

  it('seed does not report a row as dropped when the store holds its agent again after the delete', () => {
    // The state store ignores a live create for a deleted ID, so a store
    // that holds a tombstoned agent is stubbed here: the rule still keys
    // off whether the store holds the agent, not off the tombstone alone.
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b')]);
    emit(sm, 'agent.a.deleted', { agentId: 'a' });
    emit(sm, 'agent.b.deleted', { agentId: 'b' });
    const heldAgain = makeAgent('a');
    const state: AgentSeedEpochState = {
      beginSeedEpoch: () => sm.beginSeedEpoch(),
      seedAgents: (agents, options) => sm.seedAgents(agents, options),
      endSeedEpoch: (token) => sm.endSeedEpoch(token),
      getDeletedAgentIds: () => sm.getDeletedAgentIds(),
      getAgent: (id) => (id === 'a' ? heldAgain : sm.getAgent(id)),
      addEventListener: sm.addEventListener.bind(sm),
      removeEventListener: sm.removeEventListener.bind(sm),
    };
    const epoch = new AgentSeedEpoch(state);
    const result = epoch.seed([makeAgent('a'), makeAgent('b')], { partial: false });
    epoch.close();
    // Both stay off the result; only the agent the store no longer holds is dropped.
    expect(result.agents.map((x) => x.id)).toEqual([]);
    expect(result.dropped).toEqual(['b']);
  });

  it('a live create for a deleted agent is ignored, so a response row for it counts as dropped', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b')]);
    emit(sm, 'agent.a.deleted', { agentId: 'a' });
    emit(sm, 'agent.a.created', { ...makeAgent('a'), agentId: 'a' });
    expect(sm.getAgent('a')).toBeUndefined();
    const epoch = new AgentSeedEpoch(sm);
    const result = epoch.seed([makeAgent('a'), makeAgent('b')], { partial: false });
    epoch.close();
    expect(result.agents.map((x) => x.id)).toEqual(['b']);
    expect(result.dropped).toEqual(['a']);
  });

  it('a create for an agent deleted while the request is in flight is ignored and stays off the result', () => {
    const sm = newState();
    sm.seedAgents([makeAgent('a'), makeAgent('b')]);
    const epoch = new AgentSeedEpoch(sm);
    emit(sm, 'agent.a.deleted', { agentId: 'a' });
    emit(sm, 'agent.a.created', { ...makeAgent('a'), agentId: 'a' });
    expect(sm.getAgent('a')).toBeUndefined();
    const result = epoch.seed([makeAgent('b')], { partial: false, isMember: () => true });
    epoch.close();
    // The live create does not join: the agent stays deleted for list pages.
    expect(result.agents.map((x) => x.id)).toEqual(['b']);
    expect(result.liveCreated).toEqual([]);
    expect(result.dropped).toEqual([]);
  });

  it('sawResync is set by a live-connection resync while open, and only then', () => {
    const resync = (sm: StateManager): void => {
      sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
      sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    };
    const sm = newState();
    const epoch = new AgentSeedEpoch(sm);
    expect(epoch.sawResync).toBe(false);
    // A first connect is not a resync.
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    expect(epoch.sawResync).toBe(false);
    resync(sm);
    expect(epoch.sawResync).toBe(true);
    epoch.close();

    const later = new AgentSeedEpoch(sm);
    later.close();
    resync(sm);
    expect(later.sawResync).toBe(false);
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
