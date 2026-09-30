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
import { StateManager, type AgentsChangedDetail } from './state.js';
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
  | { kind: 'created'; id: string; phase: string; name: string }
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
      events.push({ kind: 'created', id, phase: pick(PHASES), name: `Agent ${id}` });
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
      if (rand() < 0.2) ev.detail = { message: `m${i}`, currentTurns: i % 7 };
      events.push(ev);
    }
  }
  return events;
}

function fuzzEventToDelta(ev: FuzzEvent): Partial<Agent> {
  switch (ev.kind) {
    case 'created':
      return { phase: ev.phase, name: ev.name } as Partial<Agent>;
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
 * Independent reference reducer mirroring the documented merge semantics
 * (sticky activity, detail promotion, capability preservation, buffering of
 * early deltas, tombstones) with NO equality-skip and NO coalescing. Used to
 * check that the production code's final `state.agents` content is exactly
 * what applying every delta immediately would produce — i.e. the new
 * shallow-equal no-op never causes a real change to be dropped.
 */
function referenceApply(events: FuzzEvent[]): Map<string, Agent> {
  const agents = new Map<string, Agent>();
  const pending = new Map<string, Partial<Agent>>();

  for (const ev of events) {
    if (ev.kind === 'deleted') {
      agents.delete(ev.id);
      pending.delete(ev.id);
      continue;
    }
    if (ev.kind === 'ports') {
      const existing = agents.get(ev.id);
      if (existing) {
        agents.set(ev.id, { ...existing, exposedPorts: ev.ports } as Agent);
      }
      continue;
    }

    const existing = agents.get(ev.id);
    const isCreated = ev.kind === 'created';
    if (!existing && !isCreated) {
      const delta = fuzzEventToDelta(ev);
      const prev = pending.get(ev.id);
      pending.set(ev.id, prev ? { ...prev, ...delta } : delta);
      continue;
    }

    const base = existing || ({} as Agent);
    let delta = fuzzEventToDelta(ev);
    if (isCreated) {
      const p = pending.get(ev.id);
      if (p) delta = { ...delta, ...p };
      pending.delete(ev.id);
    }

    const incomingActivity = delta.activity as string | undefined;
    if (
      incomingActivity !== undefined &&
      (incomingActivity === 'working' || incomingActivity === '') &&
      base.activity &&
      STICKY_ACTIVITIES.has(base.activity)
    ) {
      delete delta.activity;
    }
    const detail = delta.detail;
    if (detail) {
      if (detail.message) (delta as Record<string, unknown>).message = detail.message;
      if (detail.currentTurns !== undefined) {
        (delta as Record<string, unknown>).currentTurns = detail.currentTurns;
      }
      if (detail.currentModelCalls !== undefined) {
        (delta as Record<string, unknown>).currentModelCalls = detail.currentModelCalls;
      }
      if (detail.startedAt) (delta as Record<string, unknown>).startedAt = detail.startedAt;
    }
    const updated = { ...base, ...delta, id: ev.id } as Agent;
    if (!delta._capabilities && base._capabilities) {
      updated._capabilities = base._capabilities;
    }
    agents.set(ev.id, updated);
  }
  return agents;
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
    case 'created':
      emit(sm, `agent.${ev.id}.created`, { phase: ev.phase, name: ev.name });
      break;
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
  });

  it('every changed ID appears in upserted, deleted or unknown, and reports at most one notify per flush', () => {
    const events = genEvents(10_000, 1234567);
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });

    const changedSpy = vi.fn<(e: Event) => void>();
    sm.addEventListener('agents-changed', changedSpy as EventListener);

    for (const ev of events) {
      applyEvent(sm, ev);
    }
    vi.advanceTimersByTime(100);

    expect(changedSpy).toHaveBeenCalledTimes(1);
    const detail = (changedSpy.mock.calls[0]?.[0] as CustomEvent<{ data: AgentsChangedDetail }>)
      .detail.data;

    const finalIds = new Set(sm.getAgents().map((a) => a.id));
    const knownEver = new Set<string>();
    for (const ev of events) {
      if (ev.kind === 'created') knownEver.add(ev.id);
    }
    const reportedIds = new Set([...detail.upserted, ...detail.deleted, ...detail.unknown.keys()]);

    // Every ID that ended up known, or that was ever created (even if later
    // deleted), must have been reported by this single flush.
    for (const id of finalIds) {
      expect(reportedIds.has(id)).toBe(true);
    }
    for (const id of knownEver) {
      expect(reportedIds.has(id)).toBe(true);
    }
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
});

describe('W2 resync edges (§7 N4, pinned against sse-client.ts)', () => {
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
});
