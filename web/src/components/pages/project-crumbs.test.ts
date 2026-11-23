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
 * Tests for project-family breadcrumb trails (ptone/scion#8281 Phase 2a,
 * preserving all ptone/scion#8731 invariants): project display name -> slug ->
 * ID fallback, truncation, URL encoding, origin-aware artifact trails, version
 * crumbs, and loading/error ancestor-only trails.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import { stateManager } from '../../client/state.js';
import { resetProjectLabelResolverForTests } from '../../client/project-label.js';
import type { Project } from '../../shared/types.js';
import type { Crumb, ScionBreadcrumb } from '../shared/breadcrumb.js';
import { crumbTuples } from '../../test-utils/crumbs.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

import './project-settings.js';
import './project-schedules.js';
import './project-decisions.js';
import './metrics-dashboard.js';
import './project-create.js';
import './agent-create.js';
import './agent-edit.js';
import './agent-graph.js';
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

function crumbsOf(root: ParentNode): readonly Crumb[] {
  return root.querySelector<ScionBreadcrumb>('scion-breadcrumb')?.crumbs ?? [];
}

describe('project-family breadcrumbs (ptone/scion#8281, #8731)', () => {
  beforeEach(() => {
    clearProjects();
    resetProjectLabelResolverForTests();
  });

  afterEach(() => {
    clearProjects();
    resetProjectLabelResolverForTests();
    document.body.innerHTML = '';
  });

  describe('project-settings', () => {
    it('shows Projects / {project} / Settings with name -> slug fallback and truncation', () => {
      const el = document.createElement('scion-page-project-settings');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        project: makeProject({ name: 'Payments Core', slug: 'payments-core' }),
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);

      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Payments Core', '/projects/p-1'],
        ['Settings', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);

      // Slug fallback when name is blank.
      Object.assign(el, {
        project: makeProject({ name: '  ', slug: 'payments-core' }),
      });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['payments-core', '/projects/p-1'],
        ['Settings', undefined],
      ]);
    });

    it('renders ancestor-only crumbs in loading, error, and noAccess states', () => {
      const el = document.createElement('scion-page-project-settings');
      const host = document.createElement('div');

      Object.assign(el, { loading: true, projectId: 'p-1', project: null, error: null });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['p-1', '/projects/p-1'],
      ]);
      expect(crumbsOf(host)[1].pending).toBe(true);

      Object.assign(el, { loading: false, projectId: '', project: null, error: 'boom' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([['Projects', '/projects']]);

      Object.assign(el, { projectId: 'p-1' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['p-1', '/projects/p-1'],
      ]);
      expect(crumbsOf(host)[1].pending).toBeUndefined();

      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Cached Project' })]);
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Cached Project', '/projects/p-1'],
      ]);

      Object.assign(el, { noAccess: true });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([['Projects', '/projects']]);
    });
  });

  describe('project-schedules', () => {
    it('shows Projects / {project} / Schedules with slug fallback and ancestor-only loading/error', () => {
      const el = document.createElement('scion-page-project-schedules');
      Object.assign(el, {
        loading: false,
        projectId: 'p-1',
        project: { id: 'p-1', name: 'Nightly Ops', slug: 'nightly-ops' },
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Nightly Ops', '/projects/p-1'],
        ['Schedules', undefined],
      ]);

      Object.assign(el, { project: { id: 'p-1', name: '   ', slug: 'nightly-ops' } });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['nightly-ops', '/projects/p-1'],
        ['Schedules', undefined],
      ]);

      Object.assign(el, { loading: false, project: null, error: 'boom' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['p-1', '/projects/p-1'],
      ]);
    });
  });

  describe('project-metrics', () => {
    it('shows Projects / {project} / Metrics for project metrics and none for hub metrics', () => {
      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Telemetry Core' })]);
      const el = document.createElement('scion-page-project-metrics');
      Object.assign(el, {
        projectId: 'p-1',
        loading: false,
        summary: {
          periodDays: 7,
          totalSessions: 1,
          totalApiCalls: 2,
          totalTokens: 3,
          uniqueAgents: 1,
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Telemetry Core', '/projects/p-1'],
        ['Metrics', undefined],
      ]);

      Object.assign(el, { error: 'boom' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Telemetry Core', '/projects/p-1'],
      ]);

      // Hub-level metrics (/metrics) has no breadcrumb.
      const hubEl = document.createElement('scion-page-metrics');
      Object.assign(hubEl, { projectId: '', loading: false });
      render((hubEl as unknown as { render: () => TemplateResult }).render(), host);
      expect(host.querySelector('scion-breadcrumb')).toBeNull();
    });
  });

  describe('project-create', () => {
    it('renders Projects / New project', () => {
      const el = document.createElement('scion-page-project-create');
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['New project', undefined],
      ]);
    });
  });

  describe('agent-create', () => {
    it('renders Projects / {project} / New agent when project-scoped and Agents / New agent otherwise', () => {
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
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['infra-tools', '/projects/p-1'],
        ['New agent', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);

      Object.assign(el, { projectFromUrl: false, projectId: '', projects: [] });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Agents', '/agents'],
        ['New agent', undefined],
      ]);
    });
  });

  describe('agent-edit', () => {
    it('renders Projects / {project} / {agent} / Edit in the back slot and renames bottom link to Cancel', () => {
      const el = document.createElement('scion-page-agent-edit');
      Object.assign(el, {
        loading: false,
        agentId: 'a-1',
        agent: {
          id: 'a-1',
          name: 'worker-1',
          projectId: 'p-1',
          project: 'Payments',
          phase: 'stopped',
          status: 'stopped',
        },
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Payments', '/projects/p-1'],
        ['worker-1', '/agents/a-1'],
        ['Edit', undefined],
      ]);
      const cancel = host.querySelector<HTMLAnchorElement>('[data-testid="back"]');
      expect(cancel?.textContent?.trim()).toBe('Cancel');
      expect(cancel?.getAttribute('href')).toBe('/agents/a-1');
    });
  });

  describe('agent-graph', () => {
    it('renders Projects / {project} / Agent graph when scoped and Agents / Graph when unscoped', () => {
      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Payments' })]);
      const el = document.createElement('scion-page-agent-graph');
      Object.assign(el, { loading: false, projectFilter: 'p-1' });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Payments', '/projects/p-1'],
        ['Agent graph', undefined],
      ]);

      Object.assign(el, { projectFilter: '' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Agents', '/agents'],
        ['Graph', undefined],
      ]);

      Object.assign(el, { loading: true });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([['Agents', '/agents']]);
    });
  });

  describe('project-scoped resource detail pages', () => {
    it('template-detail shows 5-crumb project trail with cached project name or pending id', () => {
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
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['p-1', '/projects/p-1'],
        ['Settings', '/projects/p-1/settings'],
        ['Templates', '/projects/p-1/settings?tab=templates'],
        ['default', undefined],
      ]);
      expect(crumbsOf(host)[1].pending).toBe(true);

      stateManager.seedProjects([makeProject({ id: 'p-1', name: 'Core Platform', slug: 'core' })]);
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Core Platform', '/projects/p-1'],
        ['Settings', '/projects/p-1/settings'],
        ['Templates', '/projects/p-1/settings?tab=templates'],
        ['default', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);
      expect(crumbsOf(host)[1].pending).toBeUndefined();
    });

    it('harness-config-detail shows 5-crumb project trail with slug fallback', () => {
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
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['edge-net', '/projects/p-1'],
        ['Settings', '/projects/p-1/settings'],
        ['Harness Configs', '/projects/p-1/settings?tab=harness-configs'],
        ['claude', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);
    });

    it('gcp-service-account-detail shows project-scoped and hub-scoped trails', () => {
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
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Data Mesh', '/projects/p-1'],
        ['Settings', '/projects/p-1/settings'],
        ['Service Accounts', '/projects/p-1/settings?tab=gcp-sa'],
        ['bot@proj.iam.gserviceaccount.com', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);

      Object.assign(el, { projectId: '' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Hub Resources', '/settings'],
        ['Service Accounts', '/settings?tab=service-accounts'],
        ['bot@proj.iam.gserviceaccount.com', undefined],
      ]);
    });

    it('artifact-detail shows project trail, origin-aware Artifacts trail, and Version crumb', () => {
      const el = document.createElement('scion-page-artifact-detail');
      Object.assign(el, {
        artifactId: 'a-1',
        data: {
          artifact: {
            id: 'a-1',
            ref: 'scion://artifact/a-1',
            scopeKind: 'project',
            scopeRef: 'p-1',
            ownerKind: 'user',
            ownerRef: 'u-1',
            title: 'RFC 1',
            currentSeq: 3,
            createdAt: '2026-01-01T00:00:00Z',
            updatedAt: '2026-01-01T00:00:00Z',
          },
          version: {
            seq: 3,
            ref: 'scion://artifact/a-1@3',
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
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['p-1', '/projects/p-1'],
        ['RFC 1', undefined],
      ]);

      stateManager.seedProjects([
        makeProject({ id: 'p-1', name: 'Design Docs', slug: 'design-docs' }),
      ]);
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Design Docs', '/projects/p-1'],
        ['RFC 1', undefined],
      ]);
      expect(crumbsOf(host)[1].truncate).toBe(true);

      // Historical version appends Version {seq} and links the title to latest.
      Object.assign(el, { seq: 2 });
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['Design Docs', '/projects/p-1'],
        ['RFC 1', '/projects/p-1/artifacts/a-1'],
        ['Version 2', undefined],
      ]);

      // Origin = artifacts keeps ?from=artifacts on the latest link.
      Object.assign(el, { origin: 'artifacts' });
      render((el as unknown as { renderHeader: () => TemplateResult }).renderHeader(), host);
      expect(crumbTuples(host)).toEqual([
        ['Artifacts', '/artifacts'],
        ['RFC 1', '/projects/p-1/artifacts/a-1?from=artifacts'],
        ['Version 2', undefined],
      ]);
    });
  });

  describe('URL-encodes projectId in breadcrumb hrefs', () => {
    const rawId = 'org/my project?v=1';
    const encodedId = encodeURIComponent(rawId);

    it('encodes projectId in project-settings loaded and error breadcrumbs', () => {
      const el = document.createElement('scion-page-project-settings');
      Object.assign(el, {
        loading: false,
        projectId: rawId,
        project: makeProject({ id: rawId, name: 'My Project' }),
      });
      const host = document.createElement('div');
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        ['My Project', `/projects/${encodedId}`],
        ['Settings', undefined],
      ]);

      Object.assign(el, { project: null, error: 'boom' });
      render((el as unknown as { render: () => TemplateResult }).render(), host);
      expect(crumbTuples(host)).toEqual([
        ['Projects', '/projects'],
        [rawId, `/projects/${encodedId}`],
      ]);
    });

    it('encodes projectId in template-detail and harness-config-detail breadcrumbs', () => {
      const tplEl = document.createElement('scion-page-template-detail');
      Object.assign(tplEl, {
        loading: false,
        projectId: rawId,
        template: {
          id: 't-1',
          name: 'default',
          slug: 'default',
          harness: 'claude',
          status: 'active',
          scope: 'project',
        },
      });
      const tplHost = document.createElement('div');
      render((tplEl as unknown as { renderHeader: () => TemplateResult }).renderHeader(), tplHost);
      expect(crumbTuples(tplHost)).toEqual([
        ['Projects', '/projects'],
        [rawId, `/projects/${encodedId}`],
        ['Settings', `/projects/${encodedId}/settings`],
        ['Templates', `/projects/${encodedId}/settings?tab=templates`],
        ['default', undefined],
      ]);

      const hcEl = document.createElement('scion-page-harness-config-detail');
      Object.assign(hcEl, {
        loading: false,
        projectId: rawId,
        harnessConfig: {
          id: 'h-1',
          name: 'claude',
          slug: 'claude',
          harness: 'claude',
          status: 'active',
          scope: 'project',
        },
      });
      const hcHost = document.createElement('div');
      render((hcEl as unknown as { renderHeader: () => TemplateResult }).renderHeader(), hcHost);
      expect(crumbTuples(hcHost)).toEqual([
        ['Projects', '/projects'],
        [rawId, `/projects/${encodedId}`],
        ['Settings', `/projects/${encodedId}/settings`],
        ['Harness Configs', `/projects/${encodedId}/settings?tab=harness-configs`],
        ['claude', undefined],
      ]);
    });
  });
});
