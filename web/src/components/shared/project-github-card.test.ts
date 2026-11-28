/**
 * Tests for Project GitHub App card (M1.1, extracted in M1.5):
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
  type ScionProjectGithubCard,
} from './project-github-card.js';

vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(),
}));

import { showConfirm } from './confirm-dialog.js';

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
      basis: 'creator_pull',
      write_allowed: false,
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
      mode: 'custom',
      app_grant_known: true,
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
      unbind: true,
      check: true,
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

    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function mountCard(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
): Promise<ScionProjectGithubCard> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-project-github-card') as ScionProjectGithubCard;
  el.projectId = 'proj-gh-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 250));
  await el.updateComplete;
  return el;
}

describe('Project GitHub App card (M1.1)', () => {
  beforeAll(async () => {
    await import('./project-github-card.js');
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
    const el = await mountCard(createFetchHandler({ githubInfo: makeProjectGitHub() }));
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
    const elDenied = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          can: {
            bind: false,
            unbind: true,
            check: true,
            edit_permissions: true,
            edit_extra_repos: true,
            discover: false,
          },
        }),
      })
    );
    expect(elDenied.shadowRoot!.querySelector('[data-testid="github-bind-button"]')).toBeNull();
    elDenied.remove();

    const elAllowed = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          can: {
            bind: true,
            unbind: true,
            check: true,
            edit_permissions: true,
            edit_extra_repos: true,
            discover: true,
          },
        }),
      })
    );
    expect(
      elAllowed.shadowRoot!.querySelector('[data-testid="github-bind-button"]')
    ).not.toBeNull();
  });

  it('hides all write buttons on a connected project when can flags are all false', async () => {
    const el = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          can: {
            bind: false,
            unbind: false,
            check: false,
            edit_permissions: false,
            edit_extra_repos: false,
            discover: false,
          },
        }),
      })
    );
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-testid="github-app-section"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="github-bind-button"]')).toBeNull();
    expect(root.querySelector('[data-testid="github-check-button"]')).toBeNull();
    expect(root.querySelector('[data-testid="github-remove-button"]')).toBeNull();
    expect(root.querySelector('[data-testid="github-configure-link"]')).toBeNull();
  });

  it('shows unloaded App permissions note, neutral unverified copy, bound_by_name, and install link', async () => {
    const elUnloaded = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          association: {
            installation_id: 4412,
            account_login: 'acme',
            basis: null,
            write_allowed: false,
            authorized: false,
            verified: false,
            error_code: 'association_unverified',
            reason: 'GitHub App association could not be verified.',
          },
          permissions: {
            mode: 'default',
            app_grant_known: false,
            requested: { contents: 'write', metadata: 'read' },
            app_granted: null,
            effective: {},
            capped: [],
            cap_reasons: {},
          },
        }),
      })
    );
    const rootUnloaded = elUnloaded.shadowRoot!;
    expect(
      rootUnloaded.querySelector('[data-testid="github-app-perms-unloaded"]')?.textContent
    ).toContain('App permissions not yet loaded');
    const unverifiedEl = rootUnloaded.querySelector(
      '[data-testid="github-association-unverified"]'
    );
    expect(unverifiedEl?.textContent).toContain('Unverified');
    expect(rootUnloaded.textContent).not.toContain('Not authorized');
    expect(rootUnloaded.textContent).not.toContain('App not granted');
    elUnloaded.remove();

    const elBound = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
          association: {
            installation_id: 4412,
            account_login: 'acme',
            basis: 'admin_binding',
            write_allowed: true,
            bound_by: 'user-uuid-123',
            bound_by_name: 'Dana Admin',
            bound_at: '2026-11-20T12:00:00Z',
            authorized: true,
            verified: true,
            error_code: '',
            reason: '',
          },
        }),
      })
    );
    expect(elBound.shadowRoot!.textContent?.replace(/\s+/g, ' ')).toContain(
      'Bound by Dana Admin on'
    );
    elBound.remove();

    const elUnassoc = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
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
        }),
      })
    );
    const installLink = elUnassoc.shadowRoot!.querySelector(
      '[data-testid="github-install-link"]'
    ) as HTMLAnchorElement | null;
    expect(installLink).not.toBeNull();
    expect(installLink?.href).toBe('https://github.com/apps/scion-bot/installations/new');
  });

  it('filters candidates, prompts with confirm dialog copy, and sends PUT bind', async () => {
    const boundBodies: Array<{ installation_id: number }> = [];
    vi.mocked(showConfirm).mockResolvedValueOnce(true);

    const el = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
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

  it('shows multi-candidate bind picker when multiple active installations match owner', async () => {
    const boundBodies: Array<{ installation_id: number }> = [];
    vi.mocked(showConfirm).mockResolvedValueOnce(true);

    const el = await mountCard(
      createFetchHandler({
        githubInfo: makeProjectGitHub({
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
            installation_id: 4412,
            account_login: 'acme',
            account_type: 'Organization',
            app_id: 1,
            status: 'active',
          },
          {
            installation_id: 5512,
            account_login: 'ACME',
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
    bindBtn.click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    await el.updateComplete;

    const picker = el.shadowRoot!.querySelector('[data-testid="github-bind-picker"]');
    expect(picker).not.toBeNull();
    expect(picker!.textContent).toContain('#4412');
    expect(picker!.textContent).toContain('#5512');
    expect(showConfirm).not.toHaveBeenCalled();

    const candidateButtons = picker!.querySelectorAll('sl-button');
    expect(candidateButtons.length).toBe(2);
    (candidateButtons[1] as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    await el.updateComplete;

    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showConfirm).mock.calls[0][0]).toContain('#5512');
    expect(boundBodies).toEqual([{ installation_id: 5512 }]);
  });
});
