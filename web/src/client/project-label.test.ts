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

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';

import { stateManager } from './state.js';
import type { Project } from '../shared/types.js';
import {
  cachedProjectLabel,
  findCachedProject,
  projectDisplayName,
  projectLabelNow,
  resetProjectLabelResolverForTests,
  resolveProjectLabel,
} from './project-label.js';

const apiFetchMock = vi.fn<typeof fetch>();
vi.mock('./api.js', () => ({
  apiFetch: (...args: Parameters<typeof fetch>) => apiFetchMock(...args),
}));

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'p-1',
    name: 'My Project',
    slug: 'my-project',
    path: '/tmp/my-project',
    status: 'active',
    agentCount: 0,
    ...overrides,
  };
}

function clearProjects(): void {
  const internal = stateManager as unknown as {
    state: { projects: Map<string, Project>; deletedProjectIds: Set<string> };
  };
  internal.state.projects.clear();
  internal.state.deletedProjectIds.clear();
}

describe('projectDisplayName', () => {
  it('prefers the trimmed display name', () => {
    expect(projectDisplayName({ name: '  Payments  ', slug: 'payments' })).toBe('Payments');
  });

  it('falls back to the trimmed slug when name is missing or blank', () => {
    expect(projectDisplayName({ name: '', slug: '  payments-api  ' })).toBe('payments-api');
    expect(projectDisplayName({ name: '   ', slug: 'payments-api' })).toBe('payments-api');
    expect(projectDisplayName({ slug: 'payments-api' })).toBe('payments-api');
  });

  it('falls back to the provided fallback when both name and slug are unavailable', () => {
    expect(projectDisplayName(null, 'Project')).toBe('Project');
    expect(projectDisplayName(undefined, 'Project')).toBe('Project');
    expect(projectDisplayName({ name: '  ', slug: '' }, 'Project')).toBe('Project');
    expect(projectDisplayName({ name: '', slug: '' })).toBe('');
  });
});

describe('findCachedProject', () => {
  beforeEach(clearProjects);
  afterEach(clearProjects);

  it('finds a cached project by id or slug', () => {
    stateManager.seedProjects([makeProject({ id: 'uuid-1', name: 'Billing', slug: 'billing' })]);
    expect(findCachedProject('uuid-1')?.name).toBe('Billing');
    expect(findCachedProject('billing')?.name).toBe('Billing');
    expect(findCachedProject('unknown')).toBeUndefined();
    expect(findCachedProject('')).toBeUndefined();
  });
});

describe('cachedProjectLabel', () => {
  beforeEach(clearProjects);
  afterEach(clearProjects);

  it('returns cached display name, then cached slug, then id/slug, then fallback', () => {
    stateManager.seedProjects([
      makeProject({ id: 'p-1', name: 'Core Platform', slug: 'core-platform' }),
      makeProject({ id: 'p-2', name: '  ', slug: 'edge-proxy' }),
    ]);

    expect(cachedProjectLabel('p-1', 'Project')).toBe('Core Platform');
    expect(cachedProjectLabel('core-platform', 'Project')).toBe('Core Platform');
    expect(cachedProjectLabel('p-2', 'Project')).toBe('edge-proxy');
    expect(cachedProjectLabel('p-uncached', 'Project')).toBe('p-uncached');
    expect(cachedProjectLabel('   ', 'Project')).toBe('Project');
    expect(cachedProjectLabel('')).toBe('');
  });
});

describe('projectLabelNow', () => {
  beforeEach(clearProjects);
  afterEach(clearProjects);

  it('returns cached name with pending=false and falls back to id with pending=true on miss', () => {
    stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Core Platform', slug: 'core' })]);

    expect(projectLabelNow('p-1')).toEqual({
      id: 'p-1',
      label: 'Core Platform',
      pending: false,
    });
    expect(projectLabelNow('p-miss')).toEqual({
      id: 'p-miss',
      label: 'p-miss',
      pending: true,
    });
    expect(projectLabelNow('  ')).toEqual({ id: '', label: '', pending: false });
  });
});

describe('resolveProjectLabel', () => {
  beforeEach(() => {
    clearProjects();
    resetProjectLabelResolverForTests();
    apiFetchMock.mockReset();
  });

  afterEach(() => {
    clearProjects();
    resetProjectLabelResolverForTests();
  });

  it('returns cached label without issuing a fetch', async () => {
    stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Cached Project' })]);
    const res = await resolveProjectLabel('p-1');
    expect(res).toEqual({ id: 'p-1', label: 'Cached Project', pending: false });
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('dedupes concurrent fetches and seeds stateManager without shrinking existing projects', async () => {
    stateManager.seedProjects([makeProject({ id: 'existing', name: 'Existing' })]);
    let resolveFetch!: (value: Response) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        })
    );

    const p1 = resolveProjectLabel('p-remote');
    const p2 = resolveProjectLabel('p-remote');
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/projects/p-remote',
      expect.objectContaining({ authErrors: 'caller', suppressAccessDeniedToast: true })
    );

    resolveFetch(
      new Response(JSON.stringify(makeProject({ id: 'p-remote', name: 'Remote Project' })), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const [r1, r2] = await Promise.all([p1, p2]);
    expect(r1).toEqual({ id: 'p-remote', label: 'Remote Project', pending: false });
    expect(r2).toEqual({ id: 'p-remote', label: 'Remote Project', pending: false });
    expect(stateManager.getProject('existing')?.name).toBe('Existing');
    expect(stateManager.getProject('p-remote')?.name).toBe('Remote Project');

    // Subsequent call uses the seeded cache with no extra fetch.
    const r3 = await resolveProjectLabel('p-remote');
    expect(r3).toEqual({ id: 'p-remote', label: 'Remote Project', pending: false });
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it.each([403, 404])(
    'falls back to id on HTTP %i and does not retry this session',
    async (code) => {
      apiFetchMock.mockResolvedValueOnce(new Response('denied', { status: code }));

      const first = await resolveProjectLabel('p-denied');
      expect(first).toEqual({ id: 'p-denied', label: 'p-denied', pending: false });
      expect(apiFetchMock).toHaveBeenCalledTimes(1);

      const second = await resolveProjectLabel('p-denied');
      expect(second).toEqual({ id: 'p-denied', label: 'p-denied', pending: false });
      expect(apiFetchMock).toHaveBeenCalledTimes(1);
    }
  );
});
