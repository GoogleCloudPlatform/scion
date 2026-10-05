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
 * scion-principal-picker — group search filtering.
 *
 * Project members groups are system-managed and cannot be granted roles or
 * nested in another group, so the picker never offers them.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { PickerGroup } from './principal-picker.js';

const CANONICAL: PickerGroup = {
  id: 'g-canonical',
  name: 'Alpha Members',
  slug: 'project:alpha:members',
  projectId: 'p-alpha',
  annotations: { 'scion.io/project-members-group': 'true' },
};
const LEGACY: PickerGroup = {
  id: 'g-legacy',
  name: 'Beta Members',
  slug: 'project:beta:members',
  projectId: 'p-beta',
  annotations: { 'scion.io/system-project-members-group': 'true' },
};
const NORMAL: PickerGroup = { id: 'g-normal', name: 'Platform Team', slug: 'platform-team' };
const PROJECT_UNMARKED: PickerGroup = {
  id: 'g-project-unmarked',
  name: 'Alpha Reviewers',
  slug: 'alpha-reviewers',
  projectId: 'p-alpha',
};
const MARKER_FALSE: PickerGroup = {
  id: 'g-marker-false',
  name: 'Alpha Other',
  slug: 'alpha-other',
  projectId: 'p-alpha',
  annotations: { 'scion.io/project-members-group': 'false' },
};

// Conflicting markers: legacy "true" with canonical "false" is still a members
// group (either key set to "true" is enough).
const CONFLICTING: PickerGroup = {
  id: 'g-conflicting',
  name: 'Gamma Members',
  slug: 'project:gamma:members',
  projectId: 'p-gamma',
  annotations: {
    'scion.io/project-members-group': 'false',
    'scion.io/system-project-members-group': 'true',
  },
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let mod: any;

beforeAll(async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
  );
  mod = await import('./principal-picker.js');
  vi.restoreAllMocks();
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

describe('selectableGroups', () => {
  it('drops the canonical and the legacy marker', () => {
    expect(mod.selectableGroups([CANONICAL, LEGACY])).toEqual([]);
  });

  it('drops conflicting markers (legacy "true", canonical "false")', () => {
    expect(mod.selectableGroups([CONFLICTING])).toEqual([]);
  });

  it('keeps ordinary groups', () => {
    const noProject = { ...CANONICAL, projectId: undefined } as PickerGroup;
    expect(mod.selectableGroups([NORMAL, PROJECT_UNMARKED, MARKER_FALSE, noProject])).toEqual([
      NORMAL,
      PROJECT_UNMARKED,
      MARKER_FALSE,
      noProject,
    ]);
  });
});

describe('group search results', () => {
  it('drops project members groups and keeps the others', async () => {
    const all = [CANONICAL, NORMAL, LEGACY, PROJECT_UNMARKED, MARKER_FALSE, CONFLICTING];
    const fetchMock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ groups: all }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = new mod.ScionPrincipalPicker();
    el.principalType = 'group';
    document.body.appendChild(el);
    await el.updateComplete;

    await (el as unknown as { searchGroups(q: string): Promise<void> }).searchGroups('al');
    const results = (el as unknown as { groupSearchResults: PickerGroup[] }).groupSearchResults;
    expect(results.map((g) => g.id)).toEqual(['g-normal', 'g-project-unmarked', 'g-marker-false']);
    const url = String((fetchMock.mock.calls[0] as unknown[])[0]);
    expect(url).toContain(`limit=${mod.GROUP_SEARCH_LIMIT}`);
    expect(mod.GROUP_SEARCH_LIMIT).toBe(25);
  });
});
