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
 * Tests for project-detail.ts lazy file-tab mounting (ptone/scion#2381).
 *
 * All three file-tab panels used to instantiate `<scion-file-browser>`,
 * including hidden ones, each issuing its own listing request and building
 * a full file-row table on mount. These tests pin the fixed behavior:
 * inactive/unvisited tabs make no listing request and create no file-row
 * DOM; opening a tab mounts and loads it exactly once; a previously visited
 * tab stays mounted (no remount/refetch) when revisited; and the whole
 * Files section is deferred until it nears the viewport (or immediately,
 * when no IntersectionObserver is available, e.g. in this test environment).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { PageData, UserRole } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

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
const SHARED_DIR_A = 'shared-a';
const SHARED_DIR_B = 'shared-b';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function fileListResponse(paths: string[]): Response {
  return jsonResponse({
    files: paths.map((path) => ({ path, size: 12, modTime: '2026-01-01T00:00:00Z', mode: '-rw-' })),
    totalSize: paths.length * 12,
    totalCount: paths.length,
  });
}

/** Tracks calls per logical listing endpoint (workspace / each shared dir). */
function createFetchHandler() {
  const listingCalls: Record<string, number> = {
    workspace: 0,
    [SHARED_DIR_A]: 0,
    [SHARED_DIR_B]: 0,
  };

  const handler = (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;

    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(jsonResponse({ projects: [] }));
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
      return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/workspace/files`)) {
      listingCalls.workspace++;
      return Promise.resolve(fileListResponse(['workspace-file.txt']));
    }
    for (const dir of [SHARED_DIR_A, SHARED_DIR_B]) {
      if (path.includes(`/api/v1/projects/${PROJECT_ID}/shared-dirs/${dir}/files`)) {
        listingCalls[dir]++;
        return Promise.resolve(fileListResponse([`${dir}-file.txt`]));
      }
    }
    if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
      return Promise.resolve(
        jsonResponse({
          id: PROJECT_ID,
          name: 'Project One',
          slug: 'project-one',
          // No gitRemote => hub-native project => gets a 'workspace' tab,
          // plus one tab per shared dir (see getFileTabs()).
          sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
          _capabilities: { actions: ['read', 'update'] },
        })
      );
    }
    return Promise.resolve(jsonResponse({}, 404));
  };

  return { handler, listingCalls };
}

type TestElement = HTMLElement & {
  updateComplete: Promise<boolean>;
  pageData: PageData | null;
  projectId: string;
};

async function createComponent(role: UserRole = 'member'): Promise<{
  el: TestElement;
  listingCalls: Record<string, number>;
}> {
  const { handler, listingCalls } = createFetchHandler();
  vi.stubGlobal('fetch', vi.fn(handler));
  const el = document.createElement('scion-page-project-detail') as TestElement;
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  await el.updateComplete;
  return { el, listingCalls };
}

function fileBrowsers(el: TestElement): NodeListOf<Element> {
  return el.shadowRoot!.querySelectorAll('scion-file-browser');
}

function fileBrowserFor(el: TestElement, tab: string): Element | null {
  return el.shadowRoot!.querySelector(`scion-file-browser[data-tab="${tab}"]`);
}

/** Invokes the private onFileTabChange handler with `this` correctly bound to el. */
async function changeFileTab(el: TestElement, tab: string): Promise<void> {
  (el as unknown as Record<string, (e: CustomEvent) => void>)['onFileTabChange'].call(
    el,
    new CustomEvent('sl-tab-show', { detail: { name: tab } })
  );
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('scion-page-project-detail — lazy file tabs', () => {
  let element: TestElement | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // No IntersectionObserver in this environment => the Files section
    // reveals immediately rather than waiting for a real viewport signal.
    // This exercises the "explicitly opened" fallback path.
    vi.stubGlobal('IntersectionObserver', undefined);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('mounts only the active tab and issues a listing request for it', async () => {
    const { el, listingCalls } = await createComponent();
    element = el;

    // Default active tab is 'workspace' for a hub-native project.
    expect(fileBrowsers(el).length).toBe(1);
    expect(fileBrowserFor(el, 'workspace')).not.toBeNull();
    // Exactly-once dedup of the mount-time request is ptone/scion#2380's
    // fix, covered in file-browser-dedup.test.ts; here we only assert a
    // browser was actually mounted and loaded.
    expect(listingCalls.workspace).toBeGreaterThan(0);
  });

  it('unvisited tabs make no listing request and create no file-row DOM', async () => {
    const { el, listingCalls } = await createComponent();
    element = el;

    expect(fileBrowserFor(el, SHARED_DIR_A)).toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBe(0);
    expect(listingCalls[SHARED_DIR_B]).toBe(0);
  });

  it('opening a tab mounts its browser and loads its own data source', async () => {
    const { el, listingCalls } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_A);

    const browser = fileBrowserFor(el, SHARED_DIR_A);
    expect(browser).not.toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBeGreaterThan(0);
    // Switching away doesn't touch the other, still-unvisited tab.
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
    expect(listingCalls[SHARED_DIR_B]).toBe(0);
  });

  it('keeps a visited tab mounted and does not refetch on revisit', async () => {
    const { el, listingCalls } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_A);
    const countAfterFirstVisit = listingCalls[SHARED_DIR_A];
    expect(countAfterFirstVisit).toBeGreaterThan(0);
    const firstInstance = fileBrowserFor(el, SHARED_DIR_A);

    await changeFileTab(el, 'workspace');
    await changeFileTab(el, SHARED_DIR_A);

    // Same element instance (not torn down/recreated) and no extra fetch
    // beyond whatever the initial mount already issued.
    expect(fileBrowserFor(el, SHARED_DIR_A)).toBe(firstInstance);
    expect(listingCalls[SHARED_DIR_A]).toBe(countAfterFirstVisit);
  });

  it('retains correct data-source selection per tab after switching', async () => {
    const { el } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_B);

    const browserB = fileBrowserFor(el, SHARED_DIR_B) as unknown as {
      dataSource: { listFiles: () => Promise<{ files: Array<{ path: string }> }> } | null;
    };
    expect(browserB.dataSource).not.toBeNull();
    const result = await browserB.dataSource!.listFiles();
    expect(result.files.map((f) => f.path)).toEqual([`${SHARED_DIR_B}-file.txt`]);
  });

  it('defers the whole Files section until the placeholder is observed as visible', async () => {
    // Simulate a real IntersectionObserver so we can control when the
    // section is revealed, proving it does not mount eagerly.
    const observedTargets: Element[] = [];
    let capturedCallback: IntersectionObserverCallback | null = null;
    class FakeIntersectionObserver {
      constructor(cb: IntersectionObserverCallback) {
        capturedCallback = cb;
      }
      observe(target: Element): void {
        observedTargets.push(target);
      }
      unobserve(): void {}
      disconnect(): void {}
      takeRecords(): IntersectionObserverEntry[] {
        return [];
      }
    }
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver);

    const { el, listingCalls } = await createComponent();
    element = el;

    // Not yet revealed: placeholder is present, no browser mounted, no
    // listing request issued for any tab.
    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).not.toBeNull();
    expect(fileBrowsers(el).length).toBe(0);
    expect(listingCalls.workspace).toBe(0);
    expect(observedTargets.length).toBeGreaterThan(0);

    // Simulate the placeholder scrolling into view.
    capturedCallback!(
      [{ isIntersecting: true, target: observedTargets[0] } as IntersectionObserverEntry],
      new FakeIntersectionObserver(() => {}) as unknown as IntersectionObserver
    );
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(fileBrowsers(el).length).toBe(1);
    expect(listingCalls.workspace).toBeGreaterThan(0);
  });
});
