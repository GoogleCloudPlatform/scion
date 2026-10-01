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
 * W3 (design doc §9): seed epochs.
 *
 * §7/§8 (perf/2385-sse-coalesce, P1a): `beginSeedEpoch()` records the
 * per-ID merged deltas applied while a REST fetch is in flight, so
 * `seedAgents(list, {token, partial})` can re-apply them after setting the
 * REST objects — a stale REST snapshot must not clobber a fresher SSE
 * update that landed mid-fetch. `partial: true` merges instead of
 * replacing, so a compact seed cannot strip fields a fuller seed already
 * recorded. Tombstoned IDs (a `deleted` event) are never resurrected by any
 * seed. A scope change invalidates open tokens outright.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Agent } from '../shared/types.js';

function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

/** Stand-in for EventSource, which happy-dom does not implement; setScope opens one. */
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

describe('W3 seed epoch', () => {
  it('an SSE delta during a drain survives the seed', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    // The REST fetch this epoch guards is "in flight" here. Meanwhile SSE
    // delivers a fresher update than the REST snapshot will carry.
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a1.status', { phase: 'error' });

    // The REST response lands, stale relative to the SSE update above.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });
    sm.endSeedEpoch(token);

    expect(sm.getAgent('a1')?.phase).toBe('error');
  });

  it('a compact seed keeps full fields the state already holds', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    // A full seed (e.g. from agents.ts) establishes full fields.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running', labels: { env: 'prod' } } as Agent]);

    // A later compact seed (e.g. from the graph drain) only carries a subset.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'stopped' } as Agent], { partial: true });

    expect(sm.getAgent('a1')?.labels).toEqual({ env: 'prod' });
    expect(sm.getAgent('a1')?.phase).toBe('stopped');
  });

  it('a non-partial seed replaces the object outright', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running', labels: { env: 'prod' } } as Agent]);
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'stopped' } as Agent]);

    expect(sm.getAgent('a1')?.labels).toBeUndefined();
  });

  it('a tombstoned agent is never resurrected by a plain seed', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a1.deleted', {});

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent]);

    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('a tombstoned agent is never resurrected within a seed epoch', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a1.deleted', {});

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });
    sm.endSeedEpoch(token);

    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('a delete recorded mid-epoch is not undone by a stale recorded delta for a different ID', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a2.created', { phase: 'running', name: 'A2' });
    emit(sm, 'agent.a2.status', { phase: 'stopped' });
    emit(sm, 'agent.a1.deleted', {});

    sm.seedAgents(
      [
        { id: 'a1', name: 'A1', phase: 'running' } as Agent,
        { id: 'a2', name: 'A2', phase: 'running' } as Agent,
      ],
      { token }
    );
    sm.endSeedEpoch(token);

    expect(sm.getAgent('a1')).toBeUndefined();
    expect(sm.getAgent('a2')?.phase).toBe('stopped');
  });

  it('a scope change invalidates the token: the seed becomes a full no-op', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const token = sm.beginSeedEpoch();

    sm.setScope({ type: 'brokers-list' }); // actual scope change invalidates the token

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('endSeedEpoch is idempotent and a no-op for an already-invalidated token', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const token = sm.beginSeedEpoch();
    sm.setScope({ type: 'brokers-list' });

    expect(() => sm.endSeedEpoch(token)).not.toThrow();
    expect(() => sm.endSeedEpoch(token)).not.toThrow();
  });

  it('a plain seedAgents call with no token is unaffected by an open epoch for a different token', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.beginSeedEpoch(); // unrelated open epoch

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent]);

    expect(sm.getAgent('a1')?.phase).toBe('running');
  });

  it('B2 (round 1 review): a delta for an ID not yet in state during an epoch survives the seed (the normal first-drain case)', () => {
    // setScope clears state.agents, so every SSE delta that arrives during
    // the drain that follows is for an ID not yet known — this is the
    // common case W3's original "SSE delta during a drain" test did not
    // actually cover (it emitted "created" first, which made the ID known
    // before the status delta).
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { phase: 'error' }); // a1 is still unknown to state.agents

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    expect(sm.getAgent('a1')?.phase).toBe('error');
  });

  it('B2: the pending entry is consumed — a later created event does not re-apply it', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { phase: 'error' });
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });
    expect(sm.getAgent('a1')?.phase).toBe('error'); // applied once, as above

    // If the hub still sends the "created" event after this, it must not
    // re-apply the same buffered delta on top of whatever the create says.
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    expect(sm.getAgent('a1')?.phase).toBe('running');
  });

  it('B2: an unknown-ID delta recorded mid-epoch still goes through sticky-activity/detail merge semantics at seed time', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    // "working" would normally overwrite activity, but is suppressed when
    // the base it merges against has a sticky activity — exactly the rule
    // mergeAgentDelta shares with handleAgentEvent's known-agent path.
    emit(sm, 'agent.a1.status', { activity: 'working' });

    sm.seedAgents(
      [{ id: 'a1', name: 'A1', phase: 'running', activity: 'waiting_for_input' } as Agent],
      { token }
    );

    expect(sm.getAgent('a1')?.activity).toBe('waiting_for_input');
  });

  it('N2 (round 2 review): a partial (compact-drain) seed with a recorded epoch delta keeps full fields and applies the delta', () => {
    // §8's compact drain calls seedAgents(result.agents, {token, partial:
    // true}) — a combination not otherwise exercised together.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    // A fuller seed already established full fields for this ID.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running', labels: { env: 'prod' } } as Agent]);

    const token = sm.beginSeedEpoch();
    // SSE delivers a fresher update during the compact drain's fetch.
    emit(sm, 'agent.a1.status', { phase: 'error' });

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'stopped' } as Agent], {
      token,
      partial: true,
    });

    // The compact seed's own phase is superseded by the SSE update recorded
    // in the epoch, same as a non-partial seed; the full field the compact
    // seed never carries (labels) is kept, same as any partial seed.
    expect(sm.getAgent('a1')?.phase).toBe('error');
    expect(sm.getAgent('a1')?.labels).toEqual({ env: 'prod' });
  });

  it('N2 (round 1 review): seedAgents ends the epoch itself — a second call with the same token is a no-op', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    // The epoch is already closed; a second seed with the same token must
    // not apply (the token no longer names an open epoch).
    sm.seedAgents([{ id: 'a2', name: 'A2', phase: 'running' } as Agent], { token });

    expect(sm.getAgent('a1')?.phase).toBe('running');
    expect(sm.getAgent('a2')).toBeUndefined();
  });

  it('N1 (round 1 review): a status delta for a tombstoned ID is dropped, not buffered — a later seed with that epoch does not see it', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a1.deleted', {});

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { phase: 'error' }); // must be dropped outright (N1)

    // Even ignoring the tombstone-skip in seedAgents itself, there must be
    // no recorded delta to reapply — the ID was never buffered or recorded.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    expect(sm.getAgent('a1')).toBeUndefined();
  });
});
