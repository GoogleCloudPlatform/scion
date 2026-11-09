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
 * project-detail: the header overflow menu's Leave project item follows the
 * project's leave capabilities (ptone/scion#7494), and a project that
 * answers 404 renders "you no longer have access".
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities, PageData } from '../../shared/types.js';
import { navigateTo } from '../../client/navigation.js';

vi.mock('../../client/navigation.js', async (orig) => ({
  ...(await orig<typeof import('../../client/navigation.js')>()),
  navigateTo: vi.fn(),
}));
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import {
  LEAVE_BLOCKED_GROUP_DERIVED,
  LEAVE_BLOCKED_LAST_OWNER,
  PROJECT_NO_ACCESS_TITLE,
} from '../../client/project-leave.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

const PROJECT_ID = 'p-1';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function createFetchHandler(projectCaps: Capabilities | null) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(jsonResponse({ projects: [] }));
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
      return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
    }
    if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
      if (!projectCaps) {
        return Promise.resolve(
          jsonResponse({ error: { code: 'not_found', message: 'Project not found.' } }, 404)
        );
      }
      return Promise.resolve(
        jsonResponse({
          id: PROJECT_ID,
          name: 'Project One',
          slug: 'project-one',
          _capabilities: projectCaps,
        })
      );
    }
    return Promise.resolve(jsonResponse({}, 404));
  };
}

async function createComponent(projectCaps: Capabilities | null): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(projectCaps)));
  const el = document.createElement('scion-page-project-detail') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
    projectId: string;
  };
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

const leaveItem = (el: HTMLElement) =>
  el.shadowRoot?.querySelector<HTMLElement>('.header-actions [data-leave-project]') ?? null;

describe('scion-page-project-detail — Leave project in the header overflow', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('a plain member with canLeave sees an enabled Leave project item (no Settings needed)', async () => {
    element = await createComponent({ actions: ['read'], canLeave: true });
    const item = leaveItem(element);
    expect(item).not.toBeNull();
    expect(item!.hasAttribute('disabled')).toBe(false);
    expect(item!.textContent).toContain('Leave project');
    expect(element.shadowRoot?.querySelector('.header-actions')?.textContent).not.toContain(
      'Settings'
    );
    expect(element.shadowRoot?.querySelector('scion-project-leave-dialog')).not.toBeNull();
  });

  it('hidden when the capabilities carry no canLeave (experiment off)', async () => {
    element = await createComponent({ actions: ['read'] });
    expect(leaveItem(element)).toBeNull();
    expect(element.shadowRoot?.querySelector('[data-header-overflow]')).toBeNull();
  });

  it('hidden when canLeave is false with no reason', async () => {
    element = await createComponent({ actions: ['read'], canLeave: false });
    expect(leaveItem(element)).toBeNull();
  });

  it('disabled with guidance for the last owner', async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: false,
      leaveBlockedReason: 'last_owner',
    });
    const item = leaveItem(element)!;
    expect(item.hasAttribute('disabled')).toBe(true);
    expect(item.querySelector('[data-leave-blocked]')?.textContent).toBe(LEAVE_BLOCKED_LAST_OWNER);
  });

  it('disabled with guidance for a group-only member', async () => {
    element = await createComponent({
      actions: ['read'],
      canLeave: false,
      leaveBlockedReason: 'group_derived',
    });
    const item = leaveItem(element)!;
    expect(item.hasAttribute('disabled')).toBe(true);
    expect(item.querySelector('[data-leave-blocked]')?.textContent).toBe(
      LEAVE_BLOCKED_GROUP_DERIVED
    );
  });

  it('clicking Leave project opens the shared dialog', async () => {
    element = await createComponent({ actions: ['read'], canLeave: true });
    const dialog = element.shadowRoot!.querySelector(
      'scion-project-leave-dialog'
    )! as HTMLElement & {
      show: () => Promise<void>;
    };
    const show = vi.spyOn(dialog, 'show').mockResolvedValue();
    leaveItem(element)!.click();
    expect(show).toHaveBeenCalledTimes(1);
  });

  it('a project that answers 404 renders "you no longer have access", not the load failure', async () => {
    element = await createComponent(null);
    const state = element.shadowRoot?.querySelector('[data-project-no-access]');
    expect(state?.textContent).toContain(PROJECT_NO_ACCESS_TITLE);
    expect(element.shadowRoot?.textContent).not.toContain('Failed to Load Project');
  });

  /** GETs of the project itself (the page load and any reload). */
  function projectGets(): number {
    return vi.mocked(fetch).mock.calls.filter(([u]) => {
      const path = typeof u === 'string' ? u : u instanceof URL ? u.href : u.url;
      return path.endsWith(`/api/v1/projects/${PROJECT_ID}`);
    }).length;
  }

  function leaveDialog(el: HTMLElement): HTMLElement & { show: () => Promise<void> } {
    return el.shadowRoot!.querySelector('scion-project-leave-dialog')! as HTMLElement & {
      show: () => Promise<void>;
    };
  }

  function left(remainingAccess: string): CustomEvent {
    return new CustomEvent('scion-project-left', {
      detail: { projectId: PROJECT_ID, result: { remainingAccess } },
      bubbles: true,
      composed: true,
    });
  }

  it('a leave that kept access reloads the project; a leave with no access does not', async () => {
    element = await createComponent({ actions: ['read'], canLeave: true });
    const before = projectGets();
    leaveDialog(element).dispatchEvent(left('group'));
    await vi.waitFor(() => expect(projectGets()).toBe(before + 1));

    // The reload shows the loading state; wait for the page (and its dialog) to render again.
    const page = element;
    await vi.waitFor(() =>
      expect(page.shadowRoot!.querySelector('scion-project-leave-dialog')).not.toBeNull()
    );
    const mid = projectGets();
    leaveDialog(element).dispatchEvent(left('none'));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(projectGets()).toBe(mid);
  });

  it("the dialog's Transfer ownership link goes to settings with ?transfer=1", async () => {
    element = await createComponent({
      actions: ['read', 'update', 'delete', 'manage'],
      canLeave: false,
      leaveBlockedReason: 'last_owner',
    });
    vi.mocked(navigateTo).mockClear();
    leaveDialog(element).dispatchEvent(
      new CustomEvent('scion-leave-transfer-ownership', { bubbles: true, composed: true })
    );
    expect(navigateTo).toHaveBeenCalledWith(`/projects/${PROJECT_ID}/settings?transfer=1`);
  });

  it('a disabled (blocked) Leave project item does not open the dialog', async () => {
    element = await createComponent({
      actions: ['read'],
      canLeave: false,
      leaveBlockedReason: 'group_derived',
    });
    const show = vi.spyOn(leaveDialog(element), 'show').mockResolvedValue();
    leaveItem(element)!.click();
    expect(show).not.toHaveBeenCalled();
  });
});
