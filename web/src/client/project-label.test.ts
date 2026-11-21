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

import { describe, it, expect, afterEach, beforeEach } from 'vitest';

import { stateManager } from './state.js';
import type { Project } from '../shared/types.js';
import { cachedProjectLabel, findCachedProject, projectDisplayName } from './project-label.js';

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
