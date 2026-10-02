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
 * §7/§8 (perf/2385-sse-coalesce, P1a): `beginSeedEpoch()` records a
 * compacted, O(1)-per-ID summary of the deltas applied while a REST fetch
 * is in flight, so `seedAgents(list, {token, partial})` can re-apply them
 * after setting the REST objects — a stale REST snapshot must not clobber a
 * fresher SSE update that landed mid-fetch. `partial: true` merges instead
 * of replacing, so a compact seed cannot strip fields a fuller seed already
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

/** Two sticky activities, `working`, one other non-sticky value, and "no activity field at all" (`undefined`). */
const ACTIVITIES: ReadonlyArray<string | undefined> = [
  'working',
  'thinking',
  'waiting_for_input',
  'completed',
  undefined,
];

/** Emit one status delta: `undefined` means a delta that carries no `activity` field at all. */
function emitActivity(sm: StateManager, id: string, activity: string | undefined): void {
  emit(sm, `agent.${id}.status`, activity === undefined ? { phase: 'running' } : { activity });
}

/**
 * Independent oracle: seed `base` directly (no epoch, no buffering — the
 * agent already exists), then apply each of `deltas` immediately, one at a
 * time, through the public SSE path. This is "immediate sequential
 * application" by definition: every delta goes through the known-agent
 * merge path against a real base, in order.
 */
function sequentialActivity(
  base: Agent,
  deltas: ReadonlyArray<string | undefined>
): string | undefined {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  sm.seedAgents([base]);
  for (const activity of deltas) {
    emitActivity(sm, base.id, activity);
  }
  return sm.getAgent(base.id)?.activity as string | undefined;
}

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

  it('a delta for an ID not yet in state during an epoch survives the seed (the normal first-drain case)', () => {
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

  it('the pending entry is consumed — a later created event does not re-apply it', () => {
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

  it('an unknown-ID delta recorded mid-epoch still goes through sticky-activity/detail merge semantics at seed time', () => {
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

  it('a partial (compact-drain) seed with a recorded epoch delta keeps full fields and applies the delta', () => {
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

  it('seedAgents ends the epoch itself — a second call with the same token is a no-op', () => {
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

  it('a status delta for a tombstoned ID is dropped, not buffered — a later seed with that epoch does not see it', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a1.deleted', {});

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { phase: 'error' }); // must be dropped outright, not buffered

    // Even ignoring the tombstone-skip in seedAgents itself, there must be
    // no recorded delta to reapply — the ID was never buffered or recorded.
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('two epoch deltas with different detail fields, then seedAgents, equal immediate sequential application', () => {
    // Epoch path: both status deltas land for an ID not yet in state.agents
    // (the normal first-drain case — setScope cleared state.agents before
    // this drain's own fetch started), before the REST snapshot seeds it.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { detail: { message: 'm1' } });
    emit(sm, 'agent.a1.status', { detail: { currentTurns: 7 } });

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

    // Reference: the REST snapshot seeds first (no epoch involved), then the
    // same two deltas, in the same order, applied immediately to the now-
    // known agent — what "immediate sequential application" means once
    // there is a real base to merge against at every step.
    const sequential = new StateManager();
    sequential.setScope({ type: 'dashboard' });
    sequential.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent]);
    emit(sequential, 'agent.a1.status', { detail: { message: 'm1' } });
    emit(sequential, 'agent.a1.status', { detail: { currentTurns: 7 } });

    expect(sm.getAgent('a1')).toEqual(sequential.getAgent('a1'));
    // Spelled out, as in the W2 counterpart: `detail` is replaced wholesale
    // by the later delta, but the promoted top-level `message` field
    // survives since the later delta never carried a `message` key.
    expect(sm.getAgent('a1')?.detail).toEqual({ currentTurns: 7 });
    expect(sm.getAgent('a1')?.message).toBe('m1');
    expect(sm.getAgent('a1')?.currentTurns).toBe(7);
  });

  it('a seed-epoch replay does not let a stale sticky REST row overwrite newer live SSE state', () => {
    // The REST snapshot this epoch guards was fetched before the user
    // replied: by the time it comes back, live SSE has already carried the
    // agent from waiting_for_input (sticky) through thinking (not sticky,
    // not working/empty) to working. Replaying the epoch's recorded deltas
    // one at a time against the stale REST base must reproduce that same
    // live transition, not regress to the REST row's sticky activity.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.seedAgents([{ id: 'a1', name: 'A1', activity: 'waiting_for_input' } as Agent]);

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { activity: 'thinking' });
    emit(sm, 'agent.a1.status', { activity: 'working' });
    expect(sm.getAgent('a1')?.activity).toBe('working'); // live state, before the seed lands

    sm.seedAgents([{ id: 'a1', name: 'A1', activity: 'waiting_for_input' } as Agent], { token });

    expect(sm.getAgent('a1')?.activity).toBe('working');
  });

  it('epoch path: every 3-delta sequence for an unknown ID, over every base activity, equals immediate sequential application', () => {
    let checked = 0;
    for (const base of ACTIVITIES) {
      for (const x of ACTIVITIES) {
        for (const y of ACTIVITIES) {
          for (const z of ACTIVITIES) {
            const deltas = [x, y, z];
            const baseAgent = {
              id: 'u',
              name: 'U',
              ...(base !== undefined ? { activity: base } : {}),
            } as Agent;

            const sm = new StateManager();
            sm.setScope({ type: 'dashboard' });
            const token = sm.beginSeedEpoch();
            for (const activity of deltas) {
              emitActivity(sm, 'u', activity);
            }
            sm.seedAgents([baseAgent], { token });

            const want = sequentialActivity(baseAgent, deltas);
            expect(sm.getAgent('u')?.activity, `base=${base} seq=${deltas.join(',')}`).toBe(want);
            checked++;
          }
        }
      }
    }
    expect(checked).toBe(ACTIVITIES.length ** 4); // sanity: the full 5^4 grid ran, nothing skipped
  }, 60_000); // 5^4 = 625 StateManager instances; the default 5s test timeout is too tight under load

  it('a created event drained during an epoch is recorded before its buffered deltas, not after', () => {
    // Buffer two deltas for an unknown ID, open an epoch throughout, then
    // let "created" drain them — created's own activity must replay FIRST
    // against the REST base at seed time, with the buffered deltas on top,
    // matching the order live application used when created arrived.
    // Live order is created (completed), waiting_for_input, thinking,
    // working, which gives working. With created replayed *last* instead
    // (the bug this test catches), completed would be sticky when working
    // arrives, so the seed would yield completed.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.u.status', { activity: 'waiting_for_input' });
    emit(sm, 'agent.u.status', { activity: 'thinking' });
    emit(sm, 'agent.u.created', { name: 'U', activity: 'completed' });
    emit(sm, 'agent.u.status', { activity: 'working' });

    expect(sm.getAgent('u')?.activity).toBe('working'); // live state

    sm.seedAgents([{ id: 'u', name: 'U', activity: 'thinking' } as Agent], { token });

    expect(sm.getAgent('u')?.activity).toBe('working');
  });

  it('a delta that is a no-op against live state is still recorded into an open epoch', () => {
    // The REST row an epoch guards can be older than what the client
    // already had *before* the epoch even opened (replica lag, a stale
    // cache). Skipping epoch-recording for a delta just because it was a
    // no-op against the newer live state would silently drop it from a
    // later seed-time replay against that older, stale REST row.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.seedAgents([{ id: 'a1', name: 'A1', activity: 'thinking', message: 'new' } as Agent]);

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.status', { activity: 'thinking', message: 'new' }); // no-op against live state

    sm.seedAgents(
      [{ id: 'a1', name: 'A1', activity: 'waiting_for_input', message: 'old' } as Agent],
      { token }
    );

    expect(sm.getAgent('a1')?.activity).toBe('thinking');
    expect(sm.getAgent('a1')?.message).toBe('new');
  });

  it('a ports delta that is a no-op against live state is still recorded into an open epoch', () => {
    // Same reasoning as the status/created path above, extended to ports: a
    // REST row can carry an older exposedPorts than live state even when a
    // given ports delta was a no-op against that live state.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const port = { port: 8080, exposedAt: 't', exposedBy: 'u1' };
    sm.seedAgents([{ id: 'a1', name: 'A1', exposedPorts: [port] } as Agent]);

    const token = sm.beginSeedEpoch();
    emit(sm, 'agent.a1.ports', { ports: [{ ...port }] }); // no-op against live state (fresh array, same values)

    sm.seedAgents([{ id: 'a1', name: 'A1', exposedPorts: [] } as Agent], { token });

    expect(sm.getAgent('a1')?.exposedPorts).toEqual([port]);
  });

  describe('a TTL-expired buffered delta is not replayed by a later seed', () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it('matches live state once the buffer entry it was recorded from has expired', () => {
      // Exactly the sequence the design calls out: a status delta for an
      // unknown ID, recorded into both the 30s buffer and this still-open
      // epoch; more than 30s with nothing else touching the ID, so live
      // state drops it (see the buffer-expiry tests in
      // state-coalescing.test.ts); then "created"; then the seed. Before
      // the fix, the epoch kept its own copy past the buffer's expiry and
      // replayed it here — a field ('labels') that only the expired delta
      // ever set would resurface at seed time even though live state never
      // showed it after "created".
      const sm = new StateManager();
      sm.setScope({ type: 'dashboard' });

      const token = sm.beginSeedEpoch();
      emit(sm, 'agent.a1.status', { phase: 'error', labels: { env: 'stale' } });

      vi.advanceTimersByTime(30_000); // the buffered entry (and now the epoch's copy) expires

      emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
      // Live state already dropped the expired delta (per the buffer-expiry
      // tests in state-coalescing.test.ts).
      expect(sm.getAgent('a1')?.phase).toBe('running');
      expect(sm.getAgent('a1')?.labels).toBeUndefined();

      sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

      // The seeded state must agree with live state: no resurrected 'error'
      // phase, no resurrected 'labels' field from the expired delta.
      expect(sm.getAgent('a1')?.phase).toBe('running');
      expect(sm.getAgent('a1')?.labels).toBeUndefined();
    });

    it('an untokened seed that makes an id known cancels its stale pending-buffer timer, so later known-state epoch deltas are not wiped out from under it', () => {
      // A second way the buffer/epoch can disagree: an id becomes known not
      // through "created", but through a plain (untokened) seedAgents call
      // — every page-level seed today (agents.ts, project-detail.ts,
      // home.ts, chat.ts, agent-detail.ts, agent-graph.ts). Before this
      // fix, that path left the original buffered-phase timer running; if
      // it later fired, it deleted the whole epoch entry for the id,
      // including known-state deltas recorded into it *after* the id
      // became known — deltas live state had already applied.
      const sm = new StateManager();
      sm.setScope({ type: 'dashboard' });

      const token = sm.beginSeedEpoch();
      emit(sm, 'agent.a1.status', { phase: 'starting' }); // a1 unknown; buffered, a 30s timer starts.

      vi.advanceTimersByTime(5_000);
      // An untokened seed (a different, already-in-flight fetch) makes a1
      // known. No "created" event is involved.
      sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent]);

      vi.advanceTimersByTime(5_000); // 10s since the first delta.
      emit(sm, 'agent.a1.status', { phase: 'stopped' }); // known-state path now; recorded into the open epoch.
      expect(sm.getAgent('a1')?.phase).toBe('stopped'); // live state.

      vi.advanceTimersByTime(20_000); // 30s since the very first delta: the original timer, if still live, fires now.

      // The drain's own REST snapshot for this epoch, stale relative to live.
      sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token });

      // Must match live state ('stopped'), not regress to the stale REST
      // snapshot's 'running'.
      expect(sm.getAgent('a1')?.phase).toBe('stopped');
    });
  });
});
