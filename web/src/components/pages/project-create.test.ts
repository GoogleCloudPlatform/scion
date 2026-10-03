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
 * Tests for project-create.ts: the form is gated on hub-scope project.create
 * (design §5.F). Without it, a notice explains why instead of a form.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities, PageData, UserRole } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(opts: { caps?: Capabilities; projectsStatus?: number }) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/projects?limit=1')) {
      if (opts.projectsStatus && opts.projectsStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: { code: 'x' } }, opts.projectsStatus));
      }
      return Promise.resolve(
        jsonResponse({ projects: [], ...(opts.caps ? { _capabilities: opts.caps } : {}) })
      );
    }
    if (path.includes('/api/v1/system/status')) {
      return Promise.resolve(jsonResponse({}));
    }
    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(jsonResponse({ configured: false }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

async function createComponent(
  opts: { caps?: Capabilities; projectsStatus?: number },
  role?: UserRole
): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts)));
  const el = document.createElement('scion-page-project-create') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
  };
  el.pageData = {
    path: '/projects/new',
    title: 'Create Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', ...(role ? { role } : {}) },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function q(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

describe('scion-page-project-create — hub project.create gate', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-create.js');
  }, 60_000);

  beforeEach(() => {
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders the form when hub caps include create', async () => {
    element = await createComponent({ caps: { actions: ['list', 'create'] } }, 'member');

    expect(q(element, '.form-card')).not.toBeNull();
    expect(q(element, '#name')).not.toBeNull();
    expect(q(element, '.create-denied-notice')).toBeNull();
  });

  it('renders the notice, not the form, when hub caps lack create', async () => {
    element = await createComponent({ caps: { actions: ['list'] } }, 'viewer');

    const notice = q(element, '.create-denied-notice');
    expect(notice).not.toBeNull();
    expect(q(element, '.form-card')).toBeNull();
    expect(q(element, '#name')).toBeNull();

    const text = notice?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain("Your hub role (Viewer) can't create projects.");
    expect(text).toContain(
      'Ask a hub admin to change your role, or to add you to an existing project.'
    );
  });

  it('links the notice to /projects', async () => {
    element = await createComponent({ caps: { actions: ['list'] } }, 'viewer');

    const link = q(element, '.create-denied-notice a[href="/projects"]');
    expect(link).not.toBeNull();
  });

  it('gates on capabilities, not the role string', async () => {
    // A viewer granted project.create by a custom binding sees the form.
    element = await createComponent({ caps: { actions: ['create'] } }, 'viewer');
    expect(q(element, '.form-card')).not.toBeNull();
    element.remove();
    resetHubProjectCapabilitiesCache();

    // A member without project.create sees the notice.
    element = await createComponent({ caps: { actions: ['list'] } }, 'member');
    expect(q(element, '.create-denied-notice')?.textContent).toContain(
      "Your hub role (Member) can't create projects."
    );
    expect(q(element, '.form-card')).toBeNull();
  });

  it('omits the role name when the role is unknown', async () => {
    element = await createComponent({ caps: { actions: [] } });

    const text = q(element, '.create-denied-notice')?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain("Your hub role can't create projects.");
    expect(text).not.toContain('(');
  });

  it('fails closed with a neutral notice when capabilities cannot be loaded', async () => {
    element = await createComponent({ projectsStatus: 500 }, 'member');

    expect(q(element, '.form-card')).toBeNull();
    const notice = q(element, '.create-unknown-notice');
    expect(notice).not.toBeNull();
    // The role notice is a distinct state; its class must not match here.
    expect(q(element, '.create-denied-notice')).toBeNull();
    const text = notice?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain("Couldn't check whether you can create projects.");
    expect(text).toContain('Reload the page to try again.');
    // It must not claim the user's role lacks permission.
    expect(text).not.toContain("can't create projects");
    expect(q(element, '.create-unknown-notice a[href="/projects"]')).not.toBeNull();
  });

  it('fails closed with the neutral notice when the response has no capabilities', async () => {
    element = await createComponent({}, 'member');

    expect(q(element, '.form-card')).toBeNull();
    expect(q(element, '.create-unknown-notice')).not.toBeNull();
  });

  it('shows the role notice (not the neutral one) when caps load without create', async () => {
    element = await createComponent({ caps: { actions: ['list'] } }, 'viewer');

    expect(q(element, '.create-unknown-notice')).toBeNull();
    expect(q(element, '.create-denied-notice')).not.toBeNull();
  });

  it('does not redirect away from /projects/new', async () => {
    const pushState = vi.spyOn(window.history, 'pushState');
    element = await createComponent({ caps: { actions: [] } }, 'viewer');

    expect(pushState).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Template-first form (ptone/scion#2702): "Start from" Blank or a template.
// ---------------------------------------------------------------------------

interface RecordedRequest {
  path: string;
  method: string;
  body: unknown;
}

const GIT_TEMPLATE = {
  id: 'tpl-git',
  name: 'Go service',
  slug: 'go-service',
  gitRemote: 'github.com/acme/go-service-template',
  labels: {
    'scion.io/template': 'true',
    'scion.dev/workspace-mode': 'worktree-per-agent',
    'scion.dev/clone-url': 'https://github.com/acme/go-service-template.git',
    'scion.dev/default-branch': 'develop',
  },
  annotations: { 'scion.io/default-harness-config': 'claude-default' },
};

const SHARED_TEMPLATE = {
  id: 'tpl-shared',
  name: 'Research notebook',
  slug: 'research-notebook',
  labels: { 'scion.io/template': 'true' },
};

interface FormOpts {
  templates?: unknown[];
  systemStatus?: Record<string, unknown>;
  cloneStatus?: number;
  cloneBody?: unknown;
}

async function createForm(opts: FormOpts = {}): Promise<{
  el: HTMLElement & { updateComplete: Promise<boolean> };
  requests: RecordedRequest[];
}> {
  const requests: RecordedRequest[] = [];
  const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    const method = (init?.method ?? 'GET').toUpperCase();
    const body = typeof init?.body === 'string' ? (JSON.parse(init.body) as unknown) : undefined;
    requests.push({ path, method, body });

    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(
        jsonResponse({ projects: [], _capabilities: { actions: ['list', 'create'] } })
      );
    }
    if (path.includes('/api/v1/projects?isTemplate=true')) {
      return Promise.resolve(jsonResponse({ projects: opts.templates ?? [] }));
    }
    if (path.includes('/api/v1/system/status')) {
      return Promise.resolve(jsonResponse(opts.systemStatus ?? {}));
    }
    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(jsonResponse({ configured: false }));
    }
    if (method === 'POST' && /\/api\/v1\/projects\/[^/]+\/clone$/.test(path)) {
      return Promise.resolve(
        jsonResponse(opts.cloneBody ?? { id: 'new-clone' }, opts.cloneStatus ?? 201)
      );
    }
    if (method === 'POST' && path.endsWith('/api/v1/projects')) {
      return Promise.resolve(jsonResponse({ project: { id: 'new-blank' } }, 201));
    }
    return Promise.resolve(jsonResponse({}));
  };
  vi.stubGlobal('fetch', vi.fn(handler));
  const el = document.createElement('scion-page-project-create') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
  };
  el.pageData = {
    path: '/projects/new',
    title: 'Create Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  await settle(el);
  return { el, requests };
}

async function settle(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
}

/** Set a Shoelace control's value and fire its change/input event, as a user would. */
async function setValue(
  el: HTMLElement & { updateComplete: Promise<boolean> },
  selector: string,
  value: string,
  event: 'sl-change' | 'sl-input'
): Promise<void> {
  const control = q(el, selector) as (HTMLElement & { value: string }) | null;
  expect(control, `control ${selector}`).not.toBeNull();
  control!.value = value;
  control!.dispatchEvent(new Event(event, { bubbles: true, composed: true }));
  await el.updateComplete;
}

function optionValues(el: HTMLElement, selectSelector: string): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll(`${selectSelector} sl-option`) ?? []).map(
    (o) => o.getAttribute('value') ?? ''
  );
}

function text(node: Element | null | undefined): string {
  return node?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

async function submit(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  const button = q(el, '.form-actions sl-button[variant="primary"]') as HTMLElement;
  button.click();
  await settle(el);
}

function posts(requests: RecordedRequest[]): RecordedRequest[] {
  return requests.filter((r) => r.method === 'POST');
}

describe('scion-page-project-create — Start from (Blank / template)', () => {
  let element: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeAll(async () => {
    await import('./project-create.js');
  }, 60_000);

  beforeEach(() => {
    resetHubProjectCapabilitiesCache();
    vi.spyOn(window.history, 'pushState').mockImplementation(() => {});
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('defaults to Blank with Shared workspace directory, listing templates', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE, SHARED_TEMPLATE] });
    element = el;

    expect(requests.some((r) => r.path.includes('/api/v1/projects?isTemplate=true'))).toBe(true);
    expect((q(el, '#startFrom') as HTMLElement & { value: string }).value).toBe('blank');
    expect(optionValues(el, '#startFrom')).toEqual(['blank', 'tpl-git', 'tpl-shared']);
    expect(text(q(el, '#startFrom sl-option[value="tpl-git"]'))).toContain(
      'Git · worktree per agent'
    );
    expect(text(q(el, '#startFrom sl-option[value="tpl-shared"]'))).toContain(
      'Shared workspace directory'
    );

    expect((q(el, '#mode') as HTMLElement & { value: string }).value).toBe('shared');
    expect(q(el, '.template-summary')).toBeNull();
    expect(text(q(el, '.form-actions sl-button[variant="primary"]'))).toBe('Create Project');
  });

  it('offers Git and Shared workspace directory only — no Hub-managed, From Template or Linked off-workstation', async () => {
    const { el } = await createForm({ systemStatus: { embeddedBrokerID: 'b1' } });
    element = el;

    expect(optionValues(el, '#mode')).toEqual(['git', 'shared']);
    const modeText = text(q(el, '#mode'));
    expect(modeText).toContain('Shared workspace directory');
    expect(modeText).not.toContain('Hub-managed');
    expect(modeText).not.toContain('From Template');
  });

  it('offers Local Directory (linked) only on a workstation hub with an embedded broker', async () => {
    let { el } = await createForm({ systemStatus: { workstation: true, embeddedBrokerID: 'b1' } });
    element = el;
    expect(optionValues(el, '#mode')).toEqual(['git', 'shared', 'linked']);
    el.remove();
    resetHubProjectCapabilitiesCache();

    ({ el } = await createForm({ systemStatus: { workstation: true } }));
    element = el;
    expect(optionValues(el, '#mode')).toEqual(['git', 'shared']);
  });

  it('shows an empty-state hint when there are no templates', async () => {
    const { el } = await createForm({ templates: [] });
    element = el;

    expect(optionValues(el, '#startFrom')).toEqual(['blank']);
    expect(text(q(el, '.start-from-hint'))).toContain('No project templates yet');
  });

  it('Blank + Shared workspace directory posts {name, slug} to /projects', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#name', 'My Notes', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      { path: '/api/v1/projects', method: 'POST', body: { name: 'My Notes', slug: 'my-notes' } },
    ]);
  });

  it('Blank + Git Repository posts the git body to /projects', async () => {
    const { el, requests } = await createForm();
    element = el;

    await setValue(el, '#mode', 'git', 'sl-change');
    await setValue(el, '#gitRemote', 'git@github.com:acme/payments-api.git', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/projects',
        method: 'POST',
        body: {
          name: 'payments-api',
          slug: 'payments-api',
          gitRemote: 'git@github.com:acme/payments-api.git',
          workspaceMode: 'per-agent',
          labels: {
            'scion.dev/default-branch': 'main',
            'scion.dev/clone-url': 'https://github.com/acme/payments-api.git',
            'scion.dev/source-url': 'git@github.com:acme/payments-api.git',
            'scion.dev/workspace-mode': 'per-agent',
          },
        },
      },
    ]);
  });

  it('a git template hides per-project workspace fields and shows them read-only', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE, SHARED_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');

    // Hidden: workspace type select, Blank git inputs, token, branch.
    for (const sel of ['#mode', '#gitRemote', '#githubToken', '#branch', '#localPath']) {
      expect(q(el, sel), sel).toBeNull();
    }
    // Editable: name, slug, git remote override.
    expect(q(el, '#name')).not.toBeNull();
    expect(q(el, '#slug')).not.toBeNull();
    const override = q(el, '#templateGitRemote') as HTMLElement & { placeholder: string };
    expect(override).not.toBeNull();
    expect(override.getAttribute('placeholder')).toBe(
      'https://github.com/acme/go-service-template.git'
    );
    expect(text(q(el, '.badge-override'))).toBe('Override');

    // Locked summary.
    const summary = q(el, '.template-summary');
    expect(text(summary?.querySelector('h3'))).toBe('From template: Go service');
    expect(text(q(el, '.summary-workspace-type'))).toBe('Git Repository');
    expect(text(q(el, '.summary-repository'))).toBe('github.com/acme/go-service-template');
    expect(text(q(el, '.summary-workspace-mode'))).toBe('Worktree per agent');
    expect(text(q(el, '.summary-branch'))).toBe('develop');
    expect(text(q(el, '.summary-harness-config'))).toBe('claude-default');
    expect(text(summary)).toContain('GCP service accounts and the default service account');
    expect(text(summary)).toContain('Not copied: secrets');

    expect(text(q(el, '.form-actions sl-button[variant="primary"]'))).toBe('Create from template');
  });

  it('a git template without an override posts {name} to /clone', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'payments-api', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      { path: '/api/v1/projects/tpl-git/clone', method: 'POST', body: { name: 'payments-api' } },
    ]);
    expect(window.history.pushState).toHaveBeenCalledWith({}, '', '/projects/new-clone');
  });

  it('a git remote override marks the field, updates the summary, and is sent', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'payments-api', 'sl-input');
    await setValue(
      el,
      '#templateGitRemote',
      'https://github.com/acme/payments-api.git',
      'sl-input'
    );

    expect(text(q(el, '.badge-override'))).toBe('Overridden');
    expect(q(el, '.override-field.active')).not.toBeNull();
    expect(text(q(el, '.summary-repository'))).toBe('github.com/acme/payments-api (override)');
    expect(text(q(el, '.summary-branch'))).toBe('main');
    expect(text(q(el, '.warn-note'))).toContain('GitHub token is a secret and is not copied');

    await submit(el);
    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/projects/tpl-git/clone',
        method: 'POST',
        body: { name: 'payments-api', gitRemote: 'https://github.com/acme/payments-api.git' },
      },
    ]);
  });

  it('"Use template value" clears the override', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#templateGitRemote', 'https://github.com/acme/other.git', 'sl-input');
    (q(el, '.reset-link') as HTMLElement).click();
    await el.updateComplete;

    expect((q(el, '#templateGitRemote') as HTMLElement & { value: string }).value).toBe('');
    expect(text(q(el, '.badge-override'))).toBe('Override');
    expect(q(el, '.reset-link')).toBeNull();
    expect(text(q(el, '.summary-repository'))).toBe('github.com/acme/go-service-template');
  });

  it('a shared-directory template has no override field and sends an edited slug', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE, SHARED_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-shared', 'sl-change');
    expect(q(el, '#templateGitRemote')).toBeNull();
    expect(q(el, '#mode')).toBeNull();
    expect(text(q(el, '.summary-workspace-type'))).toBe('Shared workspace directory');
    expect(q(el, '.summary-repository')).toBeNull();
    expect(q(el, '.summary-harness-config')).toBeNull();

    await setValue(el, '#name', 'Q4 market scan', 'sl-input');
    await setValue(el, '#slug', 'q4-scan', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/projects/tpl-shared/clone',
        method: 'POST',
        body: { name: 'Q4 market scan', slug: 'q4-scan' },
      },
    ]);
  });

  it('switching templates drops a git remote override', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE, SHARED_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#templateGitRemote', 'https://github.com/acme/other.git', 'sl-input');
    await setValue(el, '#startFrom', 'tpl-shared', 'sl-change');
    await setValue(el, '#name', 'notes', 'sl-input');
    await submit(el);

    expect(posts(requests).map((r) => r.body)).toEqual([{ name: 'notes' }]);
  });

  it('shows a clone 409 inline on Slug without navigating', async () => {
    const { el } = await createForm({
      templates: [SHARED_TEMPLATE],
      cloneStatus: 409,
      cloneBody: {
        error: { code: 'conflict', message: 'A project with slug "taken" already exists' },
      },
    });
    element = el;

    await setValue(el, '#startFrom', 'tpl-shared', 'sl-change');
    await setValue(el, '#name', 'Taken', 'sl-input');
    await setValue(el, '#slug', 'taken', 'sl-input');
    await submit(el);

    expect(text(q(el, '.slug-error'))).toContain('already exists');
    expect(q(el, '.error-banner')).toBeNull();
    expect(window.history.pushState).not.toHaveBeenCalled();
  });
});
