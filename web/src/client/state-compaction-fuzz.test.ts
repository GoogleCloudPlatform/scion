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
 * `CompactedDelta` (§7, §8) vs. immediate sequential application, over a
 * wider field mix than the W2 fuzz and the W2/W3 exhaustive enumerations
 * exercise: `detail` objects followed by top-level-only fields, `null` and
 * truthy `_capabilities`, and an explicit `activity: undefined` key (not
 * just a missing one). A detail-bearing delta followed by a top-level-only
 * one is exactly the shape that caught a real regression: a stale
 * `detail.message` got re-promoted over a later plain `message` at apply
 * time, because `fields` already had both correctly folded but
 * `applyCompactedDelta` ran `mergeAgentDelta`'s own promotion a second
 * time, out of order. The W2 fuzz and the W2/W3 exhaustive enumerations
 * only vary `activity`, so none of them could have caught it.
 *
 * Covers all four places a `CompactedDelta` gets built and applied: an
 * unknown ID buffered then drained by `created`; a known-ID seed epoch; an
 * unknown-ID seed epoch; and an unknown ID buffered *and* recorded into an
 * epoch, drained by a `created` that itself lands mid-epoch, with more
 * deltas after it, then seeded. Each test's oracle is an independent
 * re-implementation — seed or create directly, then apply every delta
 * immediately as a known agent through the public SSE path — not a call
 * into anything `state.ts` uses internally.
 *
 * Deterministic (fixed seeds, fixed case counts): a failure is reproducible
 * without re-running.
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

function mk(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  return sm;
}

/** Deterministic PRNG (same family as state-coalescing.test.ts's mulberry32). */
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

/** Sentinel meaning "omit this field from the generated delta entirely" — distinct from an explicit `undefined`. */
const MISSING = Symbol('missing');

function makeGenerators(rand: () => number) {
  const pick = <T>(arr: T[]): T => arr[Math.floor(rand() * arr.length)] as T;
  // MISSING appears twice to weight "field omitted" roughly as likely as
  // any single concrete value; `null` and an explicit `undefined` are each
  // their own case, distinct from MISSING and from each other —
  // `agentsShallowEqual`'s hasOwnProperty check and `mergeAgentDelta`'s
  // falsy-capabilities fallback both care about the difference.
  const ACTIVITIES: unknown[] = [
    MISSING,
    MISSING,
    'working',
    '',
    'thinking',
    'waiting_for_input',
    'completed',
    'limits_exceeded',
    null,
    undefined,
  ];
  const CAPS: unknown[] = [MISSING, MISSING, MISSING, { a: 1 }, { b: 2 }, null, undefined];
  const MESSAGES: unknown[] = [MISSING, 'm1', 'm2', ''];
  const DETAILS: unknown[] = [
    MISSING,
    MISSING,
    { message: 'dm' },
    { currentTurns: 3 },
    { message: '', currentTurns: 0, startedAt: 's' },
  ];

  function set(o: Record<string, unknown>, key: string, value: unknown): void {
    if (value !== MISSING) o[key] = value;
  }

  function genDelta(): Record<string, unknown> {
    const o: Record<string, unknown> = {};
    set(o, 'activity', pick(ACTIVITIES));
    set(o, '_capabilities', pick(CAPS));
    set(o, 'message', pick(MESSAGES));
    set(o, 'detail', pick(DETAILS));
    if (rand() < 0.3) o.phase = pick(['running', 'error', 'stopped']);
    return o;
  }

  function genRest(id: string): Agent {
    const o: Record<string, unknown> = { id, name: id };
    const a = pick(ACTIVITIES);
    if (a !== MISSING && a !== null && a !== undefined) o.activity = a;
    const c = pick([MISSING, { r: 1 }]);
    if (c !== MISSING) o._capabilities = c;
    if (rand() < 0.5) o.message = 'rest';
    return o as Agent;
  }

  return { genDelta, genRest };
}

/**
 * Canonical string form for comparison: own keys only, sorted, with an
 * explicit `undefined` *value* marked distinctly from a key that is simply
 * absent (`JSON.stringify` alone would drop both the same way).
 */
function canon(a: Agent | undefined): string {
  if (!a) return 'none';
  const o = a as unknown as Record<string, unknown>;
  return JSON.stringify(
    Object.keys(o)
      .sort()
      .map((k) => [k, o[k] === undefined ? '<undef>' : o[k]])
  );
}

const N = 3000;

describe('compacted buffer/epoch paths vs. sequential application', () => {
  it('known-ID epoch vs sequential', () => {
    const rand = mulberry32(0xc0ffee);
    const { genDelta, genRest } = makeGenerators(rand);
    const bad: string[] = [];
    for (let i = 0; i < N; i++) {
      const live0 = genRest('a');
      const rest = genRest('a');
      const deltas = Array.from({ length: 1 + Math.floor(rand() * 6) }, genDelta);

      const sm = mk();
      sm.seedAgents([live0]);
      const token = sm.beginSeedEpoch();
      for (const d of deltas) emit(sm, 'agent.a.status', d);
      sm.seedAgents([rest], { token });

      const oracle = mk();
      oracle.seedAgents([rest]);
      for (const d of deltas) emit(oracle, 'agent.a.status', d);

      const got = canon(sm.getAgent('a'));
      const want = canon(oracle.getAgent('a'));
      if (got !== want) bad.push(JSON.stringify({ live0, rest, deltas, got, want }));
    }
    expect(bad, `${bad.length} mismatches; first: ${bad[0]}`).toEqual([]);
  }, 60_000); // N=3000 cases; default 5s test timeout is too tight under load

  it('unknown-ID epoch vs sequential', () => {
    const rand = mulberry32(0x1234567);
    const { genDelta, genRest } = makeGenerators(rand);
    const bad: string[] = [];
    for (let i = 0; i < N; i++) {
      const rest = genRest('u');
      const deltas = Array.from({ length: 1 + Math.floor(rand() * 6) }, genDelta);

      const sm = mk();
      const token = sm.beginSeedEpoch();
      for (const d of deltas) emit(sm, 'agent.u.status', d);
      sm.seedAgents([rest], { token });

      const oracle = mk();
      oracle.seedAgents([rest]);
      for (const d of deltas) emit(oracle, 'agent.u.status', d);

      const got = canon(sm.getAgent('u'));
      const want = canon(oracle.getAgent('u'));
      if (got !== want) bad.push(JSON.stringify({ rest, deltas, got, want }));
    }
    expect(bad, `${bad.length} mismatches; first: ${bad[0]}`).toEqual([]);
  }, 60_000); // N=3000 cases; default 5s test timeout is too tight under load

  it('buffer then created vs sequential', () => {
    const rand = mulberry32(0xabcdef);
    const { genDelta } = makeGenerators(rand);
    const bad: string[] = [];
    for (let i = 0; i < N; i++) {
      const deltas = Array.from({ length: 1 + Math.floor(rand() * 6) }, genDelta);
      const created = { name: 'u', ...genDelta() };

      const sm = mk();
      for (const d of deltas) emit(sm, 'agent.u.status', d);
      emit(sm, 'agent.u.created', created);

      const oracle = mk();
      emit(oracle, 'agent.u.created', created);
      for (const d of deltas) emit(oracle, 'agent.u.status', d);

      const got = canon(sm.getAgent('u'));
      const want = canon(oracle.getAgent('u'));
      if (got !== want) bad.push(JSON.stringify({ created, deltas, got, want }));
    }
    expect(bad, `${bad.length} mismatches; first: ${bad[0]}`).toEqual([]);
  }, 60_000); // N=3000 cases; default 5s test timeout is too tight under load

  it('buffer + created inside epoch + post-deltas, seed vs sequential', () => {
    const rand = mulberry32(0x9e3779b9);
    const { genDelta, genRest } = makeGenerators(rand);
    const bad: string[] = [];
    for (let i = 0; i < N; i++) {
      const rest = genRest('u');
      const pre = Array.from({ length: Math.floor(rand() * 5) }, genDelta);
      const created = { name: 'u', ...genDelta() };
      const post = Array.from({ length: Math.floor(rand() * 4) }, genDelta);

      const sm = mk();
      const token = sm.beginSeedEpoch();
      for (const d of pre) emit(sm, 'agent.u.status', d);
      emit(sm, 'agent.u.created', created);
      for (const d of post) emit(sm, 'agent.u.status', d);
      sm.seedAgents([rest], { token });

      const oracle = mk();
      oracle.seedAgents([rest]);
      emit(oracle, 'agent.u.created', created);
      for (const d of [...pre, ...post]) emit(oracle, 'agent.u.status', d);

      const got = canon(sm.getAgent('u'));
      const want = canon(oracle.getAgent('u'));
      if (got !== want) bad.push(JSON.stringify({ rest, pre, created, post, got, want }));
    }
    expect(bad, `${bad.length} mismatches; first: ${bad[0]}`).toEqual([]);
  }, 60_000); // N=3000 cases; default 5s test timeout is too tight under load
});
