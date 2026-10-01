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
import { AgentMemberIndex } from './agent-member-index.js';

describe('AgentMemberIndex', () => {
  it('seeds from stats.agents entries and reports total/running stats', () => {
    const idx = new AgentMemberIndex();
    idx.seed([
      ['a', 'running'],
      ['b', 'stopped'],
      ['c', 'running'],
    ]);
    expect(idx.size).toBe(3);
    expect(idx.stats).toEqual({ total: 3, running: 2 });
    expect(idx.getPhase('a')).toBe('running');
    expect(idx.has('b')).toBe(true);
    expect(idx.has('z')).toBe(false);
  });

  it('set() adds or updates a member and is reflected in stats', () => {
    const idx = new AgentMemberIndex();
    idx.seed([['a', 'stopped']]);
    idx.set('a', 'running');
    idx.set('b', 'running');
    expect(idx.stats).toEqual({ total: 2, running: 2 });
  });

  it('delete() is idempotent — deleting an absent ID is a safe no-op (deleted is a safe superset)', () => {
    const idx = new AgentMemberIndex();
    idx.seed([['a', 'running']]);
    idx.delete('never-seen');
    expect(idx.stats).toEqual({ total: 1, running: 1 });
    idx.delete('a');
    idx.delete('a');
    expect(idx.stats).toEqual({ total: 0, running: 0 });
  });

  it('seed() replaces the whole index', () => {
    const idx = new AgentMemberIndex();
    idx.seed([
      ['a', 'running'],
      ['b', 'running'],
    ]);
    idx.seed([['c', 'stopped']]);
    expect(idx.has('a')).toBe(false);
    expect(idx.has('c')).toBe(true);
    expect(idx.stats).toEqual({ total: 1, running: 0 });
  });

  it('ids() iterates every member id', () => {
    const idx = new AgentMemberIndex();
    idx.seed([
      ['a', 'running'],
      ['b', 'stopped'],
    ]);
    expect(new Set(idx.ids())).toEqual(new Set(['a', 'b']));
  });
});
