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
 * The shared agent-delete helper (ptone/scion#2483 phase 2): every answer
 * the hub can give, the confirms, force, and the design acceptance "a
 * second browser can force-complete a failed delete it did not start".
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DeletionInfo } from '../shared/types.js';

vi.mock('../components/shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

/**
 * The helper records a 202 into `stateManager`. Tests point that at a
 * StateManager of their own (a "browser"), or at a spy.
 */
const holder = vi.hoisted(() => ({ sm: null as unknown }));
vi.mock('./state.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./state.js')>();
  return {
    ...actual,
    get stateManager(): unknown {
      return holder.sm ?? actual.stateManager;
    },
  };
});

import {
  runAgentDelete,
  lifecycleActionErrorMessage,
  FORCE_FALLBACK_CONFIRM_MESSAGE,
  forceDeleteConfirmMessage,
} from './agent-delete.js';
import { StateManager } from './state.js';
import { showConfirm } from '../components/shared/confirm-dialog.js';
import { effectiveDeletion, START_BLOCKED_BY_DELETE_MESSAGE } from '../shared/agent-deletion.js';

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

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const noContent = (): Response => new Response(null, { status: 204 });
const accepted = (d: DeletionInfo): Response => json(202, { agentId: 'a1', deletion: d });
const hubError = (status: number, code: string, message: string): Response =>
  json(status, { error: { code, message } });

/** Stub fetch with one queued answer per call; records each URL and method. */
function stubFetch(...answers: Array<Response | Error>): Array<{ url: string; method: string }> {
  const calls: Array<{ url: string; method: string }> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(input), method: init?.method ?? 'GET' });
      const next = answers.shift();
      if (!next) throw new Error('unexpected fetch');
      return next instanceof Error ? Promise.reject(next) : Promise.resolve(next);
    })
  );
  return calls;
}

const applyDeleteAccepted = vi.fn(() => true);

beforeEach(() => {
  vi.mocked(showConfirm).mockReset();
  vi.mocked(showConfirm).mockResolvedValue(true);
  applyDeleteAccepted.mockClear();
  holder.sm = { applyDeleteAccepted };
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  holder.sm = null;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('runAgentDelete', () => {
  it('204 → deleted, after the confirm', async () => {
    const calls = stubFetch(noContent());
    const busy: boolean[] = [];
    const out = await runAgentDelete({
      agentId: 'a1',
      agentName: 'alpha',
      onBusy: (b) => busy.push(b),
    });
    expect(out).toEqual({ kind: 'deleted', forced: false });
    expect(calls).toEqual([{ url: '/api/v1/agents/a1', method: 'DELETE' }]);
    expect(showConfirm).toHaveBeenCalledWith('Are you sure you want to delete agent "alpha"?');
    expect(busy).toEqual([true, false]);
    expect(applyDeleteAccepted).not.toHaveBeenCalled();
  });

  it('202 → accepted, and the view is applied to the state manager', async () => {
    const view = deleting();
    stubFetch(accepted(view));
    const out = await runAgentDelete({ agentId: 'a1' });
    expect(out).toEqual({ kind: 'accepted', forced: false, deletion: view });
    expect(applyDeleteAccepted).toHaveBeenCalledWith('a1', view);
  });

  it('a cancelled confirm sends nothing', async () => {
    const calls = stubFetch();
    vi.mocked(showConfirm).mockResolvedValueOnce(false);
    const busy: boolean[] = [];
    const out = await runAgentDelete({ agentId: 'a1', onBusy: (b) => busy.push(b) });
    expect(out).toEqual({ kind: 'cancelled' });
    expect(calls).toEqual([]);
    expect(busy).toEqual([]);
  });

  it('Alt-click and confirm:false skip the confirm', async () => {
    stubFetch(noContent(), noContent());
    await runAgentDelete({ agentId: 'a1', event: { altKey: true } as MouseEvent });
    await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(showConfirm).not.toHaveBeenCalled();
  });

  for (const status of [502, 503]) {
    it(`${status} → force confirm → force 204 → deleted (forced)`, async () => {
      const calls = stubFetch(hubError(status, 'runtime_error', 'broker down'), noContent());
      const out = await runAgentDelete({ agentId: 'a1', confirm: false });
      expect(out).toEqual({ kind: 'deleted', forced: true });
      expect(calls.map((c) => c.url)).toEqual([
        '/api/v1/agents/a1',
        '/api/v1/agents/a1?force=true',
      ]);
      expect(showConfirm).toHaveBeenCalledWith(
        FORCE_FALLBACK_CONFIRM_MESSAGE,
        expect.objectContaining({ title: 'Force Delete', variant: 'danger' })
      );
    });
  }

  it('502 → force confirm → force 202 → accepted (forced), view applied', async () => {
    const view = deleting(2);
    stubFetch(hubError(502, 'runtime_error', 'broker down'), accepted(view));
    const out = await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(out).toEqual({ kind: 'accepted', forced: true, deletion: view });
    expect(applyDeleteAccepted).toHaveBeenCalledWith('a1', view);
  });

  it('502 with the force offer declined → failed with the original error', async () => {
    const calls = stubFetch(hubError(502, 'runtime_error', 'broker down'));
    vi.mocked(showConfirm).mockResolvedValueOnce(false);
    const out = await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(out).toEqual({
      kind: 'failed',
      forced: false,
      status: 502,
      code: 'runtime_error',
      message: 'broker down',
    });
    expect(calls).toHaveLength(1);
  });

  it('502 → force fails too → failed (forced) with the force error', async () => {
    stubFetch(hubError(502, 'runtime_error', 'broker down'), hubError(500, 'internal', 'db gone'));
    const out = await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(out).toEqual({
      kind: 'failed',
      forced: true,
      status: 500,
      code: 'internal',
      message: 'db gone',
    });
  });

  it('forceFallback:false reports a 502 without offering force', async () => {
    const calls = stubFetch(hubError(502, 'runtime_error', 'broker down'));
    const out = await runAgentDelete({ agentId: 'a1', confirm: false, forceFallback: false });
    expect(out).toMatchObject({ kind: 'failed', status: 502, message: 'broker down' });
    expect(showConfirm).not.toHaveBeenCalled();
    expect(calls).toHaveLength(1);
  });

  it('409 → failed with its code and message, no force offer', async () => {
    stubFetch(hubError(409, 'conflict', 'ambiguous target'));
    const out = await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(out).toEqual({
      kind: 'failed',
      forced: false,
      status: 409,
      code: 'conflict',
      message: 'ambiguous target',
    });
    expect(showConfirm).not.toHaveBeenCalled();
  });

  it('an error body without JSON falls back to the generic text', async () => {
    stubFetch(new Response('oops', { status: 500 }));
    const out = await runAgentDelete({ agentId: 'a1', confirm: false });
    expect(out).toMatchObject({ kind: 'failed', status: 500, message: 'Failed to delete agent' });
  });

  it('a network error → failed with status null, and busy is cleared', async () => {
    stubFetch(new TypeError('Failed to fetch'));
    const busy: boolean[] = [];
    const out = await runAgentDelete({
      agentId: 'a1',
      confirm: false,
      onBusy: (b) => busy.push(b),
    });
    expect(out).toEqual({
      kind: 'failed',
      forced: false,
      status: null,
      code: '',
      message: 'Failed to fetch',
    });
    expect(busy).toEqual([true, false]);
  });

  describe('force (the failure banner)', () => {
    it('asks the force confirm, then sends ?force=true directly', async () => {
      const calls = stubFetch(noContent());
      const out = await runAgentDelete({ agentId: 'a1', agentName: 'alpha', force: true });
      expect(out).toEqual({ kind: 'deleted', forced: true });
      expect(calls).toEqual([{ url: '/api/v1/agents/a1?force=true', method: 'DELETE' }]);
      expect(showConfirm).toHaveBeenCalledTimes(1);
      expect(showConfirm).toHaveBeenCalledWith(
        forceDeleteConfirmMessage('alpha'),
        expect.objectContaining({ confirmText: 'Force Delete', variant: 'danger' })
      );
    });

    it('asks even with confirm:false or Alt-click, and a cancel sends nothing', async () => {
      const calls = stubFetch();
      vi.mocked(showConfirm).mockResolvedValueOnce(false);
      const out = await runAgentDelete({
        agentId: 'a1',
        force: true,
        confirm: false,
        event: { altKey: true } as MouseEvent,
      });
      expect(out).toEqual({ kind: 'cancelled' });
      expect(showConfirm).toHaveBeenCalledTimes(1);
      expect(calls).toEqual([]);
    });

    it('a force 502 is reported, never re-offered', async () => {
      stubFetch(hubError(502, 'runtime_error', 'still down'));
      const out = await runAgentDelete({ agentId: 'a1', force: true });
      expect(out).toMatchObject({
        kind: 'failed',
        forced: true,
        status: 502,
        message: 'still down',
      });
      expect(showConfirm).toHaveBeenCalledTimes(1);
    });

    it('a force 202 is applied like any other', async () => {
      const view = deleting(3);
      stubFetch(accepted(view));
      const out = await runAgentDelete({ agentId: 'a1', force: true });
      expect(out).toEqual({ kind: 'accepted', forced: true, deletion: view });
      expect(applyDeleteAccepted).toHaveBeenCalledWith('a1', view);
    });
  });
});

describe('lifecycleActionErrorMessage', () => {
  it('explains a 409 delete_in_progress instead of the generic text', async () => {
    const msg = await lifecycleActionErrorMessage(
      hubError(409, 'delete_in_progress', 'agent is being deleted'),
      'Failed to start agent'
    );
    expect(msg).toBe(START_BLOCKED_BY_DELETE_MESSAGE);
    expect(msg).toMatch(/can't be started/);
    expect(msg).toMatch(/Force delete/);
  });

  it('passes other errors through (another 409 code, other statuses)', async () => {
    expect(
      await lifecycleActionErrorMessage(hubError(409, 'agent_launching', 'launching'), 'x')
    ).toBe('launching');
    expect(await lifecycleActionErrorMessage(hubError(500, 'delete_in_progress', 'odd'), 'x')).toBe(
      'odd'
    );
    expect(await lifecycleActionErrorMessage(new Response('', { status: 500 }), 'fb')).toBe('fb');
  });
});

/**
 * Design acceptance (§8 phase 2): a second browser can force-complete a
 * failed delete it did not start. Two StateManagers stand in for the two
 * browsers; the hub stub fans the same SSE events out to both.
 */
describe('a second browser force-completes a failed delete it did not start', () => {
  class FakeEventSource extends EventTarget {
    readyState = 0;
    close(): void {
      this.readyState = 2;
    }
  }

  function emit(sm: StateManager, subject: string, data: unknown): void {
    (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
      subject,
      data,
    });
  }

  function browser(): StateManager {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1' });
    vi.advanceTimersByTime(100);
    return sm;
  }

  /** The hub publishes to every open browser. */
  function hubPublish(browsers: StateManager[], subject: string, data: unknown): void {
    for (const sm of browsers) emit(sm, subject, data);
    vi.advanceTimersByTime(100);
  }

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], now: T0 });
    vi.stubGlobal('requestAnimationFrame', () => 0);
    vi.stubGlobal('cancelAnimationFrame', () => {});
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  /** A starts a delete that fails on the broker; B only watches. */
  function failedDeleteStartedByA(): { a: StateManager; b: StateManager } {
    const a = browser();
    const b = browser();
    hubPublish([a, b], 'agent.a1.status', { deletion: deleting(1, T0 + 60_000) });
    hubPublish([a, b], 'agent.a1.status', {
      deletion: {
        ...deleting(1),
        state: 'failed',
        code: 'runtime_error',
        error: 'broker refused',
        leaseExpiresAt: undefined,
        expiresAt: iso(T0 + 15 * 60_000),
      },
    });
    const bView = effectiveDeletion(b.getAgent('a1')?.deletion, Date.now());
    expect(bView).toMatchObject({ state: 'failed', code: 'runtime_error' });
    return { a, b };
  }

  it('Force → 204: B gets deleted, and the hub deleted event removes the agent in both', async () => {
    const { a, b } = failedDeleteStartedByA();
    holder.sm = b; // B's page runs the helper
    const calls = stubFetch(noContent());

    const out = await runAgentDelete({ agentId: 'a1', agentName: 'A1', force: true });
    expect(out).toEqual({ kind: 'deleted', forced: true });
    expect(calls).toEqual([{ url: '/api/v1/agents/a1?force=true', method: 'DELETE' }]);

    hubPublish([a, b], 'agent.a1.deleted', {});
    expect(a.getAgent('a1')).toBeUndefined();
    expect(b.getAgent('a1')).toBeUndefined();
  });

  it('Force → 202: B shows the new claim as Deleting…, then deleted removes it in both', async () => {
    const { a, b } = failedDeleteStartedByA();
    holder.sm = b;
    const forceClaim = deleting(2, T0 + 60_000);
    stubFetch(accepted(forceClaim));

    const out = await runAgentDelete({ agentId: 'a1', force: true });
    expect(out).toEqual({ kind: 'accepted', forced: true, deletion: forceClaim });
    vi.advanceTimersByTime(100);
    expect(b.getAgent('a1')?.deletion).toMatchObject({ state: 'deleting', claim: 2 });
    // A learns of the force claim from SSE only.
    hubPublish([a, b], 'agent.a1.status', { deletion: forceClaim });
    expect(a.getAgent('a1')?.deletion).toMatchObject({ state: 'deleting', claim: 2 });

    hubPublish([a, b], 'agent.a1.deleted', {});
    expect(a.getAgent('a1')).toBeUndefined();
    expect(b.getAgent('a1')).toBeUndefined();
  });
});
