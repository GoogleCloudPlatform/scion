/**
 * Tests for Project Settings -> GitHub card (M1.1):
 * - Read-only permissions table rendering including cap reasons
 * - Bind installation button shown only when can.bind is true
 * - Client-side candidate filtering (active + case-insensitive owner match)
 * - Confirm dialog copy and PUT /projects/{id}/github-installation
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ProjectGitHubInfo, GitHubInstallation } from '../../shared/types.js';
import {
  filterBindableGitHubInstallations,
  formatGitHubCapReason,
  type ScionPageProjectSettings,
} from './project-settings.js';

vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(),
}));

import { showConfirm } from '../shared/confirm-dialog.js';

const GITHUB_PROJECT = {
  id: 'proj-gh-1',
  name: 'Acme API',
  slug: 'acme-api',
  gitRemote: 'https://github.com/Acme/api.git',
  _capabilities: { actions: ['read', 'update', 'manage'] },
};

function makeProjectGitHub(overrides?: Partial<ProjectGitHubInfo>): ProjectGitHubInfo {
  return {
    project_id: 'proj-gh-1',
    app: { configured: true, slug: 'scion-bot', web_host: 'github.com' },
    remote: { owner: 'Acme', repo: 'api', on_app_host: true },
    association: {
      installation_id: 4412,
      account_login: 'acme',
      basis: 'creator_pull',
      write_allowed: false,
      authorized: true,
      error_code: '',
      reason: '',
    },
    status: {
      state: 'ok',
      last_checked: '2026-11-25T10:00:00Z',
    },
    permissions: {
      mode: 'custom',
      requested: { contents: 'write', issues: 'write', metadata: 'read' },
      app_granted: { contents: 'write', issues: 'read', metadata: 'read' },
      effective: { contents: 'read', issues: 'read', metadata: 'read' },
      capped: ['contents', 'issues'],
      cap_reasons: {
        contents: 'association_read_only',
        issues: 'app_grant',
      },
    },
    can: {
      bind: true,
      edit_permissions: true,
      edit_extra_repos: true,
      discover: true,
    },
    ...overrides,
  };
}

function createFetchHandler(opts: {
  githubInfo: ProjectGitHubInfo;
  installations?: GitHubInstallation[];
  onBindPut?: (body: { installation_id: number }) => void;
}) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (init?.method === 'PUT' && path.includes('/github-installation')) {
      const body = JSON.parse(init.body as string) as { installation_id: number };
      opts.onBindPut?.(body);
      return Promise.resolve(
        new Response(
          JSON.stringify({
            project_id: 'proj-gh-1',
            installation_id: body.installation_id,
            status: 'associated',
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/api/v1/github-app/installations')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            installations: opts.installations ?? [],
            total: (opts.installations ?? []).length,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.endsWith('/api/v1/projects/proj-gh-1/github')) {
      return Promise.resolve(
        new Response(JSON.stringify(opts.githubInfo), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.match(/\/api\/v1\/projects\/[^/]+$/) || path.match(/\/api\/v1\/projects\/[^/]+\?/)) {
      return Promise.resolve(
        new Response(JSON.stringify(GITHUB_PROJECT), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/settings/resolved')) {
      return Promise.resolve(
        new Response(JSON.stringify({ project: {}, settings: {} }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function mountSettings(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
): Promise<ScionPageProjectSettings> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-project-settings') as ScionPageProjectSettings;
  el.projectId = 'proj-gh-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 250));
  await el.updateComplete;
  return el;
}

describe('Project Settings — GitHub App card (M1.1)', () => {
  beforeAll(async () => {
    await import('./project-settings.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it('filters installations to active ones matching remote owner case-insensitively', () => {
    const installs: GitHubInstallation[] = [
      {
        installation_id: 1,
        account_login: 'other',
        account_type: 'Organization',
        app_id: 1,
        status: 'active',
      },
      {
        installation_id: 2,
        account_login: 'acme',
        account_type: 'Organization',
        app_id: 1,
        status: 'suspended',
      },
      {
        installation_id: 3,
        account_login: 'acme',
        account_type: 'Organization',
        app_id: 1,
        status: 'active',
      },
      {
        installation_id: 4,
        account_login: 'ACME',
        account_type: 'Organization',
        app_id: 1,
        status: 'active',
      },
    ];
    const filtered = filterBindableGitHubInstallations(installs, 'Acme');
    expect(filtered.map((i) => i.installation_id)).toEqual([3, 4]);
  });

  it('formats cap reasons clearly', () => {
    expect(formatGitHubCapReason('app_grant', 'write')).toContain('App not granted write');
    expect(formatGitHubCapReason('association_read_only', 'write')).toContain(
      'Association is read-only'
    );
  });

  it('renders the read-only permissions table with cap reasons and mode badge', async () => {
    const el = await mountSettings(createFetchHandler({ githubInfo: makeProjectGitHub() }));
    const root = el.shadowRoot!;

    const table = root.querySelector('[data-testid="github-permissions-table"]');
    expect(table).not.toBeNull();
    expect(root.querySelector('[data-testid="github-perm-mode"]')?.textContent?.trim()).toBe(
      'Custom'
    );

    const contentsRow = table!.querySelector('tr[data-permission="contents"]')!;
    expect(contentsRow.textContent).toContain('Contents');
    expect(contentsRow.textContent).toContain('write');
    expect(contentsRow.textContent).toContain('read');
    expect(contentsRow.textContent).toContain('Association is read-only — ask a hub admin to bind');

    const issuesRow = table!.querySelector('tr[data-permission="issues"]')!;
    expect(issuesRow.textContent).toContain('App not granted write — update the App on GitHub');
  });

  it('shows Bind installation button only when can.bind is true', async () => {
    const elDenied = await mountSettings(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          can: { bind: false, edit_permissions: true, edit_extra_repos: true, discover: false },
        }),
      })
    );
    expect(elDenied.shadowRoot!.querySelector('[data-testid="github-bind-button"]')).toBeNull();
    elDenied.remove();

    const elAllowed = await mountSettings(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          can: { bind: true, edit_permissions: true, edit_extra_repos: true, discover: true },
        }),
      })
    );
    expect(
      elAllowed.shadowRoot!.querySelector('[data-testid="github-bind-button"]')
    ).not.toBeNull();
  });

  it('filters candidates, prompts with confirm dialog copy, and sends PUT bind', async () => {
    const boundBodies: Array<{ installation_id: number }> = [];
    vi.mocked(showConfirm).mockResolvedValueOnce(true);

    const el = await mountSettings(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          association: {
            installation_id: null,
            account_login: '',
            basis: null,
            write_allowed: false,
            authorized: false,
            error_code: '',
            reason: '',
          },
        }),
        installations: [
          {
            installation_id: 100,
            account_login: 'wrong-org',
            account_type: 'Organization',
            app_id: 1,
            status: 'active',
          },
          {
            installation_id: 200,
            account_login: 'acme',
            account_type: 'Organization',
            app_id: 1,
            status: 'suspended',
          },
          {
            installation_id: 4412,
            account_login: 'acme',
            account_type: 'Organization',
            app_id: 1,
            status: 'active',
          },
        ],
        onBindPut: (body) => boundBodies.push(body),
      })
    );

    const bindBtn = el.shadowRoot!.querySelector(
      '[data-testid="github-bind-button"]'
    ) as HTMLElement;
    expect(bindBtn).not.toBeNull();
    bindBtn.click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    await el.updateComplete;

    expect(showConfirm).toHaveBeenCalledTimes(1);
    const confirmMsg = vi.mocked(showConfirm).mock.calls[0][0];
    expect(confirmMsg).toContain('acme');
    expect(confirmMsg).toContain('#4412');
    expect(confirmMsg).toContain(
      "authorized by your hub-admin binding, not the creator's GitHub access"
    );
    expect(boundBodies).toEqual([{ installation_id: 4412 }]);
  });
});
