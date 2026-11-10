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
 * Project settings > Danger Zone: the Leave this project row, above Delete,
 * follows the project's leave capabilities (ptone/scion#7494); a project
 * that answers 404 renders "you no longer have access".
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { Capabilities } from '../../shared/types.js';
import type { ScionPageProjectSettings } from './project-settings.js';
import { LEAVE_BLOCKED_LAST_OWNER, PROJECT_NO_ACCESS_TITLE } from '../../client/project-leave.js';

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function requestPath(url: string | URL | Request): string {
  return typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
}

function createFetchHandler(caps: Capabilities | null) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = requestPath(url);
    if (path.match(/\/api\/v1\/projects\/[^/?]+(\?|$)/)) {
      if (!caps) return json({ error: { code: 'not_found', message: 'Project not found.' } }, 404);
      return json({
        id: 'proj-1',
        name: 'Test Project',
        slug: 'test-project',
        _capabilities: caps,
      });
    }
    if (path.includes('/settings/resolved')) {
      return json({ projectId: 'proj-1', settings: {}, resolvedSettings: {} });
    }
    if (path.includes('/templates')) {
      return json({ templates: [], page: 1, pageSize: 100, total: 0 });
    }
    return json({});
  };
}

async function createComponent(caps: Capabilities | null): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(caps)));
  const el = document.createElement('scion-page-project-settings') as InstanceType<
    typeof ScionPageProjectSettings
  >;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

const danger = (el: HTMLElement) => el.shadowRoot?.querySelector('.danger-section') ?? null;
const leaveRow = (el: HTMLElement) => el.shadowRoot?.querySelector('[data-leave-row]') ?? null;

describe('project-settings: Danger Zone Leave row (ptone/scion#7494)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({ actions: [] })));
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows Leave above Delete for an owner who can leave', async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: true,
    });
    const rows = Array.from(danger(element)!.querySelectorAll('.delete-area'));
    expect(rows).toHaveLength(2);
    expect(rows[0].hasAttribute('data-leave-row')).toBe(true);
    expect(rows[1].textContent).toContain('Delete this project');
    const button = rows[0].querySelector('[data-leave-project]')!;
    expect(button.hasAttribute('disabled')).toBe(false);
  });

  it('an admin without delete still gets the Danger Zone with only Leave', async () => {
    element = await createComponent({ actions: ['read', 'update', 'manage'], canLeave: true });
    expect(leaveRow(element)).not.toBeNull();
    expect(danger(element)!.textContent).not.toContain('Delete this project');
  });

  it('no Leave row without the leave capabilities (experiment off)', async () => {
    element = await createComponent({ actions: ['read', 'update', 'delete', 'manage'] });
    expect(leaveRow(element)).toBeNull();
    expect(danger(element)!.textContent).toContain('Delete this project');
  });

  it('without delete and without leave, the Permissions note stays', async () => {
    element = await createComponent({ actions: ['read', 'update'] });
    expect(danger(element)).toBeNull();
    expect(element.shadowRoot?.textContent).toContain("You don't have permission to modify");
  });

  it('the last owner sees Leave disabled with guidance', async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: false,
      leaveBlockedReason: 'last_owner',
    });
    const row = leaveRow(element)!;
    expect(row.querySelector('[data-leave-project]')!.hasAttribute('disabled')).toBe(true);
    expect(row.querySelector('[data-leave-blocked]')?.textContent).toBe(LEAVE_BLOCKED_LAST_OWNER);
  });

  it('a project that answers 404 renders "you no longer have access"', async () => {
    element = await createComponent(null);
    expect(element.shadowRoot?.querySelector('[data-project-no-access]')?.textContent).toContain(
      PROJECT_NO_ACCESS_TITLE
    );
    expect(element.shadowRoot?.textContent).not.toContain('Failed to Load Settings');
  });

  /** GETs of the project itself (the page load and any reload). */
  function projectGets(): number {
    return vi
      .mocked(fetch)
      .mock.calls.filter(([u]) => /\/api\/v1\/projects\/proj-1(\?|$)/.test(requestPath(u))).length;
  }

  type EditorHandle = HTMLElement & {
    openTransferOnLoad: boolean;
    openTransferOwnership: () => void;
  };
  const editorOf = (el: HTMLElement): EditorHandle =>
    el.shadowRoot!.querySelector('scion-project-members-editor') as EditorHandle;
  const dialogOf = (el: HTMLElement): HTMLElement =>
    el.shadowRoot!.querySelector('scion-project-leave-dialog') as HTMLElement;

  it('?transfer=1 is read once: the editor is asked to open transfer, and only that param leaves the URL', async () => {
    window.history.replaceState(null, '', '/projects/proj-1/settings?transfer=1&tab=templates');
    try {
      element = await createComponent({ actions: ['read', 'update', 'delete', 'manage'] });
      expect(editorOf(element).openTransferOnLoad).toBe(true);
      expect(window.location.search).toBe('?tab=templates');
      // After the editor's one-time open, the flag is cleared.
      editorOf(element).dispatchEvent(
        new CustomEvent('scion-transfer-opened-on-load', { bubbles: true, composed: true })
      );
      await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
      expect(editorOf(element).openTransferOnLoad).toBe(false);
    } finally {
      window.history.replaceState(null, '', '/');
    }
  });

  it('without ?transfer the editor is not asked to open transfer', async () => {
    window.history.replaceState(null, '', '/projects/proj-1/settings?tab=templates');
    try {
      element = await createComponent({ actions: ['read', 'update', 'delete', 'manage'] });
      expect(editorOf(element).openTransferOnLoad).toBe(false);
      expect(window.location.search).toBe('?tab=templates');
    } finally {
      window.history.replaceState(null, '', '/');
    }
  });

  it('a leave that kept access (from the Danger Zone or the members editor) reloads the project', async () => {
    element = await createComponent({ actions: ['read', 'update', 'manage'], canLeave: true });
    const ev = (remainingAccess: string) =>
      new CustomEvent('scion-project-left', {
        detail: { projectId: 'proj-1', result: { remainingAccess } },
        bubbles: true,
        composed: true,
      });
    // After a reload the page has rendered again: loading done, dialog and editor back.
    const page = element;
    const settled = async (): Promise<void> => {
      await vi.waitFor(() => {
        expect((page as unknown as { loading: boolean }).loading).toBe(false);
        expect(page.shadowRoot!.querySelector('scion-project-leave-dialog')).not.toBeNull();
        expect(page.shadowRoot!.querySelector('scion-project-members-editor')).not.toBeNull();
      });
    };
    const n0 = projectGets();
    dialogOf(element).dispatchEvent(ev('group'));
    await vi.waitFor(() => expect(projectGets()).toBe(n0 + 1));
    await settled();
    const n1 = projectGets();
    editorOf(element).dispatchEvent(ev('hub'));
    await vi.waitFor(() => expect(projectGets()).toBe(n1 + 1));
    await settled();
    const n2 = projectGets();
    dialogOf(element).dispatchEvent(ev('none'));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(projectGets()).toBe(n2);
  });

  it('an enabled Leave Project button opens the dialog (the planted positive for the blocked case)', async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: true,
    });
    const dialog = dialogOf(element) as HTMLElement & { show: () => Promise<void> };
    const show = vi.spyOn(dialog, 'show').mockResolvedValue();
    leaveRow(element)!.querySelector<HTMLElement>('[data-leave-project]')!.click();
    expect(show).toHaveBeenCalledTimes(1);
  });

  it('a disabled (blocked) Leave Project button does not open the dialog', async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: false,
      leaveBlockedReason: 'last_owner',
    });
    const dialog = dialogOf(element) as HTMLElement & { show: () => Promise<void> };
    const show = vi.spyOn(dialog, 'show').mockResolvedValue();
    leaveRow(element)!.querySelector<HTMLElement>('[data-leave-project]')!.click();
    expect(show).not.toHaveBeenCalled();
  });

  it("the Danger Zone dialog's Transfer ownership link opens the members editor's transfer dialog", async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: false,
      leaveBlockedReason: 'last_owner',
    });
    // Start from another members tab, so the link has a tab switch to render first.
    const page = element as HTMLElement & {
      activeMembersTab: string;
      isUpdatePending: boolean;
      updateComplete: Promise<boolean>;
    };
    page.activeMembersTab = 'visitors';
    await page.updateComplete;
    const seen: Array<{ tab: string; pending: boolean }> = [];
    const open = vi.spyOn(editorOf(element), 'openTransferOwnership').mockImplementation(() => {
      seen.push({ tab: page.activeMembersTab, pending: page.isUpdatePending });
    });
    dialogOf(element).dispatchEvent(
      new CustomEvent('scion-leave-transfer-ownership', { bubbles: true, composed: true })
    );
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    // The members tab is active and rendered before the transfer dialog opens.
    expect(seen).toEqual([{ tab: 'members', pending: false }]);
  });
});
