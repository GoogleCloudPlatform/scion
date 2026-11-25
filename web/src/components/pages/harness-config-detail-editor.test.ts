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
 * Tests for harness-config-detail in-page file editor breadcrumbs,
 * dirty-check close flow, and router leave guard (ptone/scion#9024).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

const showConfirmMock = vi.fn<(message: string, options?: unknown) => Promise<boolean>>();
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: (message: string, options?: unknown) => showConfirmMock(message, options),
}));

import type { HarnessConfig } from '../../shared/types.js';
import type { ScionBreadcrumb } from '../shared/breadcrumb.js';
import type { ScionFileEditor } from '../shared/file-editor.js';
import { mayLeave } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { crumbTuples } from '../../test-utils/crumbs.js';

const TAG = 'scion-page-harness-config-detail';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-page-harness-config-detail file editor breadcrumbs and dirty-check (#9024)', () => {
  let el: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeAll(async () => {
    await import('./harness-config-detail.js');
  }, 60_000);

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

  async function mountHarnessConfigPage(
    pathname: string,
    overrides: Partial<HarnessConfig> = {}
  ): Promise<HTMLElement & { updateComplete: Promise<boolean> }> {
    const hc: HarnessConfig = {
      id: 'hc-1',
      name: 'claude',
      slug: 'claude',
      harness: 'claude',
      status: 'active',
      scope: pathname.startsWith('/projects/') ? 'project' : 'hub',
      _capabilities: { actions: ['read', 'update'] },
      ...overrides,
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(`/api/v1/harness-configs/${hc.id}/files/`)) {
          return Promise.resolve(
            jsonResponse({
              path: 'config.yaml',
              content: 'harness: claude\n',
              size: 16,
              modTime: '2026-01-01T00:00:00Z',
              encoding: 'utf-8',
            })
          );
        }
        if (url.includes(`/api/v1/harness-configs/${hc.id}/files`)) {
          return Promise.resolve(
            jsonResponse({
              files: [{ path: 'config.yaml', size: 16, modTime: '2026-01-01T00:00:00Z' }],
            })
          );
        }
        if (url.includes(`/api/v1/harness-configs/${hc.id}`)) {
          return Promise.resolve(jsonResponse(hc));
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

  it('appends the file path crumb and links the entity crumb across project and hub scopes', async () => {
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

    const projectPage = await mountHarnessConfigPage('/projects/p-1/harness-configs/hc-1');
    await openEditor(projectPage, 'config.yaml');
    expect(crumbTuples(projectPage.shadowRoot!)).toEqual([
      ['Projects', '/projects'],
      ['Payments', '/projects/p-1'],
      ['Settings', '/projects/p-1/settings'],
      ['Harness Configs', '/projects/p-1/settings?tab=harness-configs'],
      ['claude', '/projects/p-1/harness-configs/hc-1'],
      ['config.yaml', undefined],
    ]);
    const crumbs =
      projectPage.shadowRoot!.querySelector<ScionBreadcrumb>('scion-breadcrumb')!.crumbs;
    expect(crumbs[4].truncate).toBe(true);
    expect(crumbs[5].truncate).toBe(true);
    projectPage.remove();
    el = null;

    const hubPage = await mountHarnessConfigPage('/settings/harness-configs/hc-1');
    await openEditor(hubPage, 'config.yaml');
    expect(crumbTuples(hubPage.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Harness Configs', '/settings?tab=harness-configs'],
      ['claude', '/settings/harness-configs/hc-1'],
      ['config.yaml', undefined],
    ]);
  });

  it('shows "New file" when creating a file and updates the crumb when saved', async () => {
    const page = await mountHarnessConfigPage('/settings/harness-configs/hc-1');
    const editor = await openEditor(page, null);

    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Harness Configs', '/settings?tab=harness-configs'],
      ['claude', '/settings/harness-configs/hc-1'],
      ['New file', undefined],
    ]);

    editor.dispatchEvent(
      new CustomEvent('file-saved', {
        detail: { path: 'Dockerfile' },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;

    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Harness Configs', '/settings?tab=harness-configs'],
      ['claude', '/settings/harness-configs/hc-1'],
      ['Dockerfile', undefined],
    ]);
  });

  it('entity crumb plain click closes editor through dirty check and ignores modifier clicks', async () => {
    const page = await mountHarnessConfigPage('/settings/harness-configs/hc-1');
    const editor = await openEditor(page, 'config.yaml');

    const breadcrumb = page.shadowRoot!.querySelector<ScionBreadcrumb>('scion-breadcrumb')!;
    await breadcrumb.updateComplete;
    const entityLink = Array.from(breadcrumb.shadowRoot!.querySelectorAll('a')).find(
      (a) => a.textContent?.trim() === 'claude'
    )!;
    expect(entityLink).toBeTruthy();

    // Ctrl-click leaves the editor open without preventDefault.
    const ctrlClick = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      ctrlKey: true,
    });
    entityLink.dispatchEvent(ctrlClick);
    expect(ctrlClick.defaultPrevented).toBe(false);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    // Dirty editor: cancel keeps it open; confirm closes it.
    editor.shadowRoot!.querySelector('scion-code-editor')!.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: 'harness: claude\nimage: custom\n' },
        bubbles: true,
        composed: true,
      })
    );
    await editor.updateComplete;

    showConfirmMock.mockResolvedValueOnce(false);
    const cancelClick = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    entityLink.dispatchEvent(cancelClick);
    expect(cancelClick.defaultPrevented).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    showConfirmMock.mockResolvedValueOnce(true);
    const confirmClick = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    entityLink.dispatchEvent(confirmClick);
    expect(confirmClick.defaultPrevented).toBe(true);
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(page.shadowRoot!.querySelector('scion-file-editor')).toBeNull();
    expect(crumbTuples(page.shadowRoot!)).toEqual([
      ['Hub Resources', '/settings'],
      ['Harness Configs', '/settings?tab=harness-configs'],
      ['claude', undefined],
    ]);
  });

  it('Back to files routes through requestClose and leave guard protects unsaved edits', async () => {
    const confirmSpy = vi.fn(() => false);
    vi.stubGlobal('confirm', confirmSpy);
    const page = await mountHarnessConfigPage('/settings/harness-configs/hc-1');
    const editor = await openEditor(page, 'config.yaml');

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

    expect(mayLeave()).toBe(false);
    expect(confirmSpy).toHaveBeenCalledWith('You have unsaved changes. Leave anyway?');

    const backBtn = Array.from(page.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      (b.textContent ?? '').includes('Back to files')
    )!;
    showConfirmMock.mockResolvedValueOnce(false);
    backBtn.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(page.shadowRoot!.querySelector('scion-file-editor')).not.toBeNull();

    showConfirmMock.mockResolvedValueOnce(true);
    backBtn.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(page.shadowRoot!.querySelector('scion-file-editor')).toBeNull();

    // After closing, leave guard no longer prompts.
    expect(mayLeave()).toBe(true);
    expect(confirmSpy).toHaveBeenCalledTimes(1);

    // Disconnect removes the guard.
    page.remove();
    el = null;
    expect(mayLeave()).toBe(true);
  });
});
