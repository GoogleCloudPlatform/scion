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

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import type { ReactiveController, ReactiveControllerHost } from 'lit';
import { DeletionLeaseController, type DeletionClock } from './deletion-badge.js';
import './deletion-badge.js';
import type { ScionDeletionBadge } from './deletion-badge.js';
import type { Agent, DeletionInfo } from '../../shared/types.js';

const T0 = Date.parse('2026-10-04T12:00:00Z');
const iso = (ms: number): string => new Date(ms).toISOString();

function deleting(leaseMs: number, claim = 1): DeletionInfo {
  return {
    state: 'deleting',
    soft: false,
    claim,
    startedAt: iso(T0),
    leaseExpiresAt: iso(leaseMs),
  };
}

/** Manually advanced clock: at most a handful of timers, fired by `advance`. */
class FakeClock implements DeletionClock {
  t = T0;
  private nextId = 1;
  timers = new Map<number, { at: number; fn: () => void }>();
  now(): number {
    return this.t;
  }
  setTimeout(fn: () => void, ms: number): unknown {
    const id = this.nextId++;
    this.timers.set(id, { at: this.t + ms, fn });
    return id;
  }
  clearTimeout(h: unknown): void {
    this.timers.delete(h as number);
  }
  advance(ms: number): void {
    this.t += ms;
    for (const [id, timer] of [...this.timers]) {
      if (timer.at <= this.t) {
        this.timers.delete(id);
        timer.fn();
      }
    }
  }
}

class FakeHost implements ReactiveControllerHost {
  controllers: ReactiveController[] = [];
  updates = 0;
  addController(c: ReactiveController): void {
    this.controllers.push(c);
  }
  removeController(): void {}
  requestUpdate(): void {
    this.updates++;
  }
  get updateComplete(): Promise<boolean> {
    return Promise.resolve(true);
  }
  /** Simulate a host render cycle completing. */
  updated(): void {
    for (const c of this.controllers) c.hostUpdated?.();
  }
}

describe('DeletionLeaseController', () => {
  it('flips a deleting agent to abandoned at leaseExpiresAt with one timer', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agents: Array<Pick<Agent, 'deletion'>> = [
      { deletion: deleting(T0 + 20_000) },
      { deletion: deleting(T0 + 50_000) },
    ];
    const c = new DeletionLeaseController(host, () => agents, clock);
    host.updated();

    expect(clock.timers.size).toBe(1);
    expect([...clock.timers.values()][0].at).toBe(T0 + 20_000);
    expect(c.isDeleting(agents[0])).toBe(true);
    expect(c.view(agents[0])?.state).toBe('deleting');

    clock.advance(20_000);
    expect(host.updates).toBe(1);
    expect(c.isDeleting(agents[0])).toBe(false);
    expect(c.view(agents[0])).toMatchObject({ state: 'failed', code: 'abandoned' });
    expect(c.isDeleting(agents[1])).toBe(true);

    // The host's re-render re-arms for the next lease.
    host.updated();
    expect(clock.timers.size).toBe(1);
    expect([...clock.timers.values()][0].at).toBe(T0 + 50_000);
  });

  it('re-arms when a renewal pushes the lease out', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent: Pick<Agent, 'deletion'> = { deletion: deleting(T0 + 20_000) };
    const c = new DeletionLeaseController(host, () => [agent], clock);
    host.updated();

    clock.advance(15_000);
    agent.deletion = deleting(T0 + 40_000); // renewal delta
    host.updated();
    expect(clock.timers.size).toBe(1);
    expect([...clock.timers.values()][0].at).toBe(T0 + 40_000);

    clock.advance(10_000); // past the old lease
    expect(host.updates).toBe(0);
    expect(c.isDeleting(agent)).toBe(true);

    clock.advance(15_000);
    expect(host.updates).toBe(1);
    expect(c.isDeleting(agent)).toBe(false);
  });

  it('keeps the same timer across unrelated host updates', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent = { deletion: deleting(T0 + 20_000) };
    new DeletionLeaseController(host, () => [agent], clock);
    host.updated();
    const [id] = [...clock.timers.keys()];
    host.updated();
    host.updated();
    expect([...clock.timers.keys()]).toEqual([id]);
  });

  it('disarms when the deletion clears or the host disconnects', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent: Pick<Agent, 'deletion'> = { deletion: deleting(T0 + 20_000) };
    const c = new DeletionLeaseController(host, () => [agent], clock);
    host.updated();
    agent.deletion = null;
    host.updated();
    expect(clock.timers.size).toBe(0);

    agent.deletion = deleting(T0 + 20_000, 2);
    host.updated();
    expect(clock.timers.size).toBe(1);
    c.hostDisconnected();
    expect(clock.timers.size).toBe(0);
  });
});

describe('scion-deletion-badge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function render(d: DeletionInfo | null): Promise<ScionDeletionBadge> {
    const el = document.createElement('scion-deletion-badge');
    el.deletion = d;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders nothing and is hidden for null', async () => {
    const el = await render(null);
    expect(el.hasAttribute('hidden')).toBe(true);
    expect(el.shadowRoot?.querySelector('.badge')).toBeNull();
  });

  it('renders Deleting… and clears when deletion becomes null', async () => {
    const el = await render(deleting(T0 + 20_000));
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(badge?.textContent?.trim()).toBe('Deleting…');
    expect(badge?.getAttribute('data-state')).toBe('deleting');
    expect(el.hasAttribute('hidden')).toBe(false);

    el.deletion = null;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.badge')).toBeNull();
    expect(el.hasAttribute('hidden')).toBe(true);
  });

  it('renders Delete failed with the error message', async () => {
    const el = await render({
      ...deleting(T0),
      state: 'failed',
      code: 'runtime_error',
      error: 'broker refused',
    });
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(badge?.textContent?.trim()).toBe('Delete failed: broker refused');
    expect(badge?.classList.contains('failed')).toBe(true);
  });
});

describe('DeletionLeaseController deadlines beyond the lease', () => {
  it('flips to abandoned at the lease, then hides the view at lease + 15m', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent: Pick<Agent, 'deletion'> = { deletion: deleting(T0 + 20_000) };
    const c = new DeletionLeaseController(host, () => [agent], clock);
    host.updated();

    clock.advance(20_000);
    expect(host.updates).toBe(1);
    expect(c.view(agent)?.code).toBe('abandoned');

    host.updated(); // re-render after the flip arms the expiry timer
    expect([...clock.timers.values()].map((t) => t.at)).toEqual([T0 + 20_000 + 15 * 60_000]);
    clock.advance(15 * 60_000);
    expect(host.updates).toBe(2);
    expect(c.view(agent)).toBeNull();
    host.updated();
    expect(clock.timers.size).toBe(0);
  });

  it('hides a failed view at its expiresAt', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent: Pick<Agent, 'deletion'> = {
      deletion: {
        state: 'failed',
        code: 'conflict',
        soft: false,
        claim: 1,
        startedAt: iso(T0),
        expiresAt: iso(T0 + 60_000),
      },
    };
    const c = new DeletionLeaseController(host, () => [agent], clock);
    host.updated();
    expect(c.view(agent)?.state).toBe('failed');
    clock.advance(59_999);
    expect(host.updates).toBe(0);
    clock.advance(1);
    expect(host.updates).toBe(1);
    expect(c.view(agent)).toBeNull();
  });

  it('never arms for an in_doubt failure with no expiresAt', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent: Pick<Agent, 'deletion'> = {
      deletion: { state: 'failed', code: 'in_doubt', soft: false, claim: 1, startedAt: iso(T0) },
    };
    new DeletionLeaseController(host, () => [agent], clock);
    host.updated();
    expect(clock.timers.size).toBe(0);
  });

  it('clamps a far-future deadline to the setTimeout maximum (no immediate fire)', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const farLease = T0 + 2 ** 33; // overflows a 32-bit setTimeout delay
    const agent = { deletion: deleting(farLease) };
    const delays: number[] = [];
    const recording: DeletionClock = {
      now: () => clock.now(),
      setTimeout: (fn, ms) => {
        delays.push(ms);
        return clock.setTimeout(fn, ms);
      },
      clearTimeout: (h) => clock.clearTimeout(h),
    };
    const c = new DeletionLeaseController(host, () => [agent], recording);
    host.updated();
    expect(delays).toEqual([2 ** 31 - 1]);
    clock.advance(0);
    expect(host.updates).toBe(0);
    // When the clamped timer fires early, the re-render re-arms for the rest.
    clock.advance(2 ** 31 - 1);
    expect(host.updates).toBe(1);
    expect(c.isDeleting(agent)).toBe(true);
    host.updated();
    // The remainder is still above the cap, so it is clamped again.
    expect(farLease - clock.now()).toBeGreaterThan(2 ** 31 - 1);
    expect(delays[1]).toBe(2 ** 31 - 1);
  });

  it('arms on hostConnected, before the first update', () => {
    const clock = new FakeClock();
    const host = new FakeHost();
    const agent = { deletion: deleting(T0 + 20_000) };
    new DeletionLeaseController(host, () => [agent], clock);
    expect(clock.timers.size).toBe(0);
    for (const ctrl of host.controllers) ctrl.hostConnected?.();
    expect(clock.timers.size).toBe(1);
  });
});

describe('scion-deletion-badge live region', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('sets role="status" only when live', async () => {
    const d = deleting(T0 + 20_000);
    const quiet = document.createElement('scion-deletion-badge');
    quiet.deletion = d;
    const live = document.createElement('scion-deletion-badge');
    live.deletion = d;
    live.live = true;
    document.body.append(quiet, live);
    await quiet.updateComplete;
    await live.updateComplete;
    expect(quiet.shadowRoot?.querySelector('.badge')?.hasAttribute('role')).toBe(false);
    expect(quiet.shadowRoot?.querySelector('.badge')?.getAttribute('title')).toBe('Deleting…');
    expect(live.shadowRoot?.querySelector('.badge')?.getAttribute('role')).toBe('status');
  });
});
