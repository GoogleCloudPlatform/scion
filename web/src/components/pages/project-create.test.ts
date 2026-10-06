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
import {
  MEMBERSHIP_CHANGED_EVENT,
  type MembershipChangedDetail,
} from '../../utils/membership-events.js';

const removeListeners: Array<() => void> = [];

afterEach(() => {
  for (const remove of removeListeners.splice(0)) remove();
});

/** Collect the membership-changed events dispatched on `window` during the test. */
function heardMembership(): MembershipChangedDetail[] {
  const heard: MembershipChangedDetail[] = [];
  const onEvent = (e: Event): void => {
    heard.push((e as CustomEvent<MembershipChangedDetail>).detail);
  };
  window.addEventListener(MEMBERSHIP_CHANGED_EVENT, onEvent);
  removeListeners.push(() => window.removeEventListener(MEMBERSHIP_CHANGED_EVENT, onEvent));
  return heard;
}

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

const EMPTY_PER_AGENT_TEMPLATE = {
  id: 'tpl-empty',
  name: 'Batch evaluator',
  slug: 'batch-evaluator',
  labels: { 'scion.io/template': 'true', 'scion.dev/workspace-mode': 'per-agent' },
};

interface FormOpts {
  templates?: unknown[];
  systemStatus?: Record<string, unknown>;
  cloneStatus?: number;
  cloneBody?: unknown;
  /** Paged template list: page 0 has no cursor; page i is fetched with cursor=p<i>. */
  templatePages?: { projects: unknown[]; nextCursor?: string }[];
  /** Per-page template response (overrides templatePages); page index as above. */
  templatePage?: (page: number) => { status?: number; body: unknown };
  /** Status for POST /api/v1/projects (201 created, 200 already exists). */
  createStatus?: number;
  validatePath?: Record<string, unknown>;
  providersStatus?: number;
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
      const cursor = new URL(path, 'http://x').searchParams.get('cursor');
      const page = cursor ? Number(cursor.slice(1)) : 0;
      if (opts.templatePage) {
        const { status, body } = opts.templatePage(page);
        return Promise.resolve(jsonResponse(body, status ?? 200));
      }
      if (opts.templatePages) {
        return Promise.resolve(jsonResponse(opts.templatePages[page]));
      }
      return Promise.resolve(jsonResponse({ projects: opts.templates ?? [] }));
    }
    if (method === 'POST' && path.endsWith('/api/v1/system/fs/validate-path')) {
      return Promise.resolve(jsonResponse(opts.validatePath ?? {}));
    }
    if (method === 'POST' && /\/api\/v1\/projects\/[^/]+\/providers$/.test(path)) {
      return Promise.resolve(jsonResponse({}, opts.providersStatus ?? 201));
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
      return Promise.resolve(
        jsonResponse({ project: { id: 'new-blank' } }, opts.createStatus ?? 201)
      );
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

  it('offers Git, Shared and Empty per agent only — no Hub-managed, From Template or Linked off-workstation', async () => {
    const { el } = await createForm({ systemStatus: { embeddedBrokerID: 'b1' } });
    element = el;

    expect(optionValues(el, '#mode')).toEqual(['git', 'shared', 'empty-per-agent']);
    const modeText = text(q(el, '#mode'));
    expect(modeText).toContain('Shared workspace directory');
    expect(modeText).not.toContain('Hub-managed');
    expect(modeText).not.toContain('From Template');
  });

  it('offers Local Directory (linked) only on a workstation hub with an embedded broker', async () => {
    let { el } = await createForm({ systemStatus: { workstation: true, embeddedBrokerID: 'b1' } });
    element = el;
    expect(optionValues(el, '#mode')).toEqual(['git', 'shared', 'empty-per-agent', 'linked']);
    el.remove();
    resetHubProjectCapabilitiesCache();

    ({ el } = await createForm({ systemStatus: { workstation: true } }));
    element = el;
    expect(optionValues(el, '#mode')).toEqual(['git', 'shared', 'empty-per-agent']);
  });

  it('labels Empty directory per agent with a New badge, a hint and a deletion note', async () => {
    const { el } = await createForm();
    element = el;

    const option = q(el, '#mode sl-option[value="empty-per-agent"]');
    expect(text(option)).toContain('Empty directory per agent');
    expect(text(option?.querySelector('sl-badge[slot="suffix"]'))).toBe('New');
    expect(q(el, '#mode sl-option[value="shared"] sl-badge')).toBeNull();
    expect(q(el, '.empty-per-agent-note')).toBeNull();

    await setValue(el, '#mode', 'empty-per-agent', 'sl-change');
    expect(text(q(el, '#mode')?.parentElement?.querySelector('.hint'))).toContain(
      'Each agent gets its own new, empty directory.'
    );
    expect(text(q(el, '.empty-per-agent-note'))).toContain('deleted when the agent is deleted');
    expect(q(el, '#gitRemote')).toBeNull();
  });

  it('Blank + Empty directory per agent posts workspaceMode per-agent with no gitRemote or label', async () => {
    const { el, requests } = await createForm();
    element = el;

    await setValue(el, '#mode', 'empty-per-agent', 'sl-change');
    await setValue(el, '#name', 'Scratch Runs', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/projects',
        method: 'POST',
        body: { name: 'Scratch Runs', slug: 'scratch-runs', workspaceMode: 'per-agent' },
      },
    ]);
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

  it('announces a membership change for a project created from Blank', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;
    const heard = heardMembership();

    await setValue(el, '#name', 'My Notes', 'sl-input');
    await submit(el);

    expect(heard).toEqual([{ kind: 'project', id: 'new-blank' }]);
  });

  it('announces a membership change for a project created from a template', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;
    const heard = heardMembership();

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'payments-api', 'sl-input');
    await submit(el);

    expect(heard).toEqual([{ kind: 'project', id: 'new-clone' }]);
  });

  it('announces nothing when the template clone fails', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE], cloneStatus: 500 });
    element = el;
    const heard = heardMembership();

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'payments-api', 'sl-input');
    await submit(el);

    expect(heard).toEqual([]);
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

  it.each([
    'git@github.com:Acme/go-service-template.git',
    // An explicit default port names the same repository (#2712 r4).
    'https://github.com:443/acme/go-service-template',
  ])('treats an override naming the template repository (%s) as not overridden', async (remote) => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'same-repo', 'sl-input');
    // Template remote is github.com/acme/go-service-template.
    await setValue(el, '#templateGitRemote', remote, 'sl-input');

    expect(text(q(el, '.badge-override'))).toBe('Override');
    expect(q(el, '.override-field.active')).toBeNull();
    expect(text(q(el, '.summary-repository'))).toBe('github.com/acme/go-service-template');
    expect(text(q(el, '.summary-repository'))).not.toContain('(override)');
    expect(text(q(el, '.summary-branch'))).toBe('develop');
    expect(text(q(el, '.override-hint'))).toContain('Same repository as the template');
    expect(text(q(el, '.override-hint'))).not.toContain('main branch');
    expect(q(el, '.warn-note')).toBeNull();
    // The reset link still clears what was typed.
    expect(q(el, '.reset-link')).not.toBeNull();

    await submit(el);
    expect(posts(requests)).toHaveLength(1);
  });

  it('never displays credentials, query or fragment from a pasted override', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(
      el,
      '#templateGitRemote',
      'https://x-access-token:ghp_SECRET@github.com/acme/payments.git?access_token=SECRET_Q#SECRET_F',
      'sl-input'
    );

    expect(text(q(el, '.summary-repository'))).toBe('github.com/acme/payments (override)');
    expect(el.shadowRoot?.textContent ?? '').not.toMatch(/SECRET|x-access-token/);
  });

  it.each([
    ['/home/user/code/repo', 'must be a remote git URL'],
    ['ssh://git@git.example.com:2222/group/repo.git', 'ssh URLs with a port are not supported yet'],
  ])('flags an invalid override (%s) inline before submitting', async (remote, message) => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'bad-remote', 'sl-input');
    await setValue(el, '#templateGitRemote', remote, 'sl-input');
    q(el, '#templateGitRemote')!.dispatchEvent(new Event('sl-blur'));
    await el.updateComplete;

    expect(text(q(el, '.git-remote-error'))).toContain(message);
    expect(q(el, '#templateGitRemote > .git-remote-error')?.getAttribute('slot')).toBe('help-text');
    expect(q(el, '#templateGitRemote')?.getAttribute('aria-invalid')).toBe('true');

    await submit(el);
    expect(posts(requests)).toEqual([]);
    expect(q(el, '.error-banner')).toBeNull();

    // Typing clears the error.
    await setValue(el, '#templateGitRemote', 'github.com/acme/other', 'sl-input');
    expect(q(el, '.git-remote-error')).toBeNull();
    expect(q(el, '#templateGitRemote')?.getAttribute('aria-invalid')).toBe('false');
  });

  it('flags an override whose ? or # falls inside the userinfo as invalid, without showing it', async () => {
    const { el, requests } = await createForm({ templates: [GIT_TEMPLATE] });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'userinfo-query', 'sl-input');
    await setValue(el, '#templateGitRemote', 'https://user:PSECRET?W@github.com/acme/payments.git', 'sl-input');
    q(el, '#templateGitRemote')!.dispatchEvent(new Event('sl-blur'));
    await el.updateComplete;

    expect(text(q(el, '.git-remote-error'))).toContain('must be a remote git URL');
    expect(q(el, '#templateGitRemote')?.getAttribute('aria-invalid')).toBe('true');
    expect(text(q(el, '.summary-repository'))).not.toContain('PSECRET');
    await submit(el);
    expect(posts(requests)).toEqual([]);
  });

  it('shows a hub 400 on gitRemote inline on the override field', async () => {
    const { el } = await createForm({
      templates: [GIT_TEMPLATE],
      cloneStatus: 400,
      cloneBody: {
        error: {
          code: 'validation_error',
          message: 'gitRemote must be a remote git URL',
          details: { field: 'gitRemote' },
        },
      },
    });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'hub-says-no', 'sl-input');
    await setValue(el, '#templateGitRemote', 'github.com/acme/other', 'sl-input');
    await submit(el);

    expect(text(q(el, '.git-remote-error'))).toContain('gitRemote must be a remote git URL');
    expect(q(el, '.error-banner')).toBeNull();
    expect(window.history.pushState).not.toHaveBeenCalled();
  });

  it('shows other clone 400s in the banner', async () => {
    const { el } = await createForm({
      templates: [GIT_TEMPLATE],
      cloneStatus: 400,
      cloneBody: { error: { code: 'validation_error', message: 'name is required' } },
    });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'x', 'sl-input');
    await submit(el);

    expect(text(q(el, '.error-banner'))).toContain('name is required');
    expect(q(el, '.git-remote-error')).toBeNull();
  });

  it('shows an error and does not navigate when the clone response has no ID', async () => {
    const { el } = await createForm({ templates: [GIT_TEMPLATE], cloneBody: {} });
    element = el;

    await setValue(el, '#startFrom', 'tpl-git', 'sl-change');
    await setValue(el, '#name', 'x', 'sl-input');
    await submit(el);

    expect(text(q(el, '.error-banner'))).toContain('No project ID in response');
    expect(window.history.pushState).not.toHaveBeenCalled();
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

  it('an empty-per-agent template shows the mode on the card and clones with {name}', async () => {
    const { el, requests } = await createForm({
      templates: [SHARED_TEMPLATE, EMPTY_PER_AGENT_TEMPLATE],
    });
    element = el;

    expect(text(q(el, '#startFrom sl-option[value="tpl-empty"]'))).toContain(
      'Empty directory per agent'
    );
    await setValue(el, '#startFrom', 'tpl-empty', 'sl-change');
    expect(q(el, '#templateGitRemote')).toBeNull();
    expect(q(el, '#mode')).toBeNull();
    expect(text(q(el, '.summary-workspace-type'))).toBe('Empty directory per agent');
    expect(q(el, '.summary-repository')).toBeNull();

    await setValue(el, '#name', 'Batch eval', 'sl-input');
    await submit(el);

    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/projects/tpl-empty/clone',
        method: 'POST',
        body: { name: 'Batch eval' },
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

    // a11y: the input is marked invalid and the error is its help text, which
    // sl-input wires to the native input via aria-describedby.
    expect(q(el, '#slug')?.getAttribute('aria-invalid')).toBe('true');
    expect(q(el, '#slug > .slug-error')?.getAttribute('slot')).toBe('help-text');

    await setValue(el, '#slug', 'taken-2', 'sl-input');
    expect(q(el, '.slug-error')).toBeNull();
    expect(q(el, '#slug')?.getAttribute('aria-invalid')).toBe('false');
  });

  it('shows a clone 409 in the banner, not on Slug, when no slug was sent', async () => {
    const { el, requests } = await createForm({
      templates: [SHARED_TEMPLATE],
      cloneStatus: 409,
      cloneBody: { error: { code: 'conflict', message: 'Template is being modified' } },
    });
    element = el;

    await setValue(el, '#startFrom', 'tpl-shared', 'sl-change');
    await setValue(el, '#name', 'Fresh', 'sl-input');
    await submit(el);

    expect(posts(requests)[0].body).toEqual({ name: 'Fresh' });
    expect(q(el, '.slug-error')).toBeNull();
    expect(q(el, '#slug')?.getAttribute('aria-invalid')).toBe('false');
    expect(text(q(el, '.error-banner'))).toContain('Template is being modified');
    expect(window.history.pushState).not.toHaveBeenCalled();
  });

  it('follows nextCursor so the template list is not truncated', async () => {
    const { el, requests } = await createForm({
      templatePages: [
        { projects: [GIT_TEMPLATE], nextCursor: 'p1' },
        { projects: [SHARED_TEMPLATE] },
      ],
    });
    element = el;

    const listCalls = requests.filter((r) => r.path.includes('isTemplate=true'));
    expect(listCalls.map((r) => r.path)).toEqual([
      '/api/v1/projects?isTemplate=true',
      '/api/v1/projects?isTemplate=true&cursor=p1',
    ]);
    expect(optionValues(el, '#startFrom')).toEqual(['blank', 'tpl-git', 'tpl-shared']);
    expect(text(q(el, '.start-from-hint'))).not.toContain("Couldn't");
  });

  it('keeps the templates already loaded when a later page fails', async () => {
    const { el } = await createForm({
      templatePage: (page) =>
        page === 0
          ? { body: { projects: [GIT_TEMPLATE], nextCursor: 'p1' } }
          : { status: 500, body: { error: { code: 'internal', message: 'boom' } } },
    });
    element = el;

    expect(optionValues(el, '#startFrom')).toEqual(['blank', 'tpl-git']);
    expect(text(q(el, '.start-from-hint'))).toContain("Couldn't load all project templates");
  });

  it('says the list is incomplete when the first page fails', async () => {
    const { el } = await createForm({
      templatePage: () => ({ status: 500, body: { error: { code: 'internal' } } }),
    });
    element = el;

    expect(optionValues(el, '#startFrom')).toEqual(['blank']);
    expect(text(q(el, '.start-from-hint'))).toContain("Couldn't load project templates");
  });

  it('stops at the page cap with a warning instead of truncating silently', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { el, requests } = await createForm({
      templatePage: (page) => ({
        body: { projects: [{ ...SHARED_TEMPLATE, id: `tpl-${page}` }], nextCursor: `p${page + 1}` },
      }),
    });
    element = el;

    const listCalls = requests.filter((r) => r.path.includes('isTemplate=true'));
    expect(listCalls).toHaveLength(20);
    expect(optionValues(el, '#startFrom')).toHaveLength(21); // Blank + 20 templates
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('incomplete'));
    expect(text(q(el, '.start-from-hint'))).toContain("Couldn't load all project templates");
  });

  it('labels the Start from and Workspace Type selects through sl-select', async () => {
    const { el } = await createForm();
    element = el;

    expect(q(el, '#startFrom')?.getAttribute('label')).toBe('Start from');
    expect(q(el, '#mode')?.getAttribute('label')).toBe('Workspace Type');
    // A <label for> cannot reach the control inside sl-select's shadow DOM.
    expect(q(el, 'label[for="startFrom"]')).toBeNull();
    expect(q(el, 'label[for="mode"]')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Blank + Local Directory (linked): two-step create, and the 200 "exists" path.
// ---------------------------------------------------------------------------

describe('scion-page-project-create — linked create and existing projects', () => {
  let element: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  const WORKSTATION = { workstation: true, embeddedBrokerID: 'broker-1' };
  const VALID_DIR = {
    resolved: '/home/u/code/notes',
    exists: true,
    isDir: true,
    isGit: false,
    isManaged: false,
    alreadyLinked: false,
  };

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

  /** Pick Linked, enter a path, and wait out the validate-path debounce. */
  async function fillLinked(el: HTMLElement & { updateComplete: Promise<boolean> }) {
    await setValue(el, '#mode', 'linked', 'sl-change');
    await setValue(el, '#name', 'Notes', 'sl-input');
    await setValue(el, '#localPath', '~/code/notes', 'sl-input');
    await new Promise((resolve) => setTimeout(resolve, 600));
    await settle(el);
  }

  it('validates the path, creates the project, then links the directory', async () => {
    const { el, requests } = await createForm({
      systemStatus: WORKSTATION,
      validatePath: VALID_DIR,
    });
    element = el;

    await fillLinked(el);
    await submit(el);

    expect(posts(requests)).toEqual([
      {
        path: '/api/v1/system/fs/validate-path',
        method: 'POST',
        body: { path: '~/code/notes' },
      },
      { path: '/api/v1/projects', method: 'POST', body: { name: 'Notes', slug: 'notes' } },
      {
        path: '/api/v1/projects/new-blank/providers',
        method: 'POST',
        body: { brokerId: 'broker-1', localPath: '/home/u/code/notes' },
      },
    ]);
    expect(window.history.pushState).toHaveBeenCalledWith({}, '', '/projects/new-blank');
  });

  it('still links the directory when the project already exists (200)', async () => {
    const { el, requests } = await createForm({
      systemStatus: WORKSTATION,
      validatePath: VALID_DIR,
      createStatus: 200,
    });
    element = el;
    const heard = heardMembership();

    await fillLinked(el);
    await submit(el);

    expect(heard).toEqual([]);

    expect(posts(requests).map((r) => r.path)).toEqual([
      '/api/v1/system/fs/validate-path',
      '/api/v1/projects',
      '/api/v1/projects/new-blank/providers',
    ]);
    expect(q(el, 'sl-dialog[label="Project Already Exists"]')?.hasAttribute('open')).toBe(false);
    expect(window.history.pushState).toHaveBeenCalledWith({}, '', '/projects/new-blank');
  });

  it('keeps the user on the form with an error when linking fails', async () => {
    const { el } = await createForm({
      systemStatus: WORKSTATION,
      validatePath: VALID_DIR,
      providersStatus: 500,
    });
    element = el;
    const heard = heardMembership();

    await fillLinked(el);
    await submit(el);

    // The project was created before the link failed.
    expect(heard).toEqual([{ kind: 'project', id: 'new-blank' }]);

    expect(q(el, '.error-banner')).not.toBeNull();
    expect(window.history.pushState).not.toHaveBeenCalled();
  });

  it('does not create when the path is not a valid directory', async () => {
    const { el, requests } = await createForm({
      systemStatus: WORKSTATION,
      validatePath: { ...VALID_DIR, exists: false },
    });
    element = el;

    await fillLinked(el);
    await submit(el);

    expect(posts(requests).map((r) => r.path)).toEqual(['/api/v1/system/fs/validate-path']);
    expect(text(q(el, '.error-banner'))).toContain('valid directory');
  });

  it('a non-linked 200 offers the existing project instead of navigating', async () => {
    const { el } = await createForm({ createStatus: 200 });
    element = el;
    const heard = heardMembership();

    await setValue(el, '#name', 'Existing', 'sl-input');
    await submit(el);

    expect(heard).toEqual([]);

    expect(q(el, 'sl-dialog[label="Project Already Exists"]')?.hasAttribute('open')).toBe(true);
    expect(window.history.pushState).not.toHaveBeenCalled();
  });
});

describe('cloneUrlCredentialHint', () => {
  let hint: (remote: string) => string | null;
  beforeAll(async () => {
    ({ cloneUrlCredentialHint: hint } = await import('./project-create.js'));
  });

  it.each([
    'https://user:pass@github.com/org/repo',
    'https://TOKEN@github.com/org/repo',
    'http://user@internal.host/repo',
    'user:pass@github.com/org/repo',
    'TOKEN@github.com/org/repo',
    'ssh://git:secret@github.com/org/repo',
    'https://user:8443/x@host/repo',
    'https://github.com/org/repo@v1',
    'user:PW@host/org/repo://',
    'https:/user:PW@host/r',
    'git@host:repo@v1',
    'git@PW@host:org/repo',
    'git@a@b@host:x',
    'git@user:PW@host:org/repo',
    '//user:PW@host/repo',
  ])('flags userinfo in %s', (url) => {
    expect(hint(url)).toMatch(/username, password or token/);
  });

  it.each(['https://github.com/org/repo?access_token=x', 'https://github.com/org/repo#frag'])(
    'flags query or fragment in %s',
    (url) => {
      expect(hint(url)).toMatch(/query string or fragment/);
    },
  );

  it.each([
    'https://github.com/org/repo',
    'github.com/org/repo',
    'git@github.com:org/repo.git',
    'ssh://git@github.com/org/repo.git',
    'https://github.com:8443/org/repo',
    'deploy@host:team/proj',
    '/tmp/repo#1',
  ])('accepts %s', (url) => {
    expect(hint(url)).toBeNull();
  });

  it('flags control characters', () => {
    expect(hint('https://host/r\nhttps://u:PW@h/x')).toMatch(/control or non-ASCII/);
  });
});

describe('deriveCloneUrl', () => {
  let derive: (remote: string) => string;
  beforeAll(async () => {
    ({ deriveCloneUrl: derive } = await import('./project-create.js'));
  });

  it.each(['https://user:PSECRET?W@github.com/org/repo', ''])('returns empty for %j', (input) => {
    expect(derive(input)).toBe('');
  });

  it.each([
    ['https://github.com/org/repo', 'https://github.com/org/repo.git'],
    ['github.com/org/repo.git', 'https://github.com/org/repo.git'],
    ['git@github.com:org/repo.git', 'https://github.com/org/repo.git'],
    ['ssh://git@github.com/org/repo', 'https://github.com/org/repo.git'],
    ['deploy@host:team/proj', 'https://host/team/proj.git'],
    ['https://user:pass@github.com/org/repo', 'https://github.com/org/repo.git'],
    ['https://github.com/org/repo?ref=main#x', 'https://github.com/org/repo.git'],
    ['https://dev.azure.com/org/proj/_git/repo.git', 'https://dev.azure.com/org/proj/_git/repo'],
    ['ssh://git@host:22/org/repo', 'https://host/org/repo.git'],
    ['SSH://git@host:2222/org/repo.git', 'https://host/org/repo.git'],
    ['https://host:8443/org/repo', 'https://host:8443/org/repo.git'],
  ])('derives %s', (input, want) => {
    const got = derive(input);
    expect(got).toBe(want);
    expect(got).not.toContain('@');
  });
});
