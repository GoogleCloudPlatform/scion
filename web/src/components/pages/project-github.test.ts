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
 * Tests for Project GitHub page shell (M1.5):
 * - Route renders `/projects/{id}/github` and page title
 * - Shared status header (connected repo, App handle, readiness/routing placeholders)
 * - Tabs (Agent access, Connection) and clean tab extension point
 * - Disconnected projects land on Connection with other tabs disabled ("connect first")
 * - Gate: hidden when remote is not on App host or App is not configured (ungated by hub.github_triggers)
 * - Sidebar nav entry and Project Detail header button
 * - Project Settings one-line link (connected / not connected / hidden)
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import { html } from 'lit';
import type { ProjectGitHubInfo } from '../../shared/types.js';
import { resetProjectGitHubRequestsForTests } from '../../client/project-github.js';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';
import {
  clearProjectGitHubTabsForTests,
  registerProjectGitHubTab,
  type ScionPageProjectGithub,
} from './project-github.js';
import type { ScionNav } from '../shared/nav.js';
import type { ScionPageProjectDetail } from './project-detail.js';
import type { ScionPageProjectSettings } from './project-settings.js';
import type { ScionApp } from '../app-shell.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeProjectGitHub(overrides?: Partial<ProjectGitHubInfo>): ProjectGitHubInfo {
  return {
    project_id: 'proj-gh-1',
    app: {
      configured: true,
      slug: 'scion-bot',
      web_host: 'github.com',
      installation_url: 'https://github.com/apps/scion-bot/installations/new',
    },
    remote: { owner: 'Acme', repo: 'api', on_app_host: true },
    association: {
      installation_id: 4412,
      account_login: 'acme',
      basis: 'admin_binding',
      write_allowed: true,
      bound_by: 'user-1',
      bound_by_name: 'Dana Admin',
      bound_at: '2026-11-20T12:00:00Z',
      authorized: true,
      verified: true,
      error_code: '',
      reason: '',
    },
    status: {
      state: 'ok',
      last_checked: '2026-11-25T10:00:00Z',
    },
    permissions: {
      mode: 'default',
      app_grant_known: true,
      requested: { contents: 'write', pull_requests: 'write', metadata: 'read' },
      app_granted: { contents: 'write', pull_requests: 'write', metadata: 'read' },
      effective: { contents: 'write', pull_requests: 'write', metadata: 'read' },
      capped: [],
      cap_reasons: {},
    },
    can: {
      bind: true,
      unbind: true,
      check: true,
      edit_permissions: true,
      edit_extra_repos: true,
      discover: true,
    },
    ...overrides,
  };
}

const GITHUB_PROJECT = {
  id: 'proj-gh-1',
  name: 'Acme API',
  slug: 'acme-api',
  gitRemote: 'https://github.com/Acme/api.git',
  status: 'active',
  _capabilities: { actions: ['read', 'update', 'manage'] },
};

function stubProjectFetch(githubInfo: ProjectGitHubInfo | null, project = GITHUB_PROJECT): void {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((url: string | URL | Request) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.endsWith(`/api/v1/projects/${project.id}/github`)) {
        if (!githubInfo) {
          return Promise.resolve(jsonResponse({ message: 'not found' }, 404));
        }
        return Promise.resolve(jsonResponse(githubInfo));
      }
      if (path.includes('/settings/resolved')) {
        return Promise.resolve(jsonResponse({ project: {}, settings: {} }));
      }
      if (path.includes('/agents')) {
        return Promise.resolve(jsonResponse({ agents: [] }));
      }
      if (path.match(/\/api\/v1\/projects\/[^/]+(\?.*)?$/)) {
        return Promise.resolve(jsonResponse(project));
      }
      return Promise.resolve(jsonResponse({}));
    })
  );
}

describe('Project GitHub page shell (M1.5)', () => {
  beforeAll(async () => {
    await Promise.all([
      import('./project-github.js'),
      import('../shared/nav.js'),
      import('./project-detail.js'),
      import('./project-settings.js'),
      import('../app-shell.js'),
    ]);
  }, 30_000);

  beforeEach(() => {
    clearProjectGitHubTabsForTests();
    resetProjectGitHubRequestsForTests();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    window.history.replaceState({}, '', '/projects/proj-gh-1/github');
  });

  afterEach(() => {
    clearProjectGitHubTabsForTests();
    resetProjectGitHubRequestsForTests();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders status header and Agent access + Connection tabs for a connected project even with hub.github_triggers off', async () => {
    setServerFlags({ 'hub.github_triggers': false });
    stubProjectFetch(makeProjectGitHub());

    const el = document.createElement('scion-page-project-github') as ScionPageProjectGithub;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-testid="github-status-header"]')).not.toBeNull();
    });
    await el.updateComplete;

    const root = el.shadowRoot!;
    expect(root.querySelector('scion-page-404')).toBeNull();

    // Shared status header contents
    const header = root.querySelector('[data-testid="github-status-header"]')!;
    expect(header.textContent).toContain('Connected');
    expect(root.querySelector('[data-testid="github-header-repo"]')?.textContent).toContain(
      'Acme/api'
    );
    expect(root.querySelector('[data-testid="github-header-app"]')?.textContent).toContain(
      '@scion-bot'
    );
    expect(root.querySelector('[data-testid="github-header-readiness"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="github-header-routing"]')).not.toBeNull();

    // Tabs: Agent access (active by default) and Connection, no Automation stub
    const agentTab = root.querySelector('[data-testid="github-tab-agent-access"]');
    const connTab = root.querySelector('[data-testid="github-tab-connection"]');
    expect(agentTab).not.toBeNull();
    expect(connTab).not.toBeNull();
    expect(root.querySelector('[data-testid="github-tab-automation"]')).toBeNull();
    expect(agentTab!.hasAttribute('active')).toBe(true);
    expect(agentTab!.hasAttribute('disabled')).toBe(false);
    expect(connTab!.hasAttribute('disabled')).toBe(false);

    // Extracted components mounted in each tab panel
    const agentCard = root.querySelector(
      '[data-testid="github-panel-agent-access"] scion-project-github-card'
    );
    const connCard = root.querySelector(
      '[data-testid="github-panel-connection"] scion-project-github-card'
    );
    expect(agentCard?.getAttribute('section')).toBe('permissions');
    expect(connCard?.getAttribute('section')).toBe('connection');
  });

  it('lands disconnected projects on Connection with other tabs disabled ("connect first")', async () => {
    window.history.replaceState({}, '', '/projects/proj-gh-1/github?tab=agent-access');
    stubProjectFetch(
      makeProjectGitHub({
        association: {
          installation_id: null,
          account_login: '',
          basis: null,
          write_allowed: false,
          authorized: false,
          verified: true,
          error_code: '',
          reason: '',
        },
        status: null,
      })
    );

    const el = document.createElement('scion-page-project-github') as ScionPageProjectGithub;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-testid="github-tabs"]')).not.toBeNull();
    });
    await el.updateComplete;

    const root = el.shadowRoot!;
    expect(root.querySelector('[data-testid="github-header-connection"]')?.textContent).toContain(
      'Not connected'
    );

    const agentTab = root.querySelector('[data-testid="github-tab-agent-access"]')!;
    const connTab = root.querySelector('[data-testid="github-tab-connection"]')!;
    expect(connTab.hasAttribute('active')).toBe(true);
    expect(connTab.hasAttribute('disabled')).toBe(false);
    expect(agentTab.hasAttribute('active')).toBe(false);
    expect(agentTab.hasAttribute('disabled')).toBe(true);
    expect(agentTab.textContent).toContain('connect first');
  });

  it('supports registering an extension tab (e.g. M5.1 Automation) and disables it while disconnected', async () => {
    registerProjectGitHubTab({
      id: 'automation',
      label: 'Automation',
      order: 10,
      requiresConnection: true,
      render: () => html`<div data-testid="ext-automation">Automation body</div>`,
    });

    stubProjectFetch(
      makeProjectGitHub({
        association: {
          installation_id: null,
          account_login: '',
          basis: null,
          write_allowed: false,
          authorized: false,
          verified: true,
          error_code: '',
          reason: '',
        },
      })
    );

    const el = document.createElement('scion-page-project-github') as ScionPageProjectGithub;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-testid="github-tab-automation"]')).not.toBeNull();
    });

    const autoTab = el.shadowRoot!.querySelector('[data-testid="github-tab-automation"]')!;
    expect(autoTab.hasAttribute('disabled')).toBe(true);
    expect(autoTab.textContent).toContain('connect first');
    expect(
      el.shadowRoot!.querySelector('[data-testid="github-tab-connection"]')?.hasAttribute('active')
    ).toBe(true);
  });

  it('renders 404 when the project remote is not on the App host', async () => {
    stubProjectFetch(
      makeProjectGitHub({
        remote: { owner: 'Acme', repo: 'api', on_app_host: false },
      })
    );

    const el = document.createElement('scion-page-project-github') as ScionPageProjectGithub;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    });
    expect(el.shadowRoot?.querySelector('[data-testid="github-tabs"]')).toBeNull();
  });

  it('renders 404 when the GitHub App is not configured', async () => {
    stubProjectFetch(
      makeProjectGitHub({
        app: { configured: false, slug: '', web_host: 'github.com' },
      })
    );

    const el = document.createElement('scion-page-project-github') as ScionPageProjectGithub;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    });
    expect(el.shadowRoot?.querySelector('[data-testid="github-tabs"]')).toBeNull();
  });

  it('registers GitHub page title in scion-app for /projects/:id/github', async () => {
    stubProjectFetch(makeProjectGitHub());
    const app = document.createElement('scion-app') as ScionApp;
    app.currentPath = '/projects/proj-gh-1/github';
    document.body.appendChild(app);
    await app.updateComplete;

    const header = app.shadowRoot?.querySelector('scion-header') as
      | (HTMLElement & { pageTitle: string })
      | null;
    expect(header?.pageTitle).toBe('GitHub');
  });

  it('shows the contextual GitHub nav entry in scion-nav only when remote is on App host and App is configured', async () => {
    stubProjectFetch(makeProjectGitHub());
    const nav = document.createElement('scion-nav') as ScionNav;
    nav.currentPath = '/projects/proj-gh-1/github';
    document.body.appendChild(nav);

    await vi.waitFor(() => {
      expect(nav.shadowRoot?.querySelector('a[href="/projects/proj-gh-1/github"]')).not.toBeNull();
    });
    const link = nav.shadowRoot!.querySelector('a[href="/projects/proj-gh-1/github"]')!;
    expect(link.classList.contains('active')).toBe(true);
    expect(link.textContent).toContain('GitHub');
    expect(link.querySelector('sl-icon')?.getAttribute('name')).toBe('github');

    // Leaving project routes hides the contextual section
    nav.currentPath = '/projects';
    await nav.updateComplete;
    expect(nav.shadowRoot?.querySelector('[data-testid="nav-project-section"]')).toBeNull();
    nav.remove();

    // Unconfigured App hides the nav entry
    resetProjectGitHubRequestsForTests();
    stubProjectFetch(
      makeProjectGitHub({
        app: { configured: false, slug: '', web_host: 'github.com' },
      })
    );
    const navHidden = document.createElement('scion-nav') as ScionNav;
    navHidden.currentPath = '/projects/proj-gh-1';
    document.body.appendChild(navHidden);
    await navHidden.updateComplete;
    await new Promise((r) => setTimeout(r, 30));
    await navHidden.updateComplete;
    expect(navHidden.shadowRoot?.querySelector('a[href="/projects/proj-gh-1/github"]')).toBeNull();
  });

  it('shows the GitHub header button on project-detail only when App is configured and remote is on App host', async () => {
    window.history.replaceState({}, '', '/projects/proj-gh-1');
    class MockEventSource {
      addEventListener(): void {}
      removeEventListener(): void {}
      close(): void {}
    }
    vi.stubGlobal('EventSource', MockEventSource);

    // Visible case
    stubProjectFetch(makeProjectGitHub());
    const detail = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    detail.projectId = 'proj-gh-1';
    document.body.appendChild(detail);

    await vi.waitFor(() => {
      expect(detail.shadowRoot?.querySelector('[data-github-link]')).not.toBeNull();
    });
    const link = detail.shadowRoot!.querySelector('[data-github-link]') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('/projects/proj-gh-1/github');
    expect(detail.shadowRoot!.querySelector('[data-github-button]')?.textContent).toContain(
      'GitHub'
    );
    detail.remove();

    // Hidden case (remote not on App host)
    resetProjectGitHubRequestsForTests();
    stubProjectFetch(
      makeProjectGitHub({
        remote: { owner: 'Acme', repo: 'api', on_app_host: false },
      })
    );
    const detailHidden = document.createElement(
      'scion-page-project-detail'
    ) as ScionPageProjectDetail;
    detailHidden.projectId = 'proj-gh-1';
    document.body.appendChild(detailHidden);
    await detailHidden.updateComplete;
    await new Promise((r) => setTimeout(r, 50));
    await detailHidden.updateComplete;
    expect(detailHidden.shadowRoot?.querySelector('[data-github-link]')).toBeNull();
  });

  it('renders a one-line GitHub link in Project Settings (connected, disconnected, and hidden states)', async () => {
    // Connected
    stubProjectFetch(makeProjectGitHub());
    const settingsConn = document.createElement(
      'scion-page-project-settings'
    ) as ScionPageProjectSettings;
    settingsConn.projectId = 'proj-gh-1';
    document.body.appendChild(settingsConn);

    await vi.waitFor(() => {
      expect(
        settingsConn.shadowRoot?.querySelector('[data-testid="project-settings-github-link"]')
      ).not.toBeNull();
    });
    const connBlock = settingsConn.shadowRoot!.querySelector(
      '[data-testid="project-settings-github-link"]'
    )!;
    expect(connBlock.textContent?.replace(/\s+/g, ' ')).toContain(
      'GitHub: connected to Acme/api — Manage →'
    );
    const manageLink = settingsConn.shadowRoot!.querySelector(
      '[data-testid="project-settings-github-manage-link"]'
    ) as HTMLAnchorElement;
    expect(manageLink.getAttribute('href')).toBe('/projects/proj-gh-1/github');
    expect(settingsConn.shadowRoot!.querySelector('scion-project-github-card')).toBeNull();
    settingsConn.remove();

    // Disconnected
    resetProjectGitHubRequestsForTests();
    stubProjectFetch(
      makeProjectGitHub({
        association: {
          installation_id: null,
          account_login: '',
          basis: null,
          write_allowed: false,
          authorized: false,
          verified: true,
          error_code: '',
          reason: '',
        },
      })
    );
    const settingsDisconn = document.createElement(
      'scion-page-project-settings'
    ) as ScionPageProjectSettings;
    settingsDisconn.projectId = 'proj-gh-1';
    document.body.appendChild(settingsDisconn);

    await vi.waitFor(() => {
      expect(
        settingsDisconn.shadowRoot?.querySelector(
          '[data-testid="project-settings-github-connect-link"]'
        )
      ).not.toBeNull();
    });
    const disconnBlock = settingsDisconn.shadowRoot!.querySelector(
      '[data-testid="project-settings-github-link"]'
    )!;
    expect(disconnBlock.textContent?.replace(/\s+/g, ' ')).toContain(
      'GitHub: not connected — Connect →'
    );
    settingsDisconn.remove();

    // Hidden when App not configured
    resetProjectGitHubRequestsForTests();
    stubProjectFetch(
      makeProjectGitHub({
        app: { configured: false, slug: '', web_host: 'github.com' },
      })
    );
    const settingsHidden = document.createElement(
      'scion-page-project-settings'
    ) as ScionPageProjectSettings;
    settingsHidden.projectId = 'proj-gh-1';
    document.body.appendChild(settingsHidden);
    await settingsHidden.updateComplete;
    await new Promise((r) => setTimeout(r, 80));
    await settingsHidden.updateComplete;
    expect(
      settingsHidden.shadowRoot?.querySelector('[data-testid="project-settings-github-link"]')
    ).toBeNull();
  });
});
