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
 * `mergeChanged` (design §6.2, §7): applying a coalesced `agents-changed`
 * payload to a held agent array with identity preserved.
 */

import { describe, it, expect } from 'vitest';
import type { Agent, Capabilities } from '../shared/types.js';
import type { AgentsChangedDetail } from './state.js';
import { mergeChanged, dropTombstoned } from './agent-merge.js';

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    ...overrides,
  } as Agent;
}

function changeOf(partial: Partial<AgentsChangedDetail> = {}): AgentsChangedDetail {
  return {
    upserted: [],
    deleted: [],
    unknown: new Map(),
    generation: 0,
    ...partial,
  };
}

describe('mergeChanged', () => {
  it('returns the same array reference when nothing changed', () => {
    const held = [agent('a1'), agent('a2')];
    const result = mergeChanged(held, changeOf(), { getAgent: () => undefined });
    expect(result).toBe(held);
  });

  it('returns the same array reference when an upserted ID has no full agent yet', () => {
    // A delta that raced a delete, or belongs to a generation `getAgent`
    // (stateManager) no longer has an entry for.
    const held = [agent('a1')];
    const result = mergeChanged(held, changeOf({ upserted: ['ghost'] }), {
      getAgent: () => undefined,
    });
    expect(result).toBe(held);
  });

  it('unchanged agents stay the same object (===) when another agent is upserted', () => {
    const a1 = agent('a1');
    const a2 = agent('a2');
    const a2Updated = agent('a2', { phase: 'stopped' });
    const held = [a1, a2];

    const result = mergeChanged(held, changeOf({ upserted: ['a2'] }), {
      getAgent: (id) => (id === 'a2' ? a2Updated : undefined),
    });

    expect(result).not.toBe(held); // a new array, since something changed
    expect(result.find((a) => a.id === 'a1')).toBe(a1); // untouched identity preserved
    expect(result.find((a) => a.id === 'a2')).toBe(a2Updated);
  });

  it('an upsert whose full object is already === the held one is a no-op', () => {
    const a1 = agent('a1');
    const held = [a1];
    const result = mergeChanged(held, changeOf({ upserted: ['a1'] }), {
      getAgent: () => a1,
    });
    expect(result).toBe(held);
  });

  it('applies a delete, dropping the row and changing the array identity', () => {
    const a1 = agent('a1');
    const a2 = agent('a2');
    const held = [a1, a2];

    const result = mergeChanged(held, changeOf({ deleted: ['a1'] }), {
      getAgent: () => undefined,
    });

    expect(result).not.toBe(held);
    expect(result.map((a) => a.id)).toEqual(['a2']);
    expect(result[0]).toBe(a2);
  });

  it('a delete for an ID not currently held is a safe no-op (no array change)', () => {
    const held = [agent('a1')];
    const result = mergeChanged(held, changeOf({ deleted: ['ghost'] }), {
      getAgent: () => undefined,
    });
    expect(result).toBe(held);
  });

  it('adds a genuinely new ID under the page add rule', () => {
    const a1 = agent('a1');
    const a2 = agent('a2');
    const held = [a1];

    const result = mergeChanged(held, changeOf({ upserted: ['a2'] }), {
      getAgent: (id) => (id === 'a2' ? a2 : undefined),
      shouldAdd: () => true,
    });

    expect(result.map((a) => a.id).sort()).toEqual(['a1', 'a2']);
    expect(result.find((a) => a.id === 'a1')).toBe(a1);
    expect(result.find((a) => a.id === 'a2')).toBe(a2);
  });

  it("a new ID outside the page's add rule is not added (today's rule, design §6.2)", () => {
    const held = [agent('a1')];
    const outside = agent('a2', { projectId: 'other-project' });

    const result = mergeChanged(held, changeOf({ upserted: ['a2'] }), {
      getAgent: (id) => (id === 'a2' ? outside : undefined),
      shouldAdd: (a) => a.projectId === 'p-1',
    });

    expect(result).toBe(held);
  });

  it('an existing member is still updated in place even when shouldAdd would now reject it', () => {
    // §6.2: the add rule only gates *new* members; an existing one keeps
    // getting its updates (e.g. it left scope but a `deleted` has not
    // arrived yet).
    const existing = agent('a1');
    const updated = agent('a1', { projectId: 'other-project', phase: 'stopped' });
    const held = [existing];

    const result = mergeChanged(held, changeOf({ upserted: ['a1'] }), {
      getAgent: () => updated,
      shouldAdd: (a) => a.projectId === 'p-1',
    });

    expect(result[0]).toBe(updated);
  });

  it('inherits scope capabilities onto a brand-new agent with none of its own', () => {
    const caps: Capabilities = { actions: ['read'] };
    const created = agent('a2'); // no _capabilities, as an SSE-created delta carries none
    const held: Agent[] = [];

    const result = mergeChanged(held, changeOf({ upserted: ['a2'] }), {
      getAgent: () => created,
      scopeCapabilities: caps,
    });

    expect(result[0]._capabilities).toBe(caps);
    expect(created._capabilities).toBeUndefined(); // the input object itself is not mutated
  });

  it('does not override an existing agent that already has its own capabilities', () => {
    const caps: Capabilities = { actions: ['read'] };
    const otherCaps: Capabilities = { actions: ['read', 'update'] };
    const updated = agent('a1', { _capabilities: otherCaps, phase: 'stopped' });
    const held = [agent('a1', { _capabilities: otherCaps })];

    const result = mergeChanged(held, changeOf({ upserted: ['a1'] }), {
      getAgent: () => updated,
      scopeCapabilities: caps,
    });

    expect(result[0]._capabilities).toBe(otherCaps);
  });

  it('carries an existing member capabilities forward when the incoming update has none of its own', () => {
    // The regression this guards: an SSE-created agent gets scope
    // capabilities inherited onto the page's own copy (a new ID, via
    // `scopeCapabilities` below), but `stateManager`'s own object for that
    // ID never gained them, since inheritance is a page-level display
    // concern. The agent's next delta must not silently drop the
    // capabilities the user could already act on.
    const caps: Capabilities = { actions: ['stop', 'delete'] };
    const created = agent('a1', { _capabilities: caps }); // held after an earlier new-ID upsert.
    const nextDelta = agent('a1', { phase: 'stopped' }); // no _capabilities of its own.
    const held = [created];

    const result = mergeChanged(held, changeOf({ upserted: ['a1'] }), {
      getAgent: () => nextDelta,
    });

    expect(result[0].phase).toBe('stopped'); // the delta's own fields still apply.
    expect(result[0]._capabilities).toBe(caps); // capabilities are not dropped.
    expect(result[0]).not.toBe(created); // still a new object (something changed).
  });

  it('does not inherit scope capabilities onto a new agent that already carries its own', () => {
    const caps: Capabilities = { actions: ['read'] };
    const ownCaps: Capabilities = { actions: ['manage'] };
    const created = agent('a2', { _capabilities: ownCaps });

    const result = mergeChanged([], changeOf({ upserted: ['a2'] }), {
      getAgent: () => created,
      scopeCapabilities: caps,
    });

    expect(result[0]._capabilities).toBe(ownCaps);
  });

  it('applies deletes and upserts together in one call', () => {
    const a1 = agent('a1');
    const a3 = agent('a3');
    const a2Updated = agent('a2', { phase: 'stopped' });
    const held = [a1, agent('a2')];

    const result = mergeChanged(held, changeOf({ upserted: ['a2', 'a3'], deleted: ['a1'] }), {
      getAgent: (id) => ({ a2: a2Updated, a3 })[id],
      shouldAdd: () => true,
    });

    expect(result.map((a) => a.id).sort()).toEqual(['a2', 'a3']);
    expect(result.find((a) => a.id === 'a2')).toBe(a2Updated);
    expect(result.find((a) => a.id === 'a3')).toBe(a3);
  });

  it('ignores unknown-ID deltas: there is no full agent to adopt yet', () => {
    const held = [agent('a1')];
    const result = mergeChanged(
      held,
      changeOf({ unknown: new Map([['ghost', { phase: 'error' }]]) }),
      { getAgent: () => undefined }
    );
    expect(result).toBe(held);
  });

  it('a burst of many deltas in one flush yields exactly one merged array', () => {
    const base = Array.from({ length: 20 }, (_, i) => agent(`a${i}`));
    const updated2 = agent('a2', { phase: 'stopped' });
    const updated7 = agent('a7', { phase: 'error' });
    const created = agent('a20');
    const held = base;

    const result = mergeChanged(
      held,
      changeOf({ upserted: ['a2', 'a7', 'a20'], deleted: ['a5'] }),
      {
        getAgent: (id) => ({ a2: updated2, a7: updated7, a20: created })[id],
        shouldAdd: () => true,
      }
    );

    expect(result).toHaveLength(20); // 20 - 1 deleted + 1 created
    expect(result.find((a) => a.id === 'a5')).toBeUndefined();
    expect(result.find((a) => a.id === 'a2')).toBe(updated2);
    expect(result.find((a) => a.id === 'a7')).toBe(updated7);
    expect(result.find((a) => a.id === 'a20')).toBe(created);
    // Every other agent keeps its exact original reference.
    for (const a of base) {
      if (['a2', 'a5', 'a7'].includes(a.id)) continue;
      expect(result.find((r) => r.id === a.id)).toBe(a);
    }
  });

  /**
   * Pins the identity-preservation contract directly: `mergeChanged` must
   * carry an untouched object through by reference, never a rebuilt copy.
   * A local mutation that replaced the final `Array.from(byId.values())`
   * with `.map((a) => ({ ...a }))` was run against this suite by hand and
   * failed exactly this test plus six others that assert `.toBe(...)` on an
   * object `mergeChanged` is supposed to pass through untouched.
   */
  it('carries the untouched object through by reference, not a structurally-equal copy', () => {
    const a1 = agent('a1');
    const held = [a1];
    const result = mergeChanged(held, changeOf({ upserted: ['a2'] }), {
      getAgent: (id) => (id === 'a2' ? agent('a2') : undefined),
      shouldAdd: () => true,
    });
    const untouched = result.find((a) => a.id === 'a1')!;
    expect(untouched).toBe(a1); // reference equality, not just deep equality
    expect(untouched).toEqual(a1);
  });
});

describe('dropTombstoned', () => {
  it('returns the same array reference when there are no deleted IDs at all', () => {
    const held = [agent('a1'), agent('a2')];
    expect(dropTombstoned(held, new Set())).toBe(held);
  });

  it('returns the same array reference when no held agent is tombstoned', () => {
    const held = [agent('a1'), agent('a2')];
    expect(dropTombstoned(held, new Set(['ghost']))).toBe(held);
  });

  it('drops a tombstoned agent and returns a new array', () => {
    const a1 = agent('a1');
    const a2 = agent('a2');
    const held = [a1, a2];
    const result = dropTombstoned(held, new Set(['a1']));
    expect(result).not.toBe(held);
    expect(result).toEqual([a2]);
    expect(result[0]).toBe(a2); // the surviving agent keeps its reference
  });

  it('drops every tombstoned agent when more than one is present', () => {
    const held = [agent('a1'), agent('a2'), agent('a3')];
    const result = dropTombstoned(held, new Set(['a1', 'a3']));
    expect(result.map((a) => a.id)).toEqual(['a2']);
  });
});
