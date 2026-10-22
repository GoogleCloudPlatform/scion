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
 * Publish dialog with chooseProject (the hub Artifacts page, ptone/scion#5477):
 * the Project picker lists only the projects where the user may publish,
 * preselects the global project or the only choice, and the new artifact is
 * homed in the picked project.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ScionArtifactPublishDialog } from './artifact-publish-dialog.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

interface Project {
  id: string;
  name: string;
  slug: string;
}

const GLOBAL: Project = { id: 'g-1', name: 'Global', slug: 'global' };
const WEB: Project = { id: 'w-1', name: 'Web frontend', slug: 'web-frontend' };
const BILLING: Project = { id: 'b-1', name: 'Billing', slug: 'billing' };
const RELEASE: Project = { id: 'r-1', name: 'Release tools', slug: 'release-tools' };

interface Mock {
  calls: string[];
  posted: Array<Record<string, unknown>>;
}

/**
 * Mocks fetch: the projects the caller can see (or projectsStatus; with
 * nextCursor set, every page says there is more; ?slug= answers from
 * globalLookup), canCreate per project id in publishable (never answered for
 * ids in hold; signals collects their signals), and the create, upload and
 * finalize calls of a publish.
 */
function mockFetch(
  projects: Project[],
  publishable: Set<string>,
  projectsStatus = 200,
  opts: {
    hold?: Set<string>;
    nextCursor?: string;
    /** Projects the ?slug= lookup searches (default: projects). */
    globalLookup?: Project[];
    signals?: AbortSignal[];
    /** The paged list (not the ?slug= lookup) never answers. */
    holdList?: boolean;
    /** The paged list answers with this error status. */
    listStatus?: number;
    /** canCreate checks of these ids answer 500. */
    failScope?: Set<string>;
    /** Global's canCreate check answers only once this resolves. */
    gate?: Promise<void>;
  } = {}
): Mock {
  const m: Mock = { calls: [], posted: [] };
  const json = (body: unknown, status = 200): Response =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      m.calls.push(`${method} ${url}`);
      if (url.startsWith('/api/v1/projects?')) {
        if (projectsStatus !== 200) {
          return Promise.resolve(
            json({ error: { code: 'internal', message: 'boom' } }, projectsStatus)
          );
        }
        const slug = new URL(url, 'http://x').searchParams.get('slug');
        if (slug) {
          const found = (opts.globalLookup ?? projects).filter((p) => p.slug === slug);
          return Promise.resolve(json({ projects: found }));
        }
        if (opts.holdList) return new Promise<Response>(() => {});
        if (opts.listStatus) {
          return Promise.resolve(
            json({ error: { code: 'internal', message: 'list boom' } }, opts.listStatus)
          );
        }
        return Promise.resolve(json({ projects, nextCursor: opts.nextCursor }));
      }
      if (method === 'GET' && url.startsWith('/api/v1/artifacts?')) {
        const scope = new URL(url, 'http://x').searchParams.get('scope') ?? '';
        if (init?.signal) opts.signals?.push(init.signal);
        if (opts.hold?.has(scope)) return new Promise<Response>(() => {});
        if (opts.failScope?.has(scope)) {
          return Promise.resolve(json({ error: { code: 'internal', message: 'check boom' } }, 500));
        }
        if (opts.gate && scope === 'g-1') {
          return opts.gate.then(() => json({ artifacts: [], canCreate: publishable.has(scope) }));
        }
        return Promise.resolve(json({ artifacts: [], canCreate: publishable.has(scope) }));
      }
      if (method === 'POST' && url === '/api/v1/artifacts') {
        m.posted.push(
          JSON.parse((init?.body as string | undefined) ?? '{}') as Record<string, unknown>
        );
        return Promise.resolve(
          json(
            { artifact: { id: 'n-1' }, version: { seq: 1 }, upload: { required: ['a.md'] } },
            201
          )
        );
      }
      if (method === 'PUT') return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(
        json({ artifact: { id: 'n-1', scopeRef: 'w-1' }, version: { seq: 1 } })
      );
    })
  );
  return m;
}

async function settle(el: ScionArtifactPublishDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function openDialog(chooseProject = true): Promise<ScionArtifactPublishDialog> {
  const el = document.createElement('scion-artifact-publish-dialog') as ScionArtifactPublishDialog;
  el.chooseProject = chooseProject;
  if (!chooseProject) el.projectId = 'p-fixed';
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function picker(el: ScionArtifactPublishDialog): HTMLElement & { value: string } {
  return el.shadowRoot!.querySelector('sl-select.project-select') as HTMLElement & {
    value: string;
  };
}

/** Shoelace is not registered here: read the attributes Lit sets. */
function disabled(e: Element): boolean {
  return e.hasAttribute('disabled');
}

function optionIds(el: ScionArtifactPublishDialog): string[] {
  return Array.from(picker(el).querySelectorAll('sl-option')).map(
    (o) => o.getAttribute('value') ?? ''
  );
}

function publishButton(el: ScionArtifactPublishDialog): HTMLElement {
  return Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
    (b) => b.textContent!.trim() === 'Publish'
  ) as HTMLElement;
}

/** Picks a file and title, as the user would. */
async function fillFile(el: ScionArtifactPublishDialog): Promise<void> {
  const priv = el as unknown as {
    picked: { path: string; file: File }[];
    entry: string;
    titleValue: string;
  };
  priv.picked = [{ path: 'a.md', file: new File(['x'], 'a.md') }];
  priv.entry = 'a.md';
  priv.titleValue = 'Notes';
  await el.updateComplete;
}

describe('publish dialog project picker', () => {
  beforeAll(async () => {
    await import('./artifact-publish-dialog.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it('lists only publishable projects, global first, and preselects global', async () => {
    mockFetch([WEB, BILLING, GLOBAL, RELEASE], new Set(['g-1', 'w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['g-1', 'r-1', 'w-1']);
    expect(picker(el).value).toBe('g-1');
    expect(disabled(picker(el))).toBe(false);
  });

  it('preselects nothing when there is no global project and several choices', async () => {
    mockFetch([WEB, RELEASE], new Set(['w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['r-1', 'w-1']);
    expect(picker(el).value).toBe('');
    await fillFile(el);
    // A file and a title are not enough without a project.
    expect(disabled(publishButton(el))).toBe(true);
  });

  it('preselects nothing when the user may not publish in the global project', async () => {
    mockFetch([GLOBAL, WEB, RELEASE], new Set(['w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['r-1', 'w-1']);
    expect(picker(el).value).toBe('');
  });

  it('preselects the only publishable project', async () => {
    mockFetch([BILLING, WEB], new Set(['w-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['w-1']);
    expect(picker(el).value).toBe('w-1');
  });

  it('disables the picker and says so when there is nowhere to publish', async () => {
    mockFetch([BILLING], new Set());
    const el = await openDialog();
    expect(optionIds(el)).toEqual([]);
    expect(disabled(picker(el))).toBe(true);
    expect(picker(el).getAttribute('help-text')).toContain("can't publish");
  });

  it('shows a failed project load with Retry, and Retry loads again', async () => {
    mockFetch([WEB], new Set(['w-1']), 500);
    const el = await openDialog();
    const alert = el.shadowRoot!.querySelector('.project sl-alert')!;
    expect(alert.textContent).toContain('Could not load your projects');
    expect(picker(el)).toBeNull();

    mockFetch([WEB], new Set(['w-1']));
    (alert.querySelector('sl-button') as HTMLElement).click();
    await settle(el);
    expect(optionIds(el)).toEqual(['w-1']);
  });

  it('homes the new artifact in the picked project', async () => {
    const m = mockFetch([GLOBAL, WEB], new Set(['g-1', 'w-1']));
    const el = await openDialog();
    const select = picker(el);
    select.value = 'w-1';
    select.dispatchEvent(new Event('sl-change'));
    await fillFile(el);
    const published = vi.fn();
    el.addEventListener('artifact-published', published);
    expect(disabled(publishButton(el))).toBe(false);
    publishButton(el).click();
    await settle(el);
    expect(m.posted).toHaveLength(1);
    expect(m.posted[0].scope).toBe('w-1');
    expect(published).toHaveBeenCalledTimes(1);
  });

  it('looks up global by slug, then lists the projects the caller can see (no mine)', async () => {
    const m = mockFetch([GLOBAL, WEB], new Set(['g-1']));
    await openDialog();
    const projectCalls = m.calls.filter((c) => c.startsWith('GET /api/v1/projects?'));
    expect(projectCalls).toEqual([
      'GET /api/v1/projects?slug=global&limit=1',
      'GET /api/v1/projects?limit=200',
    ]);
  });

  it('offers and preselects global even when the capped list does not reach it', async () => {
    // The paged list (newest first) is capped before global, the oldest.
    mockFetch([WEB, RELEASE], new Set(['g-1', 'w-1', 'r-1']), 200, {
      nextCursor: 'more',
      globalLookup: [GLOBAL],
    });
    const el = await openDialog();
    expect(optionIds(el)[0]).toBe('g-1');
    expect(picker(el).value).toBe('g-1');
    expect(el.shadowRoot!.querySelector('.choices-note.truncated')).not.toBeNull();
  });

  it('lists and checks global once when the paged list also holds it', async () => {
    const m = mockFetch([WEB, GLOBAL], new Set(['g-1', 'w-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['g-1', 'w-1']);
    expect(m.calls.filter((c) => c.includes('scope=g-1'))).toHaveLength(1);
  });

  it('preselects global without waiting for the other projects to be listed', async () => {
    mockFetch([GLOBAL, WEB], new Set(['g-1', 'w-1']), 200, { holdList: true });
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['g-1']);
    expect(picker(el).value).toBe('g-1');
    expect(el.shadowRoot!.querySelector('.choices-note.checking')).not.toBeNull();
  });

  it('shows a failed global check at once, without waiting for the other projects', async () => {
    mockFetch([GLOBAL, WEB], new Set(['w-1']), 200, {
      holdList: true,
      failScope: new Set(['g-1']),
    });
    const el = await openDialog();
    expect(el.shadowRoot!.querySelector('.project sl-alert')!.textContent).toContain('check boom');
    expect(picker(el)).toBeNull();
  });

  it('a global pass after the list failed does not preselect behind the error', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const signals: AbortSignal[] = [];
    mockFetch([GLOBAL, WEB], new Set(['g-1', 'w-1']), 200, { listStatus: 500, gate, signals });
    const el = await openDialog();
    expect(el.shadowRoot!.querySelector('.project sl-alert')!.textContent).toContain('list boom');
    // The global check still in flight was cancelled.
    expect(signals[0].aborted).toBe(true);
    release();
    await settle(el);
    expect(picker(el)).toBeNull();
    expect((el as unknown as { chosen: string }).chosen).toBe('');
    await fillFile(el);
    expect(disabled(publishButton(el))).toBe(true);
  });

  it('moves focus back to Retry when Retry fails again', async () => {
    mockFetch([WEB], new Set(['w-1']), 500);
    const el = await openDialog();
    const focused: Element[] = [];
    const spy = vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (
      this: HTMLElement
    ) {
      focused.push(this);
    });
    (el.shadowRoot!.querySelector('.project sl-alert sl-button') as HTMLElement).click();
    await settle(el);
    spy.mockRestore();
    const retry = el.shadowRoot!.querySelector('.project sl-alert sl-button');
    expect(retry).not.toBeNull();
    expect(focused).toContain(retry);
  });

  it('moves focus to the Project field after a successful Retry', async () => {
    mockFetch([WEB], new Set(['w-1']), 500);
    const el = await openDialog();
    mockFetch([WEB], new Set(['w-1']));
    const focused: Element[] = [];
    const spy = vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (
      this: HTMLElement
    ) {
      focused.push(this);
    });
    (el.shadowRoot!.querySelector('.project sl-alert sl-button') as HTMLElement).click();
    await settle(el);
    spy.mockRestore();
    expect(focused).toContain(picker(el));
  });

  it('cancels the running checks when closed, and reopening starts afresh', async () => {
    const signals: AbortSignal[] = [];
    const m = mockFetch([GLOBAL, WEB], new Set(['g-1', 'w-1']), 200, {
      hold: new Set(['g-1', 'w-1']),
      signals,
    });
    const el = await openDialog();
    expect(signals).toHaveLength(2);
    const cancel = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent!.trim() === 'Cancel'
    ) as HTMLElement;
    cancel.click();
    await settle(el);
    expect(signals.every((sig) => sig.aborted)).toBe(true);
    const checksBefore = m.calls.filter((c) => c.includes('scope=')).length;
    el.open = true;
    await settle(el);
    // A new load, with new signals, and the old ones stay cancelled.
    expect(m.calls.filter((c) => c.includes('scope=')).length).toBe(checksBefore + 2);
    expect(signals.slice(2).some((sig) => sig.aborted)).toBe(false);
  });

  it('shows choices as they pass, global preselected, while other checks run', async () => {
    mockFetch([GLOBAL, WEB, RELEASE], new Set(['g-1', 'w-1', 'r-1']), 200, {
      hold: new Set(['r-1']),
    });
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['g-1', 'w-1']);
    expect(picker(el).value).toBe('g-1');
    expect(disabled(picker(el))).toBe(false);
    expect(el.shadowRoot!.querySelector('.choices-note.checking')).not.toBeNull();
  });

  it('does not replace a project the user picked while checks run', async () => {
    mockFetch([WEB, RELEASE], new Set(['w-1', 'r-1']), 200, { hold: new Set(['r-1']) });
    const el = await openDialog();
    // Release tools is still being checked, so nothing is preselected yet.
    expect(picker(el).value).toBe('');
    const select = picker(el);
    select.value = 'w-1';
    select.dispatchEvent(new Event('sl-change'));
    await settle(el);
    expect(picker(el).value).toBe('w-1');
  });

  it('says so when the project list stopped at the cap', async () => {
    mockFetch([WEB], new Set(['w-1']), 200, { nextCursor: 'more' });
    const el = await openDialog();
    expect(el.shadowRoot!.querySelector('.choices-note.truncated')!.textContent).toContain(
      'Showing projects from the first 2000'
    );
  });

  it('reports how many choices it found when the load finishes', async () => {
    mockFetch([GLOBAL, WEB, BILLING], new Set(['g-1', 'w-1']));
    const el = document.createElement(
      'scion-artifact-publish-dialog'
    ) as ScionArtifactPublishDialog;
    el.chooseProject = true;
    const counts: number[] = [];
    el.addEventListener('artifact-publish-choices', (e) =>
      counts.push((e as CustomEvent<{ count: number }>).detail.count)
    );
    document.body.appendChild(el);
    el.open = true;
    await settle(el);
    expect(counts).toEqual([2]);
  });

  it('without chooseProject shows no picker and loads no projects', async () => {
    const m = mockFetch([GLOBAL], new Set(['g-1']));
    const el = await openDialog(false);
    expect(picker(el)).toBeNull();
    expect(m.calls).toEqual([]);
  });
});
