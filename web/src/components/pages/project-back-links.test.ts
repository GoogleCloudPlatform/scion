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
 * Tests for ptone/scion#8731: back-to-project links show the project display
 * name, falling back to slug (or known id/slug when uncached), and gracefully
 * to generic text when nothing is known.
 */

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render, type TemplateResult } from 'lit';

import { stateManager } from '../../client/state.js';
import type { Project } from '../../shared/types.js';

import './project-settings.js';
import './project-schedules.js';
import './agent-create.js';
import './template-detail.js';
import './harness-config-detail.js';
import './gcp-service-account-detail.js';
import './artifact-detail.js';

function clearProjects(): void {
  const internal = stateManager as unknown as {
    state: { projects: Map<string, Project>; deletedProjectIds: Set<string> };
  };
  internal.state.projects.clear();
  internal.state.deletedProjectIds.clear();
}

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'p-1',
    name: 'Payments',
    slug: 'payments',
    path: '/tmp/payments',
    status: 'active',
    agentCount: 0,
    ...overrides,
  };
}

describe('project back links (ptone/scion#8731)', () => {
  beforeEach(clearProjects);

  afterEach(() => {
    clearProjects();
    document.body.innerHTML = '';
  });

  describe('project-settings', () => {
    it('shows project display name and falls back to slug on back-to-project link', () => {
      const el = document.createElement('scion-page-project-settings');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        project: makeProject({ name: 'Payments Core', slug: 'payments-core' }),
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);

      const link = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(link).not.toBeNull();
      expect(link.getAttribute('href')).toBe('/projects/p-1');
      expect(link.textContent?.trim()).toBe('Payments Core');
      expect(link.getAttribute('title')).toBe('Payments Core');

      // Slug fallback when name is blank.
      Object.assign(el, {
        project: makeProject({ name: '  ', slug: 'payments-core' }),
      });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      const slugLink = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(slugLink.textContent?.trim()).toBe('payments-core');
      expect(slugLink.getAttribute('title')).toBe('payments-core');
    });

    it('falls back cleanly in error state using cached project, id, or generic label', () => {
      const el = document.createElement('scion-page-project-settings');
      Object.assign(el, { loading: false, projectId: '', project: null, error: 'boom' });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);

      const generic = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(generic.textContent?.trim()).toBe('Back to Project');

      Object.assign(el, { projectId: 'p-1' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(host.querySelector<HTMLAnchorElement>('a.back-link')!.textContent?.trim()).toBe('p-1');

      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Cached Project' })]);
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      const cached = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(cached.textContent?.trim()).toBe('Cached Project');
      expect(cached.getAttribute('title')).toBe('Cached Project');
    });
  });

  describe('project-schedules', () => {
    it('shows project display name and falls back to slug on back-to-project link', () => {
      const el = document.createElement('scion-page-project-schedules');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        project: { id: 'p-1', name: 'Nightly Ops', slug: 'nightly-ops' },
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);

      const link = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(link).not.toBeNull();
      expect(link.getAttribute('href')).toBe('/projects/p-1');
      expect(link.textContent?.trim()).toBe('Back to Nightly Ops');

      // Slug fallback when name is empty.
      Object.assign(el, {
        project: { id: 'p-1', name: '   ', slug: 'nightly-ops' },
      });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(host.querySelector<HTMLAnchorElement>('a.back-link')!.textContent?.trim()).toBe(
        'Back to nightly-ops'
      );
    });
  });

  describe('agent-create', () => {
    it('uses project display name and falls back to slug on back-to-project link', () => {
      const el = document.createElement('scion-page-agent-create');
      Object.assign(el, {
        loading: false,
        loadedOnce: true,
        projectFromUrl: true,
        projectId: 'p-1',
        projects: [makeProject({ id: 'p-1', name: '', slug: 'infra-tools' })],
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);

      const link = host.querySelector<HTMLAnchorElement>('a.back-link')!;
      expect(link).not.toBeNull();
      expect(link.getAttribute('href')).toBe('/projects/p-1');
      expect(link.textContent?.trim()).toBe('To infra-tools');
      expect(link.getAttribute('title')).toBe('infra-tools');
    });
  });

  describe('project-scoped resource detail pages', () => {
    it('template-detail shows cached project name, or falls back to projectId without fetching', () => {
      const el = document.createElement('scion-page-template-detail');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        template: {
          id: 't-1',
          name: 'default',
          slug: 'default',
          harness: 'claude',
          status: 'active',
          scope: 'project',
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      const uncached = Array.from(host.querySelectorAll('scion-back-link'));
      expect(uncached.map((l) => [l.getAttribute('href'), l.textContent?.trim()])).toEqual([
        ['/projects/p-1/settings?tab=templates', 'Templates'],
        ['/projects/p-1/settings', 'p-1 Settings'],
      ]);

      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Core Platform', slug: 'core' })]);
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      const cached = Array.from(host.querySelectorAll('scion-back-link'));
      expect(cached.map((l) => [l.getAttribute('href'), l.textContent?.trim()])).toEqual([
        ['/projects/p-1/settings?tab=templates', 'Templates'],
        ['/projects/p-1/settings', 'Core Platform Settings'],
      ]);
      expect(cached[1].hasAttribute('truncate')).toBe(true);
      expect(cached[1].getAttribute('tooltip')).toBe('Core Platform Settings');
    });

    it('harness-config-detail shows project slug fallback when name is empty', () => {
      stateManager.seedProjects([makeProject({ id: 'p-1', name: '', slug: 'edge-net' })]);
      const el = document.createElement('scion-page-harness-config-detail');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        harnessConfig: {
          id: 'h-1',
          name: 'claude',
          slug: 'claude',
          harness: 'claude',
          status: 'active',
          scope: 'project',
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      const links = Array.from(host.querySelectorAll('scion-back-link'));
      expect(links.map((l) => [l.getAttribute('href'), l.textContent?.trim()])).toEqual([
        ['/projects/p-1/settings?tab=harness-configs', 'Harness Configs'],
        ['/projects/p-1/settings', 'edge-net Settings'],
      ]);
      expect(links[1].hasAttribute('truncate')).toBe(true);
      expect(links[1].getAttribute('tooltip')).toBe('edge-net Settings');
    });

    it('gcp-service-account-detail shows project name in settings back link when cached', () => {
      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Data Mesh', slug: 'data-mesh' })]);
      const el = document.createElement('scion-page-gcp-service-account-detail');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        account: {
          id: 'sa-1',
          email: 'bot@proj.iam.gserviceaccount.com',
          verified: true,
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      const link = host.querySelector('scion-back-link')!;
      expect(link.getAttribute('href')).toBe('/projects/p-1/settings?tab=gcp-sa');
      expect(link.textContent?.trim()).toBe('Data Mesh Settings');
      expect(link.hasAttribute('truncate')).toBe(true);
      expect(link.getAttribute('tooltip')).toBe('Data Mesh Settings');
    });

    it('artifact-detail shows cached project name, or falls back to scopeRef', () => {
      const el = document.createElement('scion-page-artifact-detail');
      Object.assign(el, {
        data: {
          artifact: {
            id: 'a-1',
            ref: 'scion://artifact/a-1',
            scopeKind: 'project',
            scopeRef: 'p-1',
            ownerKind: 'user',
            ownerRef: 'u-1',
            title: 'RFC 1',
            currentSeq: 1,
            createdAt: '2026-01-01T00:00:00Z',
            updatedAt: '2026-01-01T00:00:00Z',
          },
          version: {
            seq: 1,
            ref: 'scion://artifact/a-1@1',
            kind: 'publish',
            entryPath: 'a.md',
            totalBytes: 5,
            fileCount: 1,
            createdAt: '2026-01-01T00:00:00Z',
            state: 'ready',
            files: [{ path: 'a.md', size: 5, sha256: 'ab', mediaType: 'text/markdown' }],
          },
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      expect(host.querySelector('scion-back-link')!.textContent?.trim()).toBe('p-1');

      stateManager.seedProjects([
        makeProject({ id: 'p-1', name: 'Design Docs', slug: 'design-docs' }),
      ]);
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      const link = host.querySelector('scion-back-link')!;
      expect(link.getAttribute('href')).toBe('/projects/p-1');
      expect(link.textContent?.trim()).toBe('Design Docs');
      expect(link.hasAttribute('truncate')).toBe(true);
      expect(link.getAttribute('tooltip')).toBe('Design Docs');
    });
  });
});
