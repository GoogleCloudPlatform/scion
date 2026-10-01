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
 * `agent-sort.ts`'s moved comparator equals today's two
 * inline `displayAgents` comparators (project-detail.ts and agents.ts) on
 * randomized data. The two pre-change blocks are snapshotted here, inline,
 * so a future edit to `agent-sort.ts` that silently changes behavior is
 * caught even though the originals are now deleted from the page files.
 */

import { describe, it, expect } from 'vitest';

import type { Agent } from './types.js';
import { getAgentDisplayStatus } from './types.js';
import { agentCompare, sortAgents, serverOrderCompare } from './agent-sort.js';
import type { AgentSortField, SortDir } from './agent-sort.js';

/** Snapshot of project-detail.ts's pre-change inline comparator (falls back to createdAt/updatedAt). */
function projectDetailCompare(
  a: Agent,
  b: Agent,
  sortField: AgentSortField,
  sortDir: SortDir
): number {
  let cmp = 0;
  switch (sortField) {
    case 'name':
      cmp = (a.name || '').localeCompare(b.name || '');
      break;
    case 'status':
      cmp = getAgentDisplayStatus(a).localeCompare(getAgentDisplayStatus(b));
      break;
    case 'created':
      cmp = (a.created || a.createdAt || '').localeCompare(b.created || b.createdAt || '');
      break;
    case 'updated':
      cmp = (
        a.lastActivityEvent && !a.lastActivityEvent.startsWith('0001')
          ? a.lastActivityEvent
          : a.updated || a.updatedAt || ''
      ).localeCompare(
        b.lastActivityEvent && !b.lastActivityEvent.startsWith('0001')
          ? b.lastActivityEvent
          : b.updated || b.updatedAt || ''
      );
      break;
  }
  return sortDir === 'asc' ? cmp : -cmp;
}

/** Snapshot of agents.ts's pre-change inline comparator (no createdAt/updatedAt fallback). */
function agentsPageCompare(
  a: Agent,
  b: Agent,
  sortField: AgentSortField,
  sortDir: SortDir
): number {
  let cmp = 0;
  switch (sortField) {
    case 'name':
      cmp = (a.name || '').localeCompare(b.name || '');
      break;
    case 'status':
      cmp = getAgentDisplayStatus(a).localeCompare(getAgentDisplayStatus(b));
      break;
    case 'created':
      cmp = (a.created || '').localeCompare(b.created || '');
      break;
    case 'updated':
      cmp = (
        a.lastActivityEvent && !a.lastActivityEvent.startsWith('0001')
          ? a.lastActivityEvent
          : a.updated || ''
      ).localeCompare(
        b.lastActivityEvent && !b.lastActivityEvent.startsWith('0001')
          ? b.lastActivityEvent
          : b.updated || ''
      );
      break;
  }
  return sortDir === 'asc' ? cmp : -cmp;
}

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

const PHASES: Agent['phase'][] = [
  'running',
  'stopped',
  'suspended',
  'error',
  'starting',
  'stopping',
];
const NAMES = ['alpha', 'Bravo', 'charlie', 'Delta-1', 'delta-2', 'Écho', ''];

function genAgent(rand: () => number, i: number): Agent {
  const name = NAMES[Math.floor(rand() * NAMES.length)];
  const created = `2026-0${1 + Math.floor(rand() * 9)}-${String(1 + Math.floor(rand() * 27)).padStart(2, '0')}T00:00:0${Math.floor(rand() * 9)}Z`;
  const hasActivity = rand() < 0.5;
  const zeroActivity = rand() < 0.15;
  return {
    id: `a-${i}`,
    name,
    projectId: 'p-1',
    template: 't',
    phase: PHASES[Math.floor(rand() * PHASES.length)],
    created,
    updated: `2026-0${1 + Math.floor(rand() * 9)}-${String(1 + Math.floor(rand() * 27)).padStart(2, '0')}T00:00:0${Math.floor(rand() * 9)}Z`,
    lastActivityEvent: zeroActivity
      ? '0001-01-01T00:00:00Z'
      : hasActivity
        ? `2026-0${1 + Math.floor(rand() * 9)}-${String(1 + Math.floor(rand() * 27)).padStart(2, '0')}T00:00:0${Math.floor(rand() * 9)}Z`
        : undefined,
    messageMode: 'project',
  } as Agent;
}

const FIELDS: AgentSortField[] = ['name', 'status', 'created', 'updated'];
const DIRS: SortDir[] = ['asc', 'desc'];

describe('agent-sort — moved comparator matches both pre-change inline blocks', () => {
  const rand = mulberry32(42);
  const agents = Array.from({ length: 80 }, (_, i) => genAgent(rand, i));

  // The O(n^2) pairwise comparisons below are real work, not a loop bug —
  // generous per-test timeouts avoid flaking under a loaded test runner.
  const PAIRWISE_TIMEOUT_MS = 30_000;

  for (const field of FIELDS) {
    for (const dir of DIRS) {
      it(
        `agentCompare matches project-detail.ts's original comparator for ${field}/${dir}`,
        () => {
          for (let i = 0; i < agents.length; i++) {
            for (let j = 0; j < agents.length; j++) {
              expect(Math.sign(agentCompare(agents[i], agents[j], field, dir))).toBe(
                Math.sign(projectDetailCompare(agents[i], agents[j], field, dir))
              );
            }
          }
        },
        PAIRWISE_TIMEOUT_MS
      );

      it(
        `agentCompare matches agents.ts's original comparator for ${field}/${dir} (createdAt/updatedAt unset)`,
        () => {
          // agents.ts never populated createdAt/updatedAt either; with those
          // fields absent (as in every real response) the two pre-change
          // blocks are behaviorally identical.
          for (let i = 0; i < agents.length; i++) {
            for (let j = 0; j < agents.length; j++) {
              expect(Math.sign(agentCompare(agents[i], agents[j], field, dir))).toBe(
                Math.sign(agentsPageCompare(agents[i], agents[j], field, dir))
              );
            }
          }
        },
        PAIRWISE_TIMEOUT_MS
      );

      it(`sortAgents(${field}, ${dir}) equals a stable sort by the original comparator`, () => {
        const expected = [...agents].sort((a, b) => projectDetailCompare(a, b, field, dir));
        const actual = sortAgents(agents, field, dir);
        expect(actual.map((a) => a.id)).toEqual(expected.map((a) => a.id));
      });
    }
  }

  describe('createdAt/updatedAt legacy fallback', () => {
    // project-detail.ts's pre-change comparator fell back to createdAt/
    // updatedAt when created/updated were absent; agents.ts's did not. The
    // main pairwise loop above never sets these fields (matching every real
    // API response, per the comment on `Agent` in ./types.ts), so it never
    // actually exercises that fallback branch. These agents do, and are
    // compared against `projectDetailCompare` only — `agentsPageCompare`
    // must keep the fields unset, which it already does by construction.
    const fallbackRand = mulberry32(7);
    const fallbackAgents: Agent[] = Array.from({ length: 24 }, (_, i) => {
      const a = genAgent(fallbackRand, i);
      const { created, updated, ...rest } = a;
      return {
        ...rest,
        createdAt: created,
        updatedAt: updated,
      } as Agent;
    });

    for (const field of ['created', 'updated'] as const) {
      for (const dir of DIRS) {
        it(`agentCompare matches project-detail.ts's createdAt/updatedAt fallback for ${field}/${dir}`, () => {
          for (let i = 0; i < fallbackAgents.length; i++) {
            for (let j = 0; j < fallbackAgents.length; j++) {
              expect(
                Math.sign(agentCompare(fallbackAgents[i], fallbackAgents[j], field, dir))
              ).toBe(
                Math.sign(projectDetailCompare(fallbackAgents[i], fallbackAgents[j], field, dir))
              );
            }
          }
        });
      }
    }
  });

  it('is a stable sort: ties preserve created-desc, id-desc REST order (design §4.2)', () => {
    const tied: Agent[] = [
      {
        id: 'z',
        name: 'same',
        projectId: 'p',
        template: 't',
        phase: 'running',
        created: 'c',
        updated: 'u',
        messageMode: 'project',
      } as Agent,
      {
        id: 'y',
        name: 'same',
        projectId: 'p',
        template: 't',
        phase: 'running',
        created: 'c',
        updated: 'u',
        messageMode: 'project',
      } as Agent,
      {
        id: 'x',
        name: 'same',
        projectId: 'p',
        template: 't',
        phase: 'running',
        created: 'c',
        updated: 'u',
        messageMode: 'project',
      } as Agent,
    ];
    expect(sortAgents(tied, 'name', 'asc').map((a) => a.id)).toEqual(['z', 'y', 'x']);
    expect(sortAgents(tied, 'name', 'desc').map((a) => a.id)).toEqual(['z', 'y', 'x']);
  });
});

describe('agent-sort — serverOrderCompare (design §4.2 total order)', () => {
  const base = {
    id: 'a',
    name: 'n',
    projectId: 'p',
    template: 't',
    phase: 'running',
    messageMode: 'project',
  } as Agent;

  it('orders primarily by the updated key, honoring dir', () => {
    const older = { ...base, id: 'older', updated: '2026-01-01T00:00:00Z' } as Agent;
    const newer = { ...base, id: 'newer', updated: '2026-02-01T00:00:00Z' } as Agent;
    expect(serverOrderCompare(newer, older, 'desc')).toBeLessThan(0);
    expect(serverOrderCompare(older, newer, 'asc')).toBeLessThan(0);
  });

  it('breaks a tied key by created DESC, regardless of dir', () => {
    const a = { ...base, id: 'a', updated: 'same', created: '2026-02-01T00:00:00Z' } as Agent;
    const b = { ...base, id: 'b', updated: 'same', created: '2026-01-01T00:00:00Z' } as Agent;
    expect(serverOrderCompare(a, b, 'desc')).toBeLessThan(0);
    expect(serverOrderCompare(a, b, 'asc')).toBeLessThan(0);
  });

  it('breaks a tied key and created by id DESC, regardless of dir', () => {
    const a = { ...base, id: 'zzz', updated: 'same', created: 'same' } as Agent;
    const b = { ...base, id: 'aaa', updated: 'same', created: 'same' } as Agent;
    expect(serverOrderCompare(a, b, 'desc')).toBeLessThan(0);
    expect(serverOrderCompare(a, b, 'asc')).toBeLessThan(0);
  });

  it('is reflexive: the same id (identical key and created) compares equal to itself', () => {
    const a = { ...base, id: 'same-id', updated: 'k', created: 'c' } as Agent;
    const b = { ...base, id: 'same-id', updated: 'k', created: 'c' } as Agent;
    expect(serverOrderCompare(a, b, 'desc')).toBe(0);
    expect(serverOrderCompare(a, b, 'asc')).toBe(0);
    expect(serverOrderCompare(a, a, 'desc')).toBe(0);
  });
});
