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

import { agentRowText, buildAgentCandidate } from './agent-palette-candidate.js';

const slugs = new Map([
  ['p-alpha', 'alpha'],
  ['p-beta', 'beta'],
]);
const lookup = (projectId: string): string | undefined => slugs.get(projectId);

describe('agentRowText', () => {
  it('shows the project slug when the lookup knows it', () => {
    const row = agentRowText(
      {
        id: 'a0',
        name: 'coordinator',
        slug: 'coordinator',
        projectId: 'p-alpha',
        project: 'Alpha',
      },
      lookup
    );
    expect(row).toEqual({
      label: 'coordinator',
      secondaryLabel: 'alpha',
      searchFields: ['coordinator', 'alpha', 'Alpha'],
    });
  });

  it('gives same-named agents in different projects different second lines', () => {
    const a = agentRowText(
      { id: 'a0', name: 'coordinator', slug: 'coordinator', projectId: 'p-alpha' },
      lookup
    );
    const b = agentRowText(
      { id: 'a1', name: 'coordinator', slug: 'coordinator', projectId: 'p-beta' },
      lookup
    );
    expect(a.label).toBe(b.label);
    expect([a.secondaryLabel, b.secondaryLabel]).toEqual(['alpha', 'beta']);
  });

  it('falls back to the project name, never the project ID, while the slug is unknown', () => {
    const row = agentRowText(
      { id: 'a0', name: 'coordinator', projectId: 'p-gamma', project: 'Gamma' },
      lookup
    );
    expect(row.secondaryLabel).toBe('Gamma');
    expect(agentRowText({ id: 'a0', name: 'coordinator', projectId: 'p-gamma' }, lookup)).toEqual({
      label: 'coordinator',
      secondaryLabel: '',
      searchFields: ['coordinator'],
    });
  });

  it('shows the project name without a lookup', () => {
    const row = agentRowText({ id: 'a0', name: 'Coder', projectId: 'p-alpha', project: 'Alpha' });
    expect(row.secondaryLabel).toBe('Alpha');
  });

  it('falls back to an agent slug that differs from the label, and never repeats the label', () => {
    expect(agentRowText({ id: 'a0', name: 'Coder One', slug: 'coder-one' }).secondaryLabel).toBe(
      'coder-one'
    );
    expect(agentRowText({ id: 'a0', name: 'coder', slug: 'coder' }).secondaryLabel).toBe('');
  });
});

describe('buildAgentCandidate', () => {
  it('uses the project slug from the lookup for the second line', () => {
    const candidate = buildAgentCandidate(
      { id: 'a0', name: 'coordinator', projectId: 'p-beta', project: 'Beta' },
      lookup
    );
    expect(candidate.secondaryLabel).toBe('beta');
    expect(candidate.target).toEqual({ kind: 'agent', agentId: 'a0', displayName: 'coordinator' });
  });
});
