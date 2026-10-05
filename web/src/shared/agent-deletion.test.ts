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

import { describe, it, expect } from 'vitest';
import {
  deletionBadgeLabel,
  nextDeletionDeadline,
  DELETION_DISPLAY_TTL_MS,
  effectiveDeletion,
  isDeletionActive,
  readAcceptedDeletion,
  shouldApplyAcceptedDeletion,
} from './agent-deletion.js';
import type { DeletionInfo } from './types.js';

const T0 = Date.parse('2026-10-04T12:00:00Z');
const iso = (ms: number): string => new Date(ms).toISOString();

function deleting(overrides: Partial<DeletionInfo> = {}): DeletionInfo {
  return {
    state: 'deleting',
    soft: false,
    claim: 1,
    startedAt: iso(T0),
    leaseExpiresAt: iso(T0 + 60_000),
    ...overrides,
  };
}

describe('effectiveDeletion', () => {
  it('returns null for null/undefined', () => {
    expect(effectiveDeletion(null, T0)).toBeNull();
    expect(effectiveDeletion(undefined, T0)).toBeNull();
  });

  it('keeps a deleting view before its lease expires', () => {
    const d = deleting();
    expect(effectiveDeletion(d, T0 + 59_999)).toBe(d);
  });

  it('flips a deleting view to failed/abandoned at leaseExpiresAt', () => {
    const v = effectiveDeletion(deleting(), T0 + 60_000);
    expect(v?.state).toBe('failed');
    expect(v?.code).toBe('abandoned');
  });

  it('leaves failed views and lease-less deleting views alone', () => {
    const f: DeletionInfo = { ...deleting(), state: 'failed', code: 'conflict' };
    expect(effectiveDeletion(f, T0 + 1e9)).toBe(f);
    const noLease = deleting({ leaseExpiresAt: undefined });
    expect(effectiveDeletion(noLease, T0 + 1e9)).toBe(noLease);
  });
});

describe('isDeletionActive', () => {
  it('is true only while deleting with a live lease', () => {
    expect(isDeletionActive({ deletion: deleting() }, T0)).toBe(true);
    expect(isDeletionActive({ deletion: deleting() }, T0 + 60_000)).toBe(false);
    expect(isDeletionActive({ deletion: null }, T0)).toBe(false);
    expect(isDeletionActive({}, T0)).toBe(false);
    expect(
      isDeletionActive({ deletion: { ...deleting(), state: 'failed', code: 'conflict' } }, T0)
    ).toBe(false);
  });
});

describe('deletionBadgeLabel', () => {
  it('labels each state', () => {
    expect(deletionBadgeLabel(deleting())).toBe('Deleting…');
    const failed = (o: Partial<DeletionInfo>): DeletionInfo => ({
      ...deleting(),
      state: 'failed',
      ...o,
    });
    expect(deletionBadgeLabel(failed({ code: 'abandoned' }))).toBe('Delete interrupted');
    expect(deletionBadgeLabel(failed({ code: 'runtime_error', error: 'container stuck' }))).toBe(
      'Delete failed: container stuck'
    );
    expect(deletionBadgeLabel(failed({ code: 'in_doubt' }))).toBe('Delete failed: outcome unknown');
    expect(deletionBadgeLabel(failed({ code: 'some_new_code' }))).toBe(
      'Delete failed: some new code'
    );
    expect(deletionBadgeLabel(failed({}))).toBe('Delete failed');
  });
});

describe('nextDeletionDeadline', () => {
  it('returns the earliest future lease among deleting agents', () => {
    const agents = [
      { deletion: deleting({ leaseExpiresAt: iso(T0 + 30_000) }) },
      { deletion: deleting({ leaseExpiresAt: iso(T0 + 10_000) }) },
      { deletion: deleting({ leaseExpiresAt: iso(T0 - 5_000) }) }, // already past
      { deletion: { ...deleting({ leaseExpiresAt: iso(T0 + 1_000) }), state: 'failed' as const } },
      { deletion: null },
      {},
    ];
    expect(nextDeletionDeadline(agents, T0)).toBe(T0 + 10_000);
  });

  it('returns null when nothing needs a timer', () => {
    expect(nextDeletionDeadline([{ deletion: null }], T0)).toBeNull();
    expect(nextDeletionDeadline([], T0)).toBeNull();
  });
});

describe('shouldApplyAcceptedDeletion', () => {
  it('applies when there is no current view or the claim is newer', () => {
    expect(shouldApplyAcceptedDeletion(null, deleting())).toBe(true);
    expect(shouldApplyAcceptedDeletion(undefined, deleting())).toBe(true);
    expect(shouldApplyAcceptedDeletion(deleting({ claim: 1 }), deleting({ claim: 2 }))).toBe(true);
  });

  it('keeps an SSE view for the same or a newer claim', () => {
    const failed: DeletionInfo = { ...deleting({ claim: 3 }), state: 'failed', code: 'conflict' };
    expect(shouldApplyAcceptedDeletion(failed, deleting({ claim: 3 }))).toBe(false);
    expect(shouldApplyAcceptedDeletion(deleting({ claim: 4 }), deleting({ claim: 3 }))).toBe(false);
  });
});

describe('readAcceptedDeletion', () => {
  const res = (body: unknown): Response =>
    ({ json: () => Promise.resolve(body) }) as unknown as Response;

  it('reads the deletion from a 202 body', async () => {
    const d = deleting();
    expect(await readAcceptedDeletion(res({ agentId: 'a', deletion: d }))).toEqual(d);
  });

  it('returns null for missing or malformed bodies', async () => {
    expect(await readAcceptedDeletion(res(null))).toBeNull();
    expect(await readAcceptedDeletion(res({ agentId: 'a' }))).toBeNull();
    expect(await readAcceptedDeletion(res({ deletion: 'x' }))).toBeNull();
    const bad = { json: () => Promise.reject(new Error('bad json')) } as unknown as Response;
    expect(await readAcceptedDeletion(bad)).toBeNull();
  });
});

describe('failed-view expiry (expiresAt)', () => {
  const failedView = (o: Partial<DeletionInfo> = {}): DeletionInfo => ({
    ...deleting(),
    state: 'failed',
    code: 'runtime_error',
    leaseExpiresAt: undefined,
    expiresAt: iso(T0 + 15 * 60_000),
    ...o,
  });

  it('a failed view disappears at expiresAt', () => {
    const f = failedView();
    expect(effectiveDeletion(f, T0 + 15 * 60_000 - 1)).toBe(f);
    expect(effectiveDeletion(f, T0 + 15 * 60_000)).toBeNull();
    expect(isDeletionActive({ deletion: f }, T0 + 15 * 60_000)).toBe(false);
  });

  it('in_doubt and other failed views without expiresAt never expire', () => {
    const f = failedView({ code: 'in_doubt', expiresAt: undefined });
    expect(effectiveDeletion(f, T0 + 1e12)).toBe(f);
  });

  it('the client-flipped abandoned view expires DELETION_DISPLAY_TTL_MS after the lease', () => {
    const lease = T0 + 60_000;
    const d = deleting({ leaseExpiresAt: iso(lease) });
    expect(DELETION_DISPLAY_TTL_MS).toBe(15 * 60_000);
    const flipped = effectiveDeletion(d, lease);
    expect(flipped).toMatchObject({
      state: 'failed',
      code: 'abandoned',
      expiresAt: iso(lease + DELETION_DISPLAY_TTL_MS),
    });
    expect(effectiveDeletion(d, lease + DELETION_DISPLAY_TTL_MS - 1)?.code).toBe('abandoned');
    expect(effectiveDeletion(d, lease + DELETION_DISPLAY_TTL_MS)).toBeNull();
  });

  it('nextDeletionDeadline includes failed expiresAt and the flipped view expiry', () => {
    // A failed view's expiry is the only deadline.
    expect(nextDeletionDeadline([{ deletion: failedView() }], T0)).toBe(T0 + 15 * 60_000);
    // An expired failure and an in_doubt failure need no timer.
    expect(
      nextDeletionDeadline(
        [
          { deletion: failedView({ expiresAt: iso(T0 - 1) }) },
          { deletion: failedView({ code: 'in_doubt', expiresAt: undefined }) },
        ],
        T0
      )
    ).toBeNull();
    // After the lease passes, the next deadline is the abandoned view's expiry.
    const lease = T0 + 60_000;
    const d = { deletion: deleting({ leaseExpiresAt: iso(lease) }) };
    expect(nextDeletionDeadline([d], T0)).toBe(lease);
    expect(nextDeletionDeadline([d], lease)).toBe(lease + DELETION_DISPLAY_TTL_MS);
    expect(nextDeletionDeadline([d], lease + DELETION_DISPLAY_TTL_MS)).toBeNull();
    // Earliest wins across kinds.
    expect(
      nextDeletionDeadline([{ deletion: failedView({ expiresAt: iso(T0 + 5_000) }) }, d], T0)
    ).toBe(T0 + 5_000);
  });
});
