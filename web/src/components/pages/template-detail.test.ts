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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render, type TemplateResult } from 'lit';

const showConfirmMock = vi.fn<(message: string, options?: unknown) => Promise<boolean>>();
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: (message: string, options?: unknown) => showConfirmMock(message, options),
}));

import type { Template } from '../../shared/types.js';
import type { ScionHashDisplay } from '../shared/hash-display.js';
import type { ScionBreadcrumb } from '../shared/breadcrumb.js';
import type { ScionFileEditor } from '../shared/file-editor.js';
import { mayLeave } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { crumbTuples } from '../../test-utils/crumbs.js';

const TAG = 'scion-page-template-detail';
const FULL_SHA = '0123456789abcdef0123456789abcdef01234567';

beforeAll(async () => {
  await import('./template-detail.js');
});

function renderTemplateHeader(template: Partial<Template>): HTMLElement {
  const el = document.createElement(TAG);
  const full: Template = {
    id: 'tpl-1',
    name: 'my-tpl',
    slug: 'my-tpl',
    harness: 'claude',
    status: 'active',
    scope: 'project',
    ...template,
  };
  Object.assign(el, { loading: false, template: full });
  const tpl = (el as unknown as { renderHeader: () => TemplateResult }).renderHeader();
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

describe('scion-page-template-detail sourceCommit', () => {
  it('renders short SHA with GitHub tree link and full SHA on hash-display', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'https://github.com/acme/repo/tree/main/.scion/templates/my-tpl',
      sourceCommit: FULL_SHA,
    });
    const meta = host.querySelector('.source-commit-meta');
    expect(meta).not.toBeNull();
    const display = meta!.querySelector('scion-hash-display') as ScionHashDisplay | null;
    expect(display).not.toBeNull();
    expect(display!.hash).toBe(FULL_SHA);
    expect(display!.displayText).toBe('0123456');
    expect(display!.href).toBe(
      `https://github.com/acme/repo/tree/${FULL_SHA}/.scion/templates/my-tpl`
    );
  });

  it('renders muted hint when GitHub sourceUrl is set but sourceCommit is empty', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'https://github.com/acme/repo/tree/main/.scion/templates/my-tpl',
    });
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    const empty = host.querySelector('.source-commit-empty');
    expect(empty).not.toBeNull();
    expect(empty!.textContent).toContain('not recorded — refresh from source to record');
  });

  it('hides sourceCommit row when neither sourceCommit nor refreshable sourceUrl is set', () => {
    const host = renderTemplateHeader({});
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    expect(host.querySelector('.source-commit-empty')).toBeNull();
  });

  it('hides sourceCommit hint for builtin:// sources', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'builtin://scion/1.0/template/default',
    });
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    expect(host.querySelector('.source-commit-empty')).toBeNull();
  });
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-page-template-detail file editor breadcrumbs and dirty-check (#9024)', () => {
  let el: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeEach(() => {
    showConfirmMock.mockReset();
    window.history.replaceState({}, '', '/');
  });

  afterEach(() => {
    el?.remove();
    el = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    window.history.replaceState({}, '', '/');
  });

  async function mountTemplatePage(
    pathname: string,
    templateOverrides: Partial<Template> = {}
  ): Promise<HTMLElement & { updateComplete: Promise<boolean> }> {
    const template: Template = {
      id: 'tpl-1',
      name: 'base',
      slug: 'base',
      harness: 'claude',
      status: 'active',
      scope: pathname.startsWith('/projects/')
        ? 'project'
        : pathname.startsWith('/profile/')
          ? 'user'
          : 'hub',
      _capabilities: { actions: ['read', 'update'] },
      ...templateOverrides,
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(`/api/v1/templates/${template.id}/files/`)) {
          return Promise.resolve(
            jsonResponse({
              path: '.scion/agent.md',
              content: 'original content',
              size: 16,
              modTime: '2026-01-01T00:00:00Z',
              encoding: 'utf-8',
            })
          );
        }
        if (url.includes(`/api/v1/templates/${template.id}/files`)) {
          return Promise.resolve(
            jsonResponse({
              files: [{ path: '.scion/agent.md', size: 16, modTime: '2026-01-01T00:00:00Z' }],
            })
          );
        }
        if (url.includes(`/api/v1/templates/${template.id}`)) {
          return Promise.resolve(jsonResponse(template));
        }
        return Promise.resolve(jsonResponse({}, 404));
      })
    );

    window.history.replaceState({}, '', pathname);
    const page = document.createElement(TAG) as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    document.body.appendChild(page);
    await page.updateComplete;
    await new Promise((r) => setTimeout(r, 20));
    await page.updateComplete;
    el = page;
    return page;
  }

  async function openEditor(
    page: HTMLElement & { updateComplete: Promise<boolean> },
    path: string | null
  ): Promise<ScionFileEditor> {
    const browser = page.shadowRoot!.querySelector('scion-file-browser')!;
    expect(browser).not.toBeNull();
    if (path === null) {
      browser.dispatchEvent(
        new CustomEvent('file-create-requested', { bubbles: true, composed: true })
      );
    } else {
      browser.dispatchEvent(
        new CustomEvent('file-edit-requested', {
          detail: { path },
          bubbles: true,
          composed: true,
        })
      );
    }
    await page.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    const editor = page.shadowRoot!.querySelector<ScionFileEditor>('scion-file-editor')!;
    expect(editor).not.toBeNull();
    await editor.updateComplete;
    return editor;
  }

  it('appends the file path crumb and links the entity crumb across project, hub, and user scopes', async () => {
    stateManager.seedProjects([
      {
        id: 'p-1',
        name: 'Payments',
        slug: 'payments',
        path: '/tmp/payments',
        status: 'active',
        agentCount: 0,
      },
    ]);

    const projectPage = await mountTemplatePage('/projects/p-1/templates/tpl-1');
    await openEditor(projectPage, '.scion/agent.md');
    expect(crumbTuples(projectPage.shadowRoot!)).toEqual([
      ['Projects', '/projects'],
      ['Payments', '/projects/p-1'],
      ['Settings', '/projects/p-1/settings'],
      ['Templates', '/projects/p-1/settings?tab=templates'],
      ['base', '/projects/p-1/templates/tpl-1'],
      ['.scion/agent.md', undefined],
    ]);
    const crumbs =
      projectPage.shadowRoot!.querySelector<ScionBreadcrumb>('scion-breadcrumb')!.crumbs;
    expect(crumbs[4].truncate).toBe(true);
    expect(crumbs[5].truncate).toBe(true);
    projectPage.remove();
    el = null;

    const hubPage = await mountTemplatePage('/settings/templates/tpl-1');
    await openEditor(hubPage, '.scion/agent.md');
    expect(crumbTuples(hubPage.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Templates', '/settings?tab=templates'],
      ['base', '/settings/templates/tpl-1'],
      ['.scion/agent.md', undefined],
    ]);
    hubPage.remove();
    el = null;

    const userPage = await mountTemplatePage('/profile/templates/tpl-1');
    await openEditor(userPage, '.scion/agent.md');
    expect(crumbTuples(userPage.shadowRoot!)).toEqual([
      ['Profile', '/profile'],
      ['Templates', '/profile/templates'],
      ['base', '/profile/templates/tpl-1'],
      ['.scion/agent.md', undefined],
    ]);
  });

  it('shows "New file" when creating a file and updates the crumb when saved', async () => {
    const page = await mountTemplatePage('/settings/templates/tpl-1');
    const editor = await openEditor(page, null);

    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Templates', '/settings?tab=templates'],
      ['base', '/settings/templates/tpl-1'],
      ['New file', undefined],
    ]);

    editor.dispatchEvent(
      new CustomEvent('file-saved', {
        detail: { path: 'created.md' },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;

    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Templates', '/settings?tab=templates'],
      ['base', '/settings/templates/tpl-1'],
      ['created.md', undefined],
    ]);
  });

  it('entity crumb plain click closes clean editor via preventDefault, prompts when dirty, and ignores modifier clicks', async () => {
    const page = await mountTemplatePage('/settings/templates/tpl-1');
    const editor = await openEditor(page, '.scion/agent.md');

    const breadcrumb = page.shadowRoot!.querySelector<ScionBreadcrumb>('scion-breadcrumb')!;
    await breadcrumb.updateComplete;
    const entityLink = Array.from(breadcrumb.shadowRoot!.querySelectorAll('a')).find(
      (a) => a.textContent?.trim() === 'base'
    )!;
    expect(entityLink).toBeTruthy();

    // Modifier click does not preventDefault or close editor.
    const metaClick = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      metaKey: true,
    });
    entityLink.dispatchEvent(metaClick);
    expect(metaClick.defaultPrevented).toBe(false);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    // Make dirty and plain-click entity crumb: cancel keeps editor open.
    editor.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: 'edited' },
        bubbles: true,
        composed: true,
      })
    );
    await editor.updateComplete;

    showConfirmMock.mockResolvedValueOnce(false);
    const dirtyClickCancel = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    entityLink.dispatchEvent(dirtyClickCancel);
    expect(dirtyClickCancel.defaultPrevented).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    // Accept confirm: closes editor and restores entity as current crumb.
    showConfirmMock.mockResolvedValueOnce(true);
    const dirtyClickConfirm = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    entityLink.dispatchEvent(dirtyClickConfirm);
    expect(dirtyClickConfirm.defaultPrevented).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(page.shadowRoot!.querySelector('scion-file-editor')).toBeNull();
    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Templates', '/settings?tab=templates'],
      ['base', undefined],
    ]);

    // Reopen clean and plain-click entity crumb: closes without prompting.
    await openEditor(page, '.scion/agent.md');
    await breadcrumb.updateComplete;
    const cleanEntityLink = Array.from(breadcrumb.shadowRoot!.querySelectorAll('a')).find(
      (a) => a.textContent?.trim() === 'base'
    )!;
    const cleanClick = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    cleanEntityLink.dispatchEvent(cleanClick);
    expect(cleanClick.defaultPrevented).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(showConfirmMock).toHaveBeenCalledTimes(2);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).toBeNull();
  });

  it('Back to files routes through requestClose and leave guard protects unsaved edits', async () => {
    const confirmSpy = vi.fn(() => false);
    vi.stubGlobal('confirm', confirmSpy);
    const page = await mountTemplatePage('/settings/templates/tpl-1');
    const editor = await openEditor(page, '.scion/agent.md');

    expect(mayLeave()).toBe(true);
    expect(confirmSpy).not.toHaveBeenCalled();

    editor.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: 'dirty edit' },
        bubbles: true,
        composed: true,
      })
    );
    await editor.updateComplete;

    // Leave guard vetoes when dirty and user cancels.
    expect(mayLeave()).toBe(false);
    expect(confirmSpy).toHaveBeenCalledWith('You have unsaved changes. Leave anyway?');

    // Back to files prompts and keeps editor open on cancel.
    const backBtn = Array.from(page.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      (b.textContent ?? '').includes('Back to files')
    )!;
    showConfirmMock.mockResolvedValueOnce(false);
    backBtn.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    // Disconnect clears the leave guard.
    page.remove();
    el = null;
    expect(mayLeave()).toBe(true);
  });
});
