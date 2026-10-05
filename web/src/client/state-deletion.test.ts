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
 * ptone/scion#2483 phase 1b: `agent.deletion` in the StateManager — SSE
 * merge (an explicit `null` clears), DELETE 202 acceptance
 * (`applyDeleteAccepted`), the tombstone, and a two-browser simulation.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { DeletionInfo } from '../shared/types.js';
import { isDeletionActive } from '../shared/agent-deletion.js';

function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

class FakeEventSource extends EventTarget {
  readyState = 0;
  close(): void {
    this.readyState = 2;
  }
}

const T0 = Date.parse('2026-10-04T12:00:00Z');
const iso = (ms: number): string => new Date(ms).toISOString();

function deleting(claim = 1, leaseMs = T0 + 60_000): DeletionInfo {
  return {
    state: 'deleting',
    soft: false,
    claim,
    startedAt: iso(T0),
    leaseExpiresAt: iso(leaseMs),
  };
}

function failed(claim = 1): DeletionInfo {
  return { ...deleting(claim), state: 'failed', code: 'runtime_error', error: 'broker refused' };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('requestAnimationFrame', () => 0);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function freshManager(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
  vi.advanceTimersByTime(100);
  return sm;
}

describe('agent.deletion SSE merge', () => {
  it('stores a deleting view and clears it on an explicit deletion:null', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: deleting() });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion?.state).toBe('deleting');

    const updated = vi.fn();
    sm.addEventListener('agents-updated', updated);
    emit(sm, 'agent.a1.status', { deletion: null });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion).toBeNull();
    expect(updated).toHaveBeenCalledTimes(1);
  });

  it('a delta without a deletion key leaves the view in place', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: deleting() });
    emit(sm, 'agent.a1.status', { activity: 'working' });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion?.state).toBe('deleting');
  });

  it('an identical deletion republish is a no-op (value compare, not identity)', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: deleting() });
    vi.advanceTimersByTime(100);
    const before = sm.getAgent('a1');

    const updated = vi.fn();
    sm.addEventListener('agents-updated', updated);
    emit(sm, 'agent.a1.status', { deletion: deleting() });
    vi.advanceTimersByTime(1000);
    expect(updated).not.toHaveBeenCalled();
    expect(sm.getAgent('a1')).toBe(before);
  });

  it('a lease renewal is applied', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: deleting(1, T0 + 20_000) });
    emit(sm, 'agent.a1.status', { deletion: deleting(1, T0 + 40_000) });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion?.leaseExpiresAt).toBe(iso(T0 + 40_000));
  });

  it('a failed delta replaces deleting', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: deleting() });
    emit(sm, 'agent.a1.status', { deletion: failed() });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion).toMatchObject({ state: 'failed', error: 'broker refused' });
  });
});

describe('applyDeleteAccepted (DELETE 202)', () => {
  it('keeps the agent with the returned deletion; a later SSE deleted removes it', () => {
    const sm = freshManager();
    expect(sm.applyDeleteAccepted('a1', deleting())).toBe(true);
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.deletion?.state).toBe('deleting');

    emit(sm, 'agent.a1.deleted', {});
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')).toBeUndefined();
    expect(sm.getDeletedAgentIds().has('a1')).toBe(true);
  });

  it('does not overwrite an SSE view for the same claim (a failure that beat the 202)', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: failed(2) });
    vi.advanceTimersByTime(100);
    expect(sm.applyDeleteAccepted('a1', deleting(2))).toBe(false);
    expect(sm.getAgent('a1')?.deletion?.state).toBe('failed');
  });

  it('replaces an older claim (retry after a failure)', () => {
    const sm = freshManager();
    emit(sm, 'agent.a1.status', { deletion: failed(1) });
    vi.advanceTimersByTime(100);
    expect(sm.applyDeleteAccepted('a1', deleting(2))).toBe(true);
    expect(sm.getAgent('a1')?.deletion).toMatchObject({ state: 'deleting', claim: 2 });
  });

  it('ignores unknown, tombstoned, and missing deletions', () => {
    const sm = freshManager();
    expect(sm.applyDeleteAccepted('nope', deleting())).toBe(false);
    expect(sm.getAgent('nope')).toBeUndefined();
    expect(sm.applyDeleteAccepted('a1', null)).toBe(false);
    expect(sm.applyDeleteAccepted('a1', undefined)).toBe(false);

    emit(sm, 'agent.a1.deleted', {});
    vi.advanceTimersByTime(100);
    expect(sm.applyDeleteAccepted('a1', deleting())).toBe(false);
    expect(sm.getAgent('a1')).toBeUndefined();
  });
});

describe('two browsers, one delete (state-level simulation)', () => {
  it('both see Deleting…, then both drop the agent on deleted; the initiator never removes locally first', () => {
    // Browser A issues the DELETE; browser B is only watching. The hub's
    // claim delta fans out to both, the 202 reaches only A, and the final
    // `deleted` reaches both.
    const a = freshManager();
    const b = freshManager();
    const now = T0 + 1_000;

    const claim = deleting(1, T0 + 20_000);
    emit(a, 'agent.a1.status', { deletion: claim });
    emit(b, 'agent.a1.status', { deletion: claim });
    vi.advanceTimersByTime(100);
    a.applyDeleteAccepted('a1', claim); // same claim: no-op
    expect(isDeletionActive(a.getAgent('a1')!, now)).toBe(true);
    expect(isDeletionActive(b.getAgent('a1')!, now)).toBe(true);

    // Renewal reaches both.
    const renewed = deleting(1, T0 + 40_000);
    emit(a, 'agent.a1.status', { deletion: renewed });
    emit(b, 'agent.a1.status', { deletion: renewed });
    vi.advanceTimersByTime(100);
    expect(isDeletionActive(b.getAgent('a1')!, T0 + 30_000)).toBe(true);

    emit(a, 'agent.a1.deleted', {});
    emit(b, 'agent.a1.deleted', {});
    // A replayed created must not resurrect the row in either browser.
    emit(b, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    expect(a.getAgent('a1')).toBeUndefined();
    expect(b.getAgent('a1')).toBeUndefined();
  });

  it('a failure reaches the watcher too, and a clear (null) restores both', () => {
    const a = freshManager();
    const b = freshManager();
    for (const sm of [a, b]) emit(sm, 'agent.a1.status', { deletion: failed() });
    vi.advanceTimersByTime(100);
    expect(b.getAgent('a1')?.deletion?.state).toBe('failed');
    for (const sm of [a, b]) emit(sm, 'agent.a1.status', { deletion: null });
    vi.advanceTimersByTime(100);
    expect(a.getAgent('a1')?.deletion).toBeNull();
    expect(b.getAgent('a1')?.deletion).toBeNull();
  });
});
