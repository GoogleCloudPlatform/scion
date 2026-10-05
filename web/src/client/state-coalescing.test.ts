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
 * W2 (design doc §9): SSE coalescing fuzz, resync edges and `sseConnected`.
 *
 * §7 (perf/2385-sse-coalesce, P1a): `handleAgentEvent` keeps applying every
 * delta immediately, but now skips the mutation and every notification when
 * the merged object is shallow-equal to what state already holds (`detail`
 * compared by value), and coalesces `agent-created` / `agents-changed` /
 * `agents-updated` into one flush per animation frame (or a 100ms fallback).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager, type AgentsChangedDetail, type ViewScope } from './state.js';
import type { Agent, AgentDetail, ExposedPort } from '../shared/types.js';

/** Feed a subject/data pair through the SSE update path. */
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

/** Deterministic PRNG (mulberry32) so a failing fuzz run is reproducible. */
function mulberry32(seed: number): () => number {
  let a = seed;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

type FuzzEvent =
  | { kind: 'created'; id: string; phase: string; name: string; capabilityActions?: string[] }
  | {
      kind: 'status';
      id: string;
      phase?: string;
      activity?: string;
      lastActivityEvent?: string;
      detail?: Partial<AgentDetail>;
    }
  | { kind: 'ports'; id: string; ports: ExposedPort[] }
  | { kind: 'deleted'; id: string };

const IDS = Array.from({ length: 24 }, (_, i) => `a${i}`);
const PHASES = ['running', 'stopped', 'error', 'stopping'];
const ACTIVITIES = ['working', '', 'thinking', 'waiting_for_input', 'completed'];
const STICKY_ACTIVITIES = new Set(['waiting_for_input', 'completed', 'limits_exceeded']);

function genEvents(count: number, seed: number): FuzzEvent[] {
  const rand = mulberry32(seed);
  const pick = <T>(arr: T[]): T => arr[Math.floor(rand() * arr.length)] as T;
  const events: FuzzEvent[] = [];
  for (let i = 0; i < count; i++) {
    const id = pick(IDS);
    const roll = rand();
    if (roll < 0.15) {
      const ev: FuzzEvent = { kind: 'created', id, phase: pick(PHASES), name: `Agent ${id}` };
      // Fuzz capability preservation under the equality skip, not just the
      // two hand-seeded IDs.
      if (rand() < 0.3) ev.capabilityActions = ['stop', 'restart'];
      events.push(ev);
    } else if (roll < 0.25) {
      events.push({ kind: 'deleted', id });
    } else if (roll < 0.35) {
      events.push({
        kind: 'ports',
        id,
        ports: [{ port: Math.floor(rand() * 65535) } as ExposedPort],
      });
    } else {
      const ev: FuzzEvent = { kind: 'status', id };
      if (rand() < 0.7) ev.phase = pick(PHASES);
      if (rand() < 0.7) ev.activity = pick(ACTIVITIES);
      if (rand() < 0.3) ev.lastActivityEvent = `evt-${i}`;
      if (rand() < 0.2) {
        // Partial, not always both fields together: a buffered/recorded
        // delta whose `detail` differs in shape from the previous one for
        // the same ID is exactly what `promoteDetailFields` must handle
        // correctly (§7) — always pairing message+currentTurns would never
        // exercise a later delta dropping a field the earlier one set.
        const detail: Partial<AgentDetail> = {};
        if (rand() < 0.5) detail.message = `m${i}`;
        if (rand() < 0.5 || Object.keys(detail).length === 0) detail.currentTurns = i % 7;
        ev.detail = detail;
      }
      events.push(ev);
    }
  }
  return events;
}

function fuzzEventToDelta(ev: FuzzEvent): Partial<Agent> {
  switch (ev.kind) {
    case 'created':
      return {
        phase: ev.phase,
        name: ev.name,
        ...(ev.capabilityActions ? { _capabilities: { actions: ev.capabilityActions } } : {}),
      } as Partial<Agent>;
    case 'status': {
      const delta: Partial<Agent> = {};
      if (ev.phase !== undefined) delta.phase = ev.phase as Agent['phase'];
      if (ev.activity !== undefined) delta.activity = ev.activity as Agent['activity'];
      if (ev.lastActivityEvent !== undefined) delta.lastActivityEvent = ev.lastActivityEvent;
      if (ev.detail !== undefined) delta.detail = ev.detail as AgentDetail;
      return delta;
    }
    default:
      return {};
  }
}

/**
 * Mirrors `promoteDetailFields` in state.ts (not exported, so duplicated
 * for this independent reducer).
 */
function promoteDetailFieldsRef(delta: Partial<Agent>): Partial<Agent> {
  const detail = delta.detail as AgentDetail | undefined;
  if (!detail) return delta;
  const promoted: Partial<Agent> = { ...delta };
  if (detail.message) (promoted as Record<string, unknown>).message = detail.message;
  if (detail.currentTurns !== undefined) {
    (promoted as Record<string, unknown>).currentTurns = detail.currentTurns;
  }
  if (detail.currentModelCalls !== undefined) {
    (promoted as Record<string, unknown>).currentModelCalls = detail.currentModelCalls;
  }
  if (detail.startedAt) (promoted as Record<string, unknown>).startedAt = detail.startedAt;
  return promoted;
}

/**
 * Apply one raw delta to a real base via the known-agent merge semantics
 * (sticky activity, then `promoteDetailFieldsRef`). This — one delta at a
 * time, each against whatever base the previous step produced — is the
 * actual definition of "immediate sequential application" the fuzz checks
 * against, not an implementation shortcut.
 */
function applyKnown(base: Agent, rawDelta: Partial<Agent>, id: string): Agent {
  let delta: Partial<Agent> = { ...rawDelta };
  const incomingActivity = delta.activity as string | undefined;
  if (
    incomingActivity !== undefined &&
    (incomingActivity === 'working' || incomingActivity === '') &&
    base.activity &&
    STICKY_ACTIVITIES.has(base.activity)
  ) {
    delete delta.activity;
  }
  delta = promoteDetailFieldsRef(delta);
  const updated = { ...base, ...delta, id } as Agent;
  if (!delta._capabilities && base._capabilities) {
    updated._capabilities = base._capabilities;
  }
  return updated;
}

/**
 * Independent, incremental reference model mirroring the documented merge
 * semantics (sticky activity, detail promotion, capability preservation,
 * buffering of early deltas, tombstones) with NO equality-skip and NO
 * coalescing. `applyOne` lets a run be checked against production at many
 * points during a single pass, not only once at the end.
 *
 * A buffered ID's deltas are kept raw, in arrival order, and replayed one at
 * a time through `applyKnown` once `created` supplies a base — the
 * definition of sequential application: apply each delta, in order, to
 * whatever base the previous step produced. This model is written
 * independently of `state.ts` (it does not import or call anything from
 * it), so a bug that is only in `state.ts`'s own logic — including one in
 * how it replays buffered/epoch deltas — still shows up as a mismatch here.
 */
class ReferenceModel {
  readonly agents = new Map<string, Agent>();
  private readonly pendingDeltas = new Map<string, Partial<Agent>[]>();
  // Mirrors state.deletedAgentIds: never cleared per-ID, only by setScope.
  private readonly deletedIds = new Set<string>();

  applyOne(ev: FuzzEvent): void {
    if (ev.kind === 'deleted') {
      this.agents.delete(ev.id);
      this.pendingDeltas.delete(ev.id);
      this.deletedIds.add(ev.id);
      return;
    }
    if (ev.kind === 'ports') {
      const existing = this.agents.get(ev.id);
      if (existing) {
        this.agents.set(ev.id, { ...existing, exposedPorts: ev.ports } as Agent);
      }
      return;
    }

    const existing = this.agents.get(ev.id);
    const isCreated = ev.kind === 'created';
    if (!existing && !isCreated) {
      // A status/ports delta for an ID already known to be deleted (and
      // not yet recreated) is dropped outright, not buffered.
      if (this.deletedIds.has(ev.id)) return;
      const list = this.pendingDeltas.get(ev.id) ?? [];
      list.push(fuzzEventToDelta(ev));
      this.pendingDeltas.set(ev.id, list);
      return;
    }

    if (isCreated) {
      // ptone/scion#2886: a delete is terminal for its ID, so a `created`
      // replayed after the tombstone (an SSE redelivery or a reordered
      // event) is dropped. A recreated agent gets a new ID.
      if (this.deletedIds.has(ev.id)) return;
      let base = applyKnown(existing ?? ({} as Agent), fuzzEventToDelta(ev), ev.id);
      const buffered = this.pendingDeltas.get(ev.id);
      this.pendingDeltas.delete(ev.id);
      if (buffered) {
        for (const raw of buffered) {
          base = applyKnown(base, raw, ev.id);
        }
      }
      this.agents.set(ev.id, base);
      return;
    }

    this.agents.set(ev.id, applyKnown(existing as Agent, fuzzEventToDelta(ev), ev.id));
  }
}

/** Convenience wrapper over `ReferenceModel` for an all-at-once comparison. */
function referenceApply(events: FuzzEvent[]): Map<string, Agent> {
  const model = new ReferenceModel();
  for (const ev of events) model.applyOne(ev);
  return model.agents;
}

let rafCallbacks: FrameRequestCallback[];

beforeEach(() => {
  vi.useFakeTimers();
  rafCallbacks = [];
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    rafCallbacks.push(cb);
    return rafCallbacks.length;
  });
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** Apply a fuzz event to a live StateManager via the SSE update path. */
function applyEvent(sm: StateManager, ev: FuzzEvent): void {
  switch (ev.kind) {
    case 'created': {
      const data: Record<string, unknown> = { phase: ev.phase, name: ev.name };
      if (ev.capabilityActions) data._capabilities = { actions: ev.capabilityActions };
      emit(sm, `agent.${ev.id}.created`, data);
      break;
    }
    case 'deleted':
      emit(sm, `agent.${ev.id}.deleted`, {});
      break;
    case 'ports':
      emit(sm, `agent.${ev.id}.ports`, { ports: ev.ports });
      break;
    case 'status': {
      const data: Record<string, unknown> = {};
      if (ev.phase !== undefined) data.phase = ev.phase;
      if (ev.activity !== undefined) data.activity = ev.activity;
      if (ev.lastActivityEvent !== undefined) data.lastActivityEvent = ev.lastActivityEvent;
      if (ev.detail !== undefined) data.detail = ev.detail;
      emit(sm, `agent.${ev.id}.status`, data);
      break;
    }
  }
}

/** Two sticky activities, `working`, one other non-sticky value, and "no activity field at all" (`undefined`). */
const STICKY_PROBE_ACTIVITIES: ReadonlyArray<string | undefined> = [
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
 * Independent oracle: `created` first (with `createdActivity`, if any), then
 * apply each of `deltas` immediately, one at a time, through the public SSE
 * path — i.e. the agent already exists for every one of them. This is
 * "immediate sequential application" by definition.
 */
function sequentialCreatedActivity(
  createdActivity: string | undefined,
  deltas: ReadonlyArray<string | undefined>
): string | undefined {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  emit(
    sm,
    'agent.u.created',
    createdActivity === undefined ? { name: 'U' } : { name: 'U', activity: createdActivity }
  );
  for (const activity of deltas) {
    emitActivity(sm, 'u', activity);
  }
  return sm.getAgent('u')?.activity as string | undefined;
}

describe('W2 coalescing fuzz (10k random events)', () => {
  it('final state equals immediate application, with no per-event notify', () => {
    const events = genEvents(10_000, 0xc0ffee);
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const updatedSpy = vi.fn();
    sm.addEventListener('agents-updated', updatedSpy);

    for (const ev of events) {
      applyEvent(sm, ev);
    }
    // Nothing has flushed yet: state mutation is immediate, notification is not.
    expect(updatedSpy).not.toHaveBeenCalled();

    // Exactly one flush for the whole run (the 100ms fallback; rAF is stubbed
    // to capture callbacks without invoking them, simulating a hidden tab).
    vi.advanceTimersByTime(100);

    expect(updatedSpy).toHaveBeenCalledTimes(1);

    const expected = referenceApply(events);
    const actual = new Map(sm.getAgents().map((a) => [a.id, a]));
    expect(actual.size).toBe(expected.size);
    for (const [id, agent] of expected) {
      expect(actual.get(id)).toEqual(agent);
    }
  }, 60_000); // 10k-event pass; the default 5s test timeout is too tight under load

  it('final state equals immediate application at every periodic checkpoint, not just at the end', () => {
    // The test above compares only once, after all 10,000 events. With a
    // fixed 24-ID pool and ~400 events per ID, a transient divergence for
    // one ID (e.g. a dropped promoted `message` field, the
    // promoteDetailFields bug the partial-detail generator above targets)
    // gets silently overwritten by a later event for that same ID long
    // before the run ends, so that test alone could never catch it.
    // Checking every 25 events — confirmed against both seeds below by
    // stashing state.ts back to the pre-fix commit and re-running (see the
    // gs report for the exact failing checkpoint/seed) — does.
    const CHECK_EVERY = 25;
    for (const seed of [0xc0ffee, 1234567]) {
      const events = genEvents(10_000, seed);
      const sm = new StateManager();
      sm.setScope({ type: 'dashboard' });
      const model = new ReferenceModel();

      for (let i = 0; i < events.length; i++) {
        const ev = events[i] as FuzzEvent;
        applyEvent(sm, ev);
        model.applyOne(ev);
        if ((i + 1) % CHECK_EVERY !== 0) continue;

        const actual = new Map(sm.getAgents().map((a) => [a.id, a]));
        expect(actual.size, `seed ${seed}, event ${i + 1}`).toBe(model.agents.size);
        for (const [id, agent] of model.agents) {
          expect(actual.get(id), `seed ${seed}, event ${i + 1}, id ${id}`).toEqual(agent);
        }
      }
    }
  }, 60_000); // two 10k-event passes with checkpoints; the default 5s test timeout is too tight here

  it('10k-event fuzz with interleaved rAF/timeout flush points, verified at every flush', () => {
    // The previous version of this test applied all 10,000 events and then
    // flushed once, which made "at most one notify per flush" trivially
    // true (there was only one flush) and never exercised the rAF path or
    // an intermediate flush's coverage. This version flushes many times
    // during the run, alternately via the captured rAF callback and via the
    // 100ms fallback, and checks properties (a)-(d) at every flush.
    const events = genEvents(10_000, 1234567);
    const flushPick = mulberry32(2468);
    const batchPick = mulberry32(13579);

    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    // Seed capability-bearing agents before the fuzzed stream runs, so the
    // equality skip's capability-preservation path is exercised by fuzzed
    // status deltas that omit _capabilities, not just by construction.
    emit(sm, 'agent.cap-1.created', {
      phase: 'running',
      name: 'Cap1',
      _capabilities: { actions: ['stop'] },
    });
    emit(sm, 'agent.cap-2.created', {
      phase: 'running',
      name: 'Cap2',
      _capabilities: { actions: ['stop'] },
    });
    vi.advanceTimersByTime(100); // flush the seed so it isn't counted below
    rafCallbacks.length = 0;

    const changedSpy = vi.fn<(e: Event) => void>();
    const updatedSpy = vi.fn();
    sm.addEventListener('agents-changed', changedSpy as EventListener);
    sm.addEventListener('agents-updated', updatedSpy);

    let prevSnapshot = new Map<string, Agent>(sm.getAgents().map((a) => [a.id, a]));
    let flushCount = 0;
    let flushesWithNonEmptyUnknown = 0;
    // Mirrors state.deletedAgentIds: permanent within one scope (cleared
    // below on each periodic setScope, mirroring setScope's own clear),
    // used the same way production does to decide whether a status/ports
    // delta for an absent ID would be buffered as unknown at all.
    let deletedIdsShadow = new Set<string>();

    const verifyFlush = (batchEvents: FuzzEvent[]): void => {
      const before = prevSnapshot;

      // Expected dirty.unknown for this window: an ID gets added the moment
      // a non-created, non-ports delta arrives for it while it is absent
      // from `before` and not tombstoned; a later `created` or `deleted`
      // for that same ID within the window resolves it out again (mirrors
      // recordUnknownDirty/dirty.unknown.delete).
      const expectedUnknown = new Set<string>();
      const knownThisWindow = new Set<string>(before.keys());
      for (const ev of batchEvents) {
        if (ev.kind === 'deleted') {
          knownThisWindow.delete(ev.id);
          deletedIdsShadow.add(ev.id);
          expectedUnknown.delete(ev.id);
        } else if (ev.kind === 'created') {
          // A created for a tombstoned ID is dropped (ptone/scion#2886).
          if (!deletedIdsShadow.has(ev.id)) knownThisWindow.add(ev.id);
          expectedUnknown.delete(ev.id);
        } else if (ev.kind === 'status') {
          // Ports events for an absent ID are dropped outright (accepted
          // deviation 2) — never buffered, never recorded in
          // dirty.unknown — so only "status" deltas populate this set.
          if (!knownThisWindow.has(ev.id) && !deletedIdsShadow.has(ev.id)) {
            expectedUnknown.add(ev.id);
          }
        }
      }

      // Trigger exactly one flush: whichever mechanism is due is already
      // pending (scheduleFlush always arms both), so either fires it.
      if (flushPick() < 0.5) {
        const cb = rafCallbacks.shift();
        expect(cb).toBeDefined();
        cb?.(0);
      } else {
        vi.advanceTimersByTime(100);
      }
      flushCount++;

      // (a) exactly one of each notify for this flush.
      expect(changedSpy).toHaveBeenCalledTimes(flushCount);
      expect(updatedSpy).toHaveBeenCalledTimes(flushCount);

      const detail = (
        changedSpy.mock.calls[flushCount - 1]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>
      ).detail.data;

      const after = new Map(sm.getAgents().map((a) => [a.id, a]));
      const allIds = new Set([...before.keys(), ...after.keys(), ...expectedUnknown]);

      const expectedUpserted = new Set<string>();
      const removedThisWindow = new Set<string>();
      const untouched = new Set<string>();
      for (const id of allIds) {
        if (after.has(id) && before.get(id) !== after.get(id)) {
          expectedUpserted.add(id);
        } else if (before.has(id) && !after.has(id)) {
          removedThisWindow.add(id);
        } else if (!expectedUnknown.has(id)) {
          // Same reference present in both (or absent from both), and not
          // buffered as unknown this window: nothing should report it.
          untouched.add(id);
        }
      }

      // (b) two-directional, including the unknown set, not just "every
      // real change is covered".
      expect(new Set(detail.upserted)).toEqual(expectedUpserted);
      expect(new Set(detail.unknown.keys())).toEqual(expectedUnknown);
      if (expectedUnknown.size > 0) {
        flushesWithNonEmptyUnknown++;
      }
      for (const id of removedThisWindow) {
        expect(detail.deleted).toContain(id); // deleted ⊇ IDs actually removed
      }
      for (const id of untouched) {
        expect(detail.upserted).not.toContain(id);
        expect(detail.deleted).not.toContain(id);
        expect(detail.unknown.has(id)).toBe(false);
      }

      // (d) after this flush, advancing the OTHER mechanism fires nothing
      // further — no double flush between rAF and the 100ms fallback.
      vi.advanceTimersByTime(100);
      const leftoverRaf = rafCallbacks.shift();
      leftoverRaf?.(0);
      expect(changedSpy).toHaveBeenCalledTimes(flushCount);
      expect(updatedSpy).toHaveBeenCalledTimes(flushCount);

      prevSnapshot = after;
    };

    // Tombstones are permanent within one scope, so with a fixed 24-ID pool
    // every ID is deleted at least once within the first ~5% of a
    // 10k-event run, and the exact-unknown check (b) above goes untested
    // for the rest. Alternating the scope every ~500 events clears
    // deletedAgentIds (and state.agents) the same way a real navigation
    // would, giving the fuzz repeated fresh windows instead of one.
    const scopes: ViewScope[] = [{ type: 'dashboard' }, { type: 'project', projectId: 'p-fuzz' }];
    let scopeIndex = 0;
    let eventsSinceScopeChange = 0;
    const SCOPE_RESET_INTERVAL = 500;

    let i = 0;
    while (i < events.length) {
      const batchSize = 1 + Math.floor(batchPick() * 8);
      const batchEvents: FuzzEvent[] = [];
      for (let b = 0; b < batchSize && i < events.length; b++, i++) {
        const ev = events[i] as FuzzEvent;
        batchEvents.push(ev);
        applyEvent(sm, ev);
      }
      eventsSinceScopeChange += batchEvents.length;
      // Only flush if something was actually dirtied by this batch — an
      // all-no-op batch (e.g. every event targeting a just-tombstoned ID)
      // schedules nothing.
      if (rafCallbacks.length > 0) {
        verifyFlush(batchEvents);
      }

      if (eventsSinceScopeChange >= SCOPE_RESET_INTERVAL) {
        eventsSinceScopeChange = 0;
        scopeIndex = (scopeIndex + 1) % scopes.length;

        sm.setScope(scopes[scopeIndex] as ViewScope); // actual scope change

        // setScope clears state.agents and deletedAgentIds; the fuzz's own
        // shadow bookkeeping must follow, or every subsequent window's
        // expected sets would be computed against stale state.
        prevSnapshot = new Map();
        deletedIdsShadow = new Set<string>();
      }
    }

    expect(flushCount).toBeGreaterThan(50); // sanity: genuinely interleaved, not one giant flush
    // The exact-unknown check in (b) must stay exercised throughout the
    // run, not just in an opening window before every one of a small fixed
    // ID pool gets permanently tombstoned.
    expect(flushesWithNonEmptyUnknown).toBeGreaterThanOrEqual(200);
  }, 60_000); // thousands of interleaved flush points; the default 5s test timeout is too tight here

  it('setScope discards a pending dirty set — no stale agents-changed fires in the new generation', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    rafCallbacks.length = 0; // drop the already-fired seed flush's stale rAF handle

    const changedSpy = vi.fn();
    sm.addEventListener('agents-changed', changedSpy);

    emit(sm, 'agent.a1.status', { phase: 'stopped' }); // dirties a1, arms a flush
    expect(rafCallbacks.length).toBe(1); // a flush is pending

    const genBefore = sm.scopeGeneration;
    sm.setScope({ type: 'brokers-list' }); // actual scope change
    expect(sm.scopeGeneration).toBe(genBefore + 1);

    // The pending flush must not fire for the old generation's dirty set,
    // whichever mechanism something still tries to use to trigger it.
    vi.advanceTimersByTime(1000);
    const leftover = rafCallbacks.shift();
    leftover?.(0);
    expect(changedSpy).not.toHaveBeenCalled();
  });

  it('unchanged agents stay === across a flush that does not touch them', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    emit(sm, 'agent.a2.created', { phase: 'running', name: 'A2' });
    vi.advanceTimersByTime(100);

    const a1Before = sm.getAgent('a1');
    // a2 gets a genuine update; a1 gets a byte-identical replay of its own data.
    emit(sm, 'agent.a1.status', { phase: 'running' });
    emit(sm, 'agent.a2.status', { phase: 'stopped' });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')).toBe(a1Before);
    expect(sm.getAgent('a2')?.phase).toBe('stopped');
  });

  it('a shallow-equal replay is a true no-op: no dirty entry, no flush scheduled', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1', activity: 'thinking' });
    vi.advanceTimersByTime(100);

    const updatedSpy = vi.fn();
    sm.addEventListener('agents-updated', updatedSpy);
    emit(sm, 'agent.a1.status', { phase: 'running', activity: 'thinking' });

    // No flush was scheduled at all: even the 100ms fallback fires nothing.
    vi.advanceTimersByTime(1000);
    expect(updatedSpy).not.toHaveBeenCalled();
  });

  it('a changed `detail` (compared by value) is not mistaken for a no-op', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', {
      phase: 'running',
      name: 'A1',
      detail: { toolName: 'bash' },
    });
    vi.advanceTimersByTime(100);

    emit(sm, 'agent.a1.status', { phase: 'running', detail: { toolName: 'python' } });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.detail).toEqual({ toolName: 'python' });
  });

  it('a detail with a different key set is not a no-op, even when every value involved is undefined', () => {
    // shallowObjectEqual (used by agentDetailEqual) must compare key SETS,
    // not just key counts: `{message: undefined}` and `{currentTurns:
    // undefined}` both have exactly one own key, and reading the other
    // object's missing key returns `undefined` on both sides, so a
    // count-only check (or a check that indexes `a`'s keys into `b` without
    // first confirming `b` actually has that key) would wrongly call them
    // equal.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', {
      phase: 'running',
      name: 'A1',
      detail: { message: undefined },
    });
    vi.advanceTimersByTime(100);
    const before = sm.getAgent('a1');
    expect(before?.detail).toEqual({ message: undefined });

    const changedSpy = vi.fn();
    sm.addEventListener('agents-changed', changedSpy);
    emit(sm, 'agent.a1.status', { detail: { currentTurns: undefined } });
    vi.advanceTimersByTime(100);

    expect(changedSpy).toHaveBeenCalledTimes(1);
    const after = sm.getAgent('a1');
    expect(after).not.toBe(before);
    expect(after?.detail && 'message' in after.detail).toBe(false);
    expect(after?.detail && 'currentTurns' in after.detail).toBe(true);
  });

  it('a byte-identical ports replay (fresh array, same values) is a true no-op', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    const port = { port: 8080, exposedAt: '2026-01-01T00:00:00Z', exposedBy: 'u1' };
    emit(sm, 'agent.a1.ports', { ports: [port] });
    vi.advanceTimersByTime(100);
    const before = sm.getAgent('a1');

    const updatedSpy = vi.fn();
    sm.addEventListener('agents-updated', updatedSpy);
    // A fresh array, but every field is identical by value.
    emit(sm, 'agent.a1.ports', { ports: [{ ...port }] });

    vi.advanceTimersByTime(1000);
    expect(updatedSpy).not.toHaveBeenCalled();
    expect(sm.getAgent('a1')).toBe(before);
  });

  it('a genuine ports change (different value) still dirties and replaces the object', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    emit(sm, 'agent.a1.ports', { ports: [{ port: 8080, exposedAt: 't', exposedBy: 'u1' }] });
    vi.advanceTimersByTime(100);
    const before = sm.getAgent('a1');

    emit(sm, 'agent.a1.ports', { ports: [{ port: 9090, exposedAt: 't', exposedBy: 'u1' }] });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')).not.toBe(before);
    expect(sm.getAgent('a1')?.exposedPorts).toEqual([
      { port: 9090, exposedAt: 't', exposedBy: 'u1' },
    ]);
  });

  it('a created event after a delete in the same flush is dropped: the tombstone wins (ptone/scion#2886)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);

    emit(sm, 'agent.a1.deleted', {});
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' }); // replayed after the delete
    vi.advanceTimersByTime(100);

    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;
    expect(detail.upserted).not.toContain('a1');
    expect(detail.deleted).toContain('a1');
    expect(sm.getAgent('a1')).toBeUndefined();
    expect(sm.getDeletedAgentIds().has('a1')).toBe(true);
  });

  it('a created event for a new ID reusing a deleted agent name is still added (ptone/scion#2886)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'worker' });
    vi.advanceTimersByTime(100);

    emit(sm, 'agent.a1.deleted', {});
    emit(sm, 'agent.a2.created', { phase: 'running', name: 'worker' });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')).toBeUndefined();
    expect(sm.getAgent('a2')?.name).toBe('worker');
  });

  it('a restore created (restoredAt) after a delete clears the tombstone and re-adds the agent (ptone/scion#2951)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    emit(sm, 'agent.a1.deleted', {});
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')).toBeUndefined();

    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);
    const createdSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agent-created', createdSpy as EventListener);

    emit(sm, 'agent.a1.created', {
      phase: 'stopped',
      name: 'A1',
      restoredAt: '2026-10-05T01:00:00Z',
    });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.phase).toBe('stopped');
    expect(sm.getAgent('a1')).not.toHaveProperty('restoredAt');
    expect(sm.getDeletedAgentIds().has('a1')).toBe(false);
    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;
    expect(detail.upserted).toContain('a1');
    expect(detail.deleted).not.toContain('a1');
    expect(createdSpy).toHaveBeenCalledTimes(1);

    // Live again: a later status delta applies.
    emit(sm, 'agent.a1.status', { phase: 'running' });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.phase).toBe('running');
  });

  it('a delete then a restore created in the same flush leaves the agent present (ptone/scion#2951)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);
    emit(sm, 'agent.a1.deleted', {});
    emit(sm, 'agent.a1.created', {
      phase: 'stopped',
      name: 'A1',
      restoredAt: '2026-10-05T01:00:00Z',
    });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.phase).toBe('stopped');
    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;
    expect(detail.upserted).toContain('a1');
    expect(detail.deleted).not.toContain('a1');
  });

  it('a replayed unmarked created after a delete stays hidden; only the marked one restores (ptone/scion#2951)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    emit(sm, 'agent.a1.deleted', {});
    vi.advanceTimersByTime(100);

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' }); // stale replay
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1', restoredAt: '' }); // empty marker
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')).toBeUndefined();
    expect(sm.getDeletedAgentIds().has('a1')).toBe(true);
  });
});

describe('W2: tombstoned IDs are dropped outright, never buffered as unknown', () => {
  it('a status delta after a delete is dropped: no buffer, no dirty.unknown, no flush', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    emit(sm, 'agent.a1.deleted', {});
    vi.advanceTimersByTime(100); // flush the delete itself before observing the dropped delta
    rafCallbacks.length = 0; // drop the delete flush's own stale rAF handle

    const updatedSpy = vi.fn();
    sm.addEventListener('agents-updated', updatedSpy);
    emit(sm, 'agent.a1.status', { phase: 'error' });

    // No flush was scheduled at all for the dropped delta.
    expect(rafCallbacks.length).toBe(0);
    vi.advanceTimersByTime(1000);
    expect(updatedSpy).not.toHaveBeenCalled();
    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('a status delta and a replayed created after a delete both stay dropped (ptone/scion#2886)', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    emit(sm, 'agent.a1.deleted', {});
    emit(sm, 'agent.a1.status', { phase: 'error' }); // dropped, not buffered
    const pending = (sm as unknown as { pendingAgentDeltas: Map<string, unknown> })
      .pendingAgentDeltas;
    expect(pending.has('a1')).toBe(false);

    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' }); // dropped: tombstoned
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')).toBeUndefined();
  });

  it('a status delta after a delete never reports the ID as both deleted and unknown in one flush', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);

    emit(sm, 'agent.a1.deleted', {});
    emit(sm, 'agent.a1.status', { phase: 'stopped' });
    vi.advanceTimersByTime(100);

    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;
    expect(detail.deleted).toContain('a1');
    expect(detail.unknown.has('a1')).toBe(false);
  });
});

describe('W2 unknown-buffer expiry (§7: 30s TTL)', () => {
  it('a buffered delta applies to the eventual created event within 30s', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.status', { phase: 'error' });
    vi.advanceTimersByTime(29_999);
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    expect(sm.getAgent('a1')?.phase).toBe('error');
  });

  it('a buffered delta older than 30s is dropped before the created event arrives', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.status', { phase: 'error' });
    vi.advanceTimersByTime(30_000);
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    expect(sm.getAgent('a1')?.phase).toBe('running');
  });

  it('a later delta for the same ID refreshes the 30s TTL for the whole buffered entry', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.status', { phase: 'error' });
    vi.advanceTimersByTime(20_000);
    emit(sm, 'agent.a1.status', { activity: 'thinking' });
    vi.advanceTimersByTime(20_000); // 40s since the first delta, only 20s since the second
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    // The whole entry's TTL was refreshed by the second delta, so both
    // buffered fields (merged, last-wins per field) survive to apply here.
    expect(sm.getAgent('a1')?.phase).toBe('error');
    expect(sm.getAgent('a1')?.activity).toBe('thinking');
  });

  it('an entry not touched again is dropped 30s after it was buffered', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.status', { phase: 'error' });
    vi.advanceTimersByTime(20_000);
    emit(sm, 'agent.a1.status', { activity: 'thinking' });
    vi.advanceTimersByTime(30_000); // 30s since the second (and last) delta
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    expect(sm.getAgent('a1')?.phase).toBe('running');
    expect(sm.getAgent('a1')?.activity).toBeUndefined();
  });

  it('surfaces an unknown ID in the very next flush, well before expiry', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);

    emit(sm, 'agent.ghost.status', { phase: 'error' });
    vi.advanceTimersByTime(100);

    expect(changedSpy).toHaveBeenCalledTimes(1);
    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;
    expect(detail.unknown.get('ghost')).toEqual({ phase: 'error' });
  });

  it('two buffered deltas with different detail fields, then created, equal immediate sequential application', () => {
    // Buffered path: both status deltas arrive for an unknown ID, before
    // "created".
    const buffered = new StateManager();
    buffered.setScope({ type: 'dashboard' });
    emit(buffered, 'agent.a1.status', { detail: { message: 'm1' } });
    vi.advanceTimersByTime(1_000);
    emit(buffered, 'agent.a1.status', { detail: { currentTurns: 7 } });
    vi.advanceTimersByTime(1_000);
    emit(buffered, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);

    // Reference: the very same two deltas, but applied immediately — i.e.
    // the agent already exists, so each one goes through `mergeAgentDelta`
    // on its own as it arrives. This is what "immediate sequential
    // application" means once there is a real base to merge against.
    const sequential = new StateManager();
    sequential.setScope({ type: 'dashboard' });
    emit(sequential, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    emit(sequential, 'agent.a1.status', { detail: { message: 'm1' } });
    vi.advanceTimersByTime(100);
    emit(sequential, 'agent.a1.status', { detail: { currentTurns: 7 } });
    vi.advanceTimersByTime(100);

    expect(buffered.getAgent('a1')).toEqual(sequential.getAgent('a1'));
    // Spelled out: `detail` is replaced wholesale by the later delta (the
    // first delta's `message` field is gone from the nested object), but
    // the promoted top-level `message` field survives, because the second
    // delta never carried a `message` key to overwrite it with — the same
    // asymmetry `mergeAgentDelta` already has for a real base.
    expect(buffered.getAgent('a1')?.detail).toEqual({ currentTurns: 7 });
    expect(buffered.getAgent('a1')?.message).toBe('m1');
    expect(buffered.getAgent('a1')?.currentTurns).toBe(7);
  });

  it('created path: every 3-delta sequence buffered before "created", over every created activity, equals immediate sequential application', () => {
    let checked = 0;
    for (const createdActivity of STICKY_PROBE_ACTIVITIES) {
      for (const x of STICKY_PROBE_ACTIVITIES) {
        for (const y of STICKY_PROBE_ACTIVITIES) {
          for (const z of STICKY_PROBE_ACTIVITIES) {
            const deltas = [x, y, z];

            const sm = new StateManager();
            sm.setScope({ type: 'dashboard' });
            for (const activity of deltas) {
              emitActivity(sm, 'u', activity);
            }
            emit(
              sm,
              'agent.u.created',
              createdActivity === undefined
                ? { name: 'U' }
                : { name: 'U', activity: createdActivity }
            );

            const want = sequentialCreatedActivity(createdActivity, deltas);
            expect(
              sm.getAgent('u')?.activity,
              `created-activity=${createdActivity} seq=${deltas.join(',')}`
            ).toBe(want);
            checked++;
          }
        }
      }
    }
    expect(checked).toBe(STICKY_PROBE_ACTIVITIES.length ** 4); // sanity: the full 5^4 grid ran
  }, 60_000); // 5^4 = 625 StateManager instances; the default 5s test timeout is too tight under load

  it('a long-lived unknown ID does not grow pendingAgentDeltas without bound', () => {
    // A sliding TTL (refreshed on every touch) only bounds how long an
    // entry lives, not how big it gets. An off-page agent is, by design,
    // unknown to state.agents while its status events keep arriving — if
    // each one appended to a per-ID list, an agent active for the life of
    // the tab would retain every status payload it ever sent.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    for (let i = 0; i < 5_000; i++) {
      emit(sm, 'agent.off.status', {
        activity: i % 2 ? 'working' : 'thinking',
        message: `m${i}`,
      });
      vi.advanceTimersByTime(1_000); // refreshes the sliding TTL on every event, as production does
    }

    const pending = (
      sm as unknown as { pendingAgentDeltas: Map<string, { fields: object; activity: unknown }> }
    ).pendingAgentDeltas;
    const entry = pending.get('off');
    expect(entry).toBeDefined();
    // A fixed-shape compacted summary, never an array/list that grows with
    // the number of events applied. Checking the key count alone would
    // still pass a list hidden *inside* one field (e.g. a `history` array);
    // the serialized size is the property that actually matters here.
    expect(Array.isArray(entry)).toBe(false);
    expect(Object.keys(entry?.fields ?? {}).length).toBeLessThan(10);
    expect(JSON.stringify(entry).length).toBeLessThan(500);
  }, 60_000); // 5000 events with a timer advance each; the default 5s test timeout is too tight under load
});

describe('W2 resync edges (pinned against sse-client.ts)', () => {
  it('the connect after setScope raises no resync', () => {
    const sm = new StateManager();
    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);

    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    expect(resync).not.toHaveBeenCalled();
  });

  it('a double connected after one drop yields exactly one agents-resync', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);

    sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
    // `connected` can fire twice per connection: onopen, then the server's
    // own "connected" acknowledgement event.
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    expect(resync).toHaveBeenCalledTimes(1);
  });

  it('a handshake-failed retry that never opened yields none', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);

    // A rejected handshake dispatches 'handshake-failed', never 'disconnected'.
    sm.sseClientInstance.dispatchEvent(new CustomEvent('handshake-failed'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    expect(resync).not.toHaveBeenCalled();
  });

  it('a server reconnect event (disconnected then connected) yields one', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);

    sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    expect(resync).toHaveBeenCalledTimes(1);
  });

  it('a scope change resets resync tracking: a stale disconnect does not resync the new scope', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));

    sm.setScope({ type: 'brokers-list' }); // actual scope change

    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    expect(resync).not.toHaveBeenCalled();
  });
});

describe('W2 sseConnected(generation)', () => {
  it('resolves at once when the generation is already connected', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));

    let resolved = false;
    void sm.sseConnected(sm.scopeGeneration).then(() => {
      resolved = true;
    });
    await Promise.resolve();
    await Promise.resolve();
    expect(resolved).toBe(true);
  });

  it('waits for the next connected of that generation otherwise', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    let resolved = false;
    void sm.sseConnected(sm.scopeGeneration).then(() => {
      resolved = true;
    });
    await Promise.resolve();
    expect(resolved).toBe(false);

    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    await Promise.resolve();
    await Promise.resolve();
    expect(resolved).toBe(true);
  });

  it('connected then disconnected then sseConnected stays pending until the next connected', async () => {
    // The other half of the connectedGeneration contract: a drop within the
    // SAME generation (no setScope) must not resolve sseConnected early
    // either, and a call made while disconnected must still wait.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const gen = sm.scopeGeneration;

    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));

    let resolved = false;
    void sm.sseConnected(gen).then(() => {
      resolved = true;
    });
    await Promise.resolve();
    await Promise.resolve();
    expect(resolved).toBe(false);

    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    await Promise.resolve();
    await Promise.resolve();
    expect(resolved).toBe(true);
  });

  it('rejects immediately when the generation is already stale', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const staleGen = sm.scopeGeneration;
    sm.setScope({ type: 'brokers-list' });

    await expect(sm.sseConnected(staleGen)).rejects.toThrow();
  });

  it('rejects if the generation changes while waiting', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const gen = sm.scopeGeneration;

    const promise = sm.sseConnected(gen);
    const assertion = expect(promise).rejects.toThrow();
    sm.setScope({ type: 'brokers-list' });
    await assertion;
  });

  it('does not resolve at once for the new generation right after setScope, even though the previous generation was connected', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected')); // gen N connects

    sm.setScope({ type: 'project', projectId: 'p1' }); // -> gen N+1; no connected yet
    const genNPlus1 = sm.scopeGeneration;

    let resolved = false;
    void sm.sseConnected(genNPlus1).then(() => {
      resolved = true;
    });
    await Promise.resolve();
    await Promise.resolve();
    // Must still be pending: sse-client.ts's connect() tears the old
    // connection down without a `disconnected` event, so `state.connected`
    // alone would wrongly read as "still connected" here. `sseConnected`
    // tracks this itself via `connectedGeneration`, not `isConnected`.
    expect(resolved).toBe(false);

    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected')); // gen N+1 connects
    await Promise.resolve();
    await Promise.resolve();
    expect(resolved).toBe(true);
  });

  it('isConnected is left alone by setScope — chat-thread.ts:1536 reads it to seed its reconnect catch-up', () => {
    // Resetting `state.connected` in setScope alongside connectedGeneration
    // would be a silent behaviour change for chat-thread.ts, the one reader
    // of `stateManager.isConnected`: it seeds
    // `_sawSseConnect` from it, and swallows the first `connected` it sees
    // as "nothing to catch up on" when that flag is already true. Making
    // isConnected go stale-false across a setScope made a warm navigation
    // into chat wrongly swallow its post-navigation `connected` as the
    // "first" one, silently dropping messages sent in that window. Pinned
    // here so a future change that needs `isConnected` to be
    // generation-accurate also has to update chat-thread's catch-up logic
    // in the same change, not accidentally as a side effect of state.ts.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    expect(sm.isConnected).toBe(true);

    sm.setScope({ type: 'project', projectId: 'p1' }); // actual scope change

    expect(sm.isConnected).toBe(true);
  });

  it('disconnect() rejects every pending waiter instead of leaving it hanging', async () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    const gen = sm.scopeGeneration;

    const promise = sm.sseConnected(gen);
    const assertion = expect(promise).rejects.toThrow();
    sm.disconnect();
    await assertion;
  });
});
