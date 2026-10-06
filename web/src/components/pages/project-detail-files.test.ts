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

const DEFAULT_PROJECT_FIELDS = {
  id: PROJECT_ID,
  name: 'Project One',
  slug: 'project-one',
  // No gitRemote => hub-native project => gets a 'workspace' tab, plus one
  // tab per shared dir (see getFileTabs()).
  sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
  _capabilities: { actions: ['read', 'update'] },
};

/** Tracks calls per logical listing endpoint (workspace / each shared dir). */
function createFetchHandler(projectOverrides: Record<string, unknown> = {}) {
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
    // File-content endpoint (editor open): .../workspace/files/<name>?format=json
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/workspace/files/`)) {
      return Promise.resolve(
        jsonResponse({ content: 'hello from workspace', modTime: '2026-01-01T00:00:00Z' })
      );
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/workspace/files`)) {
      listingCalls.workspace++;
      return Promise.resolve(fileListResponse(['workspace-file.txt']));
    }
    for (const dir of [SHARED_DIR_A, SHARED_DIR_B]) {
      if (path.includes(`/api/v1/projects/${PROJECT_ID}/shared-dirs/${dir}/files/`)) {
        return Promise.resolve(
          jsonResponse({ content: `hello from ${dir}`, modTime: '2026-01-01T00:00:00Z' })
        );
      }
      if (path.includes(`/api/v1/projects/${PROJECT_ID}/shared-dirs/${dir}/files`)) {
        listingCalls[dir]++;
        return Promise.resolve(fileListResponse([`${dir}-file.txt`]));
      }
    }
    if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
      return Promise.resolve(jsonResponse({ ...DEFAULT_PROJECT_FIELDS, ...projectOverrides }));
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

async function createComponent(
  role: UserRole = 'member',
  projectOverrides: Record<string, unknown> = {}
): Promise<{
  el: TestElement;
  listingCalls: Record<string, number>;
}> {
  const { handler, listingCalls } = createFetchHandler(projectOverrides);
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

/** Number of tab panels configured (one <sl-tab> per rendered tab) — the
 * count every one of them would have mounted a browser for on main. */
function getTabCountForFixture(el: TestElement): number {
  return el.shadowRoot!.querySelectorAll('sl-tab').length;
}

/** The basePath of the currently-open file editor's data source, if any. */
function editorDataSourceBasePath(el: TestElement): string | undefined {
  const editor = el.shadowRoot?.querySelector('scion-file-editor') as unknown as {
    dataSource: { basePath: string } | null;
  } | null;
  return editor?.dataSource?.basePath;
}

/**
 * Switches tabs by dispatching an `sl-tab-show` CustomEvent on the rendered
 * `sl-tab-group` element, rather than calling the private onFileTabChange
 * handler directly. This exercises the actual
 * `@sl-tab-show=${this.onFileTabChange}` event-listener wiring in the
 * template.
 *
 * It does NOT exercise Shoelace's own tab-group behavior: `<sl-tab-group>`
 * is not a registered custom element in this test environment (happy-dom
 * has no upgraded Shoelace components here), so a real click or `active`
 * attribute change would do nothing, and Shoelace's internal fallback
 * (silently displaying tabs[0] without emitting sl-tab-show when the named
 * tab doesn't exist) is not covered by this helper or by any test in this
 * file. That fallback behavior is what
 * e2e/tests/project-detail-files.spec.ts (Playwright, real browser, real
 * Shoelace) exercises instead.
 */
async function changeFileTab(el: TestElement, tab: string): Promise<void> {
  const tabGroup = el.shadowRoot!.querySelector('sl-tab-group');
  if (!tabGroup) throw new Error('sl-tab-group not found — is the Files section open?');
  tabGroup.dispatchEvent(
    new CustomEvent('sl-tab-show', { detail: { name: tab }, bubbles: true, composed: true })
  );
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

/**
 * Simulates a live project update arriving over SSE: updates the shared
 * stateManager's project map and fires the same `projects-updated` event
 * type the component's onProjectsUpdated() listener is registered for
 * (production dispatches this from StateManager.notify() after processing
 * an SSE `project.{id}.updated` event). onProjectsUpdated() itself just
 * reads `stateManager.getProject(this.projectId)` back out and merges it,
 * so seeding the map and firing the event reproduces that path exactly.
 */
async function pushProjectUpdate(el: TestElement, patch: Record<string, unknown>): Promise<void> {
  const { stateManager } = await import('../../client/state.js');
  const current = stateManager.getProject(PROJECT_ID) ?? { id: PROJECT_ID };
  stateManager.seedProjects([{ ...current, ...patch } as never]);
  stateManager.dispatchEvent(new CustomEvent('projects-updated'));
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

/**
 * A minimal fake IntersectionObserver for driving the Files section's
 * deferred-reveal logic (observeFilesSection() / revealFilesSection() in
 * project-detail.ts) without a real browser. Records every observe() and
 * unobserve() target, and exposes `fire()` to simulate an intersection
 * entry through the captured callback — e.g. a placeholder scrolling into
 * view. Install with `vi.stubGlobal('IntersectionObserver', fakeIO.Ctor)`.
 *
 * `fire()` only delivers entries for targets currently observed (observed
 * and not since unobserved or disconnected) — matching real
 * IntersectionObserver semantics, where the browser never reports
 * intersections for a target you aren't (or are no longer) observing. A
 * `fire()` call with no currently-observed targets in its entries is a
 * no-op. `fire()` can be called any number of times.
 */
function makeFakeIntersectionObserver(): {
  Ctor: new (cb: IntersectionObserverCallback) => IntersectionObserver;
  observedTargets: Element[];
  unobserveCalls: Element[];
  fire: (entries: Array<{ isIntersecting: boolean; target: Element }>) => void;
} {
  const observedTargets: Element[] = [];
  const unobserveCalls: Element[] = [];
  const currentlyObserved = new Set<Element>();
  let callback: IntersectionObserverCallback | null = null;
  let instance: IntersectionObserver | null = null;

  class FakeIntersectionObserver {
    constructor(cb: IntersectionObserverCallback) {
      callback = cb;
      instance = this as unknown as IntersectionObserver;
    }
    observe(target: Element): void {
      observedTargets.push(target);
      currentlyObserved.add(target);
    }
    unobserve(target: Element): void {
      unobserveCalls.push(target);
      currentlyObserved.delete(target);
    }
    disconnect(): void {
      currentlyObserved.clear();
    }
    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }
  }

  return {
    Ctor: FakeIntersectionObserver as unknown as new (
      cb: IntersectionObserverCallback
    ) => IntersectionObserver,
    observedTargets,
    unobserveCalls,
    fire(entries) {
      // Reuse the one constructed instance as the callback's second
      // argument, rather than constructing a throwaway one — building a
      // second instance here would re-run the constructor and overwrite
      // `callback` with that instance's (no-op) callback, silently turning
      // every call after the first into a no-op.
      const deliverable = entries.filter((e) => currentlyObserved.has(e.target));
      if (deliverable.length === 0 || !callback || !instance) return;
      callback(deliverable as IntersectionObserverEntry[], instance);
    },
  };
}

describe('makeFakeIntersectionObserver (test helper)', () => {
  it('fire() can be called more than once and still invokes the callback each time', () => {
    // Round-1 Gemini-fix follow-up review, NB-1: fire() used to build a
    // throwaway second Ctor instance as the callback's second argument,
    // which re-ran the constructor and silently overwrote the captured
    // callback with that instance's own no-op — every fire() after the
    // first one did nothing. No current component test happened to call
    // fire() twice, so this went unnoticed; it would have been a silent
    // false-pass trap for the next test that did.
    const fakeIO = makeFakeIntersectionObserver();
    const target = document.createElement('div');
    let callCount = 0;
    const observer = new fakeIO.Ctor(() => {
      callCount++;
    });
    observer.observe(target);

    fakeIO.fire([{ isIntersecting: true, target }]);
    expect(callCount).toBe(1);

    fakeIO.fire([{ isIntersecting: true, target }]);
    expect(callCount).toBe(2);
  });

  it('fire() does not deliver entries for a target that was never observed, was since unobserved, or after disconnect()', () => {
    const fakeIO = makeFakeIntersectionObserver();
    const observedTarget = document.createElement('div');
    const neverObservedTarget = document.createElement('div');
    let callCount = 0;
    const observer = new fakeIO.Ctor(() => {
      callCount++;
    });
    observer.observe(observedTarget);

    fakeIO.fire([{ isIntersecting: true, target: neverObservedTarget }]);
    expect(callCount).toBe(0);

    observer.unobserve(observedTarget);
    fakeIO.fire([{ isIntersecting: true, target: observedTarget }]);
    expect(callCount).toBe(0);

    // Round-2 review, Nit 1: disconnect() was documented ("unobserved or
    // disconnected") but not self-tested.
    observer.observe(observedTarget);
    observer.disconnect();
    fakeIO.fire([{ isIntersecting: true, target: observedTarget }]);
    expect(callCount).toBe(0);
  });

  it('fire() delivers only the currently-observed entries, not the full input list, when the two differ', () => {
    // Round-2 review, Nit 2: the two existing self-tests above only ever
    // fire a single entry, so they can't tell "passes the filtered list"
    // apart from "passes the original entries" (both behave the same when
    // at least one target is observed). Fire a mix of one observed and one
    // unobserved target and pin exactly what the callback receives.
    const fakeIO = makeFakeIntersectionObserver();
    const observedTarget = document.createElement('div');
    const unobservedTarget = document.createElement('div');
    let received: IntersectionObserverEntry[] | null = null;
    const observer = new fakeIO.Ctor((entries) => {
      received = entries;
    });
    observer.observe(observedTarget);

    const observedEntry = { isIntersecting: true, target: observedTarget };
    const unobservedEntry = { isIntersecting: true, target: unobservedTarget };
    fakeIO.fire([observedEntry, unobservedEntry]);

    // toEqual compares deeply, and Vitest's deep-equality for DOM nodes
    // uses isEqualNode — two empty <div>s are equal — so toEqual([observedEntry])
    // would pass even if fire() delivered unobservedEntry instead: it
    // checks only how many entries were delivered, not which one. Assert
    // identity instead.
    expect(received).toHaveLength(1);
    expect(received![0]).toBe(observedEntry);
  });
});

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
    // Exactly one request: this branch is stacked on ptone/scion#2380's
    // dedup fix (also covered on its own in file-browser-dedup.test.ts).
    expect(listingCalls.workspace).toBe(1);
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
    expect(listingCalls[SHARED_DIR_A]).toBe(1);
    // Switching away doesn't touch the other, still-unvisited tab.
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
    expect(listingCalls[SHARED_DIR_B]).toBe(0);
  });

  it('keeps a visited tab mounted and does not refetch on revisit, without mounting untouched tabs', async () => {
    const { el, listingCalls } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_A);
    expect(listingCalls[SHARED_DIR_A]).toBe(1);
    const countAfterFirstVisit = listingCalls[SHARED_DIR_A];
    const firstInstance = fileBrowserFor(el, SHARED_DIR_A);
    // Two of the three configured tabs (workspace, shared-dir-A) have been
    // visited so far — on main, all three would already be mounted, so this
    // distinguishes the fix from main rather than trivially passing there.
    expect(fileBrowsers(el).length).toBe(2);
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();

    await changeFileTab(el, 'workspace');
    await changeFileTab(el, SHARED_DIR_A);

    // Same element instance (not torn down/recreated) and no extra fetch
    // beyond whatever the initial mount already issued.
    expect(fileBrowserFor(el, SHARED_DIR_A)).toBe(firstInstance);
    expect(listingCalls[SHARED_DIR_A]).toBe(countAfterFirstVisit);
    // Still only the two actually-visited tabs, even after switching back
    // and forth between them.
    expect(fileBrowsers(el).length).toBe(2);
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
  });

  it('retains correct data-source selection per tab after switching — verified via rendered rows', async () => {
    const { el } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_B);
    await changeFileTab(el, SHARED_DIR_A);

    // Assert what each mounted browser actually rendered, not just what its
    // dataSource object would return if called directly — this exercises
    // scion-file-browser's own connectedCallback -> loadFiles -> render
    // path for both tabs' data sources, not just getTabDataSource() wiring.
    const rowsFor = (tab: string): string =>
      (fileBrowserFor(el, tab)?.shadowRoot?.textContent ?? '').replace(/\s+/g, ' ');

    expect(rowsFor(SHARED_DIR_A)).toContain(`${SHARED_DIR_A}-file.txt`);
    expect(rowsFor(SHARED_DIR_A)).not.toContain(`${SHARED_DIR_B}-file.txt`);
    expect(rowsFor(SHARED_DIR_B)).toContain(`${SHARED_DIR_B}-file.txt`);
    expect(rowsFor(SHARED_DIR_B)).not.toContain(`${SHARED_DIR_A}-file.txt`);
  });

  it('records the before/after mounted-browser count for the configured tab set', async () => {
    const { el } = await createComponent();
    element = el;

    // Before (main): every configured tab panel unconditionally mounted a
    // scion-file-browser, so this fixture (workspace + 2 shared dirs) would
    // mount 3. After (this fix): only the active tab does, at initial load.
    expect(getTabCountForFixture(el)).toBe(3);
    expect(fileBrowsers(el).length).toBe(1);
  });

  it('defers the whole Files section until the placeholder is observed as visible', async () => {
    // Simulate a real IntersectionObserver so we can control when the
    // section is revealed, proving it does not mount eagerly.
    const fakeIO = makeFakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fakeIO.Ctor);

    const { el, listingCalls } = await createComponent();
    element = el;

    // Not yet revealed: placeholder is present, no browser mounted, no
    // listing request issued for any tab.
    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).not.toBeNull();
    expect(fileBrowsers(el).length).toBe(0);
    expect(listingCalls.workspace).toBe(0);
    expect(fakeIO.observedTargets.length).toBeGreaterThan(0);

    // Simulate the placeholder scrolling into view.
    fakeIO.fire([{ isIntersecting: true, target: fakeIO.observedTargets[0] }]);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(fileBrowsers(el).length).toBe(1);
    expect(listingCalls.workspace).toBe(1);
  });

  it('unobserves the placeholder if the Files section disappears before ever being revealed', async () => {
    // Upstream review (GoogleCloudPlatform/scion#2185): if
    // shouldShowFilesSection() flips to false before the placeholder was
    // ever revealed (e.g. the last shared dir is removed via a live
    // update), observeFilesSection() used to return early on `!placeholder`
    // without unobserving the now-detached element, holding the reference
    // in the observer and in observedFilesPlaceholder until the section
    // reappeared or the element disconnected.
    const fakeIO = makeFakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fakeIO.Ctor);

    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }],
    });
    element = el;

    // Not yet revealed: the placeholder exists and is being observed.
    const placeholder = el.shadowRoot?.querySelector('.files-section-placeholder');
    expect(placeholder).not.toBeNull();
    expect(fakeIO.observedTargets).toContain(placeholder);
    expect(fakeIO.unobserveCalls.length).toBe(0);

    // The only shared dir is removed before the section was ever revealed —
    // shouldShowFilesSection() goes false and Lit tears the placeholder down.
    await pushProjectUpdate(el, { sharedDirs: [] });

    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(fakeIO.unobserveCalls).toContain(placeholder);
  });

  it('re-observes a fresh placeholder and still reveals correctly after the section reappears', async () => {
    // Round-1 Gemini-fix review, NB-1: the leak fix above changes how the
    // old placeholder is released when the section goes
    // false -> (one or more updates) -> true. Before the fix, the
    // "replaced" branch (observedFilesPlaceholder set but pointing at a
    // different element than the current placeholder) released it on
    // flip-back; now the new early-return branch releases it while the
    // section is hidden, leaving observedFilesPlaceholder null by the time
    // it reappears. Nothing in the suite covered that flip-back path.
    const fakeIO = makeFakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fakeIO.Ctor);

    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }],
    });
    element = el;

    const firstPlaceholder = el.shadowRoot?.querySelector('.files-section-placeholder');
    expect(firstPlaceholder).not.toBeNull();

    // Remove the only shared dir: the section disappears before being
    // revealed (this is what the leak fix unobserves).
    await pushProjectUpdate(el, { sharedDirs: [] });
    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(fakeIO.unobserveCalls).toContain(firstPlaceholder);

    // Re-add it: the section reappears, still unrevealed, with a brand-new
    // placeholder element.
    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_A }] });
    const secondPlaceholder = el.shadowRoot?.querySelector('.files-section-placeholder');
    expect(secondPlaceholder).not.toBeNull();
    expect(secondPlaceholder).not.toBe(firstPlaceholder);

    // The new placeholder must actually be (re-)observed — not silently
    // skipped because the field was left looking like it already pointed
    // at something. And unobserveCalls must be exactly [firstPlaceholder]:
    // not unobserved a second time now that it's long gone, and nothing
    // else (in particular not secondPlaceholder) spuriously unobserved.
    expect(fakeIO.observedTargets).toContain(secondPlaceholder);
    expect(fakeIO.unobserveCalls).toEqual([firstPlaceholder]);

    // Firing the intersection callback on the new placeholder must still
    // reveal the section and mount/load the active tab — flip-back didn't
    // just stop leaking, it still works.
    fakeIO.fire([{ isIntersecting: true, target: secondPlaceholder! }]);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
  });

  // ── Regression coverage: activeFileTab can drift from the rendered tab
  // list (round-1 review, blocking finding 1). Reproduced originally via a
  // live project update that adds a shared dir to a project that had none —
  // exactly what onProjectsUpdated() applies from an SSE
  // project.{id}.updated event. ──

  it('mounts the first shared-dir tab at initial load for a git project that already has shared dirs', async () => {
    // Coverage gap noted in review: this path (git project, shared dirs
    // present from the very first load — loadData() sets activeFileTab to
    // the first one) already worked, but had no test pinning it.
    const { el, listingCalls } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    // Git (non-shared-workspace) projects get no workspace tab — only one
    // per shared dir (getFileTabs()) — and loadData() activates the first.
    expect(getTabCountForFixture(el)).toBe(2);
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBe(1);
  });

  it('mounts a shared dir that appears after load via a live project update, instead of leaving the panel empty', async () => {
    // Reproduces the round-1 regression: a git (non-shared-workspace)
    // project starts with zero shared dirs, so shouldShowFilesSection() is
    // false and the whole Files section — including activeFileTab
    // normalization — never runs. activeFileTab is left at its default,
    // 'workspace', which is not a real tab for this project type. A live
    // update (SSE project.{id}.updated) then adds a shared dir.
    const { el, listingCalls } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [],
    });
    element = el;

    // No shared dirs yet: the Files section doesn't render at all.
    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(el.shadowRoot?.querySelector('.workspace-section')).toBeNull();

    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_A }] });

    // Before the fix: activeFileTab stayed 'workspace', which matches no
    // tab; Shoelace falls back to displaying tabs[0] without emitting
    // sl-tab-show, so nothing mounted a browser for it and the panel shown
    // was empty with no way to recover by clicking. After the fix: the
    // normalized activeFileTab is marked visited as soon as the section
    // opens, so the newly-appeared tab is mounted and loaded correctly.
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBe(1);
  });

  it('falls back to another tab when the active shared dir is removed while the page is open', async () => {
    const { el, listingCalls } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    // loadData() activates the first shared dir at load.
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();

    // The active shared dir (A) is removed server-side; only B remains.
    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_B }] });

    // activeFileTab pointed at a now-gone tab; the component falls back to
    // the remaining one and mounts it, rather than leaving an empty panel
    // with no matching tab at all.
    expect(fileBrowserFor(el, SHARED_DIR_B)).not.toBeNull();
    expect(listingCalls[SHARED_DIR_B]).toBe(1);
  });

  it('hides the Files section entirely when the last shared dir is removed (no empty panel left behind)', async () => {
    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }],
    });
    element = el;
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();

    await pushProjectUpdate(el, { sharedDirs: [] });

    expect(el.shadowRoot?.querySelector('.workspace-section')).toBeNull();
    expect(fileBrowsers(el).length).toBe(0);
  });

  it('does not silently remount a re-added shared dir that was visited under a prior removal (visitedFileTabs is pruned)', async () => {
    const { el, listingCalls } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    // Visit A (loadData() activates it at load).
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBe(1);

    // Remove A, then re-add a dir with the exact same name.
    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_B }] });
    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }] });

    // Without pruning, the stale 'shared-a' entry in visitedFileTabs would
    // still be present, mounting a browser for it — hidden, since B is now
    // active — without the user ever having opened it this time around.
    expect(fileBrowserFor(el, SHARED_DIR_A)).toBeNull();

    // Opening it now is a fresh visit: it mounts and issues its own request.
    await changeFileTab(el, SHARED_DIR_A);
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    expect(listingCalls[SHARED_DIR_A]).toBe(2);
  });

  // ── AC3: editor/back-flow coverage (round-1 review, blocking finding 2) ──

  it('unmounts file browsers while the editor is open, and remounts previously-visited tabs on Back', async () => {
    const { el } = await createComponent();
    element = el;

    await changeFileTab(el, SHARED_DIR_A); // visit a second tab besides the default workspace one
    expect(fileBrowsers(el).length).toBe(2);

    // Open the editor the same way a real row click does: scion-file-browser
    // dispatches `file-edit-requested`, which bubbles/composes out of its
    // shadow root to the listener wired on the element in
    // renderFilesSection(). changeFileTab() above made SHARED_DIR_A active.
    fileBrowserFor(el, SHARED_DIR_A)!.dispatchEvent(
      new CustomEvent('file-edit-requested', {
        detail: { path: `${SHARED_DIR_A}-file.txt` },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    // Every file browser (and the tab group) is unmounted while editing.
    expect(fileBrowsers(el).length).toBe(0);
    expect(el.shadowRoot?.querySelector('sl-tab-group')).toBeNull();
    const editor = el.shadowRoot?.querySelector('scion-file-editor');
    expect(editor).not.toBeNull();

    // "clicks Back": dispatch a real click on the rendered Back button,
    // rather than calling handleEditorClosed() directly.
    const backButton = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      (b.textContent ?? '').includes('Back to files')
    );
    expect(backButton).toBeTruthy();
    backButton!.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // Back to the tab view: both previously-visited tabs remount ...
    expect(el.shadowRoot?.querySelector('scion-file-editor')).toBeNull();
    expect(fileBrowserFor(el, 'workspace')).not.toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    // ... and the never-visited one still does not.
    expect(fileBrowserFor(el, SHARED_DIR_B)).toBeNull();
    expect(fileBrowsers(el).length).toBe(2);
  });

  // ── Round-2 review, blocking finding 1: normalizeActiveFileTab() ran on
  // every update regardless of whether the file editor was open. If the
  // active shared dir was removed server-side while a user was editing (or
  // creating) a file in it, activeFileTab silently retargeted to another
  // shared dir, and the *already-open* editor's dataSource followed —
  // redirecting an in-progress Save to a directory the user never chose,
  // and for an existing file, discarding unsaved edits by reloading the
  // other tab's content. Fixed by skipping normalization entirely while
  // editingFilePath !== null. ──

  it('does not retarget an open existing-file edit when its shared dir is removed via a live update', async () => {
    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    // loadData() activates the first shared dir (A) at load.
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();

    fileBrowserFor(el, SHARED_DIR_A)!.dispatchEvent(
      new CustomEvent('file-edit-requested', {
        detail: { path: `${SHARED_DIR_A}-file.txt` },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    const editor = el.shadowRoot?.querySelector('scion-file-editor');
    expect(editor).not.toBeNull();
    const originalBasePath = editorDataSourceBasePath(el);
    expect(originalBasePath).toContain(`/shared-dirs/${SHARED_DIR_A}/`);

    // The active shared dir (A, being edited) is removed server-side while
    // the editor is still open.
    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_B }] });

    // The editor must still be open, still targeting A's data source — not
    // silently retargeted to B, which would discard the in-progress edit
    // and/or redirect a Save into the wrong directory.
    expect(el.shadowRoot?.querySelector('scion-file-editor')).not.toBeNull();
    expect(editorDataSourceBasePath(el)).toBe(originalBasePath);
    expect((el as unknown as { editingFilePath: string | null }).editingFilePath).toBe(
      `${SHARED_DIR_A}-file.txt`
    );
  });

  it('does not retarget an open new-file create when its shared dir is removed via a live update', async () => {
    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();

    fileBrowserFor(el, SHARED_DIR_A)!.dispatchEvent(
      new CustomEvent('file-create-requested', { bubbles: true, composed: true })
    );
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-file-editor')).not.toBeNull();
    const originalBasePath = editorDataSourceBasePath(el);
    expect(originalBasePath).toContain(`/shared-dirs/${SHARED_DIR_A}/`);

    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_B }] });

    // Still creating against A's data source — a Save must not silently
    // land in B, which the user never chose.
    expect(el.shadowRoot?.querySelector('scion-file-editor')).not.toBeNull();
    expect(editorDataSourceBasePath(el)).toBe(originalBasePath);
    expect((el as unknown as { editingFilePath: string | null }).editingFilePath).toBe('');
  });

  it('normalizes activeFileTab on the update after the editor closes, once the tab it was skipping for is gone', async () => {
    const { el } = await createComponent('member', {
      gitRemote: 'https://example.com/repo.git',
      sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
    });
    element = el;

    fileBrowserFor(el, SHARED_DIR_A)!.dispatchEvent(
      new CustomEvent('file-edit-requested', {
        detail: { path: `${SHARED_DIR_A}-file.txt` },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    await pushProjectUpdate(el, { sharedDirs: [{ name: SHARED_DIR_B }] });
    expect(el.shadowRoot?.querySelector('scion-file-editor')).not.toBeNull(); // still open, per the tests above

    const backButton = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      (b.textContent ?? '').includes('Back to files')
    );
    backButton!.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // Now that the editor is closed, normalization catches up: A is gone,
    // so the view falls back to the only remaining tab, B.
    expect(el.shadowRoot?.querySelector('scion-file-editor')).toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_B)).not.toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_A)).toBeNull();
  });
});

describe('scion-page-project-detail — empty directory per agent (#2703)', () => {
  let element: TestElement | null = null;
  const EMPTY_PER_AGENT = { labels: { 'scion.dev/workspace-mode': 'per-agent' } };

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

  it('hides the Files section and never observes or lists it when there are no shared dirs', async () => {
    const fakeIO = makeFakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fakeIO.Ctor);

    const { el, listingCalls } = await createComponent('member', {
      ...EMPTY_PER_AGENT,
      sharedDirs: [],
    });
    element = el;

    expect(el.shadowRoot?.querySelector('.files-section-placeholder')).toBeNull();
    expect(el.shadowRoot?.querySelector('sl-tab-group')).toBeNull();
    expect(fakeIO.observedTargets.length).toBe(0);
    expect(fileBrowsers(el).length).toBe(0);
    expect(listingCalls.workspace).toBe(0);
  });

  it('shows only shared-dir tabs (no workspace tab) when shared dirs exist', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);

    const { el, listingCalls } = await createComponent('member', EMPTY_PER_AGENT);
    element = el;

    const tabs = [...el.shadowRoot!.querySelectorAll('sl-tab')].map((t) => t.getAttribute('panel'));
    expect(tabs).toEqual([SHARED_DIR_A, SHARED_DIR_B]);
    expect(fileBrowserFor(el, 'workspace')).toBeNull();
    expect(fileBrowserFor(el, SHARED_DIR_A)).not.toBeNull();
    expect(listingCalls.workspace).toBe(0);
    expect(listingCalls[SHARED_DIR_A]).toBe(1);
  });

  it('still shows the workspace tab for a linked project', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);

    const { el, listingCalls } = await createComponent('member', {
      projectType: 'linked',
      sharedDirs: [],
    });
    element = el;

    expect(fileBrowserFor(el, 'workspace')).not.toBeNull();
    expect(listingCalls.workspace).toBe(1);
  });

  it('still shows the workspace tab for a shared workspace directory project', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);

    const { el, listingCalls } = await createComponent('member', {
      labels: { 'scion.dev/workspace-mode': 'shared' },
      sharedDirs: [],
    });
    element = el;

    expect(fileBrowserFor(el, 'workspace')).not.toBeNull();
    expect(listingCalls.workspace).toBe(1);
  });
});
